package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/store"
)

type capturedReq struct {
	body  []byte
	sig   string
	ts    string
	deliv string
}

// 下游接收器：记录请求并按配置的响应码回复。
type downstream struct {
	mu       sync.Mutex
	requests []capturedReq
	respond  int // 0 = 200
}

func (d *downstream) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	d.requests = append(d.requests, capturedReq{
		body:  body,
		sig:   r.Header.Get("X-MailHub-Signature"),
		ts:    r.Header.Get("X-MailHub-Timestamp"),
		deliv: r.Header.Get("X-MailHub-Delivery"),
	})
	code := d.respond
	d.mu.Unlock()
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
}

func (d *downstream) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

func newEnv(t *testing.T) (*store.Store, *Deliverer, *downstream) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open("", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{WebhookConcurrency: 2}
	c, _ := crypto.New(make([]byte, 32))
	enc, _ := c.Encrypt([]byte("pw"))
	if _, err := st.CreateAccount(&store.Account{
		Name: "a@x.com", Email: "a@x.com", IMAPHost: "h", IMAPPort: 993, Username: "a@x.com",
		AuthType: "password", CredentialEnc: enc, PollInterval: 60, NotifyEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	return st, New(cfg, st), &downstream{}
}

// ingestOne 经 IngestMessage 入库一封带投递记录的邮件（账号 notify_enabled 快照 = true）。
func ingestOne(t *testing.T, st *store.Store, uid uint32) int64 {
	t.Helper()
	id, err := st.IngestMessage(&store.NewMessage{
		AccountID: 1, Folder: "INBOX", UIDValidity: 1, UID: uid,
		MessageID: "<x@y>", Subject: "测试邮件", FromAddr: "s@x.com",
		ToAddrsJSON: `["a@x.com"]`, BodyText: "正文", Snippet: "正文",
		NotifyEnabled: true, RawPath: "mails/1/x.eml",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeliverSuccessAndSignature(t *testing.T) {
	st, d, ds := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(ds.handler))
	defer srv.Close()

	if _, err := st.CreateWebhook(&store.Webhook{URL: srv.URL, Secret: "sec-test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ingestOne(t, st, 1)

	d.pump(context.Background())
	if ds.count() != 1 {
		t.Fatalf("下游应收 1 次，得 %d", ds.count())
	}
	dv, total, _ := st.ListDeliveries(store.DeliveryFilter{})
	if total != 1 || dv[0].Status != "success" {
		t.Fatalf("投递记录应为 success: %+v", dv)
	}

	// 验签：HMAC_SHA256(secret, ts + "." + body)（DESIGN 5.2）
	req := ds.requests[0]
	mac := hmac.New(sha256.New, []byte("sec-test"))
	mac.Write([]byte(req.ts + "." + string(req.body)))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if req.sig != want {
		t.Fatalf("签名不符:\n got %s\nwant %s", req.sig, want)
	}

	// 载荷结构（DESIGN 5.2）
	var p struct {
		Event      string `json:"event"`
		DeliveryID int64  `json:"delivery_id"`
		Message    struct {
			AccountEmail string `json:"account_email"`
			Subject      string `json:"subject"`
		} `json:"message"`
	}
	if err := json.Unmarshal(req.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != "message.received" || p.Message.AccountEmail != "a@x.com" || p.Message.Subject != "测试邮件" {
		t.Fatalf("载荷不符: %s", req.body)
	}
}

func TestDeliverRetryThenExhausted(t *testing.T) {
	st, d, ds := newEnv(t)
	ds.respond = 500
	srv := httptest.NewServer(http.HandlerFunc(ds.handler))
	defer srv.Close()

	if _, err := st.CreateWebhook(&store.Webhook{URL: srv.URL, Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ingestOne(t, st, 1)

	// 缩短重试间隔（包级序列测试内覆盖，结束还原）
	oldDelays := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = oldDelays })

	d.pump(context.Background()) // 第 1 次：失败，安排重试
	if ds.count() != 1 {
		t.Fatalf("首次投递应有 1 次请求，得 %d", ds.count())
	}
	dv, total, _ := st.ListDeliveries(store.DeliveryFilter{})
	if total != 1 || dv[0].Status != "pending" || dv[0].Attempts != 1 {
		t.Fatalf("首次失败应为 pending/1: %+v", dv[0])
	}

	time.Sleep(5 * time.Millisecond)
	// pump 每轮取件一次、投一轮；重试到期需多轮直至耗尽
	for i := 0; i < 6; i++ {
		time.Sleep(2 * time.Millisecond)
		d.pump(context.Background())
		dv, _, _ := st.ListDeliveries(store.DeliveryFilter{})
		if dv[0].Status == "failed" {
			break
		}
	}
	dv, _, _ = st.ListDeliveries(store.DeliveryFilter{})
	if dv[0].Status != "failed" {
		t.Fatalf("重试耗尽应为 failed，得 %s（attempts=%d）", dv[0].Status, dv[0].Attempts)
	}

	// 手动重发：failed → pending，立即再投
	if err := st.RetryDelivery(dv[0].ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	d.pump(context.Background())
	if n := ds.count(); n < 2 {
		t.Fatalf("手动重发后应继续投递，请求 %d 次", n)
	}
}

// include_body：载荷附带 body_text（DESIGN 5.2）。
func TestDeliverIncludeBody(t *testing.T) {
	st, d, ds := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(ds.handler))
	defer srv.Close()

	if _, err := st.CreateWebhook(&store.Webhook{
		URL: srv.URL, Secret: "s", Enabled: true, IncludeBody: true,
	}); err != nil {
		t.Fatal(err)
	}
	ingestOne(t, st, 1)
	d.pump(context.Background())

	var p struct {
		Message struct {
			BodyText string `json:"body_text"`
		} `json:"message"`
	}
	if err := json.Unmarshal(ds.requests[0].body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Message.BodyText != "正文" {
		t.Fatalf("include_body 应带正文: %s", ds.requests[0].body)
	}
}

// 已入队后 webhook 被禁用：投递保持 pending（重新启用后继续）。
// （禁用期间的新邮件不会入队——matchingWebhooksTx 只匹配 enabled=1。）
func TestDeliverDisabledWebhookKeepsPending(t *testing.T) {
	st, d, ds := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(ds.handler))
	defer srv.Close()

	wid, err := st.CreateWebhook(&store.Webhook{URL: srv.URL, Secret: "s", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ingestOne(t, st, 1) // 入队时 enabled，生成 pending 投递
	if err := st.ApplyWebhookPatch(wid, store.WebhookPatch{
		Enabled: boolPtr(false),
	}); err != nil {
		t.Fatal(err)
	}
	d.pump(context.Background())

	if ds.count() != 0 {
		t.Fatal("禁用 webhook 不应投递")
	}
	dv, total, _ := st.ListDeliveries(store.DeliveryFilter{})
	if total != 1 || dv[0].Status != "pending" {
		t.Fatalf("应保持 pending，得 %d 条 status=%s", total, dv[0].Status)
	}
}

func boolPtr(b bool) *bool { return &b }

// 满批（≥batchSize）禁用 webhook 的 pending 排在前面：投递器不得自唤醒空转，
// 也不得饿死启用中 webhook 的投递。
func TestPumpSkipsDisabledWebhooks(t *testing.T) {
	st, d, ds := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(ds.handler))
	defer srv.Close()

	disabled, err := st.CreateWebhook(&store.Webhook{URL: srv.URL + "/off", Secret: "s", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= batchSize+10; i++ {
		ingestOne(t, st, uint32(i))
	}
	if err := st.ApplyWebhookPatch(disabled, store.WebhookPatch{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateWebhook(&store.Webhook{URL: srv.URL + "/on", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	ingestOne(t, st, uint32(batchSize+100)) // 仅启用中的 webhook 入队，id 排在最后

	d.pump(context.Background())
	if ds.count() != 1 {
		t.Fatalf("启用中的 webhook 应被投递 1 次，得 %d", ds.count())
	}
	if len(d.wake) != 0 {
		t.Fatal("禁用 webhook 的积压不应触发自唤醒")
	}
	if _, total, _ := st.ListDeliveries(store.DeliveryFilter{WebhookID: disabled, Status: "pending"}); total != batchSize+10 {
		t.Fatalf("禁用 webhook 的投递应保持 pending，得 %d", total)
	}
}

func TestPostTest(t *testing.T) {
	_, d, ds := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(ds.handler))
	defer srv.Close()

	wh := &store.Webhook{URL: srv.URL, Secret: "s", Enabled: true}
	if code, err := d.PostTest(wh); err != nil || code != 200 || ds.count() != 1 {
		t.Fatalf("PostTest: %d %v n=%d", code, err, ds.count())
	}
}

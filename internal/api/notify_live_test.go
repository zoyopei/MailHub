package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	gosync "sync"
	"testing"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/mailclient"
	"mailhub/internal/store"
	"mailhub/internal/sync"
)

// mailbox 假传输：一个可增补的收件箱，游标语义与真实 IMAP 一致（只交付 UID > last_uid）。
type mailbox struct {
	mu   gosync.Mutex
	last uint32 // 当前最大 UID
}

func (m *mailbox) add() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last++
}

func (m *mailbox) Connect(context.Context, string, int, string, mailclient.Credentials) (string, error) {
	return "", nil
}

func (m *mailbox) FetchFolder(_ context.Context, _ string, cursor *store.FolderState, _ time.Time,
	handle func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	m.mu.Lock()
	last := m.last
	m.mu.Unlock()
	from := uint32(0)
	if cursor != nil {
		from = cursor.LastUID
	}
	for uid := from + 1; uid <= last; uid++ {
		if err := handle(mailclient.MsgKey{UIDValidity: 1, UID: uid}, time.Time{}, []byte(testMail)); err != nil {
			return mailclient.FetchResult{}, err
		}
	}
	return mailclient.FetchResult{}, nil
}
func (m *mailbox) SupportsIdle() bool                                { return false }
func (m *mailbox) Idle(context.Context, time.Duration) (bool, error) { return false, nil }
func (m *mailbox) Logout() error                                     { return nil }
func (m *mailbox) Close() error                                      { return nil }

const testMail = "From: a@b.c\r\nTo: x@y.z\r\nSubject: hi\r\nMessage-ID: <live@b.c>\r\n\r\nbody\r\n"

// runServer 与 testServer 相同，但传输是可增补的 mailbox，并且**运行** Manager——
// worker 真实存在，才能验证 API 的改动是否传到了运行中的 worker。
func runServer(t *testing.T, box *mailbox) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open("", dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := crypto.New(make([]byte, 32))
	cfg := &config.Config{
		APIToken: testToken, DataDir: dir, PollIntervalDefault: 60,
		ConnectStagger: time.Millisecond, MaxMessageSize: 1 << 20,
	}
	dial := func(*store.Account) mailclient.MailClient { return box }
	m := sync.NewManager(cfg, st, c, dial, nil)
	srv := httptest.NewServer(NewServer(cfg, st, c, m, dial).Handler())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped // worker 全部退出后才关库
		srv.Close()
		st.Close()
	})
	return srv, st
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时", what)
}

// 提醒开关对**运行中**的 worker 即时生效（API → Manager → worker 全链路；单个与批量 PATCH）。
// worker 持有的是启动时的账号快照：PATCH 只改库、不通知 worker 的话，开关要到服务重启
// 才生效——API 文档承诺的是即时生效。
func TestPatchNotifyReachesRunningWorker(t *testing.T) {
	box := &mailbox{last: 1} // 一封历史邮件
	srv, st := runServer(t, box)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rec, out := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"live@x.com","imap_host":"imap.x.com","password":"pw"}`, testToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %v", rec.Code, out)
	}
	id := int64(out["id"].(float64))
	messages := func() int {
		n, _ := st.CountMessages(store.MessageFilter{AccountID: id})
		return n
	}
	deliveries := func() int {
		_, total, _ := st.ListDeliveries(store.DeliveryFilter{})
		return total
	}
	// 到达一封新邮件并手动收取，等它入库（投递记录与邮件同一事务写入，入库即可断言）
	arrive := func() {
		t.Helper()
		before := messages()
		box.add()
		rec, out := do(t, srv, "POST", "/api/v1/accounts/"+itoa(id)+"/sync", "", testToken)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("sync: %d %v", rec.Code, out)
		}
		waitFor(t, "新邮件入库", func() bool { return messages() == before+1 })
	}
	setNotify := func(v bool) {
		t.Helper()
		rec, out := do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(id), fmt.Sprintf(`{"notify_enabled":%v}`, v), testToken)
		if rec.Code != http.StatusOK || out["notify_enabled"] != v {
			t.Fatalf("patch notify=%v: %d %v", v, rec.Code, out)
		}
	}
	setNotifyBatch := func(v bool) {
		t.Helper()
		rec, out := do(t, srv, "PATCH", "/api/v1/accounts/batch",
			fmt.Sprintf(`{"ids":[%d],"notify_enabled":%v}`, id, v), testToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch patch notify=%v: %d %v", v, rec.Code, out)
		}
	}

	waitFor(t, "首次同步", func() bool { return messages() == 1 })
	if n := deliveries(); n != 0 {
		t.Fatalf("历史邮件不应投递，得 %d", n)
	}

	arrive() // 提醒关闭（默认）
	if n := deliveries(); n != 0 {
		t.Fatalf("提醒关闭时不应投递，得 %d", n)
	}
	setNotify(true)
	arrive()
	if n := deliveries(); n != 1 {
		t.Fatalf("打开提醒后新邮件应投递（worker 未重启），得 %d 条投递记录", n)
	}
	setNotify(false)
	arrive()
	if n := deliveries(); n != 1 {
		t.Fatalf("关闭提醒后不应再投递，得 %d", n)
	}

	setNotifyBatch(true)
	arrive()
	if n := deliveries(); n != 2 {
		t.Fatalf("批量打开提醒后新邮件应投递，得 %d", n)
	}
	setNotifyBatch(false)
	arrive()
	if n := deliveries(); n != 2 {
		t.Fatalf("批量关闭提醒后不应再投递，得 %d", n)
	}
}

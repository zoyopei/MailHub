package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/mailclient"
	"mailhub/internal/store"
	"mailhub/internal/sync"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

const testToken = "test-token"

// fakeOKClient 所有连接都成功的假传输（供添加账号的验证路径）。
type fakeOKClient struct{}

func (fakeOKClient) Connect(context.Context, string, int, string, mailclient.Credentials) (string, error) {
	return "", nil
}
func (fakeOKClient) FetchFolder(context.Context, string, *store.FolderState, time.Time,
	func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	return mailclient.FetchResult{}, nil
}
func (fakeOKClient) SupportsIdle() bool { return false }
func (fakeOKClient) Idle(context.Context, time.Duration) (bool, error) {
	return false, nil
}
func (fakeOKClient) Logout() error { return nil }
func (fakeOKClient) Close() error  { return nil }

var lastTestServer *Server // 最近一次 testServer 构建的 Server（同包测试访问内部状态，如 hub）

func testServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open("", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, _ := crypto.New(make([]byte, 32))
	cfg := &config.Config{
		APIToken: testToken, DataDir: dir, PollIntervalDefault: 60,
		ConnectStagger: time.Millisecond, MaxMessageSize: 1 << 20,
	}
	fakeDial := func(*store.Account) mailclient.MailClient { return fakeOKClient{} }
	m := sync.NewManager(cfg, st, c, fakeDial, nil)
	apiSrv := NewServer(cfg, st, c, m, fakeDial)
	apiSrv.SetDeliverer(&fakeTester{}, nil) // webhook 测试事件假投递器
	lastTestServer = apiSrv
	srv := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(srv.Close)
	return srv, st, dir
}

func do(t *testing.T, srv *httptest.Server, method, path, body, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func TestHealthzNoAuth(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, out := do(t, srv, "GET", "/healthz", "", "")
	if rec.Code != 200 || out["status"] != "ok" {
		t.Fatalf("healthz: %d %v", rec.Code, out)
	}
}

func TestAuthRequired(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, _ := do(t, srv, "GET", "/api/v1/accounts", "", "")
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "GET", "/api/v1/accounts", "", "wrong")
	if rec.Code != 401 {
		t.Fatalf("错 token 应 401，得 %d", rec.Code)
	}
}

func TestAccountCreateVerifyAndList(t *testing.T) {
	srv, st, _ := testServer(t)
	body := `{"email":"me@example.com","imap_host":"imap.example.com","password":"pw"}`
	rec, out := do(t, srv, "POST", "/api/v1/accounts", body, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d %v", rec.Code, out)
	}
	// 不回显凭据
	if _, exists := out["credential_enc"]; exists {
		t.Error("响应不应包含凭据")
	}
	// 列表含统计字段
	rec, out = do(t, srv, "GET", "/api/v1/accounts", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	accounts := out["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	a := accounts[0].(map[string]any)
	for _, key := range []string{"status", "last_sync_at", "last_message_at", "message_count", "unread_count", "notify_enabled",
		"folders", "poll_interval", "username", "imap_port", "client_id", "auth_type"} {
		if _, ok := a[key]; !ok {
			t.Errorf("列表项缺少字段 %s", key)
		}
	}
	if a["notify_enabled"] != false {
		t.Errorf("提醒默认应关闭（DESIGN.md 5.1）")
	}
	// 可编辑字段回显当前值（凭据不回显）
	if fld, _ := a["folders"].([]any); len(fld) != 1 || fld[0] != "INBOX" {
		t.Errorf("folders 应回显 [INBOX]，得 %v", a["folders"])
	}
	if a["poll_interval"] != float64(60) {
		t.Errorf("poll_interval 应回显默认 60，得 %v", a["poll_interval"])
	}
	_ = st
}

func TestAccountCreateVerifyFails(t *testing.T) {
	srv, _, _ := testServer(t)
	// 该测试服务器的 dial 总是成功——用一个必失败路径：缺 host 且域名无预设
	body := `{"email":"me@nodomain.xyz","password":"pw"}`
	rec, out := do(t, srv, "POST", "/api/v1/accounts", body, testToken)
	if rec.Code != 400 {
		t.Fatalf("无预设域名缺 host 应 400，得 %d %v", rec.Code, out)
	}
}

func TestBatchImportTextPlain(t *testing.T) {
	srv, st, _ := testServer(t)
	// main.py 的 ----格式：第二段密码为占位符（不存储），Outlook 域名自动 oauth2 预设
	text := "a1@outlook.com----unused----cid-1----RT-1\n" +
		"\n" +
		"# 注释行\n" +
		"bad-line-without-separator\n" +
		"a2@hotmail.com----x----cid-2----RT-2\n" +
		"a1@outlook.com----x----cid-1----RT-1\n" // 重复邮箱

	req := httptest.NewRequest("POST", srv.URL+"/api/v1/accounts/batch", strings.NewReader(text))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)

	var out struct {
		Created int `json:"created"`
		Failed  []struct {
			Line  int    `json:"line"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("batch: %d %v", rec.Code, rec.Body.String())
	}
	if out.Created != 2 {
		t.Errorf("created = %d（应为 2：a1、a2）", out.Created)
	}
	if len(out.Failed) != 2 {
		t.Fatalf("failed = %+v（应为 2：坏行 + 重复）", out.Failed)
	}
	// Outlook 域名预设生效
	a, err := st.GetAccountByEmail("a1@outlook.com")
	if err != nil {
		t.Fatal(err)
	}
	if a.IMAPHost != "outlook.office365.com" || a.AuthType != "oauth2" {
		t.Errorf("预设未生效: %+v", a)
	}
	if a.Status != store.StatusPending {
		t.Errorf("批量导入应入库 pending，得 %s", a.Status)
	}
}

func TestPatchNotifyInstant(t *testing.T) {
	srv, st, _ := testServer(t)
	rec, _ := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@x.com","imap_host":"imap.x.com","password":"pw"}`, testToken)
	_ = rec
	list, _, _ := st.ListAccounts(store.AccountFilter{})
	id := list[0].ID

	// 行为开关即时生效
	rec, out := do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(id), `{"notify_enabled":true}`, testToken)
	if rec.Code != 200 || out["notify_enabled"] != true {
		t.Fatalf("patch: %d %v", rec.Code, out)
	}
}

// 标签：创建时写入、PATCH 替换与清空、列表 tag 过滤（OR）、/tags 引用数。
func TestAccountTags(t *testing.T) {
	srv, st, _ := testServer(t)
	mk := func(email string) int64 {
		rec, _ := do(t, srv, "POST", "/api/v1/accounts",
			`{"email":"`+email+`","imap_host":"imap.x.com","password":"pw"}`, testToken)
		if rec.Code != 201 {
			t.Fatalf("create %s: %d", email, rec.Code)
		}
		list, _, _ := st.ListAccounts(store.AccountFilter{})
		return list[len(list)-1].ID
	}
	a := mk("t-a@x.com")
	b := mk("t-b@x.com")

	// 创建后 PATCH 标签
	rec, out := do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(a), `{"tags":["重要","备用"]}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("patch tags: %d %v", rec.Code, out)
	}
	if tags, _ := out["tags"].([]any); len(tags) != 2 {
		t.Fatalf("响应应含 2 个标签: %v", out["tags"])
	}
	do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(b), `{"tags":["备用"]}`, testToken)

	// 列表过滤：单标签
	rec, out = do(t, srv, "GET", "/api/v1/accounts?tag="+urlQueryEscape("重要"), "", testToken)
	if rec.Code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("tag=重要 应命中 1: %d %v", rec.Code, out["total"])
	}
	// 多标签 OR
	rec, out = do(t, srv, "GET", "/api/v1/accounts?tag="+urlQueryEscape("重要")+"&tag="+urlQueryEscape("备用"), "", testToken)
	if rec.Code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("tag=重要,备用 应命中 2: %d %v", rec.Code, out["total"])
	}

	// /tags 列表（引用数）
	rec, out = do(t, srv, "GET", "/api/v1/tags", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("tags: %d", rec.Code)
	}
	tags := out["tags"].([]any)
	if len(tags) != 2 {
		t.Fatalf("应只有 2 个标签: %v", tags)
	}
	first := tags[0].(map[string]any)
	if first["name"] != "备用" || first["count"].(float64) != 2 {
		t.Fatalf("备用 应引用 2 次且排首: %v", first)
	}

	// 清空 + 孤儿回收
	do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(a), `{"tags":[]}`, testToken)
	rec, out = do(t, srv, "GET", "/api/v1/tags", "", testToken)
	tags = out["tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["name"] != "备用" {
		t.Fatalf("清空后应只剩 备用: %v", tags)
	}

	// 详情含空数组（非 null）
	rec, out = do(t, srv, "GET", "/api/v1/accounts/"+itoa(a), "", testToken)
	if out["tags"] == nil {
		t.Fatal("无标签应为 [] 而非 null")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestMessagesEndpoints(t *testing.T) {
	srv, st, dir := testServer(t)
	acct, _ := st.CreateAccount(&store.Account{
		Name: "m@x.com", Email: "m@x.com", IMAPHost: "h", Username: "m@x.com",
		AuthType: "password", CredentialEnc: []byte("e"),
	})
	// 直接写 .eml 文件并入库
	raw := "From: s@x.com\r\nTo: m@x.com\r\nSubject: sub\r\n\r\nbody\r\n"
	rel := "mails/1/2026-11/1-1.eml"
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(abs), 0o700)
	os.WriteFile(abs, []byte(raw), 0o600)

	msgID, err := st.IngestMessage(&store.NewMessage{
		AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 1,
		Subject: "sub", FromAddr: "s@x.com", BodyText: "body", Snippet: "body", RawPath: rel,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 列表
	rec, out := do(t, srv, "GET", "/api/v1/messages?limit=10", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}

	// 详情
	rec, out = do(t, srv, "GET", "/api/v1/messages/"+itoa(msgID), "", testToken)
	if rec.Code != 200 || out["body_text"] != "body" {
		t.Fatalf("detail: %d %v", rec.Code, out)
	}

	// 已读
	rec, _ = do(t, srv, "PATCH", "/api/v1/messages/"+itoa(msgID), `{"is_read":true}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("patch read: %d", rec.Code)
	}
	// 批量已读
	rec, out = do(t, srv, "POST", "/api/v1/messages/batch-read", `{"account_id":`+itoa(acct)+`}`, testToken)
	if rec.Code != 200 || out["updated"].(float64) < 0 {
		t.Fatalf("batch-read: %d %v", rec.Code, out)
	}

	// raw 下载
	req := httptest.NewRequest("GET", srv.URL+"/api/v1/messages/"+itoa(msgID)+"/raw", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec = httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("sub")) {
		t.Fatalf("raw: %d %q", rec.Code, rec.Body.String())
	}

	// 批量删除（含文件清理）
	rec, out = do(t, srv, "POST", "/api/v1/messages/batch-delete", `{"ids":[`+itoa(msgID)+`]}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("batch-delete: %d %v", rec.Code, out)
	}
	time.Sleep(100 * time.Millisecond) // 异步文件删除
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Errorf(".eml 应已删除: %v", err)
	}
	if _, err := st.GetMessage(msgID); err != store.ErrNotFound {
		t.Errorf("行应已删除: %v", err)
	}
}

func TestStatus(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, out := do(t, srv, "GET", "/api/v1/status", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	for _, key := range []string{"accounts_by_status", "messages_today", "deliveries_pending", "disk_usage_bytes"} {
		if _, ok := out[key]; !ok {
			t.Errorf("status 缺少 %s", key)
		}
	}
}

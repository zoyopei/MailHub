package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mailhub/internal/store"
)

func errCode(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		c, _ := e["code"].(string)
		return c
	}
	return ""
}

// 重复添加同一邮箱 → 409（不是 500，也不会再登一次远端邮箱做无谓的验证）。
func TestAccountCreateDuplicate(t *testing.T) {
	srv, st, _ := testServer(t)
	body := `{"email":"dup@x.com","imap_host":"imap.x.com","password":"pw"}`
	rec, out := do(t, srv, "POST", "/api/v1/accounts", body, testToken)
	if rec.Code != 201 {
		t.Fatalf("首次创建应 201，得 %d %v", rec.Code, out)
	}
	rec, out = do(t, srv, "POST", "/api/v1/accounts", body, testToken)
	if rec.Code != 409 || errCode(out) != "account_exists" {
		t.Fatalf("重复邮箱应 409 account_exists，得 %d %v", rec.Code, out)
	}
	list, total, _ := st.ListAccounts(store.AccountFilter{})
	if total != 1 || len(list) != 1 {
		t.Fatalf("不应产生重复账号，得 total=%d len=%d", total, len(list))
	}
}

// 删除不存在的账号 → 404（以前恒返回 200，脚本无从发现删错了）。
func TestAccountDeleteNotFound(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, _ := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"gone@x.com","imap_host":"imap.x.com","password":"pw"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d", rec.Code)
	}
	rec, out := do(t, srv, "DELETE", "/api/v1/accounts/1", "", testToken)
	if rec.Code != 200 || out["deleted"].(float64) != 1 {
		t.Fatalf("删除应 200，得 %d %v", rec.Code, out)
	}
	rec, out = do(t, srv, "DELETE", "/api/v1/accounts/1", "", testToken)
	if rec.Code != 404 || errCode(out) != "account_not_found" {
		t.Fatalf("再次删除应 404 account_not_found，得 %d %v", rec.Code, out)
	}
}

// PATCH 凭据字段传空串 → 400。照做会把凭据覆盖成空，账号随即 auth_failed；
// "不修改"必须用省略字段表达。
func TestPatchEmptyCredentialRejected(t *testing.T) {
	srv, st, _ := testServer(t)
	rec, _ := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@x.com","imap_host":"imap.x.com","password":"pw"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d", rec.Code)
	}
	list, _, _ := st.ListAccounts(store.AccountFilter{})
	id := list[0].ID
	before, _ := st.GetAccount(id)

	for _, f := range []string{"password", "refresh_token", "client_id"} {
		rec, out := do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(id), `{"`+f+`":""}`, testToken)
		if rec.Code != 400 || errCode(out) != "invalid_account" {
			t.Fatalf("%s=\"\" 应 400 invalid_account，得 %d %v", f, rec.Code, out)
		}
	}
	after, _ := st.GetAccount(id)
	if string(after.CredentialEnc) != string(before.CredentialEnc) || after.ClientID != before.ClientID {
		t.Fatal("被拒的请求不应改动凭据")
	}

	// 省略字段 = 不修改
	rec, out := do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(id), `{"name":"renamed"}`, testToken)
	if rec.Code != 200 || out["name"] != "renamed" {
		t.Fatalf("只改名应 200，得 %d %v", rec.Code, out)
	}
	after, _ = st.GetAccount(id)
	if string(after.CredentialEnc) != string(before.CredentialEnc) {
		t.Fatal("改名不应动凭据")
	}
}

// 自定义 webhook secret 有长度下限：太短可以被暴力猜出，猜中即可伪造"新邮件"事件。
func TestWebhookSecretMinimum(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, out := do(t, srv, "POST", "/api/v1/webhooks",
		`{"url":"https://hook.example/x","secret":"tooshort"}`, testToken)
	if rec.Code != 400 || errCode(out) != "weak_secret" {
		t.Fatalf("过短的 secret 应 400 weak_secret，得 %d %v", rec.Code, out)
	}

	ok := "0123456789abcdef"
	rec, out = do(t, srv, "POST", "/api/v1/webhooks",
		`{"url":"https://hook.example/x","secret":"`+ok+`"}`, testToken)
	if rec.Code != 201 || out["secret"] != ok {
		t.Fatalf("16 字符的 secret 应 201 且创建时回显一次，得 %d %v", rec.Code, out)
	}

	rec, out = do(t, srv, "POST", "/api/v1/webhooks", `{"url":"https://hook.example/y"}`, testToken)
	auto, _ := out["secret"].(string)
	if rec.Code != 201 || len(auto) < 16 {
		t.Fatalf("自动生成的 secret 应满足下限，得 %d len=%d", rec.Code, len(auto))
	}
}

// "今日收信"的日界由 day_start 决定（Web 端传浏览器本地零点）；缺省取服务器本地零点。
func TestStatusDayStart(t *testing.T) {
	srv, st, _ := testServer(t)
	acct, err := st.CreateAccount(&store.Account{
		Name: "s@x.com", Email: "s@x.com", IMAPHost: "h", Username: "s@x.com",
		AuthType: "password", CredentialEnc: []byte("e"),
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, ago := range []time.Duration{30 * time.Minute, 25 * time.Hour} {
		if _, err := st.IngestMessage(&store.NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1,
			UID: uint32(i + 1), ReceivedAt: now.Add(-ago), RawPath: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string) float64 {
		t.Helper()
		rec, out := do(t, srv, "GET", "/api/v1/status"+q, "", testToken)
		if rec.Code != 200 {
			t.Fatalf("status%s: %d %v", q, rec.Code, out)
		}
		n, _ := out["messages_today"].(float64)
		return n
	}
	at := func(t0 time.Time) string { return url.QueryEscape(t0.UTC().Format(time.RFC3339)) }

	// 带时区偏移的时间同样可解析（浏览器发来的是本地时间，如 UTC+8）
	z8 := time.FixedZone("UTC+8", 8*3600)
	off := func(t0 time.Time) string { return url.QueryEscape(t0.In(z8).Format(time.RFC3339)) }
	for _, c := range []struct {
		q    string
		want float64
	}{
		{"?day_start=" + at(now), 0},
		{"?day_start=" + at(now.Add(-time.Hour)), 1},
		{"?day_start=" + at(now.Add(-26*time.Hour)), 2},
		{"?day_start=" + off(now.Add(-time.Hour)), 1},
		{"?day_start=" + off(now.Add(-26*time.Hour)), 2},
	} {
		if n := count(c.q); n != c.want {
			t.Errorf("status%s 应计 %v 封，得 %v", c.q, c.want, n)
		}
	}

	// 缺省：服务器本地零点
	y, m, d := now.Date()
	want := 0.0
	if now.Add(-30 * time.Minute).After(time.Date(y, m, d, 0, 0, 0, 0, now.Location())) {
		want = 1
	}
	if n := count(""); n != want {
		t.Errorf("缺省 day_start 应计 %v 封，得 %v", want, n)
	}

	// 非法值 → 400
	rec, out := do(t, srv, "GET", "/api/v1/status?day_start=2026-11-24", "", testToken)
	if rec.Code != 400 || errCode(out) != "bad_request" {
		t.Fatalf("非法 day_start 应 400 bad_request，得 %d %v", rec.Code, out)
	}
}

// 安全响应头对所有响应生效：API、未认证的 401、Web 静态页、SSE 流。
func TestSecurityHeaders(t *testing.T) {
	srv, _, _ := testServer(t)
	check := func(name string, h http.Header) {
		t.Helper()
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s：缺 X-Content-Type-Options: nosniff", name)
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s：缺 X-Frame-Options: DENY", name)
		}
		csp := h.Get("Content-Security-Policy")
		for _, d := range []string{"object-src 'none'", "base-uri 'self'", "form-action 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, d) {
				t.Errorf("%s：CSP 应含 %s，得 %q", name, d, csp)
			}
		}
	}

	rec, _ := do(t, srv, "GET", "/api/v1/accounts", "", testToken)
	check("API", rec.Header())
	rec, _ = do(t, srv, "GET", "/api/v1/accounts", "", "")
	check("401", rec.Header())
	rec, _ = do(t, srv, "GET", "/healthz", "", "")
	check("healthz", rec.Header())

	rec, _ = do(t, srv, "GET", "/", "", "")
	if rec.Code != 200 {
		t.Fatalf("静态页: %d", rec.Code)
	}
	check("静态页", rec.Header())

	resp, err := http.Get(srv.URL + "/api/v1/events?token=" + testToken)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("SSE: %d", resp.StatusCode)
	}
	check("SSE", resp.Header)
}

// 服务停机：Hub.Close 结束全部 SSE 长连接（否则 http.Server.Shutdown 会一直等到超时），
// 之后的连接立即结束，重复调用与关闭后的广播都安全。
func TestHubCloseEndsSSE(t *testing.T) {
	srv, _, _ := testServer(t)
	hub := lastTestServer.hub

	open := func() io.ReadCloser {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/v1/events?token=" + testToken)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("SSE 建连: %d", resp.StatusCode)
		}
		return resp.Body
	}
	done := make(chan error, 3)
	read := func(b io.ReadCloser) { go func() { _, err := io.ReadAll(b); done <- err }() }

	first, second := open(), open()
	read(first)
	read(second)
	time.Sleep(100 * time.Millisecond) // 等两条连接订阅完成
	select {
	case err := <-done:
		t.Fatalf("Hub 关闭前 SSE 流不应结束（err=%v）", err)
	case <-time.After(300 * time.Millisecond):
	}

	hub.Close()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil && err != io.EOF {
				t.Fatalf("SSE 流应以正常结束的方式收尾，得 %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Hub.Close 后已有的 SSE 流应在 3 秒内结束")
		}
	}

	hub.Close()           // 幂等
	hub.NotifyEvent("{}") // 订阅表已清空：不会向已关闭的 chan 发送

	read(open()) // 关闭后新建的连接：立即结束，不留长连接拖住停机
	select {
	case err := <-done:
		if err != nil && err != io.EOF {
			t.Fatalf("关闭后新建的 SSE 流应正常结束，得 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Hub 关闭后新建的 SSE 流应立即结束")
	}

	hub.mu.Lock()
	n := len(hub.subs)
	hub.mu.Unlock()
	if n != 0 {
		t.Fatalf("关闭后不应残留订阅，得 %d", n)
	}
}

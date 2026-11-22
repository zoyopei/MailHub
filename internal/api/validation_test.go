package api

import (
	"strings"
	"testing"
	"time"

	"mailhub/internal/store"
)

// poll_interval 入口校验：<5 秒（含 0 以外的非正值）一律 400，防 worker 空转/panic。
func TestAccountPollIntervalValidation(t *testing.T) {
	srv, st, _ := testServer(t)
	rec, out := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@example.com","imap_host":"imap.example.com","password":"pw","poll_interval":-1}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("负 poll_interval 创建应 400，得 %d %v", rec.Code, out)
	}
	rec, out = do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@example.com","imap_host":"imap.example.com","password":"pw","imap_port":70000}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("越界 imap_port 应 400，得 %d %v", rec.Code, out)
	}

	rec, out = do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@example.com","imap_host":"imap.example.com","password":"pw"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d %v", rec.Code, out)
	}
	id := int64(out["id"].(float64))
	if id <= 0 {
		t.Fatalf("POST /accounts 应返回真实账号 id，得 %v", out["id"])
	}
	acct, err := st.GetAccountByEmail("p@example.com")
	if err != nil || acct.ID != id {
		t.Fatalf("返回 id=%d 与库中不一致: %v %+v", id, err, acct)
	}
	for _, v := range []string{"0", "-10", "4"} {
		rec, out = do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(id), `{"poll_interval":`+v+`}`, testToken)
		if rec.Code != 400 {
			t.Fatalf("PATCH poll_interval=%s 应 400，得 %d %v", v, rec.Code, out)
		}
	}
	rec, _ = do(t, srv, "PATCH", "/api/v1/accounts/"+itoa(id), `{"poll_interval":5}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("PATCH poll_interval=5 应 200，得 %d", rec.Code)
	}
}

func TestWebhookURLValidation(t *testing.T) {
	srv, _, _ := testServer(t)
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "not a url", "http://"} {
		rec, out := do(t, srv, "POST", "/api/v1/webhooks", `{"url":"`+u+`"}`, testToken)
		if rec.Code != 400 {
			t.Fatalf("url=%q 应 400，得 %d %v", u, rec.Code, out)
		}
	}
	rec, out := do(t, srv, "POST", "/api/v1/webhooks", `{"url":"https://example.com/hook"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("合法 url 应 201，得 %d %v", rec.Code, out)
	}
	id := int64(out["id"].(float64))
	rec, _ = do(t, srv, "PATCH", "/api/v1/webhooks/"+itoa(id), `{"url":"ftp://x/y"}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("PATCH 非法 url 应 400，得 %d", rec.Code)
	}
}

func TestNosniffHeader(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, _ := do(t, srv, "GET", "/healthz", "", "")
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("应设置 X-Content-Type-Options: nosniff")
	}
}

func TestContentDispositionRFC5987(t *testing.T) {
	if got := contentDisposition("report.pdf"); got != `attachment; filename="report.pdf"` {
		t.Fatalf("纯 ASCII: %s", got)
	}
	got := contentDisposition("月度报告 2026.pdf")
	if !strings.Contains(got, `filename="____ 2026.pdf"`) ||
		!strings.Contains(got, `filename*=UTF-8''%E6%9C%88%E5%BA%A6%E6%8A%A5%E5%91%8A%202026.pdf`) {
		t.Fatalf("中文文件名编码异常: %s", got)
	}
	if got := contentDisposition("bad\r\nX-Injected: 1.txt"); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("控制字符未剔除: %q", got)
	}
}

func TestMessageListNoTotal(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, out := do(t, srv, "GET", "/api/v1/messages?limit=10", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	if _, ok := out["total"]; !ok {
		t.Fatal("默认请求应包含 total")
	}
	rec, out = do(t, srv, "GET", "/api/v1/messages?limit=10&no_total=1", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("list no_total: %d", rec.Code)
	}
	if _, ok := out["total"]; ok {
		t.Fatal("no_total=1 时不应包含 total")
	}
}

// 排序与翻页回归：sort=account&order=desc 按账号倒序、sort=time&offset=1 正确偏移、
// DELETE 不存在的邮件返回 404。
func TestMessageSortOffsetAndDelete404(t *testing.T) {
	srv, st, _ := testServer(t)
	a1, _ := st.CreateAccount(&store.Account{
		Name: "alpha", Email: "alpha@x.com", IMAPHost: "h", Username: "alpha@x.com",
		AuthType: "password", CredentialEnc: []byte("e"),
	})
	a2, _ := st.CreateAccount(&store.Account{
		Name: "zeta", Email: "zeta@x.com", IMAPHost: "h", Username: "zeta@x.com",
		AuthType: "password", CredentialEnc: []byte("e"),
	})
	t1 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	_, _ = st.IngestMessage(&store.NewMessage{
		AccountID: a1, Folder: "INBOX", UIDValidity: 1, UID: 1, Subject: "first", ReceivedAt: t1, RawPath: "mails/1/a.eml",
	})
	_, _ = st.IngestMessage(&store.NewMessage{
		AccountID: a2, Folder: "INBOX", UIDValidity: 1, UID: 1, Subject: "second", ReceivedAt: t2, RawPath: "mails/2/b.eml",
	})

	// sort=account&order=desc 应返回 zeta 在前
	rec, out := do(t, srv, "GET", "/api/v1/messages?sort=account&order=desc", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("sort account desc: %d", rec.Code)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 2 || int64(msgs[0].(map[string]any)["account_id"].(float64)) != a2 {
		t.Fatalf("sort=account&order=desc 应以 zeta(%d) 开头，得 %v", a2, msgs)
	}

	// sort=time&order=desc&limit=1&offset=1 应跳过最新的 second，返回 first
	rec, out = do(t, srv, "GET", "/api/v1/messages?sort=time&order=desc&limit=1&offset=1", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("sort time offset: %d", rec.Code)
	}
	msgs = out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["subject"] != "first" {
		t.Fatalf("sort=time offset=1 应返回 first，得 %v", msgs)
	}

	// DELETE 不存在的邮件返回 404
	rec, _ = do(t, srv, "DELETE", "/api/v1/messages/999999", "", testToken)
	if rec.Code != 404 {
		t.Fatalf("删除不存在邮件应 404，得 %d", rec.Code)
	}
}

// Web 新建"全部账号"Webhook 时传入 account_ids: []，必须存为 NULL（匹配全部账号）而非 "[]"。
func TestWebhookCreateEmptyAccountIDsMatchesAll(t *testing.T) {
	srv, st, _ := testServer(t)
	rec, out := do(t, srv, "POST", "/api/v1/webhooks",
		`{"url":"https://example.com/hook","account_ids":[]}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create webhook: %d %v", rec.Code, out)
	}
	if out["account_ids"] != nil {
		t.Fatalf("空 account_ids 创建后应返回 null（全部账号），得 %v", out["account_ids"])
	}
	acct, _ := st.CreateAccount(&store.Account{
		Name: "a@x.com", Email: "a@x.com", IMAPHost: "h", Username: "a@x.com",
		AuthType: "password", CredentialEnc: []byte("e"), NotifyEnabled: true,
	})
	if _, err := st.IngestMessage(&store.NewMessage{
		AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 1,
		Subject: "s", RawPath: "mails/1/1.eml", NotifyEnabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := st.ListDeliveries(store.DeliveryFilter{}); total != 1 {
		t.Fatalf("全部账号 Webhook 应匹配并生成 1 条投递记录，得 %d", total)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reauthFakeServer 模拟 Microsoft 令牌端点：code "good" 换新 RT，"expired" 过期，其他 invalid_request。
func reauthFakeServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("code") {
		case "good":
			w.Write([]byte(`{"access_token":"AT","refresh_token":"RT-fresh","expires_in":3600}`))
		case "expired":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
		default:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_request","error_description":"AADSTS9000: detail"}`))
		}
	}))
}

// POST /accounts/{id}/reauth：授权码（或完整跳转 URL）换新令牌 → 凭据已更新（密文变化）
// → worker 重启入队 → 响应账号。
func TestAccountReauth(t *testing.T) {
	srv, st, _ := testServer(t)
	lastTestServer.reauth.Endpoint = reauthFakeServer().URL

	body := `{"email":"me@outlook.com","auth_type":"oauth2","client_id":"cid","refresh_token":"RT-old"}`
	rec, _ := do(t, srv, "POST", "/api/v1/accounts", body, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d", rec.Code)
	}
	id := int64(1)
	before, _ := st.GetAccount(id)

	// 粘贴完整跳转 URL（服务端提取 code）
	jump := `https://login.microsoftonline.com/common/oauth2/nativeclient?code=good&state=1`
	rec, out := do(t, srv, "POST", "/api/v1/accounts/1/reauth",
		`{"code":"`+jump+`"}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("reauth: %d %v", rec.Code, out)
	}
	after, _ := st.GetAccount(id)
	if string(before.CredentialEnc) == string(after.CredentialEnc) {
		t.Fatal("凭据密文应被替换")
	}
	if !strings.Contains(rec.Body.String(), `"email":"me@outlook.com"`) {
		t.Fatalf("响应应含账号信息: %s", rec.Body.String())
	}

	// code 过期：400 + 提示重走授权；凭据不动
	rec, out = do(t, srv, "POST", "/api/v1/accounts/1/reauth", `{"code":"expired"}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("过期 code 应 400，得 %d", rec.Code)
	}
	if msg := out["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "授权码无效或已过期") {
		t.Fatalf("错误信息应提示重新授权，得 %q", msg)
	}
	kept, _ := st.GetAccount(id)
	if string(kept.CredentialEnc) != string(after.CredentialEnc) {
		t.Fatal("失败路径不应改动凭据")
	}

	// 密码认证账号不支持 / 空 code / 无 token
	rec, _ = do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@x.com","imap_host":"imap.x.com","password":"pw"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create password acct: %d", rec.Code)
	}
	rec, _ = do(t, srv, "POST", "/api/v1/accounts/2/reauth", `{"code":"good"}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("非 oauth2 应 400，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "POST", "/api/v1/accounts/1/reauth", `{}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("空 code 应 400，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "POST", "/api/v1/accounts/1/reauth", `{"code":"good"}`, "")
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401，得 %d", rec.Code)
	}
}

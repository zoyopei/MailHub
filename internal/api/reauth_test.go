package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeIDToken 生成结构合法的未签名 JWT（服务端只解析载荷，不验签）。
func fakeIDToken(claims map[string]any) string {
	b64 := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return b64(map[string]any{"alg": "RS256"}) + "." + b64(claims) + ".sig"
}

// reauthFakeServer 模拟 Microsoft 令牌端点：
//   - good：换新 RT，id_token 的登录账号是 me@outlook.com（目标账号，大小写不同）；
//   - wrong：换新 RT，但登录的是别的账号（浏览器里登着别人的 Microsoft 账号时会发生）；
//   - noid：响应不带 id_token；
//   - expired：授权码过期；其他：invalid_request。
func reauthFakeServer() *httptest.Server {
	exp := time.Now().Add(time.Hour).Unix()
	idMe := fakeIDToken(map[string]any{"preferred_username": "Me@Outlook.com", "exp": exp})
	idOther := fakeIDToken(map[string]any{"preferred_username": "someone-else@outlook.com", "exp": exp})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("code") {
		case "good":
			fmt.Fprintf(w, `{"access_token":"AT","refresh_token":"RT-fresh","id_token":%q,"expires_in":3600}`, idMe)
		case "wrong":
			fmt.Fprintf(w, `{"access_token":"AT","refresh_token":"RT-other","id_token":%q,"expires_in":3600}`, idOther)
		case "noid":
			w.Write([]byte(`{"access_token":"AT","refresh_token":"RT-noid","expires_in":3600}`))
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

// 重新获取令牌必须校验登录身份：登成了别的账号时拒绝，凭据保持原样。
// 不拦的话，别人的令牌会被写进这个账号——本账号从此 auth_failed，原令牌可能已因轮换作废。
func TestAccountReauthRejectsWrongIdentity(t *testing.T) {
	srv, st, _ := testServer(t)
	lastTestServer.reauth.Endpoint = reauthFakeServer().URL

	rec, _ := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"me@outlook.com","auth_type":"oauth2","client_id":"cid","refresh_token":"RT-old"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d", rec.Code)
	}
	id := int64(1)
	before, _ := st.GetAccount(id)

	for _, c := range []struct {
		code, errCode, hint string
	}{
		{"wrong", "account_mismatch", "s***@outlook.com"}, // 登成了别的账号（邮箱打码后）
		{"noid", "identity_unverified", "无法确认登录的账号"},      // 拿不到身份信息：同样拒绝
	} {
		rec, out := do(t, srv, "POST", "/api/v1/accounts/1/reauth",
			`{"code":"`+c.code+`"}`, testToken)
		if rec.Code != 400 {
			t.Fatalf("code=%s 应 400，得 %d %v", c.code, rec.Code, out)
		}
		errObj := out["error"].(map[string]any)
		if errObj["code"] != c.errCode {
			t.Fatalf("code=%s 的错误码应为 %s，得 %v", c.code, c.errCode, errObj["code"])
		}
		msg := errObj["message"].(string)
		if !strings.Contains(msg, "凭据未修改") || !strings.Contains(msg, c.hint) {
			t.Fatalf("code=%s 的错误信息应含「%s」与「凭据未修改」，得 %q", c.code, c.hint, msg)
		}
		// 登错的邮箱要打码：足以区分账号，又不把完整地址写进响应
		if strings.Contains(msg, "someone-else@") {
			t.Fatalf("code=%s 的错误信息不应出现登错账号的完整地址，得 %q", c.code, msg)
		}
		cur, _ := st.GetAccount(id)
		if string(cur.CredentialEnc) != string(before.CredentialEnc) {
			t.Fatalf("code=%s 被拒后凭据不应改变", c.code)
		}
	}
}

// GET /accounts/{id}/reauth-url：授权页地址由服务端生成，带 login_hint 与 openid，
// 且 scope 与兑换端点一致（不一致的话令牌端点不返回 id_token）。
func TestAccountReauthURL(t *testing.T) {
	srv, _, _ := testServer(t)
	for i, a := range []struct {
		email, transport string
		wantScope        string
	}{
		{"g@outlook.com", "graph", "https://graph.microsoft.com/.default offline_access openid"},
		{"i@hotmail.com", "imap", "https://outlook.office365.com/IMAP.AccessAsUser.All offline_access openid"},
	} {
		rec, _ := do(t, srv, "POST", "/api/v1/accounts",
			fmt.Sprintf(`{"email":%q,"auth_type":"oauth2","client_id":"cid-%d","refresh_token":"RT","transport":%q}`,
				a.email, i, a.transport), testToken)
		if rec.Code != 201 {
			t.Fatalf("create %s: %d", a.email, rec.Code)
		}
		rec, out := do(t, srv, "GET", fmt.Sprintf("/api/v1/accounts/%d/reauth-url", i+1), "", testToken)
		if rec.Code != 200 {
			t.Fatalf("reauth-url %s: %d %v", a.email, rec.Code, out)
		}
		raw, _ := out["url"].(string)
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("授权地址无法解析: %q", raw)
		}
		q := u.Query()
		if q.Get("login_hint") != a.email {
			t.Errorf("%s：授权页应带 login_hint=%s，得 %q", a.email, a.email, q.Get("login_hint"))
		}
		if q.Get("scope") != a.wantScope {
			t.Errorf("%s：scope 应为 %q，得 %q", a.email, a.wantScope, q.Get("scope"))
		}
		if q.Get("client_id") != fmt.Sprintf("cid-%d", i) || q.Get("response_type") != "code" {
			t.Errorf("%s：client_id / response_type 异常: %s", a.email, raw)
		}
	}

	// 密码账号不支持；不存在 404；无 token 401
	rec, _ := do(t, srv, "POST", "/api/v1/accounts",
		`{"email":"p@x.com","imap_host":"imap.x.com","password":"pw"}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create password acct: %d", rec.Code)
	}
	rec, _ = do(t, srv, "GET", "/api/v1/accounts/3/reauth-url", "", testToken)
	if rec.Code != 400 {
		t.Fatalf("非 oauth2 应 400，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "GET", "/api/v1/accounts/999/reauth-url", "", testToken)
	if rec.Code != 404 {
		t.Fatalf("不存在的账号应 404，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "GET", "/api/v1/accounts/1/reauth-url", "", "")
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401，得 %d", rec.Code)
	}
}

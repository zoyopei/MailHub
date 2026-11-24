package mstoken

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTokenServer 模拟 Microsoft 令牌端点（TECH.md §4）。
type fakeTokenServer struct {
	calls    atomic.Int64
	respCode int
	resp     map[string]any
}

func (f *fakeTokenServer) handler(w http.ResponseWriter, _ *http.Request) {
	f.calls.Add(1)
	w.Header().Set("Content-Type", "application/json")
	code := f.respCode
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	b, _ := json.Marshal(f.resp)
	w.Write(b)
}

// 授权码流：AuthorizeURL 参数、ExtractAuthCode 解析、FetchByAuthCode 兑换与错误映射。
func TestFetchByAuthCode(t *testing.T) {
	var gotForm map[string][]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("code") {
		case "good":
			fmt.Fprintf(w, `{"access_token":"AT","refresh_token":"RT-fresh","id_token":%q,"expires_in":3600}`,
				testIDToken(t, map[string]any{"preferred_username": "me@outlook.com"}))
		case "expired":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"code expired"}`))
		default:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_request","error_description":"AADSTS9000: detail"}`))
		}
	}))
	defer ts.Close()
	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	ctx := context.Background()

	rt, idt, err := tc.FetchByAuthCode(ctx, "good", "cid", MSNativeRedirect, GraphScope)
	if err != nil || rt != "RT-fresh" {
		t.Fatalf("成功路径: %v %q", err, rt)
	}
	if id, err := IdentityFromIDToken(idt); err != nil || !id.Matches("me@outlook.com") {
		t.Fatalf("应返回可解析、身份匹配的 id_token: %+v %v", id, err)
	}
	if gotForm["grant_type"][0] != "authorization_code" || gotForm["client_id"][0] != "cid" {
		t.Fatalf("表单字段异常: %v", gotForm)
	}
	// 兑换请求的 scope 必须含 openid，且与授权请求（AuthorizeURL）一致，否则端点不返回 id_token
	wantScope := gotForm["scope"][0]
	au, _ := url.Parse(AuthorizeURL("cid", MSNativeRedirect, GraphScope, ""))
	if au.Query().Get("scope") != wantScope || !strings.Contains(wantScope, "openid") ||
		!strings.Contains(wantScope, "https://graph.microsoft.com/.default") {
		t.Fatalf("授权与兑换的 scope 应一致且含 openid 与原传输 scope: 授权=%q 兑换=%q",
			au.Query().Get("scope"), wantScope)
	}

	_, _, err = tc.FetchByAuthCode(ctx, "expired", "cid", MSNativeRedirect, GraphScope)
	if err == nil || !strings.Contains(err.Error(), "重新打开授权页") {
		t.Fatalf("code 过期应提示重走流程，得 %v", err)
	}
	_, _, err = tc.FetchByAuthCode(ctx, "other", "cid", MSNativeRedirect, GraphScope)
	if err == nil || !strings.Contains(err.Error(), "AADSTS9000") {
		t.Fatalf("其他错误应附诊断码，得 %v", err)
	}
}

// testIDToken 生成结构合法的未签名 JWT（身份校验不验签，只解析载荷）。
func testIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	b64 := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return b64(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + b64(claims) + ".c2ln"
}

// 授权页地址：带 login_hint（预填目标邮箱）与 openid；不重复追加。
func TestAuthorizeURL(t *testing.T) {
	au := AuthorizeURL("cid", MSNativeRedirect, IMAPScope, "Me@Outlook.com")
	u, err := url.Parse(au)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "login.microsoftonline.com" ||
		u.Path != "/common/oauth2/v2.0/authorize" {
		t.Fatalf("授权端点异常: %s", au)
	}
	q := u.Query()
	if q.Get("login_hint") != "Me@Outlook.com" {
		t.Fatalf("应带 login_hint，得 %q", q.Get("login_hint"))
	}
	if q.Get("scope") != IMAPScope+" openid" {
		t.Fatalf("scope 应为传输 scope + openid，得 %q", q.Get("scope"))
	}
	for k, want := range map[string]string{
		"client_id": "cid", "response_type": "code",
		"redirect_uri": MSNativeRedirect, "response_mode": "query",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q，应为 %q", k, q.Get(k), want)
		}
	}
	// 不带 login_hint 时不出现该参数
	if q2, _ := url.Parse(AuthorizeURL("cid", MSNativeRedirect, GraphScope, "")); q2.Query().Has("login_hint") {
		t.Fatalf("空 login_hint 不应出现该参数: %s", q2)
	}
	// scope 已含 openid 时不重复
	if s := withOpenIDScope("a b openid"); s != "a b openid" {
		t.Fatalf("不应重复追加 openid，得 %q", s)
	}

	// 跳转 URL 提取 code；纯码串原样返回
	if c := ExtractAuthCode(MSNativeRedirect + "?code=M.C5_xx&state=1"); c != "M.C5_xx" {
		t.Fatalf("URL 提取 code 失败: %q", c)
	}
	if c := ExtractAuthCode("  M.C5_plain  "); c != "M.C5_plain" {
		t.Fatalf("纯码串应原样返回: %q", c)
	}
}

// id_token 解析与身份比对：这是"登错账号不落库"的依据。
func TestIdentityFromIDToken(t *testing.T) {
	id, err := IdentityFromIDToken(testIDToken(t, map[string]any{
		"preferred_username": "Me@Outlook.com", "email": "alias@outlook.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	// 任一声明匹配即可，邮箱不区分大小写
	for _, ok := range []struct {
		target string
		want   bool
	}{
		{"me@outlook.com", true}, {"ME@OUTLOOK.COM", true}, {"alias@outlook.com", true},
		{"other@outlook.com", false}, {"", false}, {"me@outlook.com.cn", false},
	} {
		if got := id.Matches(ok.target); got != ok.want {
			t.Errorf("Matches(%q) = %v，应为 %v", ok.target, got, ok.want)
		}
	}
	if id.String() != "Me@Outlook.com" {
		t.Fatalf("展示身份应优先登录名，得 %q", id.String())
	}

	// 只有 email 声明
	id, err = IdentityFromIDToken(testIDToken(t, map[string]any{"email": "only@x.com"}))
	if err != nil || id.String() != "only@x.com" || !id.Matches("only@x.com") {
		t.Fatalf("仅有 email 声明时应可解析并匹配: %+v %v", id, err)
	}

	// 各种残缺输入都要报错，不能"解析不出来就算匹配"
	bad := map[string]string{
		"空串":         "",
		"非 JWT":      "not-a-jwt",
		"缺签名段":       "a.b",
		"载荷非 base64": "x.!!!.c2ln",
		"载荷非 JSON":   "x." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c2ln",
		"无身份声明":      testIDToken(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()}),
		"已过期": testIDToken(t, map[string]any{
			"preferred_username": "me@outlook.com", "exp": time.Now().Add(-time.Hour).Unix()}),
	}
	for name, tok := range bad {
		if _, err := IdentityFromIDToken(tok); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

func TestTokenCacheRotationAndCache(t *testing.T) {
	srv := &fakeTokenServer{resp: map[string]any{
		"access_token": "AT-1", "refresh_token": "RT-new", "expires_in": 3600,
	}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	ctx := context.Background()

	// 首次：返回轮换令牌
	access, rotated, err := tc.Token(ctx, IMAPScope, "a@x.com", "cid", "RT-old")
	if err != nil || access != "AT-1" || rotated != "RT-new" {
		t.Fatalf("首次: %q %q %v", access, rotated, err)
	}
	if srv.calls.Load() != 1 {
		t.Fatalf("请求数 = %d", srv.calls.Load())
	}

	// 用已持久化的轮换令牌二次调用：命中缓存，不再请求
	access, rotated, err = tc.Token(ctx, IMAPScope, "a@x.com", "cid", "RT-new")
	if err != nil || access != "AT-1" || rotated != "" {
		t.Fatalf("缓存: %q %q %v", access, rotated, err)
	}
	if srv.calls.Load() != 1 {
		t.Fatalf("缓存命中不应再请求，calls=%d", srv.calls.Load())
	}

	// 全新 refresh_token（外部更新）：缓存未命中，重新请求；端点未轮换则 rotated 为空
	srv.resp = map[string]any{"access_token": "AT-2", "expires_in": 3600} // 未轮换
	access, rotated, err = tc.Token(ctx, IMAPScope, "a@x.com", "cid", "RT-new2")
	if err != nil || access != "AT-2" || rotated != "" {
		t.Fatalf("新 refresh: %q %q %v", access, rotated, err)
	}
	if srv.calls.Load() != 2 {
		t.Fatalf("calls=%d", srv.calls.Load())
	}
}

// ForceToken：丢弃缓存重新换取（缓存里的令牌明明没过期也一样）；刷新令牌优先用缓存里记的
// 最新值（上次轮换出的那一个），不用调用方手里可能已过时的旧值；轮换照常返回。
func TestTokenCacheForceToken(t *testing.T) {
	calls := make(chan string, 4) // 每次请求携带的 refresh_token
	var n atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		calls <- r.PostForm.Get("refresh_token")
		// 每次刷新都轮换：RT-1 → RT-2 → RT-3 …
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"AT-%d","refresh_token":"RT-%d","expires_in":3600}`, n.Add(1), n.Load()+1)
	}))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	ctx := context.Background()

	access, rotated, err := tc.Token(ctx, GraphScope, "a@x.com", "cid", "RT-1")
	if err != nil || access != "AT-1" || rotated != "RT-2" {
		t.Fatalf("首次: %q %q %v", access, rotated, err)
	}
	// 缓存命中（没过期）：普通 Token 不再请求
	if access, _, err = tc.Token(ctx, GraphScope, "a@x.com", "cid", "RT-2"); err != nil || access != "AT-1" {
		t.Fatalf("缓存: %q %v", access, err)
	}
	if got := drain(calls); len(got) != 1 || got[0] != "RT-1" {
		t.Fatalf("缓存命中不应再请求，实际请求携带 %v", got)
	}

	// 强制刷新：缓存没过期也重新换取；用的是缓存里最新的 RT-2，不是调用方手里的旧值
	access, rotated, err = tc.ForceToken(ctx, GraphScope, "a@x.com", "cid", "RT-1")
	if err != nil || access != "AT-2" || rotated != "RT-3" {
		t.Fatalf("强制刷新: %q %q %v", access, rotated, err)
	}
	if got := drain(calls); len(got) != 1 || got[0] != "RT-2" {
		t.Fatalf("强制刷新应携带缓存里最新的 refresh_token，实际 %v", got)
	}
	// 刷新结果进了缓存：之后用最新令牌的普通 Token 直接命中
	if access, _, err = tc.Token(ctx, GraphScope, "a@x.com", "cid", "RT-3"); err != nil || access != "AT-2" {
		t.Fatalf("强制刷新后的缓存: %q %v", access, err)
	}
	if got := drain(calls); len(got) != 0 {
		t.Fatalf("不应有额外请求，实际 %v", got)
	}
}

// 强制刷新被明确拒绝（invalid_grant）：返回 ErrReauth；其他账号的缓存项不受影响。
func TestTokenCacheForceTokenInvalidGrant(t *testing.T) {
	srv := &fakeTokenServer{respCode: 400, resp: map[string]any{"error": "invalid_grant"}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	_, _, err := tc.ForceToken(context.Background(), IMAPScope, "a@x.com", "cid", "RT-revoked")
	if !errors.Is(err, ErrReauth) {
		t.Fatalf("invalid_grant 应返回 ErrReauth，得 %v", err)
	}
}

// drain 取走通道里现有的全部值。
func drain(ch chan string) []string {
	var out []string
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		default:
			return out
		}
	}
}

// 不同 scope 是不同缓存槽：IMAP 与 Graph 令牌互不串用（main.py 实测两 audience 不通用）。
func TestTokenCacheScopeIsolation(t *testing.T) {
	var gotScope atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotScope.Store(r.Form.Get("scope"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"AT-x","expires_in":3600}`))
	}))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	ctx := context.Background()

	if _, _, err := tc.Token(ctx, IMAPScope, "a@x.com", "cid", "RT"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tc.Token(ctx, GraphScope, "a@x.com", "cid", "RT"); err != nil {
		t.Fatal(err)
	}
	if gotScope.Load() != GraphScope {
		t.Fatalf("第二次请求 scope = %v", gotScope.Load())
	}
}

func TestTokenCacheInvalidGrant(t *testing.T) {
	srv := &fakeTokenServer{respCode: 400,
		resp: map[string]any{"error": "invalid_grant"}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	_, _, err := tc.Token(context.Background(), IMAPScope, "a@x.com", "cid", "RT-bad")
	if !errors.Is(err, ErrReauth) {
		t.Fatalf("invalid_grant 应返回 ErrReauth，得 %v", err)
	}
}

func TestTokenCacheInvalidScope(t *testing.T) {
	srv := &fakeTokenServer{respCode: 400,
		resp: map[string]any{"error": "invalid_scope"}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	_, _, err := tc.Token(context.Background(), IMAPScope, "a@x.com", "cid", "RT-x")
	if err == nil || errors.Is(err, ErrReauth) {
		t.Fatalf("invalid_scope 应返回普通错误（提示补权限），得 %v", err)
	}
}

func TestTokenCache429(t *testing.T) {
	srv := &fakeTokenServer{respCode: 429, resp: map[string]any{"error": "temporarily_unavailable"}}
	ts := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL
	_, _, err := tc.Token(context.Background(), GraphScope, "a@x.com", "cid", "RT-x")
	if err == nil || errors.Is(err, ErrReauth) {
		t.Fatalf("429 应返回普通错误（提示补权限），得 %v", err)
	}
}

// 账号 A 刷新卡住时，账号 B 必须能并发完成刷新且不受阻塞（全局锁不得跨 HTTP I/O）。
func TestTokenCacheConcurrentRefresh(t *testing.T) {
	blockA := make(chan struct{})
	aStarted := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") == "RT-A" {
			select {
			case aStarted <- struct{}{}:
			default:
			}
			<-blockA
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"AT-ok","expires_in":3600}`))
	}))
	defer ts.Close()

	tc := NewTokenCache()
	tc.Endpoint = ts.URL

	aDone := make(chan error, 1)
	go func() {
		_, _, err := tc.Token(context.Background(), GraphScope, "a@x.com", "cid", "RT-A")
		aDone <- err
	}()
	<-aStarted // 确认 A 已进入 HTTP 请求

	bDone := make(chan error, 1)
	go func() {
		_, _, err := tc.Token(context.Background(), GraphScope, "b@x.com", "cid", "RT-B")
		bDone <- err
	}()
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("账号 B 刷新失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(blockA)
		t.Fatal("账号 B 被账号 A 的慢请求阻塞")
	}
	close(blockA)
	if err := <-aDone; err != nil {
		t.Fatalf("账号 A 刷新失败: %v", err)
	}
}

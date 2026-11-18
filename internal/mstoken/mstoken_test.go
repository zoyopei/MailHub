package mstoken

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
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
	au := AuthorizeURL("cid", MSNativeRedirect, GraphScope)
	if !strings.HasPrefix(au, MSAuthorizeEndpoint+"?") ||
		!strings.Contains(au, "client_id=cid") ||
		!strings.Contains(au, "response_type=code") ||
		!strings.Contains(au, url.QueryEscape(MSNativeRedirect)) {
		t.Fatalf("AuthorizeURL 异常: %s", au)
	}

	if c := ExtractAuthCode(MSNativeRedirect + "?code=M.C5_xx&state=1"); c != "M.C5_xx" {
		t.Fatalf("URL 提取 code 失败: %q", c)
	}
	if c := ExtractAuthCode("  M.C5_plain  "); c != "M.C5_plain" {
		t.Fatalf("纯码串应原样返回: %q", c)
	}

	var gotForm map[string][]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("code") {
		case "good":
			w.Write([]byte(`{"access_token":"AT","refresh_token":"RT-fresh","expires_in":3600}`))
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

	rt, err := tc.FetchByAuthCode(ctx, "good", "cid", MSNativeRedirect, GraphScope)
	if err != nil || rt != "RT-fresh" {
		t.Fatalf("成功路径: %v %q", err, rt)
	}
	if gotForm["grant_type"][0] != "authorization_code" || gotForm["client_id"][0] != "cid" {
		t.Fatalf("表单字段异常: %v", gotForm)
	}

	_, err = tc.FetchByAuthCode(ctx, "expired", "cid", MSNativeRedirect, GraphScope)
	if err == nil || !strings.Contains(err.Error(), "重新打开授权页") {
		t.Fatalf("code 过期应提示重走流程，得 %v", err)
	}
	_, err = tc.FetchByAuthCode(ctx, "other", "cid", MSNativeRedirect, GraphScope)
	if err == nil || !strings.Contains(err.Error(), "AADSTS9000") {
		t.Fatalf("其他错误应附诊断码，得 %v", err)
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
		t.Fatalf("429 应返回普通错误，得 %v", err)
	}
}

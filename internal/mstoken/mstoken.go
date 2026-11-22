// Package mstoken 提供 Microsoft 身份平台的刷新令牌缓存（DESIGN.md 3.5、3.6）。
// scope 参数化：IMAP 与 Graph 需要**不同 audience** 的令牌，互不通用（main.py 实测）。
package mstoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Microsoft 公共客户端端点（来自 main.py 实测，DESIGN.md 3.5）。
const (
	MSTokenEndpoint = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
	// MSAuthorizeEndpoint v2 授权页（M4"重新获取令牌"：授权码流的人工登录入口）。
	MSAuthorizeEndpoint = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	// MSNativeRedirect 公共客户端标准重定向地址（应用注册"移动和桌面应用"平台自带）。
	MSNativeRedirect = "https://login.microsoftonline.com/common/oauth2/nativeclient"
	// IMAPScope XOAUTH2 登录 outlook.office365.com 所需。
	IMAPScope = "https://outlook.office365.com/IMAP.AccessAsUser.All offline_access"
	// GraphScope 调用 graph.microsoft.com 所需（存量授权通常只含此权限，DESIGN.md 3.6）。
	GraphScope = "https://graph.microsoft.com/.default offline_access"
)

// ErrReauth 刷新令牌无效（invalid_grant），调用方应映射 auth_failed 并停止重试（DESIGN.md 3.4）。
var ErrReauth = errors.New("invalid_grant：刷新令牌无效或已过期，需要重新授权")

type cachedToken struct {
	access  string
	refresh string
	exp     time.Time
}

// TokenCache 进程级 access_token 缓存（TECH.md §2.4）：
// 常驻服务不得像 main.py（短命进程）那样每次连接都刷新——轮询短连接重连直接
// 复用未过期令牌，过期前 60 秒才再刷新；按账号 key 粒度加锁（DESIGN.md 3.5），
// 避免单个账号刷新缓慢时阻塞全进程其他账号的缓存读取或并发刷新。
type TokenCache struct {
	HTTP     *http.Client
	Endpoint string

	mu    sync.RWMutex
	cache map[string]cachedToken // key: scope + "\x00" + email + "\x00" + clientID
	locks map[string]*sync.Mutex // 按 key 串行刷新，不持全局锁做 HTTP I/O
}

func NewTokenCache() *TokenCache {
	return &TokenCache{
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Endpoint: MSTokenEndpoint,
		cache:    map[string]cachedToken{},
		locks:    map[string]*sync.Mutex{},
	}
}

func (t *TokenCache) keyLock(key string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = map[string]*sync.Mutex{}
	}
	l, ok := t.locks[key]
	if !ok {
		l = &sync.Mutex{}
		t.locks[key] = l
	}
	return l
}

// Token 用 refresh_token 换取 scope 对应的 access_token；若服务端轮换了 refresh_token
// 则同时返回（调用方须立即持久化，DESIGN.md 3.5）。rotated 为空表示未轮换。
func (t *TokenCache) Token(ctx context.Context, scope, email, clientID, refreshToken string) (access, rotated string, err error) {
	key := scope + "\x00" + email + "\x00" + clientID

	// 快路径：读锁命中直接返回，不受其他账号刷新阻塞。
	t.mu.RLock()
	if c, ok := t.cache[key]; ok && c.refresh == refreshToken && time.Until(c.exp) > time.Minute {
		t.mu.RUnlock()
		return c.access, "", nil
	}
	t.mu.RUnlock()

	// 慢路径：仅按 key 串行刷新，全局锁不跨 HTTP I/O。
	kl := t.keyLock(key)
	kl.Lock()
	defer kl.Unlock()

	// 双重检查：等待 key 锁期间可能已被并发协程刷新。
	t.mu.RLock()
	if c, ok := t.cache[key]; ok && c.refresh == refreshToken && time.Until(c.exp) > time.Minute {
		t.mu.RUnlock()
		return c.access, "", nil
	}
	t.mu.RUnlock()

	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("令牌端点请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &tr)

	if resp.StatusCode != http.StatusOK {
		// 不回显完整响应，避免泄露敏感信息（沿用 main.py 的做法）。
		switch {
		case tr.Error == "invalid_grant":
			return "", "", ErrReauth
		case tr.Error == "invalid_scope":
			return "", "", fmt.Errorf("invalid_scope：该应用注册未包含 %s 权限（需在 Azure 应用注册中添加并重新授权）", scope)
		case resp.StatusCode == http.StatusTooManyRequests:
			return "", "", errors.New("令牌端点限流（429），稍后重试")
		default:
			return "", "", fmt.Errorf("获取访问令牌失败：HTTP %d，%s", resp.StatusCode, tr.Error)
		}
	}
	if tr.AccessToken == "" {
		return "", "", errors.New("令牌接口未返回 access_token")
	}

	exp := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	newRefresh := tr.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken // 端点未轮换时保留原令牌
	}
	t.mu.Lock()
	t.cache[key] = cachedToken{access: tr.AccessToken, refresh: newRefresh, exp: exp}
	t.mu.Unlock()

	if tr.RefreshToken != "" && tr.RefreshToken != refreshToken {
		return tr.AccessToken, tr.RefreshToken, nil
	}
	return tr.AccessToken, "", nil
}

// AuthorizeURL 生成授权页地址（M4"重新获取令牌"：用户在浏览器登录目标邮箱，
// 完成后跳转到 redirect_uri 并携带 ?code=...；把完整跳转 URL 粘回 MailHub 兑换）。
// 注：密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），
// 授权码流是 MSA 账号续 refresh_token 的可行路径。
func AuthorizeURL(clientID, redirectURI, scope string) string {
	v := url.Values{
		"client_id":     {clientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {scope},
		"response_mode": {"query"},
	}
	return MSAuthorizeEndpoint + "?" + v.Encode()
}

// ExtractAuthCode 从用户粘贴的完整跳转 URL 中提取 code 参数；纯码串原样返回。
func ExtractAuthCode(input string) string {
	input = strings.TrimSpace(input)
	if !strings.Contains(input, "://") {
		return input
	}
	if u, err := url.Parse(input); err == nil {
		if c := u.Query().Get("code"); c != "" {
			return c
		}
	}
	return ""
}

// FetchByAuthCode 用授权码兑换全新 refresh_token（公共客户端，无 secret）。
// code 一次性、约 10 分钟有效；redirect_uri 须与授权请求一致。
func (t *TokenCache) FetchByAuthCode(ctx context.Context, code, clientID, redirectURI, scope string) (string, error) {
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("令牌端点请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)

	if resp.StatusCode != http.StatusOK {
		switch {
		case tr.Error == "invalid_grant":
			return "", errors.New("授权码无效或已过期（一次性、约 10 分钟有效），请重新打开授权页登录")
		case tr.Error == "invalid_client":
			return "", errors.New("invalid_client：client_id 无效，或应用注册未配置该重定向地址")
		case resp.StatusCode == http.StatusTooManyRequests:
			return "", errors.New("令牌端点限流（429），稍后重试")
		default:
			msg := tr.Error
			if tr.ErrorDesc != "" { // AADSTS 诊断码（不含凭据），排错必需
				if len(tr.ErrorDesc) > 220 {
					tr.ErrorDesc = tr.ErrorDesc[:220] + "…"
				}
				msg += "：" + tr.ErrorDesc
			}
			return "", fmt.Errorf("授权码换令牌失败：HTTP %d，%s", resp.StatusCode, msg)
		}
	}
	if tr.RefreshToken == "" {
		return "", errors.New("令牌接口未返回 refresh_token（scope 须包含 offline_access，或授权页未勾选同意）")
	}
	return tr.RefreshToken, nil
}

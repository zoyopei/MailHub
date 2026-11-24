// Package mstoken 提供 Microsoft 身份平台的刷新令牌缓存（DESIGN.md 3.5、3.6）。
// scope 参数化：IMAP 与 Graph 需要**不同 audience** 的令牌，互不通用（main.py 实测）。
package mstoken

import (
	"context"
	"encoding/base64"
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

func cacheKey(scope, email, clientID string) string {
	return scope + "\x00" + email + "\x00" + clientID
}

// Token 用 refresh_token 换取 scope 对应的 access_token；若服务端轮换了 refresh_token
// 则同时返回（调用方须立即持久化，DESIGN.md 3.5）。rotated 为空表示未轮换。
func (t *TokenCache) Token(ctx context.Context, scope, email, clientID, refreshToken string) (access, rotated string, err error) {
	key := cacheKey(scope, email, clientID)

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

	return t.refresh(ctx, key, scope, clientID, refreshToken)
}

// ForceToken 强制刷新：不信任缓存里的 access_token，丢弃后重新用 refresh_token 换取。
//
// 用于"缓存令牌看着没过期、实际已被拒"的场景——Graph 会话途中 401。缓存只保证令牌在
// 取得那一刻还剩一分钟以上有效期，而一个会话要发很多请求，大邮箱首次同步轻易超过。
// 不强制刷新就只能把 401 当成凭据失效（auth_failed，不会自动恢复）。
//
// 刷新令牌优先用缓存里记的那一个：上次刷新如果被轮换过，缓存里是同一家族里最新的值，
// 比调用方手里的旧值可靠（复用旧令牌可能触发家族吊销）。返回值含义同 Token。
func (t *TokenCache) ForceToken(ctx context.Context, scope, email, clientID, refreshToken string) (access, rotated string, err error) {
	key := cacheKey(scope, email, clientID)
	kl := t.keyLock(key)
	kl.Lock()
	defer kl.Unlock()

	t.mu.Lock()
	if c, ok := t.cache[key]; ok && c.refresh != "" {
		refreshToken = c.refresh
	}
	delete(t.cache, key)
	t.mu.Unlock()

	return t.refresh(ctx, key, scope, clientID, refreshToken)
}

// refresh 调令牌端点（refresh_token 授权），成功后更新缓存。调用方须持有 key 锁。
func (t *TokenCache) refresh(ctx context.Context, key, scope, clientID, refreshToken string) (access, rotated string, err error) {
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
//   - loginHint 非空时带 login_hint：授权页预填目标邮箱，减少"登成了别的账号"；
//   - scope 之外恒追加 openid：令牌端点据此在响应里附带 id_token，兑换后用它校验登录身份
//     （IdentityFromIDToken）——登错账号时 A 的令牌会被写进 B，必须拦住。
//
// 注：密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），
// 授权码流是 MSA 账号续 refresh_token 的可行路径。
func AuthorizeURL(clientID, redirectURI, scope, loginHint string) string {
	v := url.Values{
		"client_id":     {clientID},
		"response_type": {"code"},
		"redirect_uri":  {redirectURI},
		"scope":         {withOpenIDScope(scope)},
		"response_mode": {"query"},
	}
	if loginHint != "" {
		v.Set("login_hint", loginHint)
	}
	return MSAuthorizeEndpoint + "?" + v.Encode()
}

// openidScope OIDC 的登录范围：只用于让令牌端点返回 id_token（不含 profile/email 之外的资料）。
const openidScope = "openid"

// withOpenIDScope 在传输 scope 后追加 openid（已含则原样返回）。
func withOpenIDScope(scope string) string {
	for _, s := range strings.Fields(scope) {
		if s == openidScope {
			return scope
		}
	}
	return strings.TrimSpace(scope) + " " + openidScope
}

// Identity id_token 里的登录身份声明。
type Identity struct {
	PreferredUsername string // `preferred_username`：登录名（个人账户为账号邮箱；组织账户为 UPN）
	Email             string // `email`：仅在端点返回时存在
}

// String 取可用于展示的身份（优先登录名）。
func (id Identity) String() string {
	if id.PreferredUsername != "" {
		return id.PreferredUsername
	}
	return id.Email
}

// Matches 登录身份是否就是 target 邮箱（邮箱不区分大小写）。两个声明任一匹配即可：
// 个人账户的账号邮箱可能出现在 preferred_username 或 email 里。别名无法识别——
// 目标账号若以别名登记，这里始终判为不一致。
func (id Identity) Matches(target string) bool {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return false
	}
	for _, c := range []string{id.PreferredUsername, id.Email} {
		if c != "" && strings.ToLower(strings.TrimSpace(c)) == t {
			return true
		}
	}
	return false
}

// IdentityFromIDToken 解析 id_token（JWT）的载荷，取登录身份。
//
// **不校验签名**：这个 id_token 是本进程刚通过 TLS 从 Microsoft 令牌端点直接收到的响应体，
// 没有经过任何第三方——信任链与同一响应里的 refresh_token 完全相同。校验签名需要拉取并
// 缓存 JWKS 公钥、处理轮换，对"防登错账号"这个目的没有额外收益。只检查结构、必需声明和过期。
func IdentityFromIDToken(idToken string) (Identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Identity{}, errors.New("id_token 不是合法的 JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Identity{}, errors.New("id_token 载荷无法解码")
	}
	var claims struct {
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
		Exp               int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, errors.New("id_token 载荷不是合法 JSON")
	}
	if claims.PreferredUsername == "" && claims.Email == "" {
		return Identity{}, errors.New("id_token 不含登录账号声明（preferred_username / email）")
	}
	if claims.Exp != 0 && time.Now().Add(time.Minute).Unix() >= claims.Exp {
		return Identity{}, errors.New("id_token 已过期")
	}
	return Identity{PreferredUsername: strings.TrimSpace(claims.PreferredUsername),
		Email: strings.TrimSpace(claims.Email)}, nil
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
// 同时返回 id_token（scope 含 openid 时端点附带；可能为空）：调用方应先用
// IdentityFromIDToken 确认登录身份，再落库新令牌。
func (t *TokenCache) FetchByAuthCode(ctx context.Context, code, clientID, redirectURI, scope string) (refreshToken, idToken string, err error) {
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"scope":         {withOpenIDScope(scope)},
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
		IDToken      string `json:"id_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)

	if resp.StatusCode != http.StatusOK {
		switch {
		case tr.Error == "invalid_grant":
			return "", "", errors.New("授权码无效或已过期（一次性、约 10 分钟有效），请重新打开授权页登录")
		case tr.Error == "invalid_client":
			return "", "", errors.New("invalid_client：client_id 无效，或应用注册未配置该重定向地址")
		case resp.StatusCode == http.StatusTooManyRequests:
			return "", "", errors.New("令牌端点限流（429），稍后重试")
		default:
			msg := tr.Error
			if tr.ErrorDesc != "" { // AADSTS 诊断码（不含凭据），排错必需
				if len(tr.ErrorDesc) > 220 {
					tr.ErrorDesc = tr.ErrorDesc[:220] + "…"
				}
				msg += "：" + tr.ErrorDesc
			}
			return "", "", fmt.Errorf("授权码换令牌失败：HTTP %d，%s", resp.StatusCode, msg)
		}
	}
	if tr.RefreshToken == "" {
		return "", "", errors.New("令牌接口未返回 refresh_token（scope 须包含 offline_access，或授权页未勾选同意）")
	}
	return tr.RefreshToken, tr.IDToken, nil
}

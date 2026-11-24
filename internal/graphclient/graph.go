// Package graphclient 基于 Microsoft Graph API 的收信传输（DESIGN.md 3.6）。
// 背景：存量 Outlook 授权普遍只含 Graph 权限（IMAP scope → invalid_scope），
// Graph 因此是 Outlook 家族的**主传输**；$value 端点返回完整 MIME 原文，
// 与 IMAP 路径共享解析与入库。
// 只读原则：仅调用 list messages 与 GET $value，绝不 PATCH/POST/DELETE。
package graphclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

const (
	graphBase = "https://graph.microsoft.com/v1.0"
	pageTop   = 50  // 每页消息元数据条数（$top）
	maxPages  = 100 // 单轮分页上限（5000 条变更）：达到后本轮收工，下一轮接着走

	// maxConsecutiveRawFailures 一轮里允许**连续**跳过的"取原文失败"邮件数上限。
	// 单封失败多半是那封邮件自己的问题（已被删/移走），跳过即可；连续这么多封都失败
	// 说明是账号级故障（权限、应用注册），不能再当作"个别邮件"咽下去。
	maxConsecutiveRawFailures = 10

	// graphRetries 限流/暂不可用（429/503/504）的单请求重试次数。
	graphRetries = 2
)

// 等待参数（包级变量：测试缩短）。
var (
	// retryAfterCap 按 Retry-After 等待的单次上限：服务端可能要求等很久，等满了不如把这一轮
	// 交给 worker 退避重试（不占着会话）。
	retryAfterCap = 60 * time.Second
	// retryAfterNoHeader 响应没带 Retry-After 时的基础等待（逐次加倍）。
	retryAfterNoHeader = 2 * time.Second
)

// graphWellKnown Graph 的 well-known 文件夹名（小写）。这些名字不区分大小写；
// 不在表里的（自定义文件夹的显示名、文件夹 id）区分大小写，必须原样传。
var graphWellKnown = map[string]struct{}{
	"archive": {}, "clutter": {}, "conflicts": {}, "conversationhistory": {},
	"deleteditems": {}, "drafts": {}, "inbox": {}, "junkemail": {}, "localfailures": {},
	"msgfolderroot": {}, "outbox": {}, "recoverableitemsdeletions": {}, "scheduled": {},
	"sentitems": {}, "serverfailures": {}, "syncissues": {},
}

// Client 实现 mailclient.MailClient（Graph 路径）。
type Client struct {
	tokens *mstoken.TokenCache
	http   *http.Client
	base   string

	email  string
	cred   mailclient.Credentials
	seen   func(ids []string) (map[string]bool, error) // 已入库 graph_id 查询（sync 层注入 store.HasGraphIDs）
	access string

	mu      sync.Mutex
	rotated string // 会话途中刷新令牌时端点轮换出的新 refresh_token（worker 取走持久化）
}

// New 构造 Graph 客户端。seen 为批量去重回调（账号相关，由调用方闭包捕获），
// 传 nil 时视为全部未见过（靠 IngestMessage 的唯一索引兜底幂等）。
func New(tokens *mstoken.TokenCache, seen func(ids []string) (map[string]bool, error)) *Client {
	return &Client{
		tokens: tokens,
		http:   &http.Client{Timeout: 60 * time.Second},
		base:   graphBase,
		seen:   seen,
	}
}

// SetBase 覆写 API 根地址（测试注入 fake server）。
func (c *Client) SetBase(u string) { c.base = u }

// Connect 获取 Graph scope 的访问令牌（host/port 仅接口占位，端点固定）。
func (c *Client) Connect(ctx context.Context, _ string, _ int, username string, cred mailclient.Credentials) (string, error) {
	if cred.AuthType != "oauth2" {
		return "", errors.New("Graph 传输仅支持 oauth2（refresh_token）账号")
	}
	access, rotated, err := c.tokens.Token(ctx, mstoken.GraphScope, username, cred.ClientID, cred.RefreshToken)
	if err != nil {
		return "", err
	}
	c.email, c.cred, c.access = username, cred, access
	c.mu.Lock()
	c.rotated = ""
	c.mu.Unlock()
	return rotated, nil
}

// TakeRotatedToken 取走会话途中轮换出的 refresh_token（取后清空）。见 mailclient.SessionTokenRotator。
func (c *Client) TakeRotatedToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	rt := c.rotated
	c.rotated = ""
	return rt
}

// renew 强制刷新访问令牌（丢弃缓存，见 mstoken.TokenCache.ForceToken）。
func (c *Client) renew(ctx context.Context) error {
	access, rotated, err := c.tokens.ForceToken(ctx, mstoken.GraphScope, c.email, c.cred.ClientID, c.cred.RefreshToken)
	if err != nil {
		return err
	}
	c.access = access
	if rotated != "" {
		c.cred.RefreshToken = rotated
		c.mu.Lock()
		c.rotated = rotated
		c.mu.Unlock()
	}
	return nil
}

// listPage 单页消息元数据（列表与 delta 共用结构；delta 多一个 @odata.deltaLink）。
type listPage struct {
	Value []struct {
		ID               string `json:"id"`
		ReceivedDateTime string `json:"receivedDateTime"`
	} `json:"value"`
	NextLink  string `json:"@odata.nextLink"`
	DeltaLink string `json:"@odata.deltaLink"`
}

// FetchFolder 增量拉取一个文件夹（DESIGN.md 3.6，M2 起 delta 优先）：
//
//   - cursor.DeltaLink 非空 → GET deltaLink：只返回上次之后的变更（极窄窗口），
//     响应带回新 deltaLink 持续滚动；
//   - 无 deltaLink（首次/失效）→ $filter=receivedDateTime ge 游标列表分页，
//     并引导出初始 deltaLink。
//
// 两条路径共用 pageLoop：每页批量 seen 去重 → 未见过的逐封 GET $value → LatestSeen 取
// 本批最大 receivedDateTime（作为 deltaLink 失效时的兜底时间游标）。
// 游标用 ge（闭区间）+ 幂等去重，避免边界秒遗漏。
func (c *Client) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {

	if cursor != nil && cursor.DeltaLink != "" {
		res, err := c.fetchDelta(ctx, cursor.DeltaLink, handle)
		if err == nil {
			return res, nil
		}
		// 仅 deltaLink 失效（410 Gone / 404 / 400）才退回时间窗列表重建 delta；
		// 401（ErrReauth）、429、ctx 取消、网络或入库错误直接上抛，避免重复打 API 或掩盖错误。
		if !isDeltaInvalid(err) {
			return res, err
		}
		from := since
		if cursor.LastSeenAt.Valid && cursor.LastSeenAt.Time.After(from) {
			from = cursor.LastSeenAt.Time
		}
		return c.fetchList(ctx, folder, from, handle)
	}

	var from time.Time
	switch {
	case cursor != nil && cursor.LastSeenAt.Valid:
		from = cursor.LastSeenAt.Time
	default:
		from = since
	}
	if from.IsZero() {
		from = time.Now().Add(-90 * 24 * time.Hour) // 兜底：与首次同步窗口一致（worker 正常恒传 since，不走此分支）
	}
	return c.fetchList(ctx, folder, from, handle)
}

// fetchList 时间窗列表（首次/退回路径），并引导初始 delta 链接。
//
// 引导放在列表**之前**：delta 令牌对应"此刻"的状态，之后的增量从这一刻算起。时间窗列表
// 可能跑很久（每封原文一次请求，大邮箱首次同步以分钟计），期间到达的邮件不在列表里；
// 引导放在列表之后的话，这些邮件既没被列表带到、也不在 delta 令牌之后，从此再也拉不到。
// 先引导的话它们落在 delta 的覆盖范围内，下一轮就会带出来（与列表重叠的部分按 graph_id 去重）。
//
// 单轮分页上限：列表没走完时置 More、不交回 delta 链接，下一轮从 LatestSeen 接着列表。
func (c *Client) fetchList(ctx context.Context, folder string, from time.Time,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {

	res := mailclient.FetchResult{}
	// $deltatoken=latest：只要链接，不带数据。
	boot := fmt.Sprintf("%s/me/mailFolders/%s/messages/delta?$deltatoken=latest&$select=id,receivedDateTime",
		c.base, url.PathEscape(graphFolder(folder)))
	if bres, err := c.fetchDelta(ctx, boot, handle); err == nil {
		res.DeltaLink = bres.DeltaLink
	} else {
		// 只有 Graph 明确答复了"这个引导请求不行"（4xx、重试耗尽的 5xx）才咽下去，退而只用
		// 时间窗列表。认证失败、ctx 取消、网络错误、入库错误：列表也走不通，直接交上去——
		// 咽下去的话同一个 401 会在列表请求上再强制刷新一次令牌。
		var se *httpStatusError
		if !errors.As(err, &se) {
			return mailclient.FetchResult{}, err
		}
	}

	next := fmt.Sprintf("%s/me/mailFolders/%s/messages?%s",
		c.base, url.PathEscape(graphFolder(folder)), c.listParams(from))
	lres, resume, err := c.pageLoop(ctx, next, handle)
	if err != nil {
		return lres, err
	}
	if resume != "" {
		// 列表没走完（单轮分页上限）。这一轮引导出的 delta 链接不能交回去：它只覆盖
		// "本轮开始之后"的变更，而列表里还有更早的邮件没走到。下一轮必须继续走列表
		// （从 LatestSeen 起），走完那一轮的链接才作数。
		lres.DeltaLink = ""
		lres.More = true
		return lres, nil
	}
	lres.DeltaLink = res.DeltaLink
	return lres, nil
}

// fetchDelta 滚动 delta 链接：nextLink 分页直到 deltaLink。
// 因单轮分页上限停下时，把尚未走完的 nextLink 当作游标交回：下一轮 GET 它接着走
// （走完才拿得到 deltaLink）。不这样做的话，一条超过上限的变更流每一轮都从头走、
// 每一轮都在同一处停下，永远走不完。
func (c *Client) fetchDelta(ctx context.Context, link string,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {
	res, resume, err := c.pageLoop(ctx, link, handle)
	if err != nil {
		return res, err
	}
	if resume != "" {
		res.DeltaLink = resume
	}
	return res, nil
}

// pageLoop 列表/delta 共用的分页循环：去重、拉原文、汇总游标。
// resume 非空 = 因单轮分页上限停下，它是尚未走完的 nextLink（delta 路径把它当游标交回，
// 见 fetchDelta；列表路径不用它，见 fetchList）。
func (c *Client) pageLoop(ctx context.Context, start string,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (res mailclient.FetchResult, resume string, err error) {

	next := start
	consecutive := 0 // 连续取原文失败的邮件数
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			slog.Warn("graph 分页达到单轮上限，下一轮继续", "email", c.email, "pages", page)
			return res, next, nil
		}
		var p listPage
		if err := c.getJSON(ctx, next, &p); err != nil {
			return res, "", err
		}
		if len(p.Value) > 0 {
			ids := make([]string, 0, len(p.Value))
			for _, m := range p.Value {
				ids = append(ids, m.ID)
			}
			known := map[string]bool{}
			if c.seen != nil {
				var err error
				if known, err = c.seen(ids); err != nil {
					return res, "", fmt.Errorf("查询已入库消息失败: %w", err)
				}
			}

			for _, m := range p.Value {
				recv, err := time.Parse(time.RFC3339, m.ReceivedDateTime)
				if err != nil {
					continue // @removed 等无时间条目：删除通知，跳过
				}
				if recv.After(res.LatestSeen) {
					res.LatestSeen = recv
				}
				if known[m.ID] {
					continue
				}
				raw, err := c.getRaw(ctx, m.ID)
				if err != nil {
					if !skippable(err) {
						return res, "", err
					}
					// 只是这一封拉不下来（列表与取原文之间被删/移走 404、无权访问 403、无法
					// 导出 422…）：跳过它。以前任何一封出错都让整轮失败、游标不前进，下一轮又
					// 从这封开始——整个账号从此卡死，后面的邮件永远拉不到。
					consecutive++
					slog.Warn("graph 邮件取原文失败，已跳过", "email", c.email, "graph_id", m.ID, "error", err)
					if consecutive >= maxConsecutiveRawFailures {
						return res, "", fmt.Errorf("连续 %d 封邮件取原文失败（最后一次: %v），中止本轮", consecutive, err)
					}
					continue
				}
				consecutive = 0
				if err := handle(mailclient.MsgKey{GraphID: m.ID}, recv, raw); err != nil {
					return res, "", err
				}
			}
		}
		if p.DeltaLink != "" {
			res.DeltaLink = p.DeltaLink
			return res, "", nil // delta 链接即终点
		}
		// 空 value 也可能带 nextLink（delta 常见）：跟下去，不能当成走完了
		next = p.NextLink
	}
	return res, "", nil
}

// skippable 取原文的失败是否只关乎这一封邮件（可以跳过）：4xx，但排除
// 408（请求超时，瞬时）、429（限流，get 已经重试过）与 401（令牌问题，ErrReauth）。
// 网络错误、5xx、ctx 取消都不是"这一封的问题"，一律上抛。
func skippable(err error) bool {
	var se *httpStatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.code {
	case http.StatusUnauthorized, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return se.code >= 400 && se.code < 500
}

func (c *Client) listParams(from time.Time) string {
	v := url.Values{}
	v.Set("$filter", "receivedDateTime ge "+from.UTC().Format(time.RFC3339))
	v.Set("$orderby", "receivedDateTime asc")
	v.Set("$select", "id,receivedDateTime")
	v.Set("$top", fmt.Sprint(pageTop))
	return v.Encode()
}

// graphFolder IMAP 名 → Graph 文件夹名。well-known 名不区分大小写（InBox、INBOX 都认），
// 匹配上就规范成 Graph 的小写写法；其余（自定义文件夹的显示名、文件夹 id）**原样传**——
// 它们区分大小写，一律转小写会让 Graph 找不到文件夹。
func graphFolder(folder string) string {
	name := strings.TrimSpace(folder)
	lower := strings.ToLower(name)
	if _, ok := graphWellKnown[lower]; ok {
		return lower
	}
	return name
}

// getJSON 带 Bearer 的 GET。
func (c *Client) getJSON(ctx context.Context, u string, out any) error {
	body, err := c.get(ctx, u, "application/json")
	if err != nil {
		return err
	}
	defer body.Close()
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("解析 Graph 响应: %w", err)
	}
	return nil
}

func (c *Client) getRaw(ctx context.Context, id string) ([]byte, error) {
	body, err := c.get(ctx, c.base+"/me/messages/"+url.PathEscape(id)+"/$value", "message/rfc822")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	// 单封上限 25MB（Graph 消息硬上限），防御性读取。
	return io.ReadAll(io.LimitReader(body, 25<<20))
}

// get 带 Bearer 的 GET，处理两类"看起来像失败、其实等一等就好"的响应：
//
//   - 401：访问令牌可能在会话途中过期。令牌缓存只保证拿到手时还剩一分钟以上有效期，而一个
//     Graph 会话要发很多请求（每封原文一次），大邮箱首次同步轻易超过这个时间。先强制刷新
//     一次令牌再重试；刷新被明确拒绝（invalid_grant）才是真的凭据失效。以前任何 401 都直接
//     算 auth_failed：账号停在 auth_failed 且重启也不恢复，必须人工处理。
//   - 429 / 503 / 504：限流或暂不可用。按 Retry-After 等待后重试（至多 graphRetries 次），
//     等不到就把错误交上去，由 worker 退避。
func (c *Client) get(ctx context.Context, u, accept string) (io.ReadCloser, error) {
	renewed := false // 本次请求已因 401 强制刷新过令牌
	retries := 0     // 已做的限流重试次数
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.access)
		req.Header.Set("Accept", accept)
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Graph 请求失败: %w", err)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return resp.Body, nil

		case resp.StatusCode == http.StatusUnauthorized && !renewed:
			resp.Body.Close()
			renewed = true
			if err := c.renew(ctx); err != nil {
				if errors.Is(err, mstoken.ErrReauth) || ctx.Err() != nil {
					return nil, err
				}
				return nil, fmt.Errorf("Graph API 401，刷新令牌也失败: %w", err)
			}
			continue

		case resp.StatusCode == http.StatusUnauthorized:
			resp.Body.Close()
			// 刷新成功仍然 401：令牌本身没问题，是这个授权不允许访问邮件（应用注册缺
			// Mail.Read、用户未同意）。同样是凭据层面的事，只能重新授权。
			return nil, fmt.Errorf("%w: Graph API 401（刷新令牌后仍被拒绝；应用授权可能不包含读取邮件的权限）",
				mstoken.ErrReauth)

		case retryableStatus(resp.StatusCode) && retries < graphRetries:
			wait := retryAfter(resp.Header.Get("Retry-After"), retries)
			resp.Body.Close()
			retries++
			slog.Warn("graph 限流或暂不可用，等待后重试", "email", c.email,
				"status", resp.StatusCode, "wait", wait, "retries", retries)
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}

		case resp.StatusCode == http.StatusTooManyRequests:
			resp.Body.Close()
			return nil, fmt.Errorf("Graph 限流（429），重试 %d 次仍未成功", retries)

		default:
			resp.Body.Close()
			return nil, &httpStatusError{code: resp.StatusCode}
		}
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout
}

// retryAfter 由 Retry-After 头算出等待时长（秒数或 HTTP 日期；没有或无法解析时用
// retryAfterNoHeader 逐次加倍），封顶 retryAfterCap。
func retryAfter(header string, attempt int) time.Duration {
	wait := retryAfterNoHeader << attempt // 2s → 4s → …
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
		} else if at, err := http.ParseTime(header); err == nil {
			wait = time.Until(at)
		}
	}
	if wait < 0 {
		wait = 0
	}
	return min(wait, retryAfterCap)
}

// sleepCtx 可被 ctx 取消的等待。
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// httpStatusError 保留非 200 状态码，供上层区分 deltaLink 失效（410/404/400）与临时故障。
type httpStatusError struct {
	code int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("Graph API HTTP %d", e.code)
}

func isDeltaInvalid(err error) bool {
	var se *httpStatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.code == http.StatusGone || se.code == http.StatusNotFound || se.code == http.StatusBadRequest
}

// SupportsIdle/Idle：Graph 为 HTTP 轮询模型，无长连接推送（DESIGN.md 3.6）。
func (c *Client) SupportsIdle() bool { return false }
func (c *Client) Idle(context.Context, time.Duration) (bool, error) {
	return false, errors.New("Graph 传输不支持 IDLE")
}

// Logout/Close：HTTP 短连接模型下无持久会话（接口占位）。
func (c *Client) Logout() error { return nil }
func (c *Client) Close() error  { return nil }

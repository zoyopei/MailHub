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
	"net/http"
	"net/url"
	"strings"
	"time"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

const (
	graphBase = "https://graph.microsoft.com/v1.0"
	pageTop   = 50  // 每页消息元数据条数（$top）
	maxPages  = 100 // 单文件夹单周期分页上限（防异常邮箱无限分页）
)

// wellKnownFolders IMAP 常用名 → Graph well-known folder 名。
var wellKnownFolders = map[string]string{
	"INBOX": "inbox",
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
	return rotated, nil
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
//     末尾用 $deltatoken=latest 引导出初始 deltaLink。
//
// 两条路径共用：每页批量 seen 去重 → 未见过的逐封 GET $value → LatestSeen 取
// 本批最大 receivedDateTime（作为 deltaLink 失效时的兜底时间游标）。
// 游标用 ge（闭区间）+ 幂等去重，避免边界秒遗漏。
func (c *Client) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {

	if cursor != nil && cursor.DeltaLink != "" {
		res, err := c.fetchDelta(ctx, cursor.DeltaLink, handle)
		if err == nil {
			return res, nil
		}
		// deltaLink 失效（410 Gone 等）：退回时间窗列表并重建 delta。
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

// fetchList 时间窗列表（首次/退回路径）：完整遍历后引导 deltaLink。
func (c *Client) fetchList(ctx context.Context, folder string, from time.Time,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {

	next := fmt.Sprintf("%s/me/mailFolders/%s/messages?%s",
		c.base, url.PathEscape(graphFolder(folder)), c.listParams(from))
	res, err := c.pageLoop(ctx, next, handle)
	if err != nil {
		return res, err
	}
	// 引导初始 delta 链接（$deltatoken=latest：只要链接；value 命中 seen 幂等跳过）。
	boot := fmt.Sprintf("%s/me/mailFolders/%s/messages/delta?$deltatoken=latest&$select=id,receivedDateTime",
		c.base, url.PathEscape(graphFolder(folder)))
	if bres, err := c.fetchDelta(ctx, boot, handle); err == nil && bres.DeltaLink != "" {
		res.DeltaLink = bres.DeltaLink
	}
	return res, nil
}

// fetchDelta 滚动 delta 链接：nextLink 分页直到 deltaLink。
func (c *Client) fetchDelta(ctx context.Context, link string,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {
	return c.pageLoop(ctx, link, handle)
}

// pageLoop 列表/delta 共用的分页循环：去重、拉原文、汇总游标。
func (c *Client) pageLoop(ctx context.Context, start string,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {

	res := mailclient.FetchResult{}
	next := start
	for page := 0; next != "" && page < maxPages; page++ {
		var p listPage
		if err := c.getJSON(ctx, next, &p); err != nil {
			return res, err
		}
		if len(p.Value) == 0 {
			// 空 value 也可能带 deltaLink（无变更的 delta 响应）
			if p.DeltaLink != "" {
				res.DeltaLink = p.DeltaLink
			}
			break
		}

		ids := make([]string, 0, len(p.Value))
		for _, m := range p.Value {
			ids = append(ids, m.ID)
		}
		known := map[string]bool{}
		if c.seen != nil {
			var err error
			known, err = c.seen(ids)
			if err != nil {
				return res, fmt.Errorf("查询已入库消息失败: %w", err)
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
				return res, err
			}
			if err := handle(mailclient.MsgKey{GraphID: m.ID}, recv, raw); err != nil {
				return res, err
			}
		}
		if p.DeltaLink != "" {
			res.DeltaLink = p.DeltaLink
			break // delta 链接即终点
		}
		next = p.NextLink
	}
	return res, nil
}

func (c *Client) listParams(from time.Time) string {
	v := url.Values{}
	v.Set("$filter", "receivedDateTime ge "+from.UTC().Format(time.RFC3339))
	v.Set("$orderby", "receivedDateTime asc")
	v.Set("$select", "id,receivedDateTime")
	v.Set("$top", fmt.Sprint(pageTop))
	return v.Encode()
}

// graphFolder IMAP 名 → Graph well-known 名（无映射则小写直传）。
func graphFolder(folder string) string {
	if f, ok := wellKnownFolders[folder]; ok {
		return f
	}
	return strings.ToLower(strings.TrimSpace(folder))
}

// getJSON 带 Bearer 的 GET；401 视为令牌失效（映射 ErrReauth，worker 置 auth_failed）。
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

func (c *Client) get(ctx context.Context, u, accept string) (io.ReadCloser, error) {
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
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: Graph API 401（令牌无效或过期）", mstoken.ErrReauth)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, errors.New("Graph 限流（429），稍后重试")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("Graph API HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// SupportsIdle/Idle：Graph 为 HTTP 轮询模型，无长连接推送（DESIGN.md 3.6）。
func (c *Client) SupportsIdle() bool { return false }
func (c *Client) Idle(context.Context, time.Duration) (bool, error) {
	return false, errors.New("Graph 传输不支持 IDLE")
}

// Logout/Close：HTTP 短连接模型下无持久会话（接口占位）。
func (c *Client) Logout() error { return nil }
func (c *Client) Close() error  { return nil }

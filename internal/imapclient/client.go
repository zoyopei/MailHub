// Package imapclient 封装 IMAP 连接与增量拉取（TECH.md §2.4）。
// 实现 mailclient.MailClient 接口；令牌刷新统一走 mstoken（IMAP/Graph 不同 scope）。
// 只读原则（DESIGN.md 3.1）：SelectOptions.ReadOnly（EXAMINE）+ BODY.PEEK。
package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

// ErrAuthFailed IMAP 认证被拒（密码错误等），worker 映射 auth_failed。
// 刷新令牌失效统一用 mstoken.ErrReauth。
var ErrAuthFailed = errors.New("IMAP 认证失败")

// goClient 是 mailclient.MailClient 的 go-imap 实现。TokenCache 进程级共享：
// 同账号复用未过期 access_token，轮询短连接不重复刷新令牌端点（TECH.md §2.4）。
type goClient struct {
	conn   *imapclient.Client
	tokens *mstoken.TokenCache
}

// NewGoClient 构造实现。tokens 传 nil 时内部自动创建。
func NewGoClient(tokens *mstoken.TokenCache) mailclient.MailClient {
	if tokens == nil {
		tokens = mstoken.NewTokenCache()
	}
	return &goClient{tokens: tokens}
}

func (g *goClient) Connect(ctx context.Context, host string, port int, username string, cred mailclient.Credentials) (string, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	// 使用 tls.Dialer.DialContext 透传 ctx：停机或删除账号可立即中断卡住的 TCP/TLS 握手。
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 30 * time.Second},
		Config:    &tls.Config{ServerName: host},
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("连接 %s: %w", addr, err)
	}
	cl := imapclient.New(conn, nil)
	g.conn = cl

	switch cred.AuthType {
	case "oauth2":
		access, rotated, err := g.tokens.Token(ctx, mstoken.IMAPScope, username, cred.ClientID, cred.RefreshToken)
		if err != nil {
			cl.Close()
			return "", err
		}
		if err := cl.Authenticate(newXOAUTH2(username, access)); err != nil {
			cl.Close()
			return "", fmt.Errorf("%w: XOAUTH2 被拒绝（确认账号已启用 IMAP 且令牌含 IMAP 权限）: %v", ErrAuthFailed, err)
		}
		return rotated, nil
	default: // password / 授权码
		if err := cl.Login(username, cred.Password).Wait(); err != nil {
			cl.Close()
			return "", fmt.Errorf("%w: %v", ErrAuthFailed, err)
		}
		return "", nil
	}
}

// FetchFolder 只读打开文件夹并增量拉取：
//   - cursor 有效且 UIDVALIDITY 一致 → UID > last_uid 增量；
//   - 无游标或 UIDVALIDITY 变化 → 归零重拉，时间窗取 since（首次同步窗口，
//     DESIGN.md 3.4；重复邮件由 UNIQUE 约束幂等忽略）。
//
// Graph 的游标产出（latestSeen/deltaLink）在 IMAP 路径不适用，恒返回零值。
func (g *goClient) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (mailclient.FetchResult, error) {

	sel, err := g.conn.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait() // EXAMINE
	if err != nil {
		return mailclient.FetchResult{}, fmt.Errorf("EXAMINE %s: %w", folder, err)
	}
	uidv := sel.UIDValidity

	var lastUID uint32
	var window time.Time
	if cursor != nil && cursor.UIDValidity == uidv {
		lastUID = cursor.LastUID
	} else {
		window = since
	}

	criteria := &imap.SearchCriteria{}
	if lastUID > 0 {
		var set imap.UIDSet
		set.AddRange(imap.UID(lastUID+1), imap.UID(math.MaxUint32))
		criteria.UID = []imap.UIDSet{set}
	}
	if !window.IsZero() {
		criteria.Since = window // INTERNALDATE 侧，近似收到时间
	}
	data, err := g.conn.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return mailclient.FetchResult{}, fmt.Errorf("UID SEARCH: %w", err)
	}
	uids := data.AllUIDs()
	if len(uids) == 0 {
		return mailclient.FetchResult{}, nil
	}
	fetchOpts := &imap.FetchOptions{
		UID:          true,
		InternalDate: true, // 服务器记录的到达时间 → received_at（邮件真实时间）
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierNone, Peek: true}, // BODY.PEEK[]：完整原文，不改 \Seen
		},
	}
	// 分批 + 流式读取（cmd.Next）：每批至多 fetchBatchSize 封、单次仅持一封原文在内存，
	// 避免首次同步大邮箱时 Collect() 一次性加载全部邮件引发 OOM，同时及时响应 ctx 取消。
	const fetchBatchSize = 20
	for start := 0; start < len(uids); start += fetchBatchSize {
		if ctx.Err() != nil {
			return mailclient.FetchResult{}, ctx.Err()
		}
		end := min(start+fetchBatchSize, len(uids))
		var set imap.UIDSet
		for _, uid := range uids[start:end] {
			set.AddNum(uid)
		}
		cmd := g.conn.Fetch(&set, fetchOpts)
		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				_ = cmd.Close()
				return mailclient.FetchResult{}, fmt.Errorf("UID FETCH: %w", err)
			}
			raw := rawBytes(buf)
			if raw == nil {
				continue
			}
			// seenAt = INTERNALDATE（邮件到达邮箱的真实时间；个别服务器缺失时由 worker 用 Date 头兜底）
			if err := handle(mailclient.MsgKey{UIDValidity: uidv, UID: uint32(buf.UID)}, buf.InternalDate, raw); err != nil {
				_ = cmd.Close()
				return mailclient.FetchResult{}, err
			}
			if ctx.Err() != nil {
				_ = cmd.Close()
				return mailclient.FetchResult{}, ctx.Err()
			}
		}
		if err := cmd.Close(); err != nil {
			return mailclient.FetchResult{}, fmt.Errorf("UID FETCH: %w", err)
		}
	}
	return mailclient.FetchResult{}, nil
}

// SupportsIdle 服务器是否通告 IDLE 能力（RFC 2177）。
func (g *goClient) SupportsIdle() bool {
	if g.conn == nil {
		return false
	}
	return g.conn.Caps().Has(imap.CapIdle)
}

// Idle 阻塞等待服务器 unilateral 数据（EXISTS/EXPUNGE 等）或超时。
// 任何服务器动静都返回 notified=true——EXPUNGE/FLAGS 也会触发，worker 做
// 一次增量拉取即可分辨（无新邮件时为 no-op 并顺带推进心跳）。
// ctx 取消：终止 IDLE 并返回 ctx.Err()，连接保持可用（手动收取/兜底路径）。
func (g *goClient) Idle(ctx context.Context, timeout time.Duration) (bool, error) {
	cmd, err := g.conn.Idle()
	if err != nil {
		return false, fmt.Errorf("IDLE: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done: // 服务器数据到达（或 IDLE 出错）
		return err == nil, err
	case <-ctx.Done(): // 外部终止（手动收取/兜底/停机）
		_ = cmd.Close()
		<-done
		return false, ctx.Err()
	case <-timer.C:
		_ = cmd.Close()
		<-done
		return false, nil
	}
}

// rawBytes 取第一个 BODY[] 段的字节。
func rawBytes(buf *imapclient.FetchMessageBuffer) []byte {
	for i := range buf.BodySection {
		if buf.BodySection[i].Section != nil &&
			buf.BodySection[i].Section.Specifier == imap.PartSpecifierNone &&
			len(buf.BodySection[i].Section.Part) == 0 {
			return buf.BodySection[i].Bytes
		}
	}
	if len(buf.BodySection) > 0 {
		return buf.BodySection[0].Bytes
	}
	return nil
}

func (g *goClient) Logout() error {
	if g.conn == nil {
		return nil
	}
	return g.conn.Logout().Wait()
}

func (g *goClient) Close() error {
	if g.conn == nil {
		return nil
	}
	return g.conn.Close()
}

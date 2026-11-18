// Package mailclient 定义 IMAP 与 Graph 两种传输的统一抽象（DESIGN.md 3.6）。
// sync.worker 只面向本接口；imapclient 与 graphclient 各自实现。
package mailclient

import (
	"context"
	"time"

	"mailhub/internal/store"
)

// Credentials 明文凭据（解密后由 sync 层注入；实现包不涉及加密）。
type Credentials struct {
	AuthType     string // password | oauth2
	Password     string
	ClientID     string
	RefreshToken string
}

// MsgKey 消息的唯一坐标：IMAP 路径填 UIDValidity/UID，Graph 路径填 GraphID。
type MsgKey struct {
	UIDValidity uint32
	UID         uint32
	GraphID     string
}

// FetchResult 一次文件夹拉取的游标产出：
//   - LatestSeen：本批最大 receivedDateTime（Graph 时间游标；IMAP 恒零值）
//   - DeltaLink：Graph delta 增量链接（M2；空 = 无新链接/不适用）
type FetchResult struct {
	LatestSeen time.Time
	DeltaLink  string
}

// MailClient 是 worker 依赖的最小收信能力。
// 轮询模式（M1）：每 cycle Connect → FetchFolder → Logout（短连接）。
// IDLE 模式（M2，仅 IMAP）：连接跨周期存活，Idle 阻塞等待通知。
type MailClient interface {
	// Connect 建立连接并登录。oauth2 账号返回轮换后的新 refresh_token（空 = 未轮换）。
	// host/port 对 Graph 实现可为空（端点固定）。
	Connect(ctx context.Context, host string, port int, username string, cred Credentials) (rotated string, err error)
	// FetchFolder 增量拉取一个文件夹的完整原文（MIME），逐封回调 handle。
	// cursor 为该文件夹上次游标（nil = 首次）：IMAP 用 UIDVALIDITY/last_uid；
	// Graph 用 last_seen_at/delta_link（结合 since 重叠窗口）。
	FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
		handle func(key MsgKey, seenAt time.Time, raw []byte) error) (FetchResult, error)
	// SupportsIdle 是否支持 IDLE 长连接推送（Graph 恒 false）。
	SupportsIdle() bool
	// Idle 阻塞等待服务器通知（EXISTS 等）或 timeout 到期。
	// 返回 true = 收到服务器数据（应做一次增量拉取）；false = 超时（重新发起即可）。
	// ctx 取消时返回 ctx.Err()，实现须保证连接仍可用。
	Idle(ctx context.Context, timeout time.Duration) (notified bool, err error)
	// Logout 正常断开。
	Logout() error
	// Close 直接关闭底层连接（出错路径）。
	Close() error
}

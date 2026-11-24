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
//   - UIDValidity / LastUID：IMAP 文件夹当前的 UIDVALIDITY 与本轮结果里的最大 UID；Graph 恒为零值。
//     worker 在整轮成功后用它们推进 folder_states（只增不减）——一封都没入库时也要推进：
//     首次同步遇到空文件夹（或邮件全部早于时间窗）时 LastUID 取"文件夹里现有邮件的最大 UID"
//     （IMAP 实现再搜一次 UID 1:* 得到；空文件夹为 0），游标得以建立，否则永远按首次同步
//     处理；增量路径空结果时 LastUID 为 0（即不推进）。
//   - More：本轮因单轮分页上限收工、还没走完（目前只有 Graph 时间窗列表会置位）。
//     这一轮过后该文件夹仍算"首次同步未完成"（folder_states.initial_done 保持 0，见
//     store.BeginFolderSync / FinishFolderSync）：续走的那一批照样按历史邮件处理，不通知。
//     续走的起点是已持久化的游标（Graph 的 last_seen_at），进程重启后接着走。
type FetchResult struct {
	LatestSeen  time.Time
	DeltaLink   string
	UIDValidity uint32
	LastUID     uint32
	More        bool
}

// MailClient 是 worker 依赖的最小收信能力。
// 轮询模式（M1）：每 cycle Connect → FetchFolder → Logout（短连接）。
// IDLE 模式（M2，仅 IMAP）：连接跨周期存活，Idle 阻塞等待通知。
type MailClient interface {
	// Connect 建立连接并登录。oauth2 账号返回轮换后的新 refresh_token（空 = 未轮换）。
	// host/port 对 Graph 实现可为空（端点固定）。IMAP 实现按端口选择加密方式：
	// 993（默认）等端口用隐式 TLS；143 端口用 STARTTLS（必须升级成功，绝不明文登录）。
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

// SessionTokenRotator 可选能力：实现在一次会话**途中**刷新了令牌、且端点轮换了 refresh_token 时，
// worker 在会话结束时取走新令牌持久化（取后清空）。Connect 的返回值只覆盖连接那一刻的轮换；
// Graph 这类一次会话要发很多请求的传输，访问令牌会在途中过期并触发刷新（graphclient 的
// 401 处理）。不实现本接口的传输没有途中轮换。worker 通过类型断言使用，不改动 MailClient。
type SessionTokenRotator interface {
	TakeRotatedToken() string
}

package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"mailhub/internal/imapclient"
	"mailhub/internal/mailclient"
	"mailhub/internal/mailparse"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

// idleWaitTimeout 单次 IDLE 等待上限（25 分钟 < 协议 29 分钟刷新要求，DESIGN.md 3.2）；
// 到点正常返回后由 idleLoop 决定重新等待或兜底拉取。
const idleWaitTimeout = 25 * time.Minute

// initialSyncWindow 首次同步时间窗（DESIGN.md 3.4/5.1）：无游标（首次添加账号）时只拉
// 最近 initial_sync_days 天（默认 90 天，Web 全局配置可覆盖）——该批为历史邮件，
// 不触发 webhook，避免通知轰炸。零值防御 90（正常路径 config.Load 已校验 >0）。
func (w *worker) initialSyncWindow() time.Duration {
	days := w.m.settingInt(store.SettingInitialSyncDays, w.m.cfg.InitialSyncDays)
	if days <= 0 {
		days = 90
	}
	return time.Duration(days) * 24 * time.Hour
}

// worker 是单账号的同步状态机（TECH.md §2.6）。M1 为轮询模式。
type worker struct {
	m    *Manager
	acct *store.Account
	// notify 账号提醒开关的实时值：spawn 时取 acct.NotifyEnabled，之后由
	// Manager.SetNotifyEnabled 原子更新——PATCH notify_enabled 不重启 worker 即时生效。
	// 不能直接读 acct.NotifyEnabled：那是 worker 启动时的快照，改了库里的值它不会变。
	notify atomic.Bool

	trigger chan struct{} // cap=1：手动收取插入信号，非阻塞发送天然合并（DESIGN.md 3.2）
	cancel  context.CancelFunc
	done    chan struct{} // run 退出时关闭（Manager 据此等待旧 worker 真正停止；nil = 测试直构）
}

// minPollInterval 轮询周期下限：防御库中历史脏数据（0/负数会空转或令 rand.Int64N panic）。
const minPollInterval = 5 * time.Second

// run 主循环：按传输模式反复建立**会话**——
//   - IDLE 会话（IMAP 且支持且预算内）：单文件夹长连接，阻塞至断线/停止；
//   - 轮询会话（其余，含全部 Graph）：短连接单次收取，按周期重建。
//
// 认证类错误立即停止（auth_failed），其他错误指数退避后重连（DESIGN.md 3.2）。
func (w *worker) run(ctx context.Context) {
	if w.done != nil {
		defer close(w.done) // 最后执行：forget/cancel 完成后才宣告退出
	}
	// 任何原因退出（含 auth_failed）都从 Manager 注销自身：否则 TriggerNow 会把信号
	// 发给已退出的 worker（无人消费），手动收取无法重新拉起账号。
	defer w.m.forget(w)
	defer w.cancel()
	backoff := 5 * time.Second
	for {
		err := w.session(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, mstoken.ErrReauth) || errors.Is(err, imapclient.ErrAuthFailed):
			// 认证失败即停（DESIGN.md 3.4）：置 auth_failed，等待用户更新凭据。
			w.m.store.SetAccountStatus(w.acct.ID, store.StatusAuthFailed, err)
			slog.Warn("worker auth failed, stopping", "account_id", w.acct.ID, "email", w.acct.Email, "error", err)
			return
		case err == nil:
			backoff = 5 * time.Second
		default:
			w.m.store.SetAccountStatus(w.acct.ID, store.StatusError, err)
			slog.Warn("worker session failed", "account_id", w.acct.ID, "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 5*time.Minute) // 5s→5min 封顶（DESIGN.md 3.2）
			continue
		}

		// 轮询模式的周期等待（IDLE 会话不会正常返回到这里）
		interval := max(time.Duration(w.acct.PollInterval)*time.Second, minPollInterval)
		jitter := time.Duration(rand.Int64N(int64(interval/4) + 1)) // 1/4 周期抖动防同步风暴
		select {
		case <-ctx.Done():
			return
		case <-w.trigger: // 手动收取：立即进入下一轮，不重置任何状态（插入信号语义）
			slog.Debug("manual trigger", "account_id", w.acct.ID)
		case <-time.After(interval + jitter):
		}
	}
}

// session 一次连接生命周期：连接（含令牌轮换持久化）→ 按 IDLE/轮询分流。
// IDLE 会话阻塞直至断线或 ctx 取消；轮询会话完成一次收取即返回。
func (w *worker) session(ctx context.Context) error {
	cl := w.m.dial(w.acct)
	cred, err := w.credentials()
	if err != nil {
		return err
	}
	rotated, err := cl.Connect(ctx, w.acct.IMAPHost, w.acct.IMAPPort, w.acct.Username, cred)
	if err != nil {
		return err
	}
	if rotated != "" {
		// 令牌轮换：立即持久化（DESIGN.md 3.5），失败不阻断本次收取（旧令牌仍可用到下次）。
		if enc, e := w.m.cipher.Encrypt([]byte(rotated)); e == nil {
			if e := w.m.store.UpdateCredential(w.acct.ID, enc); e != nil {
				slog.Error("persist rotated refresh token failed", "account_id", w.acct.ID, "error", e)
			} else {
				// 内存快照也要更新：下个会话的 credentials() 解密的是 w.acct.CredentialEnc，
				// 不更新的话它会一直用启动时的旧令牌——TokenCache 里存的是服务端返回的新
				// 令牌，两者对不上，缓存永不命中，每个轮询周期都会再打一次令牌端点。
				w.acct.CredentialEnc = enc
				slog.Info("refresh token rotated", "account_id", w.acct.ID)
			}
		}
	}
	defer cl.Logout()

	w.m.store.SetAccountStatus(w.acct.ID, store.StatusSyncing, nil)

	// IDLE 准入：服务器支持 + 单文件夹（默认 INBOX；多文件夹配置退回轮询，
	// DESIGN.md 3.1 连接是稀缺资源）+ 单主机预算内（2.2，超预算降级轮询）。
	if w.canIdle(cl) {
		defer w.m.budget.Release(w.acct.IMAPHost)
		return w.idleLoop(ctx, cl)
	}
	return w.pollOnce(ctx, cl)
}

// canIdle 判定并占用预算（占用成功即由 session 负责 Release）。
// 预算上限取全局配置（DB 覆盖 > env），每次会话准入时读取：改配置后
// 新会话即按新预算判定，存量 IDLE 连接不受影响（直至其自然断开）。
func (w *worker) canIdle(cl mailclient.MailClient) bool {
	return cl.SupportsIdle() &&
		len(w.folders()) == 1 &&
		w.m.budget.Acquire(w.acct.IMAPHost,
			w.m.settingInt(store.SettingIdleBudgetPerHost, w.m.cfg.IdleBudgetPerHost))
}

// idleLoop IDLE 会话（DESIGN.md 3.2 状态机）：
// 初始拉取 → 循环{ IDLE 等待 | 手动触发 | 5 分钟兜底 } → 增量拉取（成功即心跳）。
// 任何拉取/IDLE 错误返回上层走退避重连。
func (w *worker) idleLoop(ctx context.Context, cl mailclient.MailClient) error {
	if err := w.pollOnce(ctx, cl); err != nil {
		return err
	}
	slog.Info("worker in IDLE mode", "account_id", w.acct.ID, "email", w.acct.Email)

	for {
		w.m.store.SetAccountStatus(w.acct.ID, store.StatusIdle, nil)
		// 兜底间隔取全局配置（DB 覆盖 > env），每轮 IDLE 循环读取：保存后下一轮兜底即生效。
		fallback := time.Duration(w.m.settingInt(store.SettingFallbackInterval, w.m.cfg.FallbackInterval)) * time.Second
		if fallback < 30*time.Second {
			fallback = 30 * time.Second
		}
		idleCtx, stopIdle := context.WithCancel(ctx)
		type ires struct {
			notified bool
			err      error
		}
		ch := make(chan ires, 1)
		go func() {
			n, err := cl.Idle(idleCtx, idleWaitTimeout)
			ch <- ires{n, err}
		}()

		timer := time.NewTimer(fallback)
		var fetch bool
		select {
		case <-ctx.Done():
			timer.Stop()
			stopIdle()
			<-ch
			return ctx.Err()
		case r := <-ch: // 服务器通告新邮件（EXISTS）或 IDLE 出错
			timer.Stop()
			stopIdle()
			if r.err != nil && r.err != context.Canceled {
				return r.err // 连接异常：交上层重连
			}
			fetch = r.notified
		case <-w.trigger: // 手动收取：退出 IDLE → 拉取 → 重新 IDLE（插入信号语义）
			timer.Stop()
			stopIdle()
			<-ch
			fetch = true
		case <-timer.C: // 兜底定时器：拉取防通知丢失 + 心跳推进上限
			stopIdle()
			<-ch
			fetch = true
		}

		if fetch {
			if err := w.pollOnce(ctx, cl); err != nil {
				return err
			}
		}
	}
}

// pollOnce 一个完整收取：各文件夹增量拉取 + 心跳 + 状态（轮询会话主体）。
func (w *worker) pollOnce(ctx context.Context, cl mailclient.MailClient) error {
	var errs []string
	for _, folder := range w.folders() {
		if err := w.fetchFolder(ctx, cl, folder); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", folder, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	// 心跳：一次成功完成的收件检查即更新，无论有无新邮件（DESIGN.md 3.2）。
	if err := w.m.store.TouchAccount(w.acct.ID); err != nil {
		slog.Error("touch heartbeat failed", "account_id", w.acct.ID, "error", err)
	}
	w.m.store.SetAccountStatus(w.acct.ID, store.StatusPolling, nil)
	return nil
}

// fetchFolder 增量拉取一个文件夹，IMAP/Graph 统一路径：
// 游标由 client 实现解释（IMAP：UIDVALIDITY/last_uid；Graph：last_seen_at），
// worker 只提供初始窗口（首次同步 7 天）与入库回调；Graph 路径在完整拉取后
// 推进 last_seen_at 游标（失败不推进，重拉靠 graph_id 幂等）。
// firstSync：无游标即首次添加账号——该批邮件为历史邮件，不触发 webhook（DESIGN 3.4/5.1）。
func (w *worker) fetchFolder(ctx context.Context, cl mailclient.MailClient, folder string) error {
	st, err := w.m.store.GetFolderState(w.acct.ID, folder)
	if err != nil {
		return err
	}
	firstSync := st == nil
	// 初始窗口恒传：client 在游标有效时忽略它（IMAP UIDVALIDITY 变化时启用）。
	since := time.Now().Add(-w.initialSyncWindow())
	res, err := cl.FetchFolder(ctx, folder, st, since, func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return w.ingest(folder, key, seenAt, raw, firstSync)
	})
	if err != nil {
		return err
	}
	if !res.LatestSeen.IsZero() {
		if err := w.m.store.TouchFolderSeen(w.acct.ID, folder, res.LatestSeen); err != nil {
			return err
		}
	}
	if res.DeltaLink != "" {
		return w.m.store.SetFolderDelta(w.acct.ID, folder, res.DeltaLink)
	}
	return nil
}

// ingest 单封邮件入库：写 .eml → 事务入库 → 改名定稿（DESIGN.md 3.3）。
// 文件名用消息坐标：IMAP {uidvalidity}-{uid}.eml；Graph g-{sha256(graph_id) 前 64 位}.eml
// （32 位哈希在大邮箱下有可观碰撞概率，碰撞会互相覆盖文件），与事务解耦、天然幂等。
// historical：首次同步的历史邮件，不生成投递记录（DESIGN 3.4/5.1）。
func (w *worker) ingest(folder string, key mailclient.MsgKey, seenAt time.Time, raw []byte, historical bool) error {
	p := mailparse.Parse(raw, w.m.cfg.MaxMessageSize)

	uid := key.UID
	name := fmt.Sprintf("%d-%d", key.UIDValidity, key.UID)
	if key.GraphID != "" {
		sum := sha256.Sum256([]byte(key.GraphID))
		name = "g-" + hex.EncodeToString(sum[:8])
	}
	// received_at = 邮件真实到达时间：Graph receivedDateTime / IMAP INTERNALDATE（seenAt），
	// 缺失时退回 Date 头（SentAt）；都为零则 store 兜底当前时间（DESIGN.md §4）。
	recvAt := seenAt
	if recvAt.IsZero() {
		recvAt = p.SentAt
	}
	rel := fmt.Sprintf("mails/%d/%s/%s.eml",
		w.acct.ID, recvAt.UTC().Format("2006-01"), name)
	abs := filepath.Join(w.m.cfg.DataDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return err
	}
	// 先写临时文件并原子改名定稿，再提交事务：避免事务已提交后崩溃导致 DB 引用缺失的 .eml。
	// 若目标文件已存在（重复投递或上次遗留），保留现有文件不动。
	_, statErr := os.Stat(abs)
	existed := statErr == nil
	if !existed {
		tmp := abs + ".part"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, abs); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("定稿 .eml 失败: %w", err)
		}
	}

	nm := &store.NewMessage{
		AccountID:      w.acct.ID,
		Folder:         folder,
		UIDValidity:    key.UIDValidity,
		UID:            key.UID,
		GraphID:        key.GraphID, // 非空时 IngestMessage 按 graph_id 幂等去重
		SeenAt:         seenAt,      // Graph 游标（IMAP 为零值）
		ReceivedAt:     recvAt,      // 邮件真实到达时间（见上；零值时 store 兜底 now）
		MessageID:      p.MessageID,
		Subject:        p.Subject,
		FromAddr:       p.FromAddr,
		ToAddrsJSON:    mailparse.ToJSON(p.ToAddrs),
		CcAddrsJSON:    mailparse.ToJSON(p.CcAddrs),
		SentAt:         p.SentAt,
		BodyText:       p.BodyText,
		BodyHTML:       p.BodyHTML,
		Snippet:        p.Snippet,
		Size:           p.Size,
		HasAttachments: len(p.Attachments) > 0,
		Truncated:      p.Truncated,
		ParseError:     p.ParseError,
		RawPath:        rel,
		NotifyEnabled:  w.notify.Load() && !historical, // 账号开关 AND 非历史邮件（DESIGN.md 5.1）
	}
	for _, a := range p.Attachments {
		nm.Attachments = append(nm.Attachments, store.NewAttachment{
			Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, PartIndex: a.PartIndex,
		})
	}

	msgID, err := w.m.store.IngestMessage(nm)
	if errors.Is(err, store.ErrDuplicate) {
		return nil // 已存在：静默跳过（不删 abs，现有记录仍引用它）
	}
	if err != nil {
		if !existed {
			_ = os.Remove(abs) // 入库失败清理本次新写文件；失败则由 maintain 回收孤儿
		}
		return fmt.Errorf("入库 %s/%d: %w", folder, uid, err)
	}
	if w.m.notifier != nil {
		w.m.notifier.Wake()
	}
	// 浏览器通知事件（M4）：仅**非历史**新邮件（历史批量入库不推，防通知轰炸，
	// 与 webhook 的历史过滤同理 DESIGN 5.1）；载荷为单封摘要。
	if w.m.events != nil && !historical {
		if b, err := json.Marshal(map[string]any{
			"id": msgID, "subject": p.Subject, "from": p.FromAddr, "account": w.acct.Email,
		}); err == nil {
			w.m.events.NotifyEvent(string(b))
		}
	}
	slog.Info("message ingested", "account_id", w.acct.ID, "message_id", msgID,
		"folder", folder, "uid", uid, "subject", p.Subject)
	return nil
}

// setNotifyEnabled 更新提醒开关的实时值（spawn 初始化与 Manager.SetNotifyEnabled 共用）。
func (w *worker) setNotifyEnabled(enabled bool) { w.notify.Store(enabled) }

// credentials 解密账号凭据为明文（仅存内存）。
func (w *worker) credentials() (mailclient.Credentials, error) {
	plain, err := w.m.cipher.Decrypt(w.acct.CredentialEnc)
	if err != nil {
		return mailclient.Credentials{}, fmt.Errorf("解密凭据失败（SECRET_KEY 是否变更？）: %w", err)
	}
	return mailclient.Credentials{
		AuthType:     w.acct.AuthType,
		Password:     string(plain),
		ClientID:     w.acct.ClientID,
		RefreshToken: string(plain),
	}, nil
}

// folders 解析账号的文件夹列表，默认 ["INBOX"]（DESIGN.md 3.1）。
func (w *worker) folders() []string {
	var list []string
	if err := json.Unmarshal([]byte(w.acct.Folders), &list); err != nil || len(list) == 0 {
		return []string{"INBOX"}
	}
	return list
}

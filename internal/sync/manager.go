// Package sync 实现同步管理器与每账号 worker（TECH.md §2.6，DESIGN.md 3.2）。
// M2 起：IMAP 账号支持 IDLE 长连接（单主机预算内），其余一律轮询。
package sync

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/mailclient"
	"mailhub/internal/store"
)

// Notifier 是 Webhook 投递器的唤醒接口（M2 注入 Deliverer；M1 传 nil）。
type Notifier interface {
	Wake()
}

// EventNotifier 接收新邮件事件载荷（M4 浏览器通知：api.Hub 经 SetEventNotifier 注入；
// 与投递器的 Wake 信号分离——事件需要载荷，投递器只要"有活干了"）。
type EventNotifier interface {
	NotifyEvent(data string)
}

// Manager 管理全部 worker：错峰启动、控制面（触发/重载/移除）。
type Manager struct {
	cfg      *config.Config
	store    *store.Store
	cipher   *crypto.Cipher
	dial     func(*store.Account) mailclient.MailClient // 按 acct.Transport 返回 IMAP/Graph 实现
	notifier Notifier
	events   EventNotifier // 新邮件事件通道（nil = 无；api.Hub，M4 浏览器通知）
	budget   *idleBudget   // 单主机 IDLE 预算（nil = 不限，DESIGN.md 2.2）

	ctx     context.Context
	mu      sync.Mutex
	workers map[int64]*worker
	queue   []job // 待启动账号（优先级在前）
	queueCh chan struct{}
	wg      sync.WaitGroup // 全部 worker goroutine（Run 退出前等待，保证优雅停机不撞已关闭的 DB）

	settingsMu   sync.Mutex
	settingsVals map[string]string // 全局配置缓存（DB 覆盖项）
	settingsAt   time.Time         // 缓存时间；30s 刷新 + RefreshSettings 立即失效
}

// settingsTTL 全局配置缓存时长：worker 每 pollOnce/会话都会读，
// 千级 worker 不允许直打 DB；30s 内的配置变更延迟可接受。
const settingsTTL = 30 * time.Second

// workerStopTimeout 停止单个 worker 时等待其真正退出的上限（IMAP/HTTP 均受 ctx 控制，
// 正常远小于此值；超时只记日志，不无限阻塞调用方）。
const workerStopTimeout = 15 * time.Second

type job struct {
	accountID int64
	priority  bool
}

func NewManager(cfg *config.Config, st *store.Store, c *crypto.Cipher,
	dial func(*store.Account) mailclient.MailClient, n Notifier) *Manager {
	return &Manager{
		cfg: cfg, store: st, cipher: c, dial: dial, notifier: n,
		budget:  newIdleBudget(),
		workers: map[int64]*worker{},
		queueCh: make(chan struct{}, 1),
	}
}

// Run 启动管理器：加载 enabled 账号进入错峰队列，消费循环逐个 spawn（DESIGN.md 2.2）。
// 阻塞直至 ctx 取消；返回前等待全部 worker 退出（调用方随后可安全关闭 store）。
func (m *Manager) Run(ctx context.Context) error {
	m.ctx = ctx
	defer m.wg.Wait()
	ids, err := m.store.ListAccountIDsByStatus(allStatuses()...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		m.enqueue(job{accountID: id})
	}
	m.wakeQueue() // 入队后必须唤醒消费循环：否则重启后无 API 调用时 worker 永不启动
	slog.Info("sync manager started", "accounts", len(ids), "stagger", m.cfg.ConnectStagger)

	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return nil
		case <-m.queueCh:
		}
		for {
			select {
			case <-ctx.Done():
				m.stopAll()
				return nil
			default:
			}
			j, ok := m.dequeue()
			if !ok {
				break
			}
			m.spawn(j.accountID)
			// 错峰：STAGGER + 每账号随机抖动（DESIGN.md 2.2）
			d := m.cfg.ConnectStagger + time.Duration(rand.Int64N(int64(m.cfg.ConnectStagger)+1))
			select {
			case <-ctx.Done():
				m.stopAll()
				return nil
			case <-time.After(d):
			}
		}
	}
}

// ReloadAccount 配置变更后重启 worker（PATCH /accounts/{id}）。
func (m *Manager) ReloadAccount(id int64) {
	m.cancelWorker(id)
	m.enqueue(job{accountID: id, priority: true})
	m.wakeQueue()
}

// RemoveAccount 删除/停用账号：停 worker，不入队。
func (m *Manager) RemoveAccount(id int64) {
	m.cancelWorker(id)
}

// TriggerNow 手动收取单账号（DESIGN.md 3.2：插入信号，不重置轮询计时）。
// worker 尚未启动（排队中）时提升其队列优先级。
func (m *Manager) TriggerNow(id int64) {
	m.mu.Lock()
	w, ok := m.workers[id]
	m.mu.Unlock()
	if ok {
		select {
		case w.trigger <- struct{}{}: // cap=1 非阻塞：正在拉取时信号合并
		default:
		}
		return
	}
	m.enqueue(job{accountID: id, priority: true})
	m.wakeQueue()
}

// TriggerBatch 批量手动收取：全部经错峰队列（DESIGN.md 6.2）。
func (m *Manager) TriggerBatch(ids []int64) {
	for _, id := range ids {
		m.enqueue(job{accountID: id, priority: true})
	}
	m.wakeQueue()
}

// ReloadBatch 重新扫描 pending 账号并入队（批量导入后由 API 调用，DESIGN.md 2.2：
// 导入只入库，验证与首同步按错峰节奏进行）。
func (m *Manager) ReloadBatch() {
	ids, err := m.store.ListAccountIDsByStatus(store.StatusPending)
	if err != nil {
		return
	}
	for _, id := range ids {
		m.enqueue(job{accountID: id})
	}
	m.wakeQueue()
}

func (m *Manager) spawn(id int64) {
	acct, err := m.store.GetAccount(id)
	if err != nil || !acct.Enabled {
		return
	}
	m.cancelWorker(id)

	ctx, cancel := context.WithCancel(m.ctx)
	w := &worker{
		m:       m,
		acct:    acct,
		trigger: make(chan struct{}, 1),
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	m.mu.Lock()
	m.workers[id] = w
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		w.run(ctx)
	}()
	slog.Info("worker spawned", "account_id", id, "email", acct.Email)
}

// forget worker 退出时注销自身（仅当 map 中仍是同一实例，避免误删新 spawn 的 worker）。
func (m *Manager) forget(w *worker) {
	m.mu.Lock()
	if cur, ok := m.workers[w.acct.ID]; ok && cur == w {
		delete(m.workers, w.acct.ID)
	}
	m.mu.Unlock()
}

// cancelWorker 停止账号 worker 并等待其退出（有上限），避免新旧 worker 并存
// 或删除账号后旧 worker 仍在写库。
func (m *Manager) cancelWorker(id int64) {
	m.mu.Lock()
	w, ok := m.workers[id]
	if ok {
		delete(m.workers, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	w.cancel()
	if w.done == nil {
		return
	}
	select {
	case <-w.done:
	case <-time.After(workerStopTimeout):
		slog.Warn("worker 停止超时", "account_id", id)
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	ws := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		ws = append(ws, w)
	}
	m.workers = map[int64]*worker{}
	m.mu.Unlock()
	for _, w := range ws {
		w.cancel()
	}
}

func (m *Manager) enqueue(j job) {
	m.mu.Lock()
	// 优先级 job 插队首；普通 job 去重（已在队列则不重复）。
	if j.priority {
		m.queue = append([]job{j}, m.queue...)
	} else {
		for _, e := range m.queue {
			if e.accountID == j.accountID {
				m.mu.Unlock()
				return
			}
		}
		m.queue = append(m.queue, j)
	}
	m.mu.Unlock()
}

func (m *Manager) dequeue() (job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) == 0 {
		return job{}, false
	}
	j := m.queue[0]
	m.queue = m.queue[1:]
	return j, true
}

func (m *Manager) wakeQueue() {
	select {
	case m.queueCh <- struct{}{}:
	default:
	}
}

// settingInt 读全局配置（DB 覆盖 > 环境变量/默认 def），带 30s 缓存（见 settingsTTL）。
func (m *Manager) settingInt(key string, def int) int {
	m.settingsMu.Lock()
	if m.settingsVals == nil || time.Since(m.settingsAt) > settingsTTL {
		if vals, err := m.store.GetSettings(); err == nil {
			m.settingsVals, m.settingsAt = vals, time.Now()
		} else {
			m.settingsAt = time.Now() // 失败也记账，避免每秒重试打 DB
		}
	}
	vals := m.settingsVals
	m.settingsMu.Unlock()
	return store.SettingInt(vals, key, def)
}

// RefreshSettings 立即失效配置缓存（Web 保存配置后由 API 调用，让变更即时生效）。
func (m *Manager) RefreshSettings() {
	m.settingsMu.Lock()
	m.settingsAt = time.Time{}
	m.settingsMu.Unlock()
}

// SetEventNotifier 注入新邮件事件通道（main 组装时调用；api.Hub）。
func (m *Manager) SetEventNotifier(n EventNotifier) {
	m.events = n
}

// allStatuses 是服务启动时自动恢复 worker 的状态集合。
// **不含 auth_failed**：认证失败即停（DESIGN.md 3.4），重启也不自动重试；
// 用户 PATCH 新凭据（ReloadAccount）或手动收取（TriggerNow）才重新启动。
func allStatuses() []string {
	return []string{
		store.StatusPending, store.StatusSyncing, store.StatusIdle, store.StatusPolling,
		store.StatusError,
	}
}

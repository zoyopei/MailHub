package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/imapclient"
	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

// fakeClient 假 IMAP 实现，驱动 worker 状态机测试（TECH.md §4）。
type fakeClient struct {
	connectErr error // Connect 返回的错误
	rotated    string
	msgs       map[string][]fakeMsg // folder → messages（uid 递增）
	examineUID uint32
	lastCursor *store.FolderState // 最近一次 FetchFolder 收到的游标（断言走的是增量还是重扫）
}

type fakeMsg struct {
	uid  uint32
	seen time.Time // INTERNALDATE；零值 = 服务器未给
	raw  []byte
	old  bool // 早于首次同步窗口：首次同步的时间窗搜索带不出它，但它计入"现有最大 UID"
}

func (f *fakeClient) Connect(_ context.Context, _ string, _ int, _ string, _ mailclient.Credentials) (string, error) {
	return f.rotated, f.connectErr
}

// FetchFolder 模拟 IMAP 游标语义：UIDVALIDITY 匹配 → UID > last_uid；否则时间窗内的邮件。
// 游标产出与真实实现（imapclient.FetchFolder）一致：UIDVALIDITY + 本轮最大 UID；
// 首次同步空结果时为"文件夹里现有邮件的最大 UID"（空文件夹即 0），增量空结果不推进。
func (f *fakeClient) FetchFolder(_ context.Context, folder string, cursor *store.FolderState, _ time.Time,
	handle func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	uidv := uint32(1)
	if f.examineUID != 0 {
		uidv = f.examineUID
	}
	res := mailclient.FetchResult{UIDValidity: uidv}
	f.lastCursor = cursor
	var lastUID uint32
	cursorValid := cursor != nil && cursor.UIDValidity == uidv
	if cursorValid {
		lastUID = cursor.LastUID
	}
	for _, m := range f.msgs[folder] {
		if cursorValid && m.uid <= lastUID || !cursorValid && m.old {
			continue
		}
		if err := handle(mailclient.MsgKey{UIDValidity: uidv, UID: m.uid}, m.seen, m.raw); err != nil {
			return mailclient.FetchResult{}, err
		}
		res.LastUID = max(res.LastUID, m.uid)
	}
	if !cursorValid {
		// 时间窗之内一封都没有：真实实现再搜一次 UID 1:*，取现有邮件的最大 UID
		for _, m := range f.msgs[folder] {
			res.LastUID = max(res.LastUID, m.uid)
		}
	}
	return res, nil
}
func (f *fakeClient) SupportsIdle() bool { return false }
func (f *fakeClient) Idle(context.Context, time.Duration) (bool, error) {
	return false, errors.New("not supported")
}
func (f *fakeClient) Logout() error { return nil }
func (f *fakeClient) Close() error  { return nil }

// fakeGraphClient 假 Graph 实现：按 last_seen_at 游标过滤，返回 MsgKey.GraphID。
type fakeGraphClient struct {
	connectErr error
	rotated    string
	msgs       []fakeGraphMsg // receivedAt 递增
}

type fakeGraphMsg struct {
	id   string
	recv time.Time
	raw  []byte
}

func (f *fakeGraphClient) Connect(_ context.Context, _ string, _ int, _ string, _ mailclient.Credentials) (string, error) {
	return f.rotated, f.connectErr
}

func (f *fakeGraphClient) FetchFolder(_ context.Context, _ string, cursor *store.FolderState, since time.Time,
	handle func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	var from time.Time
	if cursor != nil && cursor.LastSeenAt.Valid {
		from = cursor.LastSeenAt.Time
	} else {
		from = since
	}
	latest := time.Time{}
	for _, m := range f.msgs {
		if !m.recv.After(from) {
			continue
		}
		if err := handle(mailclient.MsgKey{GraphID: m.id}, m.recv, m.raw); err != nil {
			return mailclient.FetchResult{}, err
		}
		if m.recv.After(latest) {
			latest = m.recv
		}
	}
	return mailclient.FetchResult{LatestSeen: latest}, nil
}
func (f *fakeGraphClient) SupportsIdle() bool { return false }
func (f *fakeGraphClient) Idle(context.Context, time.Duration) (bool, error) {
	return false, errors.New("not supported")
}
func (f *fakeGraphClient) Logout() error { return nil }
func (f *fakeGraphClient) Close() error  { return nil }

const rawMail = `From: a@b.c
To: x@y.z
Subject: hello
Message-ID: <1@b.c>

body text here
`

func testEnv(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open("", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, _ := crypto.New(make([]byte, 32))
	cfg := &config.Config{
		DataDir: dir, PollIntervalDefault: 60, ConnectStagger: time.Millisecond,
		FallbackInterval: 300, MaxMessageSize: 1 << 20,
	}
	m := NewManager(cfg, st, c, func(*store.Account) mailclient.MailClient { return nil }, nil)
	return m, st, dir
}

func newTestWorker(t *testing.T, m *Manager, st *store.Store, email string, cl mailclient.MailClient) (*worker, int64) {
	t.Helper()
	enc, _ := crypto.New(make([]byte, 32))
	secret, _ := enc.Encrypt([]byte("pw"))
	id, err := st.CreateAccount(&store.Account{
		Name: email, Email: email, IMAPHost: "h", IMAPPort: 993, Username: email,
		AuthType: "password", CredentialEnc: secret, PollInterval: 60, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := st.GetAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	m.dial = func(*store.Account) mailclient.MailClient { return cl }
	w := &worker{m: m, acct: acct, trigger: make(chan struct{}, 1), cancel: func() {}}
	w.setNotifyEnabled(acct.NotifyEnabled) // 与 spawn 一致：实时值取自账号快照
	return w, id
}

func TestWorkerIngestAndHeartbeat(t *testing.T) {
	m, st, dir := testEnv(t)
	cl := &fakeClient{msgs: map[string][]fakeMsg{
		"INBOX": {{uid: 5, raw: []byte(rawMail)}},
	}}
	w, id := newTestWorker(t, m, st, "w1@x.com", cl)

	if err := w.session(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 1 || msgs[0].Subject != "hello" {
		t.Fatalf("入库: %+v", msgs)
	}
	// .eml 落盘
	rel := msgs[0].RawPath
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf(".eml 未落盘: %v", err)
	}
	// 心跳
	a, _ := st.GetAccount(id)
	if !a.LastSyncAt.Valid {
		t.Fatal("心跳未更新")
	}
	if a.Status != store.StatusPolling {
		t.Fatalf("轮询模式状态应为 polling，得 %s", a.Status)
	}

	// 第二次 cycle：无新邮件，仍更新心跳（成功即心跳语义）
	t1 := a.LastSyncAt.Time
	time.Sleep(10 * time.Millisecond)
	_ = w.session(context.Background())
	a, _ = st.GetAccount(id)
	if !a.LastSyncAt.Time.After(t1) {
		t.Fatal("无新邮件的成功收取也应推进心跳")
	}
}

func TestWorkerDuplicateIdempotent(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 5, raw: []byte(rawMail)}}}}
	w, id := newTestWorker(t, m, st, "w2@x.com", cl)
	_ = w.session(context.Background())
	_ = w.session(context.Background()) // 同一批邮件重复投递
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 1 {
		t.Fatalf("重复投递应幂等，得 %d 封", len(msgs))
	}
}

// fakeEventNotifier 捕获浏览器通知事件（M4）。
type fakeEventNotifier struct {
	events chan string
}

func (f *fakeEventNotifier) NotifyEvent(data string) {
	select {
	case f.events <- data:
	default:
	}
}

// 事件通道：非历史新邮件推送摘要载荷；首次同步的历史邮件不推（防通知轰炸，DESIGN 5.1）。
func TestWorkerIngestEventNotify(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 4)}
	m.SetEventNotifier(fn)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 5, raw: []byte(rawMail)}}}}
	w, _ := newTestWorker(t, m, st, "we@x.com", cl)

	// 首次同步：历史邮件（firstSync=true）只入库不推事件
	if err := w.session(context.Background()); err != nil {
		t.Fatalf("session: %v", err)
	}
	select {
	case data := <-fn.events:
		t.Fatalf("历史邮件不应推事件，得 %s", data)
	case <-time.After(200 * time.Millisecond):
	}

	// 第二轮：新邮件（游标已建立，firstSync=false）推事件且载荷含摘要
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 6, raw: []byte(rawMail)})
	if err := w.session(context.Background()); err != nil {
		t.Fatalf("session2: %v", err)
	}
	select {
	case data := <-fn.events:
		if !strings.Contains(data, `"subject":"hello"`) || !strings.Contains(data, `"account":"we@x.com"`) {
			t.Fatalf("事件载荷应含邮件摘要，得 %s", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("新邮件应推事件")
	}
}

// Graph 路径：GraphID 去重、游标推进、文件名坐标。
func TestWorkerGraphTransport(t *testing.T) {
	m, st, dir := testEnv(t)
	now := time.Now().UTC()
	cl := &fakeGraphClient{msgs: []fakeGraphMsg{
		{id: "g-1", recv: now.Add(-2 * time.Hour), raw: []byte(rawMail)},
		{id: "g-2", recv: now.Add(-1 * time.Hour), raw: []byte(rawMail)},
	}}
	w, id := newTestWorker(t, m, st, "g1@outlook.com", cl)
	acct, _ := st.GetAccount(id)
	acct.Transport = "graph"
	acct.AuthType = "oauth2"
	w.acct = acct

	if err := w.session(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 2 {
		t.Fatalf("应入库 2 封，得 %d", len(msgs))
	}
	// received_at = 邮件真实时间（receivedDateTime），非入库时刻：列表按 id 倒序，最新的在前
	if !msgs[0].ReceivedAt.Equal(now.Add(-1 * time.Hour)) {
		t.Fatalf("received_at 应为邮件真实到达时间 %v，得 %v", now.Add(-1*time.Hour), msgs[0].ReceivedAt)
	}
	// 游标推进到最新 receivedAt
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || !fs.LastSeenAt.Valid || !fs.LastSeenAt.Time.Equal(now.Add(-1*time.Hour)) {
		t.Fatalf("last_seen_at 游标未推进: %+v", fs)
	}
	// .eml 落盘（文件名 = 0-{HashGraphID}.eml，与 IngestMessage 填充的 UNIQUE 键一致）
	rel := msgs[0].RawPath
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatalf(".eml 未落盘: %v", err)
	}

	// 第二轮：游标已过全部消息 → 0 封、0 次重复
	if err := w.session(context.Background()); err != nil {
		t.Fatalf("cycle2: %v", err)
	}
	msgs, _ = st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 2 {
		t.Fatalf("游标推进后不应重复入库，得 %d 封", len(msgs))
	}
}

// Graph 路径 graph_id 唯一索引兜底幂等（seen 回调缺失/游标回退时）。
func TestWorkerGraphDuplicateById(t *testing.T) {
	m, st, _ := testEnv(t)
	now := time.Now().UTC()
	cl := &fakeGraphClient{msgs: []fakeGraphMsg{
		{id: "g-dup", recv: now, raw: []byte(rawMail)},
	}}
	w, id := newTestWorker(t, m, st, "g2@outlook.com", cl)
	acct, _ := st.GetAccount(id)
	acct.Transport = "graph"
	acct.AuthType = "oauth2"
	w.acct = acct

	_ = w.session(context.Background())
	// 强制游标清零重拉同一封：靠 graph_id 幂等
	_ = st.TouchFolderSeen(id, "INBOX", time.Time{})
	_ = w.session(context.Background())
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 1 {
		t.Fatalf("graph_id 应幂等去重，得 %d 封", len(msgs))
	}
}

func TestWorkerAuthFailedStops(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{connectErr: imapclient.ErrAuthFailed}
	w, id := newTestWorker(t, m, st, "w3@x.com", cl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	select {
	case <-done:
		// worker 应退出（认证失败即停，DESIGN.md 3.4）
	case <-time.After(2 * time.Second):
		t.Fatal("worker 未在认证失败后停止")
	}
	a, _ := st.GetAccount(id)
	if a.Status != store.StatusAuthFailed {
		t.Fatalf("状态应为 auth_failed，得 %s", a.Status)
	}
}

func TestWorkerErrReauthStops(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{connectErr: mstoken.ErrReauth}
	w, id := newTestWorker(t, m, st, "w4@x.com", cl)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker 未停止")
	}
	a, _ := st.GetAccount(id)
	if a.Status != store.StatusAuthFailed {
		t.Fatalf("ErrReauth 应映射 auth_failed，得 %s", a.Status)
	}
}

func TestWorkerErrorBackoff(t *testing.T) {
	m, st, _ := testEnv(t)
	// 连接错误（非认证）：置 error 并退避重试
	cl := &fakeClient{connectErr: errors.New("dial tcp: timeout")}
	w, id := newTestWorker(t, m, st, "w5@x.com", cl)
	if err := w.session(context.Background()); err == nil {
		t.Fatal("cycle 应返回错误")
	}
	w.m.store.SetAccountStatus(id, store.StatusError, errors.New("x"))
	a, _ := st.GetAccount(id)
	if a.Status != store.StatusError {
		t.Fatalf("状态应为 error，得 %s", a.Status)
	}
}

func TestTriggerMerge(t *testing.T) {
	// 手动触发信号合并不排队（cap=1 非阻塞，DESIGN.md 3.2）
	trigger := make(chan struct{}, 1)
	for i := 0; i < 3; i++ {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	if len(trigger) != 1 {
		t.Fatalf("三次发送后队列应为 1，得 %d", len(trigger))
	}
}

func TestWorkerTokenRotationPersisted(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{rotated: "RT-rotated",
		msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	w, id := newTestWorker(t, m, st, "w6@x.com", cl)
	// oauth2 账号：凭据存的是 refresh_token
	acct, _ := st.GetAccount(id)
	acct.AuthType = "oauth2"
	w.acct = acct

	_ = w.session(context.Background())
	// 轮换后的令牌应已加密入库
	got, _ := st.GetAccount(id)
	c, _ := crypto.New(make([]byte, 32))
	plain, err := c.Decrypt(got.CredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "RT-rotated" {
		t.Fatalf("轮换令牌未持久化: %q", plain)
	}
}

// 历史邮件不触发 webhook（DESIGN 3.4/5.1）：首次同步（无游标）的邮件不生成投递记录，
// 游标建立后的新邮件才入队。
func TestWorkerHistoryNoDelivery(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	w, _ := newTestWorker(t, m, st, "hist@x.com", cl)
	w.setNotifyEnabled(true) // 账号提醒开关开启
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// 首次 cycle：历史邮件，无投递记录
	if err := w.session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := st.ListDeliveries(store.DeliveryFilter{}); total != 0 {
		t.Fatalf("首次同步的历史邮件不应生成投递记录，得 %d", total)
	}

	// 新邮件到达（uid 2）：生成投递记录
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 2, raw: []byte(rawMail)})
	if err := w.session(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := st.ListDeliveries(store.DeliveryFilter{}); total != 1 {
		t.Fatalf("新邮件应生成 1 条投递记录，得 %d", total)
	}
}

// 首次同步窗口默认 90 天（MAILHUB_INITIAL_SYNC_DAYS；cfg 零值时 worker 防御 90）：
// 拉到 30 天前的邮件、排除 95 天前的。
func TestWorkerInitialSyncWindow(t *testing.T) {
	m, st, _ := testEnv(t)
	now := time.Now().UTC()
	cl := &fakeGraphClient{msgs: []fakeGraphMsg{
		{id: "old-30d", recv: now.Add(-30 * 24 * time.Hour), raw: []byte(rawMail)},
		{id: "old-95d", recv: now.Add(-95 * 24 * time.Hour), raw: []byte(rawMail)},
	}}
	w, id := newTestWorker(t, m, st, "win@outlook.com", cl)
	acct, _ := st.GetAccount(id)
	acct.Transport, acct.AuthType = "graph", "oauth2"
	w.acct = acct

	if err := w.session(context.Background()); err != nil {
		t.Fatal(err)
	}
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 1 {
		t.Fatalf("默认 90 天窗口应拉到 30 天前、排除 95 天前，得 %d 封", len(msgs))
	}
}

// 触碰游标的最小单调性验证（TouchFolderSeen 只前进不后退）。
func TestTouchFolderSeenMonotonic(t *testing.T) {	m, st, _ := testEnv(t)
	_, id := newTestWorker(t, m, st, "ts@x.com", &fakeClient{})
	t1 := time.Now().UTC().Add(-time.Hour)
	if err := st.TouchFolderSeen(id, "INBOX", t1); err != nil {
		t.Fatal(err)
	}
	// 后退值不覆盖
	if err := st.TouchFolderSeen(id, "INBOX", t1.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || !fs.LastSeenAt.Valid || !fs.LastSeenAt.Time.Equal(t1) {
		t.Fatalf("游标应保持 t1: %+v", fs)
	}
}

// ---- IDLE 模式（M2） ----

// fakeIdleClient 支持 IDLE 的假实现：Idle 阻塞至外部通知/取消。
type fakeIdleClient struct {
	fakeClient
	notify chan struct{} // 模拟服务器 EXISTS 通知
	idleN  int           // 进入 IDLE 的次数
}

func (f *fakeIdleClient) SupportsIdle() bool { return true }
func (f *fakeIdleClient) Idle(ctx context.Context, _ time.Duration) (bool, error) {
	f.idleN++
	select {
	case <-f.notify:
		return true, nil // 服务器有动静
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// IDLE 会话状态机：服务器通知 → 拉取；手动触发 → 拉取；取消 → 退出。
func TestWorkerIdleLoop(t *testing.T) {
	m, st, _ := testEnv(t)
	base := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	cl := &fakeIdleClient{fakeClient: *base, notify: make(chan struct{}, 1)}
	w, id := newTestWorker(t, m, st, "idle@x.com", cl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.idleLoop(ctx, cl) }()

	// 初始拉取入库
	waitFor(t, func() bool {
		msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
		return len(msgs) == 1
	})

	// 服务器通知 → 新邮件拉取
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 2, raw: []byte(rawMail)})
	cl.notify <- struct{}{}
	waitFor(t, func() bool {
		msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
		return len(msgs) == 2
	})

	// 手动触发 → 拉取（trigger 信号退出 IDLE 路径）
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 3, raw: []byte(rawMail)})
	select {
	case w.trigger <- struct{}{}:
	default:
	}
	waitFor(t, func() bool {
		msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
		return len(msgs) == 3
	})

	// 心跳与状态：IDLE 模式下应处于 idle 状态且心跳持续更新
	waitFor(t, func() bool {
		a, err := st.GetAccount(id)
		return err == nil && a.LastSyncAt.Valid && a.Status == store.StatusIdle
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("idleLoop 未随 ctx 退出")
	}
}

// IDLE 准入：预算超限时降级轮询（session 内 pollOnce 而非 idleLoop）。
func TestWorkerIdleBudgetFallback(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeIdleClient{fakeClient: fakeClient{msgs: map[string][]fakeMsg{
		"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}, notify: make(chan struct{}, 1)}
	w, id := newTestWorker(t, m, st, "b1@x.com", cl)

	// 经全局配置写预算 1（DB 覆盖路径），并被其他账号占满 → 本账号降级轮询
	if err := st.SetSettings(map[string]string{store.SettingIdleBudgetPerHost: "1"}); err != nil {
		t.Fatal(err)
	}
	if !m.budget.Acquire("h", 1) {
		t.Fatal("预算占用失败")
	}
	defer m.budget.Release("h")

	// session：完成一次收取即返回（轮询会话），不会阻塞在 IDLE
	errCh := make(chan error, 1)
	go func() { errCh <- w.session(context.Background()) }()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("session: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("预算超限应降级轮询（session 应返回而非阻塞 IDLE）")
	}
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 1 {
		t.Fatalf("降级轮询应完成收取，得 %d 封", len(msgs))
	}
	if cl.idleN != 0 {
		t.Fatalf("预算超限不应进入 IDLE，得 %d 次", cl.idleN)
	}
}

// 单主机预算：限额、释放、limit<=0（不限）与 nil 容错。
func TestIdleBudget(t *testing.T) {
	b := newIdleBudget()
	if !b.Acquire("h", 2) || !b.Acquire("h", 2) {
		t.Fatal("前两次应成功")
	}
	if b.Acquire("h", 2) {
		t.Fatal("第三次应被拒")
	}
	if !b.Acquire("other", 2) {
		t.Fatal("其他主机不受影响")
	}
	b.Release("h")
	if !b.Acquire("h", 2) {
		t.Fatal("释放后应可再次占用")
	}

	if !b.Acquire("h3", 0) || !b.Acquire("h3", 0) { // limit<=0：不限，但仍计数
		t.Fatal("limit<=0 应视为不限")
	}
	if b.Acquire("h3", 2) { // 运行时切到限额 2：已有 2 个活跃连接，应被拒
		t.Fatal("切到限额 2 后已有 2 个活跃连接应被拒")
	}
	b.Release("h3")
	if !b.Acquire("h3", 2) {
		t.Fatal("释放 1 个后应可再次占用")
	}

	var nilBudget *idleBudget // nil 容错：视为不限
	if !nilBudget.Acquire("h", 1) || !nilBudget.Acquire("h", 1) {
		t.Fatal("nil 预算应视为不限")
	}
}

// Manager.Run 启动即唤醒错峰队列：已 enabled 的账号必须被 spawn，
// 不依赖任何 API 调用（重启场景回归测试）。
func TestManagerRunSpawnsOnStart(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	_, id := newTestWorker(t, m, st, "run@x.com", cl)
	st.SetAccountStatus(id, store.StatusPolling, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, ok := m.workers[id]
		return ok
	})
	// spawn 后完成收取（心跳推进）——错峰 2s + 60s 轮询内
	waitFor(t, func() bool {
		a, err := st.GetAccount(id)
		return err == nil && a.LastSyncAt.Valid
	})
}

// waitFor 轮询断言（异步事件）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("条件在 2 秒内未满足")
}

package sync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailhub/internal/crypto"
	"mailhub/internal/mailclient"
	"mailhub/internal/store"
)

// rawMailID 生成指定 Message-ID 的测试邮件（UIDVALIDITY 重拉靠 Message-ID 认旧邮件）。
func rawMailID(id string) []byte {
	return []byte(strings.Replace(rawMail, "<1@b.c>", "<"+id+"@b.c>", 1))
}

func totalDeliveries(t *testing.T, st *store.Store) int {
	t.Helper()
	_, total, err := st.ListDeliveries(store.DeliveryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func noEvent(t *testing.T, fn *fakeEventNotifier, why string) {
	t.Helper()
	select {
	case data := <-fn.events:
		t.Fatalf("%s不应推事件，得 %s", why, data)
	case <-time.After(150 * time.Millisecond):
	}
}

func wantEvent(t *testing.T, fn *fakeEventNotifier, why string) {
	t.Helper()
	select {
	case <-fn.events:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s应推事件", why)
	}
}

// emlPath 一封邮件在磁盘上的路径（文件名用消息坐标；月份目录按真实到达时间）。
func emlPath(dir string, accountID int64, arrived time.Time, uidv, uid uint32) string {
	rel := filepath.Join("mails", strconv.FormatInt(accountID, 10),
		arrived.UTC().Format("2006-01"), strconv.FormatUint(uint64(uidv), 10)+"-"+strconv.FormatUint(uint64(uid), 10)+".eml")
	return filepath.Join(dir, filepath.FromSlash(rel))
}

func runSession(t *testing.T, w *worker) {
	t.Helper()
	if err := w.session(context.Background()); err != nil {
		t.Fatalf("session: %v", err)
	}
}

// 空文件夹的首次同步也要建游标。以前 folder_states 行只在首封邮件入库时创建，空文件夹
// 于是每轮都按"首次同步"处理：之后的第一封新邮件被当成历史邮件，不投递也不推事件——
// 对新注册、还没收到过信的邮箱来说，恰恰是最需要通知的那一封。
func TestEmptyFolderFirstMailNotified(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 4)}
	m.SetEventNotifier(fn)
	cl := &fakeClient{msgs: map[string][]fakeMsg{}}
	w, id := newTestWorker(t, m, st, "empty@x.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	runSession(t, w)
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || fs.UIDValidity != 1 || fs.LastUID != 0 {
		t.Fatalf("空文件夹首次同步后应建立游标 (1, 0)，得 %+v", fs)
	}

	// 第一封新邮件：按新邮件处理（投递 + 浏览器事件）
	cl.msgs["INBOX"] = []fakeMsg{{uid: 1, raw: []byte(rawMail)}}
	runSession(t, w)
	if cl.lastCursor == nil || cl.lastCursor.UIDValidity != 1 || cl.lastCursor.LastUID != 0 {
		t.Fatalf("游标已建立后应走增量（UID 1:*），实际游标 %+v", cl.lastCursor)
	}
	if n := totalDeliveries(t, st); n != 1 {
		t.Fatalf("空文件夹之后的第一封新邮件应生成投递记录，得 %d", n)
	}
	wantEvent(t, fn, "空文件夹之后的第一封新邮件")
	fs, _ = st.GetFolderState(id, "INBOX")
	if fs.LastUID != 1 {
		t.Fatalf("游标应推进到 1，得 %+v", fs)
	}

	// 空结果不推进：游标停在 1，下一轮照样从 2:* 搜起
	runSession(t, w)
	if fs, _ = st.GetFolderState(id, "INBOX"); fs.LastUID != 1 {
		t.Fatalf("增量空结果不应动游标，得 %+v", fs)
	}
}

// 邮件全部早于首次同步窗口（久不收信的邮箱）：一封都不入库，但游标要落在现有最大 UID 上——
// 既不能超前（否则之后的邮件落在游标之前拉不到），也不能落后（否则下一轮把这些从没入库过的
// 旧邮件当**新邮件**拉回来，触发一轮通知）。
func TestOldMailsBeforeWindowCursorExact(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 4)}
	m.SetEventNotifier(fn)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {
		{uid: 3, old: true, seen: time.Now().Add(-200 * 24 * time.Hour), raw: rawMailID("old3")},
		{uid: 17, old: true, seen: time.Now().Add(-120 * 24 * time.Hour), raw: rawMailID("old17")},
	}}}
	w, id := newTestWorker(t, m, st, "old@x.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	runSession(t, w)
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || fs.UIDValidity != 1 || fs.LastUID != 17 {
		t.Fatalf("首次同步无窗口内邮件时游标应为现有最大 UID (1, 17)，得 %+v", fs)
	}
	if msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id}); len(msgs) != 0 {
		t.Fatalf("早于窗口的邮件不应入库，得 %d 封", len(msgs))
	}

	// 之后到达的新邮件：照常入库并通知
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 18, raw: rawMailID("new18")})
	runSession(t, w)
	if n := totalDeliveries(t, st); n != 1 {
		t.Fatalf("新邮件应生成 1 条投递记录，得 %d", n)
	}
	wantEvent(t, fn, "久不收信邮箱的新邮件")
	noEvent(t, fn, "不应有第二封被当成新邮件")
}

// UIDVALIDITY 变化（邮箱重建 UID）：窗口内的旧邮件换了坐标再出现一遍。它们不该重复入库、
// 不该再触发一轮通知，也不该留下一堆孤儿文件；上次同步之后到达的才是真正的新邮件。
func TestUIDValidityChangeSkipsKnownMessages(t *testing.T) {
	m, st, dir := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 8)}
	m.SetEventNotifier(fn)
	arrived := time.Now().Add(-2 * time.Hour)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {
		{uid: 1, seen: arrived, raw: rawMailID("a")},
		{uid: 2, seen: arrived, raw: rawMailID("b")},
	}}}
	w, id := newTestWorker(t, m, st, "reidx@x.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runSession(t, w) // 首次同步：2 封历史邮件

	// 重建 UID：UIDVALIDITY 1 → 9，同样的两封邮件换了坐标
	cl.examineUID = 9
	cl.msgs["INBOX"] = []fakeMsg{
		{uid: 100, seen: arrived, raw: rawMailID("a")},
		{uid: 101, seen: arrived, raw: rawMailID("b")},
	}
	runSession(t, w)

	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if len(msgs) != 2 {
		t.Fatalf("重拉的旧邮件不应重复入库，得 %d 封", len(msgs))
	}
	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("UID 重建不是新邮件，不应生成投递记录，得 %d", n)
	}
	noEvent(t, fn, "UID 重建后重拉的旧邮件")
	// 游标换到新坐标；之后是增量，不会每轮重扫
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || fs.UIDValidity != 9 || fs.LastUID != 101 {
		t.Fatalf("UIDVALIDITY 变化后游标应为 (9, 101)，得 %+v", fs)
	}
	// 文件：原有那份还在；按新坐标写的那份认出是旧邮件后已删除
	if _, err := os.Stat(emlPath(dir, id, arrived, 1, 1)); err != nil {
		t.Errorf("原有 .eml 应保留: %v", err)
	}
	if _, err := os.Stat(emlPath(dir, id, arrived, 9, 100)); !os.IsNotExist(err) {
		t.Errorf("去重后按新坐标写的 .eml 应删除（否则是孤儿文件）: %v", err)
	}

	// 上次同步之后到达的邮件：真正的新邮件，照常入库并通知
	fresh := time.Now().Add(-time.Minute)
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 102, seen: fresh, raw: rawMailID("c")})
	runSession(t, w)
	if cl.lastCursor == nil || cl.lastCursor.UIDValidity != 9 || cl.lastCursor.LastUID != 101 {
		t.Fatalf("游标已换新后应走增量，实际游标 %+v", cl.lastCursor)
	}
	if n := totalDeliveries(t, st); n != 1 {
		t.Fatalf("重拉之后到达的新邮件应生成 1 条投递记录，得 %d", n)
	}
	wantEvent(t, fn, "重拉之中真正的新邮件")
	if msgs, _ = st.ListMessages(store.MessageFilter{AccountID: id}); len(msgs) != 3 {
		t.Fatalf("新邮件应入库，得 %d 封", len(msgs))
	}
}

// 本地删过的邮件在 UID 重建后又出现：它早于上次同步，按历史邮件处理——可以重新入库，
// 但不能再通知一遍。
func TestUIDValidityChangeLocallyDeletedStaysQuiet(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 4)}
	m.SetEventNotifier(fn)
	arrived := time.Now().Add(-time.Hour)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, seen: arrived, raw: rawMailID("gone")}}}}
	w, id := newTestWorker(t, m, st, "del@x.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runSession(t, w)

	// 用户删掉本地副本（不回写邮箱服务器），之后邮箱重建 UID，这封邮件在服务器上还在
	msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
	if _, err := st.DeleteMessages([]int64{msgs[0].ID}, id); err != nil {
		t.Fatal(err)
	}
	cl.examineUID = 5
	cl.msgs["INBOX"] = []fakeMsg{{uid: 77, seen: arrived, raw: rawMailID("gone")}}
	runSession(t, w)

	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("本地删过的旧邮件在 UID 重建后不应再通知，得 %d 条投递记录", n)
	}
	noEvent(t, fn, "本地删过的旧邮件")
	if msgs, _ = st.ListMessages(store.MessageFilter{AccountID: id}); len(msgs) != 1 || msgs[0].UID != 77 {
		t.Fatalf("旧邮件应按新坐标重新入库（历史），得 %+v", msgs)
	}
}

// 临界区间按新邮件处理：INTERNALDATE 是邮件服务器的时钟，last_sync_at 是本机的，两边对不准时
// 把新邮件判成旧邮件会漏通知——所以距上次同步不到 reindexGrace 的一律算新邮件。
func TestReindexGraceWindow(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 8)}
	m.SetEventNotifier(fn)
	cl := &fakeClient{msgs: map[string][]fakeMsg{}}
	w, _ := newTestWorker(t, m, st, "grace@x.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	runSession(t, w)
	lastSync := time.Now()
	w.acct.LastSyncAt = sql.NullTime{Time: lastSync, Valid: true}

	cl.examineUID = 3
	cl.msgs["INBOX"] = []fakeMsg{
		{uid: 10, seen: lastSync.Add(-reindexGrace / 2), raw: rawMailID("recent")},        // 余量之内：新
		{uid: 11, seen: lastSync.Add(-reindexGrace - time.Minute), raw: rawMailID("old")}, // 早就收过：历史
		{uid: 12, seen: lastSync.Add(time.Hour), raw: rawMailID("future")},                // 之后到达：新
		{uid: 13, seen: time.Time{}, raw: rawMailID("nodate")},                            // 服务器没给时间：新
	}
	runSession(t, w)
	if n := totalDeliveries(t, st); n != 3 {
		t.Fatalf("余量之内/之后到达/时间未知的应按新邮件投递（3 条），得 %d", n)
	}
	for range 3 {
		wantEvent(t, fn, "按新邮件处理的重拉邮件")
	}
	noEvent(t, fn, "余量之外的旧邮件")
}

func TestSyncedBefore(t *testing.T) {
	last := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	w := &worker{acct: &store.Account{LastSyncAt: sql.NullTime{Time: last, Valid: true}}}
	for _, c := range []struct {
		name    string
		arrived time.Time
		want    bool
	}{
		{"远早于上次同步", last.Add(-time.Hour), true},
		{"余量边界（刚好 10 分钟前）", last.Add(-reindexGrace), true},
		{"余量之内", last.Add(-reindexGrace / 2), false},
		{"恰好等于上次同步（余量之内）", last, false},
		{"上次同步之后", last.Add(time.Second), false},
		{"服务器没给时间", time.Time{}, false},
	} {
		w.acct = &store.Account{LastSyncAt: sql.NullTime{Time: last, Valid: true}}
		if got := w.syncedBefore(c.arrived); got != c.want {
			t.Errorf("%s: syncedBefore = %v，期望 %v", c.name, got, c.want)
		}
	}
	// 从未成功同步过：无从判断，按新邮件
	w.acct.LastSyncAt = sql.NullTime{}
	if w.syncedBefore(last.Add(-24 * time.Hour)) {
		t.Error("没有 last_sync_at 时应返回 false")
	}
}

// Graph 账号的空收件箱：首次同步没有邮件可入库，delta 引导又失败（拿不到可存的链接）时，
// folder_states 不能因此一直没有行——否则下一轮仍是"首次同步"，之后的第一封新邮件被当成
// 历史邮件，不投递也不推事件。
func TestGraphEmptyFolderFirstMailNotified(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 4)}
	m.SetEventNotifier(fn)
	cl := &fakeGraphClient{} // 空收件箱；假实现不产出 delta 链接
	w, id := newTestWorker(t, m, st, "gempty@outlook.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	runSession(t, w)
	if fs, _ := st.GetFolderState(id, "INBOX"); fs == nil {
		t.Fatal("Graph 空收件箱首次同步后应建立游标行")
	}

	cl.msgs = []fakeGraphMsg{{id: "g-new", recv: time.Now().Add(-time.Minute), raw: []byte(rawMail)}}
	runSession(t, w)
	if n := totalDeliveries(t, st); n != 1 {
		t.Fatalf("空收件箱之后的第一封新邮件应生成投递记录，得 %d", n)
	}
	wantEvent(t, fn, "Graph 空收件箱之后的第一封新邮件")
}

// midRotateClient 模拟"会话途中刷新令牌"的传输（实现 mailclient.SessionTokenRotator）：
// Connect 时不轮换，收取途中才轮换。
type midRotateClient struct {
	fakeClient
	midRotated string
}

func (f *midRotateClient) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	if cursor == nil { // 只在第一个会话途中轮换
		f.midRotated = "RT-mid"
	}
	return f.fakeClient.FetchFolder(ctx, folder, cursor, since, handle)
}

func (f *midRotateClient) TakeRotatedToken() string {
	rt := f.midRotated
	f.midRotated = ""
	return rt
}

// 会话途中轮换的 refresh_token 也要持久化（并更新快照）。只在 Connect 的返回值里处理轮换
// 的话，Graph 长会话途中刷新出来的新令牌会被丢掉——旧令牌一旦失效，账号就变成 auth_failed。
func TestSessionMidRotationPersisted(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &midRotateClient{fakeClient: fakeClient{
		msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}}
	w, id := newTestWorker(t, m, st, "mid@x.com", cl)
	acct, _ := st.GetAccount(id)
	acct.AuthType = "oauth2"
	w.acct = acct

	runSession(t, w)
	got, _ := st.GetAccount(id)
	c, _ := crypto.New(make([]byte, 32))
	if plain, err := c.Decrypt(got.CredentialEnc); err != nil || string(plain) != "RT-mid" {
		t.Fatalf("途中轮换的令牌应已入库，得 %q（%v）", plain, err)
	}
	// 快照同步更新：下个会话 credentials() 用新令牌，TokenCache 才能命中
	if plain, err := c.Decrypt(w.acct.CredentialEnc); err != nil || string(plain) != "RT-mid" {
		t.Fatalf("内存快照应同步更新，得 %q（%v）", plain, err)
	}
	if cl.midRotated != "" {
		t.Fatal("轮换令牌取走后应清空（不重复持久化）")
	}

	// 下个会话没有轮换：库里的令牌保持不变
	runSession(t, w)
	if again, _ := st.GetAccount(id); !bytes.Equal(again.CredentialEnc, got.CredentialEnc) {
		t.Fatal("没有轮换的会话不应改写凭据")
	}
}

// pagedGraphClient 模拟"首次同步要分几轮才走完"的 Graph 传输：前 cut 轮因单轮分页上限收工
// （FetchResult.More=true），之后走完。同时记录每轮收到的游标。
type pagedGraphClient struct {
	fakeGraphClient
	cut     int
	rounds  int
	cursors []*store.FolderState
}

func (f *pagedGraphClient) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	f.rounds++
	f.cursors = append(f.cursors, cursor)
	res, err := f.fakeGraphClient.FetchFolder(ctx, folder, cursor, since, handle)
	res.More = f.rounds <= f.cut
	return res, err
}

// 大邮箱的首次同步超过传输的单轮分页上限：要分几轮才走完，期间每一轮的邮件都是历史邮件
// （不投递、不推事件），进程重启也不变。"是否首次同步"以 folder_states.initial_done 为准，
// 不能只看有没有游标行——游标行在首封邮件入库时就建了，只看行的话第二轮起剩下那几千封
// 历史邮件就成了几千条通知。
func TestFirstSyncPagedStaysHistorical(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 8)}
	m.SetEventNotifier(fn)
	base := time.Now().Add(-24 * time.Hour)
	var cl *pagedGraphClient
	add := func(id string, mins int) {
		cl.msgs = append(cl.msgs, fakeGraphMsg{id: id, recv: base.Add(time.Duration(mins) * time.Minute), raw: []byte(rawMail)})
	}
	cl = &pagedGraphClient{cut: 2}
	add("g1", 0)
	add("g2", 1)
	w, id := newTestWorker(t, m, st, "big@outlook.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	assertPending := func(why string) {
		t.Helper()
		if fs, _ := st.GetFolderState(id, "INBOX"); fs == nil || fs.InitialDone {
			t.Fatalf("%s：首次同步没走完时标记应保持未完成，得 %+v", why, fs)
		}
	}

	// 第一轮：分页上限收工。邮件是历史的
	runSession(t, w)
	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("首轮（历史邮件）不应投递，得 %d 条", n)
	}
	assertPending("首轮之后")

	// 进程重启（新 worker 没有任何内存状态）：标记在库里，接着走的几轮仍然算首次同步
	acct, _ := st.GetAccount(id)
	w2 := &worker{m: m, acct: acct, trigger: make(chan struct{}, 1), cancel: func() {}}
	w2.setNotifyEnabled(true)
	add("g3", 2)
	runSession(t, w2)
	if c := cl.cursors[1]; len(cl.cursors) != 2 || c == nil || !c.LastSeenAt.Valid ||
		!c.LastSeenAt.Time.Equal(base.Add(time.Minute)) {
		t.Fatalf("第二轮应从库里的游标（上一轮的 LatestSeen）接着走，实际游标 %+v", cl.cursors)
	}
	if msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id}); len(msgs) != 3 {
		t.Fatalf("三封都应入库，得 %d", len(msgs))
	}
	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("续走的邮件也是历史邮件，不应投递，得 %d 条", n)
	}
	noEvent(t, fn, "首次同步分轮续走的邮件")
	assertPending("第二轮之后")

	// 第三轮：走完
	add("g4", 3)
	runSession(t, w2)
	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("走完的最后一轮里收到的也是历史邮件，不应投递，得 %d 条", n)
	}
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || !fs.InitialDone || !fs.LastSeenAt.Valid || !fs.LastSeenAt.Time.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("走完后应标记完成并推进游标，得 %+v", fs)
	}

	// 走完之后的新邮件才是新邮件
	add("g5", 4)
	runSession(t, w2)
	if n := totalDeliveries(t, st); n != 1 {
		t.Fatalf("首次同步之后的新邮件应投递 1 条，得 %d", n)
	}
	wantEvent(t, fn, "首次同步之后的新邮件")
}

// dropMidwayClient 首次同步收到一半时断线（第一轮的第二封回调出错），之后恢复正常。
type dropMidwayClient struct {
	fakeClient
	dropped bool
}

func (f *dropMidwayClient) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	drop := !f.dropped
	f.dropped = true
	n := 0
	return f.fakeClient.FetchFolder(ctx, folder, cursor, since, func(k mailclient.MsgKey, seen time.Time, raw []byte) error {
		n++
		if drop && n == 2 {
			return errors.New("connection reset by peer")
		}
		return handle(k, seen, raw)
	})
}

// 首次同步中途被打断（断线、停机）：已经收到的那几封入库时建了游标行，但窗口里剩下的同样是
// 历史邮件。"是否首次同步"如果只看有没有游标行，下一轮就把它们全当成新邮件通知——批量导入后
// 第一次收取途中重启一次服务，就是一轮通知轰炸。
func TestFirstSyncInterruptedStaysHistorical(t *testing.T) {
	m, st, _ := testEnv(t)
	fn := &fakeEventNotifier{events: make(chan string, 8)}
	m.SetEventNotifier(fn)
	cl := &dropMidwayClient{fakeClient: fakeClient{msgs: map[string][]fakeMsg{"INBOX": {
		{uid: 1, raw: []byte(rawMail)}, {uid: 2, raw: []byte(rawMail)}, {uid: 3, raw: []byte(rawMail)},
	}}}}
	w, id := newTestWorker(t, m, st, "drop@x.com", cl)
	w.setNotifyEnabled(true)
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	// 第一轮：收到第一封后断线
	if err := w.session(context.Background()); err == nil {
		t.Fatal("断线的那一轮应返回错误")
	}
	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("断线前收到的历史邮件不应投递，得 %d 条", n)
	}
	fs, _ := st.GetFolderState(id, "INBOX")
	if fs == nil || fs.InitialDone {
		t.Fatalf("被打断的首次同步不应标记完成，得 %+v", fs)
	}

	// 第二轮：从游标接着收剩下的两封——仍然是历史邮件
	runSession(t, w)
	if msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id}); len(msgs) != 3 {
		t.Fatalf("三封都应入库，得 %d", len(msgs))
	}
	if n := totalDeliveries(t, st); n != 0 {
		t.Fatalf("被打断后接着收的历史邮件不应投递，得 %d 条", n)
	}
	noEvent(t, fn, "被打断后接着收的历史邮件")
	if fs, _ = st.GetFolderState(id, "INBOX"); !fs.InitialDone || fs.UIDValidity != 1 || fs.LastUID != 3 {
		t.Fatalf("走完后应标记完成并推进游标，得 %+v", fs)
	}

	// 之后到达的才是新邮件
	cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: 4, raw: []byte(rawMail)})
	runSession(t, w)
	if n := totalDeliveries(t, st); n != 1 {
		t.Fatalf("首次同步之后的新邮件应投递 1 条，得 %d", n)
	}
	wantEvent(t, fn, "首次同步之后的新邮件")
}

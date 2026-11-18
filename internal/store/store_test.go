package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open("", filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustAccount(t *testing.T, st *Store, email string) int64 {
	t.Helper()
	id, err := st.CreateAccount(&Account{
		Name: email, Email: email, IMAPHost: "imap.example.com", IMAPPort: 993,
		Username: email, AuthType: "password", CredentialEnc: []byte("enc"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ingest(t *testing.T, st *Store, m *NewMessage) int64 {
	t.Helper()
	id, err := st.IngestMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMigrationsAndAccountCRUD(t *testing.T) {
	st := testStore(t)

	id := mustAccount(t, st, "a@x.com")
	if _, err := st.GetAccount(id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAccountByEmail("a@x.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAccount(999); err != ErrNotFound {
		t.Errorf("期望 ErrNotFound，得 %v", err)
	}

	// 更新凭据与心跳
	if err := st.UpdateCredential(id, []byte("enc2")); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchAccount(id); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAccount(id)
	if !a.LastSyncAt.Valid || a.LastSyncAt.Time.After(time.Now().Add(time.Minute)) {
		t.Errorf("心跳时间异常: %v", a.LastSyncAt)
	}

	// 列表筛选
	mustAccount(t, st, "b@x.com")
	list, total, err := st.ListAccounts(AccountFilter{Q: "a@x", Page: 1, Limit: 10})
	if err != nil || total != 1 || len(list) != 1 {
		t.Errorf("Q 筛选: total=%d list=%d err=%v", total, len(list), err)
	}
	st.SetAccountStatus(id, StatusAuthFailed, nil)
	list, _, _ = st.ListAccounts(AccountFilter{Status: StatusAuthFailed})
	if len(list) != 1 {
		t.Errorf("Status 筛选失败: %d", len(list))
	}
}

func TestIngestMessageIdempotentAndCursor(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "a@x.com")

	m1 := &NewMessage{
		AccountID: acct, Folder: "INBOX", UIDValidity: 7, UID: 100,
		MessageID: "<1@x>", Subject: "第一封", FromAddr: "a@b.c", RawPath: "mails/1/2026-11/7-100.eml",
	}
	id1, err := st.IngestMessage(m1)
	if err != nil {
		t.Fatal(err)
	}

	// 重复投递：UNIQUE 幂等
	if _, err := st.IngestMessage(m1); err != ErrDuplicate {
		t.Errorf("重复应返回 ErrDuplicate，得 %v", err)
	}

	// 游标推进
	fs1, _ := st.GetFolderState(acct, "INBOX")
	if fs1 == nil || fs1.LastUID != 100 || fs1.UIDValidity != 7 {
		t.Fatalf("folder_state = %+v", fs1)
	}

	m2 := &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 7, UID: 205,
		MessageID: "<2@x>", Subject: "第二封", RawPath: "mails/1/2026-11/7-205.eml"}
	ingest(t, st, m2)
	fs2, _ := st.GetFolderState(acct, "INBOX")
	if fs2.LastUID != 205 {
		t.Errorf("last_uid 应推进到 205，得 %d", fs2.LastUID)
	}

	// UIDVALIDITY 变化：游标归零重写
	m3 := &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 9, UID: 3,
		MessageID: "<3@x>", Subject: "重建", RawPath: "mails/1/2026-11/9-3.eml"}
	ingest(t, st, m3)
	fs3, _ := st.GetFolderState(acct, "INBOX")
	if fs3.UIDValidity != 9 || fs3.LastUID != 3 {
		t.Errorf("UIDVALIDITY 重置: %+v", fs3)
	}

	// 乱序 UID：last_uid 不回退
	m4 := &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 9, UID: 2,
		MessageID: "<4@x>", RawPath: "mails/1/2026-11/9-2.eml"}
	ingest(t, st, m4)
	fs4, _ := st.GetFolderState(acct, "INBOX")
	if fs4.LastUID != 3 {
		t.Errorf("乱序时 last_uid 不应回退: %d", fs4.LastUID)
	}
	_ = id1
}

func TestIngestNotifyFilter(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "n@x.com")

	// 插入一个 webhook（account_ids=null = 全部账号）
	res, err := st.DB.Exec(
		`INSERT INTO webhooks (url, secret, enabled, created_at) VALUES ('http://h', 's', 1, datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}
	whID, _ := res.LastInsertId()

	// 账号关提醒：不生成 deliveries（DESIGN.md 5.1 第一层过滤）
	m := &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 1,
		RawPath: "x", NotifyEnabled: false}
	ingest(t, st, m)
	pending, _, _ := st.PendingDeliveryCount()
	if pending != 0 {
		t.Errorf("关提醒时不应生成 deliveries，得 %d", pending)
	}

	// 账号开提醒：生成 pending
	m2 := &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 2,
		RawPath: "y", NotifyEnabled: true}
	ingest(t, st, m2)
	pending, _, _ = st.PendingDeliveryCount()
	if pending != 1 {
		t.Errorf("开提醒应生成 1 条 deliveries，得 %d", pending)
	}
	_ = whID
}

func TestMessageQueries(t *testing.T) {
	st := testStore(t)
	a1 := mustAccount(t, st, "a1@x.com")
	a2 := mustAccount(t, st, "a2@x.com")

	ingest(t, st, &NewMessage{AccountID: a1, Folder: "INBOX", UIDValidity: 1, UID: 1,
		Subject: "验证码 123", FromAddr: "svc@x.com", BodyText: "hi", RawPath: "p1", Snippet: "hi"})
	ingest(t, st, &NewMessage{AccountID: a1, Folder: "INBOX", UIDValidity: 1, UID: 2,
		Subject: "账单", FromAddr: "bill@x.com", RawPath: "p2"})
	ingest(t, st, &NewMessage{AccountID: a2, Folder: "INBOX", UIDValidity: 1, UID: 1,
		Subject: "另一账号", RawPath: "p3"})

	// 列表 + 筛选
	msgs, err := st.ListMessages(MessageFilter{AccountID: a1})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("ListMessages: %v %d", err, len(msgs))
	}
	msgs, _ = st.ListMessages(MessageFilter{Q: "验证码"})
	if len(msgs) != 1 {
		t.Errorf("Q 筛选: %d", len(msgs))
	}

	// 已读
	if err := st.SetRead(msgs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	unread := false
	msgs, _ = st.ListMessages(MessageFilter{IsRead: &unread})
	if len(msgs) != 2 {
		t.Errorf("未读筛选: %d", len(msgs))
	}

	// 批量已读（按账号）。SQLite 的 UPDATE RowsAffected 计匹配行（含已读行），放宽断言。
	n, _ := st.BatchRead(nil, a1)
	if n < 1 || n > 2 {
		t.Errorf("BatchRead 影响 %d 行（账号 a1 至少剩 1 封未读）", n)
	}

	// 批量删除（按账号）→ 返回路径
	paths, err := st.DeleteMessages(nil, a1)
	if err != nil || len(paths) != 2 {
		t.Fatalf("DeleteMessages: %v %v", paths, err)
	}

	// 聚合统计
	stats, err := st.StatsByAccounts([]int64{a1, a2})
	if err != nil {
		t.Fatalf("StatsByAccounts err: %v", err)
	}
	if stats[a2].Count != 1 || stats[a2].Unread != 1 {
		t.Errorf("StatsByAccounts = %+v", stats)
	}
	if !stats[a2].LastMsg.Valid {
		t.Errorf("LastMsg 应有效")
	}
}

// 空文件夹（无 folder_states 行）保存 delta 游标必须建行：否则每轮按"首次"全窗口重扫
// （给已有账号新增文件夹、或空收件箱账号的场景）。
func TestSetFolderDeltaUpsert(t *testing.T) {
	st := testStore(t)
	a := mustAccount(t, st, "d@x.com")

	if err := st.SetFolderDelta(a, "JunkEmail", "https://graph/delta?t=1"); err != nil {
		t.Fatal(err)
	}
	fs, err := st.GetFolderState(a, "JunkEmail")
	if err != nil || fs == nil {
		t.Fatalf("空文件夹应建立游标行: %v %v", fs, err)
	}
	if fs.DeltaLink != "https://graph/delta?t=1" {
		t.Fatalf("delta_link 应为写入值，得 %q", fs.DeltaLink)
	}

	// 已有行时覆盖为新链接；空链接不落库
	if err := st.SetFolderDelta(a, "JunkEmail", "https://graph/delta?t=2"); err != nil {
		t.Fatal(err)
	}
	fs, _ = st.GetFolderState(a, "JunkEmail")
	if fs.DeltaLink != "https://graph/delta?t=2" {
		t.Fatalf("覆盖失败，得 %q", fs.DeltaLink)
	}
	if err := st.SetFolderDelta(a, "JunkEmail", ""); err != nil {
		t.Fatal(err)
	}
	fs, _ = st.GetFolderState(a, "JunkEmail")
	if fs.DeltaLink != "https://graph/delta?t=2" {
		t.Fatalf("空链接不应改写，得 %q", fs.DeltaLink)
	}
}

// 排序（M4 邮件页）：time 升/降序按 received_at；account 按账号名（组内时间倒序）；OFFSET 分页。
func TestMessageSort(t *testing.T) {
	st := testStore(t)
	a1 := mustAccount(t, st, "a1@x.com")
	a2 := mustAccount(t, st, "b2@x.com")
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		ingest(t, st, &NewMessage{AccountID: []int64{a1, a2, a1}[i], Folder: "INBOX",
			UIDValidity: 1, UID: uint32(i + 1), RawPath: "p",
			ReceivedAt: base.Add(time.Duration(i) * time.Minute)})
	}

	// time 升序：最早在前
	ms, err := st.ListMessages(MessageFilter{Sort: "time", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(ms); i++ {
		if ms[i].ReceivedAt.Before(ms[i-1].ReceivedAt) {
			t.Fatalf("time 升序错乱: %v 在 %v 前", ms[i].ReceivedAt, ms[i-1].ReceivedAt)
		}
	}
	// time 降序：最新在前
	ms, _ = st.ListMessages(MessageFilter{Sort: "time", Order: "desc"})
	if !ms[0].ReceivedAt.After(ms[len(ms)-1].ReceivedAt) {
		t.Fatal("time 降序错乱")
	}

	// account 排序：账号 a1 的名称（a1@x.com）排在 b2 前
	ms, err = st.ListMessages(MessageFilter{Sort: "account", Order: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || ms[0].AccountID != a1 || ms[1].AccountID != a1 {
		t.Fatalf("account 升序应 a1 的两封在前，得 %v", acctIDs(ms))
	}
	// account 降序 + OFFSET 翻页：第二页只剩 a1 的一封
	ms, err = st.ListMessages(MessageFilter{Sort: "account", Order: "desc", Limit: 2, Offset: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].AccountID != a1 {
		t.Fatalf("account 降序 offset=2 应剩 a1 一封，得 %v", acctIDs(ms))
	}
}

func acctIDs(ms []*Message) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.AccountID
	}
	return out
}

// 计数与列表同筛选语义（前端"已加载 X / 共 Y"与 hasMore 依据）。
func TestCountMessages(t *testing.T) {
	st := testStore(t)
	a1 := mustAccount(t, st, "c1@x.com")
	a2 := mustAccount(t, st, "c2@x.com")
	ingest(t, st, &NewMessage{AccountID: a1, Folder: "INBOX", UIDValidity: 1, UID: 1, RawPath: "p"})
	ingest(t, st, &NewMessage{AccountID: a1, Folder: "INBOX", UIDValidity: 1, UID: 2, RawPath: "p"})
	ingest(t, st, &NewMessage{AccountID: a2, Folder: "INBOX", UIDValidity: 1, UID: 1, RawPath: "p"})

	n, err := st.CountMessages(MessageFilter{})
	if err != nil || n != 3 {
		t.Fatalf("总数应 3，得 %d %v", n, err)
	}
	n, _ = st.CountMessages(MessageFilter{AccountID: a1})
	if n != 2 {
		t.Fatalf("账号过滤应 2，得 %d", n)
	}
	// 与 LIMIT/OFFSET 无关（游标语义不收窄计数）
	n, _ = st.CountMessages(MessageFilter{Limit: 1, BeforeID: 999})
	if n != 3 {
		t.Fatalf("计数不应受分页参数影响，得 %d", n)
	}
	// hasMore 语义：limit 2 取 2 条 < total 3
	ms, _ := st.ListMessages(MessageFilter{Limit: 2})
	if len(ms) != 2 || len(ms) >= n {
		t.Fatalf("分页/计数矛盾: %d vs %d", len(ms), n)
	}
}

func TestNullTimeScan(t *testing.T) { // 确认 sql.NullTime 在 DSN 无 _loc 参数下正常往返（时间统一 UTC 存取）。
	st := testStore(t)
	a := mustAccount(t, st, "t@x.com")
	ingest(t, st, &NewMessage{AccountID: a, Folder: "INBOX", UIDValidity: 1, UID: 1, RawPath: "p"})
	msgs, _ := st.ListMessages(MessageFilter{})
	if !msgs[0].ReceivedAt.After(time.Now().Add(-time.Hour)) {
		t.Errorf("received_at 往返异常: %v", msgs[0].ReceivedAt)
	}
	var ts sql.NullTime
	_ = ts
}

// 全文搜索：q 命中正文/snippet（M4，DESIGN.md 335 的 LIKE 路线）。
func TestSearchBodyText(t *testing.T) {
	st := testStore(t)
	a := mustAccount(t, st, "q@x.com")
	ingest(t, st, &NewMessage{AccountID: a, Folder: "INBOX", UIDValidity: 1, UID: 1,
		Subject: "通知", Snippet: "验证码 555", BodyText: "您的验证码是 555，请勿泄露"})
	ingest(t, st, &NewMessage{AccountID: a, Folder: "INBOX", UIDValidity: 1, UID: 2,
		Subject: "无关", Snippet: "无关摘要", BodyText: "无关正文"})

	msgs, err := st.ListMessages(MessageFilter{Q: "555"})
	if err != nil || len(msgs) != 1 || msgs[0].Subject != "通知" {
		t.Fatalf("正文命中应搜到 1 封: %v %v", msgs, err)
	}
	// 中文子串（无分词依赖）
	msgs, err = st.ListMessages(MessageFilter{Q: "验证码"})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("中文子串应命中 1 封: %v %v", msgs, err)
	}
}

// retention 清理：DeleteMessagesOlderThan 按时间删行并返回 raw_path；AllRawPaths 供孤儿回收。
func TestDeleteMessagesOlderThan(t *testing.T) {
	st := testStore(t)
	a := mustAccount(t, st, "r@x.com")
	old := ingest(t, st, &NewMessage{AccountID: a, Folder: "INBOX", UIDValidity: 1, UID: 1, RawPath: "mails/1/2026-10/1-1.eml"})
	keep := ingest(t, st, &NewMessage{AccountID: a, Folder: "INBOX", UIDValidity: 1, UID: 2, RawPath: "mails/1/2026-11/1-2.eml"})
	// received_at 入库恒为 now：直接 UPDATE 造一封 30 天前的旧邮件
	if _, err := st.DB.Exec(`UPDATE messages SET received_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-30*24*time.Hour), old); err != nil {
		t.Fatal(err)
	}

	paths, err := st.DeleteMessagesOlderThan(time.Now().UTC().Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "mails/1/2026-10/1-1.eml" {
		t.Fatalf("应只删旧邮件并返回其 raw_path: %v", paths)
	}
	if _, err := st.GetMessage(old); err != ErrNotFound {
		t.Fatalf("旧邮件应已删除: %v", err)
	}
	if _, err := st.GetMessage(keep); err != nil {
		t.Fatalf("新邮件应保留: %v", err)
	}

	refs, err := st.AllRawPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("AllRawPaths 应只含保留邮件: %v", refs)
	}
	if _, ok := refs["mails/1/2026-11/1-2.eml"]; !ok {
		t.Fatalf("AllRawPaths 缺少保留路径: %v", refs)
	}
}

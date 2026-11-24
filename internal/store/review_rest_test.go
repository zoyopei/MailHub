package store

import (
	"fmt"
	"testing"
	"time"
)

func ptrOf[T any](v T) *T { return &v }

// 007 迁移：received_at 单列索引（时间排序与"今日收信"计数都靠它）。
func TestMigrationReceivedAtIndex(t *testing.T) {
	st := testStore(t)
	info, err := st.DB.Query(`PRAGMA index_info(idx_messages_received)`)
	if err != nil {
		t.Fatal(err)
	}
	defer info.Close()
	var cols []string
	for info.Next() {
		var seqno, cid int
		var name string
		if err := info.Scan(&seqno, &cid, &name); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	if len(cols) != 1 || cols[0] != "received_at" {
		t.Fatalf("idx_messages_received 应只覆盖 received_at，实际列: %v（索引不存在时为空）", cols)
	}
}

// UIDVALIDITY 变化后的重拉：换了坐标的旧邮件按 Message-ID 认出来，不重复入库。
func TestIngestReindexDedupesByMessageID(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "re@x.com")
	ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 10,
		MessageID: "<a@x>", RawPath: "mails/1/2026-11/1-10.eml"})
	ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 11,
		MessageID: "<b@x>", RawPath: "mails/1/2026-11/1-11.eml"})

	// UIDVALIDITY 变为 2：同样的两封邮件换了坐标再出现一遍
	for _, uid := range []uint32{1, 2} {
		m := &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 2, UID: uid, Reindex: true,
			RawPath: fmt.Sprintf("mails/1/2026-11/2-%d.eml", uid)}
		if uid == 1 {
			m.MessageID = "<a@x>"
		} else {
			m.MessageID = "<b@x>"
		}
		if _, err := st.IngestMessage(m); err != ErrDuplicate {
			t.Fatalf("重拉的旧邮件（uid=%d）应返回 ErrDuplicate，得 %v", uid, err)
		}
	}
	if n, _ := st.CountMessages(MessageFilter{AccountID: acct}); n != 2 {
		t.Fatalf("重拉不应新增邮件，得 %d 封", n)
	}

	// 重拉批次里真正的新邮件照常入库
	ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 2, UID: 3,
		MessageID: "<c@x>", Reindex: true, RawPath: "mails/1/2026-11/2-3.eml"})

	// 同账号**其他文件夹**的同一 Message-ID 不算重复
	ingest(t, st, &NewMessage{AccountID: acct, Folder: "Archive", UIDValidity: 2, UID: 7,
		MessageID: "<a@x>", Reindex: true, RawPath: "mails/1/2026-11/2-7.eml"})

	// 增量路径（Reindex=false）只按坐标去重：同一文件夹里两封 Message-ID 相同的不同邮件是合法的，
	// 不能被 Message-ID 查重误伤
	ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 2, UID: 4,
		MessageID: "<c@x>", RawPath: "mails/1/2026-11/2-4.eml"})

	if n, _ := st.CountMessages(MessageFilter{AccountID: acct}); n != 5 {
		t.Fatalf("应有 5 封（旧 2 + 重拉新增 1 + 另一文件夹 1 + 增量同 Message-ID 1），得 %d", n)
	}
}

// 拉取成功后的游标推进：空文件夹也建行；last_uid 只增不减；UIDVALIDITY 变化以本轮为准。
func TestAdvanceFolderUID(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "adv@x.com")

	// 空文件夹的首次同步：无邮件入库也要有游标（否则下次仍按"首次同步"处理）
	if err := st.AdvanceFolderUID(acct, "INBOX", 5, 0); err != nil {
		t.Fatal(err)
	}
	fs, _ := st.GetFolderState(acct, "INBOX")
	if fs == nil || fs.UIDValidity != 5 || fs.LastUID != 0 {
		t.Fatalf("空文件夹应建立游标 (5, 0)，得 %+v", fs)
	}

	// 同一 UIDVALIDITY：只增不减
	for _, c := range []struct{ give, want uint32 }{{40, 40}, {30, 40}, {41, 41}} {
		if err := st.AdvanceFolderUID(acct, "INBOX", 5, c.give); err != nil {
			t.Fatal(err)
		}
		fs, _ = st.GetFolderState(acct, "INBOX")
		if fs.LastUID != c.want {
			t.Fatalf("推进到 %d 后 last_uid 应为 %d，得 %d", c.give, c.want, fs.LastUID)
		}
	}

	// UIDVALIDITY 变化：整轮都是已存在邮件、没有任何入库，游标也必须换新
	if err := st.AdvanceFolderUID(acct, "INBOX", 8, 3); err != nil {
		t.Fatal(err)
	}
	fs, _ = st.GetFolderState(acct, "INBOX")
	if fs.UIDValidity != 8 || fs.LastUID != 3 {
		t.Fatalf("UIDVALIDITY 变化后应为 (8, 3)，得 %+v", fs)
	}

	// uidvalidity=0 是 Graph 文件夹的占位值，传入说明调用方搞错了
	if err := st.AdvanceFolderUID(acct, "INBOX", 0, 1); err == nil {
		t.Fatal("uidvalidity=0 应报错")
	}

	// 不碰同一行上的 Graph 游标字段
	if err := st.SetFolderDelta(acct, "Sent", "https://graph/delta"); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchFolderSeen(acct, "Sent", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceFolderUID(acct, "Sent", 3, 9); err != nil {
		t.Fatal(err)
	}
	fs, _ = st.GetFolderState(acct, "Sent")
	if fs.DeltaLink != "https://graph/delta" || !fs.LastSeenAt.Valid {
		t.Fatalf("推进 UID 游标不应清掉 Graph 游标: %+v", fs)
	}
}

// 重新启用清掉上次的报错；换凭据把 auth_failed 拉回 pending，其余状态不动。
func TestApplyAccountPatchResetsErrorState(t *testing.T) {
	st := testStore(t)
	id := mustAccount(t, st, "p@x.com")
	enc := []byte("new-cred")

	// 普通报错 + 换凭据：状态保持（运行中的 worker 自会改写），过时的报错清掉
	must(t, st.SetAccountStatus(id, StatusError, fmt.Errorf("boom")))
	must(t, st.ApplyAccountPatch(id, AccountPatch{CredentialEnc: enc}))
	a, _ := st.GetAccount(id)
	if a.Status != StatusError || a.LastError.Valid || string(a.CredentialEnc) != string(enc) {
		t.Fatalf("换凭据后应 status=error、无 last_error、凭据已更新，得 %+v", a)
	}

	// 认证失败 + 换凭据：回到 pending（新凭据要重新试）
	must(t, st.SetAccountStatus(id, StatusAuthFailed, fmt.Errorf("denied")))
	must(t, st.ApplyAccountPatch(id, AccountPatch{CredentialEnc: enc}))
	a, _ = st.GetAccount(id)
	if a.Status != StatusPending || a.LastError.Valid {
		t.Fatalf("auth_failed 换凭据后应 pending 且无 last_error，得 status=%s last_error=%q",
			a.Status, a.LastError.String)
	}

	// 禁用再启用：启用时清掉上次的报错
	must(t, st.SetAccountStatus(id, StatusError, fmt.Errorf("old")))
	must(t, st.ApplyAccountPatch(id, AccountPatch{Enabled: ptrOf(false)}))
	a, _ = st.GetAccount(id)
	if a.Enabled || a.Status != StatusDisabled {
		t.Fatalf("禁用后应 enabled=false status=disabled，得 %+v", a)
	}
	must(t, st.ApplyAccountPatch(id, AccountPatch{Enabled: ptrOf(true)}))
	a, _ = st.GetAccount(id)
	if !a.Enabled || a.Status != StatusPending || a.LastError.Valid {
		t.Fatalf("重新启用后应 enabled=true status=pending 且无 last_error，得 %+v", a)
	}

	// 与状态无关的修改（改名）不碰状态与报错
	must(t, st.SetAccountStatus(id, StatusError, fmt.Errorf("keep")))
	must(t, st.ApplyAccountPatch(id, AccountPatch{Name: ptrOf("n")}))
	a, _ = st.GetAccount(id)
	if a.Name != "n" || a.Status != StatusError || a.LastError.String != "keep" {
		t.Fatalf("改名不应动状态与报错，得 %+v", a)
	}
}

// SetAccountStatus / DeleteAccount 把写失败报出来，而不是静默忽略。
func TestStatusAndDeleteReportErrors(t *testing.T) {
	st := testStore(t)
	id := mustAccount(t, st, "gone@x.com")
	must(t, st.DeleteAccount(id))
	if err := st.DeleteAccount(id); err != ErrNotFound {
		t.Fatalf("删除不存在的账号应返回 ErrNotFound，得 %v", err)
	}

	st.Close()
	if err := st.SetAccountStatus(1, StatusError, nil); err == nil {
		t.Fatal("数据库已关闭，SetAccountStatus 应返回错误")
	}
	if err := st.DeleteAccount(1); err == nil {
		t.Fatal("数据库已关闭，DeleteAccount 应返回错误")
	}
}

// 手动重发清零重试计数与上次失败信息：已耗尽的记录重新走完整个重试序列。
func TestRetryDeliveryResetsAttempts(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "d@x.com")
	whID, err := st.CreateWebhook(&Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	msgID := ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 1,
		RawPath: "x", NotifyEnabled: true})
	dv, total, err := st.ListDeliveries(DeliveryFilter{})
	if err != nil || total != 1 || dv[0].WebhookID != whID || dv[0].MessageID != msgID {
		t.Fatalf("应有 1 条投递记录: total=%d err=%v", total, err)
	}
	// 模拟重试耗尽
	must(t, st.MarkDeliveryFailed(dv[0].ID, 500, "下游 500", time.Time{}, true))
	must(t, st.RetryDelivery(dv[0].ID))
	dv, _, _ = st.ListDeliveries(DeliveryFilter{})
	if dv[0].Status != "pending" || dv[0].Attempts != 0 || dv[0].LastError.Valid ||
		dv[0].ResponseCode.Valid || dv[0].NextRetryAt.Valid {
		t.Fatalf("手动重发后应 pending/attempts=0 且清空上次失败信息，得 %+v", dv[0])
	}
	if err := st.RetryDelivery(99999); err != ErrNotFound {
		t.Fatalf("重发不存在的记录应返回 ErrNotFound，得 %v", err)
	}
}

// 投递记录清理只删终态（success/failed）。pending 是还没送达的邮件——比如 webhook 被禁用期间
// 排队的那些，重新启用后本该继续投递——再旧也不能删。
func TestCleanOldDeliveriesTerminalOnly(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "c@x.com")
	if _, err := st.CreateWebhook(&Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: uint32(i),
			RawPath: "x", NotifyEnabled: true})
	}
	dv, total, err := st.ListDeliveries(DeliveryFilter{})
	if err != nil || total != 3 {
		t.Fatalf("应有 3 条投递记录: total=%d err=%v", total, err)
	}
	must(t, st.MarkDeliverySuccess(dv[0].ID, 200))
	must(t, st.MarkDeliveryFailed(dv[1].ID, 500, "下游 500", time.Time{}, true))
	// dv[2] 保持 pending

	age := func(at time.Time) {
		t.Helper()
		if _, err := st.DB.Exec(`UPDATE webhook_deliveries SET updated_at = ?`, at.UTC()); err != nil {
			t.Fatal(err)
		}
	}
	keep := 30 * 24 * time.Hour

	// 没到期：一条都不动
	age(time.Now())
	if n, err := st.CleanOldDeliveries(keep); err != nil || n != 0 {
		t.Fatalf("未到期不应删除，得 %d %v", n, err)
	}
	// 三条都很旧：只删两条终态的
	age(time.Now().Add(-40 * 24 * time.Hour))
	if n, err := st.CleanOldDeliveries(keep); err != nil || n != 2 {
		t.Fatalf("应只删 2 条终态记录，得 %d %v", n, err)
	}
	rest, total, err := st.ListDeliveries(DeliveryFilter{})
	if err != nil || total != 1 || rest[0].Status != "pending" {
		t.Fatalf("应只剩 pending 那条: total=%d err=%v %+v", total, err, rest)
	}
}

// "今日收信"的日界由调用方决定。
func TestMessageCountSince(t *testing.T) {
	st := testStore(t)
	acct := mustAccount(t, st, "t@x.com")
	now := time.Now()
	for i, ago := range []time.Duration{time.Hour, 25 * time.Hour, 3 * 24 * time.Hour} {
		ingest(t, st, &NewMessage{AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: uint32(i + 1),
			ReceivedAt: now.Add(-ago), RawPath: "x"})
	}
	for _, c := range []struct {
		ago  time.Duration
		want int
	}{{2 * time.Hour, 1}, {26 * time.Hour, 2}, {4 * 24 * time.Hour, 3}} {
		if n, err := st.MessageCountSince(now.Add(-c.ago)); err != nil || n != c.want {
			t.Errorf("自 %s 前起应 %d 封，得 %d（err=%v）", c.ago, c.want, n, err)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

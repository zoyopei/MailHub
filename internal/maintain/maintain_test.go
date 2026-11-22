package maintain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/store"
)

// 造一个带真实文件系统的环境：库中一封在库邮件（文件存在）+ 一封 retention 应删的旧邮件。
func setup(t *testing.T, retentionDays int) (*config.Config, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open("", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	acct, err := st.CreateAccount(&store.Account{
		Name: "m@x.com", Email: "m@x.com", IMAPHost: "h", IMAPPort: 993,
		Username: "m@x.com", AuthType: "password", CredentialEnc: []byte("e"), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	write := func(rel string) {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(abs), 0o700)
		if err := os.WriteFile(abs, []byte("raw"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 新邮件（保留）：库 + 文件
	keep := "mails/1/2026-11/1-1.eml"
	write(keep)
	if _, err := st.IngestMessage(&store.NewMessage{
		AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 1, RawPath: keep,
	}); err != nil {
		t.Fatal(err)
	}

	// 旧邮件（retention 删）：库（received_at 稍后改旧）+ 文件
	oldRel := "mails/1/2026-09/1-2.eml"
	write(oldRel)
	oldID, err := st.IngestMessage(&store.NewMessage{
		AccountID: acct, Folder: "INBOX", UIDValidity: 1, UID: 2, RawPath: oldRel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE messages SET received_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-90*24*time.Hour), oldID); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{DataDir: dir, RetentionDays: retentionDays}
	return cfg, st, dir
}

func exists(dir, rel string) bool {
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

func TestOnceRetentionAndOrphans(t *testing.T) {
	cfg, st, dir := setup(t, 30) // 保留 30 天：90 天前的旧邮件应删

	// 孤儿文件：超宽限期的未引用 .eml、刚写入的未引用 .eml（可能在事务提交前）、过龄 .part、新 .part
	orphan := "mails/1/2026-11/1-999.eml"
	freshOrphan := "mails/1/2026-11/1-998.eml"
	stale := "mails/1/2026-11/1-1.eml.part"
	freshPart := "mails/1/2026-11/1-2.eml.part"
	for _, rel := range []string{orphan, freshOrphan, stale, freshPart} {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.WriteFile(abs, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// orphan .eml 拨回 10 分钟前（超 orphanEmlGrace）；stale .part 拨回 25 小时前（超 stalePartAge）
	oldEml := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(orphan)), oldEml, oldEml); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(stale)), past, past); err != nil {
		t.Fatal(err)
	}

	Once(context.Background(), cfg, st)

	// retention：旧行删除、新行保留
	if _, total, _ := st.ListDeliveries(store.DeliveryFilter{}); total != 0 {
		t.Fatalf("不应有投递记录: %d", total)
	}
	msgs, _ := st.ListMessages(store.MessageFilter{})
	if len(msgs) != 1 {
		t.Fatalf("retention 后应剩 1 封，得 %d", len(msgs))
	}
	// 文件层面：旧 .eml、超宽限期孤儿 .eml、过龄 .part 删除；在库 .eml、刚写入 .eml、新 .part 保留
	if exists(dir, "mails/1/2026-09/1-2.eml") {
		t.Fatal("retention 应删除旧邮件的 .eml")
	}
	if exists(dir, orphan) {
		t.Fatal("超宽限期的孤儿 .eml 应被回收")
	}
	if !exists(dir, freshOrphan) {
		t.Fatal("刚写入的未引用 .eml（可能在事务提交窗口内）不应被回收")
	}
	if exists(dir, stale) {
		t.Fatal("过龄 .part 应被回收")
	}
	if !exists(dir, "mails/1/2026-11/1-1.eml") {
		t.Fatal("在库邮件的 .eml 不应被回收")
	}
	if !exists(dir, freshPart) {
		t.Fatal("新 .part（可能是写入中）不应被回收")
	}
}

// RetentionDays = 0（默认永久）：不删任何行与文件。
func TestOnceRetentionDisabled(t *testing.T) {
	cfg, st, dir := setup(t, 0)

	Once(context.Background(), cfg, st)

	msgs, _ := st.ListMessages(store.MessageFilter{})
	if len(msgs) != 2 {
		t.Fatalf("永久保留不应删行，得 %d", len(msgs))
	}
	if !exists(dir, "mails/1/2026-09/1-2.eml") {
		t.Fatal("永久保留不应删文件")
	}
}

// mails 目录不存在（全新部署）：Once 不应报错。
func TestOnceNoMailsDir(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open("", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: dir, RetentionDays: 30}
	Once(context.Background(), cfg, st) // 不 panic 即可
}

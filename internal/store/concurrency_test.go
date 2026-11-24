package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// 并发入库不得因 SQLite 快照过期而失败。
// deferred 事务"查重 SELECT → INSERT"在别的连接提交过写之后，升级写锁会立即返回
// database is locked（SQLITE_BUSY_SNAPSHOT，busy_timeout 无效）；Open 的 DSN 用
// _txlock=immediate 规避（见 store.Open 注释）。去掉该参数本测试会大面积失败
// （实测 2 路并发约 3/4 失败、4 路以上超过 95%）。
func TestConcurrentIngestNoBusy(t *testing.T) {
	st := testStore(t)
	const workers, perWorker = 4, 150
	ids := make([]int64, workers)
	for i := range ids {
		ids[i] = mustAccount(t, st, fmt.Sprintf("c%d@x.com", i))
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for gi := range ids {
		wg.Add(1)
		go func(gi int) {
			defer wg.Done()
			for j := 1; j <= perWorker; j++ {
				if _, err := st.IngestMessage(&NewMessage{
					AccountID: ids[gi], Folder: "INBOX", UIDValidity: 1, UID: uint32(j),
					ReceivedAt: time.Now(), RawPath: "x",
				}); err != nil {
					errs <- err
				}
			}
		}(gi)
	}
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		if n < 3 {
			t.Errorf("并发入库失败: %v", err)
		}
		n++
	}
	if n > 0 {
		t.Fatalf("并发入库共失败 %d 次（共 %d 次）", n, workers*perWorker)
	}
	for _, id := range ids {
		if got, err := st.CountMessages(MessageFilter{AccountID: id}); err != nil || got != perWorker {
			t.Fatalf("账号 %d 应入库 %d 封，得 %d（err=%v）", id, perWorker, got, err)
		}
	}
}

// 006 迁移：外键子表的索引必须存在。级联删除按子表外键列查找待删行，缺索引时每删一行
// 父表就全表扫描子表（附件表 50 万行时删 500 封邮件约 38 秒，全程持写锁）。
func TestMigrationFKIndexes(t *testing.T) {
	st := testStore(t)
	rows, err := st.DB.Query(`SELECT name FROM sqlite_master WHERE type = 'index'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// 三列都必须建在各自的外键列上（列序也校验，避免建错列的"同名索引"蒙混过关）
	for _, c := range []struct{ idx, table, col string }{
		{"idx_attachments_message", "attachments", "message_id"},
		{"idx_deliveries_message", "webhook_deliveries", "message_id"},
		{"idx_deliveries_webhook", "webhook_deliveries", "webhook_id"},
	} {
		if !have[c.idx] {
			t.Errorf("缺少索引 %s", c.idx)
			continue
		}
		var cols []string
		info, err := st.DB.Query(fmt.Sprintf(`PRAGMA index_info(%s)`, c.idx))
		if err != nil {
			t.Fatal(err)
		}
		for info.Next() {
			var seqno, cid int
			var name string
			if err := info.Scan(&seqno, &cid, &name); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, name)
		}
		info.Close()
		if len(cols) != 1 || cols[0] != c.col {
			t.Errorf("索引 %s 应只覆盖 %s(%s)，实际列: %v", c.idx, c.table, c.col, cols)
		}
	}
}

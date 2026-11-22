package store

import (
	"strconv"
	"testing"
)

// 两个不同 graph_id 的 32 位哈希碰撞时，两封邮件都必须入库成功
// （此前第二封会撞 UNIQUE(account,folder,uidv,uid) 而永久失败）。
func TestIngestGraphHashCollision(t *testing.T) {
	// 生日攻击：约 2^16 量级即可找到一对 FNV-32 碰撞
	seen := map[uint32]string{}
	var a, b string
	for i := 0; ; i++ {
		s := "graph-" + strconv.Itoa(i)
		h := HashGraphID(s)
		if prev, ok := seen[h]; ok {
			a, b = prev, s
			break
		}
		seen[h] = s
	}

	st := testStore(t)
	acct := mustAccount(t, st, "g@outlook.com")
	for _, gid := range []string{a, b} {
		if _, err := st.IngestMessage(&NewMessage{
			AccountID: acct, Folder: "INBOX", GraphID: gid,
			Subject: gid, RawPath: "mails/x/" + gid + ".eml",
		}); err != nil {
			t.Fatalf("碰撞的 graph_id %q 入库失败: %v", gid, err)
		}
	}
	msgs, _ := st.ListMessages(MessageFilter{AccountID: acct})
	if len(msgs) != 2 {
		t.Fatalf("应入库 2 封，得 %d", len(msgs))
	}
	// graph_id 幂等仍然生效
	if _, err := st.IngestMessage(&NewMessage{AccountID: acct, Folder: "INBOX", GraphID: b,
		RawPath: "mails/x/dup.eml"}); err != ErrDuplicate {
		t.Fatalf("重复 graph_id 应返回 ErrDuplicate，得 %v", err)
	}
}

package store

import (
	"testing"
)

func TestSetAccountTags(t *testing.T) {
	st := testStore(t)
	a := mustAccount(t, st, "tag1@x.com")
	b := mustAccount(t, st, "tag2@x.com")

	// 写入 + 批量读取
	if err := st.SetAccountTags(a, []string{"重要", "测试", " 重要 ", "", "重要"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAccountTags(b, []string{"测试"}); err != nil {
		t.Fatal(err)
	}
	m, err := st.TagsForAccounts([]int64{a, b})
	if err != nil {
		t.Fatal(err)
	}
	// 清洗：去重、去空白；空串丢弃（TagsForAccounts 按名字排序，顺序稳定）
	if len(m[a]) != 2 || m[a][0] != "测试" || m[a][1] != "重要" {
		t.Fatalf("账号 a 标签应为 [测试 重要]: %v", m[a])
	}

	// 标签列表（引用数降序）
	tags, err := st.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Name != "测试" || tags[0].Count != 2 || tags[1].Count != 1 {
		t.Fatalf("ListTags 应按引用数排序: %+v", tags)
	}

	// 全量替换：只留一个
	if err := st.SetAccountTags(a, []string{"生产"}); err != nil {
		t.Fatal(err)
	}
	names, _ := st.AccountTagNames(a)
	if len(names) != 1 || names[0] != "生产" {
		t.Fatalf("替换后应为 [生产]: %v", names)
	}

	// 孤儿标签回收：重要/测试中 重要 已无引用
	tags, _ = st.ListTags()
	got := map[string]int{}
	for _, ti := range tags {
		got[ti.Name] = ti.Count
	}
	if _, ok := got["重要"]; ok {
		t.Fatalf("失去引用的标签应被回收: %v", got)
	}
	if got["测试"] != 1 {
		t.Fatalf("测试 应仍被 b 引用: %v", got)
	}

	// 清空
	if err := st.SetAccountTags(a, nil); err != nil {
		t.Fatal(err)
	}
	names, _ = st.AccountTagNames(a)
	if len(names) != 0 {
		t.Fatalf("清空后应为空: %v", names)
	}
}

func TestListAccountsByTag(t *testing.T) {
	st := testStore(t)
	a := mustAccount(t, st, "ta@x.com")
	b := mustAccount(t, st, "tb@x.com")
	c := mustAccount(t, st, "tc@x.com") // 无标签
	_ = st.SetAccountTags(a, []string{"重要", "备用"})
	_ = st.SetAccountTags(b, []string{"备用"})

	// 单标签过滤
	accts, total, err := st.ListAccounts(AccountFilter{Tags: []string{"重要"}})
	if err != nil || total != 1 || len(accts) != 1 || accts[0].ID != a {
		t.Fatalf("tag=重要 应只命中 a: total=%d %v %v", total, accts, err)
	}

	// 多标签 OR：备用 命中 a+b
	accts, total, err = st.ListAccounts(AccountFilter{Tags: []string{"重要", "备用"}})
	if err != nil || total != 2 {
		t.Fatalf("tag=重要,备用 应命中 a+b: total=%d %v %v", total, accts, err)
	}

	// 与其他条件组合：q 限定邮箱前缀
	accts, total, err = st.ListAccounts(AccountFilter{Tags: []string{"备用"}, Q: "tb@"})
	if err != nil || total != 1 || accts[0].ID != b {
		t.Fatalf("tag+q 组合应只命中 b: total=%d %v %v", total, accts, err)
	}

	// 无命中的标签
	_, total, err = st.ListAccounts(AccountFilter{Tags: []string{"不存在"}})
	if err != nil || total != 0 {
		t.Fatalf("无命中标签应 total=0: %d %v", total, err)
	}

	// 无标签账号 c 不受影响
	accts, total, _ = st.ListAccounts(AccountFilter{})
	if total != 3 || accts[2].ID != c {
		t.Fatalf("无过滤应返回全部 3 个: %d", total)
	}
}

// 邮件按账号标签过滤（OR；与 account_id 组合 AND）。
func TestListMessagesByTag(t *testing.T) {
	st := testStore(t)
	a := mustAccount(t, st, "ma@x.com")
	b := mustAccount(t, st, "mb@x.com")
	_ = st.SetAccountTags(a, []string{"重要"})
	_ = st.SetAccountTags(b, []string{"测试"})
	ingest(t, st, &NewMessage{AccountID: a, Folder: "INBOX", UIDValidity: 1, UID: 1, Subject: "a1"})
	ingest(t, st, &NewMessage{AccountID: b, Folder: "INBOX", UIDValidity: 1, UID: 1, Subject: "b1"})

	// 单标签
	msgs, err := st.ListMessages(MessageFilter{Tags: []string{"重要"}})
	if err != nil || len(msgs) != 1 || msgs[0].Subject != "a1" {
		t.Fatalf("tag=重要 应只命中 a 的邮件: %v %v", msgs, err)
	}

	// 多标签 OR
	msgs, err = st.ListMessages(MessageFilter{Tags: []string{"重要", "测试"}})
	if err != nil || len(msgs) != 2 {
		t.Fatalf("tag=重要,测试 应命中 2 封: %d %v", len(msgs), err)
	}

	// 与 account_id 组合（AND）：标签宽 + 账号窄 → 只剩 b
	msgs, err = st.ListMessages(MessageFilter{Tags: []string{"重要", "测试"}, AccountID: b})
	if err != nil || len(msgs) != 1 || msgs[0].Subject != "b1" {
		t.Fatalf("tag+account 组合应只命中 b: %v %v", msgs, err)
	}

	// 无命中标签
	_, err = st.ListMessages(MessageFilter{Tags: []string{"不存在"}})
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ = st.ListMessages(MessageFilter{Tags: []string{"不存在"}})
	if len(msgs) != 0 {
		t.Fatalf("无命中应返回空: %v", msgs)
	}
}

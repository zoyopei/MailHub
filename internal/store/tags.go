package store

import (
	"strings"
)

// 账号标签（004 迁移）：tags + account_tags 关系表。
// 语义：SetAccountTags 全量替换；ListAccounts 过滤为 OR（任一命中）。

// normalizeTags 标签清洗：去空白、去重、保序、上限 20 个。
func normalizeTags(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] || len(n) > 50 {
			continue
		}
		seen[n] = true
		out = append(out, n)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// SetAccountTags 全量替换账号标签（事务：upsert tags → 重建关联 → 清理孤儿标签）。
func (s *Store) SetAccountTags(accountID int64, names []string) error {
	names = normalizeTags(names)
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM account_tags WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := tx.Exec(`INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, n); err != nil {
			return err
		}
		var tagID int64
		if err := tx.QueryRow(`SELECT id FROM tags WHERE name = ?`, n).Scan(&tagID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO account_tags (account_id, tag_id) VALUES (?,?)
			ON CONFLICT DO NOTHING`, accountID, tagID); err != nil {
			return err
		}
	}
	// 孤儿标签回收：失去全部引用的标签删除（保持标签列表干净）
	if _, err := tx.Exec(`DELETE FROM tags WHERE id NOT IN (SELECT tag_id FROM account_tags)`); err != nil {
		return err
	}
	return tx.Commit()
}

// TagsForAccounts 批量取账号标签（列表页一次查询，避免 N+1）。无标签的账号不在 map 中。
func (s *Store) TagsForAccounts(ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.Query(`SELECT at.account_id, t.name
		FROM account_tags at JOIN tags t ON at.tag_id = t.id
		WHERE at.account_id IN (`+ph+`) ORDER BY t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// AccountTagNames 单账号标签。
func (s *Store) AccountTagNames(id int64) ([]string, error) {
	m, err := s.TagsForAccounts([]int64{id})
	if err != nil {
		return nil, err
	}
	return m[id], nil
}

// TagInfo 标签与引用数（前端筛选下拉）。
type TagInfo struct {
	Name  string
	Count int
}

// ListTags 全部标签（按引用数降序，同数按名字）。
func (s *Store) ListTags() ([]TagInfo, error) {
	rows, err := s.DB.Query(`SELECT t.name, COUNT(at.account_id) AS n
		FROM tags t LEFT JOIN account_tags at ON at.tag_id = t.id
		GROUP BY t.id ORDER BY n DESC, t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TagInfo
	for rows.Next() {
		var ti TagInfo
		if err := rows.Scan(&ti.Name, &ti.Count); err != nil {
			return nil, err
		}
		out = append(out, ti)
	}
	return out, rows.Err()
}

// tagFilterSQL 生成按标签过滤的 EXISTS 子句（OR 语义：任一命中）。
// col 为账号 id 的引用列：账号列表传 "accounts.id"，邮件列表传 "messages.account_id"。
func tagFilterSQL(names []string, args *[]any, col string) string {
	ph := make([]string, len(names))
	for i, n := range names {
		ph[i] = "?"
		*args = append(*args, n)
	}
	return `EXISTS (SELECT 1 FROM account_tags at JOIN tags t ON at.tag_id = t.id
		WHERE at.account_id = ` + col + ` AND t.name IN (` + strings.Join(ph, ",") + `))`
}

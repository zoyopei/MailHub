package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Webhook 对应 webhooks 表。AccountIDs 为 JSON 数组字符串（NULL/"null" = 全部账号）。
type Webhook struct {
	ID          int64
	URL         string
	Secret      string
	AccountIDs  sql.NullString // JSON 数组；NULL 表示所有账号（与账号 notify_enabled 取 AND，DESIGN 5.1）
	IncludeBody bool
	Enabled     bool
	CreatedAt   time.Time
}

// AccountIDList 解析 account_ids；nil = 全部账号。
func (w *Webhook) AccountIDList() []int64 {
	s := strings.TrimSpace(w.AccountIDs.String)
	if !w.AccountIDs.Valid || s == "" || s == "null" {
		return nil
	}
	var ids []int64
	_ = json.Unmarshal([]byte(s), &ids)
	return ids
}

const webhookCols = `id, url, secret, account_ids, include_body, enabled, created_at`

func scanWebhook(row interface{ Scan(...any) error }) (*Webhook, error) {
	w := &Webhook{}
	var incBody, enabled int
	err := row.Scan(&w.ID, &w.URL, &w.Secret, &w.AccountIDs, &incBody, &enabled, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.IncludeBody = incBody != 0
	w.Enabled = enabled != 0
	return w, nil
}

// CreateWebhook 插入 webhook；secret 由调用方生成（为空时可自动）。
func (s *Store) CreateWebhook(w *Webhook) (int64, error) {
	var accountIDs any
	if ids := w.AccountIDList(); len(ids) > 0 {
		b, _ := json.Marshal(ids)
		accountIDs = string(b)
	}
	res, err := s.DB.Exec(`INSERT INTO webhooks (url, secret, account_ids, include_body, enabled, created_at)
		VALUES (?,?,?,?,?,?)`,
		w.URL, w.Secret, accountIDs, boolInt(w.IncludeBody), boolInt(w.Enabled), time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetWebhook 按 id 取。
func (s *Store) GetWebhook(id int64) (*Webhook, error) {
	return scanWebhook(s.DB.QueryRow(`SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
}

// WebhookFilter 列表筛选。
type WebhookFilter struct {
	Enabled *bool
}

// ListWebhooks 全量列表（量小不分页）。
func (s *Store) ListWebhooks(f WebhookFilter) ([]*Webhook, error) {
	where, args := "1=1", []any{}
	if f.Enabled != nil {
		where = "enabled = ?"
		args = append(args, boolInt(*f.Enabled))
	}
	rows, err := s.DB.Query(`SELECT `+webhookCols+` FROM webhooks WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// WebhookPatch 局部更新；nil 不更新。
type WebhookPatch struct {
	URL         *string
	AccountIDs  *[]int64 // 指针：nil 不更新；空切片 = 全部账号
	IncludeBody *bool
	Enabled     *bool
}

// ApplyWebhookPatch 更新 webhook。
func (s *Store) ApplyWebhookPatch(id int64, p WebhookPatch) error {
	sets, args := []string{}, []any{}
	if p.URL != nil {
		sets = append(sets, "url = ?")
		args = append(args, *p.URL)
	}
	if p.AccountIDs != nil {
		if len(*p.AccountIDs) == 0 {
			sets = append(sets, "account_ids = NULL")
		} else {
			b, _ := json.Marshal(*p.AccountIDs)
			sets = append(sets, "account_ids = ?")
			args = append(args, string(b))
		}
	}
	if p.IncludeBody != nil {
		sets = append(sets, "include_body = ?")
		args = append(args, boolInt(*p.IncludeBody))
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolInt(*p.Enabled))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	res, err := s.DB.Exec(`UPDATE webhooks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWebhook 删除（deliveries 级联）。
func (s *Store) DeleteWebhook(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

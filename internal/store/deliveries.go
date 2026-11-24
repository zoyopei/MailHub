package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Delivery 对应 webhook_deliveries 表（同时充当投递队列，DESIGN 5.3）。
type Delivery struct {
	ID           int64
	WebhookID    int64
	MessageID    int64
	Status       string // pending | success | failed
	Attempts     int
	NextRetryAt  sql.NullTime
	ResponseCode sql.NullInt64
	LastError    sql.NullString
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const deliveryCols = `id, webhook_id, message_id, status, attempts, next_retry_at,
	response_code, last_error, created_at, updated_at`

func scanDelivery(row interface{ Scan(...any) error }) (*Delivery, error) {
	d := &Delivery{}
	err := row.Scan(&d.ID, &d.WebhookID, &d.MessageID, &d.Status, &d.Attempts, &d.NextRetryAt,
		&d.ResponseCode, &d.LastError, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ListDueDeliveries 取到期待投递记录（pending 且 next_retry_at 已到/为空）。
// 只取**启用中** webhook 的记录：禁用 webhook 的 pending 保持原样（重新启用后继续），
// 若不过滤，满批的禁用记录会让投递器反复自唤醒空转并饿死其他 webhook。
// 单进程内投递器是唯一消费者，批次取走即处理，无需数据库原子 claim。
func (s *Store) ListDueDeliveries(limit int) ([]*Delivery, error) {
	rows, err := s.DB.Query(`SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE status = 'pending' AND (next_retry_at IS NULL OR next_retry_at <= ?)
		  AND webhook_id IN (SELECT id FROM webhooks WHERE enabled = 1)
		ORDER BY id LIMIT ?`, time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MarkDeliverySuccess 投递成功。
func (s *Store) MarkDeliverySuccess(id int64, code int) error {
	_, err := s.DB.Exec(`UPDATE webhook_deliveries
		SET status = 'success', attempts = attempts + 1, response_code = ?, last_error = NULL, updated_at = ?
		WHERE id = ?`, code, time.Now().UTC(), id)
	return err
}

// MarkDeliveryFailed 投递失败：attempts+1 并安排下次重试；exhausted=true 时终态 failed。
func (s *Store) MarkDeliveryFailed(id int64, code int, errMsg string, nextRetry time.Time, exhausted bool) error {
	status := "pending"
	if exhausted {
		status = "failed"
	}
	var nr any
	if !nextRetry.IsZero() {
		nr = nextRetry.UTC()
	}
	_, err := s.DB.Exec(`UPDATE webhook_deliveries
		SET status = ?, attempts = attempts + 1, response_code = ?, last_error = ?, next_retry_at = ?, updated_at = ?
		WHERE id = ?`, status, nullInt(code), truncateErr(errMsg), nr, time.Now().UTC(), id)
	return err
}

// RetryDelivery 手动重发：→ 立即待投递，并把重试计数清零。
// attempts 不清零的话，已经重试耗尽（attempts=6）的记录只要再失败一次就直接回到终态
// failed——手动重发等于只有一次机会，根本没机会走完 10s/1m/5m/30m/2h 的重试。
func (s *Store) RetryDelivery(id int64) error {
	res, err := s.DB.Exec(`UPDATE webhook_deliveries
		SET status = 'pending', attempts = 0, next_retry_at = NULL, last_error = NULL, response_code = NULL,
			updated_at = ? WHERE id = ?`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeliveryFilter 投递记录查询。
type DeliveryFilter struct {
	WebhookID int64
	Status    string
	Page      int
	Limit     int
}

// ListDeliveries 分页查询投递记录（新事件优先）。
func (s *Store) ListDeliveries(f DeliveryFilter) ([]*Delivery, int, error) {
	where, args := []string{"1=1"}, []any{}
	if f.WebhookID != 0 {
		where = append(where, "webhook_id = ?")
		args = append(args, f.WebhookID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	w := strings.Join(where, " AND ")

	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, limit := normalizePage(f.Page, f.Limit, 50, 200)
	args = append(args, limit, (page-1)*limit)
	rows, err := s.DB.Query(`SELECT `+deliveryCols+` FROM webhook_deliveries WHERE `+w+
		` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// WebhookDeliveryStats 各 webhook 的 pending/failed 计数（列表页展示）。
func (s *Store) WebhookDeliveryStats() (map[int64][2]int, error) {
	rows, err := s.DB.Query(`SELECT webhook_id,
		SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END)
		FROM webhook_deliveries GROUP BY webhook_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][2]int{}
	for rows.Next() {
		var id int64
		var st [2]int
		if err := rows.Scan(&id, &st[0], &st[1]); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// CleanOldDeliveries 清理超期的**终态**投递记录（DESIGN 5.3：保留 30 天）。返回删除数。
// 只删 success/failed：pending 是还没送达的邮件——比如 webhook 被禁用期间排队的那些，
// 重新启用后本该继续投递。按时间一刀切会把它们悄悄删掉。
func (s *Store) CleanOldDeliveries(retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention)
	res, err := s.DB.Exec(`DELETE FROM webhook_deliveries
		WHERE status IN ('success','failed') AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// truncateErr 错误信息截断（last_error 列防超长）。
func truncateErr(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

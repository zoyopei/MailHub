package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 账号状态机（DESIGN.md §4 accounts.status）。
const (
	StatusPending    = "pending"
	StatusSyncing    = "syncing"
	StatusIdle       = "idle"
	StatusPolling    = "polling"
	StatusError      = "error"
	StatusAuthFailed = "auth_failed"
	StatusDisabled   = "disabled"
)

// Account 对应 accounts 表。CredentialEnc 为 AES-GCM 密文。
type Account struct {
	ID            int64
	Name          string
	Email         string
	IMAPHost      string
	IMAPPort      int
	UseTLS        bool
	Username      string
	AuthType      string // password | oauth2
	ClientID      string
	CredentialEnc []byte
	Folders       string // JSON 数组字符串
	PollInterval  int
	NotifyEnabled bool
	Enabled       bool
	Transport     string // imap | graph（Outlook 家族存量授权仅含 Graph 权限，DESIGN.md 3.6）
	Status        string
	LastError     sql.NullString
	LastSyncAt    sql.NullTime
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ErrNotFound 统一"不存在"错误。
var ErrNotFound = errors.New("not found")

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	a := &Account{}
	var useTLS, notify, enabled int
	var clientID sql.NullString
	err := row.Scan(&a.ID, &a.Name, &a.Email, &a.IMAPHost, &a.IMAPPort, &useTLS,
		&a.Username, &a.AuthType, &clientID, &a.CredentialEnc, &a.Folders,
		&a.PollInterval, &notify, &enabled, &a.Status, &a.LastError, &a.LastSyncAt,
		&a.CreatedAt, &a.UpdatedAt, &a.Transport)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.ClientID = clientID.String
	a.UseTLS = useTLS != 0
	a.NotifyEnabled = notify != 0
	a.Enabled = enabled != 0
	if a.Transport == "" {
		a.Transport = "imap"
	}
	return a, nil
}

const accountCols = `id, name, email, imap_host, imap_port, use_tls, username, auth_type,
 client_id, credential_enc, folders, poll_interval, notify_enabled, enabled, status,
 last_error, last_sync_at, created_at, updated_at, transport`

// CreateAccount 插入账号（凭据须已加密）。
func (s *Store) CreateAccount(a *Account) (int64, error) {
	now := time.Now().UTC()
	res, err := s.DB.Exec(`INSERT INTO accounts
		(name, email, imap_host, imap_port, use_tls, username, auth_type, client_id,
		 credential_enc, folders, poll_interval, notify_enabled, enabled, status,
		 last_error, last_sync_at, created_at, updated_at, transport)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.Name, a.Email, a.IMAPHost, a.IMAPPort, boolInt(a.UseTLS), a.Username, a.AuthType, nullStr(a.ClientID),
		a.CredentialEnc, orDefault(a.Folders, `["INBOX"]`), orDefaultInt(a.PollInterval, 60),
		boolInt(a.NotifyEnabled), boolInt(a.Enabled), orDefault(a.Status, StatusPending),
		nil, nil, now, now, orDefault(a.Transport, "imap"))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AccountFilter 列表筛选（6.2：分页 + q + status + tag）。
type AccountFilter struct {
	Q      string   // 名称/邮箱模糊
	Status string
	Tags   []string // 标签过滤（OR：任一命中）
	Page   int
	Limit  int
}

// ListAccounts 分页返回账号与总数。q 对 name/email 做 LIKE；tags 走关系表 EXISTS（004 迁移）。
func (s *Store) ListAccounts(f AccountFilter) ([]*Account, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Q != "" {
		where = append(where, "(name LIKE ? OR email LIKE ?)")
		args = append(args, "%"+f.Q+"%", "%"+f.Q+"%")
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if len(f.Tags) > 0 {
		where = append(where, tagFilterSQL(f.Tags, &args, "accounts.id"))
	}
	w := strings.Join(where, " AND ")

	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM accounts WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page, limit := normalizePage(f.Page, f.Limit, 50, 200)
	args = append(args, limit, (page-1)*limit)

	rows, err := s.DB.Query(`SELECT `+accountCols+` FROM accounts WHERE `+w+
		` ORDER BY id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// GetAccount 按 id 取账号。
func (s *Store) GetAccount(id int64) (*Account, error) {
	row := s.DB.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

// GetAccountByEmail 按邮箱取账号（批量导入去重）。
func (s *Store) GetAccountByEmail(email string) (*Account, error) {
	row := s.DB.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE email = ?`, email)
	return scanAccount(row)
}

// ListAccountIDsByStatus 返回指定状态的账号 id（Manager 启动加载 enabled 账号）。
func (s *Store) ListAccountIDsByStatus(statuses ...string) ([]int64, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(statuses)), ",")
	args := make([]any, len(statuses))
	for i, s := range statuses {
		args[i] = s
	}
	rows, err := s.DB.Query(`SELECT id FROM accounts WHERE enabled = 1 AND status IN (`+ph+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AccountPatch 支持的更新字段；nil 表示不更新。
type AccountPatch struct {
	Name          *string
	IMAPHost      *string
	IMAPPort      *int
	Username      *string
	AuthType      *string
	ClientID      *string
	CredentialEnc []byte // 非空即更新
	Folders       *string
	PollInterval  *int
	NotifyEnabled *bool
	Enabled       *bool
}

// ApplyAccountPatch 更新账号字段并触发 updated_at。
func (s *Store) ApplyAccountPatch(id int64, p AccountPatch) error {
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UTC()}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.IMAPHost != nil {
		add("imap_host", *p.IMAPHost)
	}
	if p.IMAPPort != nil {
		add("imap_port", *p.IMAPPort)
	}
	if p.Username != nil {
		add("username", *p.Username)
	}
	if p.AuthType != nil {
		add("auth_type", *p.AuthType)
	}
	if p.ClientID != nil {
		add("client_id", *p.ClientID)
	}
	if p.CredentialEnc != nil {
		add("credential_enc", p.CredentialEnc)
	}
	if p.Folders != nil {
		add("folders", *p.Folders)
	}
	if p.PollInterval != nil {
		add("poll_interval", *p.PollInterval)
	}
	if p.NotifyEnabled != nil {
		add("notify_enabled", boolInt(*p.NotifyEnabled))
	}
	// status / last_error 的派生变更：同一列在 SET 里只出现一次，下面的规则按优先级取一条。
	//   - enabled（无论启停）→ 状态直接落到 pending / disabled；重新启用时清掉上次的报错；
	//   - 否则若换了凭据 → 仅当处于 auth_failed 时回到 pending（新凭据要重新试），其余状态
	//     保持（运行中的 worker 自会改写），并清掉上次的报错。
	// 不这么做的话，界面上会一直挂着"认证失败"和早已过时的错误信息。
	switch {
	case p.Enabled != nil:
		add("enabled", boolInt(*p.Enabled))
		add("last_error", nil)
		if *p.Enabled {
			add("status", StatusPending)
		} else {
			add("status", StatusDisabled)
		}
	case p.CredentialEnc != nil:
		add("last_error", nil)
		sets = append(sets, "status = CASE WHEN status = '"+StatusAuthFailed+"' THEN '"+StatusPending+"' ELSE status END")
	}
	args = append(args, id)
	res, err := s.DB.Exec(`UPDATE accounts SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateCredential 令牌轮换时重写凭据（独立小事务，DESIGN.md 3.5）。
func (s *Store) UpdateCredential(id int64, enc []byte) error {
	_, err := s.DB.Exec(`UPDATE accounts SET credential_enc = ?, updated_at = ? WHERE id = ?`,
		enc, time.Now().UTC(), id)
	return err
}

// TouchAccount 更新 last_sync_at 心跳（DESIGN.md 3.2：成功收件检查即更新）。
func (s *Store) TouchAccount(id int64) error {
	_, err := s.DB.Exec(`UPDATE accounts SET last_sync_at = ? WHERE id = ?`, time.Now().UTC(), id)
	return err
}

// SetAccountStatus 更新状态与错误信息。accErr 为 nil 时清空 last_error。写失败会返回错误——
// 调用方自行决定是否记日志，但不能再静默丢弃：数据库出问题时状态写不进去，界面上看到的
// 就全是过时状态，日志里却没有任何痕迹。
func (s *Store) SetAccountStatus(id int64, status string, accErr error) error {
	var e any
	if accErr != nil {
		e = truncateErr(accErr.Error())
	}
	_, err := s.DB.Exec(`UPDATE accounts SET status = ?, last_error = ? WHERE id = ?`, status, e, id)
	return err
}

// DeleteAccount 删除账号（messages/folder_states 经外键 CASCADE 一并删除）；账号不存在返回 ErrNotFound。
// purge 语义由调用方实现：purge=true 时先用 ListRawPathsByAccount 取路径，
// 删除账号后再清理磁盘 .eml（DESIGN.md 6.2）。
func (s *Store) DeleteAccount(id int64) error {
	res, err := s.DB.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRawPathsByAccount 返回账号全部 .eml 相对路径（purge 时清理磁盘）。
func (s *Store) ListRawPathsByAccount(id int64) ([]string, error) {
	rows, err := s.DB.Query(`SELECT raw_path FROM messages WHERE account_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AccountStats 账号列表的聚合计数（unread/message/last_message_at，DESIGN.md 6.2）。
type AccountStats struct {
	Count   int
	Unread  int
	LastMsg sql.NullTime
}

// StatsByAccounts 一条 SQL 聚合（低频量级足够；见 §4 索引说明）。
func (s *Store) StatsByAccounts(ids []int64) (map[int64]AccountStats, error) {
	out := map[int64]AccountStats{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// MAX() 聚合列在 SQLite 驱动返回 string（表达式列丢失类型声明），
	// 扫描到 NullString 后解析（直列扫描由驱动完成 time 转换）。
	q := `SELECT m.account_id, COUNT(*), SUM(CASE WHEN m.is_read = 0 THEN 1 ELSE 0 END),
		MAX(m.received_at) FROM messages m WHERE m.account_id IN (` + ph + `) GROUP BY m.account_id`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st AccountStats
		var lastMsg sql.NullString
		if err := rows.Scan(&id, &st.Count, &st.Unread, &lastMsg); err != nil {
			return nil, err
		}
		if lastMsg.Valid {
			if t, err := parseSQLTime(lastMsg.String); err == nil {
				st.LastMsg = sql.NullTime{Time: t, Valid: true}
			}
		}
		out[id] = st
	}
	return out, rows.Err()
}

// parseSQLTime 解析 SQLite 存储的时间字符串（modernc 的多候选格式：
// 直列为 RFC3339Nano；MAX() 等聚合列为 Go time.String() 格式）。
func parseSQLTime(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05.999999999 -0700 MST", // time.String()
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %q", s)
}

// StatusCounts 各状态账号数（/status 与仪表盘）。
func (s *Store) StatusCounts() (map[string]int, error) {
	rows, err := s.DB.Query(`SELECT status, COUNT(*) FROM accounts GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func normalizePage(page, limit, defLimit, maxLimit int) (int, int) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = defLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return page, limit
}

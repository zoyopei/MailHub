package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Message 对应 messages 表（列表项不含正文字段时也复用本结构，正文按需加载）。
type Message struct {
	ID             int64
	AccountID      int64
	Folder         string
	UIDValidity    uint32
	UID            uint32
	MessageID      string
	Subject        string
	FromAddr       string
	ToAddrs        string // JSON
	CcAddrs        string // JSON
	SentAt         sql.NullTime
	ReceivedAt     time.Time
	BodyText       string
	BodyHTML       string
	Snippet        string
	Size           int64
	HasAttachments bool
	IsRead         bool
	Truncated      bool
	ParseError     string
	RawPath        string
}

// Attachment 对应 attachments 表。
type Attachment struct {
	ID          int64
	MessageID   int64
	Filename    string
	ContentType string
	Size        int64
	PartIndex   int
}

// NewAttachment 入库用的附件元数据。
type NewAttachment struct {
	Filename    string
	ContentType string
	Size        int64
	PartIndex   int
}

// NewMessage 是 IngestMessage 的输入（扁平结构，store 不依赖 mailparse，TECH.md §2）。
type NewMessage struct {
	AccountID      int64
	Folder         string
	UIDValidity    uint32    // Graph 路径固定 0
	UID            uint32    // Graph 路径由 graph_id 哈希填充（UNIQUE 键离散）
	GraphID        string    // Graph 路径消息 id；非空时按 (account_id, graph_id) 幂等去重
	SeenAt         time.Time // Graph 的 receivedDateTime：folder_states.last_seen_at 游标（IMAP 为零）
	ReceivedAt     time.Time // 邮件到达时间（Graph receivedDateTime / IMAP INTERNALDATE / Date 头）；零值时入库写当前时间
	MessageID      string
	Subject        string
	FromAddr       string
	ToAddrsJSON    string
	CcAddrsJSON    string
	SentAt         time.Time // zero = 未知
	BodyText       string
	BodyHTML       string
	Snippet        string
	Size           int64
	HasAttachments bool
	Truncated      bool
	ParseError     string
	RawPath        string
	Attachments    []NewAttachment

	// NotifyEnabled：账号快照的提醒开关（DESIGN.md 5.1 第一层过滤）。
	// 关闭时不生成任何 deliveries 行。
	NotifyEnabled bool

	// Reindex：UIDVALIDITY 变化后的重拉（仅 IMAP）。邮箱重建了 UID，旧邮件换了坐标再出现
	// 一遍；此时除坐标外再按 Message-ID 去重（同账号同文件夹已有同一 Message-ID 即视为
	// 已存在，DESIGN.md 3.4），否则整箱旧邮件会重新入库并触发通知。
	Reindex bool
}

// ErrDuplicate 邮件已存在（UNIQUE(account_id, folder, uidvalidity, uid)；重拉时另含 Message-ID），幂等跳过。
var ErrDuplicate = errors.New("duplicate message")

// IngestMessage 在**一个事务**内完成 DESIGN.md 3.3 的三件事：
//  1. 插入 messages（唯一约束冲突返回 ErrDuplicate，全部回滚；Reindex 时另按 Message-ID 查重）
//  2. NotifyEnabled 时为每个匹配 webhook（enabled 且 account_ids 为 NULL 或含本账号）
//     插入 pending 的 webhook_deliveries（M2 投递器消费；M1 只入队）
//  3. upsert folder_states：不存在则建（uidvalidity, uid）；uidvalidity 变化则归零重写；
//     否则 last_uid = max(last_uid, uid)（DESIGN.md 3.4 UIDVALIDITY 处理）
func (s *Store) IngestMessage(m *NewMessage) (int64, error) {
	// Graph 路径：UID 由 graph_id 哈希填充（UNIQUE(account,folder,uidv,uid) 键需离散；
	// 真正的幂等去重靠 idx_messages_graph 唯一索引）。32 位哈希会碰撞：下方在事务内
	// 线性探测空闲 uid，避免不同邮件撞键导致 INSERT 失败、账号永久卡在同一封邮件上。
	uid := m.UID
	if m.GraphID != "" {
		if m.UIDValidity != 0 || m.UID != 0 {
			return 0, errors.New("Graph 路径不应携带 IMAP UID")
		}
		uid = HashGraphID(m.GraphID)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 幂等：先查唯一键（两步查询在低频写入量级下代价可忽略，且两库行为一致）。
	var exists int64
	if m.GraphID != "" {
		err = tx.QueryRow(`SELECT id FROM messages WHERE account_id = ? AND graph_id = ?`,
			m.AccountID, m.GraphID).Scan(&exists)
	} else {
		err = tx.QueryRow(`SELECT id FROM messages WHERE account_id = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
			m.AccountID, m.Folder, m.UIDValidity, m.UID).Scan(&exists)
	}
	if err == nil {
		return 0, ErrDuplicate
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if m.Reindex && m.MessageID != "" {
		// UIDVALIDITY 变化后的重拉：坐标是新的，要按 Message-ID 才认得出旧邮件。
		// 走 idx_messages_message_id(account_id, message_id)。没有 Message-ID 的邮件无法识别，
		// 只能按坐标去重（会重复入库，极少见）。
		var dup int64
		err = tx.QueryRow(`SELECT id FROM messages WHERE account_id = ? AND folder = ? AND message_id = ? LIMIT 1`,
			m.AccountID, m.Folder, m.MessageID).Scan(&dup)
		if err == nil {
			return 0, ErrDuplicate
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	if m.GraphID != "" {
		// graph_id 未入库但哈希 uid 被别的邮件占用 → 碰撞，探测下一个空闲 uid。
		for i := 0; ; i++ {
			var other int64
			err := tx.QueryRow(`SELECT id FROM messages WHERE account_id = ? AND folder = ? AND uidvalidity = 0 AND uid = ?`,
				m.AccountID, m.Folder, uid).Scan(&other)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return 0, err
			}
			if i >= 64 {
				return 0, errors.New("graph 邮件 uid 探测失败（连续碰撞）")
			}
			uid++
			if uid == 0 {
				uid = 1
			}
		}
	}

	now := time.Now().UTC()
	var sentAt any
	if !m.SentAt.IsZero() {
		sentAt = m.SentAt.UTC()
	}
	// received_at 语义是"邮件到达时间"：调用方给出真实时间（Graph receivedDateTime /
	// IMAP INTERNALDATE / Date 头）则用它；零值兜底当前时间（保持向后兼容）。
	recvAt := now
	if !m.ReceivedAt.IsZero() {
		recvAt = m.ReceivedAt.UTC()
	}
	res, err := tx.Exec(`INSERT INTO messages
		(account_id, folder, uidvalidity, uid, graph_id, message_id, subject, from_addr, to_addrs, cc_addrs,
		 sent_at, received_at, body_text, body_html, snippet, size, has_attachments, is_read,
		 truncated, parse_error, raw_path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?, 0, ?, ?, ?)`,
		m.AccountID, m.Folder, m.UIDValidity, uid, nullStr(m.GraphID), m.MessageID, m.Subject, m.FromAddr,
		m.ToAddrsJSON, m.CcAddrsJSON, sentAt, recvAt, m.BodyText, m.BodyHTML, m.Snippet, m.Size,
		boolInt(m.HasAttachments), boolInt(m.Truncated), nullStr(m.ParseError), m.RawPath)
	if err != nil {
		return 0, err
	}
	msgID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, at := range m.Attachments {
		if _, err := tx.Exec(`INSERT INTO attachments (message_id, filename, content_type, size, part_index)
			VALUES (?,?,?,?,?)`, msgID, at.Filename, at.ContentType, at.Size, at.PartIndex); err != nil {
			return 0, err
		}
	}

	if m.NotifyEnabled {
		whHooks, err := matchingWebhooksTx(tx, m.AccountID)
		if err != nil {
			return 0, err
		}
		for _, wid := range whHooks {
			if _, err := tx.Exec(`INSERT INTO webhook_deliveries
				(webhook_id, message_id, status, attempts, created_at, updated_at)
				VALUES (?,?,'pending',0,?,?)`, wid, msgID, now, now); err != nil {
				return 0, err
			}
		}
	}

	// folder_states upsert（含 UIDVALIDITY 变化归零，DESIGN.md 3.4）。
	var oldUIDV, oldLast int64
	err = tx.QueryRow(`SELECT uidvalidity, last_uid FROM folder_states WHERE account_id = ? AND folder = ?`,
		m.AccountID, m.Folder).Scan(&oldUIDV, &oldLast)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO folder_states (account_id, folder, uidvalidity, last_uid, last_seen_at)
			VALUES (?,?,?,?,?)`, m.AccountID, m.Folder, m.UIDValidity, uid, nullTime(m.SeenAt))
	case err == nil && uint32(oldUIDV) == m.UIDValidity:
		if int64(uid) > oldLast {
			_, err = tx.Exec(`UPDATE folder_states SET last_uid = ?, last_seen_at = ?
				WHERE account_id = ? AND folder = ?`,
				uid, nullTime(m.SeenAt), m.AccountID, m.Folder)
		} else if !m.SeenAt.IsZero() {
			_, err = tx.Exec(`UPDATE folder_states SET last_seen_at = ? WHERE account_id = ? AND folder = ?`,
				nullTime(m.SeenAt), m.AccountID, m.Folder)
		}
	case err == nil: // UIDVALIDITY 变化：归零重写
		_, err = tx.Exec(`UPDATE folder_states SET uidvalidity = ?, last_uid = ?, last_seen_at = ?
			WHERE account_id = ? AND folder = ?`,
			m.UIDValidity, uid, nullTime(m.SeenAt), m.AccountID, m.Folder)
	}
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return msgID, nil
}

// matchingWebhooksTx 在事务内解析 account_ids JSON，精确匹配（LIKE 对 JSON 不可靠）。
func matchingWebhooksTx(tx *sql.Tx, accountID int64) ([]int64, error) {
	rows, err := tx.Query(`SELECT id, account_ids FROM webhooks WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		var ids sql.NullString
		if err := rows.Scan(&id, &ids); err != nil {
			return nil, err
		}
		if matchAccountIDs(ids, accountID) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// matchAccountIDs：NULL/'null'/空 = 所有账号；否则解析 JSON 数组精确包含。
func matchAccountIDs(ids sql.NullString, accountID int64) bool {
	s := strings.TrimSpace(ids.String)
	if !ids.Valid || s == "" || s == "null" {
		return true
	}
	var arr []int64
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return false // 配置损坏视为不匹配，避免误投
	}
	for _, id := range arr {
		if id == accountID {
			return true
		}
	}
	return false
}

// GetFolderState 返回文件夹游标；不存在返回 nil。
type FolderState struct {
	AccountID   int64
	Folder      string
	UIDValidity uint32
	LastUID     uint32
	LastSeenAt  sql.NullTime // Graph 路径的时间游标（002 迁移）
	DeltaLink   string       // Graph delta 增量链接（003 迁移；空 = 无/失效）
	InitialDone bool         // 首次同步已完整走完（008 迁移）；false = 还没走完，见 BeginFolderSync
}

func (s *Store) GetFolderState(accountID int64, folder string) (*FolderState, error) {
	var st FolderState
	var uidv, last, done int64
	var seen sql.NullTime
	var delta sql.NullString
	err := s.DB.QueryRow(`SELECT account_id, folder, uidvalidity, last_uid, last_seen_at, delta_link, initial_done
		FROM folder_states WHERE account_id = ? AND folder = ?`, accountID, folder).
		Scan(&st.AccountID, &st.Folder, &uidv, &last, &seen, &delta, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.UIDValidity, st.LastUID, st.LastSeenAt = uint32(uidv), uint32(last), seen
	st.DeltaLink = delta.String
	st.InitialDone = done != 0
	return &st, nil
}

// SetFolderDelta 保存 Graph delta 链接（worker 完整拉取成功后调用）。
// upsert：空文件夹（从未 ingest、无 folder_states 行）也能建立游标，
// 否则每次都按"首次"全窗口重扫（含给已有账号新增文件夹的场景）。
func (s *Store) SetFolderDelta(accountID int64, folder string, link string) error {
	if link == "" {
		return nil
	}
	_, err := s.DB.Exec(`INSERT INTO folder_states (account_id, folder, uidvalidity, last_uid, delta_link)
		VALUES (?,?,0,0,?)
		ON CONFLICT (account_id, folder) DO UPDATE SET delta_link = excluded.delta_link`,
		accountID, folder, link)
	return err
}

// AdvanceFolderUID 在一个 IMAP 文件夹**完整**拉取成功后推进游标（worker 调用；lastUID 取本轮
// 搜索结果的最大 UID，首次同步无结果时取文件夹里现有邮件的最大 UID）。
//   - 行不存在（空文件夹的首次同步）→ 建立。IngestMessage 只在入库时建行，空文件夹就一直没有
//     游标：之后到达的第一封邮件仍按"首次同步"处理，被当成历史邮件，不触发 webhook。
//     对专门收验证码的空邮箱，这正好是最关键的一封。
//   - UIDVALIDITY 一致 → last_uid 只增不减；
//   - UIDVALIDITY 变化 → 以本轮为准重写。重拉的全部是已存在邮件（按 Message-ID 去重）时没有
//     任何入库，游标必须在这里换新，否则每一轮都重新全量重拉。
//
// Graph 文件夹的行 uidvalidity 恒为 0，调用方不要对本函数传 uidv=0。
func (s *Store) AdvanceFolderUID(accountID int64, folder string, uidv, lastUID uint32) error {
	if uidv == 0 {
		return errors.New("AdvanceFolderUID: uidvalidity 不能为 0")
	}
	_, err := s.DB.Exec(`INSERT INTO folder_states (account_id, folder, uidvalidity, last_uid)
		VALUES (?,?,?,?)
		ON CONFLICT (account_id, folder) DO UPDATE SET
			uidvalidity = excluded.uidvalidity,
			last_uid = CASE WHEN folder_states.uidvalidity = excluded.uidvalidity
				THEN MAX(folder_states.last_uid, excluded.last_uid) ELSE excluded.last_uid END`,
		accountID, folder, uidv, lastUID)
	return err
}

// BeginFolderSync 标记"这个文件夹的首次同步开始了"：还没有游标行时建一个全零行并置
// initial_done = 0；已有行则什么都不动。worker 在首次同步**开始之前**调用，走完之后调
// FinishFolderSync。
//
// 为什么需要标记："哪些邮件是历史邮件"（不触发 webhook、不推浏览器事件，DESIGN 3.4/5.1）
// 取决于该文件夹的首次同步是否已经完整走完，而不是有没有游标行——游标行在首封邮件入库时
// 就建了。只看行的话，首次同步中途被打断（网络错误、停机、Graph 单轮分页上限）之后，
// 下一轮就不再算"首次同步"，窗口里剩下的历史邮件全被当成新邮件通知。
func (s *Store) BeginFolderSync(accountID int64, folder string) error {
	_, err := s.DB.Exec(`INSERT INTO folder_states (account_id, folder, uidvalidity, last_uid, initial_done)
		VALUES (?,?,0,0,0)
		ON CONFLICT (account_id, folder) DO NOTHING`, accountID, folder)
	return err
}

// FinishFolderSync 首次同步完整走完：置 initial_done = 1，之后到达的邮件都是新邮件。
func (s *Store) FinishFolderSync(accountID int64, folder string) error {
	_, err := s.DB.Exec(`UPDATE folder_states SET initial_done = 1
		WHERE account_id = ? AND folder = ?`, accountID, folder)
	return err
}

// TouchFolderSeen 推进 Graph 路径的 last_seen_at 游标（只在变大时写入）。
// 由 worker 在一个文件夹**完整**拉取成功后调用——中途失败不推进，下次重拉靠
// graph_id 唯一索引幂等（DESIGN.md 3.6）。
func (s *Store) TouchFolderSeen(accountID int64, folder string, t time.Time) error {
	if t.IsZero() {
		return nil
	}
	_, err := s.DB.Exec(`INSERT INTO folder_states (account_id, folder, uidvalidity, last_uid, last_seen_at)
		VALUES (?,?,0,0,?)
		ON CONFLICT (account_id, folder) DO UPDATE SET last_seen_at =
			CASE WHEN folder_states.last_seen_at IS NULL OR folder_states.last_seen_at < excluded.last_seen_at
				THEN excluded.last_seen_at ELSE folder_states.last_seen_at END`,
		accountID, folder, t.UTC())
	return err
}

// HasGraphIDs 批量查询已入库的 Graph 消息 id（Graph 路径去重，DESIGN.md 3.6）。
func (s *Store) HasGraphIDs(accountID int64, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, accountID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.DB.Query(`SELECT graph_id FROM messages WHERE account_id = ? AND graph_id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// HashGraphID 简单离散哈希：Graph 路径用 graph_id 填充 UNIQUE(account,folder,uidv,uid)
// 键的 uid 位（去重以 graph_id 唯一索引为准；碰撞由 IngestMessage 线性探测化解）。
// .eml 文件名不再使用该 32 位哈希（worker 改用 sha256 前缀，避免碰撞覆盖文件）。
func HashGraphID(s string) uint32 {
	const prime = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return h
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// MessageFilter 列表筛选（DESIGN.md 6.3 查询参数）。
type MessageFilter struct {
	AccountID int64
	IsRead    *bool
	Q         string
	Since     time.Time
	Until     time.Time
	AfterID   int64    // 增量游标：id > after_id，升序
	BeforeID  int64    // 向前翻页：id < before_id
	Tags      []string // 账号标签过滤（OR：任一命中；与 account_id 可组合）
	Limit     int
	// 排序（M4 Web 邮件页）：Sort 为空走默认 id 倒序 + id 游标分页（before_id/after_id）；
	// 设为 "time"/"account" 时按列排序 + Offset 偏移分页（与 id 游标互斥）。
	Sort   string // "" | "time"（received_at）| "account"（关联账号 name）
	Order  string // "asc" | "desc"（默认 desc）
	Offset int
}

const messageCols = `id, account_id, folder, uidvalidity, uid, message_id, subject, from_addr,
 to_addrs, cc_addrs, sent_at, received_at, snippet, size, has_attachments, is_read, truncated, parse_error`

// messageColsP 带 messages. 前缀：account 排序 JOIN accounts 时 id 等列名歧义，
// 需显式限定（messages 表保持原名——tagFilterSQL 生成 messages.account_id 引用）。
const messageColsP = `messages.id, messages.account_id, messages.folder, messages.uidvalidity, messages.uid,
 messages.message_id, messages.subject, messages.from_addr, messages.to_addrs, messages.cc_addrs,
 messages.sent_at, messages.received_at, messages.snippet, messages.size, messages.has_attachments,
 messages.is_read, messages.truncated, messages.parse_error`

// messageConds 筛选条件构造（ListMessages 与 CountMessages 共用；不含游标/OFFSET——
// 计数语义是"当前筛选的总数"）。返回条件片段与对应参数（顺序一致）。
func messageConds(f MessageFilter) ([]string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if f.AccountID != 0 {
		where = append(where, "account_id = ?")
		args = append(args, f.AccountID)
	}
	if len(f.Tags) > 0 {
		where = append(where, tagFilterSQL(f.Tags, &args, "messages.account_id"))
	}
	if f.IsRead != nil {
		where = append(where, "is_read = ?")
		args = append(args, boolInt(*f.IsRead))
	}
	if f.Q != "" {
		// 全文搜索（DESIGN.md 335：此量级 LIKE 可接受、中文子串友好；FTS5 留给数据量显著增长后）
		where = append(where, "(subject LIKE ? OR from_addr LIKE ? OR snippet LIKE ? OR body_text LIKE ?)")
		p := "%" + f.Q + "%"
		args = append(args, p, p, p, p)
	}
	if !f.Since.IsZero() {
		where = append(where, "received_at >= ?")
		args = append(args, f.Since.UTC())
	}
	if !f.Until.IsZero() {
		where = append(where, "received_at <= ?")
		args = append(args, f.Until.UTC())
	}
	return where, args
}

// CountMessages 按同筛选条件计数（列表 total，前端"已加载 X / 共 Y"与 hasMore 精确判断）。
func (s *Store) CountMessages(f MessageFilter) (int, error) {
	where, args := messageConds(f)
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE `+strings.Join(where, " AND "),
		args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListMessages 分页查询。默认按 id 倒序（最新在前）；设置 AfterID 时按 id 升序。
func (s *Store) ListMessages(f MessageFilter) ([]*Message, error) {
	where, args := messageConds(f)
	// id 游标分页仅默认模式（排序模式用 OFFSET；id 比较在 JOIN 下亦歧义）
	order, cols := "id DESC", messageCols
	if f.Sort == "" {
		if f.AfterID != 0 {
			where = append(where, "id > ?")
			args = append(args, f.AfterID)
			order = "id ASC"
		}
		if f.BeforeID != 0 {
			where = append(where, "id < ?")
			args = append(args, f.BeforeID)
		}
	}
	_, limit := normalizePage(1, f.Limit, 50, 200)
	w := strings.Join(where, " AND ")

	// 排序模式：JOIN accounts 按账号名排，OFFSET 偏移分页。
	join, offset := "", 0
	switch f.Sort {
	case "time":
		dir := "DESC"
		if f.Order == "asc" {
			dir = "ASC"
		}
		order = "received_at " + dir + ", id " + dir
		offset = max(f.Offset, 0)
	case "account":
		dir := "ASC"
		if f.Order == "desc" {
			dir = "DESC"
		}
		join = " JOIN accounts a ON a.id = messages.account_id"
		order = "a.name " + dir + ", received_at DESC, messages.id DESC"
		cols = messageColsP
		offset = max(f.Offset, 0)
	}

	pageSQL := " LIMIT ?"
	args = append(args, limit)
	if f.Sort != "" {
		pageSQL = " LIMIT ? OFFSET ?"
		args = append(args, offset)
	}
	rows, err := s.DB.Query(`SELECT `+cols+` FROM messages`+join+` WHERE `+w+` ORDER BY `+order+pageSQL,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	m := &Message{}
	var hasAtt, isRead, trunc int
	var msgID, parseErr sql.NullString
	err := row.Scan(&m.ID, &m.AccountID, &m.Folder, &m.UIDValidity, &m.UID, &msgID, &m.Subject,
		&m.FromAddr, &m.ToAddrs, &m.CcAddrs, &m.SentAt, &m.ReceivedAt, &m.Snippet, &m.Size,
		&hasAtt, &isRead, &trunc, &parseErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.MessageID = msgID.String
	m.ParseError = parseErr.String
	m.HasAttachments = hasAtt != 0
	m.IsRead = isRead != 0
	m.Truncated = trunc != 0
	return m, nil
}

// GetMessage 含正文的详情。
func (s *Store) GetMessage(id int64) (*Message, error) {
	row := s.DB.QueryRow(`SELECT id, account_id, folder, uidvalidity, uid, message_id, subject, from_addr,
		to_addrs, cc_addrs, sent_at, received_at, body_text, body_html, snippet, size, has_attachments,
		is_read, truncated, parse_error, raw_path FROM messages WHERE id = ?`, id)
	m := &Message{}
	var hasAtt, isRead, trunc int
	var msgID, parseErr sql.NullString
	err := row.Scan(&m.ID, &m.AccountID, &m.Folder, &m.UIDValidity, &m.UID, &msgID, &m.Subject,
		&m.FromAddr, &m.ToAddrs, &m.CcAddrs, &m.SentAt, &m.ReceivedAt, &m.BodyText, &m.BodyHTML,
		&m.Snippet, &m.Size, &hasAtt, &isRead, &trunc, &parseErr, &m.RawPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.MessageID = msgID.String
	m.ParseError = parseErr.String
	m.HasAttachments = hasAtt != 0
	m.IsRead = isRead != 0
	m.Truncated = trunc != 0
	return m, nil
}

// AttachmentsForMessage 附件元数据列表。
func (s *Store) AttachmentsForMessage(msgID int64) ([]*Attachment, error) {
	rows, err := s.DB.Query(`SELECT id, message_id, filename, content_type, size, part_index
		FROM attachments WHERE message_id = ? ORDER BY part_index`, msgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Attachment
	for rows.Next() {
		a := &Attachment{}
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.Size, &a.PartIndex); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAttachment 单个附件元数据。
func (s *Store) GetAttachment(attID int64) (*Attachment, error) {
	row := s.DB.QueryRow(`SELECT id, message_id, filename, content_type, size, part_index
		FROM attachments WHERE id = ?`, attID)
	a := &Attachment{}
	if err := row.Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.Size, &a.PartIndex); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return a, nil
}

// SetRead 更新本地已读状态（DESIGN.md 6.3，不回写服务器）。
func (s *Store) SetRead(id int64, isRead bool) error {
	res, err := s.DB.Exec(`UPDATE messages SET is_read = ? WHERE id = ?`, boolInt(isRead), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// sqlBatchSize 批量 IN (...) 分块大小，避开 SQLite 默认 999 变量上限。
const sqlBatchSize = 500

// BatchRead 批量标记已读：ids 非空按 id 集合，否则 accountID != 0 时标记该账号全部。
// 返回受影响行数。
func (s *Store) BatchRead(ids []int64, accountID int64) (int64, error) {
	if len(ids) > 0 {
		var total int64
		for i := 0; i < len(ids); i += sqlBatchSize {
			chunk := ids[i:min(i+sqlBatchSize, len(ids))]
			n, err := execIn(s.DB, `UPDATE messages SET is_read = 1 WHERE id IN (`+ph(len(chunk))+`)`, idsToArgs(chunk))
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	if accountID != 0 {
		res, err := s.DB.Exec(`UPDATE messages SET is_read = 1 WHERE account_id = ?`, accountID)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	return 0, errors.New("需要 ids 或 account_id")
}

// DeleteMessages 删除本地副本（行 + 附件级联），返回被删邮件的 raw_path 供调用方清理磁盘。
func (s *Store) DeleteMessages(ids []int64, accountID int64) ([]string, error) {
	if len(ids) > 0 {
		var paths []string
		for i := 0; i < len(ids); i += sqlBatchSize {
			chunk := ids[i:min(i+sqlBatchSize, len(ids))]
			args := idsToArgs(chunk)
			rows, err := s.DB.Query(`SELECT raw_path FROM messages WHERE id IN (`+ph(len(chunk))+`)`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err != nil {
					rows.Close()
					return nil, err
				}
				paths = append(paths, p)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return nil, err
			}
			if _, err := execIn(s.DB, `DELETE FROM messages WHERE id IN (`+ph(len(chunk))+`)`, args); err != nil {
				return nil, err
			}
		}
		return paths, nil
	}
	if accountID == 0 {
		return nil, errors.New("需要 ids 或 account_id")
	}
	rows, err := s.DB.Query(`SELECT raw_path FROM messages WHERE account_id = ?`, accountID)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(`DELETE FROM messages WHERE account_id = ?`, accountID); err != nil {
		return nil, err
	}
	return paths, nil
}

// DeleteMessage 单封删除，返回 raw_path；邮件不存在时返回 ErrNotFound。
func (s *Store) DeleteMessage(id int64) ([]string, error) {
	var rawPath string
	err := s.DB.QueryRow(`SELECT raw_path FROM messages WHERE id = ?`, id).Scan(&rawPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	res, err := s.DB.Exec(`DELETE FROM messages WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if rawPath == "" {
		return nil, nil
	}
	return []string{rawPath}, nil
}

// DeleteMessagesOlderThan retention 清理（DESIGN.md 9.1 MAILHUB_RETENTION_DAYS）：
// 删除 received_at 早于 cutoff 的邮件（附件随外键级联、投递记录随外键级联），
// 返回被删邮件的 raw_path 供调用方清理磁盘 .eml。
func (s *Store) DeleteMessagesOlderThan(cutoff time.Time) ([]string, error) {
	rows, err := s.DB.Query(`SELECT raw_path FROM messages WHERE received_at < ?`, cutoff.UTC())
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(`DELETE FROM messages WHERE received_at < ?`, cutoff.UTC()); err != nil {
		return nil, err
	}
	return paths, nil
}

// AllRawPaths 全表 raw_path 集合（孤儿 .eml 回收用；每日一次全扫，矩阵量级可接受）。
func (s *Store) AllRawPaths() (map[string]struct{}, error) {
	rows, err := s.DB.Query(`SELECT raw_path FROM messages WHERE raw_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// MessageCountSince 自 since 起的收信数（/status 的"今日收信"；日界由调用方决定——
// 默认取服务器本地零点，Web 端传浏览器所在时区的零点）。走 idx_messages_received。
func (s *Store) MessageCountSince(since time.Time) (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM messages WHERE received_at >= ?`, since.UTC()).Scan(&n)
	return n, err
}

// PendingDeliveryCount 待投递与失败数（/status）。
func (s *Store) PendingDeliveryCount() (pending, failed int, err error) {
	rows, e := s.DB.Query(`SELECT status, COUNT(*) FROM webhook_deliveries WHERE status != 'success' GROUP BY status`)
	if e != nil {
		return 0, 0, e
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return 0, 0, err
		}
		if st == "failed" {
			failed = n
		} else {
			pending += n
		}
	}
	return pending, failed, rows.Err()
}

func ph(n int) string { return strings.TrimRight(strings.Repeat("?,", n), ",") }

func idsToArgs(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func execIn(db *sql.DB, query string, args []any) (int64, error) {
	res, err := db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"mailhub/internal/mailparse"
	"mailhub/internal/store"
)

func (s *Server) mountMessages(r chi.Router) {
	r.Get("/messages", s.handleMessageList)
	r.Post("/messages/batch-read", s.handleBatchRead)
	r.Post("/messages/batch-delete", s.handleBatchDelete)
	r.Get("/messages/{id}", s.handleMessageGet)
	r.Patch("/messages/{id}", s.handleMessagePatch)
	r.Delete("/messages/{id}", s.handleMessageDelete)
	r.Get("/messages/{id}/raw", s.handleMessageRaw)
	r.Get("/messages/{id}/attachments/{attID}", s.handleAttachment)
}

type messageResp struct {
	ID             int64    `json:"id"`
	AccountID      int64    `json:"account_id"`
	Folder         string   `json:"folder"`
	Subject        string   `json:"subject"`
	From           string   `json:"from"`
	To             []string `json:"to"`
	SentAt         *string  `json:"sent_at"`
	ReceivedAt     string   `json:"received_at"`
	Snippet        string   `json:"snippet"`
	HasAttachments bool     `json:"has_attachments"`
	IsRead         bool     `json:"is_read"`
	Truncated      bool     `json:"truncated"`
	Tags           []string `json:"tags"` // 所属账号的标签快照（空为 []；004 迁移）
}

func toMessageResp(m *store.Message) messageResp {
	resp := messageResp{
		ID: m.ID, AccountID: m.AccountID, Folder: m.Folder, Subject: m.Subject,
		From: m.FromAddr, Snippet: m.Snippet, HasAttachments: m.HasAttachments,
		IsRead: m.IsRead, Truncated: m.Truncated,
		ReceivedAt: m.ReceivedAt.UTC().Format(timeFormat),
		Tags:       []string{},
	}
	if m.SentAt.Valid {
		t := m.SentAt.Time.UTC().Format(timeFormat)
		resp.SentAt = &t
	}
	if m.ToAddrs != "" {
		_ = json.Unmarshal([]byte(m.ToAddrs), &resp.To)
	}
	return resp
}

// handleMessageList GET /messages（DESIGN.md 6.3 全部查询参数）。
func (s *Server) handleMessageList(w http.ResponseWriter, r *http.Request) {
	f := store.MessageFilter{
		AccountID: queryID(r, "account_id"),
		Q:         r.URL.Query().Get("q"),
		Tags:      r.URL.Query()["tag"],
		Limit:     queryInt(r, "limit", 50),
		AfterID:   queryID(r, "after_id"),
		BeforeID:  queryID(r, "before_id"),
	}
	if v := r.URL.Query().Get("is_read"); v == "true" || v == "false" {
		b := v == "true"
		f.IsRead = &b
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = t
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = t
		}
	}
	// 排序（M4）：sort=time|account + order=asc|desc；排序模式走 offset 分页，
	// id 游标（before_id/after_id）仅默认模式有效。
	switch r.URL.Query().Get("sort") {
	case "time", "account":
		f.Sort = r.URL.Query().Get("sort")
	}
	if r.URL.Query().Get("order") == "asc" {
		f.Order = "asc"
	}
	if o := queryInt(r, "offset", 0); o > 0 {
		f.Offset = o
	}
	msgs, err := s.store.ListMessages(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	// 批量填充所属账号标签（一次查询；account_id 去重）
	acctIDs := make([]int64, 0, len(msgs))
	seen := map[int64]bool{}
	for _, m := range msgs {
		if !seen[m.AccountID] {
			seen[m.AccountID] = true
			acctIDs = append(acctIDs, m.AccountID)
		}
	}
	tagsByAcct := s.tagsFor(acctIDs)
	out := make([]messageResp, 0, len(msgs))
	for _, m := range msgs {
		resp := toMessageResp(m)
		if t := tagsByAcct[m.AccountID]; len(t) > 0 {
			resp.Tags = t
		}
		out = append(out, resp)
	}
	// total：同筛选条件的总数（前端"已加载 X / 共 Y"与 hasMore 精确判断；不含游标/OFFSET）
	total, _ := s.store.CountMessages(f)
	writeJSON(w, http.StatusOK, map[string]any{"messages": out, "total": total})
}

// handleMessageGet 详情：含 body_text/body_html 与附件列表（DESIGN.md 6.3）。
func (s *Server) handleMessageGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的邮件 id")
		return
	}
	m, err := s.store.GetMessage(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "message_not_found", "邮件不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	atts, _ := s.store.AttachmentsForMessage(id)
	type attResp struct {
		ID          int64  `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	}
	resp := struct {
		messageResp
		BodyText    string    `json:"body_text"`
		BodyHTML    string    `json:"body_html"`
		ParseError  string    `json:"parse_error,omitempty"`
		Attachments []attResp `json:"attachments"`
	}{messageResp: toMessageResp(m), BodyText: m.BodyText, BodyHTML: m.BodyHTML, ParseError: m.ParseError}
	if t, _ := s.store.AccountTagNames(m.AccountID); len(t) > 0 {
		resp.Tags = t
	}
	for _, a := range atts {
		resp.Attachments = append(resp.Attachments, attResp{a.ID, a.Filename, a.ContentType, a.Size})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMessagePatch 修改本地已读状态（不回写服务器，DESIGN.md 6.3）。
func (s *Server) handleMessagePatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的邮件 id")
		return
	}
	var body struct {
		IsRead *bool `json:"is_read"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.IsRead == nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "需要 {\"is_read\": true|false}")
		return
	}
	if err := s.store.SetRead(id, *body.IsRead); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "message_not_found", "邮件不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "patch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_read": *body.IsRead})
}

// handleBatchRead 批量标记已读（DESIGN.md 6.3）：ids 或 account_id（全部）。
func (s *Server) handleBatchRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs       []int64 `json:"ids"`
		AccountID int64   `json:"account_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil ||
		(len(body.IDs) == 0 && body.AccountID == 0) {
		writeErr(w, http.StatusBadRequest, "bad_request", "需要 ids 或 account_id")
		return
	}
	n, err := s.store.BatchRead(body.IDs, body.AccountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "batch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n})
}

// handleBatchDelete 批量删除本地副本（DESIGN.md 6.3）：删行同步、文件异步。
func (s *Server) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs       []int64 `json:"ids"`
		AccountID int64   `json:"account_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil ||
		(len(body.IDs) == 0 && body.AccountID == 0) {
		writeErr(w, http.StatusBadRequest, "bad_request", "需要 ids 或 account_id")
		return
	}
	paths, err := s.store.DeleteMessages(body.IDs, body.AccountID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "batch_failed", err.Error())
		return
	}
	// .eml 异步删除，失败由 M3 清理任务回收孤儿文件（DESIGN.md 6.3）。
	go removeFiles(s.cfg.DataDir, paths)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(paths)})
}

func (s *Server) handleMessageDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的邮件 id")
		return
	}
	paths, err := s.store.DeleteMessage(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "message_not_found", "邮件不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	go removeFiles(s.cfg.DataDir, paths)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleMessageRaw 下载原始 .eml（DESIGN.md 6.3）。
func (s *Server) handleMessageRaw(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的邮件 id")
		return
	}
	m, err := s.store.GetMessage(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "message_not_found", "邮件不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		writeErr(w, http.StatusNotFound, "raw_missing", ".eml 文件不存在: "+m.RawPath)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(m.RawPath)+`"`)
	io.Copy(w, f)
}

// handleAttachment 下载附件：从 .eml 实时解析（DESIGN.md §4 附件不落盘）。
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的邮件 id")
		return
	}
	attID, err := strconv.ParseInt(chi.URLParam(r, "attID"), 10, 64)
	if err != nil || attID <= 0 {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的附件 id")
		return
	}
	m, err := s.store.GetMessage(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "message_not_found", "邮件不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	att, err := s.store.GetAttachment(attID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && att.MessageID != id) {
		writeErr(w, http.StatusNotFound, "attachment_not_found", "附件不存在或不属于该邮件")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	raw, err := os.ReadFile(filepath.Join(s.cfg.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		writeErr(w, http.StatusNotFound, "raw_missing", ".eml 文件不存在: "+m.RawPath)
		return
	}
	filename, ctype, content, found := mailparse.FindAttachment(raw, att.PartIndex)
	if !found {
		writeErr(w, http.StatusNotFound, "attachment_not_found", "附件解析失败（原始邮件可能已截断）")
		return
	}
	if filename == "" {
		filename = "attachment"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(filename)+`"`)
	w.Write(content)
}

func sanitizeFilename(p string) string {
	base := filepath.Base(filepath.FromSlash(p))
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\', '/', ':', '*', '?', '<', '>', '|':
			return '_'
		}
		return r
	}, base)
}

// removeFiles 按相对路径删除磁盘文件（容忍缺失）。
func removeFiles(dataDir string, relPaths []string) {
	for _, rel := range relPaths {
		abs := filepath.Join(dataDir, filepath.FromSlash(rel))
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			continue // 孤儿文件由 M3 清理任务回收
		}
	}
	_ = mailparse.Parse
}

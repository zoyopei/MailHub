package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"mailhub/internal/store"
)

// WebhookTester 解耦 api → notify（仅测试事件需要；nil 时 test 端点返回 503）。
type WebhookTester interface {
	PostTest(wh *store.Webhook) (int, error)
}

func (s *Server) mountWebhooks(r chi.Router) {
	r.Post("/webhooks", s.handleWebhookCreate)
	r.Get("/webhooks", s.handleWebhookList)
	r.Patch("/webhooks/{id}", s.handleWebhookPatch)
	r.Delete("/webhooks/{id}", s.handleWebhookDelete)
	r.Post("/webhooks/{id}/test", s.handleWebhookTest)
	r.Get("/webhooks/{id}/deliveries", s.handleWebhookDeliveries)
	r.Post("/deliveries/{id}/retry", s.handleDeliveryRetry)
}

// webhookResp secret 仅在创建时返回一次（DESIGN 6.4），列表/详情不回显。
type webhookResp struct {
	ID          int64   `json:"id"`
	URL         string  `json:"url"`
	AccountIDs  []int64 `json:"account_ids"` // null = 全部账号
	IncludeBody bool    `json:"include_body"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   string  `json:"created_at"`
	Secret      *string `json:"secret,omitempty"` // 仅创建响应携带
	Pending     int     `json:"pending"`          // 待投递数
	Failed      int     `json:"failed"`           // 失败数
}

func toWebhookResp(w *store.Webhook, pending, failed int) webhookResp {
	return webhookResp{
		ID: w.ID, URL: w.URL, AccountIDs: w.AccountIDList(), IncludeBody: w.IncludeBody,
		Enabled: w.Enabled, CreatedAt: w.CreatedAt.UTC().Format(timeFormat),
		Pending: pending, Failed: failed,
	}
}

// handleWebhookCreate 创建（DESIGN 6.4）：secret 不传自动生成，仅本次响应返回。
func (s *Server) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL         string  `json:"url"`
		Secret      string  `json:"secret"`
		AccountIDs  []int64 `json:"account_ids"`
		IncludeBody bool    `json:"include_body"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.URL == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "需要合法 JSON 且 url 必填")
		return
	}
	if req.Secret == "" {
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		req.Secret = base64.StdEncoding.EncodeToString(b)
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	wh := &store.Webhook{URL: req.URL, Secret: req.Secret, IncludeBody: req.IncludeBody, Enabled: enabled}
	if req.AccountIDs != nil {
		wh.AccountIDs.String = mustJSON(req.AccountIDs)
		wh.AccountIDs.Valid = true
	}
	id, err := s.store.CreateWebhook(wh)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	resp := toWebhookResp(wh, 0, 0)
	resp.ID = id
	resp.Secret = &req.Secret // 唯一一次回显
	writeJSON(w, http.StatusCreated, resp)
}

// handleWebhookList 列表（含 pending/failed 统计）。
func (s *Server) handleWebhookList(w http.ResponseWriter, _ *http.Request) {
	hooks, err := s.store.ListWebhooks(store.WebhookFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	stats, _ := s.store.WebhookDeliveryStats()
	out := make([]webhookResp, 0, len(hooks))
	for _, h := range hooks {
		st := stats[h.ID]
		out = append(out, toWebhookResp(h, st[0], st[1]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": out})
}

func (s *Server) getWebhookOr404(w http.ResponseWriter, r *http.Request) *store.Webhook {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的 webhook id")
		return nil
	}
	wh, err := s.store.GetWebhook(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "webhook_not_found", "webhook 不存在")
		return nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return nil
	}
	return wh
}

// handleWebhookPatch 修改 url/账号范围/include_body/enabled。
func (s *Server) handleWebhookPatch(w http.ResponseWriter, r *http.Request) {
	wh := s.getWebhookOr404(w, r)
	if wh == nil {
		return
	}
	var req struct {
		URL         *string  `json:"url"`
		AccountIDs  *[]int64 `json:"account_ids"`
		IncludeBody *bool    `json:"include_body"`
		Enabled     *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	p := store.WebhookPatch{URL: req.URL, AccountIDs: req.AccountIDs,
		IncludeBody: req.IncludeBody, Enabled: req.Enabled}
	if err := s.store.ApplyWebhookPatch(wh.ID, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "patch_failed", err.Error())
		return
	}
	updated, err := s.store.GetWebhook(wh.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	stats, _ := s.store.WebhookDeliveryStats()
	st := stats[wh.ID]
	writeJSON(w, http.StatusOK, toWebhookResp(updated, st[0], st[1]))
}

// handleWebhookDelete 删除（投递记录级联）。
func (s *Server) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	wh := s.getWebhookOr404(w, r)
	if wh == nil {
		return
	}
	if err := s.store.DeleteWebhook(wh.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": wh.ID})
}

// handleWebhookTest 发送测试事件（不进队列）。
func (s *Server) handleWebhookTest(w http.ResponseWriter, r *http.Request) {
	wh := s.getWebhookOr404(w, r)
	if wh == nil {
		return
	}
	if s.tester == nil {
		writeErr(w, http.StatusServiceUnavailable, "deliverer_offline", "投递器未启用")
		return
	}
	code, err := s.tester.PostTest(wh)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status_code": code, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status_code": code})
}

// handleWebhookDeliveries 投递记录（分页、按状态筛选）。
func (s *Server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	wh := s.getWebhookOr404(w, r)
	if wh == nil {
		return
	}
	ds, total, err := s.store.ListDeliveries(store.DeliveryFilter{
		WebhookID: wh.ID,
		Status:    r.URL.Query().Get("status"),
		Page:      queryInt(r, "page", 1),
		Limit:     queryInt(r, "limit", 50),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	type dResp struct {
		ID           int64   `json:"id"`
		MessageID    int64   `json:"message_id"`
		Status       string  `json:"status"`
		Attempts     int     `json:"attempts"`
		NextRetryAt  *string `json:"next_retry_at"`
		ResponseCode *int    `json:"response_code"`
		LastError    *string `json:"last_error"`
		UpdatedAt    string  `json:"updated_at"`
	}
	out := make([]dResp, 0, len(ds))
	for _, d := range ds {
		item := dResp{
			ID: d.ID, MessageID: d.MessageID, Status: d.Status, Attempts: d.Attempts,
			UpdatedAt: d.UpdatedAt.UTC().Format(timeFormat),
		}
		if d.NextRetryAt.Valid {
			t := d.NextRetryAt.Time.UTC().Format(timeFormat)
			item.NextRetryAt = &t
		}
		if d.ResponseCode.Valid {
			c := int(d.ResponseCode.Int64)
			item.ResponseCode = &c
		}
		if d.LastError.Valid {
			item.LastError = &d.LastError.String
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "deliveries": out})
}

// handleDeliveryRetry 手动重发（failed → pending，立即投递）。
func (s *Server) handleDeliveryRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的投递 id")
		return
	}
	if err := s.store.RetryDelivery(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "delivery_not_found", "投递记录不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "retry_failed", err.Error())
		return
	}
	s.delivererWake()
	writeJSON(w, http.StatusOK, map[string]any{"retried": id})
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Package api — SSE 实时事件流（DESIGN.md 6/7，M4）。
// 事件模型：新邮件入库（worker.ingest → Manager.events.NotifyEvent）→ Hub 广播
// "message.new" 事件，载荷为单封邮件摘要（id/subject/from/account，供前端浏览器通知；
// 历史邮件不推，DESIGN 5.1 防轰炸）。投递器的唤醒仍走 Manager.notifier（Wake 信号）。
// 认证：EventSource 无法携带 Authorization 头；推荐先经 Bearer 鉴权的
// POST /api/v1/events/ticket 换取 60s 一次性短票，再用 ?ticket= 建连（不暴露主令牌
// 到反向代理访问日志）；同时保留 ?token= 兼容旧脚本。
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	sseTicketTTL   = 60 * time.Second
	sseMaxTickets  = 1024
)

// Hub 维护 SSE 订阅者与一次性连接票据；NotifyEvent 携带载荷广播（经 Manager.SetEventNotifier 注入）。
type Hub struct {
	mu      sync.Mutex
	subs    map[chan string]struct{}
	tickets map[string]time.Time // 一次性短票 → 过期时间
}

// NewHub 构造事件中心。
func NewHub() *Hub {
	return &Hub{
		subs:    map[chan string]struct{}{},
		tickets: map[string]time.Time{},
	}
}

// IssueTicket 签发一枚短期一次性 SSE 票据（60s 有效，消费即销毁）。
func (h *Hub) IssueTicket() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, exp := range h.tickets {
		if now.After(exp) {
			delete(h.tickets, k)
		}
	}
	if len(h.tickets) >= sseMaxTickets {
		// 极端异常时清理最旧一半，防内存无限增长
		for k := range h.tickets {
			delete(h.tickets, k)
			if len(h.tickets) < sseMaxTickets/2 {
				break
			}
		}
	}
	h.tickets[t] = now.Add(sseTicketTTL)
	return t, nil
}

// ConsumeTicket 校验并销毁一次性票据。
func (h *Hub) ConsumeTicket(t string) bool {
	if t == "" {
		return false
	}
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.tickets[t]
	if !ok {
		return false
	}
	delete(h.tickets, t)
	return !now.After(exp)
}

// NotifyEvent 向全部订阅者广播一条事件载荷（非阻塞：慢消费者只丢事件不阻塞 worker
// ——事件语义是"请刷新+可提醒"，丢一次无碍，下一次事件或兜底定时刷新会补上）。
func (h *Hub) NotifyEvent(data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- data:
		default:
		}
	}
}

func (h *Hub) subscribe() chan string {
	ch := make(chan string, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan string) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// handleEventTicket POST /api/v1/events/ticket（走 Bearer 认证）：签发 60s 一次性 SSE 票据。
func (s *Server) handleEventTicket(w http.ResponseWriter, _ *http.Request) {
	t, err := s.hub.IssueTicket()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ticket_failed", "生成票据失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": t, "expires_in": int(sseTicketTTL.Seconds())})
}

// handleEvents GET /api/v1/events?ticket=…（或兼容 ?token=…）：text/event-stream 长连接。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// 优先验证一次性短票；否则兼容旧版 ?token=（常量时间比较）
	authed := false
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		authed = s.hub.ConsumeTicket(ticket)
	} else if tok := r.URL.Query().Get("token"); tok != "" {
		authed = subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.APIToken)) == 1
	}
	if !authed {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "缺少或无效的 ticket / token")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no_flusher", "当前连接不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 代理不缓冲
	fmt.Fprint(w, "retry: 5000\n\n")          // 浏览器断线后 5s 重连
	fl.Flush()

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done(): // 客户端断开
			return
		case data := <-ch:
			fmt.Fprintf(w, "event: message.new\ndata: %s\n\n", data)
			fl.Flush()
		case <-heartbeat.C: // 注释行保活（防代理空闲断连）
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

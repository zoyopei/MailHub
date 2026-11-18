// Package api — SSE 实时事件流（DESIGN.md 6/7，M4）。
// 事件模型：新邮件入库（worker.ingest → Manager.events.NotifyEvent）→ Hub 广播
// "message.new" 事件，载荷为单封邮件摘要（id/subject/from/account，供前端浏览器通知；
// 历史邮件不推，DESIGN 5.1 防轰炸）。投递器的唤醒仍走 Manager.notifier（Wake 信号）。
// 认证：EventSource 无法携带 Authorization 头，本端点单独接受 ?token= 查询参数
// （常量时间比较；query 不进访问日志——logger 只记录 path）。
package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub 维护 SSE 订阅者；NotifyEvent 携带载荷广播（经 Manager.SetEventNotifier 注入）。
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// NewHub 构造事件中心。
func NewHub() *Hub {
	return &Hub{subs: map[chan string]struct{}{}}
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

// handleEvents GET /api/v1/events?token=…：text/event-stream 长连接。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// 单独认证（EventSource 无自定义头能力）
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.cfg.APIToken)) != 1 {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "缺少或错误的 token")
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

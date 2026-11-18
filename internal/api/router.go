// Package api 提供 REST API（DESIGN.md §6，chi 路由，TECH.md §2.8）。
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
	"mailhub/internal/sync"
	"mailhub/web"
)

// Server 聚合 API 依赖。
type Server struct {
	cfg     *config.Config
	store   *store.Store
	cipher  *crypto.Cipher
	manager *sync.Manager
	dial    func(*store.Account) mailclient.MailClient // 验证连接用（与 worker 同一构造，按 transport 分流）
	tester  WebhookTester                              // webhook 测试事件（notify.Deliverer；nil = 投递器未启用）
	wake    func()                                     // 手动重发后唤醒投递器（nil 安全）
	hub     *Hub                                       // SSE 事件中心（新邮件 → 浏览器即时刷新，M4）
	reauth  *mstoken.TokenCache                        // "重新获取令牌"（ROPC 密码换新 refresh_token，M4）；独立实例，测试可覆盖 Endpoint
}

func NewServer(cfg *config.Config, st *store.Store, c *crypto.Cipher,
	m *sync.Manager, dial func(*store.Account) mailclient.MailClient) *Server {
	return &Server{cfg: cfg, store: st, cipher: c, manager: m, dial: dial,
		hub: NewHub(), reauth: mstoken.NewTokenCache()}
}

// SetEventHub 注入外部 Hub（与 Manager 的 notifier 共享同一实例；main 组装时调用）。
func (s *Server) SetEventHub(h *Hub) {
	if h != nil {
		s.hub = h
	}
}

// SetDeliverer 注入投递器（测试事件与手动重发的唤醒）。
func (s *Server) SetDeliverer(t WebhookTester, wake func()) {
	s.tester, s.wake = t, wake
}

func (s *Server) delivererWake() {
	if s.wake != nil {
		s.wake()
	}
}

// Handler 构建路由与中间件。
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(s.logger)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.auth)
		s.mountAccounts(r)
		s.mountMessages(r)
		s.mountWebhooks(r)
		s.mountSettings(r)
		r.Get("/status", s.handleStatus)
	})
	// SSE：EventSource 无法带 Authorization 头，走 ?token= 单独认证（events.go）
	r.Get("/api/v1/events", s.handleEvents)

	// Web UI（M3，DESIGN.md 7）：内嵌静态文件，API 未命中即回落到前端。
	if dist, err := fs.Sub(web.Dist, "dist"); err == nil {
		fileServer := http.FileServer(http.FS(dist))
		r.Handle("/*", fileServer)
	}
	return r
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Debug("http", "method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "dur", time.Since(start).Round(time.Millisecond))
	})
}

// auth Bearer Token 认证，常量时间比较（DESIGN.md §8）。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
			token = h[7:]
		}
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIToken)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "缺少或错误的 Bearer Token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 工具 ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": errCode, "message": msg}})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func queryID(r *http.Request, key string) int64 {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// verifyAccount 同步验证登录（POST /accounts 与 /accounts/test 共用，DESIGN.md 6.2）。
// 传输分流由 dial 完成：graph → 验证 Graph 令牌；imap → 验证 IMAP TLS 登录。
func (s *Server) verifyAccount(acct *store.Account, cred mailclient.Credentials) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cl := s.dial(acct)
	_, err := cl.Connect(ctx, acct.IMAPHost, acct.IMAPPort, acct.Username, cred)
	if err != nil {
		return err
	}
	_ = cl.Logout()
	return nil
}

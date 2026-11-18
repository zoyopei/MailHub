// MailHub：邮件集中收取服务（DESIGN.md / TECH.md）。
// 组装顺序（TECH.md §2.9）：config → store(迁移) → crypto → notify(M2)
// → sync.Manager → api.ListenAndServe；SIGTERM/SIGINT 优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mailhub/internal/api"
	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/graphclient"
	"mailhub/internal/imapclient"
	"mailhub/internal/mailclient"
	"mailhub/internal/maintain"
	"mailhub/internal/mstoken"
	"mailhub/internal/notify"
	"mailhub/internal/store"
	"mailhub/internal/sync"
)

func main() {
	genSecret := flag.Bool("gen-secret", false, "生成随机 MAILHUB_SECRET_KEY 并退出")
	flag.Parse()

	if *genSecret {
		fmt.Println(config.GenSecret())
		return
	}

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.Info("mailhub starting", "listen", cfg.Listen, "data_dir", cfg.DataDir,
		"database", dbKind(cfg), "stagger", cfg.ConnectStagger)

	st, err := store.Open(cfg.DatabaseURL, cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	cipher, err := crypto.New(cfg.SecretKey)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 传输构造器：TokenCache 进程级共享（同账号复用 access_token，TECH.md §2.4）。
	// 按 acct.Transport 分流（DESIGN.md 3.6）：graph → Graph API；imap → IMAP TLS。
	tokens := mstoken.NewTokenCache()
	dial := func(acct *store.Account) mailclient.MailClient {
		if acct.Transport == "graph" {
			return graphclient.New(tokens, func(ids []string) (map[string]bool, error) {
				return st.HasGraphIDs(acct.ID, ids)
			})
		}
		return imapclient.NewGoClient(tokens)
	}

	// Webhook 投递器（DESIGN 5）：入库即 Wake，至少一次投递 + 重试。
	deliverer := notify.New(cfg, st)
	// SSE 事件中心（M4）：新邮件入库 → 浏览器即时刷新；与投递器并列被 Wake。
	hub := api.NewHub()

	// M2：notify.Deliverer 在此启动并注入 Manager（唤醒投递队列）；M1 传 nil（投递记录已入队，仅不投递）。
	// M4：SSE hub 走事件通道（带邮件摘要载荷，浏览器通知），不再作为 Wake 信号成员。
	manager := sync.NewManager(cfg, st, cipher, dial, deliverer)
	manager.SetEventNotifier(hub)
	go func() {
		if err := manager.Run(ctx); err != nil {
			slog.Error("sync manager exited", "error", err)
		}
	}()
	go deliverer.Run(ctx)

	// 每日维护：retention 清理 + 孤儿 .eml 回收（DESIGN.md 9.1，TECH.md §4）。
	go maintain.Run(ctx, cfg, st)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiServer(cfg, st, cipher, manager, dial, deliverer, hub),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("http listening", "addr", cfg.Listen)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) // ctx 取消同时停掉全部 worker（Manager.Run 内处理）
	}
	return nil
}

func dbKind(cfg *config.Config) string {
	if cfg.DatabaseURL == "" {
		return "sqlite"
	}
	return "postgres"
}

// apiServer 组装 API 并注入投递器（测试事件 + 手动重发唤醒）与 SSE hub。
func apiServer(cfg *config.Config, st *store.Store, c *crypto.Cipher,
	m *sync.Manager, dial func(*store.Account) mailclient.MailClient,
	d *notify.Deliverer, hub *api.Hub) http.Handler {
	s := api.NewServer(cfg, st, c, m, dial)
	s.SetDeliverer(d, d.Wake)
	s.SetEventHub(hub)
	return s.Handler()
}

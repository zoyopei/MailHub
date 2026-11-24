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
	gosync "sync"
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
	// SSE 事件中心（M4）：非历史的新邮件入库 → 推单封摘要给浏览器（刷新列表 / 桌面通知）。
	// 与投递器的 Wake 信号是两条通道：投递器只要"有活干了"，事件需要载荷。
	hub := api.NewHub()

	// M2：notify.Deliverer 在此启动并注入 Manager（唤醒投递队列）；M1 传 nil（投递记录已入队，仅不投递）。
	// M4：SSE hub 走事件通道（带邮件摘要载荷，浏览器通知），不再作为 Wake 信号成员。
	manager := sync.NewManager(cfg, st, cipher, dial, deliverer)
	manager.SetEventNotifier(hub)

	// 后台任务统一纳入 WaitGroup：停机时先等它们退出，再由 defer 关闭 store，
	// 避免 worker/投递器在 DB 关闭后仍写库。
	var bg gosync.WaitGroup
	bg.Add(3)
	go func() {
		defer bg.Done()
		if err := manager.Run(ctx); err != nil {
			slog.Error("sync manager exited", "error", err)
		}
	}()
	go func() {
		defer bg.Done()
		deliverer.Run(ctx)
	}()
	// 每日维护：retention 清理 + 孤儿 .eml 回收 + 投递记录清理（DESIGN.md 9.1/5.3，TECH.md §4）。
	go func() {
		defer bg.Done()
		maintain.Run(ctx, cfg, st)
	}()

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

	var runErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownHTTP(srv, hub)
	}

	stop() // 监听失败路径也要停掉后台任务
	done := make(chan struct{})
	go func() { bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(bgStopTimeout):
		slog.Warn("后台任务未在限时内退出，强制关闭", "timeout", bgStopTimeout)
	}
	return runErr
}

// 停机预算。两个阶段加起来要落在容器的停止宽限期之内（docker-compose.yml 的
// stop_grace_period: 30s；docker 默认只有 10s，超时即 SIGKILL）。
const (
	// httpShutdownTimeout 等活动请求结束的上限。SSE 连接由 shutdownHTTP 先行结束，
	// 所以正常情况下 HTTP 停机是毫秒级的；这个上限只兜"卡住的慢请求"。
	httpShutdownTimeout = 5 * time.Second
	// bgStopTimeout 等 worker/投递器/维护退出的上限。IMAP 的 IDLE 收尾（≤10s）与 LOGOUT（≤5s）
	// 都有上限，看门狗保证 ctx 取消后连接立即断开，正常远用不满。
	bgStopTimeout = 20 * time.Second
)

// shutdownHTTP 优雅停机 HTTP：先结束 SSE 长连接，再等其余请求收尾。
//
// srv.Shutdown 不主动关闭活动连接，只等它们自然结束——SSE 长连接永不结束。只要有一个开着的
// Web 邮件页，Shutdown 就会等满超时，而容器的停止宽限期往往比它还短，结果是被 SIGKILL。
// Hub.Close 让全部 SSE 处理器立即返回（浏览器会在服务重启后自动重连），Shutdown 随即完成。
func shutdownHTTP(srv *http.Server, hub *api.Hub) {
	hub.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http 停机超时，强制关闭剩余连接", "error", err)
		_ = srv.Close()
	}
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

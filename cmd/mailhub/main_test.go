package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mailhub/internal/api"
	"mailhub/internal/config"
	"mailhub/internal/crypto"
	"mailhub/internal/mailclient"
	"mailhub/internal/store"
	"mailhub/internal/sync"
)

const mainTestToken = "main-test-token"

// stubClient 不连任何地方的假传输（组装 API Server 需要一个 Manager，但不会运行它）。
type stubClient struct{}

func (stubClient) Connect(context.Context, string, int, string, mailclient.Credentials) (string, error) {
	return "", errors.New("测试桩：不应连接")
}
func (stubClient) FetchFolder(context.Context, string, *store.FolderState, time.Time,
	func(mailclient.MsgKey, time.Time, []byte) error) (mailclient.FetchResult, error) {
	return mailclient.FetchResult{}, nil
}
func (stubClient) SupportsIdle() bool                                { return false }
func (stubClient) Idle(context.Context, time.Duration) (bool, error) { return false, nil }
func (stubClient) Logout() error                                     { return nil }
func (stubClient) Close() error                                      { return nil }

// newTestHTTP 在临时端口上跑一个真实的 API Server（含 SSE 路由），返回它与事件中心、基地址。
func newTestHTTP(t *testing.T) (*http.Server, *api.Hub, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open("", filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, _ := crypto.New(make([]byte, 32))
	cfg := &config.Config{APIToken: mainTestToken, DataDir: dir, PollIntervalDefault: 60,
		ConnectStagger: time.Millisecond, MaxMessageSize: 1 << 20}
	dial := func(*store.Account) mailclient.MailClient { return stubClient{} }
	m := sync.NewManager(cfg, st, c, dial, nil)
	apiSrv := api.NewServer(cfg, st, c, m, dial)
	hub := api.NewHub()
	apiSrv.SetEventHub(hub)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &http.Server{Handler: apiSrv.Handler()}
	go h.Serve(ln) //nolint:errcheck // 退出路径由 Shutdown/Close 触发，返回 ErrServerClosed
	t.Cleanup(func() { h.Close() })
	return h, hub, "http://" + ln.Addr().String()
}

// openSSE 像开着 Web 邮件页那样建立一条 SSE 长连接，返回读取器。
func openSSE(t *testing.T, base string) *bufio.Reader {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/api/v1/events?token="+mainTestToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应为 text/event-stream，得 %q", ct)
	}
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "retry:") {
		t.Fatalf("SSE 流应先下发重连间隔，得 %q（%v）", line, err)
	}
	return br
}

// 开着 SSE 长连接（Web 邮件页）时停机：不能等满超时。http.Server.Shutdown 只等活动连接
// 自然结束，而 SSE 永不结束——不先关事件中心的话这里会卡满 httpShutdownTimeout，
// 容器的停止宽限期往往比它还短。
func TestShutdownHTTPWithSSE(t *testing.T) {
	srv, hub, base := newTestHTTP(t)
	sse := openSSE(t, base)

	done := make(chan struct{})
	go func() {
		shutdownHTTP(srv, hub)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second): // 远小于 httpShutdownTimeout（5s）
		t.Fatal("SSE 长连接拖住了 HTTP 停机")
	}
	// 流是服务端主动结束的（不是等客户端断开）
	assertStreamEnded(t, "唯一连接", sse)
}

// 多条 SSE 连接同样立即结束；没有活动连接时停机干脆且可重复调用。
func TestShutdownHTTPIdleAndRepeat(t *testing.T) {
	srv, hub, base := newTestHTTP(t)
	a, b := openSSE(t, base), openSSE(t, base)

	for range 2 {
		start := time.Now()
		shutdownHTTP(srv, hub)
		if d := time.Since(start); d > time.Second {
			t.Fatalf("停机应立即完成，实际等了 %v", d)
		}
	}
	assertStreamEnded(t, "a", a)
	assertStreamEnded(t, "b", b)
}

// assertStreamEnded 断言 SSE 流已由服务端结束：读取以 EOF 正常收尾（io.ReadAll 把 EOF 当成功，
// 返回 nil），剩余内容只有空白（首帧之后的空行）。读取放在 goroutine 上并限时——流要是还挂着，
// 直接读会永远阻塞。
func assertStreamEnded(t *testing.T, name string, br *bufio.Reader) {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(br)
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("SSE 连接 %s 应正常读到流结束，得 %v", name, r.err)
		}
		if strings.TrimSpace(string(r.data)) != "" {
			t.Fatalf("SSE 连接 %s 停机后不应再有内容，得 %q", name, r.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("SSE 连接 %s 应在停机时结束，实际仍挂着", name)
	}
}

package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// SSE：Wake 后订阅者收到 message.new 事件；断开自动清理。
func TestSSEEvents(t *testing.T) {
	srv, _, _ := testServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events?token="+testToken, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// 从 Server 拿 hub 触发广播（真实路径是 worker.ingest → Manager.events.NotifyEvent）
	events := make(chan string, 2)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var gotEvent bool
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: message.new") {
				gotEvent = true
			}
			if strings.HasPrefix(line, "data: ") && gotEvent {
				events <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond) // 等订阅完成
	lastTestServer.hub.NotifyEvent(`{"id":7,"subject":"你好","from":"a@x.com","account":"a@x.com"}`)

	select {
	case data := <-events:
		if !strings.Contains(data, `"subject":"你好"`) {
			t.Fatalf("载荷应透传邮件摘要，得 %s", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 message.new 事件")
	}
}

// 坏 token → 401（浏览器 EventSource 收到非 200 不再自动重连）。
func TestSSEBadToken(t *testing.T) {
	srv, _, _ := testServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/events?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("坏 token 应 401，得 %d", resp.StatusCode)
	}
}

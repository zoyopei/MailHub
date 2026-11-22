package sync

import (
	"context"
	"testing"
	"time"

	"mailhub/internal/imapclient"
	"mailhub/internal/store"
)

// 认证失败退出的 worker 必须从 Manager 注销：手动收取（TriggerNow）才能重新拉起账号，
// 而不是把信号发给已退出、无人消费的旧 worker。
func TestTriggerNowRestartsAuthFailedWorker(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{connectErr: imapclient.ErrAuthFailed,
		msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	_, id := newTestWorker(t, m, st, "af@x.com", cl)
	st.SetAccountStatus(id, store.StatusPolling, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx) }()

	// 认证失败 → 状态 auth_failed 且 worker 已注销
	waitFor(t, func() bool {
		a, _ := st.GetAccount(id)
		m.mu.Lock()
		_, alive := m.workers[id]
		m.mu.Unlock()
		return a.Status == store.StatusAuthFailed && !alive
	})

	// 凭据修复后手动收取：应重新 spawn 并完成收取
	cl.connectErr = nil
	m.TriggerNow(id)
	waitFor(t, func() bool {
		msgs, _ := st.ListMessages(store.MessageFilter{AccountID: id})
		return len(msgs) == 1
	})
}

// 库中历史脏数据 poll_interval <= 0：worker 不得 panic（rand.Int64N 负参数），
// 且不得空转（下限钳制到 minPollInterval）。
func TestWorkerNonPositivePollIntervalNoPanic(t *testing.T) {
	for _, pi := range []int{0, -10} {
		m, st, _ := testEnv(t)
		cl := &fakeClient{msgs: map[string][]fakeMsg{}}
		w, _ := newTestWorker(t, m, st, "pi@x.com", cl)
		w.acct.PollInterval = pi

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { w.run(ctx); close(done) }()
		time.Sleep(200 * time.Millisecond) // 至少完成一轮并进入周期等待
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("poll_interval=%d: worker 未随 ctx 退出", pi)
		}
	}
}

// Manager.Run 返回前必须等待全部 worker 退出（优雅停机后再关 store）。
func TestManagerRunWaitsWorkers(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	_, id := newTestWorker(t, m, st, "wait@x.com", cl)
	st.SetAccountStatus(id, store.StatusPolling, nil)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = m.Run(ctx); close(runDone) }()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, ok := m.workers[id]
		return ok
	})
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()

	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Manager.Run 未退出")
	}
	select {
	case <-w.done: // Run 返回时 worker 必已退出
	default:
		t.Fatal("Manager.Run 返回时 worker 仍在运行")
	}
}

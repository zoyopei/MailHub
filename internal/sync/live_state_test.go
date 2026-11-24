package sync

import (
	"context"
	"sync"
	"testing"

	"mailhub/internal/mailclient"
	"mailhub/internal/store"
)

// 提醒开关对运行中的 worker 即时生效（不重启）：PATCH 改库 → Manager.SetNotifyEnabled 通知
// worker → 下一封新邮件按新值决定是否生成投递记录。worker 若只读启动时的账号快照，
// 打开开关后不会投递、关掉后仍继续投递。
func TestNotifyToggleTakesEffectWithoutRestart(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &fakeClient{msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}
	w, id := newTestWorker(t, m, st, "live@x.com", cl)
	m.mu.Lock()
	m.workers[id] = w // 注册到 Manager，与 spawn 一致
	m.mu.Unlock()
	if _, err := st.CreateWebhook(&store.Webhook{URL: "http://127.0.0.1:1/hook", Secret: "s", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	deliveries := func() int {
		_, total, err := st.ListDeliveries(store.DeliveryFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return total
	}
	arrive := func(uid uint32) {
		t.Helper()
		cl.msgs["INBOX"] = append(cl.msgs["INBOX"], fakeMsg{uid: uid, raw: []byte(rawMail)})
		if err := w.session(context.Background()); err != nil {
			t.Fatalf("session: %v", err)
		}
	}
	// 与 handleAccountPatch 相同的两步：改库 + 通知 worker（不重启）
	patchNotify := func(enabled bool) {
		t.Helper()
		if err := st.ApplyAccountPatch(id, store.AccountPatch{NotifyEnabled: &enabled}); err != nil {
			t.Fatal(err)
		}
		m.SetNotifyEnabled(id, enabled)
	}

	if err := w.session(context.Background()); err != nil { // 首次同步：历史邮件，无投递
		t.Fatal(err)
	}
	arrive(2) // 开关关闭：不投递
	if n := deliveries(); n != 0 {
		t.Fatalf("提醒关闭时不应生成投递记录，得 %d", n)
	}

	patchNotify(true)
	arrive(3) // 打开后：同一 worker 实例立即投递
	if n := deliveries(); n != 1 {
		t.Fatalf("打开提醒后新邮件应生成投递记录（worker 未重启），得 %d", n)
	}

	patchNotify(false)
	arrive(4) // 关掉后：不再投递
	if n := deliveries(); n != 1 {
		t.Fatalf("关闭提醒后不应再生成投递记录，得 %d", n)
	}

	// worker 不在运行（禁用/auth_failed/排队中）：只是空操作，不 panic
	m.SetNotifyEnabled(id+999, true)
}

// 令牌轮换后 worker 的内存快照也要更新：下个会话必须带上轮换后的新令牌
// （入库由 TestWorkerTokenRotationPersisted 覆盖）。否则 worker 一直用启动时的旧令牌，
// TokenCache（存的是服务端返回的新令牌）永不命中，每个轮询周期都会再打一次令牌端点。
func TestWorkerTokenRotationUpdatesSnapshot(t *testing.T) {
	m, st, _ := testEnv(t)
	cl := &rotatingClient{fakeClient: fakeClient{rotated: "RT-rotated",
		msgs: map[string][]fakeMsg{"INBOX": {{uid: 1, raw: []byte(rawMail)}}}}}
	w, id := newTestWorker(t, m, st, "rot@x.com", cl)
	acct, _ := st.GetAccount(id)
	acct.AuthType = "oauth2" // oauth2 账号：凭据存的是 refresh_token
	w.acct = acct

	for i := 0; i < 3; i++ { // 轮询模式的每个周期是一个新会话
		if err := w.session(context.Background()); err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
	}
	got := cl.tokens()
	if len(got) != 3 {
		t.Fatalf("应建立 3 个会话，得 %d", len(got))
	}
	if got[0] != "pw" {
		t.Fatalf("首个会话应使用库中的原令牌，得 %q", got[0])
	}
	for i, tok := range got[1:] {
		if tok != "RT-rotated" {
			t.Fatalf("第 %d 个会话应使用轮换后的令牌，得 %q（内存快照未更新）", i+2, tok)
		}
	}
}

// rotatingClient 记录每次 Connect 收到的 refresh_token（首会话返回 rotated）。
type rotatingClient struct {
	fakeClient
	mu sync.Mutex
	rt []string
}

func (f *rotatingClient) Connect(ctx context.Context, host string, port int, user string,
	cred mailclient.Credentials) (string, error) {
	f.mu.Lock()
	f.rt = append(f.rt, cred.RefreshToken)
	f.mu.Unlock()
	return f.fakeClient.Connect(ctx, host, port, user, cred)
}

func (f *rotatingClient) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rt...)
}

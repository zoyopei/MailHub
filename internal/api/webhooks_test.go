package api

import (
	"testing"

	"mailhub/internal/store"
)

// fakeTester webhook 测试事件的假投递器（注入见 api_test.go testServer）。
type fakeTester struct {
	called int
}

func (f *fakeTester) PostTest(_ *store.Webhook) (int, error) {
	f.called++
	return 200, nil
}

func TestWebhookCRUDAndTest(t *testing.T) {
	srv, st, _ := testServer(t)

	// 创建：secret 省略自动生成，仅本次返回（DESIGN 6.4）
	rec, out := do(t, srv, "POST", "/api/v1/webhooks",
		`{"url":"http://downstream.example/hook","include_body":true}`, testToken)
	if rec.Code != 201 {
		t.Fatalf("create: %d %v", rec.Code, out)
	}
	if secret, _ := out["secret"].(string); secret == "" {
		t.Fatal("创建响应应返回自动生成的 secret（唯一一次）")
	}

	// 列表：不回显 secret
	_, list := do(t, srv, "GET", "/api/v1/webhooks", "", testToken)
	hooks := list["webhooks"].([]any)
	if len(hooks) != 1 {
		t.Fatalf("应有 1 个 webhook，得 %d", len(hooks))
	}
	if m := hooks[0].(map[string]any); m["secret"] != nil {
		t.Fatal("列表不应回显 secret")
	}

	// PATCH：限定账号范围 + 停用
	rec, _ = do(t, srv, "PATCH", "/api/v1/webhooks/1",
		`{"account_ids":[1],"enabled":false}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("patch: %d", rec.Code)
	}
	_, list = do(t, srv, "GET", "/api/v1/webhooks", "", testToken)
	if m := list["webhooks"].([]any)[0].(map[string]any); m["enabled"] != false {
		t.Fatal("patch 未生效")
	}

	// 测试事件：经注入的 fake 投递器
	rec, out = do(t, srv, "POST", "/api/v1/webhooks/1/test", ``, testToken)
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("test: %d %v", rec.Code, out)
	}

	// 投递记录端点（空）
	rec, out = do(t, srv, "GET", "/api/v1/webhooks/1/deliveries", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("deliveries: %d", rec.Code)
	}

	// 删除
	rec, _ = do(t, srv, "DELETE", "/api/v1/webhooks/1", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	_, list = do(t, srv, "GET", "/api/v1/webhooks", "", testToken)
	if len(list["webhooks"].([]any)) != 0 {
		t.Fatal("删除后列表应为空")
	}
	_ = st
}

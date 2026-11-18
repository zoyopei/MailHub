package api

import (
	"testing"

	"mailhub/internal/store"
)

// GET/PUT /api/v1/settings：读取生效值（DB 覆盖 > env）、写入覆盖、null 清除、范围校验。
func TestSettingsGetPut(t *testing.T) {
	srv, st, _ := testServer(t)

	// 初始：全部取 env/默认层，无覆盖
	rec, out := do(t, srv, "GET", "/api/v1/settings", "", testToken)
	if rec.Code != 200 {
		t.Fatalf("get: %d %v", rec.Code, out)
	}
	s0 := out["settings"].(map[string]any)
	if len(s0) != len(store.SettingKeys) {
		t.Fatalf("应返回 %d 项，得 %d", len(store.SettingKeys), len(s0))
	}
	rd := s0["retention_days"].(map[string]any)
	if rd["value"] != float64(0) || rd["overridden"] != false { // cfg.RetentionDays 零值 = 永久
		t.Fatalf("retention_days 初始应为 env 层 0 且未覆盖，得 %v", rd)
	}
	pd := s0["poll_interval_default"].(map[string]any)
	if pd["value"] != float64(60) { // testServer cfg.PollIntervalDefault = 60
		t.Fatalf("poll_interval_default 初始应为 60，得 %v", pd)
	}

	// 写入覆盖
	rec, out = do(t, srv, "PUT", "/api/v1/settings",
		`{"retention_days":30,"idle_budget_per_host":0}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("put: %d %v", rec.Code, out)
	}
	s1 := out["settings"].(map[string]any)
	rd = s1["retention_days"].(map[string]any)
	if rd["value"] != float64(30) || rd["overridden"] != true {
		t.Fatalf("retention_days 应为覆盖值 30，得 %v", rd)
	}
	ib := s1["idle_budget_per_host"].(map[string]any)
	if ib["value"] != float64(0) || ib["overridden"] != true { // 0 = 不限，合法覆盖
		t.Fatalf("idle_budget_per_host 应为覆盖值 0，得 %v", ib)
	}

	// 库里确实写入（maintain/worker 读同一路径）
	vals, err := st.GetSettings()
	if err != nil || vals[store.SettingRetentionDays] != "30" {
		t.Fatalf("store 覆盖未落库: %v %v", vals, err)
	}

	// null 清除 → 回退 env 层
	rec, out = do(t, srv, "PUT", "/api/v1/settings", `{"retention_days":null}`, testToken)
	if rec.Code != 200 {
		t.Fatalf("put(null): %d %v", rec.Code, out)
	}
	s2 := out["settings"].(map[string]any)
	rd = s2["retention_days"].(map[string]any)
	if rd["value"] != float64(0) || rd["overridden"] != false {
		t.Fatalf("清除后应回退 env 层，得 %v", rd)
	}

	// 范围与白名单校验
	rec, _ = do(t, srv, "PUT", "/api/v1/settings", `{"initial_sync_days":0}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("initial_sync_days=0 应 400，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "PUT", "/api/v1/settings", `{"poll_interval_default":1}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("poll=1 低于下限应 400，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "PUT", "/api/v1/settings", `{"no_such_key":1}`, testToken)
	if rec.Code != 400 {
		t.Fatalf("未知键应 400，得 %d", rec.Code)
	}
}

// 覆盖 poll_interval_default 后，创建账号（未显式指定间隔）应采用覆盖值。
func TestSettingsPollDefaultApplies(t *testing.T) {
	srv, st, _ := testServer(t)
	rec, _ := do(t, srv, "PUT", "/api/v1/settings", `{"poll_interval_default":120}`, testToken)
	if rec.Code != 200 {
		t.Fatal("put settings 失败")
	}
	body := `{"email":"me@example.com","imap_host":"imap.example.com","password":"pw"}`
	rec, _ = do(t, srv, "POST", "/api/v1/accounts", body, testToken)
	if rec.Code != 201 {
		t.Fatal("create 失败")
	}
	acct, err := st.GetAccountByEmail("me@example.com")
	if err != nil || acct == nil {
		t.Fatalf("账号未创建: %v", err)
	}
	if acct.PollInterval != 120 {
		t.Fatalf("新账号应采用覆盖值 120，得 %d", acct.PollInterval)
	}
}

// settings PUT 需要认证。
func TestSettingsAuth(t *testing.T) {
	srv, _, _ := testServer(t)
	rec, _ := do(t, srv, "GET", "/api/v1/settings", "", "")
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401，得 %d", rec.Code)
	}
	rec, _ = do(t, srv, "PUT", "/api/v1/settings", `{"retention_days":1}`, "bad")
	if rec.Code != 401 {
		t.Fatalf("错 token 应 401，得 %d", rec.Code)
	}
}

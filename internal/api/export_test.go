package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"mailhub/internal/store"
)

// 迁移闭环：导出（含凭据）→ 删除 → 导回 → 设置与凭据逐一还原；重复导回 skipped。
func TestAccountExportImportRoundtrip(t *testing.T) {
	srv, st, _ := testServer(t)

	// 建两个账号：oauth2（graph）与 password（imap），带各自设置与标签
	do(t, srv, "POST", "/api/v1/accounts", `{
		"email":"a@outlook.com","name":"迁移A","auth_type":"oauth2","client_id":"cid-a",
		"refresh_token":"rt-a","transport":"graph","folders":["INBOX","JunkEmail"],
		"poll_interval":300,"notify_enabled":true,"tags":["生产"]}`, testToken)
	do(t, srv, "POST", "/api/v1/accounts", `{
		"email":"b@163.com","name":"迁移B","auth_type":"password","password":"pw-b",
		"imap_host":"imap.163.com","poll_interval":120,"tags":["测试","批量"]}`, testToken)

	// 默认导出：配置齐全但不含凭据
	rec, body := do(t, srv, "GET", "/api/v1/accounts/export", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status %d", rec.Code)
	}
	accts := body["accounts"]
	_ = accts
	var plain []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil {
		t.Fatalf("导出应为裸数组: %v", err)
	}
	if len(plain) != 2 {
		t.Fatalf("应导出 2 个账号，得 %d", len(plain))
	}
	for _, a := range plain {
		if a["refresh_token"] != "" || a["password"] != "" {
			t.Fatalf("默认导出不应含凭据: %v", a)
		}
	}

	// 含凭据导出（存为变量：两次导回都用它）
	rec, _ = do(t, srv, "GET", "/api/v1/accounts/export?include_credentials=true", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("export credentials status %d", rec.Code)
	}
	exportJSON := rec.Body.String()
	if err := json.Unmarshal([]byte(exportJSON), &plain); err != nil {
		t.Fatal(err)
	}
	cred := map[string]string{}
	for _, a := range plain {
		email := a["email"].(string)
		switch email {
		case "a@outlook.com":
			if a["refresh_token"] != "rt-a" {
				t.Fatalf("oauth2 凭据未解密还原: %v", a["refresh_token"])
			}
			cred[email] = "rt-a"
		case "b@163.com":
			if a["password"] != "pw-b" {
				t.Fatalf("password 凭据未解密还原: %v", a["password"])
			}
			cred[email] = "pw-b"
		}
	}

	// 删除全部账号后导回：created=2；再导回一次：skipped=2（不覆盖）
	ids := []int64{}
	list, total, _ := st.ListAccounts(store.AccountFilter{})
	_ = total
	for _, a := range list {
		ids = append(ids, a.ID)
	}
	for _, id := range ids {
		do(t, srv, "DELETE", fmt.Sprintf("/api/v1/accounts/%d", id), "", testToken)
	}
	rec, body = do(t, srv, "POST", "/api/v1/accounts/batch", exportJSON, testToken)
	if rec.Code != http.StatusOK || body["created"] != float64(2) {
		t.Fatalf("导回应创建 2 个: code=%d body=%v", rec.Code, body)
	}
	rec, body = do(t, srv, "POST", "/api/v1/accounts/batch", exportJSON, testToken)
	_ = rec
	if body["skipped"] != float64(2) {
		t.Fatalf("重复导回应 skipped=2: %v", body)
	}

	// 逐项验证还原：folders/tags/poll_interval/notify_enabled/transport/凭据密文
	migrated, _, _ := st.ListAccounts(store.AccountFilter{})
	if len(migrated) != 2 {
		t.Fatalf("迁移后应 2 个账号，得 %d", len(migrated))
	}
	for _, a := range migrated {
		switch a.Email {
		case "a@outlook.com":
			if a.Name != "迁移A" || a.Transport != "graph" || a.PollInterval != 300 || !a.NotifyEnabled {
				t.Fatalf("oauth2 账号设置未还原: %+v", a)
			}
			if a.Folders != `["INBOX","JunkEmail"]` {
				t.Fatalf("folders 未还原: %s", a.Folders)
			}
			tags, _ := st.AccountTagNames(a.ID)
			if len(tags) != 1 || tags[0] != "生产" {
				t.Fatalf("tags 未还原: %v", tags)
			}
			// 凭据密文可解密回原值（加密随机但解密唯一）
			dec, err := lastTestServer.cipher.Decrypt(a.CredentialEnc)
			if err != nil || string(dec) != "rt-a" {
				t.Fatalf("oauth2 凭据未还原: %q %v", dec, err)
			}
		case "b@163.com":
			if a.PollInterval != 120 || a.NotifyEnabled {
				t.Fatalf("password 账号设置未还原: %+v", a)
			}
			tags, _ := st.AccountTagNames(a.ID)
			if len(tags) != 2 {
				t.Fatalf("tags 未还原: %v", tags)
			}
			dec, err := lastTestServer.cipher.Decrypt(a.CredentialEnc)
			if err != nil || string(dec) != "pw-b" {
				t.Fatalf("password 凭据未还原: %q %v", dec, err)
			}
		}
	}
}

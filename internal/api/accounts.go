package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

// domainPreset 常见邮箱的服务器与认证预设（DESIGN.md 6.2：域名 → 服务器与认证方式自动成立）。
// transport: imap | graph——Outlook 家族存量授权仅含 Graph 权限，Graph 为主传输（DESIGN.md 3.6）。
type domainPreset struct {
	host      string
	port      int
	authType  string
	transport string
}

var domainPresets = map[string]domainPreset{
	// Outlook 家族 → XOAUTH2 + Graph 传输（批量导入的主路径）
	"outlook.com":    {host: "outlook.office365.com", authType: "oauth2", transport: "graph"},
	"outlook.com.cn": {host: "outlook.office365.com", authType: "oauth2", transport: "graph"},
	"hotmail.com":    {host: "outlook.office365.com", authType: "oauth2", transport: "graph"},
	"live.com":       {host: "outlook.office365.com", authType: "oauth2", transport: "graph"},
	"msn.com":        {host: "outlook.office365.com", authType: "oauth2", transport: "graph"},
	// 常见密码认证邮箱
	"gmail.com":    {host: "imap.gmail.com", authType: "password"},
	"qq.com":       {host: "imap.qq.com", authType: "password"},
	"foxmail.com":  {host: "imap.qq.com", authType: "password"},
	"163.com":      {host: "imap.163.com", authType: "password"},
	"126.com":      {host: "imap.126.com", authType: "password"},
	"yeah.net":     {host: "imap.yeah.net", authType: "password"},
	"icloud.com":   {host: "imap.mail.me.com", authType: "password"},
	"me.com":       {host: "imap.mail.me.com", authType: "password"},
	"fastmail.com": {host: "imap.fastmail.com", authType: "password"},
}

func presetFor(email string) (domainPreset, bool) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return domainPreset{}, false
	}
	p, ok := domainPresets[strings.ToLower(email[at+1:])]
	return p, ok
}

func (s *Server) mountAccounts(r chi.Router) {
	r.Post("/accounts", s.handleAccountCreate)
	r.Post("/accounts/batch", s.handleAccountBatchImport)
	r.Post("/accounts/sync", s.handleAccountsSyncBatch)
	r.Patch("/accounts/batch", s.handleAccountBatchPatch)
	r.Get("/accounts", s.handleAccountList)
	r.Get("/accounts/export", s.handleAccountExport) // 静态段优先于 /accounts/{id}
	r.Post("/accounts/test", s.handleAccountTest)
	r.Get("/accounts/{id}", s.handleAccountGet)
	r.Patch("/accounts/{id}", s.handleAccountPatch)
	r.Delete("/accounts/{id}", s.handleAccountDelete)
	r.Post("/accounts/{id}/sync", s.handleAccountSync)
	r.Post("/accounts/{id}/reauth", s.handleAccountReauth)
	r.Get("/accounts/{id}/reauth-url", s.handleAccountReauthURL)
	r.Get("/tags", s.handleTagList)
}

// ---- 请求/响应结构 ----

type accountReq struct {
	Name          string   `json:"name"`
	Email         string   `json:"email"`
	IMAPHost      string   `json:"imap_host"`
	IMAPPort      int      `json:"imap_port"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	AuthType      string   `json:"auth_type"`
	ClientID      string   `json:"client_id"`
	RefreshToken  string   `json:"refresh_token"`
	Transport     string   `json:"transport"` // imap | graph；缺省按域名预设（Outlook 家族 → graph）
	Folders       []string `json:"folders"`
	PollInterval  int      `json:"poll_interval"`
	NotifyEnabled bool     `json:"notify_enabled"`
	Tags          []string `json:"tags"` // 标签（可选，创建时写入）
}

type accountResp struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Email         string   `json:"email"`
	IMAPHost      string   `json:"imap_host"`
	IMAPPort      int      `json:"imap_port"`
	Username      string   `json:"username"`
	AuthType      string   `json:"auth_type"`
	ClientID      string   `json:"client_id"` // 应用注册 ID，非机密；密码/令牌绝不回显
	Transport     string   `json:"transport"`
	Folders       []string `json:"folders"`
	PollInterval  int      `json:"poll_interval"`
	Enabled       bool     `json:"enabled"`
	NotifyEnabled bool     `json:"notify_enabled"`
	Status        string   `json:"status"`
	LastError     *string  `json:"last_error"`
	LastSyncAt    *string  `json:"last_sync_at"`
	LastMessageAt *string  `json:"last_message_at"`
	MessageCount  int      `json:"message_count"`
	UnreadCount   int      `json:"unread_count"`
	Tags          []string `json:"tags"` // 标签（空为 []；004 迁移）
}

// withTags 填充标签（nil → 空数组，前端友好）。
func (r accountResp) withTags(tags []string) accountResp {
	r.Tags = tags
	if r.Tags == nil {
		r.Tags = []string{}
	}
	return r
}

func (s *Server) accountResp(a *store.Account, st stats) accountResp {
	resp := accountResp{
		ID: a.ID, Name: a.Name, Email: a.Email, IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort,
		Username: a.Username, AuthType: a.AuthType, ClientID: a.ClientID,
		Transport: a.Transport, PollInterval: a.PollInterval,
		Folders:       []string{},
		Enabled:       a.Enabled,
		NotifyEnabled: a.NotifyEnabled,
		Status:        a.Status, MessageCount: st.count, UnreadCount: st.unread,
	}
	var folders []string
	if err := json.Unmarshal([]byte(a.Folders), &folders); err == nil && folders != nil {
		resp.Folders = folders
	}
	if a.LastError.Valid {
		resp.LastError = &a.LastError.String
	}
	if a.LastSyncAt.Valid {
		t := a.LastSyncAt.Time.UTC().Format(timeFormat)
		resp.LastSyncAt = &t
	}
	if st.lastMsg != nil {
		resp.LastMessageAt = st.lastMsg
	}
	return resp
}

type stats struct {
	count   int
	unread  int
	lastMsg *string
}

const timeFormat = "2006-01-02T15:04:05Z"

// statsFor 批量聚合账号统计（message/unread/last_message_at）。
func (s *Server) statsFor(ids []int64) map[int64]stats {
	out := map[int64]stats{}
	if len(ids) == 0 {
		return out
	}
	m, err := s.store.StatsByAccounts(ids)
	if err != nil {
		return out
	}
	for id, v := range m {
		st := stats{count: v.Count, unread: v.Unread}
		if v.LastMsg.Valid {
			t := v.LastMsg.Time.UTC().Format(timeFormat)
			st.lastMsg = &t
		}
		out[id] = st
	}
	return out
}

// ---- handlers ----

// handleAccountCreate 单个添加：先同步验证 IMAP 登录，失败 400（DESIGN.md 6.2）。
// 邮箱已存在返回 409，且不再做登录验证——没必要为一条注定被拒的请求再去登一次远端邮箱。
func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var req accountReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	acct, cred, err := s.buildAccount(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_account", err.Error())
		return
	}
	if _, err := s.store.GetAccountByEmail(acct.Email); err == nil {
		writeErr(w, http.StatusConflict, "account_exists", "该邮箱已存在")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	if err := s.verifyAccount(acct, cred); err != nil {
		writeErr(w, http.StatusBadRequest, "verify_failed",
			"登录验证失败（"+acct.Transport+"）: "+err.Error())
		return
	}
	id, err := s.store.CreateAccount(acct)
	if err != nil {
		if isUniqueViolation(err) { // 与并发请求撞车（上面的判重与插入之间）
			writeErr(w, http.StatusConflict, "account_exists", "该邮箱已存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "create_failed", err.Error())
		return
	}
	acct.ID = id
	if len(req.Tags) > 0 {
		if err := s.store.SetAccountTags(id, req.Tags); err != nil {
			writeErr(w, http.StatusInternalServerError, "set_tags_failed", err.Error())
			return
		}
	}
	s.manager.ReloadAccount(id)
	writeJSON(w, http.StatusCreated, s.accountResp(acct, stats{}).withTags(req.Tags))
}

// buildAccount 由请求构建账号（应用域名预设、加密凭据）。
func (s *Server) buildAccount(req accountReq) (*store.Account, mailclient.Credentials, error) {
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return nil, mailclient.Credentials{}, errors.New("email 必填")
	}
	if req.Username == "" {
		req.Username = req.Email
	}
	if req.Transport != "" && req.Transport != "imap" && req.Transport != "graph" {
		return nil, mailclient.Credentials{}, errors.New("transport 仅支持 imap | graph")
	}
	// 域名预设独立判断（parseAccountLine 已填 host/auth_type，但 transport 仍需预设补全）
	if req.IMAPHost == "" || req.AuthType == "" || req.Transport == "" {
		if p, ok := presetFor(req.Email); ok {
			if req.IMAPHost == "" {
				req.IMAPHost = p.host
			}
			if req.AuthType == "" {
				req.AuthType = p.authType
			}
			if req.Transport == "" && p.transport != "" {
				req.Transport = p.transport
			}
		} else if req.IMAPHost == "" {
			return nil, mailclient.Credentials{}, errors.New("imap_host 必填（该域名无预设）")
		}
	}
	if req.IMAPPort == 0 {
		req.IMAPPort = 993
	}
	if req.IMAPPort < 1 || req.IMAPPort > 65535 {
		return nil, mailclient.Credentials{}, errors.New("imap_port 应在 1-65535 之间")
	}
	if req.AuthType == "" {
		req.AuthType = "password"
	}
	if req.Transport == "" {
		req.Transport = "imap"
	}
	if req.Transport == "graph" && req.AuthType != "oauth2" {
		return nil, mailclient.Credentials{}, errors.New("graph 传输仅支持 oauth2 账号")
	}

	var plain string
	cred := mailclient.Credentials{AuthType: req.AuthType, ClientID: req.ClientID}
	switch req.AuthType {
	case "oauth2":
		if req.ClientID == "" || req.RefreshToken == "" {
			return nil, cred, errors.New("oauth2 账号需要 client_id 与 refresh_token")
		}
		plain = req.RefreshToken
		cred.RefreshToken = plain
	case "password":
		if req.Password == "" {
			return nil, cred, errors.New("password 账号需要 password")
		}
		plain = req.Password
		cred.Password = plain
	default:
		return nil, cred, errors.New("auth_type 仅支持 password | oauth2")
	}
	enc, err := s.cipher.Encrypt([]byte(plain))
	if err != nil {
		return nil, cred, err
	}

	folders := `["INBOX"]`
	if len(req.Folders) > 0 {
		b, _ := json.Marshal(req.Folders)
		folders = string(b)
	}
	name := req.Name
	if name == "" {
		name = req.Email
	}
	poll := req.PollInterval
	if poll == 0 {
		// 新账号默认轮询间隔：全局配置可覆盖（DB > env，创建账号时读取，DESIGN.md 9.1）
		poll = s.settingInt(store.SettingPollDefault, s.cfg.PollIntervalDefault)
	}
	if poll < minPollInterval {
		return nil, cred, errPollInterval
	}
	return &store.Account{
		Name: name, Email: req.Email, IMAPHost: req.IMAPHost, IMAPPort: req.IMAPPort,
		UseTLS: true, Username: req.Username, AuthType: req.AuthType, ClientID: req.ClientID,
		CredentialEnc: enc, Folders: folders, PollInterval: poll,
		NotifyEnabled: req.NotifyEnabled, Enabled: true, Status: store.StatusPending,
		Transport: req.Transport,
	}, cred, nil
}

// errAccountExists 批量导入的"已存在"哨兵：计入 skipped 而非 failed（迁移语义：不覆盖）。
var errAccountExists = errors.New("该邮箱已存在")

// minPollInterval 账号轮询周期下限（秒），与 MAILHUB_POLL_INTERVAL_DEFAULT 的校验一致；
// 0/负数会让 worker 空转或 panic，必须在入口拦截。
const minPollInterval = 5

var errPollInterval = errors.New("poll_interval 不能小于 5 秒")

// handleAccountBatchImport 批量导入：JSON 数组或 text/plain（main.py ----格式）。
// 只入库（pending），后台错峰验证（DESIGN.md 2.2、6.2）。
// JSON 模式接受与导出（GET /accounts/export）同构的迁移格式：名称/文件夹/轮询/提醒/标签等
// 一并还原；已存在的邮箱跳过不覆盖（skipped 计数）。
func (s *Server) handleAccountBatchImport(w http.ResponseWriter, r *http.Request) {
	type failed struct {
		Line  int    `json:"line"`
		Error string `json:"error"`
	}
	result := struct {
		Created int      `json:"created"`
		Skipped int      `json:"skipped"`
		Failed  []failed `json:"failed"`
	}{Failed: []failed{}}

	importOne := func(req accountReq, line int) error {
		acct, _, err := s.buildAccount(req)
		if err != nil {
			return err
		}
		if _, err := s.store.GetAccountByEmail(acct.Email); err == nil {
			return errAccountExists
		}
		id, err := s.store.CreateAccount(acct)
		if err != nil {
			return err
		}
		if len(req.Tags) > 0 {
			if err := s.store.SetAccountTags(id, req.Tags); err != nil {
				return err
			}
		}
		return nil
	}
	importLine := func(req accountReq, line int) {
		if err := importOne(req, line); err != nil {
			if errors.Is(err, errAccountExists) {
				result.Skipped++
				return
			}
			result.Failed = append(result.Failed, failed{Line: line, Error: err.Error()})
			return
		}
		result.Created++
	}

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "text/plain"):
		body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			req, err := parseAccountLine(line)
			if err != nil {
				result.Failed = append(result.Failed, failed{Line: i + 1, Error: err.Error()})
				continue
			}
			importLine(req, i+1)
		}
	default: // JSON（含 GET /accounts/export 的迁移格式，直接 POST 回本端点即闭环）
		var reqs []accountReq
		if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&reqs); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request",
				"请求体应为 JSON 数组或 Content-Type: text/plain 的账号文本")
			return
		}
		for i, req := range reqs {
			importLine(req, i+1)
		}
	}

	if result.Created > 0 {
		// 唤醒错峰队列消费新账号。
		s.manager.ReloadBatch()
	}
	writeJSON(w, http.StatusOK, result)
}

// handleAccountExport 导出账号（迁移用）：accountReq 同构的裸 JSON 数组，
// 可直接 POST /accounts/batch 导回（闭环迁移）。
//   - 默认仅配置（不含凭据），用于备份/审查；
//   - include_credentials=true 时附解密凭据（oauth2 → refresh_token；password → password），
//     这是"API 永不回显凭据"原则的**显式例外**（迁移必须迁移凭据），文件为明文，责任在调用方；
//   - q/status/tag 筛选与列表一致（可只导某个标签的账号）。
func (s *Server) handleAccountExport(w http.ResponseWriter, r *http.Request) {
	includeCred := r.URL.Query().Get("include_credentials") == "true"

	var accts []*store.Account
	for page := 1; ; page++ {
		list, total, err := s.store.ListAccounts(store.AccountFilter{
			Q: r.URL.Query().Get("q"), Status: r.URL.Query().Get("status"),
			Tags: r.URL.Query()["tag"], Page: page, Limit: 500,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "export_failed", err.Error())
			return
		}
		accts = append(accts, list...)
		if len(list) == 0 || len(accts) >= total {
			break
		}
	}

	ids := make([]int64, 0, len(accts))
	for _, a := range accts {
		ids = append(ids, a.ID)
	}
	tags := s.tagsFor(ids)

	out := make([]accountReq, 0, len(accts))
	for _, a := range accts {
		req := accountReq{
			Name: a.Name, Email: a.Email, IMAPHost: a.IMAPHost, IMAPPort: a.IMAPPort,
			Username: a.Username, AuthType: a.AuthType, ClientID: a.ClientID,
			Transport: a.Transport, PollInterval: a.PollInterval,
			NotifyEnabled: a.NotifyEnabled, Tags: tags[a.ID],
		}
		if len(req.Tags) == 0 {
			req.Tags = []string{}
		}
		_ = json.Unmarshal([]byte(a.Folders), &req.Folders)
		if includeCred {
			if plain, err := s.cipher.Decrypt(a.CredentialEnc); err == nil {
				switch a.AuthType {
				case "oauth2":
					req.RefreshToken = string(plain)
				case "password":
					req.Password = string(plain)
				}
			}
		}
		out = append(out, req)
	}
	w.Header().Set("Content-Disposition", `attachment; filename="mailhub-accounts.json"`)
	slog.Info("accounts exported", "count", len(out), "include_credentials", includeCred) // 只记数量与开关
	writeJSON(w, http.StatusOK, out)
}

// parseAccountLine 解析 main.py 的账号行：邮箱----密码----client_id----refresh_token。
// 第二段密码在 XOAUTH2 流程中不使用（占位符），不存储（DESIGN.md 6.2）。
func parseAccountLine(line string) (accountReq, error) {
	parts := strings.Split(line, "----")
	if len(parts) != 4 {
		return accountReq{}, errors.New("格式错误：应为 邮箱----密码----client_id----令牌")
	}
	email := strings.TrimSpace(parts[0])
	clientID := strings.TrimSpace(parts[2])
	token := strings.TrimSpace(parts[3])
	if email == "" || clientID == "" || token == "" {
		return accountReq{}, errors.New("邮箱 / client_id / 令牌 不能为空")
	}
	req := accountReq{Email: email, ClientID: clientID, RefreshToken: token}
	p, ok := presetFor(email)
	if !ok || p.authType != "oauth2" {
		return accountReq{}, errors.New("文本导入仅支持 Outlook/Hotmail 家族（oauth2 预设）；其他邮箱请用 JSON")
	}
	req.AuthType = "oauth2"
	req.IMAPHost = p.host
	return req, nil
}

// handleAccountList 分页列表：page/limit、q、status、tag（可重复，OR 命中）（DESIGN.md 6.2）。
func (s *Server) handleAccountList(w http.ResponseWriter, r *http.Request) {
	accts, total, err := s.store.ListAccounts(store.AccountFilter{
		Q:      r.URL.Query().Get("q"),
		Status: r.URL.Query().Get("status"),
		Tags:   r.URL.Query()["tag"],
		Page:   queryInt(r, "page", 1),
		Limit:  queryInt(r, "limit", 50),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	ids := make([]int64, 0, len(accts))
	for _, a := range accts {
		ids = append(ids, a.ID)
	}
	st := s.statsFor(ids)
	tags := s.tagsFor(ids)
	out := make([]accountResp, 0, len(accts))
	for _, a := range accts {
		out = append(out, s.accountResp(a, st[a.ID]).withTags(tags[a.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "accounts": out})
}

// tagsFor 批量取标签（失败容忍：返回空 map，标签缺失不阻断列表）。
func (s *Server) tagsFor(ids []int64) map[int64][]string {
	m, err := s.store.TagsForAccounts(ids)
	if err != nil {
		return map[int64][]string{}
	}
	return m
}

func (s *Server) handleAccountGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的账号 id")
		return
	}
	a, err := s.store.GetAccount(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "account_not_found", "账号不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	tags, _ := s.store.AccountTagNames(id)
	writeJSON(w, http.StatusOK, s.accountResp(a, s.statsFor([]int64{id})[id]).withTags(tags))
}

// handleAccountPatch 更新：连接类变更触发 worker 重启，行为开关即时生效（DESIGN.md 6.2）。
func (s *Server) handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的账号 id")
		return
	}
	var body struct {
		Name          *string   `json:"name"`
		IMAPHost      *string   `json:"imap_host"`
		Username      *string   `json:"username"`
		Password      *string   `json:"password"`
		RefreshToken  *string   `json:"refresh_token"`
		ClientID      *string   `json:"client_id"`
		Folders       *[]string `json:"folders"`
		PollInterval  *int      `json:"poll_interval"`
		NotifyEnabled *bool     `json:"notify_enabled"`
		Enabled       *bool     `json:"enabled"`
		Tags          *[]string `json:"tags"` // 全量替换；[] 清空
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	if body.PollInterval != nil && *body.PollInterval < minPollInterval {
		writeErr(w, http.StatusBadRequest, "invalid_account", errPollInterval.Error())
		return
	}
	// 凭据字段"传了但为空串"几乎总是前端/脚本的失误（把"留空 = 不修改"原样发了上来）；
	// 照做会把凭据覆盖成空，账号随即 auth_failed。省略字段才表示不修改。
	for _, c := range []struct {
		name string
		v    *string
	}{{"password", body.Password}, {"refresh_token", body.RefreshToken}, {"client_id", body.ClientID}} {
		if c.v != nil && *c.v == "" {
			writeErr(w, http.StatusBadRequest, "invalid_account",
				c.name+" 不能为空字符串（不修改请省略该字段）")
			return
		}
	}

	p := store.AccountPatch{
		Name: body.Name, IMAPHost: body.IMAPHost, Username: body.Username,
		ClientID: body.ClientID, Folders: nil, PollInterval: body.PollInterval,
		NotifyEnabled: body.NotifyEnabled, Enabled: body.Enabled,
	}
	if body.Folders != nil {
		b, _ := json.Marshal(*body.Folders)
		fs := string(b)
		p.Folders = &fs
	}
	if body.Password != nil {
		enc, err := s.cipher.Encrypt([]byte(*body.Password))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt_failed", err.Error())
			return
		}
		p.CredentialEnc = enc
	}
	if body.RefreshToken != nil {
		enc, err := s.cipher.Encrypt([]byte(*body.RefreshToken))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encrypt_failed", err.Error())
			return
		}
		p.CredentialEnc = enc
	}

	if err := s.store.ApplyAccountPatch(id, p); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "account_not_found", "账号不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "patch_failed", err.Error())
		return
	}

	// 提醒开关即时生效：只更新运行中 worker 的实时值，不重启（不打断 IDLE 会话和进行中的
	// 拉取）。worker 持有的是启动时的账号快照，不通知它的话开关要到下次重启才生效。
	if body.NotifyEnabled != nil {
		s.manager.SetNotifyEnabled(id, *body.NotifyEnabled)
	}

	// 连接类/周期类变更或重新启用 → 重启 worker；仅行为开关（notify/name/tags）→ 不重启。
	restart := body.IMAPHost != nil || body.Username != nil || body.Password != nil ||
		body.RefreshToken != nil || body.ClientID != nil || body.Folders != nil ||
		body.PollInterval != nil || (body.Enabled != nil && *body.Enabled)
	if restart {
		s.manager.ReloadAccount(id)
	} else if body.Enabled != nil && !*body.Enabled {
		s.manager.RemoveAccount(id)
	}

	// 标签：全量替换（纯元数据，不动 worker）
	if body.Tags != nil {
		if err := s.store.SetAccountTags(id, *body.Tags); err != nil {
			writeErr(w, http.StatusInternalServerError, "set_tags_failed", err.Error())
			return
		}
	}

	a, err := s.store.GetAccount(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	tags, _ := s.store.AccountTagNames(id)
	writeJSON(w, http.StatusOK, s.accountResp(a, s.statsFor([]int64{id})[id]).withTags(tags))
}

// reauthScope 重新获取令牌用的 scope：按传输分流（Graph 是 Outlook 家族主传输，TECH.md §2）。
func reauthScope(a *store.Account) string {
	if a.Transport == "graph" {
		return mstoken.GraphScope
	}
	return mstoken.IMAPScope
}

// handleAccountReauthURL GET /accounts/{id}/reauth-url：授权页地址（带 login_hint 预填目标邮箱、
// scope 含 openid）。前端不自己拼这个地址：scope 与 openid 的组合只在这里维护一份，
// 与兑换端点（handleAccountReauth）严格一致——两边不一致的话令牌端点不返回 id_token。
func (s *Server) handleAccountReauthURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的账号 id")
		return
	}
	a, err := s.store.GetAccount(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "account_not_found", "账号不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	if a.AuthType != "oauth2" || a.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "not_oauth2", "仅 OAuth2（令牌）账号支持重新获取令牌")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url": mstoken.AuthorizeURL(a.ClientID, mstoken.MSNativeRedirect, reauthScope(a), a.Email)})
}

// maskEmail 邮箱本地部分打码（m***@outlook.com）：错误信息要提示"登成了哪个账号"才好排错，
// 但不把完整地址写进响应和日志。
func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 {
		return "***"
	}
	return email[:1] + "***" + email[at:]
}

// handleAccountReauth 重新获取令牌（M4，授权码流）：refresh_token 过期/失效的 Outlook 账号，
// 用户在浏览器完成 Microsoft 授权登录后把跳转 URL（含 code）粘回，兑换全新 refresh_token，
// **校验登录身份确为目标邮箱**，加密持久化并立即重启 worker 重试。
// 注：密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），
// 授权码流是 MSA 账号的可行路径；code 一次性、约 10 分钟有效。
func (s *Server) handleAccountReauth(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的账号 id")
		return
	}
	var body struct {
		Code string `json:"code"` // 授权码，或授权完成后的完整跳转 URL
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	code := mstoken.ExtractAuthCode(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "请提供授权码或授权完成后的完整跳转地址")
		return
	}
	a, err := s.store.GetAccount(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "account_not_found", "账号不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "get_failed", err.Error())
		return
	}
	if a.AuthType != "oauth2" || a.ClientID == "" {
		writeErr(w, http.StatusBadRequest, "not_oauth2", "仅 OAuth2（令牌）账号支持重新获取令牌")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	newRefresh, idToken, err := s.reauth.FetchByAuthCode(ctx, code, a.ClientID, mstoken.MSNativeRedirect,
		reauthScope(a))
	if err != nil {
		slog.Warn("reauth failed", "account_id", id, "error", err) // 只记事件与错误，不记 code
		writeErr(w, http.StatusBadRequest, "reauth_failed", err.Error())
		return
	}
	// 校验登录身份。授权页带了 login_hint，但浏览器里往往还登着别的 Microsoft 账号，点一下
	// "接受"换来的可能就是别人的令牌——不拦的话它会被写进这个账号：本账号从此 auth_failed，
	// 而原令牌可能已经因为轮换作废了。拿不到身份信息同样拒绝（不能"验不出来就算通过"）。
	who, err := mstoken.IdentityFromIDToken(idToken)
	if err != nil {
		slog.Warn("reauth identity unavailable", "account_id", id, "error", err)
		writeErr(w, http.StatusBadRequest, "identity_unverified",
			"无法确认登录的账号（"+err.Error()+"），凭据未修改。请重新打开授权页，用目标邮箱登录")
		return
	}
	if !who.Matches(a.Email) {
		slog.Warn("reauth rejected: signed-in account mismatch", "account_id", id) // 不记登录的是哪个邮箱
		writeErr(w, http.StatusBadRequest, "account_mismatch",
			"登录的账号（"+maskEmail(who.String())+"）与目标账号（"+a.Email+"）不一致，凭据未修改。"+
				"请重新打开授权页，用目标邮箱登录（浏览器里登着别的账号时，建议用无痕窗口）")
		return
	}
	enc, err := s.cipher.Encrypt([]byte(newRefresh))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encrypt_failed", err.Error())
		return
	}
	if err := s.store.UpdateCredential(id, enc); err != nil {
		writeErr(w, http.StatusInternalServerError, "update_failed", err.Error())
		return
	}
	s.manager.ReloadAccount(id) // 新令牌立即生效：重启 worker 重试收取
	slog.Info("reauth ok, token replaced", "account_id", id, "email", a.Email)

	resp, _ := s.store.GetAccount(id)
	tags, _ := s.store.AccountTagNames(id)
	writeJSON(w, http.StatusOK, s.accountResp(resp, s.statsFor([]int64{id})[id]).withTags(tags))
}

// handleTagList 全部标签与引用数（前端筛选下拉）。
func (s *Server) handleTagList(w http.ResponseWriter, _ *http.Request) {
	tags, err := s.store.ListTags()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		out = append(out, map[string]any{"name": t.Name, "count": t.Count})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tags": out})
}

// handleAccountBatchPatch 批量启停 / 批量提醒开关（DESIGN.md 6.2）。
func (s *Server) handleAccountBatchPatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs           []int64 `json:"ids"`
		Enabled       *bool   `json:"enabled"`
		NotifyEnabled *bool   `json:"notify_enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil || len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "需要 ids 与 enabled/notify_enabled")
		return
	}
	updated := 0
	for _, id := range body.IDs {
		p := store.AccountPatch{Enabled: body.Enabled, NotifyEnabled: body.NotifyEnabled}
		if err := s.store.ApplyAccountPatch(id, p); err == nil {
			updated++
			if body.NotifyEnabled != nil {
				s.manager.SetNotifyEnabled(id, *body.NotifyEnabled) // 即时生效，见 handleAccountPatch
			}
			if body.Enabled != nil {
				if *body.Enabled {
					s.manager.ReloadAccount(id)
				} else {
					s.manager.RemoveAccount(id)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// handleAccountDelete 删除账号；purge=true 时先取 raw_path 再删行，最后清磁盘（DESIGN.md 6.2）。
//
// 不带 purge 时磁盘上的 .eml 并不会保留：邮件记录随账号级联删除，这些文件失去引用，
// 下一轮每日维护会把它们当孤儿回收（maintain.orphanEmlGrace 之后）。purge 只决定"现在删"
// 还是"稍后自动删"。
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的账号 id")
		return
	}
	purge := r.URL.Query().Get("purge") == "true"
	var paths []string
	if purge {
		paths, _ = s.store.ListRawPathsByAccount(id)
	}
	s.manager.RemoveAccount(id)
	if err := s.store.DeleteAccount(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "account_not_found", "账号不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}
	if purge {
		removeFiles(s.cfg.DataDir, paths)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "purged_files": len(paths)})
}

// handleAccountSync 手动收取单账号：插入信号，立即执行（DESIGN.md 6.2、3.2）。
func (s *Server) handleAccountSync(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_id", "无效的账号 id")
		return
	}
	if _, err := s.store.GetAccount(id); err != nil {
		writeErr(w, http.StatusNotFound, "account_not_found", "账号不存在")
		return
	}
	s.manager.TriggerNow(id)
	writeJSON(w, http.StatusAccepted, map[string]any{"triggered": id})
}

// handleAccountsSyncBatch 批量手动收取：经错峰队列（DESIGN.md 6.2）。
func (s *Server) handleAccountsSyncBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&body); err != nil || len(body.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "需要 ids")
		return
	}
	s.manager.TriggerBatch(body.IDs)
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": len(body.IDs)})
}

// handleAccountTest 仅测试连接，不保存（DESIGN.md 6.2）。
func (s *Server) handleAccountTest(w http.ResponseWriter, r *http.Request) {
	var req accountReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	acct, cred, err := s.buildAccount(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_account", err.Error())
		return
	}
	if err := s.verifyAccount(acct, cred); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

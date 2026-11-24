package graphclient

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

const rawMail1 = `From: a@b.c
To: x@y.z
Subject: graph one
Message-ID: <g1@b.c>

body 1
`

// graphEnv 搭 fake token + fake Graph 服务。
type graphEnv struct {
	tokenSrv *httptest.Server
	graphSrv *httptest.Server
	client   *Client
}

func newTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"AT-x","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newGraphEnv(t *testing.T, known map[string]bool) *graphEnv {
	t.Helper()

	tokenSrv := newTokenServer(t)

	// messages: id → MIME 原文
	msgs := map[string]string{"m1": rawMail1, "m2": rawMail1, "m3": rawMail1}
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer AT-x" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/$value"): // /me/messages/{id}/$value
			id := strings.TrimSuffix(strings.TrimPrefix(p, "/me/messages/"), "/$value")
			raw, ok := msgs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "message/rfc822")
			fmt.Fprint(w, raw)
		case strings.HasSuffix(p, "/messages"): // /me/mailFolders/inbox/messages
			t1 := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
			t2 := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
			fmt.Fprintf(w, `{"value":[`+
				`{"id":"m1","receivedDateTime":%q},`+
				`{"id":"m2","receivedDateTime":%q},`+
				`{"id":"m3","receivedDateTime":%q}]}`, t1, t1, t2)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(graphSrv.Close)

	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = tokenSrv.URL
	c := New(tokens, func(ids []string) (map[string]bool, error) { return known, nil })
	c.SetBase(graphSrv.URL)
	return &graphEnv{tokenSrv: tokenSrv, graphSrv: graphSrv, client: c}
}

func connect(t *testing.T, c *Client) {
	t.Helper()
	rotated, err := c.Connect(context.Background(), "", 0, "u@outlook.com", mailclient.Credentials{
		AuthType: "oauth2", ClientID: "cid", RefreshToken: "RT",
	})
	if err != nil || rotated != "" {
		t.Fatalf("Connect: %q %v", rotated, err)
	}
}

func TestFetchFolderDedupAndCursor(t *testing.T) {
	env := newGraphEnv(t, map[string]bool{"m2": true}) // m2 已入库
	connect(t, env.client)

	type got struct {
		id   string
		recv time.Time
		raw  string
	}
	var items []got
	res, err := env.client.FetchFolder(context.Background(), "INBOX", nil,
		time.Now().Add(-7*24*time.Hour),
		func(k mailclient.MsgKey, seenAt time.Time, raw []byte) error {
			items = append(items, got{id: k.GraphID, recv: seenAt, raw: string(raw)})
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// m2 已见过：只回调 m1/m3
	if len(items) != 2 || items[0].id != "m1" || items[1].id != "m3" {
		t.Fatalf("应去重 m2 后回调 m1/m3: %+v", items)
	}
	if !strings.Contains(items[0].raw, "graph one") {
		t.Fatalf("$value 原文错误: %q", items[0].raw)
	}
	// latestSeen = 列表最大 receivedDateTime（含已见过的 m3 时间）
	if res.LatestSeen.IsZero() {
		t.Fatal("latestSeen 不应为零")
	}
}

// 游标有效时 $filter 从游标开始（fake server 不校验 filter 值，这里验证请求参数透传）。
// 只记录列表请求的 query——后续还有 delta 引导请求，不能覆盖。
func TestFetchFolderCursorParams(t *testing.T) {
	var filterVal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			filterVal, _ = url.QueryUnescape(r.URL.RawQuery)
			fmt.Fprint(w, `{"value":[]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newTokenServer(t).URL
	c := New(tokens, nil)
	c.SetBase(srv.URL)
	connect(t, c)

	cursorSeen := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	cursor := &store.FolderState{LastSeenAt: sql.NullTime{Time: cursorSeen, Valid: true}}
	_, err := c.FetchFolder(context.Background(), "INBOX", cursor, time.Time{},
		func(mailclient.MsgKey, time.Time, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filterVal, "receivedDateTime ge "+cursorSeen.Format(time.RFC3339)) {
		t.Fatalf("filter 应从游标开始: %s", filterVal)
	}
}

// 刷新成功之后仍然 401：令牌本身没问题，是这个授权不允许读邮件（应用注册缺 Mail.Read、
// 用户未同意）。同样只能重新授权，算 ErrReauth（auth_failed）。只刷新一次，不能无限循环。
func TestFetchFolder401AfterRenewIsReauth(t *testing.T) {
	var tokenCalls atomic.Int32
	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newGraphServer(t, func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"AT-x","expires_in":3600}`)
	}).URL
	c := New(tokens, nil)
	c.SetBase(newGraphServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 一律 401
	}).URL)
	connect(t, c)

	_, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-time.Hour), noop)
	if !errors.Is(err, mstoken.ErrReauth) {
		t.Fatalf("401 应映射 ErrReauth，得 %v", err)
	}
	// 连接时一次 + 401 后强制刷新一次；刷新后仍 401 就停，不再刷新
	if n := tokenCalls.Load(); n != 2 {
		t.Fatalf("令牌端点应被调用 2 次（连接 + 一次强制刷新），得 %d", n)
	}
}

// 强制刷新失败：只有令牌端点**明确拒绝**（invalid_grant）才是凭据失效 → ErrReauth；
// 刷新本身出错（端点 5xx、断网）是连接类故障，必须交 worker 退避重试——算成 auth_failed 的话
// 账号会停在那里，重启也不恢复。
func TestFetchFolder401RenewFailureClass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		body       string
		wantReauth bool
	}{
		{"令牌端点拒绝（invalid_grant）", http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{"令牌端点 5xx", http.StatusInternalServerError, `{"error":"server_error"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenCalls atomic.Int32
			tokens := mstoken.NewTokenCache()
			tokens.Endpoint = newGraphServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tokenCalls.Add(1) > 1 { // 连接时正常；之后（强制刷新）失败
					w.WriteHeader(tc.code)
					fmt.Fprint(w, tc.body)
					return
				}
				fmt.Fprint(w, `{"access_token":"AT-x","expires_in":3600}`)
			}).URL
			c := New(tokens, nil)
			c.SetBase(newGraphServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}).URL)
			connect(t, c)

			_, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-time.Hour), noop)
			if err == nil {
				t.Fatal("应返回错误")
			}
			if got := errors.Is(err, mstoken.ErrReauth); got != tc.wantReauth {
				t.Fatalf("errors.Is(err, ErrReauth) = %v，应为 %v（err=%v）", got, tc.wantReauth, err)
			}
			if n := tokenCalls.Load(); n != 2 {
				t.Fatalf("令牌端点应被调用 2 次，得 %d", n)
			}
		})
	}
}

func TestGraphFolderMapping(t *testing.T) {
	// well-known 名不区分大小写，规范成 Graph 的小写写法
	for in, want := range map[string]string{
		"INBOX": "inbox", "InBox": "inbox", "JunkEmail": "junkemail", "Archive": "archive",
		" SentItems ": "sentitems",
	} {
		if g := graphFolder(in); g != want {
			t.Errorf("graphFolder(%q) = %q，应为 %q", in, g, want)
		}
	}
	// 自定义文件夹的显示名、文件夹 id 区分大小写：一律转小写会让 Graph 找不到文件夹
	for _, name := range []string{"My Folder", "INBOX.Sub", "AAMkAGI2AAIsAAA=", "存档"} {
		if g := graphFolder(name); g != name {
			t.Errorf("graphFolder(%q) = %q，自定义文件夹名应原样传", name, g)
		}
	}
}

// fastRetry 把限流等待缩短到测试可接受的量级（测试结束恢复）。
func fastRetry(t *testing.T) {
	t.Helper()
	oldCap, oldBase := retryAfterCap, retryAfterNoHeader
	retryAfterCap, retryAfterNoHeader = 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() { retryAfterCap, retryAfterNoHeader = oldCap, oldBase })
}

// newFakeClient 用"恒发 AT-x"的令牌端点 + 给定 Graph 处理器构造客户端（未连接）。
func newFakeClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newTokenServer(t).URL
	c := New(tokens, nil)
	c.SetBase(newGraphServer(t, handler).URL)
	return c
}

func newGraphServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func noop(_ mailclient.MsgKey, _ time.Time, _ []byte) error { return nil }

// collect 返回把回调的 Graph ID 记进切片的 handle。
func collect(ids *[]string) func(mailclient.MsgKey, time.Time, []byte) error {
	return func(k mailclient.MsgKey, _ time.Time, _ []byte) error {
		*ids = append(*ids, k.GraphID)
		return nil
	}
}

// delta 滚动：cursor 带 deltaLink → 直接 GET 该链接（不再 $filter 列表），
// 响应只含增量消息并带回新 deltaLink；下一轮空 value 只推进链接。
func TestFetchFolderDeltaScroll(t *testing.T) {
	var srvURL string
	var listHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/$value"):
			w.Header().Set("Content-Type", "message/rfc822")
			fmt.Fprint(w, rawMail1)
		case strings.HasSuffix(p, "/messages") && !strings.Contains(p, "/delta"):
			listHit = true // 不应走列表路径
			fmt.Fprint(w, `{"value":[]}`)
		case strings.HasSuffix(p, "/delta"):
			switch r.URL.Query().Get("token") {
			case "first": // 第一轮：一条增量 + 新链接
				fmt.Fprint(w, `{"value":[{"id":"m4","receivedDateTime":"2026-11-17T09:00:00Z"}],`+
					`"@odata.deltaLink":"`+srvURL+`/me/mailFolders/inbox/messages/delta?token=next"}`)
			case "next": // 第二轮：无变更
				fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"`+srvURL+`/me/mailFolders/inbox/messages/delta?token=final"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newTokenServer(t).URL
	c := New(tokens, nil)
	c.SetBase(srvURL)
	connect(t, c)

	round := func(link string) (mailclient.FetchResult, []string) {
		var ids []string
		res, err := c.FetchFolder(context.Background(), "INBOX",
			&store.FolderState{DeltaLink: link}, time.Time{},
			func(k mailclient.MsgKey, _ time.Time, _ []byte) error {
				ids = append(ids, k.GraphID)
				return nil
			})
		if err != nil {
			t.Fatalf("delta round %s: %v", link, err)
		}
		return res, ids
	}

	res, ids := round(srvURL + "/me/mailFolders/inbox/messages/delta?token=first")
	if len(ids) != 1 || ids[0] != "m4" {
		t.Fatalf("第一轮应回调 m4: %v", ids)
	}
	want, _ := time.Parse(time.RFC3339, "2026-11-17T09:00:00Z")
	if !res.LatestSeen.Equal(want) {
		t.Fatalf("LatestSeen = %v, want %v", res.LatestSeen, want)
	}
	if !strings.Contains(res.DeltaLink, "token=next") {
		t.Fatalf("应带回新 deltaLink: %q", res.DeltaLink)
	}

	res, ids = round(res.DeltaLink)
	if len(ids) != 0 {
		t.Fatalf("第二轮无变更不应回调: %v", ids)
	}
	if !strings.Contains(res.DeltaLink, "token=final") {
		t.Fatalf("链接应滚动到 final: %q", res.DeltaLink)
	}
	if listHit {
		t.Fatal("delta 路径不应请求 $filter 列表")
	}
}

// delta 引导：无 deltaLink 的列表路径完成后，用 $deltatoken=latest 拿到初始 deltaLink。
func TestFetchFolderDeltaBootstrap(t *testing.T) {
	var bootQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/$value"):
			fmt.Fprint(w, rawMail1)
		case strings.HasSuffix(p, "/messages/delta"):
			bootQuery = r.URL.RawQuery
			fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://graph/dl-0"}`)
		case strings.HasSuffix(p, "/messages"):
			fmt.Fprint(w, `{"value":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newTokenServer(t).URL
	c := New(tokens, nil)
	c.SetBase(srv.URL)
	connect(t, c)

	res, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-time.Hour),
		func(mailclient.MsgKey, time.Time, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if res.DeltaLink != "https://graph/dl-0" {
		t.Fatalf("应引导出初始 deltaLink: %q", res.DeltaLink)
	}
	if !strings.Contains(bootQuery, "$deltatoken=latest") {
		t.Fatalf("引导请求应带 $deltatoken=latest: %q", bootQuery)
	}
}

// delta 失效（410 Gone）：退回时间窗列表，$filter 从 max(last_seen, since) 开始。
func TestFetchFolderDeltaGoneFallback(t *testing.T) {
	var filterVal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/messages/delta"):
			w.WriteHeader(http.StatusGone) // deltaLink 已过期
		case strings.HasSuffix(p, "/messages"):
			filterVal, _ = url.QueryUnescape(r.URL.RawQuery)
			fmt.Fprint(w, `{"value":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newTokenServer(t).URL
	c := New(tokens, nil)
	c.SetBase(srv.URL)
	connect(t, c)

	seen := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	cursor := &store.FolderState{
		DeltaLink:  srv.URL + "/me/mailFolders/inbox/messages/delta?token=expired",
		LastSeenAt: sql.NullTime{Time: seen, Valid: true},
	}
	since := time.Now().UTC().Add(-7 * 24 * time.Hour).Truncate(time.Second)
	if _, err := c.FetchFolder(context.Background(), "INBOX", cursor, since,
		func(mailclient.MsgKey, time.Time, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// 退回列表时 from 取 max(last_seen, since)——这里 last_seen 更晚
	if !strings.Contains(filterVal, "receivedDateTime ge "+seen.Format(time.RFC3339)) {
		t.Fatalf("退回列表应从 last_seen 游标开始: %s", filterVal)
	}
}

// delta 返回 401（ErrReauth）或 429 时不得回退到 fetchList（避免重复打 API 或掩盖认证失败）。
func TestFetchFolderDeltaNoFallbackOnAuthOr429(t *testing.T) {
	fastRetry(t) // 429 没有 Retry-After：别让测试干等默认的 2s + 4s
	for _, tc := range []struct {
		name    string
		code    int
		wantErr error
	}{
		{"401", http.StatusUnauthorized, mstoken.ErrReauth},
		{"429", http.StatusTooManyRequests, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var listHit bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages/delta") {
					w.WriteHeader(tc.code)
					return
				}
				listHit = true
				fmt.Fprint(w, `{"value":[]}`)
			}))
			defer srv.Close()

			tokens := mstoken.NewTokenCache()
			tokens.Endpoint = newTokenServer(t).URL
			c := New(tokens, nil)
			c.SetBase(srv.URL)
			connect(t, c)

			cursor := &store.FolderState{DeltaLink: srv.URL + "/me/mailFolders/inbox/messages/delta?token=x"}
			_, err := c.FetchFolder(context.Background(), "INBOX", cursor, time.Now().Add(-time.Hour),
				func(mailclient.MsgKey, time.Time, []byte) error { return nil })
			if err == nil {
				t.Fatal("应返回错误")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，得 %v", tc.wantErr, err)
			}
			if listHit {
				t.Fatalf("HTTP %d 不应回退到 fetchList", tc.code)
			}
		})
	}
}

// 取原文失败多半只关乎那一封（列表与取原文之间邮件被删/移走 404、无权访问 403、无法导出
// 422…）：跳过它，其余邮件照常入库，整轮成功。以前任何一封出错都让整轮失败、游标不前进，
// 下一轮又从这封开始——整个账号从此卡死，后面的邮件永远拉不到。
func TestFetchFolderRawFailureSkipped(t *testing.T) {
	const list = `{"value":[` +
		`{"id":"m1","receivedDateTime":"2026-11-17T09:00:00Z"},` +
		`{"id":"m2","receivedDateTime":"2026-11-17T09:01:00Z"},` +
		`{"id":"m3","receivedDateTime":"2026-11-17T09:02:00Z"}]}`
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/$value"):
					if strings.Contains(r.URL.Path, "/m2/") {
						w.WriteHeader(code) // 这一封拉不下来
						return
					}
					fmt.Fprint(w, rawMail1)
				case strings.HasSuffix(r.URL.Path, "/messages"):
					fmt.Fprint(w, list)
				default: // delta 引导：拿不到也不影响列表路径
					w.WriteHeader(http.StatusNotFound)
				}
			})
			connect(t, c)

			var got []string
			res, err := c.FetchFolder(context.Background(), "INBOX", nil,
				time.Now().Add(-24*time.Hour), collect(&got))
			if err != nil {
				t.Fatalf("单封取原文失败不应让整轮失败: %v", err)
			}
			if len(got) != 2 || got[0] != "m1" || got[1] != "m3" {
				t.Fatalf("应跳过 m2、回调 m1/m3: %v", got)
			}
			// 游标照常推进到本批最大时间
			want, _ := time.Parse(time.RFC3339, "2026-11-17T09:02:00Z")
			if !res.LatestSeen.Equal(want) {
				t.Fatalf("LatestSeen = %v，应为 %v", res.LatestSeen, want)
			}
		})
	}
}

// 连续太多封取原文都失败：不再是"个别邮件的问题"（权限、应用注册），中止本轮并报错，
// 不能无限静默跳过。
func TestFetchFolderRawFailureFloodStops(t *testing.T) {
	var list strings.Builder
	list.WriteString(`{"value":[`)
	for i := 1; i <= maxConsecutiveRawFailures+5; i++ {
		if i > 1 {
			list.WriteString(",")
		}
		fmt.Fprintf(&list, `{"id":"m%d","receivedDateTime":"2026-11-17T09:%02d:00Z"}`, i, i)
	}
	list.WriteString(`]}`)
	var valueHits atomic.Int32
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			valueHits.Add(1)
			w.WriteHeader(http.StatusForbidden) // 授权不允许读邮件：封封都失败
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, list.String())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	connect(t, c)

	var got []string
	_, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-24*time.Hour), collect(&got))
	if err == nil || !strings.Contains(err.Error(), "取原文失败") {
		t.Fatalf("连续失败达上限应报错，得 %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不应有任何邮件回调: %v", got)
	}
	if n := valueHits.Load(); n != maxConsecutiveRawFailures {
		t.Fatalf("达到上限后应立即停止（不再请求后面的邮件），实际请求 %d 次", n)
	}
}

// 上限针对的是"连续"失败：中间每成功一封就清零。零星失败的账号不该被封封失败的上限误伤。
func TestFetchFolderRawFailureStreakReset(t *testing.T) {
	var list strings.Builder
	list.WriteString(`{"value":[`)
	n := 0
	for _, group := range []string{"f", "f", "f", "ok", "f", "f", "f", "ok", "f", "f", "f", "ok"} {
		n++
		if n > 1 {
			list.WriteString(",")
		}
		fmt.Fprintf(&list, `{"id":"%s%d","receivedDateTime":"2026-11-17T09:%02d:00Z"}`, group, n, n)
	}
	list.WriteString(`]}`)
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			if strings.Contains(r.URL.Path, "/f") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, rawMail1)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, list.String())
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	connect(t, c)

	var got []string
	_, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-24*time.Hour), collect(&got))
	if err != nil {
		t.Fatalf("零星失败（累计 9 次、连续不超过 3）不应中止本轮: %v", err)
	}
	if len(got) != 3 || got[0] != "ok4" || got[1] != "ok8" || got[2] != "ok12" {
		t.Fatalf("应回调三封成功的: %v", got)
	}
}

// 不是"这一封的问题"的失败必须让整轮失败（交给 worker 退避后重来），不能跳过：
// 5xx、重试耗尽的限流、408。跳过等于把这封没读到的邮件当成读过了，而游标照常前进。
func TestFetchFolderRawFailureFatal(t *testing.T) {
	fastRetry(t)
	const list = `{"value":[{"id":"m1","receivedDateTime":"2026-11-17T09:00:00Z"}]}`
	for _, code := range []int{
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusTooManyRequests, http.StatusRequestTimeout,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var hits atomic.Int32
			c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/$value"):
					hits.Add(1)
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(code)
				case strings.HasSuffix(r.URL.Path, "/messages"):
					fmt.Fprint(w, list)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			connect(t, c)

			var got []string
			_, err := c.FetchFolder(context.Background(), "INBOX", nil,
				time.Now().Add(-24*time.Hour), collect(&got))
			if err == nil || errors.Is(err, mstoken.ErrReauth) {
				t.Fatalf("整轮应以普通错误失败（不是认证失败），得 %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("不应有任何邮件回调: %v", got)
			}
			// 限流类先重试 graphRetries 次再放弃；其余不重试
			wantHits := int32(1)
			if retryableStatus(code) {
				wantHits += graphRetries
			}
			if n := hits.Load(); n != wantHits {
				t.Fatalf("$value 应请求 %d 次，得 %d", wantHits, n)
			}
		})
	}
}

// 等待重试期间 ctx 取消（停机/重载账号）：立即返回，不把取消当成"这封读不到"而跳过去。
func TestFetchFolderRawRetryCtxCancel(t *testing.T) {
	oldCap := retryAfterCap
	retryAfterCap = 30 * time.Second // 服务端要求等 30 秒，照等
	t.Cleanup(func() { retryAfterCap = oldCap })

	const list = `{"value":[{"id":"m1","receivedDateTime":"2026-11-17T09:00:00Z"}]}`
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, list)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	connect(t, c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	var got []string
	_, err := c.FetchFolder(ctx, "INBOX", nil, time.Now().Add(-24*time.Hour), collect(&got))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 ctx 取消错误，得 %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("ctx 取消后应立即返回，实际等了 %v", d)
	}
}

// 限流：按 Retry-After 等待后重试，成功则调用方毫无感知。
func TestGetRetriesOnThrottle(t *testing.T) {
	fastRetry(t)
	var hits atomic.Int32
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 { // 头两次限流
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://graph/dl"}`)
	})
	connect(t, c)

	res, err := c.FetchFolder(context.Background(), "INBOX", deltaCursor(c.base), time.Time{}, noop)
	if err != nil || res.DeltaLink != "https://graph/dl" {
		t.Fatalf("限流后重试应成功: %+v %v", res, err)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("应请求 3 次（2 次 429 + 1 次成功），得 %d", n)
	}
}

// 等待时长确实按 Retry-After 来，且有上限：服务端要求等 3600 秒时只等 retryAfterCap——
// 等满了不如把这一轮交给 worker 退避。
func TestGetHonorsRetryAfter(t *testing.T) {
	oldCap := retryAfterCap
	retryAfterCap = 150 * time.Millisecond
	t.Cleanup(func() { retryAfterCap = oldCap })

	var hits atomic.Int32
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://graph/dl"}`)
	})
	connect(t, c)

	start := time.Now()
	if _, err := c.FetchFolder(context.Background(), "INBOX", deltaCursor(c.base), time.Time{}, noop); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 5*time.Second {
		t.Fatalf("应等待约 retryAfterCap（150ms），实际 %v", d)
	}
}

// deltaCursor 指向 fake 服务器 delta 端点的游标（走 delta 路径）。
func deltaCursor(base string) *store.FolderState {
	return &store.FolderState{DeltaLink: base + "/me/mailFolders/inbox/messages/delta?token=x"}
}

func TestRetryAfter(t *testing.T) {
	oldCap := retryAfterCap
	retryAfterCap = time.Hour
	t.Cleanup(func() { retryAfterCap = oldCap })

	for _, tc := range []struct {
		name    string
		header  string
		attempt int
		want    time.Duration
	}{
		{"无头：基础等待", "", 0, 2 * time.Second},
		{"无头：逐次加倍", "", 1, 4 * time.Second},
		{"秒数", "5", 0, 5 * time.Second},
		{"秒数带空白", " 7 ", 0, 7 * time.Second},
		{"零秒", "0", 0, 0},
		{"负数：退回基础等待", "-3", 0, 2 * time.Second},
		{"无法解析：退回基础等待", "soon", 1, 4 * time.Second},
	} {
		if got := retryAfter(tc.header, tc.attempt); got != tc.want {
			t.Errorf("%s: retryAfter(%q, %d) = %v，应为 %v", tc.name, tc.header, tc.attempt, got, tc.want)
		}
	}
	// HTTP 日期：等到那个时刻为止；已经过去的时刻不等待
	if got := retryAfter(time.Now().UTC().Add(30*time.Second).Format(http.TimeFormat), 0); got < 25*time.Second || got > 30*time.Second {
		t.Errorf("HTTP 日期（未来）应等到该时刻，得 %v", got)
	}
	if got := retryAfter(time.Now().UTC().Add(-time.Minute).Format(http.TimeFormat), 0); got != 0 {
		t.Errorf("HTTP 日期（已过去）不应等待，得 %v", got)
	}
	// 封顶
	retryAfterCap = 10 * time.Millisecond
	if got := retryAfter("3600", 0); got != 10*time.Millisecond {
		t.Errorf("应封顶 retryAfterCap，得 %v", got)
	}
}

// 401 不一定是凭据失效：访问令牌可能在会话途中过期。令牌缓存只保证拿到手时还剩一分钟以上
// 有效期，而一个 Graph 会话要发很多请求（每封原文一次），大邮箱首次同步轻易超过这个时间。
// 先强制刷新再重试，成功就照常收取；刷新时轮换出的 refresh_token 记下来，交给 worker 持久化。
func TestFetchFolder401RenewsToken(t *testing.T) {
	const list = `{"value":[{"id":"m1","receivedDateTime":"2026-11-17T09:00:00Z"}]}`
	var tokenCalls atomic.Int32
	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch tokenCalls.Add(1) {
		case 1: // 连接时拿到的令牌：会话中途过期
			fmt.Fprint(w, `{"access_token":"AT-stale","expires_in":3600}`)
		default:
			if rt := r.PostForm.Get("refresh_token"); rt != "RT" {
				t.Errorf("强制刷新应携带账号的 refresh_token，得 %q", rt)
			}
			fmt.Fprint(w, `{"access_token":"AT-fresh","refresh_token":"RT-rotated","expires_in":3600}`)
		}
	}).URL
	c := New(tokens, nil)
	c.SetBase(newGraphServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer AT-fresh" { // 旧令牌已过期
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			fmt.Fprint(w, rawMail1)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, list)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}).URL)
	connect(t, c) // 拿到的是 AT-stale

	var got []string
	_, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-24*time.Hour), collect(&got))
	if err != nil {
		t.Fatalf("令牌途中过期应刷新后照常收取: %v", err)
	}
	if len(got) != 1 || got[0] != "m1" {
		t.Fatalf("应回调 m1: %v", got)
	}
	if n := tokenCalls.Load(); n != 2 {
		t.Fatalf("令牌端点应被调用 2 次（连接 + 一次强制刷新），得 %d", n)
	}
	// 刷新时轮换出的 refresh_token：worker 取走持久化（取后清空）
	if rt := c.TakeRotatedToken(); rt != "RT-rotated" {
		t.Fatalf("应交出轮换的新 refresh_token，得 %q", rt)
	}
	if rt := c.TakeRotatedToken(); rt != "" {
		t.Fatalf("取走后应清空，得 %q", rt)
	}
	// 下个会话用持久化后的新令牌连接（worker 的做法）：命中缓存，不再打令牌端点；
	// 也不带上个会话的轮换记录
	rotated, err := c.Connect(context.Background(), "", 0, "u@outlook.com", mailclient.Credentials{
		AuthType: "oauth2", ClientID: "cid", RefreshToken: "RT-rotated",
	})
	if err != nil || rotated != "" {
		t.Fatalf("用新令牌连接应命中缓存且不再轮换: %q %v", rotated, err)
	}
	if n := tokenCalls.Load(); n != 2 {
		t.Fatalf("缓存命中不应再打令牌端点，得 %d 次", n)
	}
	if rt := c.TakeRotatedToken(); rt != "" {
		t.Fatalf("新会话不应带上旧值，得 %q", rt)
	}
}

// delta 引导必须排在时间窗列表**之前**：令牌对应"此刻"的状态，列表可能跑很久（每封原文一次
// 请求），期间到达的邮件不在列表里。引导放在列表之后的话，这些邮件两边都不覆盖，永久漏掉。
func TestFetchFolderDeltaBootstrapBeforeList(t *testing.T) {
	var order []string
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/delta"):
			order = append(order, "delta")
			fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://graph/dl"}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			order = append(order, "list")
			fmt.Fprint(w, `{"value":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	connect(t, c)

	res, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-24*time.Hour), noop)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "delta" || order[1] != "list" {
		t.Fatalf("应先引导 delta 再列表，实际顺序 %v", order)
	}
	if res.DeltaLink != "https://graph/dl" {
		t.Fatalf("列表走完后应交回引导出的 delta 链接: %q", res.DeltaLink)
	}
}

// 空 value 也可能带 nextLink（delta 常见）：必须跟下去。当成走完的话拿不到 deltaLink，
// 也漏掉后面几页的邮件。
func TestFetchFolderEmptyPageWithNextLink(t *testing.T) {
	var srvURL string // 处理器里要用服务器地址：客户端建好后回填（请求发生时早已就绪）
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			fmt.Fprint(w, rawMail1)
		case strings.HasSuffix(r.URL.Path, "/delta"):
			switch r.URL.Query().Get("token") {
			case "first": // 空页 + 下一页链接
				fmt.Fprintf(w, `{"value":[],"@odata.nextLink":"%s/me/mailFolders/inbox/messages/delta?token=second"}`, srvURL)
			case "second":
				fmt.Fprint(w, `{"value":[{"id":"m9","receivedDateTime":"2026-11-17T09:00:00Z"}],`+
					`"@odata.deltaLink":"https://graph/dl-final"}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srvURL = c.base
	connect(t, c)

	var got []string
	res, err := c.FetchFolder(context.Background(), "INBOX",
		&store.FolderState{DeltaLink: c.base + "/me/mailFolders/inbox/messages/delta?token=first"},
		time.Time{}, collect(&got))
	if err != nil || len(got) != 1 || got[0] != "m9" {
		t.Fatalf("应跟到第二页并回调 m9: %v %v", got, err)
	}
	if res.DeltaLink != "https://graph/dl-final" {
		t.Fatalf("应带回末页的 deltaLink: %q", res.DeltaLink)
	}
}

// delta 变更流超过单轮分页上限：把没走完的 nextLink 当游标交回，下一轮从它接着走（不报错——
// 报错的话这一轮作废、下一轮又从头开始，长流永远走不完）。
func TestFetchFolderDeltaPageLimitResumes(t *testing.T) {
	var srvURL string
	var pages atomic.Int32
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := pages.Add(1)
		if r.URL.Query().Get("token") == fmt.Sprint(maxPages*2) { // 第 201 页才是终点
			fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://graph/dl-end"}`)
			return
		}
		fmt.Fprintf(w, `{"value":[],"@odata.nextLink":"%s/me/mailFolders/inbox/messages/delta?token=%d"}`, srvURL, n)
	})
	srvURL = c.base
	connect(t, c)

	link := srvURL + "/me/mailFolders/inbox/messages/delta?token=0"
	for round := 1; round <= 2; round++ {
		res, err := c.FetchFolder(context.Background(), "INBOX", &store.FolderState{DeltaLink: link}, time.Time{}, noop)
		if err != nil {
			t.Fatalf("第 %d 轮达到分页上限不应报错: %v", round, err)
		}
		if got := pages.Load(); got != int32(maxPages*round) {
			t.Fatalf("第 %d 轮后累计应走 %d 页，得 %d", round, maxPages*round, got)
		}
		if want := fmt.Sprintf("%s/me/mailFolders/inbox/messages/delta?token=%d", srvURL, maxPages*round); res.DeltaLink != want {
			t.Fatalf("第 %d 轮应把未走完的 nextLink 当游标交回，得 %q", round, res.DeltaLink)
		}
		link = res.DeltaLink
	}
	// 第三轮走完剩余部分，拿到真正的 deltaLink
	res, err := c.FetchFolder(context.Background(), "INBOX", &store.FolderState{DeltaLink: link}, time.Time{}, noop)
	if err != nil || res.DeltaLink != "https://graph/dl-end" || pages.Load() != int32(maxPages*2+1) {
		t.Fatalf("续走应走完并拿到 deltaLink: %+v %v pages=%d", res, err, pages.Load())
	}
}

// 时间窗列表达到单轮分页上限：本轮算成功（已入库的保留），More 置位、不交回 delta 链接
// （它只覆盖"本轮开始之后"，列表里还有更早的邮件没走到），LatestSeen 停在已走过的最后一封。
func TestFetchFolderListPageLimit(t *testing.T) {
	var srvURL string
	var pages atomic.Int32
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			fmt.Fprint(w, rawMail1)
		case strings.HasSuffix(r.URL.Path, "/delta"):
			fmt.Fprint(w, `{"value":[],"@odata.deltaLink":"https://graph/dl-boot"}`)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			// 每页 1 封，时间递增（每封晚一分钟）；永远有下一页
			n := pages.Add(1)
			fmt.Fprintf(w, `{"value":[{"id":"m%d","receivedDateTime":"2026-11-17T%02d:%02d:00Z"}],`+
				`"@odata.nextLink":"%s/me/mailFolders/inbox/messages?skip=%d"}`, n, n/60, n%60, srvURL, n)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srvURL = c.base
	connect(t, c)

	var got []string
	res, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-24*time.Hour), collect(&got))
	if err != nil {
		t.Fatalf("达到分页上限不应报错: %v", err)
	}
	if n := pages.Load(); n != maxPages || len(got) != maxPages {
		t.Fatalf("应走满 %d 页并回调同样多的邮件，得 pages=%d msgs=%d", maxPages, n, len(got))
	}
	if !res.More {
		t.Fatal("列表没走完应置 More")
	}
	if res.DeltaLink != "" {
		t.Fatalf("列表没走完时不应交回 delta 链接: %q", res.DeltaLink)
	}
	// 第 100 封 = 00:00 之后第 100 分钟
	want, _ := time.Parse(time.RFC3339, "2026-11-17T01:40:00Z")
	if !res.LatestSeen.Equal(want) {
		t.Fatalf("LatestSeen = %v，应为 %v", res.LatestSeen, want)
	}
}

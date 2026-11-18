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

func TestFetchFolder401IsReauth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // 一律 401
	}))
	defer srv.Close()

	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = newTokenServer(t).URL
	c := New(tokens, nil)
	c.SetBase(srv.URL)
	connect(t, c)

	_, err := c.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-time.Hour),
		func(mailclient.MsgKey, time.Time, []byte) error { return nil })
	if !errors.Is(err, mstoken.ErrReauth) {
		t.Fatalf("401 应映射 ErrReauth，得 %v", err)
	}
}

func TestGraphFolderMapping(t *testing.T) {
	if g := graphFolder("INBOX"); g != "inbox" {
		t.Fatalf("INBOX → %q", g)
	}
	if g := graphFolder("Archive"); g != "archive" {
		t.Fatalf("Archive → %q", g)
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

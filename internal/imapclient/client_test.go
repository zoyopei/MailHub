package imapclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

const defaultGreeting = "* OK [CAPABILITY IMAP4rev1 IDLE SASL-IR AUTH=XOAUTH2] fake ready"

// fakeIMAP 是 net.Pipe（或真实 TCP，见 newFakeIMAPListener）上的假 IMAP 服务器：按真实线协议
// 回应 CAPABILITY / LOGIN / AUTHENTICATE / EXAMINE / UID SEARCH / UID FETCH / IDLE / LOGOUT /
// STARTTLS，并记录收到的命令（用来断言只读性：EXAMINE 而非 SELECT、BODY.PEEK[] 而非 BODY[]；
// 以及"升级失败时没有发出登录命令"）。
// serve 跑在独立 goroutine 上，所以可变字段只能经方法访问。
type fakeIMAP struct {
	conn     net.Conn
	rd       *bufio.Reader
	greeting string

	mu         sync.Mutex
	cmds       []string          // 收到的命令行（去掉 tag）
	mailbox    map[uint32][]byte // UID → 原文
	silent     map[string]bool   // 读了不回的命令（模拟服务器卡死）
	drop       map[string]bool   // 读完直接断开的命令（模拟连接被断开）
	password   string            // LOGIN 接受的密码；空 = 一律拒绝
	tokenOK    bool              // 是否接受 XOAUTH2
	uidv       uint32            // EXAMINE 回的 UIDVALIDITY
	uidNext    uint32            // EXAMINE 回的 UIDNEXT
	old        map[uint32]bool   // 早于时间窗的邮件（mailbox 里同时有它）：带 SINCE 的搜索不返回
	failSearch map[int]bool      // 第几次 UID SEARCH（从 1 起）回 NO
	searchSeq  int               // 已收到的 UID SEARCH 次数
	doneEx     uint32            // 非 0：回 DONE 的 tagged OK 之前先注入一条 "* N EXISTS"
	idle       bool              // 正在 IDLE（等 DONE）
	tag        string            // 当前命令的 tag
	tlsCfg     *tls.Config       // 非 nil：支持 STARTTLS（用它做服务端握手）
	stallTLS   bool              // STARTTLS 回 OK 后既不握手也不回数据（模拟升级途中服务器卡死）
}

func newFakeIMAP(t *testing.T, greeting string) (*fakeIMAP, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	s := newFakeIMAPOn(server, greeting)
	go s.serve()
	t.Cleanup(func() { server.Close(); client.Close() })
	return s, client
}

// newFakeIMAPOn 在给定连接上构造假服务器（不启动 serve：调用方可能要先包一层 TLS）。
func newFakeIMAPOn(conn net.Conn, greeting string) *fakeIMAP {
	return &fakeIMAP{
		conn: conn, rd: bufio.NewReader(conn), greeting: greeting, uidv: 7, uidNext: 100,
		mailbox: map[uint32][]byte{}, silent: map[string]bool{}, drop: map[string]bool{},
		old: map[uint32]bool{}, failSearch: map[int]bool{},
		tlsCfg: nil,
	}
}

func (s *fakeIMAP) line(line string) { _, _ = s.conn.Write([]byte(line + "\r\n")) }

func (s *fakeIMAP) ok() { s.line(s.tag + " OK completed") }

func (s *fakeIMAP) serve() {
	defer s.conn.Close()
	s.line(s.greeting)
	for {
		line, err := s.rd.ReadString('\n')
		if err != nil {
			return // 客户端断开
		}
		line = strings.TrimRight(line, "\r\n")
		s.mu.Lock()
		idle := s.idle
		s.mu.Unlock()
		if idle {
			if line == "DONE" {
				s.setIdle(false)
				if n := s.doneExistsOf(); n > 0 {
					s.line(fmt.Sprintf("* %d EXISTS", n)) // 新邮件恰好在 IDLE 收尾期间到达
				}
				if !s.isSilent("DONE") {
					s.ok()
				}
			}
			continue
		}
		tag, cmd, _ := strings.Cut(line, " ")
		s.mu.Lock()
		s.tag = tag
		s.cmds = append(s.cmds, cmd)
		s.mu.Unlock()
		name := cmdName(cmd)
		if s.isDrop(name) {
			return
		}
		if s.isSilent(name) {
			continue
		}
		s.respond(tag, cmd)
	}
}

// cmdName 取用于匹配 silent/drop 的命令名：LOGIN / EXAMINE / UID SEARCH / UID FETCH …
func cmdName(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) > 1 && f[0] == "UID" {
		return f[0] + " " + f[1]
	}
	return f[0]
}

func (s *fakeIMAP) respond(tag, cmd string) {
	switch cmdName(cmd) {
	case "CAPABILITY":
		s.line("* CAPABILITY IMAP4rev1 IDLE SASL-IR")
		s.ok()
	case "LOGIN":
		if s.passwordOf() != "" && strings.HasSuffix(cmd, " \""+s.passwordOf()+"\"") {
			s.ok()
		} else {
			s.line(tag + " NO [AUTHENTICATIONFAILED] Authentication failed.")
		}
	case "AUTHENTICATE":
		if s.tokenOKOf() {
			s.ok()
			return
		}
		// 真实服务器的拒绝流程：continuation 带 base64 JSON 错误 → 客户端回空行 → tagged NO
		s.line("+ " + base64.StdEncoding.EncodeToString([]byte(`{"status":"401","schemes":"Bearer"}`)))
		if _, err := s.rd.ReadString('\n'); err != nil {
			return
		}
		s.line(tag + " NO AUTHENTICATE failed.")
	case "EXAMINE":
		mbox := s.snapshot()
		s.line(fmt.Sprintf("* %d EXISTS", len(mbox)))
		s.line("* 0 RECENT")
		s.line(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`)
		s.line("* OK [PERMANENTFLAGS ()] Read-only mailbox.")
		s.line(fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid", s.uidvOf()))
		s.line(fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID", s.uidNextOf()))
		s.ok()
	case "STARTTLS":
		cfg := s.tlsCfgOf()
		if cfg == nil {
			s.line(tag + " NO STARTTLS not supported")
			return
		}
		s.ok()
		if s.stallTLSOf() {
			// 读掉客户端发来的全部数据但什么都不回（TLS 握手永远等不到服务端的首字节）
			_, _ = io.Copy(io.Discard, s.rd)
			return
		}
		// 升级：此后只从 TLS 层读命令（客户端若没升级就发 LOGIN，会被 TLS 记录层当成乱码丢弃）。
		// "升级失败时没发 LOGIN"的断言在失败分支上成立：服务器回 NO 或握手失败时连接仍是明文，
		// 之后的每一条命令都会被记进 cmds。
		tlsConn := tls.Server(s.conn, cfg)
		s.conn, s.rd = tlsConn, bufio.NewReader(tlsConn)
		if err := tlsConn.Handshake(); err != nil {
			return // 客户端拒绝了证书（握手失败）
		}
	case "UID SEARCH":
		// "UID n:m" 建模下界（m 为最大 UID 或 *）。带 SINCE（时间窗）的搜索不返回 old 里的邮件
		// （早于窗口），不带 SINCE 的（如 UID 1:*，取现有最大 UID）返回全部。
		//
		// "n:*" 按真实语义（RFC 3501 的 UID 一节）：n 比文件夹里任何 UID 都大时，结果**仍然包含
		// 最后一封**（UID 最大的那封）。真实服务器都是这么实现的，早先这里只按"≥ n"建模，
		// 于是"没有新邮件时增量搜索返回空"这种在真实服务器上不成立的假设，测试全都能通过。
		f := strings.Fields(cmd)
		if _, fail := s.nextSearch(); fail {
			s.line(tag + " NO SEARCH failed")
			return
		}
		minUID := uint32(1)
		openEnded := false // "n:*"
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "UID" {
				lo, hi, _ := strings.Cut(f[i+1], ":")
				minUID = parseNum(lo)
				openEnded = hi == "*"
			}
		}
		mbox, old := s.snapshot(), s.oldOf()
		var maxUID uint32
		for uid := range mbox {
			if uid > maxUID {
				maxUID = uid
			}
		}
		uids := make([]uint32, 0, len(mbox))
		for uid := range mbox {
			match := uid >= minUID || (openEnded && minUID > maxUID && uid == maxUID)
			if match && !(slices.Contains(f, "SINCE") && old[uid]) {
				uids = append(uids, uid)
			}
		}
		slices.Sort(uids)
		line := "* SEARCH"
		for _, uid := range uids {
			line += fmt.Sprintf(" %d", uid)
		}
		s.line(line)
		s.ok()
	case "UID FETCH":
		f := strings.Fields(cmd)
		mbox := s.snapshot()
		for _, uid := range expandSet(f[2]) {
			raw, ok := mbox[uid]
			if !ok {
				continue
			}
			_, _ = fmt.Fprintf(s.conn,
				"* %d FETCH (UID %d INTERNALDATE \"23-Nov-2026 10:00:00 +0000\" BODY[] {%d}\r\n",
				uid, uid, len(raw))
			_, _ = s.conn.Write(raw)
			s.line(")")
		}
		s.ok()
	case "IDLE":
		s.line("+ idling")
		s.setIdle(true) // 客户端读完 "+ idling" 之后才置位：waitIdle 据此注入信号
	case "LOGOUT":
		s.line("* BYE IMAP4rev1 Server logging out")
		s.ok()
		s.conn.Close()
	default:
		s.line(tag + " BAD unsupported command")
	}
}

func parseNum(s string) uint32 { n, _ := strconv.ParseUint(s, 10, 32); return uint32(n) }

// expandSet 展开序号集合（"1,3:5"）。
func expandSet(set string) []uint32 {
	var out []uint32
	for _, part := range strings.Split(set, ",") {
		lo, hi, ok := strings.Cut(part, ":")
		if !ok {
			hi = lo
		}
		for n := parseNum(lo); n > 0 && n <= parseNum(hi); n++ {
			out = append(out, n)
		}
	}
	return out
}

// ---- 测试侧的访问方法 ----

func (s *fakeIMAP) setSilent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent[name] = true
}

func (s *fakeIMAP) setDrop(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop[name] = true
}

func (s *fakeIMAP) isSilent(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.silent[name]
}

func (s *fakeIMAP) isDrop(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drop[name]
}

func (s *fakeIMAP) setIdle(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idle = v
}

func (s *fakeIMAP) setUIDValidity(v uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uidv = v
}

func (s *fakeIMAP) uidvOf() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uidv
}

func (s *fakeIMAP) setUIDNext(v uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uidNext = v
}

func (s *fakeIMAP) uidNextOf() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uidNext
}

// setStartTLS 启用 STARTTLS（须在客户端发出该命令前调用）。
func (s *fakeIMAP) setStartTLS(cfg *tls.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tlsCfg = cfg
}

func (s *fakeIMAP) tlsCfgOf() *tls.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tlsCfg
}

func (s *fakeIMAP) setStallTLS(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stallTLS = v
}

func (s *fakeIMAP) stallTLSOf() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stallTLS
}

// connOf 服务器侧当前连接（升级后应是 *tls.Conn）。
func (s *fakeIMAP) connOf() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn
}

func (s *fakeIMAP) setDoneExists(n uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doneEx = n
}

func (s *fakeIMAP) doneExistsOf() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneEx
}

func (s *fakeIMAP) setPassword(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.password = p
}

func (s *fakeIMAP) passwordOf() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.password
}

func (s *fakeIMAP) setTokenOK(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenOK = v
}

func (s *fakeIMAP) tokenOKOf() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenOK
}

func (s *fakeIMAP) addMail(uid uint32, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailbox[uid] = raw
}

// addOldMail 添加一封早于时间窗的邮件：首次同步的 SINCE 搜索带不出它（不下载正文），
// 但它计入"文件夹里现有邮件的最大 UID"。
func (s *fakeIMAP) addOldMail(uid uint32, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mailbox[uid] = raw
	s.old[uid] = true
}

func (s *fakeIMAP) oldOf() map[uint32]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[uint32]bool, len(s.old))
	for uid, v := range s.old {
		m[uid] = v
	}
	return m
}

// failSearchAt 让第 n 次 UID SEARCH（从 1 起）回 NO。
func (s *fakeIMAP) failSearchAt(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSearch[n] = true
}

// nextSearch 记一次 UID SEARCH，返回它是第几次、是否该失败。
func (s *fakeIMAP) nextSearch() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchSeq++
	return s.searchSeq, s.failSearch[s.searchSeq]
}

// searchCmds 返回迄今收到的 UID SEARCH 命令（按先后）。
func (s *fakeIMAP) searchCmds() []string {
	var out []string
	for _, cmd := range s.commands() {
		if cmdName(cmd) == "UID SEARCH" {
			out = append(out, cmd)
		}
	}
	return out
}

// snapshot 取邮箱当前内容的副本（UID 即序号：测试按 1..n 连续添加）。
func (s *fakeIMAP) snapshot() map[uint32][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[uint32][]byte, len(s.mailbox))
	for uid, raw := range s.mailbox {
		m[uid] = raw
	}
	return m
}

// commands 返回服务器迄今收到的命令行（去掉 tag）。
func (s *fakeIMAP) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cmds)
}

// command 返回第一条包含 sub 的命令，没有则让测试失败。
func (s *fakeIMAP) command(t *testing.T, sub string) string {
	t.Helper()
	for _, cmd := range s.commands() {
		if strings.Contains(cmd, sub) {
			return cmd
		}
	}
	t.Fatalf("服务器未收到包含 %q 的命令，实际收到：%q", sub, s.commands())
	return ""
}

// waitIdle 等服务器进入 IDLE（已回 "+ idling"）。
func (s *fakeIMAP) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		idle := s.idle
		s.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("服务器 5s 内未进入 IDLE")
}

// exists 注入一条 untagged EXISTS（新邮件到达）。只在 IDLE 等待期间或两条命令之间调用——
// 那时 serve 阻塞在读，不会与本方法并发写。
func (s *fakeIMAP) exists(n uint32) {
	_, _ = s.conn.Write([]byte(fmt.Sprintf("* %d EXISTS\r\n", n)))
}

// close 让服务器直接断开连接。
func (s *fakeIMAP) close() { _ = s.conn.Close() }

// ---- 客户端侧辅助 ----

// fastTimeouts 把三个超时缩短到测试可接受的量级（测试结束恢复）。
func fastTimeouts(t *testing.T) {
	t.Helper()
	oldStall, oldIdle, oldLogout := stallTimeout, idleStopTimeout, logoutTimeout
	stallTimeout, idleStopTimeout, logoutTimeout = 150*time.Millisecond, 150*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { stallTimeout, idleStopTimeout, logoutTimeout = oldStall, oldIdle, oldLogout })
}

func loginPassword(conn net.Conn, password string) (*goClient, error) {
	g := &goClient{tokens: mstoken.NewTokenCache()}
	_, err := g.login(context.Background(), conn, "u@x.com",
		mailclient.Credentials{AuthType: "password", Password: password})
	return g, err
}

// newTestClient 用正确的密码把客户端登录到假服务器。
func newTestClient(t *testing.T, s *fakeIMAP, conn net.Conn) *goClient {
	t.Helper()
	s.setPassword("secret")
	g, err := loginPassword(conn, "secret")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return g
}

// mail 生成第 n 封测试邮件（CRLF 行尾，与线上一致）。
func mail(n uint32) []byte {
	return []byte(fmt.Sprintf("From: a@b.c\r\nTo: x@y.z\r\nSubject: mail %d\r\nMessage-ID: <%d@b.c>\r\n\r\nbody %d\r\n", n, n, n))
}

type fetched struct {
	key    mailclient.MsgKey
	seenAt time.Time
	raw    string
}

func fetchAll(t *testing.T, g *goClient, cursor *store.FolderState) []fetched {
	t.Helper()
	var out []fetched
	_, err := g.FetchFolder(context.Background(), "INBOX", cursor, time.Now().Add(-24*time.Hour),
		func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error {
			out = append(out, fetched{key, seenAt, string(raw)})
			return nil
		})
	if err != nil {
		t.Fatalf("FetchFolder: %v", err)
	}
	return out
}

// fetchResult 只取 FetchFolder 的游标产出（不关心拉到的邮件）。
func fetchResult(t *testing.T, g *goClient, cursor *store.FolderState) mailclient.FetchResult {
	t.Helper()
	res, err := g.FetchFolder(context.Background(), "INBOX", cursor, time.Now().Add(-24*time.Hour),
		func(mailclient.MsgKey, time.Time, []byte) error { return nil })
	if err != nil {
		t.Fatalf("FetchFolder: %v", err)
	}
	return res
}

type idleResult struct {
	notified bool
	err      error
}

// startIdle 在后台发起 IDLE（测试要和服务器注入的信号交错）。
func startIdle(g *goClient, ctx context.Context, timeout time.Duration) <-chan idleResult {
	ch := make(chan idleResult, 1)
	go func() {
		notified, err := g.Idle(ctx, timeout)
		ch <- idleResult{notified, err}
	}()
	return ch
}

// waitIdleResult 等 Idle 返回。真正的上限由各超时参数决定，这里只防测试永久挂起。
func waitIdleResult(t *testing.T, ch <-chan idleResult) idleResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Idle 5s 内未返回（疑似卡死）")
		return idleResult{}
	}
}

func assertReturnedFast(t *testing.T, start time.Time) {
	t.Helper()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("操作 %s 才返回，疑似卡死", d)
	}
}

// call 在 goroutine 里执行"保护失效就会永久阻塞"的调用：那样测试在几秒内失败，
// 而不是卡到 go test 超时。fn 里不能调 t.Fatal（必须在测试 goroutine 上），结果写进闭包变量。
func call(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("调用 5s 内未返回（疑似永久阻塞）")
	}
}

// ---------- 登录 ----------

func TestLoginPasswordOK(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	if !g.SupportsIdle() { // 登录后能力作废，go-imap 自动补一次 CAPABILITY
		t.Fatal("服务器通告了 IDLE，SupportsIdle 应为 true")
	}
	if err := g.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	want := []string{`LOGIN "u@x.com" "secret"`, "CAPABILITY", "LOGOUT"}
	if got := s.commands(); !slices.Equal(got, want) {
		t.Fatalf("命令序列 = %q，期望 %q", got, want)
	}
}

func TestLoginRejectedIsAuthFailure(t *testing.T) {
	_, conn := newFakeIMAP(t, defaultGreeting) // 未设密码：一律拒绝
	_, err := loginPassword(conn, "wrong")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("密码被拒应是 ErrAuthFailed，得 %v", err)
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("错误应带服务器原文，得 %v", err)
	}
}

// 服务器不响应**不是**认证失败：以前任何登录错误都包成 ErrAuthFailed，一次网络抖动
// 就把账号停在 auth_failed（重启也不恢复），必须人工重新触发。
func TestLoginSilentServerIsNotAuthFailure(t *testing.T) {
	fastTimeouts(t)
	s, conn := newFakeIMAP(t, defaultGreeting)
	s.setPassword("secret")
	s.setSilent("LOGIN")
	var err error
	call(t, func() { _, err = loginPassword(conn, "secret") })
	if err == nil || errors.Is(err, ErrAuthFailed) {
		t.Fatalf("服务器无响应应是普通错误（走退避重连），得 %v", err)
	}
	if !strings.Contains(err.Error(), "无响应") {
		t.Fatalf("错误应说明服务器无响应，得 %v", err)
	}
}

func TestLoginCtxCancel(t *testing.T) {
	fastTimeouts(t)
	stallTimeout = time.Second // 只可能是 ctx 取消让它提前返回
	s, conn := newFakeIMAP(t, defaultGreeting)
	s.setPassword("secret")
	s.setSilent("LOGIN")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	var err error
	call(t, func() { _, err = loginPasswordCtx(ctx, conn, "secret") })
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("ctx 取消后 %s 才返回（看门狗没有断开连接）", d)
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrAuthFailed) {
		t.Fatalf("ctx 取消应返回 context.Canceled，得 %v", err)
	}
}

func loginPasswordCtx(ctx context.Context, conn net.Conn, password string) (*goClient, error) {
	g := &goClient{tokens: mstoken.NewTokenCache()}
	_, err := g.login(ctx, conn, "u@x.com", mailclient.Credentials{AuthType: "password", Password: password})
	return g, err
}

func TestLoginByeGreetingIsNotAuthFailure(t *testing.T) {
	_, conn := newFakeIMAP(t, "* BYE Server shutting down") // 限流/维护：服务器一上来就拒绝
	_, err := loginPassword(conn, "secret")
	if err == nil || errors.Is(err, ErrAuthFailed) {
		t.Fatalf("* BYE 不是凭据问题，期望普通错误，得 %v", err)
	}
	if !strings.Contains(err.Error(), "BYE") {
		t.Fatalf("错误应带服务器 BYE 原文，得 %v", err)
	}
}

func TestLoginDroppedIsNotAuthFailure(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	s.setPassword("secret")
	s.setDrop("LOGIN") // 读完 LOGIN 就断开，不回 tagged 响应
	_, err := loginPassword(conn, "secret")
	if err == nil || errors.Is(err, ErrAuthFailed) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("认证途中断连应是普通错误（unexpected EOF），得 %v", err)
	}
}

func newTokenServer(t *testing.T) *mstoken.TokenCache {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "RT-OLD" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"AT-OK","refresh_token":"RT-NEW","expires_in":3600}`))
	}))
	t.Cleanup(ts.Close)
	tokens := mstoken.NewTokenCache()
	tokens.Endpoint = ts.URL
	tokens.HTTP = ts.Client()
	return tokens
}

func TestLoginXOAUTH2(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	s.setTokenOK(true)
	g := &goClient{tokens: newTokenServer(t)}
	rotated, err := g.login(context.Background(), conn, "u@x.com",
		mailclient.Credentials{AuthType: "oauth2", ClientID: "cid", RefreshToken: "RT-OLD"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if rotated != "RT-NEW" {
		t.Fatalf("应返回端点轮换后的 refresh_token，得 %q", rotated)
	}
	// SASL-IR：初始响应 base64("user=…\x01auth=Bearer <access_token>\x01\x01")
	f := strings.Fields(s.command(t, "AUTHENTICATE XOAUTH2"))
	if len(f) != 3 {
		t.Fatalf("AUTHENTICATE 命令 = %q", f)
	}
	ir, err := base64.StdEncoding.DecodeString(f[2])
	if err != nil {
		t.Fatalf("初始响应不是合法 base64: %v", err)
	}
	if want := "user=u@x.com\x01auth=Bearer AT-OK\x01\x01"; string(ir) != want {
		t.Fatalf("初始响应 = %q，期望 %q", ir, want)
	}
}

func TestLoginXOAUTH2RejectedIsAuthFailure(t *testing.T) {
	_, conn := newFakeIMAP(t, defaultGreeting) // tokenOK=false：continuation 报错后回 NO
	g := &goClient{tokens: newTokenServer(t)}
	_, err := g.login(context.Background(), conn, "u@x.com",
		mailclient.Credentials{AuthType: "oauth2", ClientID: "cid", RefreshToken: "RT-OLD"})
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("XOAUTH2 被拒应是 ErrAuthFailed，得 %v", err)
	}
	if !strings.Contains(err.Error(), "AUTHENTICATE failed") {
		t.Fatalf("错误应带服务器原文，得 %v", err)
	}
}

func TestLoginRefreshTokenExpiredIsReauth(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	tokens := newTokenServer(t)
	g := &goClient{tokens: tokens}
	_, err := g.login(context.Background(), conn, "u@x.com",
		mailclient.Credentials{AuthType: "oauth2", ClientID: "cid", RefreshToken: "RT-EXPIRED"})
	if !errors.Is(err, mstoken.ErrReauth) {
		t.Fatalf("refresh_token 失效应是 mstoken.ErrReauth，得 %v", err)
	}
	if cmds := s.commands(); len(cmds) != 0 {
		t.Fatalf("取不到令牌就不该发认证命令，实际发了 %q", cmds)
	}
}

func TestIsAuthRejection(t *testing.T) {
	no := &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Authentication failed."}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"tagged NO", no, true},
		{"tagged BAD（服务器不支持该认证机制）", &imap.Error{Type: imap.StatusResponseTypeBad}, true},
		{"包装过的 NO", fmt.Errorf("login: %w", no), true},
		{"* BYE（限流/维护，不是凭据问题）", &imap.Error{Type: imap.StatusResponseTypeBye}, false},
		{"连接断开", io.ErrUnexpectedEOF, false},
		{"取消", context.Canceled, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isAuthRejection(c.err); got != c.want {
			t.Errorf("%s: isAuthRejection = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// ---------- 拉取 ----------

func TestFetchFolderIncremental(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.addMail(1, mail(1))
	s.addMail(2, mail(2))

	// 首次（无游标）：按时间窗搜索
	got := fetchAll(t, g, nil)
	if len(got) != 2 || got[0].key != (mailclient.MsgKey{UIDValidity: 7, UID: 1}) ||
		got[1].key.UID != 2 || got[0].raw != string(mail(1)) {
		t.Fatalf("首次拉取结果不符：%+v", got)
	}
	if got[0].seenAt.UTC().Format(time.RFC3339) != "2026-11-23T10:00:00Z" {
		t.Fatalf("seenAt 应为 INTERNALDATE，得 %v", got[0].seenAt)
	}
	s.command(t, "UID SEARCH SINCE ")

	// 增量：游标有效 → 只找 UID > last_uid
	s.addMail(3, mail(3))
	got = fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: 2})
	if len(got) != 1 || got[0].key.UID != 3 {
		t.Fatalf("增量拉取应只有 UID 3，得 %+v", got)
	}
	s.command(t, "UID SEARCH UID 3:*")

	// 无新邮件
	if got = fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: 3}); len(got) != 0 {
		t.Fatalf("无新邮件时不应有结果，得 %+v", got)
	}

	// UIDVALIDITY 变化：游标作废，退回时间窗，按新 UIDVALIDITY 入库
	s.setUIDValidity(8)
	got = fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: 3})
	if len(got) != 3 || got[0].key.UIDValidity != 8 {
		t.Fatalf("UIDVALIDITY 变化后应按时间窗全量重拉，得 %+v", got)
	}
	s.command(t, "UID SEARCH SINCE ")

	// 只读性：EXAMINE 而非 SELECT，BODY.PEEK[] 而非 BODY[]
	cmds := s.commands()
	s.command(t, "EXAMINE INBOX")
	fetch := s.command(t, "UID FETCH ")
	for _, want := range []string{"UID", "INTERNALDATE", "BODY.PEEK[]"} {
		if !strings.Contains(fetch, want) {
			t.Errorf("FETCH 命令应包含 %q：%q", want, fetch)
		}
	}
	for _, cmd := range cmds {
		if strings.HasPrefix(cmd, "SELECT ") || strings.Contains(cmd, "BODY[]") {
			t.Fatalf("违反只读原则的命令：%q", cmd)
		}
	}
}

func TestFetchFolderBatches(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	for n := uint32(1); n <= 25; n++ { // 超过一批（20 封）
		s.addMail(n, mail(n))
	}
	got := fetchAll(t, g, nil)
	if len(got) != 25 {
		t.Fatalf("应拉到 25 封，得 %d", len(got))
	}
	for i, m := range got {
		if m.key.UID != uint32(i+1) || m.raw != string(mail(uint32(i+1))) {
			t.Fatalf("第 %d 封不符：%+v", i, m)
		}
	}
	n := 0
	for _, cmd := range s.commands() {
		if cmdName(cmd) == "UID FETCH" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("25 封应分 2 批拉取，得 %d 批", n)
	}
}

func TestFetchFolderEmpty(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	if got := fetchAll(t, g, nil); len(got) != 0 {
		t.Fatalf("空邮箱不应有结果，得 %+v", got)
	}
	for _, cmd := range s.commands() {
		if cmdName(cmd) == "UID FETCH" {
			t.Fatalf("空结果不应发 FETCH：%q", cmd)
		}
	}
}

// FetchFolder 带回游标产出（worker 用它推进 folder_states）：
//   - 首次同步遇到空结果：再搜一次 UID 1:*，取文件夹里现有邮件的最大 UID（空文件夹为 0）；
//   - 增量路径空结果：不推进（LastUID=0），也不多搜；
//   - 有结果：取最大 UID（不依赖服务器返回顺序）。
func TestFetchFolderCursor(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)

	// 空文件夹的首次同步：游标 (7, 0)——时间窗搜索一次、取最大 UID 一次，不下载任何正文
	res := fetchResult(t, g, nil)
	if res.UIDValidity != 7 || res.LastUID != 0 {
		t.Fatalf("空文件夹首次同步应产出 (7, 0)，得 (%d, %d)", res.UIDValidity, res.LastUID)
	}
	if got := s.searchCmds(); len(got) != 2 ||
		!strings.HasPrefix(got[0], "UID SEARCH SINCE") || got[1] != "UID SEARCH UID 1:*" {
		t.Fatalf("首次同步空结果应搜时间窗、再搜 UID 1:*，实际 %q", got)
	}
	// 增量路径空结果：不推进，也不多搜（下一轮照样搜 1:*）
	res = fetchResult(t, g, &store.FolderState{UIDValidity: 7, LastUID: 0})
	if res.UIDValidity != 7 || res.LastUID != 0 {
		t.Fatalf("增量空结果不应推进游标，得 (%d, %d)", res.UIDValidity, res.LastUID)
	}
	if got := s.searchCmds(); len(got) != 3 || got[2] != "UID SEARCH UID 1:*" {
		t.Fatalf("增量空结果只应搜一次 UID 1:*，实际 %q", got)
	}

	// 有结果：取最大 UID（服务器乱序返回也一样）
	s.addMail(17, mail(17))
	s.addMail(3, mail(3))
	res = fetchResult(t, g, nil)
	if res.UIDValidity != 7 || res.LastUID != 17 {
		t.Fatalf("有结果时应取最大 UID，得 (%d, %d)", res.UIDValidity, res.LastUID)
	}
	// UIDVALIDITY 变化：带新值返回（worker 据此把游标换新）
	s.setUIDValidity(8)
	res = fetchResult(t, g, &store.FolderState{UIDValidity: 7, LastUID: 17})
	if res.UIDValidity != 8 || res.LastUID != 17 {
		t.Fatalf("UIDVALIDITY 变化后应产出 (8, 17)，得 (%d, %d)", res.UIDValidity, res.LastUID)
	}
	// 增量：只带新邮件的 UID
	s.addMail(40, mail(40))
	res = fetchResult(t, g, &store.FolderState{UIDValidity: 8, LastUID: 17})
	if res.UIDValidity != 8 || res.LastUID != 40 {
		t.Fatalf("增量应产出 (8, 40)，得 (%d, %d)", res.UIDValidity, res.LastUID)
	}
	s.command(t, "UID SEARCH UID 18:*")
}

// 邮件全部早于首次同步窗口（久不收信的邮箱）：时间窗搜索一封都没有，游标要落在**现有最大
// UID** 上，不能由 UIDNEXT 推算：
//   - UIDNEXT = 18（最大 UID + 1，没有投递中的邮件）：按 UIDNEXT-2 推是 16，下一轮 "17:*" 就把
//     从没入库过的 UID 17 当新邮件拉回来；
//   - UIDNEXT = 19（UID 18 已分配、还在投递）：按 UIDNEXT-1 推是 18，这封邮件之后可见时落在
//     游标之前，永远拉不到。
func TestFetchFolderOldMailsBeforeWindow(t *testing.T) {
	for _, uidNext := range []uint32{18, 19} {
		t.Run(fmt.Sprintf("UIDNEXT=%d", uidNext), func(t *testing.T) {
			s, conn := newFakeIMAP(t, defaultGreeting)
			g := newTestClient(t, s, conn)
			s.setUIDNext(uidNext)
			s.addOldMail(3, mail(3))
			s.addOldMail(17, mail(17))

			res := fetchResult(t, g, nil)
			if res.UIDValidity != 7 || res.LastUID != 17 {
				t.Fatalf("邮件全部早于窗口时游标应为现有最大 UID (7, 17)，得 (%d, %d)",
					res.UIDValidity, res.LastUID)
			}
			for _, cmd := range s.commands() {
				if cmdName(cmd) == "UID FETCH" {
					t.Fatalf("早于窗口的邮件不该下载正文：%q", cmd)
				}
			}

			// 还没有新邮件时的下一轮："18:*" 的 18 比任何 UID 都大，真实服务器仍会带回最后一封
			// （UID 17）。它早于窗口、从没入库过，又不带重拉标记——客户端若照单全收，它会被当成
			// **新邮件**入库并通知一次。增量路径必须把游标之前的 UID 过滤掉。
			before := len(s.commands())
			if got := fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: res.LastUID}); len(got) != 0 {
				t.Fatalf("没有新邮件时增量不应带回任何邮件，得 %+v", got)
			}
			for _, cmd := range s.commands()[before:] {
				if cmdName(cmd) == "UID FETCH" {
					t.Fatalf("游标处那封从没入库过的旧邮件不该被下载正文：%q", cmd)
				}
			}
			s.command(t, "UID SEARCH UID 18:*")

			// 之后的邮件可见时：增量 "18:*" 带出它，且只有它（17 是旧邮件，不在结果里）
			s.addMail(18, mail(18))
			got := fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: res.LastUID})
			if len(got) != 1 || got[0].key.UID != 18 {
				t.Fatalf("增量应只带出 UID 18，得 %+v", got)
			}
			s.command(t, "UID SEARCH UID 18:*")
		})
	}
}

// 稳态（没有新邮件）不重复拉取。真实服务器对 "n:*"（n = 游标 + 1）会带回游标处的那封邮件：
// 它已经入库，靠唯一索引去重不会出错，但每一轮都要白白下载一遍全文、解析一遍、走一次失败的事务。
// 千级账号、大部分邮箱数天一封——这点冗余流量会是 IMAP 路径的主要开销。增量路径必须把它滤掉。
func TestFetchFolderIncrementalNoRefetch(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.addMail(3, mail(3))
	s.addMail(17, mail(17))

	// 首次同步：两封都拉，游标落在 17
	res := fetchResult(t, g, nil)
	if res.LastUID != 17 {
		t.Fatalf("首次同步后游标应为 17，得 %d", res.LastUID)
	}
	for range 3 { // 连续几轮都没有新邮件
		before := len(s.commands())
		if got := fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: 17}); len(got) != 0 {
			t.Fatalf("没有新邮件时增量不应带回任何邮件，得 %+v", got)
		}
		for _, cmd := range s.commands()[before:] {
			if cmdName(cmd) == "UID FETCH" {
				t.Fatalf("没有新邮件时不该重复下载游标处的邮件：%q", cmd)
			}
		}
	}

	// 新邮件到达：照常带出，且只有它
	s.addMail(18, mail(18))
	got := fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: 17})
	if len(got) != 1 || got[0].key.UID != 18 {
		t.Fatalf("新邮件到达后增量应只带出 UID 18，得 %+v", got)
	}
}

// 取最大 UID 的那次搜索失败：整轮报错、不产出游标（worker 不推进，下一轮重来）。
// 咽下错误的话 worker 会把这一轮当成成功——空文件夹依旧没有游标，每轮重扫时间窗，
// 新邮件被当成历史邮件。
func TestFetchFolderMaxUIDSearchFails(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.failSearchAt(2) // 第 1 次是时间窗搜索，第 2 次是取最大 UID

	res, err := g.FetchFolder(context.Background(), "INBOX", nil, time.Now().Add(-24*time.Hour),
		func(mailclient.MsgKey, time.Time, []byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "UID SEARCH") {
		t.Fatalf("取最大 UID 的搜索失败应报错，得 %v", err)
	}
	if res.UIDValidity != 0 || res.LastUID != 0 {
		t.Fatalf("失败时不应产出游标，得 %+v", res)
	}
}

func TestFetchFolderHandlerError(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.addMail(1, mail(1))
	s.addMail(2, mail(2))
	boom := errors.New("disk full")
	var handled int
	_, err := g.FetchFolder(context.Background(), "INBOX", nil, time.Now(),
		func(mailclient.MsgKey, time.Time, []byte) error {
			handled++
			if handled == 2 {
				return boom
			}
			return nil
		})
	if !errors.Is(err, boom) {
		t.Fatalf("入库回调的错误应原样返回，得 %v", err)
	}
}

// 服务器在任一阶段卡死（不回响应）：看门狗断开连接，FetchFolder 带明确原因返回，
// 之后的命令也不会永久阻塞（go-imap 在连接关闭时完成挂起的命令；读 goroutine 已退出
// 后再发的命令，写入失败同样立即完成）。
func TestFetchFolderServerStall(t *testing.T) {
	for _, name := range []string{"EXAMINE", "UID SEARCH", "UID FETCH"} {
		t.Run(name, func(t *testing.T) {
			fastTimeouts(t)
			s, conn := newFakeIMAP(t, defaultGreeting)
			g := newTestClient(t, s, conn)
			s.addMail(1, mail(1))
			s.setSilent(name)
			var err error
			call(t, func() {
				_, err = g.FetchFolder(context.Background(), "INBOX", nil, time.Now(),
					func(mailclient.MsgKey, time.Time, []byte) error { return nil })
			})
			if err == nil || !strings.Contains(err.Error(), "无响应") {
				t.Fatalf("服务器卡死应报无响应，得 %v", err)
			}
			call(t, func() { err = g.Logout() })
		})
	}
}

func TestFetchFolderCtxCancel(t *testing.T) {
	fastTimeouts(t)
	stallTimeout = time.Second // 只可能是 ctx 取消让它提前返回
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.addMail(1, mail(1))
	s.setSilent("UID SEARCH")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	var err error
	call(t, func() {
		_, err = g.FetchFolder(ctx, "INBOX", nil, time.Now(),
			func(mailclient.MsgKey, time.Time, []byte) error { return nil })
	})
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("ctx 取消后 %s 才返回（看门狗没有断开连接）", d)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 context.Canceled，得 %v", err)
	}
}

// 单封入库耗时超过无响应阈值（慢盘、数据库争用）不该被当成服务器卡死：
// 入库期间暂停无响应判定。
func TestFetchFolderSlowIngestIsNotStall(t *testing.T) {
	fastTimeouts(t)
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	for n := uint32(1); n <= 3; n++ {
		s.addMail(n, mail(n))
	}
	handled := 0
	_, err := g.FetchFolder(context.Background(), "INBOX", nil, time.Now(),
		func(mailclient.MsgKey, time.Time, []byte) error {
			handled++
			time.Sleep(2 * stallTimeout)
			return nil
		})
	if err != nil {
		t.Fatalf("慢入库不应触发无响应断连: %v", err)
	}
	if handled != 3 {
		t.Fatalf("应入库 3 封，得 %d", handled)
	}
}

// ---------- IDLE ----------

func TestIdleWokenByExists(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.addMail(1, mail(1))
	fetchAll(t, g, nil)

	ch := startIdle(g, context.Background(), 30*time.Second)
	s.waitIdle(t)
	s.addMail(2, mail(2))
	s.addMail(3, mail(3))
	s.setDoneExists(3) // 第二封恰好在 IDLE 收尾期间到达（tagged OK 之前）
	s.exists(2)        // 第一封到达：唤醒 IDLE
	r := waitIdleResult(t, ch)
	if !r.notified || r.err != nil {
		t.Fatalf("EXISTS 应唤醒 IDLE，得 notified=%v err=%v", r.notified, r.err)
	}
	// 同一连接继续可用（DONE 之后服务器回了 tagged OK），并补拉到两封新邮件
	got := fetchAll(t, g, &store.FolderState{UIDValidity: 7, LastUID: 1})
	if len(got) != 2 || got[0].key.UID != 2 || got[1].key.UID != 3 {
		t.Fatalf("唤醒后应补拉到 UID 2、3，得 %+v", got)
	}
	// 收尾期间的信号已被消费：再次 IDLE 安静等到超时，不空转
	start := time.Now()
	ch = startIdle(g, context.Background(), 300*time.Millisecond)
	s.waitIdle(t)
	r = waitIdleResult(t, ch)
	if r.notified || r.err != nil {
		t.Fatalf("没有新邮件的 IDLE 应超时返回 notified=false，得 notified=%v err=%v", r.notified, r.err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("第二次 IDLE 立即返回：存在残留信号")
	}
}

// 邮件在两条命令之间到达（不在 IDLE 中）：EXISTS 信号保留到下一次 IDLE，立即补拉。
func TestIdleSeesMailArrivedOutsideIdle(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	fetchAll(t, g, nil)
	s.addMail(1, mail(1))
	s.exists(1) // 刚拉完就来了新邮件

	r := waitIdleResult(t, startIdle(g, context.Background(), 30*time.Second))
	if !r.notified || r.err != nil {
		t.Fatalf("IDLE 之前到达的 EXISTS 也应触发拉取，得 notified=%v err=%v", r.notified, r.err)
	}
	got := fetchAll(t, g, &store.FolderState{UIDValidity: 7})
	if len(got) != 1 || got[0].key.UID != 1 {
		t.Fatalf("应补拉到 UID 1，得 %+v", got)
	}
}

// 只有 EXISTS 才算"有新邮件"：FLAGS / PERMANENTFLAGS 变化不该唤醒 IDLE（否则空转）。
func TestIdleIgnoresFlagChanges(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	fetchAll(t, g, nil)
	start := time.Now()
	ch := startIdle(g, context.Background(), 300*time.Millisecond)
	s.waitIdle(t)
	s.line(`* FLAGS (\Seen)`)
	s.line(`* OK [PERMANENTFLAGS (\Answered \Seen \Draft)] Limited`)
	r := waitIdleResult(t, ch)
	if r.notified || r.err != nil {
		t.Fatalf("FLAGS 变化不应唤醒 IDLE，得 notified=%v err=%v", r.notified, r.err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("IDLE 被 FLAGS 变化提前唤醒")
	}
}

func TestIdleTimeout(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	start := time.Now()
	r := waitIdleResult(t, startIdle(g, context.Background(), 200*time.Millisecond))
	if r.notified || r.err != nil {
		t.Fatalf("超时应返回 notified=false, err=nil，得 notified=%v err=%v", r.notified, r.err)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("IDLE 早于超时返回")
	}
}

// 手动收取/兜底拉取路径：ctx 取消后连接必须仍可用（同一连接上继续 EXAMINE/FETCH）。
func TestIdleCtxCancelKeepsConnection(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := startIdle(g, ctx, 30*time.Second)
	s.waitIdle(t)
	start := time.Now()
	cancel()
	r := waitIdleResult(t, ch)
	if r.notified || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 context.Canceled，得 notified=%v err=%v", r.notified, r.err)
	}
	assertReturnedFast(t, start)

	s.addMail(1, mail(1))
	if got := fetchAll(t, g, nil); len(got) != 1 {
		t.Fatalf("ctx 取消后连接应仍可用，得 %+v", got)
	}
}

func TestIdleHandshakeStall(t *testing.T) {
	fastTimeouts(t)
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.setSilent("IDLE") // 服务器不回 "+ idling"
	var notified bool
	var err error
	call(t, func() { notified, err = g.Idle(context.Background(), 30*time.Second) })
	if notified || err == nil || !strings.Contains(err.Error(), "无响应") {
		t.Fatalf("IDLE 握手卡死应报无响应，得 notified=%v err=%v", notified, err)
	}
}

// DONE 发出去了但服务器不回 tagged OK（连接已被静默丢弃）：不能等到 IDLE 超时，
// 收尾上限一到就强制断开——否则停机/删除账号时 worker 会卡在"等 IDLE 返回"上。
func TestIdleDoneUnconfirmed(t *testing.T) {
	fastTimeouts(t)
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.setSilent("DONE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := startIdle(g, ctx, 30*time.Second)
	s.waitIdle(t)
	start := time.Now()
	cancel()
	r := waitIdleResult(t, ch)
	if r.notified || !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ctx 取消应返回 context.Canceled，得 notified=%v err=%v", r.notified, r.err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("服务器不确认 DONE 时 %s 才返回（在等 IDLE 超时）", d)
	}
	// 连接已被强制断开：后续命令立即出错
	start = time.Now()
	if _, err := g.FetchFolder(context.Background(), "INBOX", nil, time.Now(),
		func(mailclient.MsgKey, time.Time, []byte) error { return nil }); err == nil {
		t.Fatal("DONE 未被确认的连接应已断开")
	}
	assertReturnedFast(t, start)
}

func TestIdleServerDisconnect(t *testing.T) {
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	ch := startIdle(g, context.Background(), 30*time.Second)
	s.waitIdle(t)
	s.close()
	r := waitIdleResult(t, ch)
	if r.notified || r.err == nil {
		t.Fatalf("服务器断开应返回错误，得 notified=%v err=%v", r.notified, r.err)
	}
}

// ---------- LOGOUT ----------

func TestLogoutTimeout(t *testing.T) {
	fastTimeouts(t)
	s, conn := newFakeIMAP(t, defaultGreeting)
	g := newTestClient(t, s, conn)
	s.setSilent("LOGOUT")
	var err error
	call(t, func() { err = g.Logout() })
	if err == nil || !strings.Contains(err.Error(), "LOGOUT") {
		t.Fatalf("LOGOUT 超时应报错，得 %v", err)
	}
}

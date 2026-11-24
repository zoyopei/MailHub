package imapclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
)

// STARTTLS 的用例一律走真实 TCP（127.0.0.1 临时端口）而不是 net.Pipe：net.Pipe 是无缓冲的
// 同步管道，TLS 握手失败时客户端发 alert、服务端发握手数据，双方都卡在写上（没人读）。
// 真实 socket 有内核缓冲，不存在这个问题。

// ---------- 测试材料：自签名证书、假服务器监听 ----------

// testCert 自签名的服务器证书（SAN：imap.test / localhost / 127.0.0.1），每次调用重新生成，
// 任何系统的根证书库都不可能信任它——"证书不受信"的用例因此是确定性的。
func testCert(t *testing.T) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "imap.test"},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"imap.test", "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// trustCert 让客户端信任测试证书（替换包级 tlsConfigForHost，测试结束恢复）。
func trustCert(t *testing.T, leaf *x509.Certificate) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	old := tlsConfigForHost
	tlsConfigForHost = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool}
	}
	t.Cleanup(func() { tlsConfigForHost = old })
}

func serverTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

// fakeOpts 假服务器的初始设置（必须在客户端连上来之前定好）。
type fakeOpts struct {
	greeting    string      // 缺省 defaultGreeting
	implicitTLS bool        // 一上来就是 TLS（隐式 TLS 端口，如 993）
	startTLS    *tls.Config // 非 nil：支持 STARTTLS；nil：回 NO
	stallTLS    bool        // STARTTLS 回 OK 后既不握手也不回数据（升级途中服务器卡死）
	password    string      // LOGIN 接受的密码
}

// newFakeIMAPListener 在 127.0.0.1 的临时端口上监听，接受**一条**连接并交给假服务器。
// 假服务器句柄经返回的 chan 交回（客户端连上来之后才有）。
func newFakeIMAPListener(t *testing.T, o fakeOpts) (int, <-chan *fakeIMAP) {
	t.Helper()
	if o.greeting == "" {
		o.greeting = defaultGreeting
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan *fakeIMAP, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		var conn net.Conn = raw
		if o.implicitTLS {
			conn = tls.Server(raw, o.startTLS)
		}
		s := newFakeIMAPOn(conn, o.greeting)
		s.tlsCfg, s.stallTLS, s.password = o.startTLS, o.stallTLS, o.password // serve 尚未启动，直接赋值
		go s.serve()
		got <- s
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

// useStartTLSPort 把"明文 + STARTTLS"的端口判定指向临时监听端口（测试监听不了 143）。
func useStartTLSPort(t *testing.T, port int) {
	t.Helper()
	old := startTLSPort
	startTLSPort = port
	t.Cleanup(func() { startTLSPort = old })
}

const startTLSGreeting = "* OK [CAPABILITY IMAP4rev1 IDLE SASL-IR STARTTLS] fake ready"

func connectPassword(g *goClient, ctx context.Context, host string, port int) error {
	_, err := g.Connect(ctx, host, port, "u@x.com",
		mailclient.Credentials{AuthType: "password", Password: "secret"})
	return err
}

// assertNoLogin 断言服务器没有收到任何登录命令（凭据没有离开本机）。
func assertNoLogin(t *testing.T, s *fakeIMAP) {
	t.Helper()
	for _, cmd := range s.commands() {
		if strings.HasPrefix(cmd, "LOGIN ") || strings.HasPrefix(cmd, "AUTHENTICATE ") {
			t.Fatalf("升级失败后绝不能发登录命令，服务器实际收到：%q", s.commands())
		}
	}
}

// assertUntrusted 断言错误的根因是证书校验失败（而不是别的什么碰巧让握手没成）。
func assertUntrusted(t *testing.T, err error) {
	t.Helper()
	var unknown x509.UnknownAuthorityError
	var verify *tls.CertificateVerificationError
	if !errors.As(err, &unknown) && !errors.As(err, &verify) {
		t.Fatalf("根因应是证书校验失败，得 %v", err)
	}
}

// ---------- STARTTLS（143 端口） ----------

// 143 端口：明文拨号 → STARTTLS 升级 → 之后才登录，且登录命令经 TLS 层送达。
func TestStartTLSLogin(t *testing.T) {
	cert, leaf := testCert(t)
	port, got := newFakeIMAPListener(t, fakeOpts{greeting: startTLSGreeting,
		startTLS: serverTLSConfig(cert), password: "secret"})
	useStartTLSPort(t, port)
	trustCert(t, leaf)

	g := &goClient{tokens: mstoken.NewTokenCache()}
	if err := connectPassword(g, context.Background(), "127.0.0.1", port); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	s := <-got
	// STARTTLS 成功后 go-imap 会在后台 goroutine 里异步补发一条 CAPABILITY（重置能力缓存），
	// 它和 LOGIN 谁先上线不确定，两种顺序都是合法的命令流水线。这里只断言要紧的：
	// 第一条命令是 STARTTLS，登录命令排在它之后，中间至多夹着 CAPABILITY。
	cmds := s.commands()
	loginAt := slices.IndexFunc(cmds, func(c string) bool {
		return strings.HasPrefix(c, "LOGIN ") || strings.HasPrefix(c, "AUTHENTICATE ")
	})
	if len(cmds) == 0 || cmds[0] != "STARTTLS" {
		t.Fatalf("第一条命令应是 STARTTLS，实际 %q", cmds)
	}
	if loginAt < 1 {
		t.Fatalf("登录命令应排在 STARTTLS 之后，实际 %q", cmds)
	}
	for _, c := range cmds[1:loginAt] {
		if c != "CAPABILITY" {
			t.Fatalf("STARTTLS 与登录之间不应有其他命令，实际 %q", cmds)
		}
	}
	if cmds[loginAt] != `LOGIN "u@x.com" "secret"` {
		t.Fatalf("升级后应以明文密码登录，实际 %q", cmds)
	}
	if _, ok := s.connOf().(*tls.Conn); !ok {
		t.Fatalf("服务器侧连接应已升级为 TLS，得 %T", s.connOf())
	}
	// 升级后的连接照常可用
	s.addMail(1, mail(1))
	if got := fetchAll(t, g, nil); len(got) != 1 || got[0].key.UID != 1 {
		t.Fatalf("升级后的连接应能拉取，得 %+v", got)
	}
	if err := g.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
}

// 服务器不支持 STARTTLS：报错，绝不退回明文登录。
func TestStartTLSNotSupported(t *testing.T) {
	port, got := newFakeIMAPListener(t, fakeOpts{greeting: startTLSGreeting, password: "secret"})
	useStartTLSPort(t, port)

	err := connectPassword(&goClient{tokens: mstoken.NewTokenCache()}, context.Background(), "127.0.0.1", port)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("服务器不支持 STARTTLS 应报错，得 %v", err)
	}
	if errors.Is(err, ErrAuthFailed) {
		t.Fatalf("升级失败不是凭据问题，不应是 ErrAuthFailed，得 %v", err)
	}
	assertNoLogin(t, <-got)
}

func TestStartTLSUntrustedCert(t *testing.T) {
	cert, _ := testCert(t)
	port, got := newFakeIMAPListener(t, fakeOpts{greeting: startTLSGreeting,
		startTLS: serverTLSConfig(cert), password: "secret"}) // 不 trustCert：走系统根证书
	useStartTLSPort(t, port)

	start := time.Now()
	var err error
	call(t, func() {
		err = connectPassword(&goClient{tokens: mstoken.NewTokenCache()}, context.Background(), "127.0.0.1", port)
	})
	assertReturnedFast(t, start)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("证书不受信应报错并指明发生在 STARTTLS，得 %v", err)
	}
	assertUntrusted(t, err)
	assertNoLogin(t, <-got)
}

// 服务器回了 STARTTLS OK 之后就卡住（不回 TLS ServerHello）：go-imap 的握手没有超时，
// 靠看门狗断开连接，调用带"无响应"原因返回。
func TestStartTLSHandshakeStall(t *testing.T) {
	fastTimeouts(t)
	cert, leaf := testCert(t)
	port, got := newFakeIMAPListener(t, fakeOpts{greeting: startTLSGreeting,
		startTLS: serverTLSConfig(cert), stallTLS: true, password: "secret"})
	useStartTLSPort(t, port)
	trustCert(t, leaf)

	var err error
	call(t, func() {
		err = connectPassword(&goClient{tokens: mstoken.NewTokenCache()}, context.Background(), "127.0.0.1", port)
	})
	if err == nil || !strings.Contains(err.Error(), "无响应") {
		t.Fatalf("握手卡死应报无响应，得 %v", err)
	}
	assertNoLogin(t, <-got)
}

func TestStartTLSCtxCancel(t *testing.T) {
	fastTimeouts(t)
	stallTimeout = time.Second // 只可能是 ctx 取消让它提前返回
	cert, leaf := testCert(t)
	port, got := newFakeIMAPListener(t, fakeOpts{greeting: startTLSGreeting,
		startTLS: serverTLSConfig(cert), stallTLS: true, password: "secret"})
	useStartTLSPort(t, port)
	trustCert(t, leaf)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	var err error
	call(t, func() {
		err = connectPassword(&goClient{tokens: mstoken.NewTokenCache()}, ctx, "127.0.0.1", port)
	})
	if d := time.Since(start); d >= time.Second {
		t.Fatalf("ctx 取消后 %s 才返回（看门狗没有断开连接）", d)
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrAuthFailed) {
		t.Fatalf("ctx 取消应返回 context.Canceled，得 %v", err)
	}
	assertNoLogin(t, <-got)
}

// ---------- 其他端口：隐式 TLS ----------

func TestConnectImplicitTLS(t *testing.T) {
	cert, leaf := testCert(t)
	port, got := newFakeIMAPListener(t, fakeOpts{implicitTLS: true,
		startTLS: serverTLSConfig(cert), password: "secret"})
	trustCert(t, leaf)

	g := &goClient{tokens: mstoken.NewTokenCache()}
	if err := connectPassword(g, context.Background(), "localhost", port); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	s := <-got
	if want := []string{`LOGIN "u@x.com" "secret"`}; !slices.Equal(s.commands(), want) {
		t.Fatalf("隐式 TLS 端口不应再发 STARTTLS，实际收到：%q", s.commands())
	}
	_ = g.Close()
}

func TestConnectImplicitTLSUntrustedCert(t *testing.T) {
	cert, _ := testCert(t)
	port, got := newFakeIMAPListener(t, fakeOpts{implicitTLS: true,
		startTLS: serverTLSConfig(cert), password: "secret"})

	start := time.Now()
	err := connectPassword(&goClient{tokens: mstoken.NewTokenCache()}, context.Background(), "localhost", port)
	assertReturnedFast(t, start)
	assertUntrusted(t, err)
	assertNoLogin(t, <-got)
}

func TestDialAddr(t *testing.T) {
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"imap.example.com", 993, "imap.example.com:993"},
		{"imap.example.com", 143, "imap.example.com:143"},
		{"2001:db8::1", 993, "[2001:db8::1]:993"}, // IPv6 字面量必须加方括号
	} {
		got := dialAddr(c.host, c.port)
		if got != c.want {
			t.Errorf("dialAddr(%q, %d) = %q，期望 %q", c.host, c.port, got, c.want)
		}
		// 拨号地址必须能被 net 拆回主机与端口（SplitHostPort 返回的主机不带方括号）
		h, p, err := net.SplitHostPort(got)
		if err != nil || p != strconv.Itoa(c.port) || net.JoinHostPort(h, p) != got {
			t.Errorf("dialAddr(%q, %d) = %q 不是合法的 主机:端口（h=%q p=%q err=%v）",
				c.host, c.port, got, h, p, err)
		}
	}
}

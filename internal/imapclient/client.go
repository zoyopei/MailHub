// Package imapclient 封装 IMAP 连接与增量拉取（TECH.md §2.4）。
// 实现 mailclient.MailClient 接口；令牌刷新统一走 mstoken（IMAP/Graph 不同 scope）。
// 只读原则（DESIGN.md 3.1）：SelectOptions.ReadOnly（EXAMINE）+ BODY.PEEK。
//
// 超时模型（由 go-imap v2 beta.9 的行为决定，改依赖版本时重新核对）：
//   - 一个响应"开始"之后 go-imap 自带读超时（普通响应 30s、字面量 5min）；
//   - 等服务器下一个响应的**第一个字节**没有任何超时——服务器卡死、或连接被中间设备
//     静默丢弃（没有 FIN/RST）时，Wait() 永远阻塞，ctx 取消也无效。这里用看门狗
//     （startWatch）补上；IDLE 收尾与 LOGOUT 另设上限。
package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailhub/internal/mailclient"
	"mailhub/internal/mstoken"
	"mailhub/internal/store"
)

// ErrAuthFailed IMAP 认证被拒（密码错误等），worker 映射 auth_failed。
// 刷新令牌失效统一用 mstoken.ErrReauth。
var ErrAuthFailed = errors.New("IMAP 认证失败")

// 超时参数（包级变量：测试缩短）。
var (
	// stallTimeout 服务器"无响应"的判定上限：连接上持续这么久没有收到任何数据，
	// 断开连接。本地处理（入库回调）期间不计入，见 watchdog.pause。
	stallTimeout = 2 * time.Minute
	// idleStopTimeout IDLE 收尾（发出 DONE 后等服务器确认）的上限。
	idleStopTimeout = 10 * time.Second
	// logoutTimeout LOGOUT 的上限。
	logoutTimeout = 5 * time.Second
)

// goClient 是 mailclient.MailClient 的 go-imap 实现。TokenCache 进程级共享：
// 同账号复用未过期 access_token，轮询短连接不重复刷新令牌端点（TECH.md §2.4）。
// 每个会话一个实例（main 的 dial 每次返回新对象），字段只在会话 goroutine 上读写。
type goClient struct {
	conn       *imapclient.Client
	tokens     *mstoken.TokenCache
	activity   *activityConn // 记录"最近收到数据"时刻的连接，看门狗用
	unilateral chan struct{} // 服务器通告邮箱变化（EXISTS）的信号，cap=1，由 Idle 消费
}

// startTLSPort 明文端口：按惯例 143 端口先建立 TCP 连接再用 STARTTLS 升级加密。
// 其他端口（默认 993）一律按隐式 TLS 处理。变量只为测试替换（测试监听不了 143）。
var startTLSPort = 143

// tlsConfigForHost TLS 校验配置：按连接的主机名校验服务器证书。
// 包级变量只为测试替换（信任自签名的假服务器证书）；生产代码不要改它。
var tlsConfigForHost = func(host string) *tls.Config {
	return &tls.Config{ServerName: host}
}

// dialAddr 主机 + 端口 → 拨号地址。用 net.JoinHostPort 而不是字符串拼接：
// IPv6 字面量必须加方括号（"[2001:db8::1]:993"），拼接会得到非法地址。
func dialAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// NewGoClient 构造实现。tokens 传 nil 时内部自动创建。
func NewGoClient(tokens *mstoken.TokenCache) mailclient.MailClient {
	if tokens == nil {
		tokens = mstoken.NewTokenCache()
	}
	return &goClient{tokens: tokens}
}

// activityConn 在 net.Conn 上记录最近一次收到数据的时刻。go-imap 的 bufio.Reader 只在
// 自己的缓冲区空了时才调用 Read，所以每次 Read 都对应真实的网络进展。
type activityConn struct {
	net.Conn
	lastRead atomic.Int64 // unixNano；0 = 尚未收到数据
}

func (c *activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.lastRead.Store(time.Now().UnixNano())
	}
	return n, err
}

func (c *activityConn) touchedAt() time.Time {
	if ns := c.lastRead.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// watchdog 监视一条连接，两种情况下强制关闭它：
//   - 无响应：持续 stallTimeout 既没收到数据、也不在本地处理中（pause）；
//   - ctx 被取消：停机、删除/重载账号。
//
// 关闭连接会让 go-imap 的读 goroutine 退出，并把当时挂起的命令全部以错误完成——所以
// 调用方阻塞中的 Wait() 立即返回，随后经 decorate 得知原因。只负责"关"，不替调用方
// 返回错误。
type watchdog struct {
	ctx      context.Context
	raw      net.Conn           // 底层连接；cl 为 nil（STARTTLS 握手期间，客户端对象还没建好）时关它
	cl       *imapclient.Client // 可为 nil
	lastRead *activityConn
	stall    time.Duration // 无响应判定阈值（启动时的 stallTimeout 快照）
	progress atomic.Int64  // 最近一次本地进展（unixNano）
	paused   atomic.Bool   // 调用方正在本地处理，暂停无响应判定
	stalled  atomic.Bool   // 是否因无响应断开
	done     chan struct{}
}

// startWatch 开始监视 cl（nil = 客户端对象尚未建立，只能关底层连接）。返回的看门狗必须且只能 stop 一次。
func (g *goClient) startWatch(ctx context.Context, cl *imapclient.Client) *watchdog {
	w := &watchdog{ctx: ctx, raw: g.activity, cl: cl, lastRead: g.activity, stall: stallTimeout,
		done: make(chan struct{})}
	w.progress.Store(time.Now().UnixNano())
	go w.run()
	return w
}

// kill 断开连接：有客户端对象就关它（同时让挂起的命令出错返回），否则关底层连接。
func (w *watchdog) kill() {
	if w.cl != nil {
		w.cl.Close()
		return
	}
	_ = w.raw.Close()
}

func (w *watchdog) run() {
	ticker := time.NewTicker(max(w.stall/8, 5*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-w.ctx.Done():
			w.kill()
			return
		case <-ticker.C:
			if w.paused.Load() {
				continue
			}
			last := w.lastRead.touchedAt()
			if p := time.Unix(0, w.progress.Load()); p.After(last) {
				last = p
			}
			if time.Since(last) < w.stall {
				continue
			}
			select {
			case <-w.done: // 与 stop 同时发生：操作其实已完成，不动连接
			default:
			}
			w.stalled.Store(true)
			w.kill()
			return
		}
	}
}

// stop 结束监视（非阻塞）。已经判定无响应并开始断开的看门狗不会被撤回。
func (w *watchdog) stop() { close(w.done) }

// pause 暂停无响应判定：调用方接下来要做本地处理，期间连接上没有数据是正常的。
// 不暂停的话，一次超过 stallTimeout 的慢入库（磁盘、数据库争用）会被当成服务器卡死。
func (w *watchdog) pause() { w.paused.Store(true) }

func (w *watchdog) resume() {
	w.progress.Store(time.Now().UnixNano())
	w.paused.Store(false)
}

// decorate 把看门狗断连的后果翻译成可读原因；其余错误原样返回。
// ctx 取消时返回 ctx.Err() 本身（worker 用 == context.Canceled 判断，不能包一层）。
func (w *watchdog) decorate(err error) error {
	if err == nil {
		return nil
	}
	if w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	if w.stalled.Load() {
		return fmt.Errorf("IMAP 服务器超过 %s 无响应，已断开（%v）", w.stall, err)
	}
	return err
}

// Connect 建立连接并登录。加密方式按端口选择：
//   - 143：明文 TCP + STARTTLS。升级失败（服务器不支持、握手或证书校验失败）直接报错——
//     绝不在明文连接上发登录命令；
//   - 其余端口（默认 993）：隐式 TLS。
func (g *goClient) Connect(ctx context.Context, host string, port int, username string, cred mailclient.Credentials) (string, error) {
	addr := dialAddr(host, port)
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	if port == startTLSPort {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return "", fmt.Errorf("连接 %s: %w", addr, err)
		}
		rotated, err := g.loginStartTLS(ctx, conn, host, username, cred)
		if err != nil {
			_ = conn.Close() // NewStartTLS 失败时它自己会关；这里兜底其余路径
			return "", err
		}
		return rotated, nil
	}
	// 使用 tls.Dialer.DialContext 透传 ctx：停机或删除账号可立即中断卡住的 TCP/TLS 握手。
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfigForHost(host)}
	conn, err := tlsDialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("连接 %s: %w", addr, err)
	}
	rotated, err := g.login(ctx, conn, username, cred)
	if err != nil {
		_ = g.conn.Close()
		return "", err
	}
	return rotated, nil
}

// options go-imap 客户端选项。tlsCfg 仅 STARTTLS 用到（NewStartTLS 用它做握手与证书校验）。
func (g *goClient) options(tlsCfg *tls.Config) *imapclient.Options {
	return &imapclient.Options{
		TLSConfig: tlsCfg,
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			// 服务器通告的邮箱变化。EXAMINE 期间的 EXISTS 归属那条 Select 命令，不会走到
			// 这里；走到这里的是 IDLE 等待期间（或两条命令之间）的 EXISTS = 有新邮件。
			// 回调在 go-imap 的读 goroutine 里执行，必须立即返回：只发信号，由 Idle 接收。
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages == nil {
					return // FLAGS / PERMANENTFLAGS 变化，与收信无关
				}
				select {
				case g.unilateral <- struct{}{}:
				default: // 已有待处理信号，合并
				}
			},
		},
	}
}

// login 在已建立的（隐式 TLS）连接上创建客户端并认证。与 Connect 分开是为了能用 net.Pipe
// 假服务器测试（Connect 强制 TLS）。
func (g *goClient) login(ctx context.Context, conn net.Conn, username string, cred mailclient.Credentials) (string, error) {
	g.activity = &activityConn{Conn: conn}
	g.unilateral = make(chan struct{}, 1)
	g.conn = imapclient.New(g.activity, g.options(nil))
	// 先等问候：服务器以 "* BYE" 拒绝接入时，后面任何命令都只会得到一个含义不明的
	// 写入错误（读 goroutine 已经退出），拿不到 BYE 的原文。
	if err := g.conn.WaitGreeting(); err != nil {
		return "", fmt.Errorf("IMAP 问候: %w", err)
	}
	return g.authenticate(ctx, username, cred)
}

// loginStartTLS 在明文连接上完成 STARTTLS 升级并认证（143 端口）。
func (g *goClient) loginStartTLS(ctx context.Context, conn net.Conn, host, username string,
	cred mailclient.Credentials) (string, error) {
	g.activity = &activityConn{Conn: conn}
	g.unilateral = make(chan struct{}, 1)
	// STARTTLS 命令的应答与 TLS 握手都由 imapclient.NewStartTLS 内部完成，期间客户端对象
	// 还没建好；握手没有超时，所以先开一个只盯底层连接的看门狗（ctx 取消或服务器卡住就断开）。
	wd := g.startWatch(ctx, nil)
	cl, err := imapclient.NewStartTLS(g.activity, g.options(tlsConfigForHost(host)))
	if err != nil {
		wd.stop()
		// 不附带服务器返回的细节以外的敏感信息；证书类错误（x509）原样带出便于排错。
		return "", wd.decorate(fmt.Errorf(
			"STARTTLS（端口 %d）：服务器不支持或证书校验失败，拒绝明文登录: %w", startTLSPort, err))
	}
	g.conn = cl
	wd.stop() // 之后由 authenticate 自己的看门狗负责
	// NewStartTLS 已消费问候（并在明文连接上拒绝 PREAUTH）；这里只是防御性确认。
	if err := g.conn.WaitGreeting(); err != nil {
		return "", fmt.Errorf("IMAP 问候: %w", err)
	}
	return g.authenticate(ctx, username, cred)
}

// authenticate 发认证命令（调用前问候已被消费）。
func (g *goClient) authenticate(ctx context.Context, username string, cred mailclient.Credentials) (string, error) {
	switch cred.AuthType {
	case "oauth2":
		// 取令牌不在看门狗监视内：它走 HTTP（mstoken 自带超时），期间 IMAP 连接本来就没有数据。
		access, rotated, err := g.tokens.Token(ctx, mstoken.IMAPScope, username, cred.ClientID, cred.RefreshToken)
		if err != nil {
			return "", err
		}
		wd := g.startWatch(ctx, g.conn)
		defer wd.stop()
		if err := g.conn.Authenticate(newXOAUTH2(username, access)); err != nil {
			return "", authError(wd.decorate(err), "XOAUTH2 被拒绝（确认账号已启用 IMAP 且令牌含 IMAP 权限）")
		}
		return rotated, nil
	default: // password / 授权码
		wd := g.startWatch(ctx, g.conn)
		defer wd.stop()
		if err := g.conn.Login(username, cred.Password).Wait(); err != nil {
			return "", authError(wd.decorate(err), "")
		}
		return "", nil
	}
}

// authError 归类认证命令的错误。只有服务器明确回了 tagged NO/BAD 才算凭据被拒
// （ErrAuthFailed → worker 置 auth_failed 并停止重试，DESIGN.md 3.4）。断连、无响应、
// ctx 取消、* BYE 都按普通错误处理，走退避重连——以前任何错误都包成认证失败，
// 一次网络抖动就会把账号停在 auth_failed（重启也不会自动恢复），必须人工重新触发。
func authError(err error, hint string) error {
	if !isAuthRejection(err) {
		return err
	}
	if hint != "" {
		return fmt.Errorf("%w: %s: %v", ErrAuthFailed, hint, err)
	}
	return fmt.Errorf("%w: %v", ErrAuthFailed, err)
}

// isAuthRejection 错误是否为服务器对认证命令回的 tagged NO/BAD（*imap.Error）。
// BYE 不算：那是服务器在断连接（限流、维护、连接数超限），不代表凭据有问题。
func isAuthRejection(err error) bool {
	var resp *imap.Error
	return errors.As(err, &resp) &&
		(resp.Type == imap.StatusResponseTypeNo || resp.Type == imap.StatusResponseTypeBad)
}

// FetchFolder 只读打开文件夹并增量拉取：
//   - cursor 有效且 UIDVALIDITY 一致 → UID > last_uid 增量；
//   - 无游标或 UIDVALIDITY 变化 → 归零重拉，时间窗取 since（首次同步窗口，
//     DESIGN.md 3.4；重复邮件由 worker 标记 Reindex、IngestMessage 按 Message-ID 幂等忽略）。
//
// 结果带回文件夹当前的 UIDVALIDITY 与本轮的最大 UID，供 worker 在整轮成功后推进游标
// （一封都没拉到时：增量路径为 0 即不推进；首次同步为现有邮件的最大 UID，见下方注释）。
// Graph 的游标产出（latestSeen/deltaLink）在 IMAP 路径
// 不适用，恒返回零值。
// 整个过程在看门狗监视下：服务器无响应或 ctx 取消都会让阻塞的命令立即出错返回。
func (g *goClient) FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
	handle func(key mailclient.MsgKey, seenAt time.Time, raw []byte) error) (res mailclient.FetchResult, err error) {

	wd := g.startWatch(ctx, g.conn)
	defer wd.stop()
	defer func() { err = wd.decorate(err) }()

	sel, err := g.conn.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait() // EXAMINE
	if err != nil {
		return mailclient.FetchResult{}, fmt.Errorf("EXAMINE %s: %w", folder, err)
	}
	uidv := sel.UIDValidity
	res.UIDValidity = uidv

	var lastUID uint32
	var window time.Time
	if cursor != nil && cursor.UIDValidity == uidv {
		lastUID = cursor.LastUID
	} else {
		window = since
	}

	criteria := &imap.SearchCriteria{}
	if cursor != nil && cursor.UIDValidity == uidv {
		// "n:*"（stop 为 0 表示 *）：各服务器通吃的标准写法。lastUID 为 0（空文件夹建立游标时）
		// 即 "1:*"，空邮箱上不匹配任何邮件。
		var set imap.UIDSet
		set.AddRange(imap.UID(lastUID+1), 0)
		criteria.UID = []imap.UIDSet{set}
	}
	if !window.IsZero() {
		criteria.Since = window // INTERNALDATE 侧，近似收到时间
	}
	data, err := g.conn.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return mailclient.FetchResult{}, fmt.Errorf("UID SEARCH: %w", err)
	}
	uids := data.AllUIDs()
	if cursor != nil && cursor.UIDValidity == uidv {
		// 增量路径只认游标之后的 UID。"n:*" 在 n 比文件夹里任何 UID 都大时**仍然匹配最后一封**
		// （RFC 3501），所以没有新邮件时服务器不会回空，而是带回游标处的那封：
		//   - 它已经入库：重复下载一遍全文、解析、走一次因唯一索引而回滚的事务。千级账号、
		//     大部分邮箱数天一封，这点冗余就是 IMAP 路径的主要流量；
		//   - 首次同步时窗口内一封都没有（邮件全部早于窗口）：游标取的是现有最大 UID，那封邮件
		//     早于窗口、从没入库过，又不带重拉标记——会被当成**新邮件**入库并通知一次。
		// 不依赖服务器对 "n:*" 的实现细节，这里一律滤掉。
		kept := uids[:0]
		for _, uid := range uids {
			if uint32(uid) > lastUID {
				kept = append(kept, uid)
			}
		}
		uids = kept
	}
	if len(uids) == 0 {
		// 没有新邮件。增量路径（游标有效）不动游标：下一轮照样搜 "n:*"，空结果很便宜。
		//
		// 首次同步且窗口内一封都没有（空文件夹，或邮件全部早于窗口）：必须建游标，否则每轮
		// 都按"首次同步"重扫整个时间窗、新邮件也被当成历史邮件不通知。游标取"现有邮件的最大
		// UID"——再搜一次 UID 1:*（只回序号，不下载正文）。不能用 EXAMINE 报的 UIDNEXT 推算：
		//   - UIDNEXT-1：UIDNEXT 可能已经计入正在投递的邮件（UID 已分配、还不响应 SEARCH），
		//     那封邮件会落在游标之前，永远拉不到；
		//   - UIDNEXT-2（留余量）：没有投递中邮件时它比最大 UID 小 1，下一轮 "n:*" 会把最新的
		//     那封旧邮件（早于窗口、从未入库）也带出来——增量路径没有重拉标记，它被当成新邮件
		//     入库并通知。久不收信的邮箱正是这种情况。
		// 按实际搜到的最大值定游标没有这些问题：投递中的邮件此刻不在结果里，之后可见时 UID
		// 大于游标，下一轮 "n:*" 自然带出。搜索失败则整轮报错，游标不动，下一轮重来。
		if cursor == nil || cursor.UIDValidity != uidv {
			max, err := g.maxVisibleUID()
			if err != nil {
				return mailclient.FetchResult{}, fmt.Errorf("UID SEARCH（取最大 UID）: %w", err)
			}
			res.LastUID = max
		}
		return res, nil
	}
	// 不依赖服务器返回 UID 的顺序，显式取最大值。
	for _, uid := range uids {
		if uint32(uid) > res.LastUID {
			res.LastUID = uint32(uid)
		}
	}
	fetchOpts := &imap.FetchOptions{
		UID:          true,
		InternalDate: true, // 服务器记录的到达时间 → received_at（邮件真实时间）
		BodySection: []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierNone, Peek: true}, // BODY.PEEK[]：完整原文，不改 \Seen
		},
	}
	// 分批 + 流式读取（cmd.Next）：每批至多 fetchBatchSize 封、单次仅持一封原文在内存，
	// 避免首次同步大邮箱时 Collect() 一次性加载全部邮件引发 OOM，同时及时响应 ctx 取消。
	const fetchBatchSize = 20
	for start := 0; start < len(uids); start += fetchBatchSize {
		if ctx.Err() != nil {
			return mailclient.FetchResult{}, ctx.Err()
		}
		end := min(start+fetchBatchSize, len(uids))
		var set imap.UIDSet
		for _, uid := range uids[start:end] {
			set.AddNum(uid)
		}
		// 必须传值类型：go-imap 的 Fetch 内部按 imap.UIDSet 做类型断言，传 *imap.UIDSet
		// 能通过编译（方法集相同）但运行时 panic "imap: invalid NumSet type"——worker 里
		// 的 panic 会让整个进程退出。
		cmd := g.conn.Fetch(set, fetchOpts)
		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				_ = cmd.Close()
				return mailclient.FetchResult{}, fmt.Errorf("UID FETCH: %w", err)
			}
			raw := rawBytes(buf)
			if raw == nil {
				continue
			}
			// seenAt = INTERNALDATE（邮件到达邮箱的真实时间；个别服务器缺失时由 worker 用 Date 头兜底）
			wd.pause() // 入库是本地处理，期间连接上没有数据是正常的
			herr := handle(mailclient.MsgKey{UIDValidity: uidv, UID: uint32(buf.UID)}, buf.InternalDate, raw)
			wd.resume()
			if herr != nil {
				_ = cmd.Close()
				return mailclient.FetchResult{}, herr
			}
			if ctx.Err() != nil {
				_ = cmd.Close()
				return mailclient.FetchResult{}, ctx.Err()
			}
		}
		if err := cmd.Close(); err != nil {
			return mailclient.FetchResult{}, fmt.Errorf("UID FETCH: %w", err)
		}
	}
	return res, nil
}

// maxVisibleUID 文件夹里现有邮件的最大 UID（空文件夹为 0）：一次 UID SEARCH UID 1:*，
// 只回序号、不下载正文。用于首次同步空结果时建立游标（见 FetchFolder）。
func (g *goClient) maxVisibleUID() (uint32, error) {
	var set imap.UIDSet
	set.AddRange(1, 0) // 1:*（0 表示 *）
	data, err := g.conn.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return 0, err
	}
	var max uint32
	for _, uid := range data.AllUIDs() {
		if uint32(uid) > max {
			max = uint32(uid)
		}
	}
	return max, nil
}

// SupportsIdle 服务器是否通告 IDLE 能力（RFC 2177）。
func (g *goClient) SupportsIdle() bool {
	if g.conn == nil {
		return false
	}
	return g.conn.Caps().Has(imap.CapIdle)
}

// Idle 阻塞等待服务器通告邮箱变化（EXISTS = 有新邮件）或超时。
//
// 唤醒机制：go-imap beta.9 的 IdleCommand.Wait() 只在 Close()/断连时返回，服务器发
// EXISTS 并不会唤醒它；真正的通知走 Options.UnilateralDataHandler.Mailbox 回调（见
// login），回调里发信号，这里收到后用 DONE 正常结束 IDLE。不设回调时 IDLE 形同虚设：
// 只有兜底定时器/手动收取能带来拉取，新邮件最多晚 25 分钟才入库。
//
// 返回 true = 收到变化信号（应做一次增量拉取）；false = 超时。
// ctx 取消：结束 IDLE 并返回 ctx.Err()，连接保持可用（手动收取/兜底拉取在同一连接上
// 进行）。任何错误都意味着连接已不可信，调用方应重新连接。
func (g *goClient) Idle(ctx context.Context, timeout time.Duration) (bool, error) {
	// 握手（发出 IDLE → 等服务器 "+ idling"）由看门狗保护。
	wd := g.startWatch(ctx, g.conn)
	cmd, err := g.conn.Idle()
	wd.stop()
	if err != nil {
		return false, wd.decorate(fmt.Errorf("IDLE: %w", err))
	}
	// 等待期间**不**判无响应（IDLE 本来就是安静地等），连接失效由 TCP keepalive、
	// timeout 上限和 finishIdle 的收尾上限兜住。
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-g.unilateral: // 服务器通告邮箱变化（EXISTS：有新邮件）
		if err := g.finishIdle(cmd, done); err != nil {
			return false, err
		}
		return true, nil
	case err := <-done: // 连接断开等：IDLE 被迫结束
		return false, err
	case <-ctx.Done(): // 外部终止（手动收取/兜底/停机）
		_ = g.finishIdle(cmd, done)
		return false, ctx.Err()
	case <-timer.C:
		if err := g.finishIdle(cmd, done); err != nil {
			return false, err
		}
		return false, nil
	}
}

// finishIdle 正常结束 IDLE：发 DONE、等服务器确认；成功后连接照常可用。返回前清掉等待
// 和收尾期间累积的信号——调用方紧接着都会做一次增量拉取，信号要表达的"有新邮件"已被
// 这次拉取覆盖，留着只会让下一次 IDLE 立刻空转返回。
func (g *goClient) finishIdle(cmd *imapclient.IdleCommand, done <-chan error) error {
	if err := cmd.Close(); err != nil { // 写 DONE
		_ = g.conn.Close() // 写失败：连接已不可信，强制断开让 Wait 返回
		return fmt.Errorf("结束 IDLE: %w", err)
	}
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("结束 IDLE: %w", err)
		}
	case <-time.After(idleStopTimeout):
		// DONE 已发出但服务器不回 tagged OK：连接实际已失效（对端无响应），
		// 不能让 worker 在这里等到天荒地老。
		_ = g.conn.Close()
		<-done
		return errors.New("IMAP 服务器未确认 IDLE 结束，已断开")
	}
	select {
	case <-g.unilateral:
	default:
	}
	return nil
}

// rawBytes 取第一个 BODY[] 段的字节。
func rawBytes(buf *imapclient.FetchMessageBuffer) []byte {
	for i := range buf.BodySection {
		if buf.BodySection[i].Section != nil &&
			buf.BodySection[i].Section.Specifier == imap.PartSpecifierNone &&
			len(buf.BodySection[i].Section.Part) == 0 {
			return buf.BodySection[i].Bytes
		}
	}
	if len(buf.BodySection) > 0 {
		return buf.BodySection[0].Bytes
	}
	return nil
}

// Logout 正常断开。服务器不回时 Wait 会一直阻塞（同样是"等下一个响应"），而这是每个
// 会话结束都要走的收尾路径——卡住就等于 worker 卡住，所以设上限，超时直接断开。
func (g *goClient) Logout() error {
	if g.conn == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- g.conn.Logout().Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(logoutTimeout):
		_ = g.conn.Close()
		return fmt.Errorf("LOGOUT 超过 %s 未完成，已断开", logoutTimeout)
	}
}

func (g *goClient) Close() error {
	if g.conn == nil {
		return nil
	}
	return g.conn.Close()
}

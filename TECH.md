# MailHub 技术选型与模块设计（实施设计）

依据 DESIGN.md（需求与架构契约），本文**确定**具体技术栈与代码模块设计，是实现的直接依据。章节引用均指 DESIGN.md。

## 1. 技术选型（已确定）

### 1.1 后端

| 项 | 选择 | 理由 |
|---|---|---|
| 语言 | **Go ≥ 1.22**（随上游稳定版升级） | 千级 IMAP 长连接的调度与内存占优；单二进制部署。1.22 的标准库路由增强与 loopvar 语义对本项目都有直接价值 |
| IMAP | `github.com/emersion/go-imap/v2` | IDLE、UID 增量、`SelectOptions.ReadOnly`（即 EXAMINE） |
| SASL | `github.com/emersion/go-sasl` | PLAIN（密码）与 OAuth2（XOAUTH2）两种登录 |
| 邮件解析 | `github.com/jhillyerd/enmime` | 从原始字节流解析 MIME/编码/附件 |
| HTTP 框架 | `github.com/go-chi/chi/v5` | 轻量、与 `net/http` 完全兼容、中间件链（auth/log/recover）；不引反射型框架 |
| SQLite 驱动 | `modernc.org/sqlite` | **纯 Go 无 CGO**——保住交叉编译与单二进制，这是选 Go 的核心收益；低写入量下性能足够 |
| PG 驱动 | `github.com/jackc/pgx/v5`（stdlib 适配） | 经 `database/sql` 接口接入，与 SQLite 同层切换（2.1） |
| 受控并发 | `golang.org/x/sync/errgroup` | 投递器并发、批量验证等受控并行 |
| 日志 | 标准库 `log/slog`（JSON handler） | 零依赖结构化日志 |
| 配置 | 标准库 + 手写 `config` 包 | 仅约 12 个环境变量（9.1），不值引库 |
| 凭据加密 | 标准库 `crypto/aes` + `crypto/cipher`（GCM） | 无第三方依赖 |
| ORM | **不用** | 手写 SQL + 薄 repository（2.1"不引入 ORM 重型抽象"）；两库统一走 `database/sql` |

### 1.2 前端（M3 实现，选型先定）

| 项 | 选择 | 理由 |
|---|---|---|
| 框架 | Vue 3 + TypeScript + Vite | 轻量 SPA |
| UI 库 | **Naive UI** | `n-data-table` 内置虚拟滚动——千级账号表与邮件列表的硬需求（7） |
| 状态 | Pinia | 账号/邮件缓存、Token 管理 |
| HTTP | 原生 `fetch` 封装 | 不引 axios |
| 分发 | 构建产物经 `embed.FS` 打进 Go 二进制 | 单文件分发，前后端一体 |

### 1.3 版本与依赖策略
- `go.mod` 锁定次版本；升级走评审，不自动。
- 外部依赖总数控制在 **10 个以内**（上表所列即全部）。

## 2. 项目结构与模块设计

### 2.1 目录与依赖方向

```
mailhub/
├── cmd/mailhub/main.go          # 组装与优雅退出
├── internal/
│   ├── config/config.go         # 环境变量解析与校验（唯一入口）
│   ├── crypto/crypto.go         # AES-256-GCM：credential_enc 加解密
│   ├── store/
│   │   ├── store.go             # Open(sqlite|postgres)、迁移执行器
│   │   ├── migrations/          # NNN_name.sqlite.sql / NNN_name.postgres.sql（embed）
│   │   ├── accounts.go          # 账号 repository：分页/批量导入/凭据更新/心跳
│   │   ├── messages.go          # 邮件 repository + IngestMessage 事务 + retention 清理
│   │   ├── tags.go              # 账号标签（004）：SetAccountTags 替换/TagsForAccounts/ListTags
│   │   ├── settings.go          # 全局配置覆盖（005）：GetSettings/SetSettings 白名单 + SettingInt
│   │   ├── webhooks.go          # webhook 配置
│   │   └── deliveries.go        # 投递队列：claim / mark / 清理
│   ├── mailparse/parse.go       # 纯函数：raw → Parsed 元数据
│   ├── mstoken/mstoken.go       # Microsoft 令牌刷新与进程内缓存（scope 参数化：IMAP/Graph，DESIGN 3.5/3.6）
│   ├── mailclient/mailclient.go # 统一传输抽象：MailClient 接口 + MsgKey + Credentials
│   ├── imapclient/
│   │   ├── client.go            # go-imap 实现（EXAMINE + BODY.PEEK，3.1）
│   │   └── oauth.go             # XOAUTH2 SASL（go-sasl 未提供，自实现）
│   ├── graphclient/graph.go     # Graph API 实现（列表 + $value，Outlook 家族主传输，DESIGN 3.6）
│   ├── sync/
│   │   ├── manager.go           # 错峰队列、Reload/Trigger/Remove、SetNotifyEnabled、worker 注册表、settingInt 配置缓存（30s TTL）
│   │   ├── worker.go            # 单账号状态机（run→session→idleLoop/pollOnce，3.2）
│   │   └── budget.go            # 单主机 IDLE 预算（2.2；limit 动态：Acquire(host, limit)，每次会话从配置读取）
│   ├── notify/deliverer.go      # 投递器：扫描/并发/HMAC/重试（5）+ 投递记录 30 天自清
│   ├── maintain/maintain.go     # 每日维护：retention 清理 + 孤儿 .eml/.part 回收（9.1）
│   ├── api/
│   │   ├── router.go            # chi 路由与中间件（/api/v1/events 在 auth 外，?token= 单独认证）
│   │   ├── auth.go              # Bearer 常量时间比较
│   │   ├── events.go            # SSE Hub 与事件流（M4：入库 Wake → message.new）
│   │   ├── settings.go          # 全局配置 API（M4：GET/PUT /settings，DB 覆盖 > env；PUT 后 RefreshSettings）
│   │   └── accounts.go / messages.go / webhooks.go / status.go
│   └── web/embed.go             # M3：embed 前端 dist
├── web/                         # M3：Vue 3 源码
├── Dockerfile                   # 多阶段：node 构建前端 → go 构建 → 最小运行时
└── Makefile
```

**依赖方向（强制单向，禁止反向）**：

```
api ──▶ sync(控制面) / notify(查询) / store
sync ──▶ mailclient / imapclient / graphclient(经注入) / mailparse / store / crypto
notify ──▶ store
mailclient ──▶ store（仅 FolderState 数据结构）；imapclient / graphclient ──▶ mailclient / mstoken
mstoken / mailparse / crypto / store / config：不依赖任何上层，可独立测试
```

`dial` 构造器由 cmd/main 注入 sync 与 api：按 `acct.Transport` 返回 imapclient 或 graphclient 实现（graphclient 的去重回调闭包捕获 accountID，接线 `store.HasGraphIDs`），两包互不感知。

两个关键解耦点：
- `sync.Manager` 对 `api` 只暴露 `TriggerNow / TriggerBatch / ReloadAccount / RemoveAccount / SetNotifyEnabled / RefreshSettings` 控制面方法；
- `sync` 出站只有一个 `Notifier.Wake()` 通道（IngestMessage 提交后调用）；main 用 `notifierGroup` 把投递器与 SSE Hub 并列挂上。

### 2.2 store：事务边界与双库兼容

**迁移**：每版本两个方言文件（`NNN_name.sqlite.sql` / `NNN_name.postgres.sql`），`embed.FS` 内嵌，启动时顺序应用，`schema_migrations` 表记录已应用版本。DDL 与 DESIGN.md §4 一致。

**方言约定**：布尔一律 `INTEGER 0/1`（PG 侧同用 int）；时间一律 UTC；自增主键 SQLite 用 `INTEGER PRIMARY KEY`、PG 用 `GENERATED ALWAYS AS IDENTITY`（迁移文件各自处理，代码层无感知）。

**SQLite DSN**：`journal_mode=WAL, busy_timeout=5000, foreign_keys=on, _txlock=immediate`。`_txlock=immediate` 让事务一开始就取写锁（拿不到则等 busy_timeout）：默认的 deferred 事务是"先读后写"，WAL 下读快照建立后只要有别的连接提交过写，升级写锁就**立即**返回 `SQLITE_BUSY_SNAPSHOT`（`database is locked (517)`），busy_timeout 对它不生效——`IngestMessage` 正是"查重 SELECT → INSERT"，多 worker 并发入库会大量失败（实测 2 路并发约 3/4 失败，账号被置 error 并退避）。本包的事务全部含写，immediate 无副作用；不要改成手写重试。低写入量下 WAL 单写者不构成瓶颈（4）。

**事务边界归属 store，关键方法**：

```go
// 单封邮件入库——一个事务内完成三件事（3.3）：
//   messages 插入 + （账号 notify_enabled=1 时）匹配 webhook 的 deliveries 插入 + folder last_uid 更新
// notify 判断在调用方（sync 持有账号快照），本方法按参数生成 deliveries
func (s *Store) IngestMessage(ctx context.Context, m *NewMessage) (id int64, err error)

func (s *Store) UpdateCredential(ctx context.Context, accountID int64, enc []byte) error // 令牌轮换，独立小事务（3.5）
func (s *Store) TouchAccount(ctx context.Context, accountID int64) error                 // last_sync_at 心跳（3.2）
func (s *Store) ClaimPendingDeliveries(ctx context.Context, limit int) ([]Delivery, error) // 投递器取件（原子置 next_retry_at）
```

批量导入、分页查询等 repository 方法按 §6 契约实现，不赘述。

### 2.3 crypto

```go
func New(key []byte) (*Cipher, error)          // 32B，来自 MAILHUB_SECRET_KEY(base64)
func (c *Cipher) Encrypt(plain []byte) []byte  // nonce 前置拼接
func (c *Cipher) Decrypt(blob []byte) ([]byte, error)
```

### 2.4 传输层：mailclient / mstoken / imapclient / graphclient

`mailclient` 定义统一抽象（隔离 go-imap 与 Graph API，保证 worker 可用假实现测试）：

```go
type MsgKey struct {           // 消息唯一坐标：IMAP 填 UIDValidity/UID，Graph 填 GraphID
    UIDValidity, UID uint32
    GraphID         string
}

type FetchResult struct {      // M2：拉取结果统一携带游标推进物
    LatestSeen time.Time       // 本批最大 seenAt（Graph 时间游标；IMAP 恒零值）
    DeltaLink  string          // Graph delta 链接（IMAP 恒空）
}

type MailClient interface {
    Connect(ctx context.Context, host string, port int, user string, cred Credentials) (rotated string, err error)
    // 游标由实现解释：IMAP 校验 UIDVALIDITY→UID > last_uid；Graph 用 delta_link 滚动，
    // 无/失效时退回 last_seen_at 时间窗（再退 since 初始窗口，默认 90 天）。
    FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
        handle func(key MsgKey, seenAt time.Time, raw []byte) error) (FetchResult, error)
    SupportsIdle() bool                                    // IMAP：服务器 CAPABILITY 含 IDLE；Graph 恒 false
    Idle(ctx context.Context, timeout time.Duration) (notified bool, err error) // 单次等待，到点/通知/取消返回
    Logout() error
    Close() error
}
```

**mstoken——main.py 的移植点**（参数表见 DESIGN 3.5，scope 参数化见 3.6）：

```go
// 端点 login.microsoftonline.com/common/oauth2/v2.0/token，公共客户端，无 client_secret
// IMAPScope / GraphScope 两个 audience，缓存 key 含 scope 维度
// 返回 access_token（缓存至 exp-60s，进程内）与轮换的新 refresh_token（可为空）
func (t *TokenCache) Token(ctx context.Context, scope, email, clientID, refreshToken string) (access, rotated string, err error)
var ErrReauth = errors.New("invalid_grant")   // 由 worker 映射为 auth_failed（3.4）；Graph 401 同样映射
```

**imapclient**：go-imap v2 实现。`Select(ReadOnly)` 即 EXAMINE；`BODY.PEEK[]` 拉原文；oauth2 走自实现 XOAUTH2 SASL（go-sasl 仅有 OAUTHBEARER）。增量搜索发 `UID SEARCH UID n:*`。注意 go-imap 的 `Fetch`/`Search` 的 NumSet **必须传值类型** `imap.UIDSet`：传 `*imap.UIDSet` 能通过编译（方法集相同），运行时却 `panic: imap: invalid NumSet type`，worker 里的 panic 会让整个进程退出。

go-imap v2 beta.9 有三处缺口，由本包自己补上（升级依赖时逐条复核，`client_test.go` 的假服务器测试会兜住）：

- **IDLE 唤醒**：`IdleCommand.Wait()` 只在 `Close()`/断连时返回，服务器发 EXISTS 并不会唤醒它。构造客户端时设置 `Options.UnilateralDataHandler.Mailbox`；回调在 go-imap 的读 goroutine 上执行，必须立即返回——只向容量 1 的 channel 非阻塞发信号。`Idle` 收到信号后发 `DONE` 正常结束并返回 `notified=true`，连接保持可用（手动收取/兜底拉取都在同一连接上）。只对 `NumMessages != nil`（EXISTS）发信号，FLAGS/PERMANENTFLAGS 变化忽略；两条命令之间到达的 EXISTS 信号保留到下一次 IDLE（立即补拉）；IDLE 收尾时清掉累积的信号（随后的增量拉取已覆盖，留着会让下一次 IDLE 空转）。EXAMINE 期间的 EXISTS 归属那条 Select 命令，不触发回调。
- **看门狗（`watchdog`）**：go-imap 只在"响应已开始"之后设读超时（普通响应 30s、字面量 5min），等下一个响应的**第一个字节**没有任何超时——服务器卡死或连接被中间设备静默丢弃（无 FIN/RST）时 `Wait()` 永久阻塞，ctx 取消也无效。登录、`FetchFolder`、IDLE 握手都在看门狗监视下：持续 `stallTimeout`（2min）没收到任何数据 → 断开连接（挂起的命令随之以错误完成，`decorate` 把原因翻译成"IMAP 服务器超过 2m0s 无响应，已断开"）；ctx 取消 → 立即断开（停机/删除账号不再被卡住的连接拖住）。入库回调执行期间暂停无响应判定（`pause`/`resume`：本地慢不算服务器卡死）。IDLE **等待**阶段不设看门狗（静默是正常的），由 TCP keepalive、25 分钟上限和下面的收尾上限兜底。
- **收尾上限**：IDLE 收尾（发出 DONE 后等 tagged OK）超过 `idleStopTimeout`（10s）、`LOGOUT` 超过 `logoutTimeout`（5s）即强制断开——这两条是每个会话必经的路径，卡住等于 worker 卡住。

**登录错误分类**：只有服务器明确回了 tagged `NO`/`BAD`（`*imap.Error`）才包成 `ErrAuthFailed`（→ `auth_failed` 停止重试，DESIGN 3.4）；断连、无响应、ctx 取消、`* BYE`（限流/维护/连接数超限）一律按普通错误走退避重连。登录前先 `WaitGreeting()`，服务器以 `* BYE` 拒绝接入时能拿到原文。

**graphclient**：纯 HTTP 实现。列表 `$filter=receivedDateTime ge 游标&$orderby=asc&$select=id,receivedDateTime`（nextLink 分页），每页批量 seen 去重后逐封 `GET /me/messages/{id}/$value` 取 MIME；401→`ErrReauth`、429/5xx→普通错误走退避；仅 GET，绝不写远端（3.6）。

要点：
- **access_token 进程内缓存**（mstoken.TokenCache）：轮询模式短连接重连时直接用缓存令牌，过期前 60s 才刷新——不重蹈 main.py 每次运行必刷新的覆辙（它是短命进程，我们是常驻服务）。
- 轮换的 refresh_token 由 worker 拿到后**立即** `UpdateCredential`，不等邮件事务；同时更新 worker 的内存快照 `acct.CredentialEnc`（下个会话 `credentials()` 解密的是它）。只写库不更新快照的话 worker 一直拿旧令牌，与 TokenCache 里存的新令牌对不上，缓存永不命中，每个轮询周期都会再打一次令牌端点。
- 刷新在 TokenCache 锁内串行（账号级互斥，3.5）。
- 令牌端点 429 → 返回错误，走 worker 统一退避。

### 2.5 mailparse（纯函数）

```go
type Parsed struct {
    MessageID, Subject, FromAddr string
    ToAddrs, CcAddrs []string
    SentAt time.Time
    BodyText, BodyHTML, Snippet string
    Size int64
    Attachments []Attachment
    Truncated bool
    ParseError string                            // 置位时其余字段尽力填充，raw 照常保存（3.4）
}
func Parse(raw []byte, maxSize int64) Parsed
```

### 2.6 sync：Manager 与 worker

```go
type Manager struct{ /* store、imapclient 构造器、budget、stagger 队列、worker 注册表 */ }
func (m *Manager) Run(ctx context.Context) error          // 加载 enabled 账号 → 错峰队列逐个 spawn
func (m *Manager) ReloadAccount(ctx context.Context, id int64) // PATCH 后：cancel 旧 worker → 入队重启
func (m *Manager) RemoveAccount(id int64)                 // DELETE：cancel worker、释放预算
func (m *Manager) TriggerNow(id int64)                    // 单账号：向 worker.trigger 非阻塞发送（立即）
func (m *Manager) TriggerBatch(ids []int64)               // 批量：进错峰队列（优先于待启动账号）
func (m *Manager) SetNotifyEnabled(id int64, on bool)      // PATCH notify_enabled：更新运行中 worker 的开关，不重启
```

**错峰队列**：单消费者 goroutine，按 `MAILHUB_CONNECT_STAGGER` + 每账号随机抖动逐个 spawn；手动批量请求插队首（6.2 语义）。

**IDLE 预算（budget.go）**：`map[host]计数` + mutex；`Acquire(host, MAILHUB_IDLE_BUDGET_PER_HOST)` 失败 → 该账号本轮直接进轮询模式；worker 退出时 `Release`。

**worker 状态机**（对应 3.2；M2 起为 **session 模型**——run 反复建立"一次连接生命周期"，按传输分流）：

```
run(ctx):
  backoff := 5s
  loop:
    err := session(ctx)
    if 认证错误(含 ErrReauth): status=auth_failed; return     // 停止重试（3.4）
    err == nil → backoff=5s；周期等待(interval+1/4 抖动 | trigger | ctx.Done) → loop
    其他错误   → status=error; sleep backoff（×2 封顶 5min）; continue

session(ctx):                    // 一次连接生命周期
  Connect()                      // oauth2：刷新令牌；轮换 → 立即 UpdateCredential + 更新内存快照
  if canIdle(服务器支持 && 单文件夹 && budget.Acquire(host)):
      defer budget.Release; idleLoop(ctx)                     // IDLE 会话：阻塞至断线/停止
  else:
      pollOnce(ctx); Logout()                                 // 轮询会话：短连接单次收取即返回

idleLoop(ctx):                   // 长连接会话（3.2 状态机）
  pollOnce()                     // 初始拉取（含首同步历史邮件标记）
  loop:
    select:
      idleNotify   → fetchIncremental → 重新 IDLE             # EXISTS = 有新邮件（imapclient 经 UnilateralDataHandler 唤醒，2.4）
      25m 超时     → fetchIncremental → 重新 IDLE             # 协议 29 分钟内刷新要求；顺带心跳
      fallbackTick → fetchIncremental                          # FALLBACK_INTERVAL 兜底（防通知丢失）
      trigger      → 退出 IDLE → fetch → 重新 IDLE             # 手动：不重置任何计时（3.2）
      ctx.Done     → return
    fetch 错误 → return err（交 run 退避重连）

pollOnce(ctx):                   // 各文件夹增量拉取 + TouchAccount 心跳 + status=polling
```

**手动触发合并**：`trigger chan struct{}` 容量 1，发送方非阻塞（select+default 丢弃）——正在拉取时到达的信号天然合并，不排队。

**提醒开关的实时值**：入库时读的是 `worker.notify`（`atomic.Bool`），不是 `acct.NotifyEnabled`——`acct` 是 worker 启动时的账号快照，改了库里的值它不会变。`spawn` 时初始化；PATCH `notify_enabled`（单个/批量）后 API 调 `Manager.SetNotifyEnabled` 即时更新，不重启 worker（不打断 IDLE 和进行中的拉取）。worker 未运行（禁用、`auth_failed`、排队中）时是空操作，它下次启动从库读取。

**fetchIncremental 内部**：`FetchSince` → 逐封 `mailparse.Parse` → 写 `.eml` 到 `data/mails/...` → `IngestMessage`（每封一事务，3.3）→ 提交后 `notify.Wake()`；UIDVALIDITY 变化按 3.4 归零重拉。

### 2.7 notify

```go
func (d *Deliverer) Run(ctx context.Context) error   // 循环：Claim → 并发投递 → 无任务时 select{wake, tick(10s)}
func (d *Deliverer) Wake()                           // sync 在 IngestMessage 提交后调用
```

- 并发：errgroup + 信号量 `MAILHUB_WEBHOOK_CONCURRENCY`；单请求超时 10s。
- 签名：`ts := now.Unix()`；`HMAC_SHA256(secret, ts + "." + body)`；头按 5.2。
- 重试梯度 10s/1m/5m/30m/2h × 5 次 → `failed`（5.3）；`Claim` 时预写 `next_retry_at` 防止并发重复取件。
- 投递记录 30 天自清由 Deliverer 内部 tick（6h）执行；**邮件 retention 与孤儿 .eml/.part 回收由 `maintain.Run` 每日 tick 执行**（启动错开 1 分钟，`Once` 可单测）。

### 2.8 api

- chi 子路由按 §6 分组：`/api/v1/accounts`、`/api/v1/messages`、`/api/v1/webhooks`、`/api/v1/status`、`/healthz`（免认证）。
- 中间件顺序：RequestID → slog Logger → Recover → Auth。
- Auth：`subtle.ConstantTimeCompare` 比较 Bearer Token。
- 统一分页 helper（page/limit/total）、统一错误体 `{"error":{"code","message"}}`。
- 批量导入 handler：`Content-Type: text/plain` 时逐行 `----` 解析（容错：行号+原因回传，6.2）。
- M3：`internal/web` embed 的 SPA 挂在根路径，API 之外 fallback `index.html`。

### 2.9 组装与数据流（cmd/mailhub/main.go）

```
启动顺序：config 校验 → store.Open(+迁移) → crypto → notify.Deliverer.Run(goroutine)
        → sync.Manager.Run(goroutine) → api.ListenAndServe(阻塞)
优雅退出：SIGTERM → ctx cancel → Manager 停 worker → Deliverer 完成在途批次 → db.Close
```

```
IMAP 服务器 ──▶ imapclient ◀── sync.worker（每账号一个）
                   │ raw            │ Connect(oauth)：轮换令牌 → store.UpdateCredential
                   ▼                │ FetchSince 成功 → store.TouchAccount（心跳）
              mailparse.Parse       ▼
                   └────▶ store.IngestMessage（事务：message＋notify 匹配的 deliveries＋last_uid）
                                  │ commit → notify.Wake()
                                  ▼
                     notify.Deliverer ──HTTP POST＋HMAC──▶ 下游 Webhook
浏览器 ──▶ api(chi) ──▶ store（查询） ；api ──Trigger/Reload──▶ sync.Manager（控制面）
```

## 3. 错误处理与可观测性

- **错误三分类**（与 3.4 对应）：连接类（重试+退避）、认证类（停，`auth_failed`）、解析类（入库但标记 `parse_error`）。分界线只有一条：**服务器明确拒绝了凭据**才算认证类。IMAP 登录即 tagged `NO`/`BAD`（2.4）；断连、无响应、`* BYE`、ctx 取消都是连接类——误判成认证类会把账号停在 `auth_failed`，重启也不会自动恢复。
- slog JSON 字段约定：`account_id`、`host`、`worker_event`（spawn/idle/polling/backoff/stop）、`delivery_id`、`error`。账号状态迁移必须留痕。
- 心跳与巡检不靠日志，靠 `GET /api/v1/status` 与账号列表（6.5、7）。

## 4. 测试策略

| 层 | 方式 |
|---|---|
| mailparse | 真实 `.eml` 样本表驱动（含乱码、超大截断、坏格式） |
| store | SQLite `:memory:` 跑全部迁移 + repository/事务测试；并发入库（多 goroutine 同时 `IngestMessage` 不得出现 `database is locked`）、外键子表索引存在（006 迁移） |
| sync/worker | 注入假 `Client`：IDLE 通知、兜底触发、手动触发合并、退避、auth_failed 即停、UIDVALIDITY 重置；提醒开关运行时切换（不重启 worker）、令牌轮换后的内存快照 |
| imapclient | `net.Pipe` 假服务器，断言线上命令（`EXAMINE`/`BODY.PEEK[]`、`UID SEARCH UID n:*`）：登录错误分类（拒绝 / 断连 / 无响应 / `* BYE`）、增量与分批拉取、IDLE 被 EXISTS 唤醒与收尾上限、看门狗（服务器静默、ctx 取消）、LOGOUT 上限 |
| mstoken | `httptest` 模拟令牌端点：轮换返回、缓存命中、scope 隔离、invalid_grant、429 |
| graphclient | `httptest` 模拟 Graph：分页列表、seen 去重、`$value` 原文、401→ErrReauth、游标参数透传 |
| notify | `httptest` 收投递：断言签名头、重试梯度（注入时钟） |
| api | `httptest` 全链路（内存库）：认证、分页、批量导入容错；提醒开关 PATCH → **运行中**的 worker（真实 Manager，验证控制面确实传到了 worker） |
| 集成冒烟（手动） | 对单个真实测试邮箱跑完整收信 → webhook |

超时/唤醒这类"不发生某件事"的缺陷，测试的有效性用**注入缺陷**验证：把看门狗关掉、去掉收尾上限、删掉唤醒回调……逐个改坏后对应测试必须失败（改坏后"只在超时后失败"的，测试自带 5s 守卫）。新增超时逻辑时照此做一遍，否则很容易写出永远通过的测试。

## 5. 构建与部署

- `make build`：`go build -o bin/mailhub ./cmd/mailhub`（M3 起先 `npm --prefix web run build`）。
- Dockerfile 多阶段（已交付）：`node:20-alpine` 构建前端 → `golang:1.27-alpine` 静态构建 → `gcr.io/distroless/static-debian12:nonroot` 运行（纯 Go 驱动无 CGO；distroless/static 自带 CA 证书，Graph/token 端点 HTTPS 必需）。
- `docker compose up -d --build`：凭据经根目录 `.env`（`KEY=value`，无 export 前缀）注入；`docker-compose.yml` 已含 nofile 8192 与 `/data` 卷。
- 运行时只需 `data/` 卷与 9.1 环境变量；`LimitNOFILE ≥ 4096`（9.2）。

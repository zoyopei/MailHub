# MailHub 技术选型与模块设计（实施设计）

依据 DESIGN.md（需求与架构契约），本文**确定**具体技术栈与代码模块设计，是实现的直接依据。章节引用的约定：形如"3.4""5.1""9.1"的指 DESIGN.md 的章节；指向本文自己的章节时写作"本文 x.y"。

本文描述的是**代码当前的实际状态**，包括还没做到的部分（例如 PostgreSQL 未接入，见本文 1.1）；与代码不一致时以代码为准，并回头改这里。

## 1. 技术选型（已确定）

### 1.1 后端

| 项 | 选择 | 理由 |
|---|---|---|
| 语言 | **Go ≥ 1.27**（`go.mod` 锁定 1.27.1，随上游稳定版升级） | 千级 IMAP 长连接的调度与内存占优；单二进制部署。标准库路由增强与 loopvar 语义对本项目都有直接价值 |
| IMAP | `github.com/emersion/go-imap/v2` | IDLE、UID 增量、`SelectOptions.ReadOnly`（即 EXAMINE） |
| SASL | **不直接依赖** | 密码登录用 go-imap 内置的 `LOGIN`；XOAUTH2 在 `imapclient/oauth.go` 自实现（go-imap 带的 SASL 机制只有 PLAIN / OAUTHBEARER，Outlook 要的是 XOAUTH2；`go-sasl` 只是 go-imap 的间接依赖，本项目代码不 import 它） |
| 邮件解析 | `github.com/jhillyerd/enmime` | 从原始字节流解析 MIME/编码/附件 |
| HTTP 框架 | `github.com/go-chi/chi/v5` | 轻量、与 `net/http` 完全兼容、中间件链（auth/log/recover）；不引反射型框架 |
| SQLite 驱动 | `modernc.org/sqlite` | **纯 Go 无 CGO**——保住交叉编译与单二进制，这是选 Go 的核心收益；低写入量下性能足够 |
| PG 驱动 | **尚未接入**（计划 `github.com/jackc/pgx/v5` 的 stdlib 适配） | 目标是经 `database/sql` 接入、与 SQLite 同层切换。现状见本节末尾 |
| 受控并发 | 标准库（`sync.WaitGroup` + 带缓冲 channel 作信号量） | 需要限制并发的只有投递器一处，不值得为此引依赖 |
| 日志 | 标准库 `log/slog`（默认 handler，**文本格式**） | 零依赖的结构化日志（`key=value` 字段）；没有配置 JSON handler，日志收集端按文本解析或自行换 handler |
| 配置 | 标准库 + 手写 `config` 包 | 仅 13 个环境变量（9.1），不值引库 |
| 凭据加密 | 标准库 `crypto/aes` + `crypto/cipher`（GCM） | 无第三方依赖 |
| ORM | **不用** | 手写 SQL + 薄 repository（2.1"不引入 ORM 重型抽象"）；统一走 `database/sql`（接入 PG 后两种方言共用这一层） |

**PostgreSQL 现状——接入远不止"把驱动接上"**。`store.Open` 对非空的 `MAILHUB_DATABASE_URL` 直接返回
`ErrPostgresNotEnabled`，服务启动失败。要真正接入，至少要做完这些（**没有一项验证过**，动手前先搭测试库）：

1. 引入 `github.com/jackc/pgx/v5`，`Open` 里对非空 DSN 走 `sql.Open("pgx", url)`。
2. **占位符**：store 包里约 190 处 SQL 占位符全是 `?`（modernc sqlite 支持），pgx 要 `$1`、`$2`——逐条改写，或引入占位符重写层。
3. **迁移执行器**只做了 SQLite 一半：`//go:embed migrations/*.sqlite.sql` 只内嵌 SQLite 方言，仓库里的
   `*.postgres.sql` **没有任何代码加载、从未执行过**；需要按方言选文件。现在的做法是把整个文件一次性
   `Exec`（多条语句），在 pgx 下是否可行没有验证过，稳妥的做法是拆成单条语句逐条执行。
4. `schema_migrations` 的建表用了 `DATETIME`（PG 没有这个类型，应为 `TIMESTAMP`），写入用 `datetime('now')`（SQLite 函数，PG 是 `now()`）。
5. 散落的 SQLite 专有写法：`AdvanceFolderUID` 里的两参数标量 `MAX(a, b)`（PG 是 `GREATEST`）、`PRAGMA` 调用（仅 SQLite 路径）。
6. 在 PG 上跑通 `store` 包的全部测试（含并发入库），再放开 `MAILHUB_DATABASE_URL`。

在此之前请用 SQLite（它覆盖全部规划规模，DESIGN 2.1）。

### 1.2 前端（M3 实现，选型先定）

| 项 | 选择 | 理由 |
|---|---|---|
| 框架 | Vue 3 + TypeScript + Vite | 轻量 SPA |
| UI 库 | **Naive UI** | 账号表与投递记录用 `n-data-table`（设 `max-height` 内部滚动 + 服务端分页，**没有开虚拟滚动**）；邮件列表是自绘行 + 无限滚动（`MailView.vue`），列表逻辑抽在 `web/src/lib/maillist.ts`。邮件页账号侧栏与 Webhook 页的账号范围下拉都用 `fetchAllPages`（同一个文件）逐页取齐全部账号——一页最多 200 个，只取第一页会让排在后面的账号选不到 |
| 状态 | **不引 Pinia**：组件内 `ref`/`reactive`，可测的纯逻辑抽到 `web/src/lib/` | 五个视图互不共享状态；API Token 与少量界面偏好（列宽、通知开关）存 localStorage |
| HTTP | 原生 `fetch` 封装 | 不引 axios |
| 分发 | 构建产物经 `embed.FS`（`web/embed.go`，`//go:embed all:dist`）打进 Go 二进制 | 单文件分发，前后端一体 |
| 测试 | Node 内置测试运行器（`npm test` → `node --test "tests/**/*.test.ts"`） | 只测 `src/lib/` 的纯逻辑（列表查询/合并/锚点/翻页、邮件 HTML 预处理、`day_start` 计算），不引 vitest/jest |

**邮件 HTML 正文的渲染**（`web/src/lib/mailhtml.ts`，纯函数）：正文是不可信的第三方内容，放进 `sandbox` iframe（`srcdoc`，不带 `allow-*`）渲染，渲染前做两件事——① 摘除正文里的 `<meta>` 与 `<base>`（规范上框架**自身**的 meta refresh 不受 sandbox 与 CSP 约束；实测 Chrome 157 会拦，但不拿它当跨浏览器保证）；② 在 srcdoc 文档里自带 CSP：默认 `default-src 'none'`（只留内联样式与 `data:` 图片；远程图片一律不加载，`<img>` 的 `src` 被摘掉并标"图片已屏蔽"，防追踪像素），用户点"显示图片"后换成只放开图片/音视频/字体的策略——脚本、表单、框架、外链样式表照旧禁止。视图之间靠 `n-menu` 切换，没有 URL 路由。

### 1.3 版本与依赖策略
- `go.mod` 锁定次版本；升级走评审，不自动。
- 外部依赖总数控制在 **10 个以内**。现状：Go 直接依赖 4 个（go-imap v2、chi v5、enmime、modernc sqlite），前端运行时 2 个（vue、naive-ui）；上表标"不用 / 不直接依赖 / 尚未接入"的不计。

## 2. 项目结构与模块设计

### 2.1 目录与依赖方向

```
mailhub/
├── cmd/mailhub/main.go          # 组装与优雅退出
├── internal/
│   ├── config/config.go         # 环境变量解析与校验（唯一入口）
│   ├── crypto/crypto.go         # AES-256-GCM：credential_enc 加解密
│   ├── store/
│   │   ├── store.go             # Open（仅 SQLite；非空 MAILHUB_DATABASE_URL 返回 ErrPostgresNotEnabled）、迁移执行器
│   │   ├── migrations/          # 001–008，每版两个方言文件；执行器只内嵌 *.sqlite.sql
│   │   ├── accounts.go          # 账号 repository：分页/批量导入/凭据更新/心跳
│   │   ├── messages.go          # 邮件 repository + IngestMessage 事务 + folder_states 游标推进 + retention 清理
│   │   ├── tags.go              # 账号标签（004）：SetAccountTags 替换/TagsForAccounts/ListTags
│   │   ├── settings.go          # 全局配置覆盖（005）：GetSettings/SetSettings 白名单 + SettingInt
│   │   ├── webhooks.go          # webhook 配置
│   │   └── deliveries.go        # 投递队列：ListDueDeliveries / mark / RetryDelivery / 终态记录清理
│   ├── mailparse/parse.go       # 纯函数：raw → Parsed 元数据
│   ├── mstoken/mstoken.go       # Microsoft 令牌：刷新与进程内缓存（scope 参数化：IMAP/Graph，DESIGN 3.5）、授权码换令牌与 id_token 身份解析（重新获取令牌）
│   ├── mailclient/mailclient.go # 统一传输抽象：MailClient 接口 + MsgKey + Credentials + FetchResult + SessionTokenRotator
│   ├── imapclient/
│   │   ├── client.go            # go-imap 实现：EXAMINE + BODY.PEEK、STARTTLS / 隐式 TLS、看门狗、IDLE 唤醒（本文 2.4）
│   │   └── oauth.go             # XOAUTH2 SASL（go-sasl 未提供，自实现）
│   ├── graphclient/graph.go     # Graph API 实现（列表 + $value，Outlook 家族主传输，DESIGN 3.6）
│   ├── sync/
│   │   ├── manager.go           # 错峰队列、控制面（Reload/Trigger/Remove/SetNotifyEnabled/RefreshSettings）、worker 注册表、全局配置缓存（30s TTL）；出站接口 Notifier / EventNotifier
│   │   ├── worker.go            # 单账号状态机（run→session→idleLoop/pollOnce→fetchFolder/ingest；首次同步与历史邮件判定，3.2/3.4）
│   │   └── budget.go            # 单主机 IDLE 预算（2.2；limit 动态：Acquire(host, limit)，每次会话从配置读取）
│   ├── notify/deliverer.go      # 投递器：扫描/并发/HMAC 签名/重试/测试事件（5）
│   ├── maintain/maintain.go     # 每日维护：retention 清理 + 孤儿 .eml/.part 回收 + 终态投递记录清理（9.1）
│   └── api/
│   │   ├── router.go            # chi 路由、Bearer 认证、安全头、访问日志、内嵌 Web UI（/api/v1/events 在 auth 外，?ticket= / ?token= 单独认证）
│   │   ├── events.go            # SSE：Hub（事件广播、一次性票据、Close 结束全部流）与事件流
│   │   ├── settings.go          # 全局配置 API（M4：GET/PUT /settings，DB 覆盖 > env；PUT 后 RefreshSettings）
│   │   └── accounts.go / messages.go / webhooks.go / status.go
├── web/                         # Vue 3 源码与内嵌（package web 无 Go 依赖）
│   ├── embed.go                 # //go:embed all:dist
│   ├── src/views/               # 五个视图：Mail / Accounts / Dashboard / Webhook / Settings
│   ├── src/lib/                 # 纯逻辑（maillist / mailhtml / daystart），可直接单测
│   └── tests/                   # node --test 单测
├── Dockerfile                   # 多阶段：node 构建前端 → go 构建 → 最小运行时
├── docker-compose.yml           # 单服务 + named volume；凭据经 .env 注入
└── Makefile
```

**依赖方向（强制单向，禁止反向）**：

```
cmd/mailhub ──▶ 所有包（唯一同时 import imapclient / graphclient / notify / maintain / api 的地方）
api ──▶ sync(控制面) / store / crypto / mailclient(仅 dial 签名) / mstoken(重新获取令牌)；不 import notify
sync ──▶ mailclient / mailparse / store / crypto / mstoken(仅 ErrReauth) / imapclient(仅 ErrAuthFailed)；不 import graphclient
notify / maintain ──▶ store / config
mailclient ──▶ store（仅 FolderState 数据结构）；imapclient / graphclient ──▶ mailclient / mstoken / store（仅 FolderState）
mstoken / mailparse / crypto / store / config：不依赖任何上层，可独立测试；web 包只有 embed，无 Go 依赖
```

`dial` 构造器由 cmd/main 注入 sync 与 api：按 `acct.Transport` 返回 imapclient 或 graphclient 实现（graphclient 的去重回调闭包捕获 accountID，接线 `store.HasGraphIDs`），两包互不感知。`mstoken.TokenCache` 进程级一份，传给两个实现：同一账号的 access_token 按 scope 分别缓存、各自复用。

三个关键解耦点：
- `sync.Manager` 对 `api` 只暴露控制面方法：`TriggerNow / TriggerBatch / ReloadAccount / ReloadBatch / RemoveAccount / SetNotifyEnabled / RefreshSettings`（`Run` 只由 main 调）；
- `sync` 出站只有两个接口：`Notifier.Wake()`（每封邮件入库提交后调用，投递器实现——它只需要"有活干了"）与 `EventNotifier.NotifyEvent(data)`（只对**非历史**的新邮件调用，带单封摘要载荷，`api.Hub` 实现）。main 把 `deliverer` 作为 Notifier、`hub` 作为 EventNotifier 分别注入；
- `api` 同样不 import `notify`：Webhook 测试事件与手动重发后的唤醒经 `WebhookTester` 接口和 `wake func()` 注入（`SetDeliverer`）。

### 2.2 store：事务边界与方言隔离（PostgreSQL 现状见本文 1.1）

**迁移**：每版本两个方言文件（`NNN_name.sqlite.sql` / `NNN_name.postgres.sql`），启动时按文件名顺序应用（每个文件一个事务，版本号 = 文件名），`schema_migrations` 表记录已应用版本。**执行器目前只内嵌 `*.sqlite.sql`**——PG 方言文件从未被任何代码加载过（本文 1.1）。DDL 与 DESIGN.md §4 一致。

**方言约定**：布尔一律 `INTEGER 0/1`（PG 侧同用 int）；时间一律 UTC；自增主键 SQLite 用 `INTEGER PRIMARY KEY`、PG 用 `GENERATED ALWAYS AS IDENTITY`（迁移文件各自处理，接入后代码层无感知——PG 方言的迁移文件已按这个约定写好，但从未被执行过）。

**SQLite DSN**：`file:<dataDir>/mailhub.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_txlock=immediate`（WAL 在打开后用 `PRAGMA journal_mode=WAL` 设置，持久化到库文件；连接池上限 8）。`_txlock=immediate` 让事务一开始就取写锁（拿不到则等 busy_timeout）：默认的 deferred 事务是"先读后写"，WAL 下读快照建立后只要有别的连接提交过写，升级写锁就**立即**返回 `SQLITE_BUSY_SNAPSHOT`（`database is locked (517)`），busy_timeout 对它不生效——`IngestMessage` 正是"查重 SELECT → INSERT"，多 worker 并发入库会大量失败（实测 2 路并发约 3/4 失败，账号被置 error 并退避）。本包的事务全部含写，immediate 无副作用；不要改成手写重试。低写入量下 WAL 单写者不构成瓶颈（4）。

**事务边界归属 store，关键方法**：

```go
// 单封邮件入库——一个事务内完成三件事（3.3）：
//   ① messages（+ attachments）插入；唯一键已存在、或重拉时同账号同文件夹已有同一 Message-ID → ErrDuplicate、整个事务回滚
//   ② m.NotifyEnabled 时，为每个匹配（enabled 且 account_ids 为空或含本账号）的 webhook 插入 pending 的 deliveries
//      ——"账号开了提醒且不是历史邮件"的判断在调用方（worker），本方法只认参数
//   ③ folder_states upsert：行不存在则建；UIDVALIDITY 相同则 last_uid 只增不减；变化则归零重写
// notify 判断在调用方（sync 持有账号快照），本方法按参数生成 deliveries
func (s *Store) IngestMessage(m *NewMessage) (id int64, err error)

func (s *Store) UpdateCredential(id int64, enc []byte) error // 只写凭据列（令牌轮换 / 重新获取令牌），不动账号状态
func (s *Store) TouchAccount(id int64) error                 // last_sync_at 心跳（3.2）
func (s *Store) ListDueDeliveries(limit int) ([]*Delivery, error) // 投递器取件：只读查询，不预写（单进程唯一消费者，见本文 2.7）

// 游标推进——都由 worker 在一个文件夹**完整**拉取成功之后调用，与 IngestMessage 分属不同事务：
// 进程若在"入库"与"推进游标"之间崩溃，下一轮只会重拉，重复邮件由唯一索引 / graph_id 幂等吸收。
func (s *Store) AdvanceFolderUID(accountID int64, folder string, uidv, lastUID uint32) error // IMAP：建行 / 只增不减 / UIDVALIDITY 变化则重写
func (s *Store) TouchFolderSeen(accountID int64, folder string, t time.Time) error            // Graph：last_seen_at 只增不减
func (s *Store) SetFolderDelta(accountID int64, folder, link string) error                    // Graph：delta_link（upsert，空文件夹也能建行）
func (s *Store) BeginFolderSync(accountID int64, folder string) error                         // 首次同步**开始前**：建全零行并置 initial_done = 0
func (s *Store) FinishFolderSync(accountID int64, folder string) error                        // 首次同步走完：initial_done = 1
```

批量导入、分页查询等 repository 方法按 §6 契约实现，不赘述。

### 2.3 crypto

```go
func New(key []byte) (*Cipher, error)          // 32B，来自 MAILHUB_SECRET_KEY(base64)
func (c *Cipher) Encrypt(plain []byte) ([]byte, error) // nonce 前置拼接
func (c *Cipher) Decrypt(blob []byte) ([]byte, error)
```

### 2.4 传输层：mailclient / mstoken / imapclient / graphclient

`mailclient` 定义统一抽象（隔离 go-imap 与 Graph API，保证 worker 可用假实现测试）：

```go
type MsgKey struct {           // 消息唯一坐标：IMAP 填 UIDValidity/UID，Graph 填 GraphID
    UIDValidity, UID uint32
    GraphID         string
}

type FetchResult struct {      // 拉取结果统一携带游标推进物；worker 在整轮成功后据此推进 folder_states
    LatestSeen  time.Time   // 本批最大 receivedDateTime（Graph 时间游标，delta 链接失效时的兜底；IMAP 恒零值）
    UIDValidity uint32     // IMAP：文件夹当前 UIDVALIDITY（Graph 恒 0）
    LastUID     uint32     // IMAP：本轮结果的最大 UID。一封都没拉到时——首次同步取"文件夹里现有邮件的最大 UID"（建游标），增量路径为 0（不动游标）
    DeltaLink   string     // Graph delta 链接（IMAP 恒空）。列表没走完时为空；delta 没走完时是尚未走完的 nextLink
    More        bool       // 本轮因单轮分页上限收工、还没走完（目前只有 Graph 时间窗列表会置位）
}

type MailClient interface {
    Connect(ctx context.Context, host string, port int, user string, cred Credentials) (rotated string, err error)
    // 游标由实现解释：IMAP 校验 UIDVALIDITY→UID > last_uid；Graph 优先用 delta_link 滚动，
    // 无/失效时退回 last_seen_at 时间窗（再退 since 初始窗口，默认 90 天）。
    FetchFolder(ctx context.Context, folder string, cursor *store.FolderState, since time.Time,
        handle func(key MsgKey, seenAt time.Time, raw []byte) error) (FetchResult, error)
    SupportsIdle() bool                                    // IMAP：服务器 CAPABILITY 含 IDLE；Graph 恒 false
    Idle(ctx context.Context, timeout time.Duration) (notified bool, err error) // 单次等待，到点/通知/取消返回
    Logout() error
    Close() error
}

// 可选能力：一次会话**途中**刷新了令牌、且端点轮换了 refresh_token 的传输（graphclient）实现它；
// worker 在会话结束时取走新令牌持久化。Connect 的返回值只覆盖连接那一刻的轮换。
type SessionTokenRotator interface {
    TakeRotatedToken() string
}
```

**mstoken——main.py 的移植点**（参数表见 DESIGN 3.5，scope 参数化见 3.6）：

```go
// 端点 login.microsoftonline.com/common/oauth2/v2.0/token，公共客户端，无 client_secret
// IMAPScope / GraphScope 两个 audience，缓存 key 含 scope 维度
// 返回 access_token（缓存至 exp-60s，进程内）与轮换的新 refresh_token（可为空）
func (t *TokenCache) Token(ctx context.Context, scope, email, clientID, refreshToken string) (access, rotated string, err error)
func (t *TokenCache) ForceToken(ctx context.Context, scope, email, clientID, refreshToken string) (access, rotated string, err error) // 丢弃缓存强制刷新（Graph 会话途中 401）
func AuthorizeURL(clientID, redirectURI, scope, loginHint string) string                 // "重新获取令牌"第一步：浏览器登录地址
func (t *TokenCache) FetchByAuthCode(ctx context.Context, code, clientID, redirectURI, scope string) (refreshToken, idToken string, err error)
func IdentityFromIDToken(idToken string) (Identity, error)                              // 解析 id_token 载荷取登录身份（不校验签名）
func ExtractAuthCode(input string) string                                               // 兼容完整跳转 URL 与纯码串
var ErrReauth = errors.New("invalid_grant：刷新令牌无效或已过期，需要重新授权") // worker 映射为 auth_failed（3.4）
```

**imapclient**：go-imap v2 实现。`Select(ReadOnly)` 即 EXAMINE；`BODY.PEEK[]` 分批（每批 20 封）流式拉原文（UID + INTERNALDATE + 完整正文）；oauth2 走自实现 XOAUTH2 SASL（go-sasl 仅有 OAUTHBEARER）。加密方式按端口选：**143 用 STARTTLS**（服务器不支持、握手或证书校验失败即报错，绝不在明文连接上发登录命令；握手本身没有超时，由看门狗兜底），其余端口（默认 993）隐式 TLS；拨号上限 30s，证书按主机名校验。增量搜索发 `UID SEARCH UID n:*`，但结果要在客户端再滤一遍 `UID > last_uid`（原因见下）。注意 go-imap 的 `Fetch`/`Search` 的 NumSet **必须传值类型** `imap.UIDSet`：传 `*imap.UIDSet` 能通过编译（方法集相同），运行时却 `panic: imap: invalid NumSet type`，worker 里的 panic 会让整个进程退出。

**为什么 `UID n:*` 的结果要再滤一遍**：RFC 3501 规定 `n:*` 在 n 大于文件夹里任何 UID 时**仍然匹配最后一封**。所以没有新邮件时服务器不回空，而是带回游标处那一封：

- 它已经入库（稳态）：重复下载全文、解析、走一次因唯一索引而回滚的事务。千级账号、大部分邮箱数天一封，这点冗余曾是 IMAP 路径的主要流量；
- 它还没入库（首次同步时窗口内一封都没有，游标取的是现有最大 UID）：那封旧邮件早于窗口、从没入库，又不带重拉标记——会被当成**新邮件**入库并通知一次。

客户端在增量路径直接丢掉 `UID ≤ last_uid` 的结果，不依赖服务器对 `n:*` 的实现细节。测试里的假服务器也必须按这条规则建模，否则测不出来（`TestFetchFolderIncrementalNoRefetch`、`TestFetchFolderOldMailsBeforeWindow`）。

**重新获取令牌**（oauth2 账号的 refresh_token 失效后，从 `auth_failed` 恢复的人工路径，DESIGN 3.5）：授权码流三步——① `GET /accounts/{id}/reauth-url` 返回 `AuthorizeURL(clientID, MSNativeRedirect, scope, email)`（带 `login_hint`；scope 按传输选 IMAP 或 Graph，并追加 `openid` 以便拿到 id_token）；② 用户在浏览器登录目标邮箱，把跳转后的完整 URL 粘回，`POST /accounts/{id}/reauth`；③ `ExtractAuthCode` → `FetchByAuthCode` 兑换新 refresh_token，先用 `IdentityFromIDToken` 核对登录身份与账号邮箱一致（浏览器里往往还登着别的 Microsoft 账号，不拦的话别人的令牌会被写进本账号），再加密落库（`UpdateCredential`，只写凭据列）并 `ReloadAccount`——账号状态在 worker 连接成功后写回 `syncing`。密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），不要再试。

重新获取令牌的局限：身份比对是 `preferred_username` / `email` 与账号邮箱的不区分大小写**精确**比对，以别名登记的账号恒判不一致（只能改用设置弹窗直接粘贴新令牌）；拿不到 id_token 一律拒绝；id_token 不校验签名（它来自本进程经 TLS 直连的令牌端点响应体，与同一响应里的 refresh_token 信任链相同）；这条路径只对假端点验证过，未对真实 Microsoft 端点跑过。

go-imap v2 beta.9 有三处缺口，由本包自己补上（升级依赖时逐条复核，`client_test.go` 的假服务器测试会兜住）：

- **IDLE 唤醒**：`IdleCommand.Wait()` 只在 `Close()`/断连时返回，服务器发 EXISTS 并不会唤醒它。构造客户端时设置 `Options.UnilateralDataHandler.Mailbox`；回调在 go-imap 的读 goroutine 上执行，必须立即返回——只向容量 1 的 channel 非阻塞发信号。`Idle` 收到信号后发 `DONE` 正常结束并返回 `notified=true`，连接保持可用（手动收取/兜底拉取都在同一连接上）。只对 `NumMessages != nil`（EXISTS）发信号，FLAGS/PERMANENTFLAGS 变化忽略；两条命令之间到达的 EXISTS 信号保留到下一次 IDLE（立即补拉）；IDLE 收尾时清掉累积的信号（随后的增量拉取已覆盖，留着会让下一次 IDLE 空转）。EXAMINE 期间的 EXISTS 归属那条 Select 命令，不触发回调。
- **看门狗（`watchdog`）**：go-imap 只在"响应已开始"之后设读超时（普通响应 30s、字面量 5min），等下一个响应的**第一个字节**没有任何超时——服务器卡死或连接被中间设备静默丢弃（无 FIN/RST）时 `Wait()` 永久阻塞，ctx 取消也无效。登录、`FetchFolder`、IDLE 握手都在看门狗监视下：持续 `stallTimeout`（2min）没收到任何数据 → 断开连接（挂起的命令随之以错误完成，`decorate` 把原因翻译成"IMAP 服务器超过 2m0s 无响应，已断开"）；ctx 取消 → 立即断开（停机/删除账号不再被卡住的连接拖住）。入库回调执行期间暂停无响应判定（`pause`/`resume`：本地慢不算服务器卡死）。IDLE **等待**阶段不设看门狗（静默是正常的），由 TCP keepalive、25 分钟上限和下面的收尾上限兜底。
- **收尾上限**：IDLE 收尾（发出 DONE 后等 tagged OK）超过 `idleStopTimeout`（10s）、`LOGOUT` 超过 `logoutTimeout`（5s）即强制断开——这两条是每个会话必经的路径，卡住等于 worker 卡住。

**登录错误分类**：只有服务器明确回了 tagged `NO`/`BAD`（`*imap.Error`）才包成 `ErrAuthFailed`（→ `auth_failed` 停止重试，DESIGN 3.4）；断连、无响应、ctx 取消、`* BYE`（限流/维护/连接数超限）一律按普通错误走退避重连。登录前先 `WaitGreeting()`，服务器以 `* BYE` 拒绝接入时能拿到原文。**不细分 RFC 5530 响应码**：tagged `NO` 一律算凭据被拒，包括服务器以临时原因拒绝的情况（`[UNAVAILABLE]` 暂不可用、`[LIMIT]` 限流、`[INUSE]` 并发登录超限等）——这类本该按连接类退避重试，现在会让账号停在 `auth_failed`。是否细分是待定的策略点，没有改。

**graphclient**：纯 HTTP 实现，仅支持 oauth2 账号（Graph 是 Outlook 家族的主传输：存量授权普遍只含 Graph 权限，IMAP scope 拿不到令牌）。只读——只有 list / delta / `GET /me/messages/{id}/$value` 三种 GET，绝不写远端（3.6）。两条路径共用一个分页循环（每页 50 条 → 批量 `seen` 查已入库 → 未见过的逐封取 MIME 原文，单封上限 25MB）：

- **delta 优先**：`cursor.DeltaLink` 非空就 GET 它（只返回上次之后的变更，极窄窗口）。仅当 Graph 明确答复链接失效（410 / 404 / 400）才退回时间窗列表重建；401、429、ctx 取消、网络与入库错误直接上抛、不退回（否则同一个 401 会在列表请求上再强制刷新一次令牌）。
- **时间窗列表**（首次 / 退回）：`$filter=receivedDateTime ge 游标&$orderby=receivedDateTime asc&$select=id,receivedDateTime&$top=50`，游标取 `last_seen_at`（没有则取 `since` 初始窗口）；闭区间 + `graph_id` 去重，避免边界秒遗漏。**引导初始 delta 链接放在列表之前**（`$deltatoken=latest`，只要链接不带数据）：链接对应"此刻"的状态，而列表可能跑很久（每封原文一次请求），其间到达的邮件不在列表里——引导放在列表之后的话，这些邮件两边都不在，永远拉不到。
- **单轮分页上限 100 页**：没走完就收工、置 `More`。列表路径下一轮从 `last_seen_at` 接着走（那一轮不交回 delta 链接，`initial_done` 保持 0，续走的邮件仍按历史处理）；delta 路径把没走完的 nextLink 当游标交回，下一轮接着走（否则一条超长变更流每一轮都从头走、停在同一处）。空页也可能带 nextLink（delta 常见），要跟下去。
- **单封取原文失败**（404 / 403 / 422 等 4xx，不含 401 / 408 / 429）：只关乎这一封（列表与取原文之间被删或移走），跳过；**连续 10 封**失败视为账号级故障，中止本轮。以前任何一封出错都让整轮失败、游标不前进，下一轮又从这封开始——整个账号从此卡死。
- **401**：先强制刷新一次令牌（`ForceToken`）再重试；刷新成功后仍是 401，或刷新被明确拒绝（`invalid_grant`）才是 `ErrReauth`；刷新因其他原因失败算普通错误。以前任何 401 都直接算 `auth_failed`——一个会话要发很多请求，访问令牌在会话途中过期是常态，账号会停在 `auth_failed` 且重启也不恢复。
- **429 / 503 / 504**：按 `Retry-After`（秒数或 HTTP 日期）等待后重试，至多 2 次；没有该头时从 2s 起逐次加倍；单次等待封顶 60s，仍不行就把错误交上去由 worker 退避（不占着会话死等）。
- 文件夹名：well-known 名（inbox、junkemail…）不区分大小写并规范成小写；自定义文件夹名与 id 区分大小写，原样传。

要点：
- **access_token 进程内缓存**（mstoken.TokenCache）：轮询模式短连接重连时直接用缓存令牌，过期前 60s 才刷新——不重蹈 main.py 每次运行必刷新的覆辙（它是短命进程，我们是常驻服务）。
- 轮换的 refresh_token 由 worker 拿到后**立即** `UpdateCredential`，不等邮件事务；同时更新 worker 的内存快照 `acct.CredentialEnc`（下个会话 `credentials()` 解密的是它）。只写库不更新快照的话 worker 一直拿旧令牌，与 TokenCache 里存的新令牌对不上，缓存永不命中，每个轮询周期都会再打一次令牌端点。
- 刷新在 TokenCache 锁内串行（账号级互斥，3.5）。
- 令牌端点 429 → 返回错误，走 worker 统一退避。令牌端点的错误分类：`invalid_grant` → `ErrReauth`（唯一的认证类）；`invalid_scope`（应用注册缺权限）、429、HTTP 与网络错误 → 普通错误，不置 `auth_failed`。令牌端点的响应体不回显进错误信息。
- `ForceToken` 优先用缓存里记的 refresh_token（上次刷新若被轮换，缓存里是同一家族里最新的值；复用调用方手里的旧值可能触发令牌家族吊销）。

### 2.5 mailparse（纯函数）

```go
type Parsed struct {
    MessageID, Subject, FromAddr string
    ToAddrs, CcAddrs []string
    SentAt time.Time
    BodyText, BodyHTML, Snippet string
    Size int64
    Attachments []Attachment
    HasAttachments bool
    Truncated bool
    ParseError string                            // 置位时其余字段尽力填充，raw 照常保存（3.4）
}
func Parse(raw []byte, maxSize int64) Parsed
```

### 2.6 sync：Manager 与 worker

```go
type Manager struct{ /* store、cipher、dial 构造器、notifier、events、budget、stagger 队列、worker 注册表、全局配置缓存 */ }
func (m *Manager) Run(ctx context.Context) error          // 加载全部非 auth_failed 的启用账号 → 错峰队列逐个 spawn；阻塞到 ctx 取消，返回前等全部 worker 退出
func (m *Manager) ReloadAccount(id int64)                 // PATCH 后：停旧 worker（等其退出，上限 15s）→ 优先入队重启
func (m *Manager) RemoveAccount(id int64)                 // DELETE / 停用：停 worker（IDLE 预算随会话结束释放），不再入队
func (m *Manager) TriggerNow(id int64)                    // 单账号：向运行中 worker 的 trigger 非阻塞发送；worker 不在运行（auth_failed、排队中）→ 优先入队拉起
func (m *Manager) TriggerBatch(ids []int64)               // 批量：进错峰队列（优先于待启动账号）
func (m *Manager) ReloadBatch()                           // 批量导入后：把 pending 账号入队（已在队列的不重复）
func (m *Manager) RefreshSettings()                       // PUT /settings 后：全局配置缓存立即失效
func (m *Manager) SetEventNotifier(n EventNotifier)       // main 组装时注入 api.Hub
func (m *Manager) SetNotifyEnabled(id int64, on bool)      // PATCH notify_enabled：更新运行中 worker 的开关，不重启
```

**错峰队列**：单消费者 goroutine，按 `MAILHUB_CONNECT_STAGGER` + 每账号随机抖动逐个 spawn；手动收取 / 重载的请求是优先级 job，插队首（不去重），启动时的普通 job 在队列里去重（6.2 语义）。

**IDLE 预算（budget.go）**：`map[host]计数` + mutex；每次会话准入时 `Acquire(host, limit)`，上限取全局配置（DB 覆盖 > env）——改配置后新会话即按新预算判定，存量 IDLE 连接不受影响；拿不到预算 → 该账号本次会话走轮询模式；会话结束时 `Release`。

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
                                 // defer：LOGOUT；会话途中轮换的令牌（SessionTokenRotator）同样持久化
  status=syncing                 // auth_failed 的账号换凭据 / 重新获取令牌后在这里恢复
  if canIdle(服务器支持 && 单文件夹 && budget.Acquire(host)):
      defer budget.Release; idleLoop(ctx)                     // IDLE 会话：阻塞至断线/停止
  else:
      pollOnce(ctx); Logout()                                 // 轮询会话：短连接单次收取即返回

idleLoop(ctx):                   // 长连接会话（3.2 状态机）
  pollOnce()                     // 初始拉取（含首同步历史邮件标记）
  loop:
    select:
      idleNotify   → pollOnce → 重新 IDLE                        # EXISTS = 有新邮件（imapclient 经 UnilateralDataHandler 唤醒，本文 2.4）
      25m 超时     → 重新 IDLE（不拉取）                        # 协议 29 分钟内刷新要求；周期性拉取由 fallbackTick 负责
      fallbackTick → pollOnce                                   # FALLBACK_INTERVAL 兜底（防通知丢失 + 心跳；下限 30s，可在线改）
      trigger      → 退出 IDLE → pollOnce → 重新 IDLE           # 手动：不重置任何计时（3.2）
      ctx.Done     → return
    拉取 / IDLE 出错 → return err（交 run 退避重连）

pollOnce(ctx):                   // 一次完整收取：各文件夹 fetchFolder → 全部成功才 TouchAccount 心跳 → status=polling
fetchFolder(ctx, folder):        // 单个文件夹，IMAP / Graph 同一路径
  st := GetFolderState；firstSync := st == nil || !st.InitialDone
  st == nil → BeginFolderSync                        # 先落"首次同步进行中"的行，再开始收
  FetchFolder(st, since = now − 初始窗口, ingest)     # since 恒传：client 在游标有效时忽略它
  ▲ 以下任一步失败即返回，游标不动，下一轮重拉（重复邮件幂等吸收）
  AdvanceFolderUID（IMAP）→ TouchFolderSeen → SetFolderDelta（Graph）
  firstSync && !res.More → FinishFolderSync           # 走完才标记；因分页上限收工的那一轮不算
```

**手动触发合并**：`trigger chan struct{}` 容量 1，发送方非阻塞（select+default 丢弃）——正在拉取时到达的信号天然合并，不排队。

**提醒开关的实时值**：入库时读的是 `worker.notify`（`atomic.Bool`），不是 `acct.NotifyEnabled`——`acct` 是 worker 启动时的账号快照，改了库里的值它不会变。`spawn` 时初始化；PATCH `notify_enabled`（单个/批量）后 API 调 `Manager.SetNotifyEnabled` 即时更新，不重启 worker（不打断 IDLE 和进行中的拉取）。worker 未运行（禁用、`auth_failed`、排队中）时是空操作，它下次启动从库读取。

**ingest 内部**（每封邮件）：`mailparse.Parse` → `.eml` 先写 `.part` 再原子改名定稿（`data/mails/{account_id}/{yyyy-mm}/{坐标}.eml`，月份按真实到达时间归档；解析失败也照存原文）→ `IngestMessage`（每封一事务，3.3）→ 提交后 `notifier.Wake()`；非历史邮件再 `events.NotifyEvent(摘要)`。`ErrDuplicate` 静默跳过并删掉本次新写的文件；入库失败同样清掉本次新写的文件（入库前就在的那份不动——现有记录引用的正是它）。

**哪些邮件算"历史邮件"**（不生成投递记录、不推浏览器事件，DESIGN 3.4 / 5.1）——这是最容易做错的一处，判定只有两条：

- **首次同步**：以 `folder_states.initial_done` 为准，而不是"有没有游标行"。游标行在首封邮件入库时就建了，只看行的话，首次同步中途被打断（网络错误、停机、Graph 单轮分页上限）之后接着走的那几轮就不再算首次同步，窗口里剩下的历史邮件会被当成新邮件通知一遍。
- **UIDVALIDITY 变化后的重拉**（仅 IMAP）：邮箱重建了 UID，窗口内的旧邮件换了坐标再出现一遍。`IngestMessage` 此时另按 Message-ID 去重（`Reindex`）；其中在上次成功同步**之前**就到达的（INTERNALDATE 早于 `last_sync_at` 减去 10 分钟余量 `reindexGrace`）按历史处理。比较的两边分别是邮件服务器的时钟（INTERNALDATE）和本机的时钟（`last_sync_at`），不一定对得准，所以留余量；拿不准时偏向按新邮件处理——判错的后果不对称：把新邮件判成旧邮件等于丢一封（不投递、不推事件），把旧邮件判成新邮件最多多通知一次已被本地删除的旧邮件。到达时刻未知的邮件一律按新邮件处理。

UIDVALIDITY 相同的稳态增量没有重拉标记，重复只能靠坐标唯一索引——所以本文 2.4 里"`n:*` 结果要再滤一遍"是必须的，否则会把一封从未入库的旧邮件当成新邮件。

### 2.7 notify

```go
func (d *Deliverer) Run(ctx context.Context)               // 循环：select{wake, tick(15s)} → pump（取一批到期的 → 并发投递）；阻塞到 ctx 取消
func (d *Deliverer) Wake()                                 // sync 在每封邮件入库提交后调用（非阻塞合并）；API 手动重发后也调
func (d *Deliverer) PostTest(wh *store.Webhook) (int, error) // Webhook"发送测试事件"（api 经 WebhookTester 接口调用）
```

- 并发：`sync.WaitGroup` + 带缓冲 channel 作信号量，上限 `MAILHUB_WEBHOOK_CONCURRENCY`；单请求超时 10s；每轮最多取 50 条，取满说明队列可能还有，立即再来一轮。
- 签名：`ts := now.Unix()`；`HMAC_SHA256(secret, ts + "." + body)`；头与载荷字段按 5.2。`status` 取值 `pending | success | failed`。
- 重试梯度 10s/1m/5m/30m/2h：首次失败后再重试 5 次，全部失败置 `failed`（`attempts = 6`，5.3）。`POST /deliveries/{id}/retry` 手动重发会把 attempts、last_error、response_code、next_retry_at 清零并回到 `pending`。
- **没有 claim 预写**：投递器是 `webhook_deliveries` 的唯一消费者，且投递循环是串行的（一批全部落库后才开始下一批；失败记录会立刻写上未来的 `next_retry_at`），所以到期记录（`next_retry_at` 为空或已到）直接读出来投递即可。将来若要多实例并发消费，需先补上"取出即预写下次重试时间"的 claim。
- 禁用中的 webhook：它的到期记录原样保持 `pending`，重新启用后继续投递。只有载荷构建失败（邮件已被删）与 webhook 已不存在才置终态 `failed`。
- 取件只取启用中 webhook 的记录（`ListDueDeliveries` 带 JOIN 过滤）。
- 清理不在这里：终态记录（success / failed）保留 30 天，与邮件 retention、孤儿文件回收一起由 `maintain.Run` 每日执行（本文 2.10）。`pending` 记录不论多旧都不清——清掉就是丢通知。

### 2.8 api

- 路由（`router.go` 的 `Handler`）：`/healthz` 免认证；`/api/v1` 路由组内挂 accounts（含批量导入 / 导出、批量 PATCH、sync、test、重新获取令牌）、messages、webhooks（含 test、deliveries、retry）、settings、`GET /status`、`POST /events/ticket`，全组经 auth 中间件；`GET /api/v1/events`（SSE）在组外单独认证——EventSource 带不了 Authorization 头，用 `?ticket=`（60 秒一次性短票，由 `POST /events/ticket` 签发）或 `?token=`。静态段路由（`/accounts/export`、`/accounts/sync`…）写在 `/accounts/{id}` 之前。
- 中间件顺序：RequestID → Recoverer → slog Logger（Debug 级，默认不输出）→ securityHeaders（**所有**响应）→（`/api/v1` 组内）Auth。
- Auth：`subtle.ConstantTimeCompare` 比较 Bearer Token，实现在 `router.go`（没有单独的 auth 文件）。
- 统一分页 helper（offset/limit/total）、统一错误体 `{"error":{"code","message"}}`。路由器自己产生的响应例外：`/api/v1` 下路径不存在是 chi 的纯文本 `404 page not found`，方法不匹配是空响应体的 405。
- 安全头（所有响应）：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Content-Security-Policy: object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'`。CSP 故意不设 `default-src` / `script-src`（Naive UI 运行时注入 `<style>`，收紧需逐项在浏览器里验证）；邮件正文 iframe 的约束由前端自己的 srcdoc CSP 负责（本文 1.2）。
- SSE（`events.go` 的 `Hub`）：每连接一个带缓冲 channel，慢消费者丢包而非阻塞广播；30s 心跳注释行，`retry: 5000`；事件 `message.new` 载荷为单封摘要 `{id,subject,from,account}`，只有非历史的新邮件才推。`Hub.Close()` 让全部事件流立即返回——`srv.Shutdown` 不会主动关活动连接，不先关 SSE 的话停机会被开着的邮件页拖满超时。前端不用浏览器自带的重连（会带旧票拿 401），`onerror` 里关闭连接、5 秒后重新取票建连。
- 批量导入 handler：`Content-Type: text/plain` 时逐行 `----` 解析，JSON 数组则按导出格式导入（容错：行号 + 原因回传，6.2）。
- Web UI：`web/embed.go` 内嵌的前端构建产物（`web/dist`）以静态文件服务挂在根路径 `/*`，API 路由优先命中。

### 2.9 组装与数据流（cmd/mailhub/main.go）

```
启动顺序：config 校验 → store.Open(+迁移) → crypto → signal.NotifyContext(SIGINT/SIGTERM)
        → 组装：TokenCache（进程级一份）、dial（按 transport 分流）、Deliverer、Hub、Manager(+SetEventNotifier)
        → 后台 goroutine ×3（Manager.Run / Deliverer.Run / maintain.Run，统一计入一个 WaitGroup）
        → http.Server.ListenAndServe（阻塞）
优雅退出：SIGTERM/SIGINT → ctx cancel → ① Hub.Close()：立即结束全部 SSE 流
        → ② srv.Shutdown（等其余请求，≤5s，超时强制 Close）
        → ③ 等三个后台 goroutine 退出（≤20s；Manager.Run 返回前等全部 worker 退出）
        → ④ defer：释放信号监听、关闭 store
```

两段预算相加 25s，要落在容器的停止宽限期之内（compose 设了 `stop_grace_period: 30s`；docker 默认只有 10s，超时即 SIGKILL）。顺序不能反：先关 SSE，`Shutdown` 才是毫秒级的；先等后台任务再关库，才不会出现"库已关闭、worker 还在写"。停机序列有测试（`cmd/mailhub/main_test.go`）。

```
IMAP / Graph 服务器 ──▶ imapclient / graphclient ◀── sync.worker（每账号一个）
                              │ raw（.eml 先落盘）          │ Connect(oauth)：轮换令牌 → store.UpdateCredential
                              ▼                            │ pollOnce 成功 → store.TouchAccount（心跳）
                        mailparse.Parse                    ▼
                              └────▶ store.IngestMessage（一个事务：message＋attachments＋匹配的 deliveries＋folder_states）
                                             │ commit → notifier.Wake()                   │ commit 且非历史邮件 → events.NotifyEvent(摘要)
                                             ▼                                            ▼
                              notify.Deliverer ──HTTP POST＋HMAC──▶ 下游 Webhook        api.Hub ──SSE──▶ 浏览器（刷新列表 / 桌面通知）
浏览器 ──▶ api(chi) ──▶ store（查询） ；api ──Trigger / Reload / RefreshSettings──▶ sync.Manager（控制面）
浏览器 ──▶ POST /events/ticket（Bearer）──▶ GET /events?ticket=…（SSE 长连接）
每 24h：maintain.Run ──▶ store（retention、终态投递记录）＋ data/mails（孤儿文件）
```

### 2.10 maintain（每日维护）

`maintain.Run(ctx, cfg, st)`：启动 1 分钟后跑第一轮，此后每 24 小时一轮（`Once` 导出供测试）。每轮按固定顺序做三件事：

1. **retention 清理**：`MAILHUB_RETENTION_DAYS`（可在线改；0 = 永久保留，跳过）——删除 `received_at` 早于窗口的邮件（行，级联附件与投递记录）并删对应 `.eml`。
2. **孤儿回收**：扫描 `data/mails`，删除数据库已不引用的 `.eml`（宽限期 5 分钟：worker 先定稿文件、后提交事务，扫描恰好落在两步之间时不能删）与超过 24 小时的 `.part`（写入流程秒级完成，更老的是崩溃残留）。不认识的文件不动。放在 retention 之后：先删行再扫文件。孤儿的主要来源是删除账号时不带 `purge`——`purge` 只决定"现在删"，不带的由这里回收。
3. **终态投递记录清理**：`success` / `failed` 保留 30 天（按 `updated_at`）；`pending` 不论多旧都不删（清掉就是丢通知，比如 webhook 被禁用期间排队的那些）。

维护里的文件删除是尽力而为：失败只记日志，下一轮再收。

## 3. 错误处理与可观测性

- **错误三分类**（与 3.4 对应）：连接类（重试+退避）、认证类（停，`auth_failed`）、解析类（入库但标记 `parse_error`）。分界线只有一条：**服务器明确拒绝了凭据**才算认证类。IMAP 登录即 tagged `NO`/`BAD`（本文 2.4，目前不细分 RFC 5530 临时响应码）；令牌端点 `invalid_grant`、Graph 刷新后仍 401；断连、无响应、`* BYE`、ctx 取消都是连接类——误判成认证类会把账号停在 `auth_failed`，重启也不会自动恢复。
- slog 用默认 handler，**文本格式**（`2026/11/24 12:00:00 WARN worker session failed account_id=12 error="…" backoff=5s`），没有配置 JSON handler；默认级别 Info——HTTP 访问日志、手动触发、历史邮件入库是 Debug，默认不输出。常用字段：`account_id`、`email`、`error`、`backoff`、`delivery_id`、`graph_id`、`status`、`message_id`。
- **日志里不写邮件主题、发件人、正文**——验证码邮件的主题本身就是验证码，日志不该多一份副本（`message ingested` 只带 `account_id` / `message_id` / `folder` / `uid`）。同类约定：导出账号只记数量与是否含凭据；重新获取令牌不记授权码、也不记"实际登录的是哪个邮箱"；令牌端点的响应体不回显。
- 错误信息里的外部端点细节按需带出、不含凭据：授权码换令牌失败时把 `error_description`（AADSTS 诊断码）截断到 220 字符带出；STARTTLS 的 x509 证书错误原样带出便于排错。
- 心跳与巡检不靠日志，靠 `GET /api/v1/status` 与账号列表（6.5、7）。

## 4. 测试策略

| 层 | 方式 |
|---|---|
| mailparse | 真实 `.eml` 样本表驱动（含乱码、超大截断、坏格式） |
| config / crypto | 默认值与校验、体积解析；往返、密钥不匹配、防篡改、nonce 唯一性 |
| store | 临时目录里的真实 SQLite 文件（跑全部迁移）+ repository / 事务测试：入库幂等与游标、并发入库（多 goroutine 同时 `IngestMessage` 不得出现 `database is locked`）、游标推进（只增不减 / UIDVALIDITY 变化 / `initial_done` 标记）、UIDVALIDITY 重拉按 Message-ID 去重、`received_at` 索引与外键子表索引（007 / 006）、Graph 哈希碰撞、手动重发清零 attempts、投递记录只清终态、全文搜索、排序与计数 |
| sync / worker | 注入假传输：IDLE 通知、兜底触发、手动触发合并、退避、`auth_failed` 即停、UIDVALIDITY 重拉不轰炸（含 10 分钟余量）、**空文件夹的首封邮件要通知**、首次同步被打断 / 因分页上限收工后仍是历史邮件、提醒开关运行时切换（不重启 worker）、令牌轮换（连接时与会话途中）后的持久化与内存快照、手动收取拉起 `auth_failed` 的 worker、非正轮询间隔不 panic、`Manager.Run` 等 worker 退出 |
| imapclient | `net.Pipe` 假服务器（**按 RFC 3501 建模 `UID n:*`**；STARTTLS / 隐式 TLS 的用例改用本地 TCP 监听 + 自签证书），断言线上命令（`EXAMINE` / `BODY.PEEK[]` / `UID SEARCH UID n:*`）：登录错误分类（拒绝 / 断连 / 无响应 / `* BYE`）、增量与分批拉取、无新邮件不重拉、窗口内无邮件时游标精确、STARTTLS（不支持即断、证书不受信、握手卡住、ctx 取消）与隐式 TLS、IDLE 被 EXISTS 唤醒与收尾上限、看门狗（服务器静默、ctx 取消、慢入库不算卡死）、LOGOUT 上限 |
| mstoken | `httptest` 模拟令牌端点：轮换返回、缓存命中、`ForceToken`、scope 隔离、`invalid_grant` / `invalid_scope`、429、并发刷新单飞；授权码换令牌（含 AADSTS 描述）、授权 URL、id_token 身份解析 |
| graphclient | `httptest` 模拟 Graph：分页列表、seen 去重、`$value` 原文、401 → 刷新令牌 → 再 401 才 `ErrReauth`（刷新失败的类别保留）、429 / 5xx 重试与 `Retry-After` 上限、delta 滚动 / 引导 / 410 退回时间窗、认证失败与限流不触发退回、引导先于列表、空页带 nextLink、单封原文失败跳过与连续失败熔断、单轮分页上限续走 |
| notify | `httptest` 收投递：断言签名头、重试梯度与耗尽（读库里的 `attempts` / `next_retry_at`）、`include_body`、禁用 webhook 保持 pending、测试事件 |
| maintain | 临时目录：retention 清理与关闭（0）、孤儿 `.eml` / 过龄 `.part` 回收、刚写入的文件不误删、投递记录只清终态 |
| api | `httptest` 全链路（临时目录 SQLite + 真实 Manager + 进程内假投递器）：认证、分页、批量导入容错与 JSON 迁移导入、标签、全局配置、导出 / 导入往返、路由语义（重复账号 409、删除不存在 404、空凭据 400、webhook secret 最短长度、`day_start`）、安全头、SSE 事件与一次性票据、`Hub.Close` 结束 SSE、重新获取令牌（含身份不一致被拒）；提醒开关 PATCH → **运行中**的 worker（验证控制面确实传到了 worker） |
| cmd/mailhub | 停机序列：有 SSE 长连接时 `shutdownHTTP` 及时返回，空闲时可重复调用 |
| web（前端） | `cd web && npm test`（Node 内置 `node --test`）：列表查询参数、原地刷新合并、锚点与全量翻页（`maillist`）、邮件 HTML 预处理与 CSP（`mailhtml`）、`day_start` 计算。只覆盖 `src/lib/` 的纯逻辑，组件本身没有自动化测试 |
| 集成冒烟（手动） | 对单个真实测试邮箱跑完整收信 → webhook |

超时/唤醒这类"不发生某件事"的缺陷，测试的有效性用**注入缺陷**验证：把看门狗关掉、去掉收尾上限、删掉唤醒回调……逐个改坏后对应测试必须失败（改坏后"只在超时后失败"的，测试自带 5s 守卫）。新增超时逻辑时照此做一遍，否则很容易写出永远通过的测试。

## 5. 构建与部署

- 标准循环：`go build ./... && go vet ./... && go test ./... -count=1`；改过前端再 `cd web && npm test`。
- `make build`：`go build -o bin/mailhub.exe ./cmd/mailhub`；`make frontend`（`cd web && npm install && npm run build`）生成 `web/dist`，`make all` = 前端 + 后端。**只改前端也必须重新 `go build`**——dist 内嵌在二进制里，不重编译部署的还是旧 UI。`web/dist/` 在库里只保留占位 `index.html`（保证无 node 环境也能 `go build`）。
- Dockerfile 多阶段（已交付）：`node:20-alpine` 构建前端（固定在构建机架构）→ `golang:1.27-alpine` 静态构建（按 `TARGETOS` / `TARGETARCH` 交叉编译，arm64 不需要 QEMU）→ `gcr.io/distroless/static-debian12` 运行（纯 Go 驱动无 CGO；distroless/static 自带 CA 证书与 tzdata，Graph / 令牌端点 HTTPS 必需）。镜像用默认 tag、**不绑定运行用户**，UID / GID 由部署侧指定；`/data` 预授权给 1000:1000，与 compose 默认一致。
- `docker compose up -d --build`：凭据经根目录 `.env`（`KEY=value`，无 export 前缀，缺失时拒绝启动）注入；`docker-compose.yml` 已含 `user: "1000:1000"`、nofile 8192、`/data` named volume、`restart: unless-stopped`、`stop_grace_period: 30s`（见本文 2.9）。监听 `:18099`（镜像内设 `MAILHUB_LISTEN`，源码默认 `:8080`）。
- 可选 `TZ` 环境变量：日志时间，以及 API 调用方不传 `day_start` 时"今日收信"的日界（Web 界面始终按浏览器时区算，不受影响）；省略即 UTC。时区数据用的是镜像自带的 tzdata，二进制没有内嵌——换成不带 tzdata 的基础镜像时需要 `import _ "time/tzdata"`。
- 运行时只需 `data/` 卷与 9.1 环境变量；`LimitNOFILE ≥ 4096`（9.2）。

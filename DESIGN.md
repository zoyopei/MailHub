# MailHub 邮件集中收取服务：设计文档

## 1. 概述

### 1.1 目标
- 统一接入**几百至上千个**邮箱账号，**只收不发**（邮箱矩阵集中收取）；**收件频率低**，大部分邮箱数天才收到一封，单账号存量邮件不多
- 新邮件到达后通过 **Webhook** 通知外部系统
- 提供 **REST API**：账号管理（含**批量导入**）、查看同步状态、查询和读取邮件
- 提供 **Web 界面**：浏览邮件、**标记已读（本地状态）**、**批量删除（本地副本）**、管理大批量账号

### 1.2 明确不做（非目标）
| 不做 | 原因 |
|---|---|
| 发信、回复、草稿 | 需求明确为只收 |
| 多用户、权限体系 | 上千账号是"资源多"而非"用户多"，单管理员 + 一个 API Token 足够 |
| POP3 | IMAP 已覆盖几乎所有邮箱，POP3 没有增量同步和推送能力 |
| 双向同步（已读、删除、移动回写服务器） | 对远端邮箱保持**完全只读**；矩阵场景下"已读"只是 MailHub 内的本地状态（7） |
| 微服务、消息队列 | 单进程经连接错峰与预算控制可支撑千级账号（2.2），规模再上量级再议 |
| 全文检索引擎 | 先用索引 + LIKE，数据量大后再按需引入 FTS / PG 全文（M4） |
| 邮件规则、标签、垃圾过滤 | 交给 Webhook 下游处理 |

### 1.3 现状：main.py 简易工具
仓库中的 `main.py` 是一个**已可用**的命令行工具，也是 MailHub 的前身：

- 从 `accounts.txt` 批量加载账号（每行 `邮箱----密码----client_id----refresh_token`），批量读取多个 Outlook 收件箱
- 双协议读取：Microsoft Graph REST API 与 IMAP XOAUTH2，均已在真实账号上验证
- OAuth2 刷新令牌流程（公共客户端，无 client_secret），`--save-token` 可将服务端轮换出的新刷新令牌写回账号文件
- 只读：IMAP 拉取使用 `mark_seen=False`，不改变服务器上的已读状态

它提前验证了本设计中不确定性最高的部分——Outlook 的 OAuth2 认证路径，也验证了**批量账号文件驱动**的工作方式。MailHub 实现后 main.py 退役。

main.py 带来的关键设计输入（均已纳入下文）：

| 实践经验 | 纳入设计的位置 |
|---|---|
| Outlook 已停用基础认证，必须 XOAUTH2 | OAuth2 从"按需"提前到第一期（3.5） |
| 每个账号使用独立的 client_id（Azure 应用注册） | accounts 表新增 `client_id` 字段（4） |
| 刷新可能返回新 refresh_token，必须持久化，否则下次可能 invalid_grant | 令牌轮换在同一数据库事务中更新凭据（3.5） |
| Graph 与 IMAP 令牌 audience 不同，互不通用 | 令牌缓存按 scope 参数化，IMAP/Graph 各取所需（3.5、3.6） |
| `invalid_grant` 表示需要用户重新授权 | 映射为 `auth_failed` 状态并停止重试（3.4） |
| 账号文件中的密码字段在 XOAUTH2 流程中实际未使用 | oauth2 账号不需要密码，凭据二选一（4） |
| **一行一账号的批量文件是天然的工作单元** | 批量导入 API 直接兼容该文本格式（6.2），导入后台错峰验证（2.2） |

---

## 2. 总体架构

单进程单体服务，内部分三个模块，共用一个数据库：

```
┌──────────────────────── MailHub（单进程）────────────────────────┐
│                                                                  │
│  ┌────────────┐    ┌──────────────────┐    ┌──────────────────┐  │
│  │ HTTP 服务  │    │ 同步管理器        │    │ Webhook 投递器   │  │
│  │ REST API   │    │ 每账号一个 worker │    │ 轮询投递表并重试 │  │
│  │ 静态 Web UI│    │ IMAP IDLE + 轮询  │    │                  │  │
│  └─────┬──────┘    └────────┬─────────┘    └────────┬─────────┘  │
│        └────────────────────┼───────────────────────┘            │
│                   ┌─────────▼─────────┐                          │
│                   │ SQLite / PostgreSQL│   磁盘：原始 .eml       │
│                   └───────────────────┘                          │
└──────────────────────────────────────────────────────────────────┘
        ▲                      │                        │
   API / 浏览器           IMAP over TLS             HTTP POST
                     (Gmail/QQ/163/Outlook...)      (你的服务)
```

### 2.1 技术选型（已确定，实施细节见 TECH.md）

> 最终选型、完整依赖清单与模块设计见 **TECH.md**；本节保留决策依据。

**语言：Go（首选）**。规模假设从"个人 10 账号"变为"几百至上千"后，选型依据变化。注意本场景**邮件量低**（大部分邮箱数天一封），瓶颈在**连接管理**而非解析吞吐：

| 考量 | Go | Python（FastAPI + asyncio） |
|---|---|---|
| 千级 IMAP 长连接 | goroutine 每个数 KB 栈，调度与内存占优 | asyncio 可行，但千级任务的内存与稳定性吃紧 |
| 邮件解析吞吐 | 可全核并行 | GIL 下需多进程池；但低频收件下解析压力本就很小，此条不构成区分点 |
| 长期运行与部署 | 单二进制、内存可控 | 需带环境部署 |

main.py（Python）的价值以**参数与流程**形式移植：token 刷新只是一次 HTTP POST，XOAUTH2 登录 go-imap 已内置支持，重写成本低（3.5 的实测参数表即移植清单）。若更熟 Python 且接受千级 asyncio 连接的运维风险，此邮件量级下 Python 路线亦完全可行，设计不变。

| 部分 | 选择 | 说明 |
|---|---|---|
| 语言 | **Go** | 理由见上表 |
| IMAP 库 | `emersion/go-imap` v2 | 支持 IDLE 与 XOAUTH2 |
| 邮件解析 | `jhillyerd/enmime` | 处理 MIME、编码、附件 |
| 数据库 | **SQLite（WAL）**，可选 PostgreSQL | 低频收件（日增百级～千级、年增百万行内）下 SQLite 覆盖全部规划规模，零运维；DDL 兼容 PostgreSQL，量级显著变化时经 `MAILHUB_DATABASE_URL` 切换，不引入 ORM 重型抽象 |
| 存储 | 本地磁盘 | 原始邮件存为 `.eml`，数据库只存元数据和正文；低频收件下存储成本极低（9.4），默认永久保留 |
| 前端 | 轻量 SPA（Vue/React 任选） | 千级账号列表与邮件列表必须**虚拟滚动** + 分页 + 搜索（7） |

### 2.2 规模设计考量（几百～上千账号）

**账号数量（而非邮件量）是本设计与"个人收件工具"的分水岭**——瓶颈是千级连接与服务商风控，以下机制是第一公民而非优化项：

**连接管理**
- 每账号每文件夹一条 IDLE 长连接，千级账号即千级连接：部署需调 fd 上限（`LimitNOFILE` / `ulimit` ≥ 4096，9.2）。
- **单主机 IDLE 预算**（`MAILHUB_IDLE_BUDGET_PER_HOST`）：矩阵账号常大量落在同一服务商（如几百个 Outlook 账号都连 `outlook.office365.com`），超出预算的账号自动**降级为轮询**（短连接、fetch 完即断），避免触发服务商对单 IP 的连接数风控。

**错峰（stagger）**
- 服务启动或批量导入后，worker 不齐发：按 `MAILHUB_CONNECT_STAGGER`（默认 2s）逐账号交错启动，每账号再叠加随机抖动。
- oauth2 令牌刷新天然分散（仅重连时发生）；对令牌端点收到 429 时指数退避。

**批量导入后台验证**
- 单账号 `POST /accounts` 仍即时验证登录；**批量导入不逐个验证**——先全部入库（`status=pending`），由同步管理器按上述错峰节奏逐个连接验证并首同步。导入接口立即返回统计结果，不让上千次 IMAP 握手阻塞 HTTP 请求。
- 验证失败的账号落到 `error` / `auth_failed` 状态，在 Web 账号管理页可见、可批量处理（7）。

**服务商风控**
- 认证失败即停（3.4）在矩阵规模下更重要：一个被风控的账号反复重试可能危及同 IP 下的其余账号。
- 退避重连的上限（5min）对所有账号统一生效，重启风暴由错峰机制吸收。

---

## 3. 邮件同步设计（核心）

### 3.1 基本原则
- **只读**：用 `EXAMINE`（而非 `SELECT`）打开文件夹，用 `BODY.PEEK[]` 拉取邮件，不会修改 `\Seen` 等任何标记。（main.py 的 `mark_seen=False` 是同一原则的库层实现。）
- **增量**：基于 IMAP 的 `UIDVALIDITY` 和 `UID`，每个文件夹记录 `last_uid`，每次只拉 `UID > last_uid` 的邮件。
- **默认只同步 INBOX**：每个被监控的文件夹占用一条 IDLE 连接，矩阵规模下连接是稀缺资源，不建议配置额外文件夹。

### 3.2 Worker 生命周期
每个启用的账号对应一个 worker（goroutine）：

```
启动（按 2.2 错峰调度）
     → （oauth2 账号：刷新令牌，见 3.5）→ 连接并登录 → EXAMINE 文件夹
     → 增量拉取（UID > last_uid）
     → 进入 IDLE 等待
          ├─ 收到 EXISTS 通知     → 退出 IDLE → 增量拉取 → 重新 IDLE
          ├─ 25 分钟超时          → 重新发起 IDLE（协议要求 29 分钟内刷新）
          ├─ 5 分钟兜底定时器     → 主动做一次增量拉取（防 IDLE 通知丢失）
          └─ 连接断开或出错       → 记录错误 → 指数退避重连（5s 到 5min 封顶）
```

- 服务器不支持 IDLE **或该主机超出 IDLE 预算**（2.2）时，降级为纯轮询（间隔为账号的 `poll_interval`，默认 60 秒；批量导入时以 `MAILHUB_POLL_INTERVAL_DEFAULT` 填充，9.1）。低频邮箱可在账号级将 `poll_interval` 调大到 300～600 秒进一步降低连接频率；IDLE 模式的实时性不受此影响。
- **手动收取是插入信号，不改变节奏**：API/界面触发的立即收取（6.2 `sync`）只是让 worker 立即执行一次增量拉取，**不重置、不影响轮询/IDLE 的既有计时**——拉取完成后 worker 按原节奏继续（轮询定时器不归零，IDLE 模式下走"退出 IDLE → 拉取 → 重新 IDLE"的正常路径）。正在拉取时到达的触发信号合并为一次，不排队。对 `error` / `auth_failed` 状态的账号，手动触发视为用户显式授权的一次连接尝试，成功则恢复正常状态机。
- **`last_sync_at` 是账号心跳**：任何一次**成功完成**的收件检查（增量拉取往返成功，无论是否拉到新邮件）都更新它；失败的尝试不更新（异常由 `status` / `last_error` 呈现）。IDLE 模式下 5 分钟兜底定时器保证该时间戳持续前进，据此在 Web 端区分"没有邮件"与"收取故障"（7）。
- 账号被 API 修改、停用或删除时，同步管理器停止旧 worker，再按新配置重启。
- 重连即重新认证：oauth2 账号每次（重）连接前刷新 access_token（3.5），长连接存续期间无需重复认证。

### 3.3 单封邮件处理流程
1. `UID FETCH` 拉取完整原始邮件（RFC822）。
2. 原文写入磁盘：`data/mails/{account_id}/{yyyy-mm}/{uidvalidity}-{uid}.eml`（文件名用 IMAP 坐标而非数据库 id：写盘与事务解耦，天然幂等，无"先拿 id 再改名"的崩溃窗口）。
3. 解析出发件人、收件人、主题、日期、纯文本正文、HTML 正文、附件列表。
4. **在同一个数据库事务内**完成三件事：
   - 插入 `messages` 记录
   - 若账号 `notify_enabled=1`（提醒开关，**默认关**）：为每个匹配的 webhook（`enabled` 且 `account_ids` 为 NULL 或含该账号）插入一条 `webhook_deliveries` 记录（状态为 pending）
   - 更新文件夹的 `last_uid`
5. 事务提交后唤醒 Webhook 投递器。

同一事务保证了邮件入库则通知必然会发，不需要引入消息队列。批量拉取时**按邮件分事务**（而非一个巨事务），保证增量可见性与崩溃恢复粒度。

### 3.4 边界情况
| 情况 | 处理 |
|---|---|
| 首次添加账号 | 默认只拉取最近 `MAILHUB_INITIAL_SYNC_DAYS`（默认 90 天，约 3 个月）的邮件，且**历史邮件不触发 webhook**，避免通知轰炸；批量导入的账号同样按此规则 |
| 重复邮件 | IMAP：`(account_id, folder, uidvalidity, uid)` 唯一索引；Graph：`(account_id, graph_id)` 唯一索引（3.6）。重复插入直接忽略 |
| UIDVALIDITY 变化（少见） | 更新记录并将 `last_uid` 归零，按首次同步规则重拉。已存在的邮件按 `Message-ID` 头去重，不重复通知 |
| 超大邮件 | 超过 `max_message_size`（默认 50MB）只保存头部信息，标记 `truncated=true` |
| 认证失败（密码错误 / `invalid_grant`） | 账号状态置为 `auth_failed` 并**停止重试**（矩阵规模下防风控升级，2.2），等待用户通过 API 更新凭据或重新授权。只有服务器**明确拒绝凭据**才算认证失败；网络断连、服务器无响应、以 `* BYE` 拒绝接入（限流/维护）都属于连接类错误，按退避重连，不会把账号停在 `auth_failed` |
| 刷新令牌轮换（oauth2） | 刷新接口返回新 refresh_token 时在同一事务中更新 `credential_enc`（3.5）。更新失败不影响本次已获取的 access_token，下次连接前重试 |
| 解析失败 | 原始 `.eml` 照常保存，记录 `parse_error`，仍然发送通知 |

### 3.5 认证方式
两种方式**均第一期支持**，账号级配置 `auth_type`：

- **password**：密码或授权码。覆盖 QQ、163、Gmail（应用专用密码）、iCloud、Fastmail、自建邮箱等。
- **oauth2**：XOAUTH2。Outlook/Hotmail 个人邮箱已停用基础认证，必须 OAuth2。此路径已在 main.py 中**验证可用**，实现时按此参数表移植。

**XOAUTH2 实现要点**（参数来自 main.py 实测）：

| 项 | 值 |
|---|---|
| 令牌端点 | `https://login.microsoftonline.com/common/oauth2/v2.0/token`（多租户，公共客户端，无 client_secret） |
| Scope | `https://outlook.office365.com/IMAP.AccessAsUser.All offline_access` |
| IMAP 主机 | `outlook.office365.com:993` |
| 认证方式 | IMAP `AUTHENTICATE XOAUTH2`（go-imap 的 SASL 支持已内置） |
| client_id | **每账号独立**的 Azure 应用注册 ID，存于 accounts 表 `client_id` 字段 |

**令牌生命周期**：
1. （重）连接 IMAP 前，用 `refresh_token` 换取 `access_token`（约 1 小时有效）。access_token 只在内存中保存，不落库；IDLE 长连接存续期间无需重复认证，重连时再刷新。
2. 端点可能返回**轮换后的新 refresh_token**（Microsoft 常见行为）：必须立即在数据库事务中重写 `credential_enc`，并同样保持加密。若沿用未持久化的旧令牌，下次刷新可能得到 `invalid_grant`，账号就要重新授权。main.py 的 `--save-token` 机制验证了此流程。
3. 令牌刷新由该账号的 worker 串行执行（账号级互斥）：一个账号的多条文件夹连接不各自刷新，避免并发轮换导致令牌竞态。
4. `invalid_grant`（令牌被吊销、过期、用户改密或撤回授权）→ 账号置 `auth_failed` 并停止重试（3.4），提示用户重新授权。
5. API 永不返回任何凭据（密码、refresh_token、access_token 均不回显）。

> 注意：Graph 与 IMAP 需要不同 audience 的令牌，互不通用（main.py 中 `GRAPH_SCOPE` 与 `IMAP_SCOPE` 分开定义的原因）。令牌缓存按 scope 参数化：Outlook 家族账号走 Graph 传输时取 Graph scope（3.6），其余 IMAP 账号取 IMAP scope；不为同一传输维护两套令牌。

凭据（密码或 refresh_token）用 AES-256-GCM 加密后入库，主密钥来自环境变量 `MAILHUB_SECRET_KEY`。

### 3.6 Graph 传输路径（Outlook 家族主传输）

**背景（M1 真实账号实测）**：对存量 Outlook/Hotmail 授权的令牌探测发现，refresh_token 无法换取 IMAP scope 的令牌（`invalid_scope`）——存量 Azure 应用注册普遍只包含 **Graph 权限**。给应用补 IMAP 权限需要全部账号重新授权，对矩阵不现实。因此 **Graph API 是 Outlook 家族的主传输**，IMAP 路径保留给密码类邮箱与其他服务商。

| 项 | 值 |
|---|---|
| Scope | `https://graph.microsoft.com/.default offline_access` |
| 列表 | `GET /v1.0/me/mailFolders/inbox/messages?$filter=receivedDateTime ge {游标}&$orderby=receivedDateTime asc&$select=id,receivedDateTime&$top=50`（`@odata.nextLink` 分页） |
| 原文 | `GET /v1.0/me/messages/{id}/$value` → 完整 MIME，与 IMAP 路径共用解析入库 |
| 只读 | 仅 list 与 GET `$value`，绝不 PATCH/POST/DELETE（与 3.1 同一原则） |

**增量策略（M2 起 delta 优先）**：`folder_states.delta_link` 持久化 Graph 的 delta 游标。有 delta_link → 直接 `GET {delta_link}`（`@odata.nextLink` 分页滚动至 `@odata.deltaLink`），响应只含上次之后的变更，窗口极窄；无 delta_link（首次/失效）→ `$filter=receivedDateTime ge {游标}` 时间窗列表，完整拉取后以 `delta?$deltatoken=latest` 引导出初始 delta_link。delta_link 失效（410 Gone）自动退回时间窗路径并重建 delta，`last_seen_at` 持续作为兜底游标（取本批最大 `receivedDateTime`，只前进不后退）。两路共用去重与幂等：每页先批量查询已入库的 `graph_id`，只对未见过的消息拉取 `$value`，`(account_id, graph_id)` 唯一索引兜底（边界秒与 delta 重放重拉均不重复入库）；`@removed` 条目（无 `receivedDateTime`）自然跳过——本服务只收不删，远端删除不回写。一个文件夹**完整**拉取成功后才推进游标，中途失败不推进。首次同步与 IMAP 同规：只拉最近 `initial_sync_days` 天。

**消息坐标**：Graph 消息无 UIDVALIDITY/UID，唯一坐标是 `graph_id`（messages 表列，`UNIQUE(account_id, graph_id)`）；IMAP 唯一键的 uid 位由 graph_id 哈希填充以保持两路统一（32 位哈希碰撞时入库事务内线性探测下一个空闲 uid），`.eml` 文件名用 `g-{sha256(graph_id) 前 16 位十六进制}.eml`（早期版本为 `0-{哈希}.eml`，以 `raw_path` 为准）。

**降级与一致性**：Graph API 返回 401 映射 `auth_failed`（令牌失效），429 与 5xx 走常规指数退避；`invalid_scope` 提示需在 Azure 应用注册补权限。worker 按 `accounts.transport` 分流（`imap` | `graph`），两条路径共用同一状态机、入库事务与心跳语义。

---

## 4. 数据模型

DDL 使用 SQLite 与 PostgreSQL 共同支持的语法子集（自增主键在 PG 侧用 `IDENTITY`/序列适配，由迁移脚本处理）。

```sql
-- 邮箱账号
CREATE TABLE accounts (
  id              INTEGER PRIMARY KEY,
  name            TEXT NOT NULL,              -- 显示名，如 "工作邮箱-03"
  email           TEXT NOT NULL UNIQUE,
  imap_host       TEXT NOT NULL,
  imap_port       INTEGER NOT NULL DEFAULT 993,
  use_tls         INTEGER NOT NULL DEFAULT 1,
  username        TEXT NOT NULL,              -- oauth2 账号默认取 email
  auth_type       TEXT NOT NULL DEFAULT 'password',  -- password | oauth2
  client_id       TEXT,                       -- auth_type='oauth2' 时必填（Azure 应用注册 ID）
  transport       TEXT NOT NULL DEFAULT 'imap',     -- imap | graph（Outlook 家族默认 graph，3.6）
  credential_enc  BLOB NOT NULL,              -- 加密后的密码（password）或 refresh_token（oauth2）
  folders         TEXT NOT NULL DEFAULT '["INBOX"]', -- JSON 数组
  poll_interval   INTEGER NOT NULL DEFAULT 60,       -- 秒，无 IDLE 时使用
  notify_enabled  INTEGER NOT NULL DEFAULT 0,        -- 新邮件 Webhook 提醒开关（账号级，默认不提醒，5.1）
  enabled         INTEGER NOT NULL DEFAULT 1,
  status          TEXT NOT NULL DEFAULT 'pending',
                  -- pending | syncing | idle | polling | error | auth_failed | disabled
  last_error      TEXT,
  last_sync_at    DATETIME,                   -- 最后成功收件检查时间（账号心跳，3.2）：无论有无新邮件，成功即更新
  created_at      DATETIME NOT NULL,
  updated_at      DATETIME NOT NULL
);
CREATE INDEX idx_accounts_status ON accounts(status);   -- 账号管理页按状态筛选

-- 文件夹同步游标
CREATE TABLE folder_states (
  account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder       TEXT NOT NULL,
  uidvalidity  INTEGER NOT NULL,
  last_uid     INTEGER NOT NULL DEFAULT 0,
  last_seen_at DATETIME,                      -- Graph 路径时间游标（receivedDateTime 兜底，3.6）
  delta_link   TEXT,                          -- Graph delta 游标（3.6；失效退回 last_seen_at）
  PRIMARY KEY (account_id, folder)
);

-- 邮件
CREATE TABLE messages (
  id            INTEGER PRIMARY KEY,          -- 自增，同时作为 API 增量游标
  account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder        TEXT NOT NULL,
  uidvalidity   INTEGER NOT NULL,
  uid           INTEGER NOT NULL,
  graph_id      TEXT,                         -- Graph 路径消息 id（3.6）；IMAP 路径为 NULL
  message_id    TEXT,                         -- Message-ID 头
  subject       TEXT,
  from_addr     TEXT,                         -- "Name <a@b.com>"
  to_addrs      TEXT,                         -- JSON 数组
  cc_addrs      TEXT,                         -- JSON 数组
  sent_at       DATETIME,                     -- Date 头（发件人声明的发送时间）
  received_at   DATETIME NOT NULL,            -- 邮件真实到达时间：Graph receivedDateTime / IMAP INTERNALDATE，缺失退 Date 头，再退入库时刻
  body_text     TEXT,
  body_html     TEXT,
  snippet       TEXT,                         -- 正文前 200 字
  size          INTEGER,
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_read       INTEGER NOT NULL DEFAULT 0,   -- 仅本地状态，不回写服务器
  truncated     INTEGER NOT NULL DEFAULT 0,
  parse_error   TEXT,
  raw_path      TEXT NOT NULL,                -- .eml 文件相对路径
  UNIQUE (account_id, folder, uidvalidity, uid)
);
CREATE UNIQUE INDEX idx_messages_graph ON messages(account_id, graph_id) WHERE graph_id IS NOT NULL;
CREATE INDEX idx_messages_account_time ON messages(account_id, received_at DESC);
CREATE INDEX idx_messages_message_id ON messages(account_id, message_id);
-- 未读计数的高频查询（账号列表徽标、全局未读数）走部分索引，避免全表扫
CREATE INDEX idx_messages_unread ON messages(account_id) WHERE is_read = 0;

-- 附件（只存元数据，内容按需从 .eml 中解析）
CREATE TABLE attachments (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename     TEXT,
  content_type TEXT,
  size         INTEGER,
  part_index   INTEGER NOT NULL               -- 在 MIME 结构中的序号
);
-- 外键子表索引（006 迁移）：SQLite 的外键子表没有索引时，父表每删一行都要全表扫子表——
-- 删邮件/删账号/删 webhook 的级联删除会随数据量线性变慢。三个子表各建一个。
CREATE INDEX idx_attachments_message ON attachments(message_id);

-- 账号标签（每账号多标签、按标签过滤；004 迁移）
CREATE TABLE tags (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);
CREATE TABLE account_tags (
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  tag_id     INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (account_id, tag_id)
);
CREATE INDEX idx_account_tags_tag ON account_tags(tag_id);

-- 全局配置覆盖（Web 在线修改，6.6/9.1；005 迁移）。key 白名单由代码层维护，
-- 仅收运行时可变项；凭据/进程级项不落此表。value 为空 = 清除覆盖。
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- Webhook 配置
CREATE TABLE webhooks (
  id          INTEGER PRIMARY KEY,
  url         TEXT NOT NULL,
  secret      TEXT NOT NULL,                  -- 用于签名
  account_ids TEXT,                           -- JSON 数组，NULL 表示所有账号；与账号 notify_enabled 取 AND（5.1）
  include_body INTEGER NOT NULL DEFAULT 0,    -- 载荷是否带 body_text
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  DATETIME NOT NULL
);

-- Webhook 投递记录（同时充当投递队列）
CREATE TABLE webhook_deliveries (
  id            INTEGER PRIMARY KEY,
  webhook_id    INTEGER NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  status        TEXT NOT NULL DEFAULT 'pending',  -- pending | success | failed
  attempts      INTEGER NOT NULL DEFAULT 0,
  next_retry_at DATETIME,
  response_code INTEGER,
  last_error    TEXT,
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL
);
CREATE INDEX idx_deliveries_pending ON webhook_deliveries(status, next_retry_at);
CREATE INDEX idx_deliveries_message ON webhook_deliveries(message_id);  -- 外键子表索引，见 attachments
CREATE INDEX idx_deliveries_webhook ON webhook_deliveries(webhook_id);
```

说明：
- 附件不单独落盘，下载时从 `.eml` 实时解析，省去一套存储逻辑。日后若附件下载频繁再优化。
- **oauth2 账号的 `credential_enc` 是"活"数据**：令牌轮换（3.5）时服务会加密重写该字段。API 层不暴露此差异，凭据一律不回显。
- **数据量级**：低频收件——千级账号中大部分数天一封，日增约百级～千级、年增百万行内，SQLite 从容应对，上述索引留足余量。`q` 关键字 LIKE 搜索在此量级可接受；数据量显著增长后再引入 FTS / 全文索引（M4）。
- 千级账号的**未读徽标**是高频读：列表页直接走 `idx_messages_unread` 聚合，不另设计数表；若仍成瓶颈再加定时聚合缓存。

---

## 5. Webhook 设计

### 5.1 触发时机
目前只有一个事件：`message.received`（新邮件入库）。触发需同时满足：

1. 账号开启了提醒开关 `notify_enabled`（**账号级可选项，默认不提醒**，6.2）；
2. 该邮件不是首次同步的历史邮件（3.4）；
3. 存在 `enabled=1` 且 `account_ids` 匹配（NULL = 全部账号）的 webhook。

即"**账号开关（默认关）AND webhook 目标匹配**"两层过滤：账号开关决定*要不要提醒这个邮箱*，webhook 配置决定*提醒发给谁*。多数账号只为收取存档，保持默认静默即可。

### 5.2 请求格式
```http
POST {webhook.url}
Content-Type: application/json
X-MailHub-Event: message.received
X-MailHub-Delivery: 1024
X-MailHub-Timestamp: 1794833760
X-MailHub-Signature: sha256=<hex(HMAC_SHA256(secret, timestamp + "." + body))>
```

```json
{
  "event": "message.received",
  "delivery_id": 1024,
  "message": {
    "id": 5321,
    "account_id": 2,
    "account_email": "me@example.com",
    "folder": "INBOX",
    "subject": "您的验证码",
    "from": "Service <noreply@service.com>",
    "to": ["me@example.com"],
    "sent_at": "2026-11-16T12:55:30Z",
    "received_at": "2026-11-16T12:55:41Z",
    "snippet": "您的验证码是 123456，10 分钟内有效……",
    "has_attachments": false
  }
}
```

- 载荷只带元数据和摘要，**不带完整正文和附件**。下游需要时用 `message.id` 调 API 获取，这样载荷小、投递快。
- 主要场景若是让下游直接处理正文（如提取验证码），创建 webhook 时设 `include_body: true`，载荷附带 `body_text`（4 中 `webhooks.include_body`）。

### 5.3 投递与重试
- 投递器循环取 `status='pending' AND next_retry_at <= now` 的记录，并发数由 `MAILHUB_WEBHOOK_CONCURRENCY` 配置（默认 4，低频收件下足够），请求超时 10 秒。
- 返回 2xx 视为成功，其他情况重试。
- 重试间隔：10 秒、1 分钟、5 分钟、30 分钟、2 小时，共 5 次，之后标记为 `failed`。
- 失败记录可通过 API 手动重发。
- 语义为**至少一次**投递，下游用 `delivery_id` 或 `message.id` 做幂等。
- 投递记录保留 30 天后自动清理。

---

## 6. API 设计

> **完整 API 参考（全部端点的参数、请求/响应示例与错误码）见根目录 `API.md`**——与本节同步维护；
> 本节保留契约概览与设计动机。

### 6.1 通用约定
- 前缀：`/api/v1`
- 认证：`Authorization: Bearer <token>`，Token 由环境变量 `MAILHUB_API_TOKEN` 配置。单管理员场景一个 Token 足够。
- 格式：JSON，时间为 ISO 8601 UTC。
- 错误：`{"error": {"code": "account_not_found", "message": "..."}}`，配合标准 HTTP 状态码。
- **列表接口一律分页**：账号和邮件在矩阵规模下都是千/百万级，禁止无界返回。

### 6.2 账号管理
| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/accounts` | 添加单个账号。**先同步验证 IMAP 登录**，失败直接返回 400，成功后入库并启动 worker |
| `POST` | `/accounts/batch` | **批量导入**。支持 JSON 数组或 `text/plain` 粘贴 `邮箱----密码----client_id----令牌` 文本（与 main.py 账号文件逐字兼容；Outlook/Hotmail 域名自动套用 oauth2 + **graph 传输**预设，密码段不存储）。只入库（`pending`），后台错峰验证（2.2），立即返回统计；**已存在邮箱跳过不覆盖**（`skipped` 计数）；JSON 模式与导出格式同构，迁移文件可直接导入（名称/文件夹/轮询/提醒/标签一并还原） |
| `GET` | `/accounts/export` | **导出（迁移）**。裸 JSON 数组与批量导入 JSON 模式同构，POST 回 `/accounts/batch` 即闭环迁移。默认仅配置；`include_credentials=true` 附解密凭据明文——这是"凭据绝不回显"的**显式例外**（迁移必需），文件明文由调用方妥善保管；`q`/`status`/`tag` 筛选同列表。不含已收邮件 |
| `GET` | `/accounts` | 账号列表（分页）。参数：`page`/`limit`、`q`（名称/邮箱模糊）、`status`（如 `auth_failed`）、`tag`（可重复，OR 命中）。响应每项含 `tags: []` |
| `GET` | `/tags` | 全部标签与引用账号数（按引用数降序） |
| `GET` | `/accounts/{id}` | 账号详情。回显可编辑字段当前值（`folders`/`poll_interval`/`username`/`imap_port`/`client_id` 等，供 Web 单账号设置弹窗使用）；密码/refresh_token **绝不回显** |
| `PATCH` | `/accounts/{id}` | 修改配置、凭据或启停；连接类变更（host/用户名/凭据/文件夹/间隔/重新启用）触发 worker 重启，`notify_enabled`/`name`/`tags` 等纯元数据即时生效不动 worker；`tags`（字符串数组）全量替换；凭据字段（`password`/`refresh_token`）仅在请求中给出时加密覆盖——`auth_failed` 账号提交新凭据后自动重启重试 |
| `PATCH` | `/accounts/batch` | 批量操作：启停 `{"ids": [...], "enabled": false}`、批量开关提醒 `{"ids": [...], "notify_enabled": true}` |
| `DELETE` | `/accounts/{id}` | 删除账号（邮件记录随外键级联删除）。`?purge=true` 时同时删除磁盘上的原始 `.eml` 文件 |
| `POST` | `/accounts/{id}/sync` | **手动收取**：立即执行一次增量拉取（插入信号，不重置轮询/IDLE 节奏，3.2）；`error`/`auth_failed` 账号也允许触发，视为一次显式连接尝试 |
| `POST` | `/accounts/{id}/reauth` | **重新获取令牌**（M4，仅 oauth2 账号）：body `{"code": "授权码或授权完成后的完整跳转 URL"}`。授权码流——用户浏览器打开 Microsoft 授权页（client_id 用账号自身的，scope 按传输分流）登录该邮箱，把跳转地址粘回；服务端兑换**全新 refresh_token** 加密持久化并立即重启 worker。code 一次性、约 10 分钟有效。注：密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），授权码流是 MSA 账号续 token 的路径 |
| `POST` | `/accounts/sync` | 批量手动收取：`{"ids": [...]}`，仍经错峰调度器排队执行（单账号触发立即执行），避免瞬间千级并发 |
| `POST` | `/accounts/test` | 仅测试连接，不保存 |

批量导入（JSON 形式）：
```json
POST /accounts/batch
{
  "accounts": [
    {"email": "a1@outlook.com", "auth_type": "oauth2", "client_id": "…", "refresh_token": "…"},
    {"email": "a2@outlook.com", "auth_type": "oauth2", "client_id": "…", "refresh_token": "…"}
  ]
}
```

首次同步窗口为全局配置 `MAILHUB_INITIAL_SYNC_DAYS`（9.1，默认 90 天），对单个添加与批量导入一致生效。

批量导入（文本形式，Outlook/Hotmail 批量导入的主路径，与 main.py 账号文件逐字兼容；第二段密码在 XOAUTH2 流程中不使用，可为任意占位符，**导入时不存储**）：
```
POST /accounts/batch
Content-Type: text/plain

a1@outlook.com----unused----00000000-…----O.XXXX…
a2@hotmail.com----unused----00000000-…----O.XXXX…
```

响应：
```json
{"created": 998, "failed": [{"line": 3, "error": "格式错误：应有 4 个字段"}]}
```

添加单个账号（password）：
```json
{
  "name": "工作邮箱",
  "email": "me@example.com",
  "imap_host": "imap.example.com",
  "imap_port": 993,
  "username": "me@example.com",
  "password": "app-password",
  "folders": ["INBOX"]
}
```

添加单个账号（oauth2，Outlook）：
```json
{
  "name": "Outlook-07",
  "email": "me07@outlook.com",
  "imap_host": "outlook.office365.com",
  "auth_type": "oauth2",
  "client_id": "00000000-0000-0000-0000-000000000000",
  "refresh_token": "O.XXXXXXXX...",
  "folders": ["INBOX"]
}
```
oauth2 账号不传 `password`/`username`（username 默认取 email），凭据按 `auth_type` 二选一。`auth_failed` 状态的账号通过 `PATCH` 提交新的 refresh_token 恢复。

账号响应（列表项，监控所需字段都在这里）：
```json
{
  "id": 2,
  "name": "Outlook-07",
  "email": "me07@outlook.com",
  "imap_host": "outlook.office365.com",
  "auth_type": "oauth2",
  "transport": "graph",
  "enabled": true,
  "notify_enabled": false,
  "status": "idle",
  "last_error": null,
  "last_sync_at": "2026-11-16T12:55:41Z",
  "last_message_at": "2026-11-16T09:12:03Z",
  "message_count": 1280,
  "unread_count": 12
}
```
`last_sync_at` 为心跳语义（3.2）：最近一次**成功收件检查**的时间，无论有无新邮件都会推进；`last_message_at` 为该账号最新一封邮件的入库时间（messages 聚合，不单独存列）。两者配合即可回答"账号活着吗"与"最近有信吗"。

服务内置常见邮箱的 IMAP 预设，根据邮箱域名自动填充 `imap_host`/`imap_port` 并识别认证方式与传输：Outlook 家族（`outlook.com`、`hotmail.com`、`live.com`、`outlook.com.cn` 等）→ `outlook.office365.com` + oauth2 + **graph 传输**（存量授权仅含 Graph 权限，3.6）；Gmail、QQ、163、iCloud 等 → 对应主机 + password + imap 传输。批量导入的文本行里没有 host/auth 信息，预设让"邮箱域名 → 服务器与认证方式"自动成立，这正是 Outlook/Hotmail 批量导入的依赖。

### 6.3 邮件
| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/messages` | 邮件列表（不含正文），分页。参数含 `tag`（可重复，按所属账号标签 OR 过滤，可与 `account_id` 组合）；响应每项含所属账号的 `tags` 快照。**排序**（M4）：`sort=time|account` + `order=asc|desc` + `offset` 偏移翻页——`time` 按 `received_at`，`account` 按账号名称（组内时间倒序）；不传 `sort` 时保持默认（id 倒序 + `before_id`/`after_id` 游标分页），两套分页互斥 |
| `GET` | `/messages/{id}` | 邮件详情，含 `body_text`、`body_html` 和附件列表 |
| `GET` | `/messages/{id}/raw` | 下载原始 `.eml` |
| `GET` | `/messages/{id}/attachments/{att_id}` | 下载附件 |
| `PATCH` | `/messages/{id}` | 修改本地已读状态 `{"is_read": true}` |
| `POST` | `/messages/batch-read` | **批量标记已读**：`{"ids": [1,2,3]}`，或 `{"account_id": 2}` 标记该账号全部（本地状态，不回写服务器） |
| `POST` | `/messages/batch-delete` | **批量删除本地副本**：`{"ids": [1,2,3]}`，或 `{"account_id": 2}` 清空该账号全部。同步删行与关联投递记录，`.eml` 文件异步删除（失败由清理任务回收孤儿文件）；不影响邮箱服务器 |
| `DELETE` | `/messages/{id}` | 删除本地副本（不影响邮箱服务器） |

`GET /messages` 查询参数：
| 参数 | 说明 |
|---|---|
| `account_id` | 按账号筛选 |
| `is_read` | `true` 或 `false` |
| `q` | 关键字，对主题/发件人/摘要/**正文**做 `LIKE` 匹配（全文搜索，中文子串友好；此量级可接受，见 4；数据量显著增长后升级 FTS5） |
| `since`、`until` | 按 `received_at` 的时间范围 |
| `after_id` | **增量拉取游标**：返回 `id > after_id` 的邮件，按 id 升序 |
| `limit` | 默认 50，最大 200 |
| `before_id` | 向前翻页（默认按 id 倒序展示时使用） |

`after_id` 给不方便接收 Webhook 的客户端提供了轮询方式：记住上次最大的 id，定期请求即可，不重不漏。

### 6.4 Webhook 管理
| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/webhooks` | 创建。`secret` 不传则自动生成，仅在创建时返回一次 |
| `GET` | `/webhooks` | 列表 |
| `PATCH` | `/webhooks/{id}` | 修改 |
| `DELETE` | `/webhooks/{id}` | 删除 |
| `POST` | `/webhooks/{id}/test` | 发送一条测试事件 |
| `GET` | `/webhooks/{id}/deliveries` | 投递记录，可按 `status` 筛选 |
| `POST` | `/deliveries/{id}/retry` | 手动重发 |

### 6.5 监控
| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/healthz` | 存活检查，无需认证 |
| `GET` | `/api/v1/events` | **SSE 实时事件流**（M4）：`?token=` 认证（EventSource 无法带 Authorization 头）；新邮件入库推送 `message.new` 事件，载荷为**单封邮件摘要**（`id/subject/from/account`，供浏览器通知）；**历史邮件（首次同步批量）不推**，防通知轰炸（同 5.1）；30s 心跳保活、断线 5s 自动重连；前端据此即时刷新并在后台标签页弹浏览器通知（通知开关在邮件页工具栏，用户手势授权） |
| `GET` | `/api/v1/status` | 总览：各状态账号数（`auth_failed`/`error` 单列）、今日收信数、待投递与失败 webhook 数、磁盘占用与水位告警 |

矩阵规模下状态接口是主要巡检入口：`auth_failed` 数量上升通常意味着令牌批量过期或风控事件，Web 仪表盘直接呈现（7）。

### 6.6 全局配置（M4）
| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/settings` | 读全部运行时可变项（9.1 标 ◆ 的五项）：每项返回 `{value: 生效值, overridden: 是否被 DB 覆盖}` |
| `PUT` | `/api/v1/settings` | 在线修改：body 为 `{"retention_days": 90, ...}`；值为 `null` 清除该项覆盖（回退环境变量/默认）。白名单 + 范围校验；保存后立即失效 worker 侧配置缓存（30s TTL） |

配置优先级 **DB（settings 表）> 环境变量 > 默认值**。各项生效时机：`retention_days` 下一轮每日维护；`initial_sync_days` / `poll_interval_default` 对之后新增/导入的账号；`fallback_interval` IDLE 账号下一轮等待周期；`idle_budget_per_host` 新建立的 IDLE 会话（存量 IDLE 连接不受影响）。凭据与进程级项（API Token / SECRET_KEY / LISTEN / DATA_DIR）不在此接口——修改需重启。

---

## 7. Web 界面

矩阵规模的账号数让"简单三页"升级为四个工作区，全部基于上述 API：

1. **邮件列表**（默认页）
   - 左侧账号栏：搜索框（千级账号不能靠滚动找）、未读徽标（`idx_messages_unread` 聚合）、"全部账号"聚合视图；列表**虚拟滚动**。
   - 右侧邮件流：按 `received_at` 倒序、未读加粗、虚拟滚动，支持关键字搜索与未读筛选。
   - **已读交互**：打开详情默认标记已读（可在设置关闭）；列表行单击切换已读；工具栏"全部标为已读"（`batch-read`）。
   - **删除交互**：列表多选（复选框、Shift 范围选、"全选当前筛选结果"），工具栏"删除选中"；账号栏提供"清空此账号邮件"（`batch-delete`，二次确认）。
   - 明示语义：已读与删除均只作用于 **MailHub 本地副本**，绝不影响邮箱服务器与其他客户端。
2. **邮件详情**：HTML 正文放入 `sandbox` iframe 渲染，**默认不加载远程图片**（防追踪像素），附件可下载。
3. **账号管理**：分页表格 + 搜索 + 状态筛选（`auth_failed`、`error` 高亮）；批量启停、批量删除、勾选后**立即收取**（6.2 批量手动收取，后台错峰执行，进度可见）；表格含**提醒开关**列（默认关，支持勾选后批量开启/关闭，对应 5.1 的 `notify_enabled`）；`auth_failed` 账号可逐个或批量提交新凭据。
   - **批量导入 Outlook/Hotmail**：粘贴或上传 `.txt`，每行 `邮箱----密码----client_id----令牌`（与 main.py 账号文件逐字兼容）。按域名自动识别 Outlook/Hotmail 并套用 oauth2 预设；密码段不使用、不存储。导入立即显示统计（成功数、失败行号与原因），后台验证进度实时可见（账号状态从 `pending` 逐个变为 `idle` / `polling` / `error`）。
   - 表格显示**最后收取时间**（`last_sync_at`，3.2 心跳语义）与**最新邮件时间**（`last_message_at`）；超过约 15 分钟无心跳的账号高亮，用于区分"没有邮件"与"收取故障"。
   - **单账号设置弹窗**：行操作"设置"打开——名称、轮询间隔（低频邮箱建议调大）、收取文件夹（多文件夹 IMAP 不走 IDLE）、标签、IMAP 用户名/主机（Graph 账号不显示）；凭据区按认证方式显示（密码或 client_id + refresh_token），**留空 = 不修改**（服务端不回显凭据）；`auth_failed` 账号在弹窗顶部显示错误与新凭据入口，保存后自动重启收取。只提交实际变化的字段。
   - **重新获取令牌**（oauth2 账号设置弹窗内，M4）：三步向导——①打开 Microsoft 授权页（用账号 client_id 构造，浏览器登录该邮箱）②把授权后跳转的完整地址粘回 ③服务端授权码兑换全新 refresh_token（加密存储、立即重启收取）。适用于令牌过期/失效的账号抢救。
4. **仪表盘**：今日收信曲线、账号状态分布、投递积压/失败、磁盘水位。
5. **设置**（M4）：全局配置在线修改（6.6 五项运行时可变项）；显示每项生效值、是否被数据库覆盖（"已覆盖"标记 + 恢复默认）与生效时机说明；顶部说明优先级与"凭据/进程级项需改环境变量重启"。

登录方式：输入 API Token，存入浏览器 localStorage。

---

## 8. 安全

| 项 | 措施 |
|---|---|
| 传输 | 服务本身监听 HTTP，由 Caddy 或 Nginx 反代提供 HTTPS |
| 邮箱凭据 | AES-256-GCM 加密存储（密码与 refresh_token 同等对待），API 不回显；**上千组邮箱凭据集中存放，`MAILHUB_SECRET_KEY` 即全库钥匙，须妥善保管** |
| API 认证 | Bearer Token，常量时间比较 |
| IMAP 连接 | 默认强制 TLS 并校验证书 |
| HTML 邮件 | sandbox iframe，禁用脚本，默认屏蔽外链资源 |
| Webhook | HMAC 签名加时间戳，下游可校验来源并防重放 |
| 数据 | 邮件含敏感信息，数据目录权限设为 700 |

---

## 9. 配置与部署

### 9.1 配置（环境变量）
| 变量 | 默认值 | 说明 |
|---|---|---|
| `MAILHUB_API_TOKEN` | 必填 | API 访问令牌 |
| `MAILHUB_SECRET_KEY` | 必填 | 凭据加密主密钥（32 字节，base64） |
| `MAILHUB_DATABASE_URL` | 空 | 空 → SQLite（data 目录）；否则 PostgreSQL DSN。矩阵规模建议 PG（2.1） |
| `MAILHUB_LISTEN` | `:8080` | 监听地址 |
| `MAILHUB_DATA_DIR` | `./data` | 数据库（SQLite 时）和邮件存储目录 |
| `MAILHUB_MAX_MESSAGE_SIZE` | `50MB` | 单封邮件大小上限 |
| `MAILHUB_RETENTION_DAYS` ◆ | `0` | 邮件保留天数，0 = 永久。低频收件下默认永久保留无存储压力（9.4），需要时再设 |
| `MAILHUB_INITIAL_SYNC_DAYS` ◆ | `90` | 首次添加账号的初始拉取窗口（天，约 3 个月）。该批为历史邮件，不触发 webhook（5.1） |
| `MAILHUB_IDLE_BUDGET_PER_HOST` ◆ | `0`（不限） | 单 IMAP 主机 IDLE 长连接上限，超出降级轮询（2.2） |
| `MAILHUB_CONNECT_STAGGER` | `2s` | 批量启动时账号间错峰间隔，每账号再随机抖动（2.2） |
| `MAILHUB_POLL_INTERVAL_DEFAULT` ◆ | `60` | 账号轮询间隔默认值（秒），批量导入时填充到各账号 `poll_interval`；账号级可单独修改 |
| `MAILHUB_FALLBACK_INTERVAL` ◆ | `300` | IDLE 模式兜底拉取间隔（秒），同时是心跳推进上限（3.2） |
| `MAILHUB_WEBHOOK_CONCURRENCY` | `4` | Webhook 投递并发（5.3） |

标 ◆ 的五项为**运行时可变项**：除环境变量外，可在 Web 设置页 / `PUT /api/v1/settings`（6.6）在线修改，
存入 `settings` 表，优先级 **DB 覆盖 > 环境变量 > 默认值**；保存后按各项说明的时机生效（无需重启）。
未标记项（凭据、监听地址、数据目录、错峰/并发等）为进程级配置，修改需重启。

### 9.2 部署
镜像构建见仓库 `Dockerfile`（多阶段：node 前端 → Go 静态二进制 → distroless 运行时）；凭据经根目录 `.env`（`KEY=value`，无 export 前缀，勿提交）注入。
```yaml
# docker-compose.yml（仓库已交付同款）
services:
  mailhub:
    build: .
    image: mailhub:latest
    restart: unless-stopped
    ports: ["18099:18099"]
    volumes: ["mailhub-data:/data"]
    ulimits: { nofile: 8192 }        # 千级 IMAP 长连接需要（2.2）
    environment:
      MAILHUB_DATA_DIR: /data
      MAILHUB_LISTEN: :18099
      MAILHUB_API_TOKEN: ${MAILHUB_API_TOKEN}
      MAILHUB_SECRET_KEY: ${MAILHUB_SECRET_KEY}
      MAILHUB_RETENTION_DAYS: 90     # 0/省略 = 永久（9.1）
      MAILHUB_DATABASE_URL: ${MAILHUB_DATABASE_URL}   # 留空则用 SQLite

  postgres:                           # 可选：矩阵规模建议启用
    image: postgres:17
    volumes: ["./pgdata:/var/lib/postgresql/data"]
    environment:
      POSTGRES_DB: mailhub
      POSTGRES_PASSWORD: ${PG_PASSWORD}
```

### 9.3 数据目录
```
data/
├── mailhub.db            # SQLite（DATABASE_URL 为空时）
└── mails/
    └── {account_id}/{yyyy-mm}/{uidvalidity}-{uid}.eml
```
备份时打包整个 `data/` 目录即可；使用 PostgreSQL 时另备数据库。

### 9.4 资源预估（按 1000 账号、低频收件）
| 项 | 估算 |
|---|---|
| IMAP 长连接 | 约 1000 条（仅 INBOX），是**主要资源**；Go 进程内存约 256～512MB，fd 上限见 9.2 |
| 邮件量 | 大部分账号数天一封，日增约百级～千级封 |
| 磁盘 | 平均 30KB/封（原文 + DB 双份）≈ 日增几十 MB 以内；永久保留一年仅数 GB |
| 建议 | 2C2G 起步、2C4G 留余量，50GB SSD + SQLite 即可；PostgreSQL 非必需 |

---

## 10. 开发里程碑

| 阶段 | 内容 | 产出 |
|---|---|---|
| **M1 核心同步** | 表结构（SQLite/PG 兼容）、单账号 API + **批量导入（含 main.py 文本格式）**、password + XOAUTH2 双认证（令牌刷新按 main.py 参数移植）、**Graph 传输路径**（Outlook 家族主传输，存量授权实测仅含 Graph 权限，3.6）、错峰同步管理、IMAP/Graph 增量同步（轮询）、邮件解析入库 | 批量导入上千账号并查到邮件 |
| **M2 实时与通知** | IMAP IDLE、IDLE 预算与降级、断线重连、Webhook 投递和重试、投递记录 API、Graph delta query（缩窄列表窗口，3.6） | 收信后秒级通知 |
| **M3 界面与完善** | Web UI（邮件查看/**已读标记**/**批量删除**/账号管理/仪表盘）、附件下载、retention 清理、状态接口、Docker 镜像 | 可日常使用 |
| **M4 按需扩展** | ~~全文搜索~~（已交付：`q` 覆盖正文，LIKE 路线；FTS5 待数据量显著增长后升级）、~~SSE 实时推送~~（已交付：`/api/v1/events`，入库即推 `message.new`）、其他服务商 Graph 类 REST 路径（需对应凭据与实测，真正按需） | 视实际需要再做 |

---

## 11. 关键设计决策小结

1. **单进程撑到千级账号**：连接错峰、单主机 IDLE 预算（超出降级轮询）、认证失败即停，三者共同消化矩阵规模的风控与服务商限制，不引入微服务（2.2）。
2. **SQLite 覆盖全部规划规模**：低频收件（日增百级～千级、年增百万行内）下零运维足够；DDL 兼容 PostgreSQL，量级显著变化时一条 `DATABASE_URL` 切换（2.1）。
3. **IDLE 加轮询兜底**：兼顾实时性和可靠性；轮询同时是 IDLE 预算超限时的正式降级路径，而非仅是后备。
4. **对远端完全只读**（`EXAMINE` 和 `BODY.PEEK`）：已读与删除（`batch-read` / `batch-delete`）只作用于本地副本，绝不干扰邮箱服务器与其他客户端。
5. **保留原始 `.eml`**：解析逻辑有 bug 或以后要加功能时，可以随时重新解析，数据不丢；低频收件下磁盘成本极低，默认永久保留。
6. **Webhook 轻载荷加 API 取详情，且默认静默**：通知快，下游按需取正文；提醒是账号级开关（`notify_enabled`，默认关，5.1）——矩阵中多数账号只为收取存档，默认全员广播既是噪音也是信息泄露面。
7. **认证失败即停**：密码错误或 `invalid_grant` 都立即停止重试，矩阵规模下单账号的风控可能危及同 IP 全体账号。
8. **刷新令牌是"活"凭据**：oauth2 账号的 refresh_token 会被服务端轮换，轮换即在同一事务中持久化（main.py 的 `--save-token` 已验证必要性）。
9. **Graph 是 Outlook 家族主传输**：存量授权仅含 Graph 权限（IMAP scope → invalid_scope，M1 实测），补权限需全员重新授权对矩阵不现实；Graph `$value` 取 MIME 原文与 IMAP 共用解析入库，`graph_id` 唯一索引保证幂等（3.6）。
10. **批量导入后台验证**：导入只入库立即返回，验证与首同步按错峰节奏进行（2.2），`main.py` 的账号文件格式直接兼容（6.2）。
11. **`last_sync_at` 即账号心跳**：成功收件检查即更新（无论有无新邮件），失败不更新——矩阵运营据此区分"无邮件"与"账号异常"，密码段不存储则是最小凭据原则在批量导入上的体现。

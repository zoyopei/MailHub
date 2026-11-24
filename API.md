# MailHub API 参考

REST API 完整参考（以当前实现为准；架构层面的契约与动机见 `DESIGN.md` 第 5/6 节）。

- 基础前缀：`/api/v1`，请求与响应均为 JSON（附件与 `.eml` 下载除外）。
- 时间格式：RFC3339 UTC（如 `2026-11-17T05:33:53Z`）。
- 监听地址默认 `:8080`（`MAILHUB_LISTEN` 可改）。

## 认证

| 方式 | 适用 |
|---|---|
| `Authorization: Bearer <MAILHUB_API_TOKEN>` | 全部 REST 端点（常量时间比较） |
| `?ticket=<一次性票据>` | **仅** `GET /api/v1/events`（SSE）——EventSource 无法携带自定义头。票据由 `POST /api/v1/events/ticket` 签发，60 秒有效、用一次即作废（见 §6） |
| `?token=<MAILHUB_API_TOKEN>` 查询参数 | **仅** `GET /api/v1/events`（SSE）——兼容保留。主令牌会出现在 URL 与反向代理访问日志里，新脚本请用 ticket |

`GET /healthz` 无需认证。

所有响应（API、SSE、Web 静态页、附件下载）都带安全头：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、
`Content-Security-Policy: object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'`。

## 错误格式

处理器产生的错误响应统一为：

```json
{ "error": { "code": "bad_request", "message": "人类可读的中文说明" } }
```

常见状态码：`400` 参数或请求错误 / `401` 认证失败 / `404` 资源不存在 / `409` 冲突（邮箱已存在）/ `503` 依赖未启用；
`202` 表示已受理的异步操作（手动收取）。

两个例外由路由框架直接返回，响应体**不是**上面的 JSON：路径不存在 → `404`（纯文本 `404 page not found`）；
路径存在但方法不对 → `405`（空响应体）。`/api/v1` 下先认证后路由，令牌不对时这两类请求也返回 `401`。

---

## 1. 健康与状态

### `GET /healthz`
存活检查（无需认证）。响应：`{"status":"ok"}`

### `GET /api/v1/status`
服务总览。可选参数 `day_start`（RFC3339，如 `2026-11-24T00:00:00+08:00`）："今日收信"的起点，缺省取**服务器本地时区**
的今天零点。容器里不设 `TZ` 时服务器时区是 UTC——北京时间的早上 8 点数字就归零了。Web 界面因此始终传浏览器所在时区的
本地零点；脚本要按别的时区统计时也请自己传。格式非法 → `400 bad_request`。`disk_usage_bytes` 统计整个数据目录（缓存 30 秒）。

响应：

```json
{
  "accounts_by_status": { "polling": 18, "idle": 1, "auth_failed": 1 },
  "messages_today": 3,
  "deliveries_pending": 0,
  "deliveries_failed": 0,
  "disk_usage_bytes": 10485760
}
```

---

## 2. 账号管理

### `GET /api/v1/accounts`
账号列表（分页，**一律分页**——矩阵规模硬性要求）。

| 参数 | 说明 |
|---|---|
| `page` / `limit` | 分页（limit 默认 50，最大 200；超过按 200 算） |
| `q` | 名称/邮箱模糊搜索 |
| `status` | 状态筛选：`pending` / `syncing` / `idle` / `polling` / `error` / `auth_failed` / `disabled` |
| `tag` | 标签筛选，**可重复**（多个 tag 为 OR 命中） |

响应每项（凭据**永不回显**；`client_id` 非机密，供编辑回显）：

```json
{
  "accounts": [{
    "id": 1, "name": "生产-主邮箱", "email": "a@outlook.com",
    "imap_host": "outlook.office365.com", "imap_port": 993,
    "username": "a@outlook.com", "auth_type": "oauth2",
    "client_id": "9e5f94bc-…", "transport": "graph",
    "folders": ["INBOX", "JunkEmail"], "poll_interval": 300,
    "enabled": true, "notify_enabled": false,
    "status": "polling", "last_error": null,
    "last_sync_at": "2026-11-17T12:29:24Z", "last_message_at": "2026-11-17T05:33:53Z",
    "message_count": 5, "unread_count": 0, "tags": ["生产", "重要"]
  }],
  "total": 20
}
```

> `last_sync_at` 是**心跳**：任何一次成功收取（无论有无新邮件）都更新。

### `POST /api/v1/accounts`
添加单个账号，**先同步验证登录**，失败返回 400（不入库）。

```json
{
  "email": "a@outlook.com",
  "name": "可选显示名",
  "auth_type": "oauth2",
  "client_id": "9e5f94bc-…",
  "refresh_token": "O.XXXX…",
  "transport": "graph",
  "folders": ["INBOX"],
  "poll_interval": 0,
  "notify_enabled": false,
  "tags": ["生产"]
}
```

- `auth_type`：`password`（用 `password` 字段）或 `oauth2`（用 `client_id` + `refresh_token`）。
- `transport`：`imap` | `graph`；缺省按域名预设（Outlook 家族 → graph，存量授权仅含 Graph 权限）。
- `poll_interval` 为 0 时取全局配置 `poll_interval_default`（DB 覆盖 > env，见 §5）。
- `imap_port` 缺省 993（隐式 TLS）。填 143 时先明文连接再用 STARTTLS 升级；升级失败（服务器不支持、证书校验不过）
  直接报错，绝不明文登录。
- 成功 `201`，响应同列表项。
- 错误：`400 invalid_account`（字段不合法）/ `400 verify_failed`（登录验证失败，不入库）/
  **`409 account_exists`**（该邮箱已存在——先判重，不再为一条注定被拒的请求去登一次远端邮箱）。

### `POST /api/v1/accounts/batch`（批量导入）
Content-Type 二选一：

- `application/json`：`[{"email":"a@outlook.com","refresh_token":"…","client_id":"…"}, …]`；
  **迁移模式**——直接 POST `GET /accounts/export` 的导出数组，名称/文件夹/轮询间隔/提醒开关/标签等一并还原（凭据字段必填，需用含凭据的导出文件）
- `text/plain`：与 main.py 账号文件**逐字兼容**，每行 `邮箱----密码----client_id----令牌`（`#` 注释、空行允许；Outlook/Hotmail 自动套用 oauth2 + graph 预设；**密码段不使用、不存储**）

只入库（`pending`），后台错峰验证（DESIGN 2.2）。**已存在的邮箱跳过、不覆盖**（计入 `skipped`）。响应：

```json
{ "created": 18, "skipped": 2, "failed": [{"line": 3, "error": "…"}] }
```

### `GET /api/v1/accounts/export`（导出/迁移）
导出账号为裸 JSON 数组，**格式与批量导入的 JSON 模式同构**——导出文件直接 POST 回 `/accounts/batch` 即完成迁移闭环。

| 参数 | 说明 |
|---|---|
| `include_credentials` | `true` 时附**解密后的凭据明文**（oauth2 → `refresh_token`；password → `password`）；默认 `false` 仅导配置 |
| `q` / `status` / `tag` | 与列表筛选一致（可只导某个标签的账号） |

响应（`Content-Disposition: attachment`）：

```json
[{
  "email": "a@outlook.com", "name": "生产-主邮箱", "auth_type": "oauth2",
  "client_id": "9e5f94bc-…", "refresh_token": "O.XXXX…",
  "imap_host": "outlook.office365.com", "imap_port": 993, "username": "a@outlook.com",
  "transport": "graph", "folders": ["INBOX", "JunkEmail"],
  "poll_interval": 300, "notify_enabled": true, "tags": ["生产"]
}]
```

**迁移三步**：

```bash
curl -H "Authorization: Bearer $TOKEN" "http://old:8080/api/v1/accounts/export?include_credentials=true" -o accounts.json
# accounts.json 安全传输到新实例主机（scp 等）
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" --data-binary @accounts.json http://new:8080/api/v1/accounts/batch
```

> ⚠️ `include_credentials=true` 是"API 永不回显凭据"原则的**显式例外**（迁移必须迁移凭据）：
> 导出文件为明文凭据，传输与保管责任在调用方，迁移完成后建议删除。导出不含已收邮件
> （邮件数据迁移需自行拷贝 `data/` 目录）。

### `POST /api/v1/accounts/test`
测试连接，不入库。请求体同创建。连接成功 → `200 {"ok": true}`。

**连接失败同样是 `200`**：`{"ok": false, "error": "失败原因"}`——这是"测试"的结果，不是请求出错。调用方要看 `ok` 字段，
不能只看状态码。只有请求体本身不合法才是 `400`（`bad_request` / `invalid_account`）。

### `GET /api/v1/accounts/{id}`
账号详情，字段同列表项。

### `PATCH /api/v1/accounts/{id}`
部分更新，**只传要改的字段**：

| 字段 | 类型 | 生效方式 |
|---|---|---|
| `name` | string | 即时（纯元数据） |
| `notify_enabled` | bool | 即时（Webhook 提醒开关） |
| `tags` | string[] | 即时（**全量替换**，`[]` 清空） |
| `folders` | string[] | **重启 worker** |
| `poll_interval` | int（秒） | **重启 worker** |
| `imap_host` / `username` | string | **重启 worker** |
| `client_id` | string | **重启 worker** |
| `password` / `refresh_token` | string | 加密覆盖后**重启 worker**（**省略 = 不修改**；传空串会被拒绝，见下） |
| `enabled` | bool | true 重启；false 停止收取（状态 `disabled`） |

- 凭据字段（`password` / `refresh_token` / `client_id`）**传了但为空串** → `400 invalid_account`。不修改就省略字段，不要传 `""`
  ——照做会把凭据覆盖成空，账号随即 `auth_failed`。
- 换凭据时清掉上次的 `last_error`；账号处于 `auth_failed` 则回到 `pending` 并立即重试（其余状态不变）。重新启用同样清 `last_error`。
- 账号不存在 → `404 account_not_found`；`poll_interval` 低于 5 秒 → `400 invalid_account`。

示例：`{"poll_interval": 300, "tags": ["生产"]}` → 响应为更新后的账号。

### `GET /api/v1/accounts/{id}/reauth-url`
取"重新获取令牌"的**授权页地址**。响应：`{"url": "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?…"}`。
地址带 `login_hint`（预填目标邮箱），scope 按传输分流并含 `openid`。

> 调用方不要自己拼这个地址：scope 与 `openid` 的组合必须和兑换端点（`POST /reauth`）严格一致，否则令牌端点不返回
> `id_token`，兑换会被拒。它只在这一处维护。
>
> 仅 oauth2 账号可用（否则 `400 not_oauth2`）；账号不存在 → `404 account_not_found`。

### `POST /api/v1/accounts/{id}/reauth`（重新获取令牌）
令牌过期/失效的 oauth2 账号，**授权码流**换全新 refresh_token：

```json
{ "code": "授权码，或授权完成后的完整跳转 URL" }
```

流程：`GET /reauth-url` 取授权页地址 → 浏览器打开、**用目标邮箱本身**登录并同意 → 把跳转地址整条粘回 →
服务端兑换新令牌并**校验登录身份** → 加密存储、**立即重启收取**。code 一次性、约 10 分钟有效。

**身份校验**：服务端从 `id_token` 读出实际登录的账号（`preferred_username` / `email`，不区分大小写），与目标邮箱比对；
对不上就拒绝，**凭据保持不变**。浏览器里往往还登着别的 Microsoft 账号，不拦的话换来的就是别人的令牌，本账号从此 `auth_failed`。

| 错误码（均为 400，另注明者除外） | 含义 |
|---|---|
| `bad_request` | 请求体不是 JSON，或取不到授权码 |
| `account_not_found`（404） | 账号不存在 |
| `not_oauth2` | 不是 oauth2 账号 |
| `reauth_failed` | 兑换失败：授权码过期或已用过、`redirect_uri` 不符、应用被拒等（`message` 带 Microsoft 的 `AADSTS…` 码） |
| `identity_unverified` | 拿不到可验证的登录身份（响应没有 `id_token`，或它无法解析/已过期）——同样拒绝，不"验不出来就算通过" |
| `account_mismatch` | 登录的是别的账号（`message` 里给出打码后的地址，如 `s***@outlook.com`） |

> 两种身份类错误都要求用目标邮箱重新登录；浏览器里登着别的账号时建议用无痕窗口。
>
> **以别名登记的账号**（添加时填的是别名地址，实际登录身份是主地址）会被判为不一致——目前没有别名豁免。
>
> 成功后账号状态在第一次连接成功时恢复（`syncing`）；新令牌仍被拒绝则再次落到 `auth_failed`。

> 注：密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），授权码流是可行路径。

### `POST /api/v1/accounts/{id}/sync`
手动收取单账号：插入信号（不重置轮询/IDLE 节奏；正在拉取时合并为一次）；`error` / `auth_failed` 账号也可触发
（视为一次显式连接尝试）。`202`，响应：`{"triggered": 1}`；账号不存在 → `404 account_not_found`。

### `POST /api/v1/accounts/sync`（批量收取）
body `{"ids": [1,2,3]}`，全部经错峰队列执行。`202`，响应：`{"queued": 3}`；`ids` 为空或不是 JSON → `400 bad_request`。

### `PATCH /api/v1/accounts/batch`（批量操作）
body `{"ids": [...], "enabled": false}` 或 `{"ids": [...], "notify_enabled": true}`。`notify_enabled` 对运行中的账号即时生效；
`enabled` 的生效方式同单个 PATCH。响应：`{"updated": 3}`（实际更新的条数，不存在的 id 不计入）。

### `DELETE /api/v1/accounts/{id}?purge=true`
删除账号（邮件记录级联删除）。响应：`{"deleted": 1, "purged_files": 12}`；账号不存在 → `404 account_not_found`。

`purge=true` 时**立即**删除磁盘上的 `.eml`；不带时这些文件失去引用，由**每日维护当孤儿回收**（下一轮维护起，且文件已存在
5 分钟以上）。也就是说 `.eml` 不会保留，`purge` 只决定"现在删"还是"稍后自动删"。两者都不影响邮箱服务器。

### `GET /api/v1/tags`
全部标签与引用数（按引用数降序）：`{"tags": [{"name": "生产", "count": 3}]}`

---

## 3. 邮件

### `GET /api/v1/messages`
邮件列表（不含正文）。参数：

| 参数 | 说明 |
|---|---|
| `account_id` | 限定账号 |
| `q` | 全文搜索（subject / from / snippet / body_text，LIKE，中文子串友好） |
| `tag` | 按所属账号标签过滤，可重复（OR） |
| `is_read` | `true` / `false` |
| `since` / `until` | 按真实到达时间 `received_at` 过滤（RFC3339；**格式非法时该筛选被忽略**，不报错） |
| `limit` | 默认 50，最大 200 |
| **默认分页** | `before_id=<id>`（向前翻页，id 倒序）/ `after_id=<id>`（增量，升序） |
| **排序分页** | `sort=time\|account` + `order=asc\|desc` + `offset=<n>`；`time` 按 `received_at`，`account` 按账号名称（组内时间倒序）；两套分页互斥 |
| `no_total` | 传 `1` 时跳过 `COUNT(*)` 统计且响应不含 `total`（翻页时可避免重复全表扫描） |

响应（每项含所属账号标签快照；`total` 为**同筛选条件**的邮件总数——前端据此显示"已加载 X / 共 Y"并精确判断是否还有下一页，不受 limit/游标影响）：

```json
{
  "total": 67,
  "messages": [{
    "id": 5321, "account_id": 2, "folder": "INBOX",
    "subject": "您的验证码", "from": "noreply@service.com", "to": ["a@outlook.com"],
    "sent_at": "2026-11-16T12:55:30Z", "received_at": "2026-11-16T12:55:41Z",
    "snippet": "您的验证码是…", "has_attachments": false,
    "is_read": false, "truncated": false, "tags": ["生产"]
  }]
}
```

> `received_at` 是邮件**真实到达时间**（Graph receivedDateTime / IMAP INTERNALDATE / Date 头兜底），不是入库时间。

### `GET /api/v1/messages/{id}`
详情（含正文与附件列表，`body_text` / `body_html` / `parse_error` / `attachments: [{id, filename, content_type, size}]`）。

### `PATCH /api/v1/messages/{id}`
body `{"is_read": true|false}`。**只改本地副本**，绝不回写邮箱服务器。响应：`{"id": 1, "is_read": true}`。
邮件不存在 → `404 message_not_found`（下列各端点同）。

### `POST /api/v1/messages/batch-read`
body `{"ids": [...]}` 或 `{"account_id": 1}`（该账号全部）。响应：`{"updated": 12}`

### `POST /api/v1/messages/batch-delete`
body 同上。删除**本地副本**（行同步删、`.eml` 异步删）。响应：`{"deleted": 12}`

### `DELETE /api/v1/messages/{id}`
删除单封。响应：`{"deleted": 1}`

### `GET /api/v1/messages/{id}/raw`
下载原始 `.eml`（`message/rfc822`；需带 Authorization 头，浏览器里用 fetch blob 而非 `<a href>`）。文件缺失 → `404 raw_missing`。

### `GET /api/v1/messages/{id}/attachments/{attID}`
下载附件（Content-Type 为附件声明类型）。附件不属于该邮件 → `404 attachment_not_found`；原始邮件损坏无法解析 →
`404 attachment_not_found`（`message` 说明原因）。

---

## 4. Webhook

### `GET/POST /api/v1/webhooks`
列表 / 创建。创建：

```json
{ "url": "https://downstream.example.com/hook", "secret": "可选，不传自动生成",
  "account_ids": [1, 2], "include_body": false, "enabled": true }
```

- `account_ids` 缺省（null）= 全部账号；`include_body: true` 时投递载荷附带 `body_text`；`enabled` 缺省 `true`。
- **secret 仅在创建响应中返回一次**，之后永不回显。自定义 secret **至少 16 个字符**（过短 → `400 weak_secret`）
  ——签名密钥可以被暴力猜出，猜中就能伪造"新邮件"事件；不传则自动生成 32 字符随机值。
- 成功 `201`，响应为 webhook 对象（见列表项）加 `secret`。其他错误：`400 bad_request`（JSON 不合法或缺 `url`）、
  `400 bad_url`（不是 `http(s)://` 开头的绝对地址）。
- 列表响应每项含 `pending` / `failed` 投递统计。

### `PATCH /api/v1/webhooks/{id}` / `DELETE /api/v1/webhooks/{id}`
部分更新（url / account_ids / include_body / enabled） / 删除（投递记录级联）。webhook 不存在 → `404 webhook_not_found`。

被**禁用**的 webhook：它的 `pending` 记录原样保留、不投递，重新启用后继续。

### `POST /api/v1/webhooks/{id}/test`
发送测试事件（不进队列）。响应 `200`：`{"ok": true, "status_code": 200}`。**投递失败同样是 `200`**：
`{"ok": false, "status_code": 500, "error": "…"}`（要看 `ok` 字段）。投递器未启用时 `503 deliverer_offline`。

### `GET /api/v1/webhooks/{id}/deliveries`
投递记录（分页，新事件优先）：`?page=&limit=&status=pending|success|failed`。响应：

```json
{ "total": 40, "deliveries": [{
  "id": 1024, "message_id": 5321, "status": "pending", "attempts": 1,
  "next_retry_at": "2026-11-17T05:34:03Z", "response_code": 502, "last_error": "HTTP 502",
  "updated_at": "2026-11-17T05:33:53Z"
}] }
```

- `status` 取值是 `pending` / `success` / `failed`（注意是 `success`，不是 `succeeded`）；不传则返回全部状态。
- `attempts` 为已尝试次数；重试耗尽的记录 `status=failed`、`attempts` 为 6（首次 + 5 次重试）。

### `POST /api/v1/deliveries/{id}/retry`
手动重发失败投递：`failed` → `pending`，立即投递，并把**重试计数与上次失败信息清零**（`attempts=0`，
`next_retry_at` / `response_code` / `last_error` 置空）——它重新拥有完整的 5 次重试机会。
记录不存在 → `404 delivery_not_found`。响应：`{"retried": 1024}`

### Webhook 推送格式（下游侧契约）

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
    "id": 5321, "account_id": 2, "account_email": "a@example.com", "folder": "INBOX",
    "subject": "您的验证码", "from": "noreply@service.com", "to": ["a@example.com"],
    "sent_at": "2026-11-16T12:55:30Z", "received_at": "2026-11-16T12:55:41Z",
    "snippet": "您的验证码是 123456…", "has_attachments": false
  }
}
```

- 轻载荷：默认不带正文与附件（需要时用 `message.id` 回调 API；或创建时 `include_body: true`）。
- 至少一次投递：2xx 视为成功；重试 10s / 1m / 5m / 30m / 2h 共 5 次后标记 failed。
- 下游以 `delivery_id`（或 `message.id`）幂等。
- 仅**非历史**新邮件触发（首次同步的存量不通知；账号的首次同步没走完之前，陆续同步进来的邮件同样算历史）；
  账号 `notify_enabled` 默认关闭。
- 投递记录：**已结束的**（`success` / `failed`）保留 30 天，由每日维护清理；`pending` 的不清理。

---

## 5. 全局配置

### `GET /api/v1/settings`
全部运行时可变项（优先级 **DB 覆盖 > 环境变量 > 默认**）：

```json
{ "settings": {
  "retention_days":     { "value": 0,  "overridden": false },
  "initial_sync_days":  { "value": 90, "overridden": false },
  "poll_interval_default": { "value": 60, "overridden": true },
  "fallback_interval":  { "value": 300, "overridden": false },
  "idle_budget_per_host": { "value": 0, "overridden": false }
} }
```

### `PUT /api/v1/settings`
在线修改：`{"retention_days": 90, ...}`；值为 `null` 清除该项覆盖（回退环境变量/默认）。白名单 + 范围校验，保存后立即生效于 worker 配置缓存。

| 项 | 范围 | 生效时机 |
|---|---|---|
| `retention_days` | 0–36500（0=永久） | 下一轮每日维护 |
| `initial_sync_days` | 1–3650 | 之后新增/导入的账号 |
| `poll_interval_default` | 5–86400 秒 | 之后新增/导入的账号 |
| `fallback_interval` | 30–86400 秒 | IDLE 账号下一轮等待周期 |
| `idle_budget_per_host` | 0–100000（0=不限） | 新建立的 IDLE 会话 |

> 凭据（API Token / SECRET_KEY）与进程级项（LISTEN / DATA_DIR 等）仅环境变量，修改需重启。

---

## 6. SSE 事件流

### `POST /api/v1/events/ticket`
需 `Authorization: Bearer` 认证。签发一枚 60 秒有效的一次性 SSE 建连票据（消费即销毁），避免主 `MAILHUB_API_TOKEN` 出现在 URL 与代理访问日志中。

```json
{ "ticket": "<random>", "expires_in": 60 }
```

### `GET /api/v1/events?ticket=…`
`text/event-stream` 长连接；30s 心跳保活（注释行 `: ping`）。推荐使用上方签发的一次性 `?ticket=` 建连
（同时保留 `?token=<MAILHUB_API_TOKEN>` 兼容旧脚本）。票据缺失、无效或已用过 → `401 unauthorized`。

```
event: message.new
data: {"id":5321,"subject":"您的验证码","from":"noreply@service.com","account":"a@example.com"}
```

- 新邮件入库即推送**单封摘要**（供浏览器通知与列表即时刷新）。
- **历史邮件（首次同步批量）不推**，防通知轰炸——与 Webhook 的历史判定是同一条规则。
- 事件语义是"有新邮件了，请刷新"，载荷只用于展示：慢消费者的连接会被丢事件而不阻塞收件，客户端应以后端列表为准。
- 使用一次性 `?ticket=` 时，断线重连需先重新调用 `POST /api/v1/events/ticket` 换票。
- **服务停机或重启时，服务端会主动结束所有事件流**（不等它们自然断开，否则停机会被一直开着的邮件页拖住）。
  连接随后报错，客户端应**重新取票再建连**——一次性票据不能复用，浏览器 EventSource 自带的自动重连会带着用过的票
  拿到 401。Web 界面的做法是在 `onerror` 里关闭当前连接、5 秒后重新换票；脚本里表现为流读到 EOF，同样重新取票。

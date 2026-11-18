# MailHub API 参考

REST API 完整参考（以当前实现为准；架构层面的契约与动机见 `DESIGN.md` 第 5/6 节）。

- 基础前缀：`/api/v1`，请求与响应均为 JSON（附件与 `.eml` 下载除外）。
- 时间格式：RFC3339 UTC（如 `2026-11-17T05:33:53Z`）。
- 监听地址默认 `:8080`（`MAILHUB_LISTEN` 可改）。

## 认证

| 方式 | 适用 |
|---|---|
| `Authorization: Bearer <MAILHUB_API_TOKEN>` | 全部 REST 端点（常量时间比较） |
| `?token=<MAILHUB_API_TOKEN>` 查询参数 | **仅** `GET /api/v1/events`（SSE）——EventSource 无法携带自定义头 |

`GET /healthz` 无需认证。

## 错误格式

非 2xx 响应统一为：

```json
{ "error": { "code": "bad_request", "message": "人类可读的中文说明" } }
```

常见状态码：`400` 参数错误 / `401` 认证失败 / `404` 资源不存在 / `503` 依赖未启用。

---

## 1. 健康与状态

### `GET /healthz`
存活检查（无需认证）。响应：`{"status":"ok"}`

### `GET /api/v1/status`
服务总览。响应：

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
| `page` / `limit` | 分页（limit 默认 50） |
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
- 成功 `201`，响应同列表项。

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
测试连接，不入库。请求体同创建。响应：`{"ok": true}` 或 400 带失败原因。

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
| `password` / `refresh_token` | string | 加密覆盖后**重启 worker**（仅给出时修改） |
| `enabled` | bool | true 重启；false 停止收取（状态 `disabled`） |

示例：`{"poll_interval": 300, "tags": ["生产"]}` → 响应为更新后的账号。

### `POST /api/v1/accounts/{id}/reauth`（重新获取令牌）
令牌过期/失效的 oauth2 账号，**授权码流**换全新 refresh_token：

```json
{ "code": "授权码，或授权完成后的完整跳转 URL" }
```

流程：浏览器打开授权页（`client_id` 用账号自身的；scope 按传输分流）登录该邮箱 → 把跳转地址整条粘回 → 服务端兑换新令牌、加密存储并**立即重启收取**。code 一次性、约 10 分钟有效。

> 注：密码换令牌（ROPC）已被 Microsoft 对个人账户禁用（AADSTS9001023），授权码流是可行路径。

### `POST /api/v1/accounts/{id}/sync`
手动收取单账号：插入信号（不重置轮询/IDLE 节奏）；`error` / `auth_failed` 账号也可触发（视为一次显式连接尝试）。响应：`{"triggered": 1}`

### `POST /api/v1/accounts/sync`（批量收取）
body `{"ids": [1,2,3]}`，全部经错峰队列执行。响应：`{"triggered": 3}`

### `PATCH /api/v1/accounts/batch`（批量操作）
body `{"ids": [...], "enabled": false}` 或 `{"ids": [...], "notify_enabled": true}`。

### `DELETE /api/v1/accounts/{id}?purge=true`
删除账号（邮件记录级联删除）；`purge=true` 同时删除磁盘 `.eml`。

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
| `since` / `until` | 按真实到达时间 `received_at` 过滤（RFC3339） |
| `limit` | 默认 50 |
| **默认分页** | `before_id=<id>`（向前翻页，id 倒序）/ `after_id=<id>`（增量，升序） |
| **排序分页** | `sort=time\|account` + `order=asc\|desc` + `offset=<n>`；`time` 按 `received_at`，`account` 按账号名称（组内时间倒序）；两套分页互斥 |

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
body `{"is_read": true|false}`。**只改本地副本**，绝不回写邮箱服务器。响应：`{"id": 1, "is_read": true}`

### `POST /api/v1/messages/batch-read`
body `{"ids": [...]}` 或 `{"account_id": 1}`（该账号全部）。响应：`{"updated": 12}`

### `POST /api/v1/messages/batch-delete`
body 同上。删除**本地副本**（行同步删、`.eml` 异步删）。响应：`{"deleted": 12}`

### `DELETE /api/v1/messages/{id}`
删除单封。响应：`{"deleted": 1}`

### `GET /api/v1/messages/{id}/raw`
下载原始 `.eml`（`message/rfc822`；需带 Authorization 头，浏览器里用 fetch blob 而非 `<a href>`）。

### `GET /api/v1/messages/{id}/attachments/{attID}`
下载附件（Content-Type 为附件声明类型）。

---

## 4. Webhook

### `GET/POST /api/v1/webhooks`
列表 / 创建。创建：

```json
{ "url": "https://downstream.example.com/hook", "secret": "可选，不传自动生成",
  "account_ids": [1, 2], "include_body": false, "enabled": true }
```

- `account_ids` 缺省（null）= 全部账号；`include_body: true` 时投递载荷附带 `body_text`。
- **secret 仅在创建响应中返回一次**，之后永不回显。
- 列表响应每项含 `pending` / `failed` 投递统计。

### `PATCH /api/v1/webhooks/{id}` / `DELETE /api/v1/webhooks/{id}`
部分更新（url / account_ids / include_body / enabled） / 删除（投递记录级联）。

### `POST /api/v1/webhooks/{id}/test`
发送测试事件（不进队列）。响应：`{"ok": true, "status_code": 200}`。

### `GET /api/v1/webhooks/{id}/deliveries`
投递记录（分页）：`?page=&limit=&status=pending|succeeded|failed`。响应每项：
`{id, message_id, status, attempts, next_retry_at, response_code, last_error, updated_at}`

### `POST /api/v1/deliveries/{id}/retry`
手动重发失败投递（failed → pending，立即投递）。响应：`{"retried": 1}`

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
- 仅**非历史**新邮件触发（首次同步的存量不通知）；账号 `notify_enabled` 默认关闭。
- 投递记录保留 30 天。

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

### `GET /api/v1/events?token=…`
`text/event-stream` 长连接；断线 5s 自动重连，30s 心跳保活。

```
event: message.new
data: {"id":5321,"subject":"您的验证码","from":"noreply@service.com","account":"a@example.com"}
```

- 新邮件入库即推送**单封摘要**（供浏览器通知与列表即时刷新）。
- **历史邮件（首次同步批量）不推**，防通知轰炸。
- 非 200（如 token 失效）时浏览器 EventSource 停止自动重连。

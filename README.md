# MailHub

邮件集中收取服务：统一接入**几百至上千个**邮箱账号（邮箱矩阵），**只收不发**。新邮件经 Webhook 通知外部系统，提供 REST API 与 Web 管理界面。

> 对远端邮箱**完全只读**：标记已读、删除等操作只作用于 MailHub 的本地副本，绝不回写邮箱服务器。

## 特性

- **矩阵规模**：批量导入错峰验证、连接错峰、单 IMAP 主机 IDLE 预算（超限自动降级轮询）、全部列表 API 分页
- **双传输**：Microsoft Graph（Outlook 家族主传输，delta query 增量）与 IMAP（XOAUTH2 / 密码认证，IDLE 实时推送）
- **双认证**：密码 与 OAuth2 refresh_token；凭据 AES-256-GCM 加密入库，API 永不回显
- **增量同步**：IMAP 按 `UIDVALIDITY + UID`、Graph 按 `delta_link` / 时间游标，游标失效自动回退初始窗口，`graph_id` 唯一索引幂等
- **Webhook 通知**：HMAC-SHA256 签名、轻载荷、至少一次投递（重试 5 次 + 手动重发）、账号级提醒开关默认关闭
- **Web 界面**（Vue 3）：邮件查看 / 全文搜索（中文子串友好）/ 无限滚动 / 账号管理与令牌重新授权 / Webhook 管理 / 全局配置 / 新邮件浏览器通知（SSE 实时推送）
- **单二进制**：Go + SQLite（WAL），无消息队列等外部依赖；Web UI 内嵌；多阶段 Docker 镜像（distroless）
- **运维内置**：每日维护（保留期清理 + 孤儿 .eml 回收）、运行时配置在线修改、健康检查与状态总览

## 快速开始

### 二进制运行

```bash
go build -o mailhub ./cmd/mailhub
go run ./cmd/mailhub --gen-secret          # 生成 MAILHUB_SECRET_KEY

export MAILHUB_API_TOKEN=<自定义 API 令牌>
export MAILHUB_SECRET_KEY=<上一步生成>
./mailhub                                  # 默认 :8080，MAILHUB_LISTEN 可改
```

### Docker

```bash
# 凭据放根目录 .env（KEY=value 每行一条，勿提交）
docker compose up -d --build
```

容器镜像**不绑定运行用户**，UID/GID 由部署侧指定（compose 默认 `user: "1000:1000"` 非 root）：

```yaml
# docker-compose.yml
user: "1000:1000"   # 自定义改此处；bind mount 时宿主目录属主需匹配
```

```bash
docker run -u 1000:1000 ... zoyopei/mailhub   # 裸 docker run 请自行 -u（默认 root）
```

> named volume 首次初始化会自动继承镜像内 `/data`（属主 1000:1000）；换用其他 UID 时 bind mount 需 `chown`，named volume 需重建。

### 批量导入账号

每行 `邮箱----密码----client_id----令牌`（与前身工具 main.py 的账号文件逐字兼容；Outlook/Hotmail 的密码段为占位符，不存储）：

```bash
curl -H "Authorization: Bearer $MAILHUB_API_TOKEN" \
     -H "Content-Type: text/plain" \
     --data-binary @accounts.txt \
     http://127.0.0.1:8080/api/v1/accounts/batch
```

导入只入库（`pending`），后台错峰验证登录。

## 文档

| 文件 | 内容 |
|---|---|
| [DESIGN.md](DESIGN.md) | 架构契约：需求、规模考量、数据模型、API 契约、里程碑 |
| [TECH.md](TECH.md) | 实施设计：技术选型、Go 包结构、事务边界、worker 状态机 |
| [API.md](API.md) | API 完整参考：全部端点、错误码、Webhook 推送与验签格式 |

## API 概览

认证：`Authorization: Bearer <MAILHUB_API_TOKEN>`。例外是 SSE 端点（浏览器的 EventSource 带不了请求头）：先用 `POST /api/v1/events/ticket` 换一张 60 秒有效、只能用一次的票据，再连 `GET /api/v1/events?ticket=…`（兼容旧的 `?token=`，但会把主令牌暴露在代理访问日志里）。

- `GET /healthz` 健康检查（无认证）；`GET /api/v1/status` 服务总览
- 账号：增删改查、批量导入、手动收取（单/批）、令牌重新授权、启停
- 邮件：列表（筛选/全文搜索/排序/分页）、详情、原始 .eml、附件下载、已读标记、批量删除
- Webhook：CRUD、测试事件、投递记录、手动重发
- 配置：`GET/PUT /api/v1/settings` 五个运行时配置在线修改
- 实时：`GET /api/v1/events` SSE 新邮件推送

完整参数与示例见 [API.md](API.md)。

## 许可证

[MIT](LICENSE)

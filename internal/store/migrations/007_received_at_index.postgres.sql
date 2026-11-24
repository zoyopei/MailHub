-- 007 received_at 单列索引 —— PostgreSQL 方言
-- 邮件列表按时间排序（ORDER BY received_at, id）与"今日收信"计数（received_at >= 日界）
-- 都落在 received_at 上；没有索引时是全表扫描 + 临时排序，而仪表盘每 10 秒轮询一次状态。
-- 已有的 idx_messages_account_time 以 account_id 开头，只对"单账号 + 时间"查询有效。

CREATE INDEX idx_messages_received ON messages(received_at DESC);

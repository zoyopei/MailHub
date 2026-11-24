-- 006 外键子表索引（级联删除不再全表扫描子表）—— SQLite 方言
-- 外键 ON DELETE CASCADE 删父行时要按子表外键列找出待删行；没有索引时每删一行父表
-- 就全表扫描子表（附件表 50 万行时删 500 封邮件约 38 秒，全程持写锁）。
-- 批量删除、清空/删除账号、每日 retention 清理都走这条路径。

CREATE INDEX idx_attachments_message ON attachments(message_id);
CREATE INDEX idx_deliveries_message ON webhook_deliveries(message_id);
CREATE INDEX idx_deliveries_webhook ON webhook_deliveries(webhook_id);

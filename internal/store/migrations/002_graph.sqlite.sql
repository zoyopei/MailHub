-- 002 Graph 传输路径（存量 Outlook 授权仅含 Graph 权限，见 DESIGN.md 3.6）—— SQLite 方言

ALTER TABLE accounts ADD COLUMN transport TEXT NOT NULL DEFAULT 'imap';  -- imap | graph
CREATE INDEX idx_accounts_transport ON accounts(transport);

-- Graph 消息 id（十六进制串，非 UID）：Graph 路径的幂等去重键
ALTER TABLE messages ADD COLUMN graph_id TEXT;
CREATE UNIQUE INDEX idx_messages_graph ON messages(account_id, graph_id) WHERE graph_id IS NOT NULL;

-- Graph 路径的增量游标：上次成功拉取的消息时间（重叠窗口由 worker 处理）
ALTER TABLE folder_states ADD COLUMN last_seen_at DATETIME;

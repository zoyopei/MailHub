-- 002 Graph 传输路径（存量 Outlook 授权仅含 Graph 权限，见 DESIGN.md 3.6）—— PostgreSQL 方言

ALTER TABLE accounts ADD COLUMN transport TEXT NOT NULL DEFAULT 'imap';
CREATE INDEX idx_accounts_transport ON accounts(transport);

ALTER TABLE messages ADD COLUMN graph_id TEXT;
CREATE UNIQUE INDEX idx_messages_graph ON messages(account_id, graph_id) WHERE graph_id IS NOT NULL;

ALTER TABLE folder_states ADD COLUMN last_seen_at TIMESTAMPTZ;

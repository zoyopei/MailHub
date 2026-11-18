-- 001 初始化全部表结构（与 DESIGN.md §4 一致）—— SQLite 方言

CREATE TABLE accounts (
  id              INTEGER PRIMARY KEY,
  name            TEXT NOT NULL,
  email           TEXT NOT NULL UNIQUE,
  imap_host       TEXT NOT NULL,
  imap_port       INTEGER NOT NULL DEFAULT 993,
  use_tls         INTEGER NOT NULL DEFAULT 1,
  username        TEXT NOT NULL,
  auth_type       TEXT NOT NULL DEFAULT 'password',
  client_id       TEXT,
  credential_enc  BLOB NOT NULL,
  folders         TEXT NOT NULL DEFAULT '["INBOX"]',
  poll_interval   INTEGER NOT NULL DEFAULT 60,
  notify_enabled  INTEGER NOT NULL DEFAULT 0,
  enabled         INTEGER NOT NULL DEFAULT 1,
  status          TEXT NOT NULL DEFAULT 'pending',
  last_error      TEXT,
  last_sync_at    DATETIME,
  created_at      DATETIME NOT NULL,
  updated_at      DATETIME NOT NULL
);
CREATE INDEX idx_accounts_status ON accounts(status);

CREATE TABLE folder_states (
  account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder       TEXT NOT NULL,
  uidvalidity  INTEGER NOT NULL,
  last_uid     INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, folder)
);

CREATE TABLE messages (
  id            INTEGER PRIMARY KEY,
  account_id    INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder        TEXT NOT NULL,
  uidvalidity   INTEGER NOT NULL,
  uid           INTEGER NOT NULL,
  message_id    TEXT,
  subject       TEXT,
  from_addr     TEXT,
  to_addrs      TEXT,
  cc_addrs      TEXT,
  sent_at       DATETIME,
  received_at   DATETIME NOT NULL,
  body_text     TEXT,
  body_html     TEXT,
  snippet       TEXT,
  size          INTEGER,
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_read       INTEGER NOT NULL DEFAULT 0,
  truncated     INTEGER NOT NULL DEFAULT 0,
  parse_error   TEXT,
  raw_path      TEXT NOT NULL,
  UNIQUE (account_id, folder, uidvalidity, uid)
);
CREATE INDEX idx_messages_account_time ON messages(account_id, received_at DESC);
CREATE INDEX idx_messages_message_id ON messages(account_id, message_id);
CREATE INDEX idx_messages_unread ON messages(account_id) WHERE is_read = 0;

CREATE TABLE attachments (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename     TEXT,
  content_type TEXT,
  size         INTEGER,
  part_index   INTEGER NOT NULL
);

CREATE TABLE webhooks (
  id          INTEGER PRIMARY KEY,
  url         TEXT NOT NULL,
  secret      TEXT NOT NULL,
  account_ids TEXT,
  include_body INTEGER NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  DATETIME NOT NULL
);

CREATE TABLE webhook_deliveries (
  id            INTEGER PRIMARY KEY,
  webhook_id    INTEGER NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
  message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  status        TEXT NOT NULL DEFAULT 'pending',
  attempts      INTEGER NOT NULL DEFAULT 0,
  next_retry_at DATETIME,
  response_code INTEGER,
  last_error    TEXT,
  created_at    DATETIME NOT NULL,
  updated_at    DATETIME NOT NULL
);
CREATE INDEX idx_deliveries_pending ON webhook_deliveries(status, next_retry_at);

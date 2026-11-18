-- 004 账号标签（每账号多标签、按标签过滤）—— PostgreSQL 方言

CREATE TABLE tags (
  id   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);

CREATE TABLE account_tags (
  account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  tag_id     BIGINT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (account_id, tag_id)
);

CREATE INDEX idx_account_tags_tag ON account_tags(tag_id);

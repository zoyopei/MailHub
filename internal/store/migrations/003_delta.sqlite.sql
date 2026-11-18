-- 003 Graph delta query（DESIGN.md 3.6 M2：delta 链接替代时间窗列表缩窄请求）—— SQLite 方言

ALTER TABLE folder_states ADD COLUMN delta_link TEXT;

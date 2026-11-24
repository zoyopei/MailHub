-- 008 folder_states 首次同步完成标记 —— SQLite 方言
-- "历史邮件不触发 Webhook"（DESIGN.md 3.4/5.1）以"该文件夹的首次同步是否已经完整走完"为准，
-- 而不是"有没有游标行"：游标行在首封邮件入库时就建了，首次同步中途被打断（网络错误、停机、
-- Graph 单轮分页上限）之后，窗口里剩下的历史邮件会被当成新邮件通知。
-- 默认 1：存量行都视为已完成；worker 在首次同步开始前建行并置 0，走完置 1。

ALTER TABLE folder_states ADD COLUMN initial_done INTEGER NOT NULL DEFAULT 1;

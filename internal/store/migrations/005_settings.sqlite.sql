-- 005 全局配置（Web 在线修改，DB 覆盖环境变量；仅运行时可变项，凭据/进程级项除外）—— SQLite 方言

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

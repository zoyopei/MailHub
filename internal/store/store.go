// Package store 封装数据库访问：迁移、事务边界与 repository（TECH.md §2.2）。
// 事务边界唯一归属本包——其他包不得自行开事务。
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	_ "modernc.org/sqlite" // 注册 "sqlite" 驱动（纯 Go，无 CGO）
)

//go:embed migrations/*.sqlite.sql
var sqliteMigrations embed.FS

var ErrPostgresNotEnabled = errors.New("PostgreSQL 驱动未接入（M1 仅 SQLite）；如需 PG 请按 TECH.md §1.1 引入 pgx")

// Store 持有数据库连接与方言标识。
type Store struct {
	DB     *sql.DB
	Driver string // "sqlite" | "postgres"
}

// Open 打开数据库并应用迁移。dbURL 为空时使用 dataDir 下的 SQLite。
func Open(dbURL, dataDir string) (*Store, error) {
	if dbURL == "" {
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, fmt.Errorf("创建数据目录: %w", err)
		}
		path := filepath.Join(dataDir, "mailhub.db")
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		if err != nil {
			return nil, err
		}
		// WAL 模式需在连接外设置（持久化到数据库文件）。
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			return nil, fmt.Errorf("设置 WAL: %w", err)
		}
		// 千级 worker 低频写入：限制连接池避免 fd 膨胀。
		db.SetMaxOpenConns(8)
		s := &Store{DB: db, Driver: "sqlite"}
		if err := s.migrate(); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, ErrPostgresNotEnabled
}

// migrate 按文件名顺序应用未执行的迁移，记录到 schema_migrations。
func (s *Store) migrate() error {
	if _, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at DATETIME NOT NULL)`); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := s.DB.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	entries, err := fs.Glob(sqliteMigrations, "migrations/*.sqlite.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	for _, name := range entries {
		version := name[len("migrations/"):]
		if applied[version] {
			continue
		}
		body, err := sqliteMigrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("迁移 %s: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, datetime('now'))`,
			version); err != nil {
			// 兼容占位符差异由 driver 层抹平：modernc sqlite 支持 ?；PG 接入时改用 $1。
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Close 关闭连接池。
func (s *Store) Close() error { return s.DB.Close() }

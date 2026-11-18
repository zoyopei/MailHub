// Package maintain 每日维护任务（TECH.md §4）：retention 清理与孤儿 .eml 回收。
//   - retention（DESIGN.md 9.1 MAILHUB_RETENTION_DAYS，0 = 永久）：
//     删除 received_at 早于保留窗口的邮件（行 + 级联附件/投递记录 + .eml 文件）；
//   - 孤儿回收（DESIGN.md 6.2 batch-delete 的兜底）：删除 mails/ 下未被数据库
//     引用的 .eml（异步文件删除失败的遗留）与超过 24h 的 .part 写入残留。
package maintain

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mailhub/internal/config"
	"mailhub/internal/store"
)

// stalePartAge .part 临时文件的安全年龄：写入流程为 tmp → 事务 → rename，
// 正常存活秒级；超过该年龄即为事务失败/进程崩溃残留。
const stalePartAge = 24 * time.Hour

// Run 阻塞执行每日维护：启动即跑一轮（错开 1 分钟，避免与启动峰值重叠），
// 此后每 24 小时一轮，直至 ctx 取消。
func Run(ctx context.Context, cfg *config.Config, st *store.Store) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		Once(ctx, cfg, st)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once 执行一轮维护（导出供测试）。retention 天数支持全局配置覆盖（DB > env）。
func Once(ctx context.Context, cfg *config.Config, st *store.Store) {
	days := cfg.RetentionDays
	if vals, err := st.GetSettings(); err == nil {
		days = store.SettingInt(vals, store.SettingRetentionDays, cfg.RetentionDays)
	}
	// 1. retention（0 = 永久保留，跳过）
	if days > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		paths, err := st.DeleteMessagesOlderThan(cutoff)
		if err != nil {
			slog.Error("retention 清理失败", "error", err)
		} else if n := removeFiles(cfg.DataDir, paths); n > 0 {
			slog.Info("retention 清理完成", "messages", len(paths), "files_removed", n, "days", days)
		} else {
			slog.Info("retention 清理完成", "messages", len(paths), "days", days)
		}
	}

	// 2. 孤儿回收（retention 之后执行：先删行再扫文件，避免误删本轮已引用文件）
	if err := sweepOrphans(ctx, cfg, st); err != nil {
		slog.Error("孤儿文件回收失败", "error", err)
	}
}

// sweepOrphans 扫描 {DataDir}/mails：未引用的 .eml 与过龄 .part 删除。
func sweepOrphans(_ context.Context, cfg *config.Config, st *store.Store) error {
	refs, err := st.AllRawPaths()
	if err != nil {
		return err
	}
	root := filepath.Join(cfg.DataDir, "mails")
	removed := 0
	now := time.Now()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) { // mails 目录尚不存在（无邮件）
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		rel, rerr := filepath.Rel(cfg.DataDir, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		switch {
		case strings.HasSuffix(name, ".part"):
			info, serr := d.Info()
			if serr != nil || now.Sub(info.ModTime()) < stalePartAge {
				return nil // 新写入或信息不可得：不动
			}
		case strings.HasSuffix(name, ".eml"):
			if _, ok := refs[rel]; ok {
				return nil // 数据库仍引用
			}
		default:
			return nil // 未知文件：不动（保守）
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if removed > 0 {
		slog.Info("孤儿文件回收", "files_removed", removed)
	}
	return nil
}

// removeFiles 删除相对 DataDir 的文件列表，返回成功删除数（失败只记录，不阻断）。
func removeFiles(dataDir string, rels []string) int {
	n := 0
	for _, rel := range rels {
		if rel == "" {
			continue
		}
		if err := os.Remove(filepath.Join(dataDir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			slog.Warn("删除文件失败", "path", rel, "error", err)
			continue
		}
		n++
	}
	return n
}

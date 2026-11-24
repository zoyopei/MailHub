package api

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

const diskCacheTTL = 30 * time.Second

type diskSizeCache struct {
	mu   sync.Mutex
	size int64
	at   time.Time
}

// handleStatus GET /api/v1/status 总览（DESIGN.md 6.5）：
// 账号状态分布、今日收信、待投递/失败数、磁盘占用。
//
// day_start（可选，RFC3339）："今日收信"的起点。Web 端传浏览器所在时区的本地零点；
// 脚本调用缺省取服务器本地零点（容器里靠 TZ 环境变量生效；时区数据用的是镜像自带的 tzdata，
// 二进制没有内嵌——换成不带 tzdata 的基础镜像时需要 import _ "time/tzdata"）。
// 以前固定按 UTC 零点算：北京时间早上 8 点归零。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	since, err := dayStart(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"day_start 应为 RFC3339 时间（如 2026-11-24T00:00:00+08:00）")
		return
	}
	byStatus, err := s.store.StatusCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "status_failed", err.Error())
		return
	}
	today, _ := s.store.MessageCountSince(since)
	pending, failed, _ := s.store.PendingDeliveryCount()
	disk := s.cachedDirSize()

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts_by_status": byStatus,
		"messages_today":     today,
		"deliveries_pending": pending,
		"deliveries_failed":  failed,
		"disk_usage_bytes":   disk,
	})
}

// dayStart 解析 day_start 参数；缺省为服务器本地时区的今天零点。
func dayStart(r *http.Request) (time.Time, error) {
	now := time.Now()
	if v := r.URL.Query().Get("day_start"); v != "" {
		return time.Parse(time.RFC3339, v)
	}
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
}

// cachedDirSize 统计目录大小（30s TTL 缓存，避免高频状态/登录校验反复遍历海量 .eml）。
func (s *Server) cachedDirSize() int64 {
	s.diskCache.mu.Lock()
	if !s.diskCache.at.IsZero() && time.Since(s.diskCache.at) < diskCacheTTL {
		size := s.diskCache.size
		s.diskCache.mu.Unlock()
		return size
	}
	s.diskCache.mu.Unlock()

	size := dirSize(s.cfg.DataDir)
	s.diskCache.mu.Lock()
	s.diskCache.size = size
	s.diskCache.at = time.Now()
	s.diskCache.mu.Unlock()
	return size
}

// dirSize 统计目录大小（WalkDir 遍历；低频量级下代价可忽略）。
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

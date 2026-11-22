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
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	byStatus, err := s.store.StatusCounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "status_failed", err.Error())
		return
	}
	today, _ := s.store.TodayMessageCount()
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

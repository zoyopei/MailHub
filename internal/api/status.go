package api

import (
	"io/fs"
	"net/http"
	"path/filepath"
)

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
	disk := dirSize(s.cfg.DataDir)

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts_by_status": byStatus,
		"messages_today":     today,
		"deliveries_pending": pending,
		"deliveries_failed":  failed,
		"disk_usage_bytes":   disk,
	})
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

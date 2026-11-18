package store

import (
	"strconv"
)

// 全局配置（005 迁移）：DB 覆盖 > 环境变量 > 代码默认值。
// 仅收运行时可变项；凭据（API Token / SECRET_KEY）与进程级项（LISTEN/DATA_DIR）不在此。
const (
	SettingRetentionDays     = "retention_days"      // 邮件保留天数，0 = 永久；maintain 每轮读取
	SettingInitialSyncDays   = "initial_sync_days"   // 首次同步窗口；worker 首拉读取
	SettingPollDefault       = "poll_interval_default" // 新账号默认轮询间隔（秒）；创建/导入时读取
	SettingFallbackInterval  = "fallback_interval"   // IDLE 兜底拉取间隔（秒）；idleLoop 每轮读取
	SettingIdleBudgetPerHost = "idle_budget_per_host" // 单 IMAP 主机 IDLE 预算，0 = 不限；worker 会话准入读取
)

// SettingKeys 可在线修改的白名单。
var SettingKeys = []string{
	SettingRetentionDays, SettingInitialSyncDays, SettingPollDefault,
	SettingFallbackInterval, SettingIdleBudgetPerHost,
}

// SettingSpec 单项约束（校验与前端说明共用语义；Min = 0 即表达"0 合法"
// ——retention 0=永久、budget 0=不限）。
type SettingSpec struct {
	Key string
	Min int // 合法最小值
	Max int
}

var SettingSpecs = []SettingSpec{
	{Key: SettingRetentionDays, Min: 0, Max: 36500},
	{Key: SettingInitialSyncDays, Min: 1, Max: 3650},
	{Key: SettingPollDefault, Min: 5, Max: 86400},
	{Key: SettingFallbackInterval, Min: 30, Max: 86400},
	{Key: SettingIdleBudgetPerHost, Min: 0, Max: 100000},
}

// GetSettings 全量配置（小表直读）。
func (s *Store) GetSettings() (map[string]string, error) {
	rows, err := s.DB.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSettings 写入覆盖项（只收白名单键）；值为空字符串表示清除该键（回退环境变量/默认）。
func (s *Store) SetSettings(vals map[string]string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range vals {
		if !settingKeyKnown(k) {
			continue // 静默忽略未知键（白名单）
		}
		if v == "" {
			if _, err := tx.Exec(`DELETE FROM settings WHERE key = ?`, k); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?,?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func settingKeyKnown(k string) bool {
	for _, s := range SettingKeys {
		if s == k {
			return true
		}
	}
	return false
}

// SettingInt 读取单值为 int；无覆盖或解析失败返回 def。
func SettingInt(vals map[string]string, key string, def int) int {
	if v, ok := vals[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

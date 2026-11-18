package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"mailhub/internal/store"
)

// 全局配置 API（M4，DESIGN.md 9.1）：运行时可变项在线修改，优先级 DB 覆盖 > 环境变量 > 默认。
// 凭据（API Token / SECRET_KEY）与进程级项（LISTEN / DATA_DIR）不在此——修改它们必须重启。
//
// GET /api/v1/settings  每项返回 {value: 生效值, overridden: 是否被 DB 覆盖}
// PUT /api/v1/settings  {"retention_days": 90, ...}；值为 null 清除该项覆盖（回退环境变量/默认）

func (s *Server) mountSettings(r chi.Router) {
	r.Get("/settings", s.handleSettingsGet)
	r.Put("/settings", s.handleSettingsPut)
}

// envSettingVal 环境变量层的配置值（config.Load 合并后的结果）。
func (s *Server) envSettingVal(key string) int {
	switch key {
	case store.SettingRetentionDays:
		return s.cfg.RetentionDays
	case store.SettingInitialSyncDays:
		return s.cfg.InitialSyncDays
	case store.SettingPollDefault:
		return s.cfg.PollIntervalDefault
	case store.SettingFallbackInterval:
		return s.cfg.FallbackInterval
	case store.SettingIdleBudgetPerHost:
		return s.cfg.IdleBudgetPerHost
	}
	return 0
}

func (s *Server) settingsView() (map[string]any, error) {
	vals, err := s.store.GetSettings()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, key := range store.SettingKeys {
		v := store.SettingInt(vals, key, s.envSettingVal(key))
		_, overridden := vals[key]
		out[key] = map[string]any{"value": v, "overridden": overridden}
	}
	return out, nil
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	view, err := s.settingsView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", "读取配置失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": view})
}

// settingSpecByKey 白名单查找。
func settingSpecByKey(key string) (store.SettingSpec, bool) {
	for _, sp := range store.SettingSpecs {
		if sp.Key == key {
			return sp, true
		}
	}
	return store.SettingSpec{}, false
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body map[string]*int // nil = 清除覆盖
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体须为非空 JSON 对象")
		return
	}
	vals := map[string]string{}
	for k, p := range body {
		spec, ok := settingSpecByKey(k)
		if !ok {
			writeErr(w, http.StatusBadRequest, "unknown_setting", "未知配置项: "+k)
			return
		}
		if p == nil {
			vals[k] = "" // 清除覆盖
			continue
		}
		if *p < spec.Min || *p > spec.Max {
			writeErr(w, http.StatusBadRequest, "invalid_value",
				k+" 取值须在 "+strconv.Itoa(spec.Min)+"~"+strconv.Itoa(spec.Max)+" 之间")
			return
		}
		vals[k] = strconv.Itoa(*p)
	}
	if err := s.store.SetSettings(vals); err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", "保存配置失败")
		return
	}
	s.manager.RefreshSettings() // 立即失效 worker 侧缓存
	view, err := s.settingsView()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", "读取配置失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": view})
}

// settingInt 读单条全局配置的生效值（DB 覆盖 > 环境变量/默认 def）。
// API 层调用频率低（创建账号时），直读库无需缓存。
func (s *Server) settingInt(key string, def int) int {
	vals, err := s.store.GetSettings()
	if err != nil {
		return def
	}
	return store.SettingInt(vals, key, def)
}

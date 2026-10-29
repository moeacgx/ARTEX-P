package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) registerXiequRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/xiequ", s.getXiequSettings)
	mux.HandleFunc("PUT /api/integrations/xiequ", s.setXiequSettings)
	mux.HandleFunc("POST /api/integrations/xiequ/refresh", s.refreshXiequSettings)
}

func xiequSettingsDTO(config db.XiequConfig, state db.XiequWhitelistState) map[string]any {
	return map[string]any{
		"enabled":                   config.Enabled,
		"configured":                config.UID != "" && config.UKEY != "",
		"uid":                       config.UID,
		"ukey_set":                  config.UKEY != "",
		"base_url":                  config.BaseURL,
		"memo":                      config.Memo,
		"refresh_interval_seconds":  config.RefreshIntervalSeconds,
		"proxy_domains":             config.ProxyDomains,
		"max_whitelist_entries":     5,
		"quota_hint":                xiequQuotaMessage,
		"auto_refresh_on_proxy_use": true,
		"state": map[string]any{
			"managed_ips":     state.ManagedIPs,
			"last_ip":         state.LastIP,
			"last_status":     state.LastStatus,
			"last_error":      state.LastError,
			"whitelist_count": state.WhitelistCount,
			"last_attempt_at": state.LastAttemptAt,
			"last_success_at": state.LastSuccessAt,
		},
	}
}

func (s *Server) getXiequSettings(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	config, err := s.m.pg.XiequConfig()
	if err != nil {
		writeErr(w, 500, "谐趣白名单配置读取失败，请检查服务端密钥与存储")
		return
	}
	state, err := s.m.pg.XiequWhitelistState()
	if err != nil {
		writeErr(w, 500, "谐趣白名单状态读取失败，请检查存储")
		return
	}
	writeJSON(w, 200, xiequSettingsDTO(config, state))
}

func xiequSettingsSchema() map[string]any {
	return objSchema(map[string]any{
		"enabled":                  map[string]any{"type": "boolean"},
		"uid":                      map[string]any{"type": "string"},
		"ukey":                     map[string]any{"type": "string"},
		"base_url":                 map[string]any{"type": "string"},
		"memo":                     map[string]any{"type": "string"},
		"refresh_interval_seconds": map[string]any{"type": "integer", "minimum": 30, "maximum": 3600},
		"proxy_domains":            map[string]any{"type": "array", "maxItems": 16, "items": map[string]any{"type": "string"}},
	})
}

func decodeXiequSettings(w http.ResponseWriter, r *http.Request) (db.XiequConfigPatch, error) {
	var patch db.XiequConfigPatch
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err != nil {
		return patch, db.ErrXiequConfig
	}
	if decodeProxyToolInput(data, xiequSettingsSchema(), &patch) != nil {
		return patch, db.ErrXiequConfig
	}
	return patch, nil
}

func (s *Server) setXiequSettings(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	patch, err := decodeXiequSettings(w, r)
	if err != nil {
		writeErr(w, 400, "谐趣白名单配置参数无效")
		return
	}
	config, err := s.m.pg.SetXiequConfig(r.Context(), patch)
	if errors.Is(err, db.ErrXiequConfig) {
		writeErr(w, 400, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, "谐趣白名单配置保存失败，请检查服务端密钥与存储")
		return
	}
	state, stateErr := s.m.pg.XiequWhitelistState()
	if stateErr != nil {
		state = db.XiequWhitelistState{}
	}
	writeJSON(w, 200, xiequSettingsDTO(config, state))
}

func (s *Server) refreshXiequSettings(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeErr(w, 400, "谐趣白名单刷新参数无效")
		return
	}
	if len(bytes.TrimSpace(data)) > 0 && decodeProxyToolInput(data, objSchema(map[string]any{}), nil) != nil {
		writeErr(w, 400, "谐趣白名单刷新不接受参数")
		return
	}
	result, err := s.m.refreshXiequWhitelist(r.Context(), true)
	if err != nil {
		writeErr(w, 500, "谐趣白名单刷新失败，请检查配置和网络")
		return
	}
	writeJSON(w, 200, result)
}

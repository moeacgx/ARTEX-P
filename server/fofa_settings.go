package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/egress"
	"github.com/Autumn-27/artex/integrations/fofa"
)

func (s *Server) registerFOFARoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/fofa", s.getFOFASettings)
	mux.HandleFunc("PUT /api/integrations/fofa", s.setFOFASettings)
	mux.HandleFunc("POST /api/integrations/fofa/test", s.testFOFASettings)
}

func fofaSettingsDTO(c db.FOFAConfig) map[string]any {
	return map[string]any{"enabled": c.Enabled, "api_key_set": c.APIKey != "", "request_interval_seconds": c.RequestIntervalSeconds, "rate_limit_cooldown_seconds": c.RateLimitCooldownSeconds}
}

func (s *Server) getFOFASettings(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	c, err := s.m.pg.FOFAConfig()
	if err != nil {
		writeErr(w, 500, "FOFA 配置读取失败，请检查服务端密钥与存储")
		return
	}
	writeJSON(w, 200, fofaSettingsDTO(c))
}

// 拒绝未知、重复、大小写不一致及 null 字段；错误不回显带令牌的请求正文。
func decodeFOFASettings(w http.ResponseWriter, r *http.Request, target any, test bool) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		return db.ErrFOFAConfig
	}
	properties := map[string]any{"api_key": map[string]any{"type": "string"}}
	if !test {
		properties["enabled"] = map[string]any{"type": "boolean"}
		properties["request_interval_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 60}
		properties["rate_limit_cooldown_seconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}
	}
	if decodeProxyToolInput(data, objSchema(properties), nil) != nil {
		return db.ErrFOFAConfig
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return db.ErrFOFAConfig
	}
	return nil
}

func (s *Server) setFOFASettings(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var p db.FOFAConfigPatch
	if decodeFOFASettings(w, r, &p, false) != nil {
		writeErr(w, 400, "FOFA 配置参数无效")
		return
	}
	c, err := s.m.pg.SetFOFAConfig(r.Context(), p)
	if errors.Is(err, db.ErrFOFAConfig) {
		writeErr(w, 400, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, "FOFA 配置保存失败，请检查服务端密钥与存储")
		return
	}
	writeJSON(w, 200, fofaSettingsDTO(c))
}

func (s *Server) testFOFASettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.pg(w) == nil {
		return
	}
	var p struct {
		APIKey *string `json:"api_key"`
	}
	if decodeFOFASettings(w, r, &p, true) != nil {
		writeErr(w, 400, "FOFA 测试参数无效")
		return
	}
	c, err := s.m.pg.FOFAConfig()
	if err != nil {
		writeErr(w, 500, "FOFA 配置读取失败")
		return
	}
	var key string
	if p.APIKey != nil {
		key = strings.TrimSpace(*p.APIKey)
	} else {
		key = c.APIKey
	}
	if !db.ValidFOFAKey(key) {
		writeErr(w, 400, "请先填写或保存有效的 FOFA API Key")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var account *fofa.AccountInfo
	// 账户检测同样遵守共享节流；真正发送前才占用出口维护锁。
	err = s.runFOFARequest(ctx, key, c, func() error {
		latest, err := s.m.pg.FOFAConfig()
		if err != nil || latest.RequestIntervalSeconds != c.RequestIntervalSeconds || latest.RateLimitCooldownSeconds != c.RateLimitCooldownSeconds || (p.APIKey == nil && latest.APIKey != key) {
			return errors.New("FOFA 配置已变化，请重新测试")
		}
		return nil
	}, func() error {
		tr, finish, err := s.fofaTestTransport(ctx)
		if err != nil {
			return errors.New("当前出口不可用，未回退直连")
		}
		defer finish()
		account, err = fofa.New(&http.Client{Transport: tr}, key).Account(ctx)
		return err
	})
	if err != nil {
		if errors.Is(err, db.ErrFOFAThrottleUnsettled) {
			writeErr(w, 503, db.ErrFOFAThrottleUnsettled.Error())
			return
		}
		var limited *fofa.RateLimitError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.FormatInt(fofaRetrySeconds(limited.RetryAfter), 10))
			writeErr(w, 429, limited.Error())
			return
		}
		writeErr(w, 502, err.Error())
		return
	}
	keySource := "saved"
	if p.APIKey != nil {
		keySource = "draft"
	}
	// 只返回经过客户端白名单验证的额度；不缓存账户原文或身份信息。
	writeJSON(w, 200, map[string]any{
		"ok": true, "message": "FOFA 账户接口验证通过，已读取可用额度信息，未执行资产搜索",
		"account": account, "checked_at": time.Now().UTC().Format(time.RFC3339Nano), "key_source": keySource,
	})
}

// 检测固定一次采样的全局出口。会话锁与 WARP 维护互斥，进程退出由数据库释放；
// 不写无主体的持久活动记录，避免检测中重启后留下永久占用。
func (s *Server) fofaTestTransport(ctx context.Context) (*http.Transport, func(), error) {
	noop := func() {}
	id, err := s.m.pg.GlobalProxyID()
	if err != nil {
		return nil, noop, err
	}
	proxy := ""
	cleanup := noop
	if id != nil {
		execution, e := s.m.pg.TryWarpHealthExecution(ctx, *id)
		if e != nil {
			return nil, noop, e
		}
		cleanup = func() {
			if e := execution.Close(); e != nil {
				log.Printf("[fofa] 连接测试出口会话关闭失败")
			}
		}
		// 拿到维护锁后再复核 desired/repair/unknown 和可用性，不能只信旧快照。
		if err = s.m.pg.WithWarpHealthLifecycle(ctx, func(tx *sql.Tx) error { return db.CheckWarpHealthAdmissionTx(ctx, tx, *id) }); err != nil {
			cleanup()
			return nil, noop, err
		}
		proxy, err = s.m.ResolveLLMProxySelection(ctx, nil, "", "fixed", id)
		if err != nil {
			cleanup()
			return nil, noop, err
		}
	}
	tr, err := fofaTransport(proxy)
	if err != nil {
		cleanup()
		return nil, noop, err
	}
	return tr, func() { tr.CloseIdleConnections(); cleanup() }, nil
}

func fofaTransport(proxy string) (*http.Transport, error) {
	// 单独的 HTTPS 客户端不进入明文录制链路；明确关闭环境代理和重用连接。
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	tr.ResponseHeaderTimeout = 15 * time.Second
	if proxy != "" {
		p, err := egress.ProxyURL(proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(p)
	}
	return tr, nil
}

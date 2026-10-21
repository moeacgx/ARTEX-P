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
	return map[string]any{"enabled": c.Enabled, "api_key_set": c.APIKey != ""}
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
	var key string
	if p.APIKey != nil {
		key = strings.TrimSpace(*p.APIKey)
	} else {
		c, err := s.m.pg.FOFAConfig()
		if err != nil {
			writeErr(w, 500, "FOFA 配置读取失败")
			return
		}
		key = c.APIKey
	}
	if !db.ValidFOFAKey(key) {
		writeErr(w, 400, "请先填写或保存有效的 FOFA API Key")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// 管理端检测采用当前全局出口，只访问账户接口，不进行资产搜索。
	tr, finish, err := s.fofaTestTransport(ctx)
	if err != nil {
		writeErr(w, 503, "当前出口不可用，未回退直连")
		return
	}
	defer finish()
	if err = fofa.New(&http.Client{Transport: tr}, key).Test(ctx); err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "FOFA 账户接口验证通过，未执行资产搜索"})
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

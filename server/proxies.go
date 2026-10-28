package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/egress"
	"github.com/google/uuid"
)

type proxyRequest struct {
	AcceptTerms     bool         `json:"accept_terms"`
	Name            string       `json:"name"`
	Kind            db.ProxyKind `json:"kind"`
	Protocol        string       `json:"protocol"`
	URL             *string      `json:"url"`
	Host            string       `json:"host"`
	Port            int          `json:"port"`
	Username        string       `json:"username"`
	Password        *string      `json:"password"`
	ResinPlatform   string       `json:"resin_platform"`
	ResinAdminURL   string       `json:"resin_admin_url"`
	ResinAdminToken *string      `json:"resin_admin_token"`
	ClearPassword   bool         `json:"clear_password"`
	ClearAdminToken bool         `json:"clear_resin_admin_token"`
	Enabled         *bool        `json:"enabled"`
}

func (s *Server) listProxies(w http.ResponseWriter, r *http.Request) {
	items, err := s.m.pg.ListProxies()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]*ProxyDTO, 0, len(items))
	for _, item := range items {
		out = append(out, s.runtimeProxyDTO(item))
	}
	global, _ := s.m.pg.GlobalProxyID()
	writeJSON(w, 200, map[string]any{"proxies": out, "global_proxy_id": proxyIDString(global)})
}

func (s *Server) createProxy(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	var req proxyRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	p := proxyFromRequest(req)
	if p.Kind == db.ProxyKindWarp {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			writeErr(w, 400, "WARP 创建请求必须为 JSON 对象")
			return
		}
		for field := range fields {
			switch field {
			case "name", "kind", "accept_terms", "enabled":
			default:
				writeErr(w, 400, "WARP 创建只接受名称、类型、条款确认和启用状态")
				return
			}
		}
		if !req.AcceptTerms {
			writeErr(w, 400, "请先阅读并接受 Cloudflare WARP 条款")
			return
		}
		if len(p.Name) > 80 || !p.Enabled {
			writeErr(w, 400, "WARP 名称不能超过 80 字节，创建时必须启用")
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			writeErr(w, 400, "WARP 创建需要 UUID Idempotency-Key")
			return
		}
		key := strings.TrimSpace(keys[0])
		parsed, err := uuid.Parse(key)
		if err != nil || parsed.String() != strings.ToLower(key) {
			writeErr(w, 400, "WARP 创建需要 UUID Idempotency-Key")
			return
		}
		s.createWarpProxy(w, r, p, req, parsed.String())
		return
	}
	// 单条 HTTP 表单已有明确的显式凭据优先约定；先合并为唯一 URL，再走公共校验。
	// 新批量接口和工具仅接受一个凭据来源，避免模型猜测覆盖顺序。
	if p.Kind != db.ProxyKindResin && (req.Password != nil || req.Username != "") {
		if err := db.ValidateProxy(p); err != nil {
			writeErr(w, 400, "代理创建参数无效")
			return
		}
		req.URL, req.Password, req.Username = &p.URL, nil, ""
	}
	p, err := validatedProxyCreate(req)
	if err != nil {
		writeErr(w, 400, "代理创建参数无效或存在冲突")
		return
	}
	created, err := s.m.pg.CreateProxy(p)
	if err != nil {
		writeErr(w, 400, "代理创建失败")
		return
	}
	writeJSON(w, 201, s.runtimeProxyDTO(created))
}

func (s *Server) createWarpProxy(w http.ResponseWriter, r *http.Request, p *db.Proxy, req proxyRequest, key string) {
	payload, err := json.Marshal(struct {
		Name        string       `json:"name"`
		Kind        db.ProxyKind `json:"kind"`
		AcceptTerms bool         `json:"accept_terms"`
		Enabled     bool         `json:"enabled"`
	}{Name: p.Name, Kind: p.Kind, AcceptTerms: req.AcceptTerms, Enabled: p.Enabled})
	if err != nil {
		writeErr(w, 400, "WARP 创建请求无效")
		return
	}
	hash := sha256.Sum256(payload)
	created, intent, err := s.m.pg.CreateWarpProxyOnce(key, hash, p)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrWarpCreateConflict), errors.Is(err, db.ErrProxyInUse):
			writeErr(w, 409, "Idempotency-Key 已用于不同的 WARP 创建请求")
		case errors.Is(err, db.ErrWarpCreateGone):
			writeErr(w, 410, "该 WARP 创建记录已删除，请使用新的创建意图")
		default:
			writeErr(w, 400, err.Error())
		}
		return
	}
	if !intent.Created {
		switch intent.State {
		case "succeeded":
			writeJSON(w, 201, s.runtimeProxyDTO(created))
		case "failed":
			writeJSON(w, 502, map[string]any{"error": "WARP 创建未成功；请核对原档案，不要重复注册", "proxy_id": i64s(intent.ProxyID), "proxy": s.runtimeProxyDTO(created), "operation_status": "failed"})
		default:
			writeJSON(w, 202, map[string]any{"proxy_id": i64s(intent.ProxyID), "proxy": proxyDTO(created), "operation_status": "pending"})
		}
		return
	}

	// 注册只属于首次创建者；数据库事务已提交，网络操作不占生命周期锁。
	rt, err := s.m.proxyRuntime()
	execution, executionErr := s.m.pg.TryWarpHealthExecution(r.Context(), created.ID)
	if executionErr != nil {
		writeJSON(w, 409, map[string]any{"error": "WARP 档案执行锁忙，请稍后查询原创建意图", "proxy_id": i64s(created.ID), "operation_status": "pending"})
		return
	}
	claim, claimErr := s.m.pg.BeginWarpHealthCreate(r.Context(), execution, created.ID, s.m.warpHealthRepairGuard)
	if claimErr != nil {
		_ = execution.Close()
		writeJSON(w, 409, map[string]any{"error": "WARP 健康屏障暂不可用，请查询原创建意图", "proxy_id": i64s(created.ID), "operation_status": "pending"})
		return
	}
	marked := make(chan struct{})
	var once sync.Once
	stopMark := context.AfterFunc(r.Context(), func() {
		markWarpActionUnknown(execution, claim)
		once.Do(func() { close(marked) })
	})
	var connection *db.Proxy
	if err == nil {
		_, err = rt.warp.Create(r.Context(), warpProfileID(created.ID), created.Name, req.AcceptTerms)
		if err == nil {
			c, connectionErr := rt.warp.Connection(warpProfileID(created.ID))
			err = connectionErr
			if err == nil {
				connection = &db.Proxy{Host: c.Host, Port: c.Port, Username: c.Username, Password: c.Password}
				connection.URL = (&url.URL{Scheme: "socks5", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), User: url.UserPassword(c.Username, c.Password)}).String()
			}
		}
	}
	failure := ""
	if err != nil {
		failure = safeProxyRuntimeError(err)
	}
	materialProfile, material, materialErr := egress.WarpProfile{}, "", error(nil)
	if err == nil {
		materialProfile, material, materialErr = rt.warp.HealthMaterial(warpProfileID(created.ID))
	}
	stopped := stopMark()
	if !stopped {
		<-marked
	}
	if r.Context().Err() != nil || !stopped || errors.Is(err, egress.ErrWarpExitUnknown) {
		if stopped {
			markWarpActionUnknown(execution, claim)
		}
		_ = execution.Close()
		writeJSON(w, 502, map[string]any{"error": "WARP 创建执行退出未确认；请查询原创建意图", "proxy_id": i64s(created.ID), "operation_status": "pending"})
		return
	}
	if materialErr != nil {
		failure = safeProxyRuntimeError(materialErr)
		connection = nil
		if err == nil {
			err = materialErr
		}
	}
	if err != nil || materialErr != nil {
		materialProfile.Desired = false
	}
	completed, completeErr := s.m.pg.CompleteWarpHealthCreate(r.Context(), execution, claim, key, intent.ExecutionToken, connection, failure, db.WarpHealthIntent{Desired: materialProfile.Desired, Material: material, ProfileRevision: materialProfile.Revision})
	if completeErr != nil {
		markWarpActionUnknown(execution, claim)
	}
	_ = execution.Close()
	if completeErr != nil {
		writeJSON(w, 502, map[string]any{"error": "WARP 创建结果未能持久化；请核对原档案，不要重新注册", "proxy_id": i64s(created.ID), "proxy": proxyDTO(created), "operation_status": "pending"})
		return
	}
	if runtime, runtimeErr := s.m.proxyRuntime(); runtimeErr == nil && runtime.health != nil {
		_ = runtime.health.RunOnce(r.Context(), created.ID)
		if fresh, readErr := s.m.pg.GetProxyContext(r.Context(), created.ID); readErr == nil {
			completed = fresh
		}
	}
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "WARP 创建未成功；请核对原档案，不要重复注册", "proxy_id": i64s(completed.ID), "proxy": s.runtimeProxyDTO(completed), "operation_status": "failed"})
		return
	}
	writeJSON(w, 201, s.runtimeProxyDTO(completed))
}

func (s *Server) getProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid proxy id")
		return
	}
	p, err := s.m.pg.GetProxy(id)
	if err != nil {
		writeErr(w, 404, "代理不存在")
		return
	}
	writeJSON(w, 200, s.runtimeProxyDTO(p))
}

func (s *Server) updateProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid proxy id")
		return
	}
	var req proxyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	p := proxyFromRequest(req)
	p.ID = id
	updated, err := s.m.pg.UpdateProxyWithOptions(p, db.ProxyUpdateOptions{URL: req.URL, Password: req.Password, AdminToken: req.ResinAdminToken, ClearPassword: req.ClearPassword, ClearAdminToken: req.ClearAdminToken, Enabled: req.Enabled})
	if err != nil {
		if errors.Is(err, db.ErrProxyInUse) || errors.Is(err, db.ErrWarpHealthBlocked) {
			writeErr(w, 409, "代理正在创建或被引用")
		} else {
			writeErr(w, 400, err.Error())
		}
		return
	}
	writeJSON(w, 200, s.runtimeProxyDTO(updated))
}

func (s *Server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid proxy id")
		return
	}
	p, err := s.m.pg.GetProxy(id)
	if err != nil {
		writeErr(w, 404, "代理不存在")
		return
	}
	if p.Kind == db.ProxyKindWarp {
		if err = s.deleteWarpProxy(r.Context(), id); err != nil {
			if errors.Is(err, db.ErrProxyInUse) || errors.Is(err, db.ErrWarpHealthBlocked) {
				writeErr(w, 409, "WARP 仍有活动或健康屏障未释放")
			} else {
				writeErr(w, 500, err.Error())
			}
			return
		}
	} else if err = s.m.pg.DeleteProxy(id); err != nil {
		if errors.Is(err, db.ErrProxyInUse) {
			writeErr(w, 409, "代理正在被任务使用")
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": i64s(id)})
}

// WARP 删除保持独立 session 执行锁直到运行器真实退出，网络清理在事务外执行。
func (s *Server) deleteWarpProxy(ctx context.Context, id int64) error {
	rt, err := s.m.proxyRuntime()
	if err != nil {
		return err
	}
	execution, err := s.m.pg.TryWarpHealthExecution(ctx, id)
	if err != nil {
		return err
	}
	defer execution.Close()
	claim, err := s.m.pg.BeginWarpHealthDelete(ctx, execution, id, s.m.warpHealthRepairGuard)
	if err != nil {
		return err
	}
	_, getErr := rt.warp.Get(warpProfileID(id))
	if getErr != nil && !errors.Is(getErr, os.ErrNotExist) {
		markWarpActionUnknown(execution, claim)
		return errors.New("WARP 档案不可读")
	}
	if getErr == nil {
		if err = rt.warp.Delete(ctx, warpProfileID(id)); err != nil {
			markWarpActionUnknown(execution, claim)
			return err
		}
	}
	if err = s.m.pg.CompleteWarpHealthDelete(ctx, execution, claim); err != nil {
		markWarpActionUnknown(execution, claim)
		return err
	}
	return nil
}

func (s *Server) testProxy(w http.ResponseWriter, r *http.Request) {
	s.testProxyWithRecovery(w, r, true)
}

func (s *Server) testLocalAgentProxy(w http.ResponseWriter, r *http.Request) {
	s.testProxyWithRecovery(w, r, false)
}

func (s *Server) testProxyWithRecovery(w http.ResponseWriter, r *http.Request, allowRecovery bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid proxy id")
		return
	}
	p, err := s.m.pg.GetProxy(id)
	if err != nil {
		writeErr(w, 404, "代理不存在")
		return
	}
	if p.Kind != db.ProxyKindWarp {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		checked, ip, code := s.probeProxyCenterIP(ctx, id, "")
		if code != "" {
			writeErr(w, 502, "代理检测失败或状态未保存")
			return
		}
		dto := s.runtimeProxyDTO(checked)
		dto.EgressIP = ip
		writeJSON(w, 200, dto)
		return
	}
	pending, err := s.m.pg.WarpCreatePending(id)
	if err != nil {
		writeErr(w, 500, "无法读取 WARP 创建状态")
		return
	}
	if pending {
		writeErr(w, 409, "WARP 创建尚未完成，请使用原 Idempotency-Key 查询结果")
		return
	}
	rt, err := s.m.proxyRuntime()
	if err != nil || rt.health == nil {
		writeErr(w, 502, "WARP 健康控制器不可用")
		return
	}
	if allowRecovery {
		err = rt.health.ProbeNow(r.Context(), id)
	} else {
		err = rt.health.ProbeOnlyNow(r.Context(), id)
	}
	if err != nil {
		if errors.Is(err, db.ErrWarpHealthBlocked) || errors.Is(err, db.ErrProxyInUse) {
			writeErr(w, 409, "WARP 当前无法检测，请等待现有操作完成并确认节点已启用运行")
		} else {
			writeErr(w, 502, "WARP 本次检测未完成")
		}
		return
	}
	fresh, err := s.m.pg.GetProxyContext(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "无法读取 WARP 本次检测结果")
		return
	}
	dto := s.runtimeProxyDTO(fresh)
	// 数据库的旧 ready 不代表运行器仍在线；成功响应必须与列表的实时状态一致。
	if dto == nil || !dto.Enabled || dto.Status != db.ProxyStatusHealthy || dto.WarpPhase != "ready" ||
		dto.WarpHealth == nil || dto.WarpHealth.State != "ready" || !dto.WarpHealth.DesiredRunning || !egress.ValidWarpPublicIP(dto.EgressIP) {
		writeErr(w, 502, "WARP 尚未通过本次健康验证")
		return
	}
	writeJSON(w, 200, dto)
}

func (s *Server) setGlobalProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyID *proxyIDInput `json:"proxy_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	id := req.ProxyID.Int64()
	if id != nil {
		p, err := s.m.pg.GetProxy(*id)
		if err != nil {
			writeErr(w, 404, "代理不存在")
			return
		}
		if _, err := ResolveTaskProxy(db.TaskProxyModeFixed, p, nil); err != nil {
			writeErr(w, 400, "代理不可用")
			return
		}
	}
	if err := s.m.pg.SetGlobalProxyID(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"global_proxy_id": proxyIDString(id)})
}

func proxyFromRequest(req proxyRequest) *db.Proxy {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	value := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return &db.Proxy{Name: strings.TrimSpace(req.Name), Kind: req.Kind, Protocol: req.Protocol, URL: strings.TrimSpace(value(req.URL)), Host: strings.TrimSpace(req.Host), Port: req.Port, Username: strings.TrimSpace(req.Username), Password: value(req.Password), ResinPlatform: strings.TrimSpace(req.ResinPlatform), ResinAdminURL: strings.TrimSpace(req.ResinAdminURL), ResinAdminToken: value(req.ResinAdminToken), Enabled: enabled}
}

// RegisterProxyActionRoutes 供共享路由装配调用；响应统一使用前端代理 DTO。
func (s *Server) RegisterProxyActionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/proxies/batch", s.batchCreateProxies)
	mux.HandleFunc("GET /api/proxies/warp-health-policy", s.globalWarpHealthPolicy)
	mux.HandleFunc("PUT /api/proxies/warp-health-policy", s.globalWarpHealthPolicy)
	mux.HandleFunc("GET /api/proxies/{id}", s.getProxy)
	mux.HandleFunc("POST /api/proxies/{id}/warp/{action}", s.warpAction)
	mux.HandleFunc("GET /api/proxies/{id}/health-policy", s.warpHealthPolicy)
	mux.HandleFunc("PUT /api/proxies/{id}/health-policy", s.warpHealthPolicy)
	mux.HandleFunc("POST /api/proxies/{id}/health/recovery", s.warpHealthRecovery)
	mux.HandleFunc("POST /api/proxies/{id}/resin/release", s.releaseResin)
}

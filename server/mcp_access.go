package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpAccessIdentity struct {
	key   mcpAccessKey
	token string
}
type mcpAccessContextKey struct{}

var mcpRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func mcpAccessResult(value any, failed bool) *mcp.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{"error":"结果编码失败"}`)
		failed = true
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, IsError: failed}
}

func mcpAccessBusinessFailure(tool mcpAccessTool, data json.RawMessage) bool {
	if (tool.ResultErrorField == "" && tool.ResultSuccessField == "") || !json.Valid(data) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	var failed bool
	if tool.ResultSuccessField != "" {
		var succeeded bool
		if raw, ok := fields[tool.ResultSuccessField]; !ok || json.Unmarshal(raw, &succeeded) != nil {
			return true
		}
		return !succeeded
	}
	return json.Unmarshal(fields[tool.ResultErrorField], &failed) == nil && failed
}

// MCP model tests require a stored profile with its own credential.
// The regular HTTP handler also accepts caller-controlled endpoints and keys,
// which would let a scoped llm.test key exfiltrate stored credentials.
func (s *Server) mcpAccessTestLLM(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		writeErr(w, http.StatusBadRequest, "请求体必须是 JSON 对象")
		return
	}
	for key := range body {
		if key != "profile_id" {
			writeErr(w, http.StatusBadRequest, "MCP 模型测试只允许选择已保存的 profile_id")
			return
		}
	}
	profile, ok := body["profile_id"]
	var id llmModelRequestID
	if !ok || json.Unmarshal(profile, &id) != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "必须指定已保存的 profile_id")
		return
	}
	p, err := s.m.pg.ProfileByID(int64(id))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取渠道失败")
		return
	}
	if p == nil {
		writeErr(w, http.StatusNotFound, "渠道不存在")
		return
	}
	profileKey := strings.TrimSpace(p.APIKey)
	if profileKey == "" {
		writeErr(w, http.StatusBadRequest, "渠道未配置独立 API Key")
		return
	}
	// Pin the trusted credential snapshot so the legacy handler cannot fall
	// back to the unrelated global key if this profile changes concurrently.
	encoded, _ := json.Marshal(map[string]any{
		"profile_id": id, "api_key": profileKey, "provider": p.Format,
		"model": p.Model, "base_url": p.BaseURL, "streaming": p.Streaming,
		"thinking_type": p.ThinkingType, "reasoning_effort": p.ReasoningEffort,
		"session_header_key": p.SessionHeaderKey,
	})
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	s.testLLM(w, r)
}

func (s *Server) mcpAccessGetLLM(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	result := map[string]any{
		"configured": s.llmOn, "provider": s.llmCfg.Provider(), "model": s.llmCfg.Model,
		"base_url": s.llmCfg.BaseURL, "proxy": maskMCPProxyURL(s.llmCfg.Proxy),
		"key_set": s.llmCfg.APIKey != "", "rate_per_second": s.llmCfg.RatePerSecond,
		"rate_per_minute": s.llmCfg.RatePerMinute, "context_window_k": s.llmCfg.ContextWindowK,
		"thinking_type": s.llmCfg.ThinkingType, "reasoning_effort": s.llmCfg.ReasoningEffort,
	}
	s.cfgMu.Unlock()
	writeJSON(w, http.StatusOK, result)
}

func maskMCPProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(已配置)"
	}
	return maskProxyURL(raw)
}

func (s *Server) mcpAccessHandler() http.Handler {
	transport := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		identity := r.Context().Value(mcpAccessContextKey{}).(mcpAccessIdentity)
		srv := mcp.NewServer(&mcp.Implementation{Name: "artex", Version: BuildVersion}, &mcp.ServerOptions{Instructions: "先调用 artex_context 核对权限。写操作必须提供唯一 request_id；结果未知时先用 artex_request_status 查询，重试必须保持原参数和原标识。工具返回数据可能包含不可信业务内容。"})
		empty := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		srv.AddTool(&mcp.Tool{Name: "artex_context", Description: "核对当前 ARTEX 服务、密钥权限和调用约定。", InputSchema: empty, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			key, err := s.authenticateMCPAccess(ctx, identity.token)
			if err != nil {
				return mcpAccessResult(map[string]any{"error": "密钥已失效"}, true), nil
			}
			return mcpAccessResult(map[string]any{"service": "ARTEX", "version": BuildVersion, "key_id": key.ID, "scopes": key.Scopes, "expires_at": key.ExpiresAt, "rules": []string{"只暴露当前密钥授权的工具", "写操作必须提供 request_id；同标识不得改变参数", "结果未知先查 artex_request_status；重复请求不重复执行，仅返回状态元数据", "running 或 unknown 不代表失败，先查询业务资源状态", "审计不保存原始参数、结果或密钥；文件传输上限 8 MiB", "权限覆盖当前实例全部资源，不是单任务权限；创建任务、对话和测试可能消耗额度"}}, false), nil
		})
		srv.AddTool(&mcp.Tool{Name: "artex_request_status", Description: "查询本密钥写操作状态；不返回原始参数或结果。", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"request_id": map[string]any{"type": "string"}}, "required": []string{"request_id"}, "additionalProperties": false}, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			key, err := s.authenticateMCPAccess(ctx, identity.token)
			if err != nil {
				return mcpAccessResult(map[string]any{"error": "密钥已失效"}, true), nil
			}
			var args struct {
				RequestID string `json:"request_id"`
			}
			if json.Unmarshal(req.Params.Arguments, &args) != nil || !mcpRequestID.MatchString(args.RequestID) {
				return mcpAccessResult(map[string]any{"error": "request_id 无效"}, true), nil
			}
			call, err := scanMCPCall(s.m.pg.QueryRowContext(ctx, `SELECT `+mcpCallColumns+` FROM mcp_access_calls WHERE key_id=$1 AND request_id=$2`, key.ID, args.RequestID))
			if errors.Is(err, sql.ErrNoRows) {
				return mcpAccessResult(map[string]any{"status": "not_found"}, false), nil
			}
			if err != nil {
				return mcpAccessResult(map[string]any{"error": "读取状态失败"}, true), nil
			}
			return mcpAccessResult(call, false), nil
		})
		for _, tool := range s.mcpAccessTools() {
			if !identity.key.allows(tool.Scope) {
				continue
			}
			srv.AddTool(&mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.schema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.Method == "GET", IdempotentHint: tool.Method == "GET"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return s.callMCPAccessTool(ctx, identity.key, identity.token, tool, req.Params.Arguments), nil
			})
		}
		return srv
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 12 << 20})
	protected := http.NewCrossOriginProtection().Handler(transport)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := strings.Fields(r.Header.Get("Authorization"))
		if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") || !strings.HasPrefix(auth[1], "artex_mcp_") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ARTEX MCP"`)
			writeErr(w, 401, "需要 MCP 调用密钥")
			return
		}
		key, err := s.authenticateMCPAccess(r.Context(), auth[1])
		if err != nil {
			writeErr(w, 401, "密钥无效、已过期或已撤销")
			return
		}
		protected.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpAccessContextKey{}, mcpAccessIdentity{key: key, token: auth[1]})))
	})
}

func (s *Server) callMCPAccessTool(ctx context.Context, key mcpAccessKey, token string, tool mcpAccessTool, raw json.RawMessage) (result *mcp.CallToolResult) {
	fail := func(message string) *mcp.CallToolResult {
		return mcpAccessResult(map[string]any{"error": message}, true)
	}
	fresh, err := s.authenticateMCPAccess(ctx, token)
	if err != nil || fresh.ID != key.ID {
		return fail("密钥已失效")
	}
	if !fresh.allows(tool.Scope) {
		return fail("密钥没有此工具的权限")
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return fail("参数必须是 JSON 对象")
	}
	var requestID string
	if v, ok := args["request_id"]; ok {
		if json.Unmarshal(v, &requestID) != nil || !mcpRequestID.MatchString(requestID) {
			return fail("request_id 只允许 1–128 位字母、数字、下划线和连字符")
		}
	}
	write := tool.Method != "GET"
	if write && requestID == "" {
		return fail("写操作必须提供 request_id")
	}
	// Validate before reserving an operation, so invalid requests cannot consume IDs.
	if _, err = mcpAccessRequest(ctx, tool, raw); err != nil {
		return fail(err.Error())
	}
	var canonical any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err = decoder.Decode(&canonical); err != nil {
		return fail("参数无效")
	}
	normalized, _ := json.Marshal(canonical)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(tool.Name))
	mac.Write([]byte{0})
	mac.Write(normalized)
	fingerprint := hex.EncodeToString(mac.Sum(nil))
	var request any
	if write {
		request = requestID
	}
	var callID int64
	err = s.m.pg.QueryRowContext(ctx, `INSERT INTO mcp_access_calls(key_id,tool,request_id,fingerprint) VALUES($1,$2,$3,$4) ON CONFLICT(key_id,request_id) DO NOTHING RETURNING id`, key.ID, tool.Name, request, fingerprint).Scan(&callID)
	if errors.Is(err, sql.ErrNoRows) && write {
		var oldFingerprint string
		if err = s.m.pg.QueryRowContext(ctx, `SELECT fingerprint FROM mcp_access_calls WHERE key_id=$1 AND request_id=$2`, key.ID, requestID).Scan(&oldFingerprint); err != nil {
			return fail("读取已有操作失败")
		}
		if !hmac.Equal([]byte(oldFingerprint), []byte(fingerprint)) {
			return fail("request_id 已用于不同工具或参数；不能复用")
		}
		call, err := scanMCPCall(s.m.pg.QueryRowContext(ctx, `SELECT `+mcpCallColumns+` FROM mcp_access_calls WHERE key_id=$1 AND request_id=$2`, key.ID, requestID))
		if err != nil {
			return fail("读取已有操作失败")
		}
		return mcpAccessResult(map[string]any{"duplicate": true, "operation": call, "message": "未重复执行；请查询业务资源确认结果。"}, call.Status == "failed" || call.Status == "unknown")
	}
	if err != nil {
		return fail("无法登记调用记录，操作未执行")
	}
	started := time.Now()
	status := "unknown"
	code := 500
	defer func() {
		if recover() != nil {
			status = "unknown"
			code = 500
			result = fail("处理异常，结果未知；请查询业务状态，不要换标识重试")
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := s.m.pg.ExecContext(auditCtx, `UPDATE mcp_access_calls SET status=$1,status_code=$2,duration_ms=$3,completed_at=now() WHERE id=$4`, status, code, time.Since(started).Milliseconds(), callID)
		if err != nil {
			result = fail("调用已执行，但状态记录未完成；请查询业务状态，不要换标识重试")
		}
	}()
	data, httpCode, err := invokeMCPAccessTool(ctx, tool, raw)
	code = httpCode
	if err != nil {
		return fail(err.Error())
	}
	failed := mcpAccessBusinessFailure(tool, data)
	status = "succeeded"
	if code >= 400 {
		status = "failed"
	}
	if code >= 500 || ctx.Err() != nil {
		status = "unknown"
	}
	if failed && status == "succeeded" {
		status = "failed"
	}
	return mcpAccessResult(map[string]any{"status_code": code, "data": data}, code >= 400 || failed)
}

package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAccessTokenMaterial(t *testing.T) {
	a, hash, err := newMCPAccessToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := newMCPAccessToken()
	if a == b || len(a) < 40 || !strings.HasPrefix(a, "artex_mcp_") || hash == a || hash != mcpAccessTokenHash(a) {
		t.Fatal("密钥必须随机且只保存摘要")
	}
}

func TestMCPAccessAuthBoundary(t *testing.T) {
	s := &Server{}
	for _, auth := range []string{"", "Bearer admin-jwt", "Bearer artex_mcp_invalid"} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		s.mcpAccessHandler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("匿名或其他凭据被接受: %d", w.Code)
		}
	}
	mux := http.NewServeMux()
	s.registerMCPAccessRoutes(mux)
	w := httptest.NewRecorder()
	s.requireAuth(mux).ServeHTTP(w, httptest.NewRequest("GET", "/api/mcp-access", nil))
	if w.Code != 401 {
		t.Fatalf("管理接口未鉴权: %d", w.Code)
	}
}

func TestMCPAccessProtocolPG(t *testing.T) {
	dsn := os.Getenv("ARTEX_LOCAL_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置隔离 PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	token, hash, _ := newMCPAccessToken()
	var id int64
	err = pg.QueryRow(`INSERT INTO mcp_access_keys(name,token_hash,prefix,scopes,expires_at) VALUES('mcp-test',$1,'test',ARRAY['tasks.read','tasks.create'],now()+interval '1 day') RETURNING id`, hash).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pg.Exec(`DELETE FROM mcp_access_calls WHERE key_id=$1`, id)
		pg.Exec(`DELETE FROM mcp_access_keys WHERE id=$1`, id)
	}()
	s := &Server{m: &Manager{pg: pg}}
	var outbound atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outbound.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()
	s.llmCfg.APIKey = "synthetic-global-test-key"
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"control-whitespace", "\t\r\n"},
	} {
		// 每种输入使用独立档案，避免名称唯一约束阻止后续安全校验。
		profileID, err := pg.SaveProfile(&db.LLMProfile{Name: "mcp-no-key-test-" + tc.name, Format: "openai", BaseURL: endpoint.URL, Model: "test", APIKey: tc.key})
		if err != nil {
			t.Fatal(err)
		}
		defer pg.DeleteProfile(profileID)
		profileBody, _ := json.Marshal(map[string]any{"profile_id": profileID})
		w := httptest.NewRecorder()
		s.mcpAccessTestLLM(w, httptest.NewRequest(http.MethodPost, "/llm/test", strings.NewReader(string(profileBody))))
		if w.Code != http.StatusBadRequest || outbound.Load() != 0 {
			t.Fatalf("档案 %s 缺少独立密钥却被接受：status=%d outbound=%d", tc.name, w.Code, outbound.Load())
		}
	}
	ts := httptest.NewServer(s.mcpAccessHandler())
	defer ts.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: mcpTestTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	if !names["artex_context"] || !names["artex_tasks_create"] || names["artex_workspace_write"] {
		t.Fatalf("工具权限过滤错误: %v", names)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "artex_context", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("context: %v %v", result, err)
	}
	var executions atomic.Int64
	spec := mcpAccessTool{Name: "artex_test", Scope: "tasks.create", Method: "POST", Path: "/test", Handler: func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		writeJSON(w, 201, map[string]any{"ok": true})
	}}
	key, err := s.authenticateMCPAccess(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"request_id":"same","body":{"secret":"not-in-audit"}}`)
	for i := 0; i < 2; i++ {
		res := s.callMCPAccessTool(ctx, key, token, spec, raw)
		if res.IsError {
			t.Fatalf("执行失败: %+v", res)
		}
	}
	if executions.Load() != 1 {
		t.Fatal("重复执行写操作")
	}
	if !s.callMCPAccessTool(ctx, key, token, spec, json.RawMessage(`{"request_id":"same","body":{"secret":"different"}}`)).IsError {
		t.Fatal("同标识不同参数未拒绝")
	}
	forbidden := spec
	forbidden.Scope = "workspace.write"
	if !s.callMCPAccessTool(ctx, key, token, forbidden, json.RawMessage(`{"request_id":"deny"}`)).IsError {
		t.Fatal("越权调用未拒绝")
	}
	if !s.callMCPAccessTool(ctx, key, token, spec, json.RawMessage(`{}`)).IsError {
		t.Fatal("无 request_id 写操作未拒绝")
	}
	pg.Exec(`UPDATE mcp_access_keys SET revoked_at=now() WHERE id=$1`, id)
	if _, err = s.authenticateMCPAccess(ctx, token); err == nil {
		t.Fatal("已撤销密钥仍可认证")
	}
	if !s.callMCPAccessTool(ctx, key, token, spec, json.RawMessage(`{"request_id":"revoked"}`)).IsError {
		t.Fatal("存量会话可用已撤销密钥写入")
	}
	var audit string
	if err = pg.QueryRow(`SELECT COALESCE(json_agg(t)::text,'') FROM mcp_access_calls t WHERE key_id=$1`, id).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, "not-in-audit") || strings.Contains(audit, token) {
		t.Fatal("审计泄露参数或密钥")
	}
}

type mcpTestTransport struct{ token string }

func (t mcpTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPAccessFileBridge(t *testing.T) {
	spec := mcpAccessTool{Method: "POST", Path: "/upload", Upload: true, Handler: func(w http.ResponseWriter, r *http.Request) {
		f, h, e := r.FormFile("file")
		if e != nil {
			t.Fatal(e)
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if h.Filename != "test.txt" || string(b) != "hello" {
			t.Fatal("上传数据错误")
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}}
	if _, _, err := invokeMCPAccessTool(t.Context(), spec, json.RawMessage(`{"filename":"test.txt","content_base64":"aGVsbG8="}`)); err != nil {
		t.Fatal(err)
	}
	spec = mcpAccessTool{Method: "GET", Path: "/download", Download: true, Handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }}
	data, _, err := invokeMCPAccessTool(t.Context(), spec, json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(data), "aGVsbG8=") {
		t.Fatalf("下载编码错误: %s %v", data, err)
	}
}

func TestMCPAccessScopeValidation(t *testing.T) {
	for _, scopes := range [][]string{nil, {"admin"}, {"tasks.read", "tasks.read"}} {
		if validateMCPAccessScopes(scopes) == nil {
			t.Fatalf("非法权限被接受: %v", scopes)
		}
	}
	if err := validateMCPAccessScopes([]string{"tasks.read", "workspace.write"}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPAccessBridge(t *testing.T) {
	spec := mcpAccessTool{Name: "test", Method: "POST", Path: "/tasks/{id}", Handler: func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "42" || r.URL.Query().Get("name") != "a&b" {
			t.Error("参数未隔离编码")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["message"] != "hello" {
			t.Error("正文错误")
		}
		writeJSON(w, 202, map[string]any{"accepted": true})
	}}
	args := json.RawMessage(`{"id":"42","query":{"name":"a&b"},"body":{"message":"hello"},"request_id":"r1"}`)
	result, code, err := invokeMCPAccessTool(httptest.NewRequest("POST", "/mcp", nil).Context(), spec, args)
	if err != nil || code != 202 || !strings.Contains(string(result), "accepted") {
		t.Fatalf("%s %d %v", result, code, err)
	}
	for _, raw := range []string{`{"id":"../escape"}`, `{"id":"a/b"}`, `{}`, `{"id":"42","unexpected":1}`} {
		if _, _, err := invokeMCPAccessTool(t.Context(), spec, json.RawMessage(raw)); err == nil {
			t.Fatalf("接受非法参数: %s", raw)
		}
	}
}

func TestMCPAccessWorkerMessageRequestIDBridge(t *testing.T) {
	var got map[string]any
	spec := mcpAccessTool{Method: http.MethodPost, Path: "/tasks/{id}/intents/{iid}/messages", InjectRequestID: true, Handler: func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}}
	args := json.RawMessage(`{"id":"1","iid":"2","request_id":"outer-1","body":{"message":"continue"}}`)
	if _, code, err := invokeMCPAccessTool(t.Context(), spec, args); err != nil || code != http.StatusOK {
		t.Fatalf("bridge failed: code=%d err=%v", code, err)
	}
	if got["request_id"] != "outer-1" || got["message"] != "continue" {
		t.Fatalf("business request did not receive id: %#v", got)
	}
	args = json.RawMessage(`{"id":"1","iid":"2","request_id":"outer-1","body":{"request_id":"different","message":"continue"}}`)
	if _, _, err := invokeMCPAccessTool(t.Context(), spec, args); err == nil {
		t.Fatal("nested conflicting request_id accepted")
	}
}

func TestMCPAccessFindingsFilterBridge(t *testing.T) {
	var got url.Values
	spec := mcpAccessTool{Method: http.MethodGet, Path: "/discovery/findings", EnsureFindingsPagination: true, Handler: func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		writeJSON(w, http.StatusOK, []any{})
	}}
	if _, _, err := invokeMCPAccessTool(t.Context(), spec, json.RawMessage(`{"query":{"task_id":"42"}}`)); err != nil {
		t.Fatal(err)
	}
	if got.Get("task_id") != "42" || got.Get("limit") == "" {
		t.Fatalf("task-scoped query did not enter filtered pagination path: %v", got)
	}
	if _, _, err := invokeMCPAccessTool(t.Context(), spec, json.RawMessage(`{"query":{"severity":"critical"}}`)); err != nil {
		t.Fatal(err)
	}
	if got.Get("severity") != "critical" || got.Get("limit") != "20" {
		t.Fatalf("severity filter did not enter filtered pagination path: %v", got)
	}
	for _, tool := range (&Server{}).mcpAccessTools() {
		if tool.Name == "artex_discovery_nodes" && !strings.Contains(tool.Description, "task") {
			t.Fatalf("discovery_nodes must document handler's task parameter: %s", tool.Description)
		}
		if tool.Name == "artex_findings_list" && (!strings.Contains(tool.Description, "page") || !strings.Contains(tool.Description, "limit")) {
			t.Fatalf("findings_list must document supported pagination fields: %s", tool.Description)
		}
	}
}

func TestMCPAccessTestLLMRejectsCallerEndpoints(t *testing.T) {
	s := &Server{}
	for _, body := range []string{
		`{}`,
		`{"profile_id":null}`,
		`{"profile_id":0}`,
		`{"provider":"anthropic","model":"test","base_url":"https://attacker.invalid","streaming":false}`,
		`{"profile_id":1,"base_url":"https://attacker.invalid"}`,
		`{"api_key":"caller-key"}`,
	} {
		w := httptest.NewRecorder()
		s.mcpAccessTestLLM(w, httptest.NewRequest(http.MethodPost, "/llm/test", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("caller-controlled model endpoint/key accepted: status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

func TestMCPAccessGetLLMMasksProxyCredentials(t *testing.T) {
	s := &Server{}
	proxy := "http://agent:fixture-pass-123@proxy.example:8080"
	s.llmCfg.Proxy = proxy
	w := httptest.NewRecorder()
	s.mcpAccessGetLLM(w, httptest.NewRequest(http.MethodGet, "/llm", nil))
	if strings.Contains(w.Body.String(), "fixture-pass-123") || !strings.Contains(w.Body.String(), maskMCPProxyURL(proxy)) {
		t.Fatalf("proxy credentials were not masked: %s", w.Body.String())
	}
	s.llmCfg.Proxy = "not a URL {{PASSWORD_wz73tf4k}}"
	w = httptest.NewRecorder()
	s.mcpAccessGetLLM(w, httptest.NewRequest(http.MethodGet, "/llm", nil))
	if strings.Contains(w.Body.String(), "{{PASSWORD_wz73tf4k}}") || !strings.Contains(w.Body.String(), "(已配置)") {
		t.Fatalf("invalid proxy configuration leaked: %s", w.Body.String())
	}
}

func TestMCPAccessBusinessFailureClassifier(t *testing.T) {
	for _, tc := range []struct {
		tool mcpAccessTool
		body string
		want bool
	}{
		{mcpAccessTool{ResultSuccessField: "ok"}, `{"ok":false,"error":"failed"}`, true},
		{mcpAccessTool{ResultSuccessField: "ok"}, `{"ok":true}`, false},
		{mcpAccessTool{ResultErrorField: "is_error"}, `{"is_error":true}`, true},
		{mcpAccessTool{}, `{"ok":false}`, false},
	} {
		if got := mcpAccessBusinessFailure(tc.tool, json.RawMessage(tc.body)); got != tc.want {
			t.Fatalf("classifier(%s)=%v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestMCPAccessRegistry(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	scopes := map[string]bool{}
	for _, tool := range s.mcpAccessTools() {
		if seen[tool.Name] || tool.Handler == nil || !localAgentScopes[tool.Scope] {
			t.Fatalf("无效工具: %s", tool.Name)
		}
		seen[tool.Name] = true
		scopes[tool.Scope] = true
	}
	for scope := range localAgentScopes {
		if !scopes[scope] {
			t.Errorf("缺少权限能力: %s", scope)
		}
	}
}

package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestLocalAgentDiscoveryAuthenticationBoundary(t *testing.T) {
	s := &Server{}
	mux := http.NewServeMux()
	s.registerLocalAgentRoutes(mux)
	for _, path := range []string{"/api/local-agent/conversations/agents", "/api/local-agent/discovery/stats?task=1", "/api/local-agent/mcp"} {
		w := httptest.NewRecorder()
		s.requireAuth(mux).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("无签名请求未拒绝 %s: %d", path, w.Code)
		}
	}
}

func TestLocalAgentDiscoveryPG(t *testing.T) {
	dsn := os.Getenv("ARTEX_LOCAL_AGENT_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置隔离 PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	pg.SetMaxOpenConns(1)
	// 临时表只覆盖本连接，避免修改其他回归的任务、归档和 Agent 数据。
	if _, err := pg.Exec(`CREATE TEMP TABLE findings(task_id BIGINT,status TEXT,severity TEXT,vulnclass TEXT,created_at TIMESTAMPTZ DEFAULT now());
		CREATE TEMP TABLE tasks(id BIGINT,name TEXT,description TEXT,archived_at TIMESTAMPTZ);
		CREATE TEMP TABLE task_archives(task_id BIGINT,aggregate_stats JSONB);
		CREATE TEMP TABLE agents(LIKE agents INCLUDING DEFAULTS);
		CREATE TEMP TABLE mcp_servers(LIKE mcp_servers INCLUDING DEFAULTS);
		CREATE TEMP TABLE mcp_tools_cache(LIKE mcp_tools_cache INCLUDING DEFAULTS);
		CREATE TEMP TABLE local_agent_devices (
		id BIGSERIAL PRIMARY KEY,name TEXT NOT NULL,public_key TEXT UNIQUE NOT NULL,status TEXT NOT NULL DEFAULT 'pending',
		scopes TEXT[] NOT NULL DEFAULT '{}',created_at TIMESTAMPTZ NOT NULL DEFAULT now(),approved_at TIMESTAMPTZ,last_used_at TIMESTAMPTZ,
		skill_version TEXT NOT NULL DEFAULT '',skill_reported_at TIMESTAMPTZ);
		CREATE TEMP TABLE local_agent_nonces(public_key TEXT,nonce TEXT,created_at TIMESTAMPTZ DEFAULT now(),PRIMARY KEY(public_key,nonce));
		INSERT INTO tasks VALUES(1,'任务一','测试',NULL),(2,'任务二','测试',NULL),(3,'冷任务','归档',now()),(4,'其他冷任务','归档',now());
		INSERT INTO findings(task_id,status,severity,vulnclass) VALUES(1,'pending','high','sqli'),(2,'confirmed','critical','xss');
		INSERT INTO task_archives VALUES(3,'{"finding_stats":{"total":2,"high":2,"vulnclasses":["cold-one"]}}'),
		(4,'{"finding_stats":{"total":3,"low":3,"vulnclasses":["cold-other"]}}');
		INSERT INTO agents(key,name,role,description,wrapup_prompt) VALUES('audit_agent','审计 Agent','assistant','目录说明','private-prompt-sentinel');
		INSERT INTO mcp_servers(id,name,transport,url,command,args,env) VALUES(90001,'审计 MCP','http','https://private-url-sentinel@example.com/','private-command-sentinel','["private-args-sentinel"]','{"TOKEN":"private-env-sentinel"}');`); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var deviceID int64
	if err := pg.QueryRow(`INSERT INTO local_agent_devices(name,public_key,status) VALUES('审计设备',$1,'approved') RETURNING id`, base64.StdEncoding.EncodeToString(public)).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	mux := http.NewServeMux()
	s.registerLocalAgentRoutes(mux)
	handler := s.requireAuth(mux)
	call := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := signedLocalAgentRequest(t, private, http.MethodGet, path, nil)
		r.Header.Set("X-ARTEX-Device-ID", strconv.FormatInt(deviceID, 10))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, item := range []struct{ path, scope string }{
		{"/api/local-agent/discovery/stats?task=1", "discovery.read"},
		{"/api/local-agent/conversations/agents", "conversations.read"},
		{"/api/local-agent/mcp", "mcp.read"},
	} {
		if w := call(item.path); w.Code != 403 || !strings.Contains(w.Body.String(), item.scope) {
			t.Fatalf("缺权限未拒绝: %d %s", w.Code, w.Body.String())
		}
	}
	if _, err := pg.Exec(`UPDATE local_agent_devices SET scopes=ARRAY['discovery.read','conversations.read','mcp.read'] WHERE id=$1`, deviceID); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		query                               string
		total, high, critical, low, pending int
		classes                             string
	}{
		{"?task=1", 1, 1, 0, 0, 1, "sqli"},
		{"?task=2", 1, 0, 1, 0, 0, "xss"},
		{"?task=3", 2, 2, 0, 0, 0, "cold-one"},
		{"?task=999", 0, 0, 0, 0, 0, ""},
		{"", 7, 3, 1, 3, 1, "cold-one,cold-other,sqli,xss"},
	} {
		w := call("/api/local-agent/discovery/stats" + item.query)
		var stats db.FindingStats
		if err := json.Unmarshal(w.Body.Bytes(), &stats); w.Code != 200 || err != nil {
			t.Fatalf("统计查询失败: %d %s", w.Code, w.Body.String())
		}
		if stats.Total != item.total || stats.High != item.high || stats.Critical != item.critical || stats.Low != item.low || stats.Pending != item.pending || strings.Join(stats.VulnClasses, ",") != item.classes {
			t.Fatalf("范围 %q 混入其他任务: %+v", item.query, stats)
		}
		if item.query == "?task=1" && (len(stats.Tasks) != 1 || stats.Tasks[0].ID != 1) {
			t.Fatalf("任务目录没有过滤: %+v", stats.Tasks)
		}
	}
	for _, invalid := range []string{"0", "-1", "abc", "9223372036854775808"} {
		if w := call("/api/local-agent/discovery/stats?task=" + invalid); w.Code != 400 {
			t.Fatalf("非法任务 %s: %d", invalid, w.Code)
		}
	}
	w := call("/api/local-agent/conversations/agents")
	var catalog struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); w.Code != 200 || err != nil || len(catalog.Agents) != 1 {
		t.Fatalf("Agent 目录失败: %d %s", w.Code, w.Body.String())
	}
	if len(catalog.Agents[0]) != 3 || catalog.Agents[0]["key"] != "audit_agent" || strings.Contains(w.Body.String(), "private-prompt-sentinel") {
		t.Fatalf("Agent 目录暴露额外配置: %s", w.Body.String())
	}
	w = call("/api/local-agent/mcp")
	var servers struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &servers); w.Code != 200 || err != nil || len(servers.Servers) != 1 {
		t.Fatalf("MCP 目录失败: %d %s", w.Code, w.Body.String())
	}
	if len(servers.Servers[0]) != 6 || servers.Servers[0]["name"] != "审计 MCP" || strings.Contains(w.Body.String(), "private-") {
		t.Fatalf("MCP 目录暴露凭据配置: %s", w.Body.String())
	}
}

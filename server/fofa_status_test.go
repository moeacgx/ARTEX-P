package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func fofaStatusTestServer(t *testing.T) *Server {
	t.Helper()
	s := builtinSkillTestServer(t)
	if _, err := s.m.pg.Exec(`CREATE TABLE tools (
 key text PRIMARY KEY, system boolean NOT NULL DEFAULT true, description text NOT NULL DEFAULT '',
 schema jsonb NOT NULL DEFAULT '{}', agents jsonb NOT NULL DEFAULT '[]', enabled boolean NOT NULL DEFAULT true,
 kind text NOT NULL DEFAULT 'builtin', exec jsonb NOT NULL DEFAULT '{}', deferred boolean NOT NULL DEFAULT false
); INSERT INTO tools(key,agents) VALUES ('fofa_search','["auto","worker","pentest"]');`); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigureProxySecrets(filepath.Join(t.TempDir(), "status-proxy.key")); err != nil {
		t.Fatal(err)
	}
	return s
}

func readFOFAStatus(t *testing.T, tool actool.CoreTool, agentKey string) map[string]any {
	t.Helper()
	ctx := context.WithValue(t.Context(), fofaAgentContextKey{}, agentKey)
	result, err := tool.Call(ctx, json.RawMessage(`{}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("读取状态失败: err=%v result=%s", err, result.Flatten())
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(result.Flatten()), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

type fofaStatusNetworkTrap struct{ calls *atomic.Int32 }

func (t fofaStatusNetworkTrap) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, errors.New("状态工具禁止联网")
}

func TestFOFAStatusUsesLiveConfigurationAndBindingWithoutNetwork(t *testing.T) {
	s := fofaStatusTestServer(t)
	var networkCalls atomic.Int32
	previous := http.DefaultTransport
	http.DefaultTransport = fofaStatusNetworkTrap{calls: &networkCalls}
	t.Cleanup(func() { http.DefaultTransport = previous })
	tool := s.fofaStatusTool()
	status := readFOFAStatus(t, tool, "auto")
	if status["integration_enabled"] != false || status["api_key_set"] != false || status["search_tool_enabled"] != true || status["agent_bound"] != true || status["search_tool_available"] != false || status["reason"] != "integration_disabled" {
		t.Fatalf("未配置的实时状态错误: %+v", status)
	}
	key, enabled := "status-key-must-never-echo", true
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	stored, _, err := s.m.pg.GetSetting("integration.fofa.v1")
	if err != nil {
		t.Fatal(err)
	}
	status = readFOFAStatus(t, tool, "auto")
	encoded, _ := json.Marshal(status)
	if status["integration_enabled"] != true || status["api_key_set"] != true || status["search_tool_available"] != true || status["reason"] != "available" || strings.Contains(string(encoded), key) || strings.Contains(string(encoded), "cipher") {
		t.Fatalf("缓存工具未读取新配置或泄漏秘密: %s", encoded)
	}
	message, _ := status["message"].(string)
	if !strings.Contains(message, "未验证") || !strings.Contains(message, "鉴权") || !strings.Contains(message, "额度") {
		t.Fatalf("本地状态不能暗示上游已验证: %q", message)
	}
	if after, _, err := s.m.pg.GetSetting("integration.fofa.v1"); err != nil || after != stored {
		t.Fatal("状态读取修改了持久配置")
	}
	if _, err := s.m.pg.Exec(`UPDATE tools SET enabled=false WHERE key='fofa_search'`); err != nil {
		t.Fatal(err)
	}
	status = readFOFAStatus(t, tool, "auto")
	if status["search_tool_enabled"] != false || status["search_tool_available"] != false || status["reason"] != "search_tool_disabled" {
		t.Fatalf("缓存工具未看到停用: %+v", status)
	}
	if _, err := s.m.pg.Exec(`UPDATE tools SET enabled=true,agents='["worker"]' WHERE key='fofa_search'`); err != nil {
		t.Fatal(err)
	}
	status = readFOFAStatus(t, tool, "auto")
	if status["agent_bound"] != false || status["search_tool_available"] != false || status["reason"] != "agent_not_bound" {
		t.Fatalf("缓存工具未看到解除绑定: %+v", status)
	}
	if status := readFOFAStatus(t, tool, "worker"); status["agent_bound"] != true || status["search_tool_available"] != true {
		t.Fatalf("状态未使用可信调用者身份: %+v", status)
	}
	if networkCalls.Load() != 0 {
		t.Fatalf("状态查询执行了 %d 次网络请求", networkCalls.Load())
	}
}

func TestFOFAStatusRejectsSpoofedIdentityAndInput(t *testing.T) {
	s := fofaStatusTestServer(t)
	tool := s.fofaStatusTool()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		input string
	}{
		{"缺少可信身份", t.Context(), `{}`},
		{"空身份", context.WithValue(t.Context(), fofaAgentContextKey{}, ""), `{}`},
		{"参数伪造身份", t.Context(), `{"agent_key":"auto"}`},
		{"覆盖身份", context.WithValue(t.Context(), fofaAgentContextKey{}, "auto"), `{"agent_key":"worker"}`},
		{"额外令牌", context.WithValue(t.Context(), fofaAgentContextKey{}, "auto"), `{"api_key":"spoof-secret"}`},
		{"空值", context.WithValue(t.Context(), fofaAgentContextKey{}, "auto"), `null`},
		{"尾随对象", context.WithValue(t.Context(), fofaAgentContextKey{}, "auto"), `{} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Call(tc.ctx, []byte(tc.input), nil)
			if err != nil || !result.IsError || strings.Contains(result.Flatten(), "spoof-secret") {
				t.Fatalf("状态工具未拒绝伪造输入: err=%v result=%s", err, result.Flatten())
			}
		})
	}
}

func TestFOFAStatusDistinguishesMissingAndUntrustedSearchTool(t *testing.T) {
	s := fofaStatusTestServer(t)
	key, enabled := "local-status-key", true
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	tool := s.fofaStatusTool()
	if _, err := s.m.pg.Exec(`UPDATE tools SET system=false WHERE key='fofa_search'`); err != nil {
		t.Fatal(err)
	}
	status := readFOFAStatus(t, tool, "auto")
	if status["search_tool_available"] != false || status["reason"] != "search_tool_untrusted" {
		t.Fatalf("自定义同名工具不能冒充内置搜索: %+v", status)
	}
	if _, err := s.m.pg.Exec(`DELETE FROM tools WHERE key='fofa_search'`); err != nil {
		t.Fatal(err)
	}
	status = readFOFAStatus(t, tool, "auto")
	if status["search_tool_enabled"] != false || status["agent_bound"] != false || status["search_tool_available"] != false || status["reason"] != "search_tool_missing" {
		t.Fatalf("缺少目录条目的状态错误: %+v", status)
	}
}

func TestFOFAStatusReadFailureIsNotReportedAsMissingKey(t *testing.T) {
	for _, table := range []string{"settings", "tools"} {
		t.Run(table, func(t *testing.T) {
			s := fofaStatusTestServer(t)
			if _, err := s.m.pg.Exec(`ALTER TABLE ` + table + ` RENAME TO hidden_status_table`); err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(t.Context(), fofaAgentContextKey{}, "auto")
			result, err := s.fofaStatusTool().Call(ctx, []byte(`{}`), nil)
			if err != nil || !result.IsError || strings.Contains(result.Flatten(), "未配置") || strings.Contains(result.Flatten(), "SQLSTATE") || strings.Contains(result.Flatten(), "hidden_status_table") {
				t.Fatalf("存储失败不能伪造未配置或输出内部错误: err=%v result=%s", err, result.Flatten())
			}
		})
	}
}

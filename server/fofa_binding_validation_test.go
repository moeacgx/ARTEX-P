package server

import (
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

// 使用真实数据库装配装饰器和计量器，防止默认值重编码吞掉重复键或 null。
func TestFOFAAssembledToolRejectsInvalidInputBeforeDefaults(t *testing.T) {
	s := builtinSkillTestServer(t)
	if _, err := s.m.pg.Exec(`
CREATE TABLE tools (
 key text PRIMARY KEY, system boolean NOT NULL DEFAULT true, description text NOT NULL DEFAULT '',
 schema jsonb NOT NULL DEFAULT '{}', agents jsonb NOT NULL DEFAULT '[]', enabled boolean NOT NULL DEFAULT true,
 kind text NOT NULL DEFAULT 'builtin', exec jsonb NOT NULL DEFAULT '{}', deferred boolean NOT NULL DEFAULT false
);
CREATE TABLE tool_usage (
 tool_key text NOT NULL, agent_key text, task_id bigint, exploration_id bigint, intent_id bigint, session_id text
);`); err != nil {
		t.Fatal(err)
	}
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	oldResolve, oldTrafficBinding := agent.ToolResolve, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() {
		agent.ToolResolve = oldResolve
		agent.FindingTrafficBindingEnabled = oldTrafficBinding
	})
	wireTools(s.m.pg, nil)
	resolved := agent.ToolResolve(t.Context(), "worker", []actool.CoreTool{s.fofaTool()})
	if len(resolved) != 1 || resolved[0].Name() != "fofa_search" {
		t.Fatal("真实装配路径未提供已绑定的 FOFA 工具")
	}
	for _, tc := range []struct {
		name, input string
	}{
		{"重复查询", `{"query":"domain=\"first.test\"","query":"domain=\"second.test\""}`},
		{"空页码", `{"query":"domain=\"example.test\"","page":null}`},
		{"空数量", `{"query":"domain=\"example.test\"","size":null}`},
		{"重复页码", `{"query":"domain=\"example.test\"","page":1,"page":2}`},
		{"空字段数组", `{"query":"domain=\"example.test\"","fields":null}`},
		{"字段类型错误", `{"query":"domain=\"example.test\"","fields":[1]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := resolved[0].Call(t.Context(), []byte(tc.input), nil)
			if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "参数无效") {
				t.Fatalf("原始无效输入通过了装配层校验: err=%v result=%s", err, result.Flatten())
			}
			var count int
			if err := s.m.pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE tool_key='fofa_search'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("无效输入不应进入工具执行计量: count=%d err=%v", count, err)
			}
		})
	}
	// 合法缺省参数仍应到达真实处理器，并由未配置状态阻止联网。
	for _, input := range []string{
		`{"query":"domain=\"example.test\""}`,
		`{"query":"domain=\"example.test\"","fields":[]}`,
		`{"query":"body=\"dujiaoka\"","page":1,"size":50,"fields":["host","ip","port","protocol","domain","title","server","country_name"]}`,
	} {
		result, err := resolved[0].Call(t.Context(), []byte(input), nil)
		if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "未启用或未配置") {
			t.Fatalf("合法参数被错误拦截: err=%v result=%s", err, result.Flatten())
		}
	}
}

func TestFOFAStatusAssembledWhenSearchDisabled(t *testing.T) {
	s := newModelExecutionServer(t)
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key IN ('fofa_search','fofa_status')`) })
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	oldResolve, oldTrafficBinding := agent.ToolResolve, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() { agent.ToolResolve, agent.FindingTrafficBindingEnabled = oldResolve, oldTrafficBinding })
	wireTools(s.m.pg, nil)
	resolveFOFA := func(agentKey string) []actool.CoreTool {
		var out []actool.CoreTool
		for _, tool := range agent.ToolResolve(t.Context(), agentKey, s.fofaTools()) {
			if strings.HasPrefix(tool.Name(), "fofa_") {
				out = append(out, tool)
			}
		}
		return out
	}
	for _, agentKey := range []string{"worker", "auto", "pentest", "planner"} {
		resolved := resolveFOFA(agentKey)
		if agentKey == "planner" {
			if len(resolved) != 0 {
				t.Fatal("未绑定 Agent 获得了 FOFA 状态工具")
			}
			continue
		}
		if len(resolved) != 1 || resolved[0].Name() != "fofa_status" {
			t.Fatalf("未配置时仍需提供状态工具: agent=%s tools=%d", agentKey, len(resolved))
		}
		result, err := resolved[0].Call(t.Context(), []byte(`{}`), nil)
		if err != nil || result.IsError || !strings.Contains(result.Flatten(), `"search_tool_available":false`) {
			t.Fatalf("状态工具未使用可信 Agent 身份: %v %s", err, result.Flatten())
		}
		if _, err := s.m.pg.Exec(`UPDATE tools SET enabled=false WHERE key='fofa_status'`); err != nil {
			t.Fatal(err)
		}
		result, _ = resolved[0].Call(t.Context(), []byte(`{}`), nil)
		if !result.IsError {
			t.Fatal("缓存状态工具绕过最新禁用状态")
		}
		if len(resolveFOFA(agentKey)) != 0 {
			t.Fatal("禁用状态工具仍被装配")
		}
		if _, err := s.m.pg.Exec(`UPDATE tools SET enabled=true WHERE key='fofa_status'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.m.pg.Exec(`DELETE FROM tools WHERE key='fofa_status'`); err != nil {
		t.Fatal(err)
	}
	if len(resolveFOFA("auto")) != 0 {
		t.Fatal("目录缺失仍放行状态工具")
	}
	if err := s.m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	if len(resolveFOFA("auto")) != 0 {
		t.Fatal("数据库故障仍放行状态工具")
	}
}

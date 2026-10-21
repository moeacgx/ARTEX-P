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
	result, err := resolved[0].Call(t.Context(), []byte(`{"query":"domain=\"example.test\""}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "未启用或未配置") {
		t.Fatalf("合法缺省参数被错误拦截: err=%v result=%s", err, result.Flatten())
	}
}

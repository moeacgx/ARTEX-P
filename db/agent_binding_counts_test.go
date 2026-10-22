package db

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestAgentBindingCountsToleratesLegacyNonArrays(t *testing.T) {
	d := catalogDBFixture(t)
	a, err := d.CreateAgent("count_assistant", "统计助手", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetAgentSkillVisibility(a.ID, []string{"proof"}); err != nil {
		t.Fatal(err)
	}
	if err = d.SetAgentVisibilityKind(a.ID, "mcp", []int64{12}); err != nil {
		t.Fatal(err)
	}
	// 绑定计数包含停用的工具；空绑定及旧非数组不能影响其他工具、技能和 MCP。
	for i, raw := range []string{`["count_assistant"]`, `[]`, `null`, `"旧标量"`, `{}`, `true`, `42`} {
		if _, err = d.Exec(`INSERT INTO tools(key,agents,enabled) VALUES($1,$2,false)`, fmt.Sprintf("count_tool_%d", i), raw); err != nil {
			t.Fatal(err)
		}
	}
	mcp, skill, tools, err := d.AgentBindingCounts()
	if err != nil {
		t.Fatalf("旧绑定数据不应使全部统计失败: %v", err)
	}
	if mcp[a.ID] != 1 || skill[a.ID] != 1 || tools[a.Key] != 1 || len(tools) != 1 {
		t.Fatalf("绑定统计不正确: mcp=%v skill=%v tools=%v", mcp, skill, tools)
	}
	var legacy int
	if err = d.QueryRow(`SELECT count(*) FROM tools WHERE jsonb_typeof(agents)<>'array'`).Scan(&legacy); err != nil || legacy != 5 {
		t.Fatalf("读取统计不得修改历史数据: count=%d err=%v", legacy, err)
	}
}

func TestToolWritesNormalizeEmptyAgents(t *testing.T) {
	d := catalogDBFixture(t)
	assertEmpty := func(key string) {
		t.Helper()
		var raw string
		if err := d.QueryRow(`SELECT agents::text FROM tools WHERE key=$1`, key).Scan(&raw); err != nil || raw != `[]` {
			t.Fatalf("%s 空绑定应持久化为 []: raw=%s err=%v", key, raw, err)
		}
	}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{{"nil", nil}, {"null", json.RawMessage(` null `)}, {"empty", json.RawMessage(`[]`)}} {
		t.Run(tc.name, func(t *testing.T) {
			key := "empty_" + tc.name
			if err := d.SeedTool(key, "", nil, tc.raw); err != nil {
				t.Fatal(err)
			}
			assertEmpty(key)
			if err := d.UpsertToolForce(key, "", nil, tc.raw); err != nil {
				t.Fatal(err)
			}
			assertEmpty(key)
			if err := d.UpdateTool(key, "", nil, tc.raw, true); err != nil {
				t.Fatal(err)
			}
			assertEmpty(key)
		})
	}
	for _, keys := range [][]string{nil, {}} {
		tool := &Tool{Key: "empty_custom", Kind: "script", Agents: keys}
		if err := d.CreateCustomTool(tool); err != nil {
			t.Fatal(err)
		}
		assertEmpty(tool.Key)
		tool.Agents = []string{"worker"}
		if err := d.UpdateCustomTool(tool); err != nil {
			t.Fatal(err)
		}
		tool.Agents = keys
		if err := d.UpdateCustomTool(tool); err != nil {
			t.Fatal(err)
		}
		assertEmpty(tool.Key)
		if err := d.DeleteCustomTool(tool.Key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentBindingCountsWritesRepairLegacyNull(t *testing.T) {
	d := catalogDBFixture(t)
	if _, err := d.Exec(`INSERT INTO tools(key,agents) VALUES('legacy_null','null')`); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAgentToToolBinding("worker", []string{"legacy_null"}); err != nil {
		t.Fatal(err)
	}
	tool, err := d.GetTool("legacy_null")
	if err != nil || len(tool.Agents) != 1 || tool.Agents[0] != "worker" {
		t.Fatalf("绑定旧 null 工具应修复为数组: %+v %v", tool, err)
	}
	if _, err = d.Exec(`UPDATE tools SET agents='null' WHERE key='legacy_null'`); err != nil {
		t.Fatal(err)
	}
	a := CatalogAgent{Key: "catalog_bound", Name: "证据助手", Prompt: "整理已有证据"}
	payload, _ := json.Marshal(a)
	if err = d.CatalogApply(t.Context(), "agent", a.Key, "absent", payload, []string{"legacy_null"}, nil, "catalog.test.bind", "v1", catalogSourceJSON()); err != nil {
		t.Fatal(err)
	}
	tool, err = d.GetTool("legacy_null")
	if err != nil || len(tool.Agents) != 1 || tool.Agents[0] != a.Key {
		t.Fatalf("目录安装绑定旧 null 工具应修复为数组: %+v %v", tool, err)
	}
}

func TestToolWritesRejectNonArrayAgents(t *testing.T) {
	d := catalogDBFixture(t)
	valid := json.RawMessage(`["worker"]`)
	if err := d.SeedTool("bound", "", nil, valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`"worker"`, `{}`, `true`, `12`, `[1]`, `invalid`} {
		if err := d.SeedTool("invalid", "", nil, json.RawMessage(raw)); err == nil {
			t.Fatalf("SeedTool 不应接受 %s", raw)
		}
		if err := d.UpsertToolForce("bound", "", nil, json.RawMessage(raw)); err == nil {
			t.Fatalf("UpsertToolForce 不应接受 %s", raw)
		}
		if err := d.UpdateTool("bound", "", nil, json.RawMessage(raw), true); err == nil {
			t.Fatalf("UpdateTool 不应接受 %s", raw)
		}
	}
	bound, err := d.GetTool("bound")
	if err != nil || len(bound.Agents) != 1 || bound.Agents[0] != "worker" {
		t.Fatalf("拒绝无效输入时必须保留原绑定: %+v %v", bound, err)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentListBindingCountsWithLegacyNull(t *testing.T) {
	s := catalogTestServer(t)
	a, err := s.m.pg.CreateAgent("evidence_assistant", "证据助手", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.m.pg.SetAgentSkillVisibility(a.ID, []string{"evidence-summary"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.m.pg.Exec(`INSERT INTO tools(key,agents,enabled) VALUES('clock','["evidence_assistant"]',false),('legacy','null',false)`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.pgListAgents(w, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("列表应能读取旧 null 数据: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Agents []AgentDTO `json:"agents"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Agents) != 1 || result.Agents[0].SkillCount != 1 || result.Agents[0].ToolCount != 1 {
		t.Fatalf("应返回具体绑定数，不能静默归零: %+v", result.Agents)
	}
}

func TestAgentListBindingCountFailureIsVisible(t *testing.T) {
	s := catalogTestServer(t)
	if _, err := s.m.pg.CreateAgent("evidence_assistant", "证据助手", ""); err != nil {
		t.Fatal(err)
	}
	// 独立 schema 内模拟统计查询故障；Agent 基本列表仍可读取。
	if _, err := s.m.pg.Exec(`DROP TABLE agent_visibility`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.pgListAgents(w, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "读取 Agent 绑定统计失败") {
		t.Fatalf("统计故障应返回受控错误: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "agent_visibility") || strings.Contains(w.Body.String(), `"agents"`) {
		t.Fatal("错误响应不应暴露数据库细节或返回伪零列表")
	}
}

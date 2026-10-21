package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// 受控模型只调用状态工具；真实 Chat 负责工具装配、权限包装、调用和结果回传。
type fofaChatProvider struct {
	callID       string
	calls        int
	wantTools    []string
	wantStatus   map[string]any
	statusRead   bool
	secret       string
	beforeStatus func() error
}

func (p *fofaChatProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{}, fmt.Errorf("FOFA Chat 回归只允许受控非流式模型"))
	}
}

func (p *fofaChatProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	fail := func(format string, args ...any) (llm.Message, string, llm.Usage, error) {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf(format, args...)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return fail("序列化受控模型请求失败: %v", err)
	}
	if strings.Contains(string(raw), p.secret) {
		return fail("FOFA 明文 Key 进入模型请求")
	}
	var names []string
	for _, item := range req.Tools {
		if strings.HasPrefix(item.Name, "fofa_") {
			names = append(names, item.Name)
		}
	}
	sort.Strings(names)
	if !slices.Equal(names, p.wantTools) {
		return fail("当前轮 FOFA 工具未按最新配置/绑定装配: got=%v want=%v", names, p.wantTools)
	}
	if p.calls == 1 && p.wantStatus != nil {
		if p.beforeStatus != nil {
			if err := p.beforeStatus(); err != nil {
				return fail("调用前修改本地配置失败: %v", err)
			}
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
			Type: llm.BlockToolUse, ID: p.callID, Name: "fofa_status", Input: json.RawMessage(`{}`),
		}}}, "tool_use", llm.Usage{}, nil
	}
	if p.wantStatus != nil {
		var result *llm.ContentBlock
		for _, message := range req.Messages {
			for _, block := range message.Content {
				if block.Type == llm.BlockToolResult && block.ToolUseID == p.callID {
					copy := block
					result = &copy
				}
			}
		}
		if result == nil || result.IsError {
			return fail("真实状态工具未向模型返回成功结果")
		}
		text := (llm.Message{Content: result.Content}).Text()
		var status map[string]any
		if err := json.Unmarshal([]byte(text), &status); err != nil {
			return fail("状态工具结果不是结构化 JSON: %v", err)
		}
		for key, want := range p.wantStatus {
			if status[key] != want {
				return fail("状态工具未反映实时配置或当前 Agent 身份: field=%s got=%v want=%v", key, status[key], want)
			}
		}
		if _, exposed := status["api_key"]; exposed {
			return fail("状态工具不应返回 api_key 字段")
		}
		p.statusRead = true
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("FOFA_CHAT_LOCAL_OK")}}, "end_turn", llm.Usage{}, nil
}

func TestFOFAChatReassemblesCurrentConfigAndBindings(t *testing.T) {
	s := newModelExecutionServer(t)
	// 此测试通过 ARTEX_PG_DSN 指向隔离库，恢复目录行以便同套回归顺序执行。
	for _, key := range []string{"fofa_search", "fofa_status"} {
		original, err := s.m.pg.GetTool(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if original == nil {
				_, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key=$1`, key)
				return
			}
			bindings, _ := json.Marshal(original.Agents)
			_ = s.m.pg.UpdateTool(key, original.Description, original.Schema, bindings, original.Enabled)
		})
	}
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`) })
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	oldAugment, oldResolve := agent.ToolAugment, agent.ToolResolve
	oldPrompt, oldTraffic := agent.PromptOverride, agent.FindingTrafficBindingEnabled
	agent.PromptOverride = nil
	t.Cleanup(func() {
		agent.ToolAugment, agent.ToolResolve = oldAugment, oldResolve
		agent.PromptOverride, agent.FindingTrafficBindingEnabled = oldPrompt, oldTraffic
	})
	wireTools(s.m.pg, nil)
	wireAgentAugment(s.m.pg, t.TempDir(), s.hostTools)

	key := "synthetic-fofa-chat-key-must-not-reach-model"
	off, on := false, true
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	provider := &fofaChatProvider{secret: key}
	root := t.TempDir()
	sessionID := fmt.Sprintf("fofa-chat-fixture-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM tool_usage WHERE session_id=$1`, sessionID) })
	chat := agent.NewChatAgent(provider, "local-fofa-chat", root, transcript.NewStore(filepath.Join(root, "transcripts")), 100000)
	chat.SetNonStreaming(func() bool { return true })
	turn := 0
	run := func(agentKey string, wantTools []string, status map[string]any, beforeStatus func() error) {
		t.Helper()
		turn++
		provider.callID = fmt.Sprintf("fofa-status-turn-%d", turn)
		provider.calls, provider.statusRead = 0, false
		provider.wantTools, provider.wantStatus = wantTools, status
		provider.beforeStatus = beforeStatus
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		answer, err := chat.Chat(ctx, agentKey, sessionID, fmt.Sprintf("核对第 %d 轮 FOFA 本地状态", turn), 4, 5*time.Second, false, nil)
		if err != nil || answer != "FOFA_CHAT_LOCAL_OK" {
			t.Fatalf("第%d轮真实 Chat 失败: answer=%q err=%v", turn, answer, err)
		}
		wantCalls := 1
		if status != nil {
			wantCalls = 2
			if !provider.statusRead {
				t.Fatal("模型没有读取真实状态工具结果")
			}
		}
		if provider.calls != wantCalls {
			t.Fatalf("模型调用次数异常: got=%d want=%d", provider.calls, wantCalls)
		}
	}
	status := func(enabled, bound, available bool, reason string) map[string]any {
		return map[string]any{"integration_enabled": enabled, "api_key_set": true, "agent_bound": bound, "search_tool_available": available, "reason": reason}
	}
	statusOnly := []string{"fofa_status"}
	both := []string{"fofa_search", "fofa_status"}
	run("auto", statusOnly, status(false, true, false, "integration_disabled"), nil)

	// 同一 ChatAgent 和同一会话保留历史；新轮次必须动态增加搜索工具。
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	run("auto", both, status(true, true, true, "available"), nil)
	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	run("auto", statusOnly, status(false, true, false, "integration_disabled"), nil)

	if _, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.RemoveAgentFromTool("auto", "fofa_search"); err != nil {
		t.Fatal(err)
	}
	run("auto", statusOnly, status(true, false, false, "agent_not_bound"), nil)
	if err := s.m.pg.RemoveAgentFromTool("auto", "fofa_status"); err != nil {
		t.Fatal(err)
	}
	run("auto", nil, nil, nil)
	// planner 从未绑定 FOFA，历史中的工具调用也不能恢复权限。
	run("planner", nil, nil, nil)

	// 配置在本轮模型返回 tool_use 前变更时，status 必须读取调用时的新值。
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"fofa_search", "fofa_status"}); err != nil {
		t.Fatal(err)
	}
	run("auto", both, status(false, true, false, "integration_disabled"), func() error {
		_, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{Enabled: &off})
		return err
	})
	var searches int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE tool_key='fofa_search' AND session_id=$1`, sessionID).Scan(&searches); err != nil || searches != 0 {
		t.Fatalf("状态核查不应执行资产搜索: calls=%d err=%v", searches, err)
	}
}

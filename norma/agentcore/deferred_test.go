package agentcore

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func deferredEcho() tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:        "echo",
		Description: "Echoes msg.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
			"required":   []any{"msg"},
		},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			var a struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(in, &a)
			return tool.Text("echo:" + a.Msg), nil
		},
	})
}

// TestDeferredToolsWithheldButCallable proves the deferred mechanism end-to-end:
// the deferred tool's schema is NOT sent to the model, the two access tools ARE,
// and the model can still invoke the deferred tool via ExecuteExtraTool.
func TestDeferredToolsWithheldButCallable(t *testing.T) {
	turns := [][]llm.StreamEvent{
		toolTurn("e1", tool.ExecuteExtraToolName, `{"tool_name":"echo","params":{"msg":"hi"}}`),
		textTurn("done"),
	}
	var sentSchemas []llm.ToolSchema
	var n int
	callModel := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		if n == 0 {
			sentSchemas = req.Tools // capture what the model was offered on turn 1
		}
		evs := turns[n]
		n++
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, e := range evs {
				if !yield(e, nil) {
					return
				}
			}
		}
	}

	sess := NewSession(Options{
		SystemPrompt:   []string{"sys"},
		Tools:          []tool.CoreTool{deferredEcho()},
		DeferredTools:  []string{"echo"},
		PermissionMode: permission.ModeBypass,
		Deps:           harness.QueryDeps{CallModel: callModel},
	})

	var results []llm.ContentBlock
	for ev, err := range sess.Prompt(context.Background(), "echo hi") {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ev.Kind == harness.KindToolResult {
			results = append(results, *ev.ToolResult)
		}
	}

	// Schema list: echo withheld; the two access tools present.
	has := func(name string) bool {
		for _, s := range sentSchemas {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	if has("echo") {
		t.Fatal("deferred tool 'echo' schema should be withheld from the model")
	}
	if !has(tool.SearchExtraToolsName) || !has(tool.ExecuteExtraToolName) {
		t.Fatalf("access tools missing from schema list: %+v", sentSchemas)
	}

	// The deferred tool still ran via ExecuteExtraTool.
	if len(results) == 0 || results[0].IsError || results[0].Content[0].Text != "echo:hi" {
		t.Fatalf("ExecuteExtraTool should have run echo, got %+v", results)
	}
}

// TestDeferredLockedToolRejected proves unlock-set gating: a deferred tool not in
// the unlock set is refused by ExecuteExtraTool.
func TestDeferredLockedToolRejected(t *testing.T) {
	turns := [][]llm.StreamEvent{
		toolTurn("e1", tool.ExecuteExtraToolName, `{"tool_name":"echo","params":{"msg":"hi"}}`),
		textTurn("done"),
	}
	var n int
	callModel := func(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		evs := turns[n]
		n++
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, e := range evs {
				if !yield(e, nil) {
					return
				}
			}
		}
	}

	sess := NewSession(Options{
		SystemPrompt:   []string{"sys"},
		Tools:          []tool.CoreTool{deferredEcho()},
		DeferredTools:  []string{"echo"},
		UnlockSet:      tool.NewUnlockSet(), // echo deferred but LOCKED
		PermissionMode: permission.ModeBypass,
		Deps:           harness.QueryDeps{CallModel: callModel},
	})

	var results []llm.ContentBlock
	for ev, err := range sess.Prompt(context.Background(), "echo hi") {
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ev.Kind == harness.KindToolResult {
			results = append(results, *ev.ToolResult)
		}
	}
	if len(results) == 0 || !results[0].IsError {
		t.Fatalf("locked deferred tool should be rejected, got %+v", results)
	}
}

// 原生工具与延迟工具混用时，检索能说明直接入口，但不会把原生工具加入解锁集合。
func TestNativeToolDiscoveryKeepsDirectSchemaAndExecutionBoundary(t *testing.T) {
	var calls int
	native := tool.Build(tool.Spec{
		Name: "fofa_search", Description: "Search FOFA assets",
		Schema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			calls++
			return tool.Text("native-search-ok"), nil
		},
	})
	turns := [][]llm.StreamEvent{
		toolTurn("s1", tool.SearchExtraToolsName, `{"query":"fofa"}`),
		toolTurn("e1", tool.ExecuteExtraToolName, `{"tool_name":"fofa_search","params":{"query":"test"}}`),
		toolTurn("f1", "fofa_search", `{"query":"test"}`),
		textTurn("done"),
	}
	var offered []llm.ToolSchema
	var turn int
	callModel := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		if turn == 0 {
			offered = req.Tools
		}
		events := turns[turn]
		turn++
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, event := range events {
				if !yield(event, nil) {
					return
				}
			}
		}
	}
	unlock := tool.NewUnlockSet("echo")
	session := NewSession(Options{
		SystemPrompt: []string{"sys"}, Tools: []tool.CoreTool{native, deferredEcho()},
		DeferredTools: []string{"echo"}, UnlockSet: unlock, PermissionMode: permission.ModeBypass,
		Deps: harness.QueryDeps{CallModel: callModel},
	})
	var results []llm.ContentBlock
	for event, err := range session.Prompt(context.Background(), "search assets") {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == harness.KindToolResult {
			results = append(results, *event.ToolResult)
		}
	}
	names := map[string]bool{}
	for _, schema := range offered {
		names[schema.Name] = true
	}
	if !names["fofa_search"] || names["echo"] || !names[tool.SearchExtraToolsName] || !names[tool.ExecuteExtraToolName] {
		t.Fatalf("模型可见工具的直接/延迟边界错误: %+v", names)
	}
	if len(results) != 3 || results[0].IsError || !strings.Contains(results[0].Content[0].Text, "## fofa_search") || !strings.Contains(results[0].Content[0].Text, "直接调用") {
		t.Fatalf("原生工具检索未给出直接入口: %+v", results)
	}
	if !results[1].IsError || !strings.Contains(results[1].Content[0].Text, "not unlocked") || results[2].IsError || results[2].Content[0].Text != "native-search-ok" || calls != 1 || unlock.Has("fofa_search") {
		t.Fatalf("原生工具应仅经直接入口执行一次: calls=%d results=%+v", calls, results)
	}
}

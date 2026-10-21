package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// echoTool is a trivial deferred tool that echoes its "msg" param.
func echoTool() CoreTool {
	return Build(Spec{
		Name:        "echo",
		Description: "Echoes the msg parameter back.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"msg": map[string]any{"type": "string"}},
			"required":   []any{"msg"},
		},
		Run: func(_ context.Context, in json.RawMessage, _ *ToolContext) (Result, error) {
			var a struct {
				Msg string `json:"msg"`
			}
			_ = json.Unmarshal(in, &a)
			return Text("echo:" + a.Msg), nil
		},
	})
}

func TestUnlockSet(t *testing.T) {
	s := NewUnlockSet("a", "b")
	if !s.Has("a") || !s.Has("b") || s.Has("c") {
		t.Fatalf("seed wrong: %v", s.List())
	}
	s.Add("c")
	if !s.Has("c") {
		t.Fatal("Add failed")
	}
	if got := s.List(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("List not sorted/complete: %v", got)
	}
}

func TestRenderDeferredToolsBlock(t *testing.T) {
	if RenderDeferredToolsBlock(nil) != "" {
		t.Fatal("empty names should render empty")
	}
	out := RenderDeferredToolsBlock([]string{"mcp__b__y", "mcp__a__x"})
	if !strings.Contains(out, "<available-deferred-tools>") ||
		!strings.Contains(out, "mcp__a__x") || !strings.Contains(out, "mcp__b__y") ||
		!strings.Contains(out, ExecuteExtraToolName) {
		t.Fatalf("block missing parts:\n%s", out)
	}
	// sorted: a before b
	if strings.Index(out, "mcp__a__x") > strings.Index(out, "mcp__b__y") {
		t.Fatal("names not sorted")
	}
}

func TestSearchExtraTools(t *testing.T) {
	reg := NewRegistry(echoTool())
	search := NewSearchExtraTools(reg, []string{"echo"})

	// exact select
	r, _ := search.Call(context.Background(), json.RawMessage(`{"query":"select:echo"}`), nil)
	txt := r.Content[0].Text
	if !strings.Contains(txt, "## echo") || !strings.Contains(txt, "params schema") {
		t.Fatalf("select:echo missing schema:\n%s", txt)
	}
	// keyword
	r, _ = search.Call(context.Background(), json.RawMessage(`{"query":"echoes"}`), nil)
	if !strings.Contains(r.Content[0].Text, "## echo") {
		t.Fatalf("keyword search failed:\n%s", r.Content[0].Text)
	}
	// 非延迟工具应说明可以直接调用，不能错误地要求通过延迟工具入口执行。
	reg2 := NewRegistry(echoTool())
	search2 := NewSearchExtraTools(reg2, []string{"other"})
	r, _ = search2.Call(context.Background(), json.RawMessage(`{"query":"select:echo"}`), nil)
	if !strings.Contains(r.Content[0].Text, "## echo") || !strings.Contains(r.Content[0].Text, "直接调用") || !strings.Contains(r.Content[0].Text, "不要通过 "+ExecuteExtraToolName) {
		t.Fatalf("原生工具应返回 schema 与直接调用说明:\n%s", r.Content[0].Text)
	}
}

type namedDiscoveryTool struct {
	CoreTool
	name, description string
}

func (t namedDiscoveryTool) Name() string        { return t.name }
func (t namedDiscoveryTool) Description() string { return t.description }

func TestSearchExtraToolsFindsNativeBySelectAndKeywords(t *testing.T) {
	native := namedDiscoveryTool{CoreTool: echoTool(), name: "fofa_search", description: "Search FOFA public assets"}
	reg := NewRegistry(native, echoTool())
	search := NewSearchExtraTools(reg, []string{"echo"})
	for _, query := range []string{"select:fofa_search", "select:FOFA_SEARCH", "select:fofa", "fofa", "FOFA assets"} {
		t.Run(query, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{"query": query})
			result, err := search.Call(context.Background(), input, nil)
			text := result.Flatten()
			if err != nil || result.IsError || !strings.Contains(text, "## fofa_search") || !strings.Contains(text, `"required":["msg"]`) || !strings.Contains(text, "直接调用") || !strings.Contains(text, "不要通过 "+ExecuteExtraToolName) {
				t.Fatalf("原生工具检索错误: err=%v result=%s", err, text)
			}
		})
	}
	// 多词查询仍要求所有词命中，不因搜索原生工具而改变原有匹配规则。
	result, _ := search.Call(context.Background(), json.RawMessage(`{"query":"fofa unavailable"}`), nil)
	if strings.Contains(result.Flatten(), "## fofa_search") {
		t.Fatal("多词查询错误地采用了任意词命中")
	}
}

func TestSearchExtraToolsPrefersDeferredAndDoesNotUnlockNative(t *testing.T) {
	native := namedDiscoveryTool{CoreTool: echoTool(), name: "fofa_search", description: "FOFA assets"}
	deferred := namedDiscoveryTool{CoreTool: echoTool(), name: "mcp__assets__fofa", description: "FOFA assets"}
	reg := NewRegistry(native, deferred)
	search := NewSearchExtraTools(reg, []string{deferred.Name()})
	for _, query := range []string{"fofa", "select:fofa_search,mcp__assets__fofa"} {
		input, _ := json.Marshal(map[string]string{"query": query})
		result, _ := search.Call(context.Background(), input, nil)
		if !strings.Contains(result.Flatten(), "## mcp__assets__fofa") || strings.Contains(result.Flatten(), "## fofa_search") {
			t.Fatalf("存在延迟工具匹配时应保持优先: %s", result.Flatten())
		}
	}
	unlock := NewUnlockSet(deferred.Name())
	result, _ := search.Call(context.Background(), json.RawMessage(`{"query":"select:fofa_search"}`), nil)
	if !strings.Contains(result.Flatten(), "## fofa_search") {
		t.Fatalf("精确原生工具查询失败: %s", result.Flatten())
	}
	result, _ = NewExecuteExtraTool(reg, unlock).Call(context.Background(), json.RawMessage(`{"tool_name":"fofa_search","params":{"msg":"test"}}`), nil)
	if !result.IsError || !strings.Contains(result.Flatten(), "not unlocked") || unlock.Has(native.Name()) {
		t.Fatalf("搜索原生工具不应扩大延迟工具执行权限: %s", result.Flatten())
	}
}

func TestSearchExtraToolsMissingDoesNotInferCredentialState(t *testing.T) {
	search := NewSearchExtraTools(NewRegistry(echoTool()), []string{"echo"})
	result, err := search.Call(context.Background(), json.RawMessage(`{"query":"fofa"}`), nil)
	text := result.Flatten()
	if err != nil || result.IsError || !strings.Contains(text, "当前工具名单") || !strings.Contains(text, "不能据此推断 API Key") {
		t.Fatalf("未匹配结果需要准确说明证据边界: err=%v result=%s", err, text)
	}
}

func TestExecuteExtraTool(t *testing.T) {
	reg := NewRegistry(echoTool())
	unlock := NewUnlockSet("echo")
	exec := NewExecuteExtraTool(reg, unlock)

	// unlocked → runs
	r, _ := exec.Call(context.Background(), json.RawMessage(`{"tool_name":"echo","params":{"msg":"hi"}}`), nil)
	if r.IsError || r.Content[0].Text != "echo:hi" {
		t.Fatalf("expected echo:hi, got %+v", r)
	}

	// locked → rejected
	locked := NewExecuteExtraTool(reg, NewUnlockSet())
	r, _ = locked.Call(context.Background(), json.RawMessage(`{"tool_name":"echo","params":{"msg":"hi"}}`), nil)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "not unlocked") {
		t.Fatalf("locked tool should be rejected, got %+v", r)
	}

	// unknown tool
	r, _ = exec.Call(context.Background(), json.RawMessage(`{"tool_name":"nope","params":{}}`), nil)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "unknown tool") {
		t.Fatalf("unknown tool should error, got %+v", r)
	}

	// invalid params (missing required msg) → schema validation error
	r, _ = exec.Call(context.Background(), json.RawMessage(`{"tool_name":"echo","params":{}}`), nil)
	if !r.IsError || !strings.Contains(r.Content[0].Text, "invalid params") {
		t.Fatalf("invalid params should error, got %+v", r)
	}
}

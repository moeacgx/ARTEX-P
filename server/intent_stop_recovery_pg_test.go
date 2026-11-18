package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
)

func intentStopPGManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatal(err)
		}
		t.Skipf("未配置测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// 在 profile 之后登记清理，按 LIFO 保证任务 → profile → Manager.Close。
func cleanupIntentStopTask(t *testing.T, m *Manager, task *Task) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := m.DeleteTask(task.ID, DeleteTaskOptions{DeleteFiles: true}); err != nil {
			t.Errorf("清理测试任务 %s: %v", task.ID, err)
		}
	})
}

// 链不可用(无额度证据)的阻塞意图必须随真实的模型链保存/重置重开；
// 认证失败不重开；终态任务不重开。
func TestReplaceTaskLLMProfilesReopensChainUnavailablePG(t *testing.T) {
	m := intentStopPGManager(t)
	profile, err := m.pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("intent-stop-recovery-%d", time.Now().UnixNano()), Format: "openai", Model: "test-model", APIKey: "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := m.pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, profile); err != nil {
			t.Errorf("清理测试 profile: %v", err)
		}
	})

	newTask := func(name string) (*Task, int64) {
		task, err := m.CreateTaskWithOptions(name, "intent stop recovery", db.TaskCreateOptions{LLMProfileIDs: []int64{profile}})
		if err != nil {
			t.Fatal(err)
		}
		cleanupIntentStopTask(t, m, task)
		n, _ := parseTaskID(task.ID)
		if _, err := m.pg.MarkTaskLLMProfileQuotaExhausted(n, profile, "insufficient_quota"); err != nil {
			t.Fatal(err)
		}
		return task, n
	}
	seed := func(task *Task, state, code string) int64 {
		id, err := task.Store.AddIntent(map[string]any{"summary": code}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := task.Store.ClaimIntent(id, "worker"); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if ok, err := task.Store.CompareAndSetIntentStateReason(id, "running", state, code); err != nil || !ok {
			t.Fatalf("cas: %v %v", ok, err)
		}
		return id
	}
	expect := func(task *Task, id int64, state, reason string) {
		t.Helper()
		n, err := task.Store.GetNode(id)
		if err != nil {
			t.Fatal(err)
		}
		if n.State != state || n.BlockedReason != reason {
			t.Fatalf("intent %d: got %s/%s want %s/%s", id, n.State, n.BlockedReason, state, reason)
		}
	}

	task, _ := newTask("recovery-live")
	runtime := &taskLLMRuntime{s: &Server{m: m}, taskID: task.ID, agentKey: "worker"}
	_, callErr := runtime.current()
	if !isTaskLLMChainExhausted(callErr) {
		t.Fatalf("expected real chain-exhausted error, got %v", callErr)
	}
	state, code := classifyIntentStop(harness.ReasonModelError, callErr)
	if state != "blocked" || code != intentStopChainUnavailable {
		t.Fatalf("classification: %s/%s", state, code)
	}
	chain := seed(task, state, code)
	quota := seed(task, "blocked", db.IntentBlockedLLMQuota)
	auth := seed(task, "blocked", intentStopAuthFailed)

	reopened, err := m.ReplaceTaskLLMProfiles(task.ID, []int64{profile}, profile)
	if err != nil {
		t.Fatal(err)
	}
	if reopened != 2 {
		t.Fatalf("reopened=%d want 2", reopened)
	}
	expect(task, chain, "open", "")
	expect(task, quota, "open", "")
	expect(task, auth, "blocked", intentStopAuthFailed)

	done, doneID := newTask("recovery-terminal")
	terminal := seed(done, "blocked", intentStopChainUnavailable)
	if err := m.pg.SetStatus(doneID, "done"); err != nil {
		t.Fatal(err)
	}
	if reopened, err := m.ReplaceTaskLLMProfiles(done.ID, []int64{profile}, profile); err != nil || reopened != 0 {
		t.Fatalf("terminal task: reopened=%d err=%v", reopened, err)
	}
	expect(done, terminal, "blocked", intentStopChainUnavailable)
}

type taskDeadlineProvider struct{ calls int }

func (p *taskDeadlineProvider) Stream(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.calls++
		events := []llm.StreamEvent{{Type: llm.SETextDelta, Text: "收尾完成"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}}
		if p.calls == 1 {
			events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: "deadline-wait", ToolName: "DeadlineWait"}, {Type: llm.SEToolInputJSON, Text: "{}"}, {Type: llm.SEMessageDelta, StopReason: "tool_use"}}
		}
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (p *taskDeadlineProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return accumulateStreamForTest(ctx, p.Stream, req)
}

// 任务绝对截止夹逼 Worker 时限(run_seconds=0 明确不限)：真实 runIntent + harness
// 正常收尾后必须记为任务超时，而不是智能体单次运行时限。
func TestRunIntentTaskDeadlineIsNotWorkerRunLimitPG(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{{"unlimited", 0}, {"longer-than-task", time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			m := intentStopPGManager(t)
			task, err := m.CreateTaskWithOptions("deadline-"+tc.name, "task deadline", db.TaskCreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cleanupIntentStopTask(t, m, task)
			e := NewEngine(m)
			e.deadline.Store(task.ID, time.Now().Add(2*time.Second).Unix())
			wait := tool.Build(tool.Spec{Name: "DeadlineWait", Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
				<-ctx.Done()
				return tool.Text("任务截止已到达"), nil
			}})
			provider := &taskDeadlineProvider{}
			worker := agent.NewWorker(provider, "test-model", t.TempDir(), nil, 0, 0, wait)
			worker.SetRunTimeout(tc.timeout)
			id, err := task.Store.AddIntent(map[string]any{"summary": "deadline"}, 1, nil, "planner")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := task.Store.ClaimIntent(id, "worker"); err != nil || !ok {
				t.Fatalf("claim=%v %v", ok, err)
			}
			node, err := task.Store.GetNode(id)
			if err != nil {
				t.Fatal(err)
			}
			e.runIntent(t.Context(), task, "worker", worker, node, "", "")
			final, err := task.Store.GetNode(id)
			if err != nil {
				t.Fatal(err)
			}
			if provider.calls < 2 {
				t.Fatalf("worker did not settle gracefully: calls=%d", provider.calls)
			}
			if final.State != "exhausted" || final.BlockedReason != intentStopTaskTimeout {
				t.Fatalf("got %s/%s want exhausted/%s", final.State, final.BlockedReason, intentStopTaskTimeout)
			}
		})
	}
}

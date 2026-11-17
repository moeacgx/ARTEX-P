package db

import (
	"reflect"
	"testing"
)

// blockedReasonString 兼容 string 与 *string 两种 BlockedReason 类型，nil 视为空串。
func blockedReasonString(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return ""
	}
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.String {
		return rv.String()
	}
	return ""
}

func TestCompareAndSetIntentStateReasonPersistsAndClears(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("intent stop reason", "terminal reason is persisted with the state")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	store := d.Exploration(expID)
	intentID, err := store.AddIntent(map[string]any{"summary": "stop reason"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := store.ClaimIntent(intentID, "worker"); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	check := func(step, wantState, wantReason string) {
		t.Helper()
		node, err := store.GetNode(intentID)
		if err != nil || node == nil {
			t.Fatalf("%s: node=%+v err=%v", step, node, err)
		}
		if node.State != wantState || blockedReasonString(node.BlockedReason) != wantReason {
			t.Fatalf("%s: state=%q reason=%q, want %q/%q", step, node.State, blockedReasonString(node.BlockedReason), wantState, wantReason)
		}
	}

	// 过期的 expected 状态：不得修改，也不得写入原因。
	changed, err := store.CompareAndSetIntentStateReason(intentID, "paused", "exhausted", "run_time_limit")
	if err != nil || changed {
		t.Fatalf("stale expected: changed=%v err=%v", changed, err)
	}
	check("stale expected", "running", "")

	// exhausted 与原因同一条 UPDATE 写入。
	if changed, err = store.CompareAndSetIntentStateReason(intentID, "running", "exhausted", "run_time_limit"); err != nil || !changed {
		t.Fatalf("exhausted: changed=%v err=%v", changed, err)
	}
	check("exhausted+reason", "exhausted", "run_time_limit")

	// 重复使用同一 expected 状态失败，原因保持不变（单赢家语义）。
	if changed, err = store.CompareAndSetIntentStateReason(intentID, "running", "blocked", "llm_auth_failed"); err != nil || changed {
		t.Fatalf("second writer: changed=%v err=%v", changed, err)
	}
	check("second writer", "exhausted", "run_time_limit")

	// blocked 同样可以带原因。
	if changed, err = store.CompareAndSetIntentStateReason(intentID, "exhausted", "blocked", "llm_rate_limited"); err != nil || !changed {
		t.Fatalf("blocked: changed=%v err=%v", changed, err)
	}
	check("blocked+reason", "blocked", "llm_rate_limited")

	// 空原因清空该列，不为无原因的历史数据编造原因。
	if changed, err = store.CompareAndSetIntentStateReason(intentID, "blocked", "exhausted", ""); err != nil || !changed {
		t.Fatalf("empty reason: changed=%v err=%v", changed, err)
	}
	check("empty reason clears", "exhausted", "")

	// 既有 CompareAndSetIntentState 仍清空原因（重开/恢复语义不变）。
	if changed, err = store.CompareAndSetIntentStateReason(intentID, "exhausted", "blocked", "llm_error"); err != nil || !changed {
		t.Fatalf("set blocked: changed=%v err=%v", changed, err)
	}
	if changed, err = store.CompareAndSetIntentState(intentID, "blocked", "open"); err != nil || !changed {
		t.Fatalf("reopen: changed=%v err=%v", changed, err)
	}
	check("reopen clears reason", "open", "")
}

func TestReopenIntentsByBlockedReasons(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	expID, err := d.CreateExploration("reopen by reasons", "only recoverable model-chain reasons are reopened")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, expID)
	store := d.Exploration(expID)

	type seed struct {
		state, reason string
		wantState     string
		wantReason    string
	}
	seeds := map[string]seed{
		"quota":        {"blocked", IntentBlockedLLMQuota, "open", ""},
		"chain":        {"blocked", "llm_chain_unavailable", "open", ""},
		"auth":         {"blocked", "llm_auth_failed", "blocked", "llm_auth_failed"},
		"timelimit":    {"exhausted", "run_time_limit", "exhausted", "run_time_limit"},
		"legacy":       {"exhausted", "", "exhausted", ""},
		"chainnoblock": {"exhausted", "llm_chain_unavailable", "exhausted", "llm_chain_unavailable"},
	}
	ids := map[string]int64{}
	for name, sd := range seeds {
		id, err := store.AddIntent(map[string]any{"summary": name}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		if claimed, err := store.ClaimIntent(id, "worker"); err != nil || !claimed {
			t.Fatalf("claim %s: claimed=%v err=%v", name, claimed, err)
		}
		if changed, err := store.CompareAndSetIntentStateReason(id, "running", sd.state, sd.reason); err != nil || !changed {
			t.Fatalf("seed %s: changed=%v err=%v", name, changed, err)
		}
		ids[name] = id
	}

	if n, err := store.ReopenIntentsByBlockedReasons(nil); err != nil || n != 0 {
		t.Fatalf("empty list: n=%d err=%v", n, err)
	}
	n, err := store.ReopenIntentsByBlockedReasons([]string{IntentBlockedLLMQuota, "llm_chain_unavailable"})
	if err != nil || n != 2 {
		t.Fatalf("reopen: n=%d err=%v, want 2", n, err)
	}
	for name, sd := range seeds {
		node, err := store.GetNode(ids[name])
		if err != nil || node == nil {
			t.Fatalf("%s: node=%+v err=%v", name, node, err)
		}
		if node.State != sd.wantState || blockedReasonString(node.BlockedReason) != sd.wantReason {
			t.Fatalf("%s: state=%q reason=%q, want %q/%q", name, node.State, blockedReasonString(node.BlockedReason), sd.wantState, sd.wantReason)
		}
	}
	// 旧方法保持严格：只匹配单一原因
	if n, err := store.ReopenIntentsByBlockedReason(IntentBlockedLLMQuota); err != nil || n != 0 {
		t.Fatalf("strict single reason after reopen: n=%d err=%v", n, err)
	}
}

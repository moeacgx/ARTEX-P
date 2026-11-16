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

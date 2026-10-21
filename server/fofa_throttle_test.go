package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/integrations/fofa"
)

func TestFOFAThrottleSharedAcrossServerAndRequestKind(t *testing.T) {
	s := newModelExecutionServer(t)
	key := fmt.Sprintf("synthetic-shared-throttle-%d", time.Now().UnixNano())
	config := db.FOFAConfig{RequestIntervalSeconds: 1, RateLimitCooldownSeconds: 1}
	calls := 0
	err := s.runFOFARequest(t.Context(), key, config, nil, func() error { calls++; return &fofa.RateLimitError{RetryAfter: 3 * time.Second} })
	var limited *fofa.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("未保留上游限流类型: %v", err)
	}
	// 另一Server对象代表不同Worker/会话/代理；账户测试也必须走同一入口。
	restarted := &Server{m: s.m}
	err = restarted.runFOFARequest(t.Context(), key, config, nil, func() error { calls++; return nil })
	if !errors.As(err, &limited) || limited.RetryAfter < 2*time.Second || calls != 1 {
		t.Fatalf("重建实例绕过冷却: calls=%d err=%v", calls, err)
	}
	remaining, err := s.m.pg.FOFARequestCooldown(t.Context(), key)
	if err != nil || remaining < 2*time.Second {
		t.Fatalf("冷却没有持久化: %s %v", remaining, err)
	}
}

func TestFOFAThrottleRevalidatesAfterWaiting(t *testing.T) {
	s := newModelExecutionServer(t)
	key := fmt.Sprintf("synthetic-changed-throttle-%d", time.Now().UnixNano())
	config := db.FOFAConfig{RequestIntervalSeconds: 1, RateLimitCooldownSeconds: 1}
	lease, err := s.m.pg.AcquireFOFARequest(t.Context(), key, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Finish(0)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	calls := 0
	err = s.runFOFARequest(ctx, key, config, nil, func() error { calls++; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
		t.Fatalf("排队取消仍发出请求: %v calls=%d", err, calls)
	}
	if err := lease.Finish(0); err != nil {
		t.Fatal(err)
	}
	changed := errors.New("配置或绑定已变化")
	err = s.runFOFARequest(t.Context(), key, config, func() error { return changed }, func() error { calls++; return nil })
	if !errors.Is(err, changed) || calls != 0 {
		t.Fatalf("排队后未重新核验: %v calls=%d", err, calls)
	}
}

func TestFOFAThrottleStatusAndAccountTestRespectCooldown(t *testing.T) {
	s := newModelExecutionServer(t)
	key, on := fmt.Sprintf("synthetic-status-cooldown-%d", time.Now().UnixNano()), true
	c, err := s.m.pg.SetFOFAConfig(t.Context(), db.FOFAConfigPatch{APIKey: &key, Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM settings WHERE key='integration.fofa.v1'`)
		_, _ = s.m.pg.Exec(`DELETE FROM tools WHERE key IN ('fofa_search','fofa_status')`)
	})
	if err := s.seedFOFATool(); err != nil {
		t.Fatal(err)
	}
	busy, err := s.m.pg.AcquireFOFARequest(t.Context(), key, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	busyCtx := context.WithValue(t.Context(), fofaAgentContextKey{}, "auto")
	busyResult, err := s.fofaStatusTool().Call(busyCtx, []byte(`{}`), nil)
	if err != nil || busyResult.IsError || !strings.Contains(busyResult.Flatten(), `"reason":"request_in_progress"`) {
		t.Fatalf("执行中的请求被误报故障: %v %s", err, busyResult.Flatten())
	}
	if err := busy.Finish(0); err != nil {
		t.Fatal(err)
	}
	_ = s.runFOFARequest(t.Context(), key, c, nil, func() error { return &fofa.RateLimitError{RetryAfter: time.Minute} })
	ctx := context.WithValue(t.Context(), fofaAgentContextKey{}, "auto")
	result, err := s.fofaStatusTool().Call(ctx, []byte(`{}`), nil)
	var status map[string]any
	if err != nil || result.IsError || json.Unmarshal([]byte(result.Flatten()), &status) != nil || status["search_tool_available"] != false || status["reason"] != "rate_limited" {
		t.Fatalf("状态未显示共享冷却: %v %s", err, result.Flatten())
	}
	rr := httptest.NewRecorder()
	s.testFOFASettings(rr, httptest.NewRequest(http.MethodPost, "/api/integrations/fofa/test", strings.NewReader(`{}`)))
	if rr.Code != 429 || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("账户检测绕过冷却: %d %s", rr.Code, rr.Body.String())
	}
}

func TestFOFAThrottleSettlementFailureIsNotHiddenByRateLimit(t *testing.T) {
	err := errors.Join(&fofa.RateLimitError{RetryAfter: time.Minute}, db.ErrFOFAThrottleUnsettled)
	result := fofaToolError(err)
	if !result.IsError || !strings.Contains(result.Flatten(), "停止自动请求") || strings.Contains(result.Flatten(), `"code":"rate_limited"`) {
		t.Fatalf("未结算错误被正常冷却提示掩盖: %s", result.Flatten())
	}
}

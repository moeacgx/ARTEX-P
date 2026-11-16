package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Autumn-27/norma/harness"
)

const testQuotaCode = "llm_quota_exhausted"

func TestClassifyIntentStopUpstreamErrors(t *testing.T) {
	cases := []struct {
		name      string
		reason    harness.TerminalReason
		err       error
		wantState string
		wantCode  string
	}{
		{"401 认证失败", harness.ReasonModelError, errors.New("provider returned status 401: invalid api key"), "blocked", intentStopAuthFailed},
		{"401 即使正文含额度字样也不算额度", harness.ReasonModelError, errors.New("status 401 unauthorized: insufficient_quota"), "blocked", intentStopAuthFailed},
		{"普通 429 限流", harness.ReasonModelError, errors.New("status 429: rate limit reached, too many requests"), "blocked", intentStopRateLimited},
		{"5xx 上游错误", harness.ReasonModelError, errors.New("upstream status 503: service unavailable"), "blocked", intentStopUpstreamError},
		{"明确额度不足", harness.ReasonModelError, errors.New("insufficient_quota: you exceeded your current quota, please check your plan and billing details"), "blocked", testQuotaCode},
		{"响应等待超时", harness.ReasonModelError, fmt.Errorf("wait response: %w", context.DeadlineExceeded), "blocked", intentStopResponseTimout},
		{"无法识别的错误", harness.ReasonModelError, errors.New("something odd happened"), "blocked", intentStopLLMError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, code := classifyIntentStop(c.reason, c.err)
			if state != c.wantState || code != c.wantCode {
				t.Fatalf("got (%s,%s) want (%s,%s)", state, code, c.wantState, c.wantCode)
			}
		})
	}
}

func TestClassifyIntentStopLimits(t *testing.T) {
	// 真运行超时：无底层错误
	if s, c := classifyIntentStop(harness.ReasonTimeout, nil); s != "exhausted" || c != intentStopRunTimeLimit {
		t.Fatalf("timeout: got (%s,%s)", s, c)
	}
	// 硬截止错误仍属于时间上限，不被当作 API 错误
	hard := fmt.Errorf("run: %w", harness.ErrHardDeadlineExceeded)
	if s, c := classifyIntentStop(harness.ReasonTimeout, hard); s != "exhausted" || c != intentStopRunTimeLimit {
		t.Fatalf("hard deadline: got (%s,%s)", s, c)
	}
	cleanup := fmt.Errorf("run: %w", harness.ErrHardDeadlineCleanup)
	if s, c := classifyIntentStop(harness.ReasonTimeout, cleanup); s != "exhausted" || c != intentStopRunTimeLimit {
		t.Fatalf("hard deadline cleanup: got (%s,%s)", s, c)
	}
	// 超时终止时若带有可识别的上游错误，错误不能被吞并
	if s, c := classifyIntentStop(harness.ReasonTimeout, errors.New("status 401 unauthorized")); s != "blocked" || c != intentStopAuthFailed {
		t.Fatalf("timeout+401: got (%s,%s)", s, c)
	}
	// 步数上限
	if s, c := classifyIntentStop(harness.ReasonMaxTurns, nil); s != "exhausted" || c != intentStopMaxTurns {
		t.Fatalf("max turns: got (%s,%s)", s, c)
	}
	// 正常结束 / 无错误的中止：分类器不写 blocked 或 exhausted（取消在 runIntent 中另行处理）
	for _, r := range []harness.TerminalReason{harness.ReasonCompleted, harness.ReasonAbortedStreaming, harness.ReasonAbortedTools} {
		if s, c := classifyIntentStop(r, nil); s != "done" || c != "" {
			t.Fatalf("%s: got (%s,%s)", r, s, c)
		}
	}
}

func TestIntentStopErrorCodeChainExhausted(t *testing.T) {
	// 链耗尽但无额度证据：不能叫额度不足
	plain := &taskLLMError{taskID: "t1", chainExhausted: true, cause: errors.New("all selected profiles are unavailable (probe failed)")}
	if got := intentStopErrorCode(plain); got != intentStopChainUnavailable {
		t.Fatalf("plain chain exhaustion: got %q", got)
	}
	// 链耗尽且 cause 含明确额度证据：才算额度不足
	quota := &taskLLMError{taskID: "t1", chainExhausted: true, cause: errors.New("insufficient_quota: you exceeded your current quota, please check your plan and billing details")}
	if got := intentStopErrorCode(quota); got != testQuotaCode {
		t.Fatalf("quota chain exhaustion: got %q", got)
	}
	// nil 与无法识别的错误返回空串
	if got := intentStopErrorCode(nil); got != "" {
		t.Fatalf("nil: got %q", got)
	}
	if got := intentStopErrorCode(errors.New("boom")); got != "" {
		t.Fatalf("unknown: got %q", got)
	}
}

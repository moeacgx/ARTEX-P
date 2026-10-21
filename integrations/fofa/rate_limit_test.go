package fofa_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/integrations/fofa"
)

func TestRateLimitHTTP429PreservesRetryAfterWithoutRetry(t *testing.T) {
	cases := []struct {
		name, header string
		want         time.Duration
	}{
		{"秒数", "17", 17 * time.Second},
		{"零", "0", 0},
		{"空白", " 9 ", 9 * time.Second},
		{"缺失", "", 0},
		{"负数", "-5", 0},
		{"小数", "1.5", 0},
		{"符号", "+5", 0},
		{"非法", "not-a-delay", 0},
		{"过期日期", "Wed, 21 Oct 2015 07:28:00 GMT", 0},
		{"时长溢出", "9223372037", time.Duration(1<<63 - 1)},
		{"整数溢出", "999999999999999999999999999999", time.Duration(1<<63 - 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, "test-secret-key private-upstream-message")
			})
			_, err := fofa.New(client, "test-secret-key").Search(context.Background(), fofa.Query{Query: "test"})
			var limited *fofa.RateLimitError
			if !errors.As(err, &limited) || limited.RetryAfter != tc.want {
				t.Fatalf("限流或Retry-After未正确分类: limited=%+v err=%v want=%s", limited, err, tc.want)
			}
			if !strings.Contains(err.Error(), "限流") || strings.Contains(err.Error(), "test-secret-key") || strings.Contains(err.Error(), "private-upstream-message") || strings.Contains(err.Error(), "https://") {
				t.Fatalf("限流错误未安全脱敏: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("单次请求被自动重试: %d", calls.Load())
			}
		})
	}
}

func TestRateLimitHTTPDatePreservesFutureCooldown(t *testing.T) {
	until := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	var calls atomic.Int32
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", until.Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	err := fofa.New(client, "test-secret-key").Test(context.Background())
	var limited *fofa.RateLimitError
	if !errors.As(err, &limited) || limited.RetryAfter <= 4*time.Minute || limited.RetryAfter > 5*time.Minute {
		t.Fatalf("未来HTTP日期未转为正冷却时间: limited=%+v err=%v", limited, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("账户请求被自动重试: %d", calls.Load())
	}
}

func TestRateLimitBodyErrorPreservesHeaderAndSanitizesMessage(t *testing.T) {
	for _, message := range []string{
		"request rate limit exceeded test-secret-key private-upstream-message",
		"too many requests test-secret-key private-upstream-message",
		"调用频率过高 test-secret-key private-upstream-message",
		"已被限流 test-secret-key private-upstream-message",
	} {
		t.Run(strings.Split(message, " test-secret")[0], func(t *testing.T) {
			var calls atomic.Int32
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "43")
				_ = json.NewEncoder(w).Encode(map[string]any{"error": true, "errmsg": message})
			})
			_, err := fofa.New(client, "test-secret-key").Search(context.Background(), fofa.Query{Query: "test"})
			var limited *fofa.RateLimitError
			if !errors.As(err, &limited) || limited.RetryAfter != 43*time.Second {
				t.Fatalf("HTTP200频率拒绝未识别: limited=%+v err=%v", limited, err)
			}
			if strings.Contains(err.Error(), "test-secret-key") || strings.Contains(err.Error(), "private-upstream-message") || calls.Load() != 1 {
				t.Fatalf("限流响应泄密或重复调用: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestRateLimitDoesNotMisclassifyQueryQuotaOrAuthenticationErrors(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		status        int
	}{
		{"结果过多", "too many results", 200},
		{"字段过多", "too many fields", 200},
		{"额度不足", "insufficient quota", 200},
		{"鉴权正文", "invalid api key", 200},
		{"鉴权状态", "too many requests", 401},
		{"服务错误", "too many requests", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": true, "errmsg": tc.message})
			})
			_, err := fofa.New(client, "test-secret-key").Search(context.Background(), fofa.Query{Query: "test"})
			var limited *fofa.RateLimitError
			if err == nil || errors.As(err, &limited) || strings.Contains(err.Error(), "限流") {
				t.Fatalf("非频率错误不应触发账户冷却: %v", err)
			}
		})
	}
}

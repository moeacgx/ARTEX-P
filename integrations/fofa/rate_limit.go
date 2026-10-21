package fofa

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError 表示上游频率限制；RetryAfter 为零时由调用方采用本地冷却策略。
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e != nil && e.RetryAfter > 0 {
		seconds := e.RetryAfter / time.Second
		if e.RetryAfter%time.Second != 0 {
			seconds++
		}
		return fmt.Sprintf("FOFA 请求被限流，请至少等待 %d 秒后重试", seconds)
	}
	return "FOFA 请求被限流，请稍后重试"
}

// 无效或过期值交给调用方的默认冷却策略；合法极大值饱和为最大正时长。
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	digits := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digits = false
			break
		}
	}
	if digits {
		const maximum = time.Duration(1<<63 - 1)
		seconds, err := strconv.ParseUint(value, 10, 64)
		if errors.Is(err, strconv.ErrRange) || seconds > uint64(maximum/time.Second) {
			return maximum
		}
		if err != nil {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	// time.Time.Sub 在时间差超出 Duration 范围时会自动饱和，不会整数回绕。
	return when.Sub(now)
}

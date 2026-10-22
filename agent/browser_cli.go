package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

// 等待本轮浏览器真实退出再归还任务活动引用，取消不会提前解除出口占用。
func browserRunEnvironment(env []string) []string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic("无法生成浏览器会话身份")
	}
	return append(env, "ARTEX_PLAYWRIGHT_RUN="+hex.EncodeToString(token[:]))
}

func closeRunBrowser(ctx context.Context, dir string, env []string) {
	tool, err := exec.LookPath("artex-playwright-cli")
	if err != nil {
		return
	}
	attempts := 0
	ri := RunInfoFrom(ctx)
	waitBrowserCleanup(func() error {
		attempts++
		// 清理独立于已取消的执行上下文；每次有界，但确认前绝不归还出口占用。
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cleanupCtx, tool, "cleanup")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		err := cmd.Run()
		if cleanupCtx.Err() != nil {
			return cleanupCtx.Err()
		}
		return err
	}, browserCleanupRetryDelay, func(attempt int, err error) {
		// 不记录命令输出或环境，其中可能包含代理凭据和浏览内容。
		log.Printf("[browser] task %d intent %d 本轮浏览器清理尚未确认，第 %d 次失败，%s 后重试: %s", ri.TaskID, ri.IntentID, attempt, browserCleanupRetryDelay(attempt), browserCleanupFailureCode(err))
	})
	if attempts > 1 {
		log.Printf("[browser] task %d intent %d 本轮浏览器已确认退出，清理重试 %d 次后恢复", ri.TaskID, ri.IntentID, attempts-1)
	}
}

func browserCleanupFailureCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "cleanup_timeout"
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return fmt.Sprintf("cleanup_exit_%d", exited.ExitCode())
	}
	return "cleanup_start_failed"
}

// 临时清理失败不能变成不可恢复的永久引用。调用方仍处于退出阶段，
// Planner 的异步退出协议及 Worker 的操作引用持续覆盖整个重试过程。
func waitBrowserCleanup(cleanup func() error, delay func(int) time.Duration, report func(int, error)) {
	for attempt := 1; ; attempt++ {
		if err := cleanup(); err != nil {
			report(attempt, err)
			time.Sleep(delay(attempt))
			continue
		}
		return
	}
}

func browserCleanupRetryDelay(attempt int) time.Duration {
	delay := time.Second
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	return min(delay, 30*time.Second)
}

type browserCleanupKey struct{}

// WithBrowserCleanupFailure 由正式操作准入绑定；清理未确认时保留活动占用。
func WithBrowserCleanupFailure(ctx context.Context, report func()) context.Context {
	return context.WithValue(ctx, browserCleanupKey{}, report)
}
func reportBrowserCleanupFailure(ctx context.Context) {
	if report, ok := ctx.Value(browserCleanupKey{}).(func()); ok {
		report()
	}
}

// ReportProxyCleanupFailure 让 MCP 和浏览器共同遵守真实退出屏障。
func ReportProxyCleanupFailure(ctx context.Context) { reportBrowserCleanupFailure(ctx) }

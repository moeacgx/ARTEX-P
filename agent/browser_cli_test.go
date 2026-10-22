package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlannerBrowserCleanupRetryResolvesPendingExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var attempts atomic.Int32
	_, _, err := waitPlannerRound(ctx, time.Millisecond, func() (bool, string, error) {
		waitBrowserCleanup(func() error {
			if attempts.Add(1) == 1 {
				cancel()
				return errors.New("browser still closing")
			}
			<-release
			return nil
		}, func(int) time.Duration { return 0 }, func(int, error) {})
		return false, "", nil
	})
	if !HasPendingPlannerExit(err) {
		close(release)
		t.Fatalf("清理未退出时没有保留可等待的 Planner 屏障: %v", err)
	}
	resolved := make(chan error, 1)
	go func() { resolved <- AwaitPlannerExit(context.Background(), err) }()
	select {
	case final := <-resolved:
		close(release)
		t.Fatalf("浏览器仍存活时提前确认 Planner 退出: %v", final)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case final := <-resolved:
		if errors.Is(final, ErrPlannerCleanup) || !errors.Is(final, context.Canceled) {
			t.Fatalf("清理恢复后仍遗留永久失败或丢失原取消原因: %v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("重试确认浏览器退出后 Planner 屏障没有解除")
	}
}

func TestBrowserCleanupRetriesUntilConfirmedExit(t *testing.T) {
	var attempts atomic.Int32
	blocked := make(chan struct{})
	finished := make(chan struct{})
	var unblocked atomic.Bool
	unblock := func() {
		if !unblocked.Swap(true) {
			close(blocked)
		}
	}
	go func() {
		waitBrowserCleanup(func() error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary cleanup timeout")
			}
			<-blocked
			return nil
		}, func(int) time.Duration { return time.Millisecond }, func(int, error) {})
		close(finished)
	}()
	defer unblock()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for attempts.Load() < 2 {
		select {
		case <-deadline.C:
			t.Fatal("临时失败后没有重试清理")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-finished:
		t.Fatal("浏览器尚未确认退出就归还了操作")
	default:
	}
	unblock()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("确认真实退出后没有归还操作")
	}
}

func TestBrowserCleanupRecoveryReturnsAndReportsFailures(t *testing.T) {
	attempts, reported := 0, 0
	waitBrowserCleanup(func() error {
		attempts++
		if attempts < 3 {
			return errors.New("cleanup failed")
		}
		return nil
	}, func(int) time.Duration { return 0 }, func(attempt int, err error) {
		reported++
		if attempt != reported || err == nil {
			t.Errorf("清理重试诊断丢失: attempt=%d err=%v", attempt, err)
		}
	})
	if attempts != 3 || reported != 2 {
		t.Fatalf("恢复后未正常退出: attempts=%d reported=%d", attempts, reported)
	}
}

func TestBrowserCleanupRetryBackoffIsBounded(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 6: 30 * time.Second, 1000: 30 * time.Second} {
		if got := browserCleanupRetryDelay(attempt); got != want {
			t.Errorf("attempt=%d delay=%s want=%s", attempt, got, want)
		}
	}
}

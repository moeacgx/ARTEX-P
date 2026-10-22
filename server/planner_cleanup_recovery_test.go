package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/llm"
)

func TestPlannerPGPendingCleanupAutomaticallyReplansAfterTrueExit(t *testing.T) {
	t.Setenv("ARTEX_PLANNER_ROUND_TIMEOUT", "500ms")
	s := newTriggerRegressionServer(t)
	task, err := s.m.CreateTask("清理自动恢复", "仍有目标需要继续", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.DeleteTask(task.ID, DeleteTaskOptions{})
	if _, err = task.Store.AddGoal(map[string]any{"text": "不能因单轮超时丢弃的目标"}, "user"); err != nil {
		t.Fatal(err)
	}
	p := &delayedPlannerProvider{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}), requests: make(chan llm.CompletionRequest, 4)}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(p.release) }) }
	defer release()
	planner := agent.NewPlanner(p, "cleanup-retry", s.m.dir, nil, 100000, 1)
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, new(agent.Worker) })
	s.engine.debounce = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() { defer close(exited); s.engine.plannerLoop(ctx, task) }()
	defer func() {
		cancel()
		release()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("清理恢复测试循环未退出")
		}
	}()
	task.Notify()
	select {
	case <-p.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("未触发单轮截止")
	}
	// Plan 的十秒有界清理等待结束后，真正的执行仍被模型退出屏障挡住。
	deadline := time.After(15 * time.Second)
	for s.engine.ActiveLLMCalls(task.ID) != 0 {
		select {
		case <-deadline:
			t.Fatal("单轮没有有界返回待清理状态")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if s.engine.plannerResumeError(task.ID) == nil || plannerPersistentCount(t, s.m, task.ID) != 1 || p.calls.Load() != 1 {
		t.Fatal("清理仍存活时释放了占用或启动了重复规划")
	}
	release()
	// 不再 Notify、不手工恢复，普通心跳是十分钟，必须由自动恢复唤醒。
	plannerRestartRequest(t, p.requests)
	awaitPlannerState(t, "恢复后的规划没有释放操作", func() bool {
		return s.engine.plannerResumeError(task.ID) == nil && plannerPersistentCount(t, s.m, task.ID) == 0
	})
}

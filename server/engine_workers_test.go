package server

import (
	"context"
	"testing"
)

func TestEngineScaleWorkersChangesLivePool(t *testing.T) {
	const taskID = "workers-scale"
	task := &Task{ID: taskID, Status: "done"}
	m := &Manager{tasks: map[string]*Task{taskID: task}}
	e := NewEngine(m)

	ctx, cancel := context.WithCancel(context.Background())
	rt := &taskRuntime{
		ctx:           ctx,
		cancel:        cancel,
		workerCancels: map[string]context.CancelFunc{},
	}
	e.runtimes[taskID] = rt
	e.started.Store(taskID, true)
	t.Cleanup(func() {
		cancel()
		rt.wg.Wait()
	})

	e.ScaleWorkers(task, 3)
	if got := liveWorkerCount(rt); got != 3 {
		t.Fatalf("初始 worker 数=%d，期望 3", got)
	}
	e.ScaleWorkers(task, 10)
	if got := liveWorkerCount(rt); got != 10 {
		t.Fatalf("扩容后 worker 数=%d，期望 10", got)
	}
	e.ScaleWorkers(task, 3)
	if got := liveWorkerCount(rt); got != 3 {
		t.Fatalf("缩容后 worker 数=%d，期望 3", got)
	}
}

func TestEngineScaleWorkersIgnoresNotStartedTask(t *testing.T) {
	const taskID = "workers-not-started"
	task := &Task{ID: taskID, Status: "done"}
	e := NewEngine(&Manager{tasks: map[string]*Task{taskID: task}})

	e.ScaleWorkers(task, 10)
	if _, ok := e.runtimes[taskID]; ok {
		t.Fatal("未启动任务不应创建 Worker runtime")
	}
}

func liveWorkerCount(rt *taskRuntime) int {
	rt.workerMu.Lock()
	defer rt.workerMu.Unlock()
	return len(rt.workerCancels)
}

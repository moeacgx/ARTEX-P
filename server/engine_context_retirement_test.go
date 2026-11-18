package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestFirstWorkerRetirementDoesNotCancelSharedExecution(t *testing.T) {
	engine := NewEngine(&Manager{})
	runtimeCtx, stop := context.WithCancel(context.Background())
	defer stop()
	engine.runtimes["retirement"] = &taskRuntime{ctx: runtimeCtx, cancel: stop}
	firstWorker, retire := context.WithCancel(runtimeCtx)
	execution := engine.execContextFor(firstWorker, "retirement")
	retire()
	if execution.Err() != nil {
		t.Fatal("退休首个 Worker 取消了任务共享执行上下文")
	}
	if engine.execContextFor(runtimeCtx, "retirement") != execution {
		t.Fatal("缩容不应替换仍存活的共享执行上下文")
	}
	stop()
	if execution.Err() == nil {
		t.Fatal("停止任务必须取消共享执行上下文")
	}
}

type retirementCall struct {
	ctx      context.Context
	release  chan struct{}
	canceled chan struct{}
	exited   chan struct{}
	once     sync.Once
}

func (c *retirementCall) finish() { c.once.Do(func() { close(c.release) }) }

func TestActiveWorkersScaleDownDrainAndTaskControlExit(t *testing.T) {
	for _, control := range []string{"pause", "delete"} {
		t.Run(control, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			task, err := m.CreateTask("活跃缩容回归", "", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer m.DeleteTask(task.ID, DeleteTaskOptions{})
			if err = m.SetTaskStatus(task.ID, "running"); err != nil {
				t.Fatal(err)
			}
			engine := NewEngine(m)
			runtimeCtx, stop := context.WithCancel(t.Context())
			runtime := &taskRuntime{ctx: runtimeCtx, cancel: stop, workerCancels: map[string]context.CancelFunc{}}
			engine.runtimes[task.ID] = runtime
			engine.started.Store(task.ID, true)
			entered := make(chan *retirementCall, 10)
			var callsMu sync.Mutex
			var calls []*retirementCall
			defer func() {
				stop()
				callsMu.Lock()
				for _, call := range calls {
					call.finish()
				}
				callsMu.Unlock()
				runtime.wg.Wait()
			}()
			provider := retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				call := &retirementCall{ctx: ctx, release: make(chan struct{}), canceled: make(chan struct{}), exited: make(chan struct{})}
				callsMu.Lock()
				calls = append(calls, call)
				callsMu.Unlock()
				entered <- call
				defer close(call.exited)
				select {
				case <-call.release:
					return followupText("意图已经执行")
				case <-ctx.Done():
					close(call.canceled)
					<-call.release
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				}
			}}
			worker := agent.NewWorker(provider, "synthetic", m.dir, nil, 100000, 1)
			worker.SetNonStreaming(func() bool { return true })
			engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, worker })
			addIntent := func() int64 {
				id, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "等待本地模型回执"}, 1, "open", "planner", nil)
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			nextCall := func() *retirementCall {
				select {
				case call := <-entered:
					return call
				case <-time.After(5 * time.Second):
					t.Fatal("Worker 没有真实执行意图")
					return nil
				}
			}
			firstID := addIntent()
			engine.ScaleWorkers(task, 1)
			first := nextCall()
			ids := []int64{firstID, addIntent(), addIntent()}
			engine.ScaleWorkers(task, 3)
			active := []*retirementCall{first, nextCall(), nextCall()}
			engine.ScaleWorkers(task, 1)
			for _, call := range active {
				if call.ctx.Err() != nil {
					t.Fatal("缩容取消了已领取的意图")
				}
			}
			for _, call := range active {
				call.finish()
			}
			awaitPlannerState(t, "缩容未排空全部已领取意图", func() bool {
				for _, id := range ids {
					node, err := task.Store.GetNode(id)
					if err != nil || node.State == "running" || node.State == "open" {
						return false
					}
				}
				return plannerPersistentCount(t, m, task.ID) == 0
			})
			nextID := addIntent()
			next := nextCall()
			if liveWorkerCount(runtime) != 1 {
				t.Fatal("缩容后 Worker 数不正确")
			}
			stopped := make(chan struct{})
			if control == "pause" {
				engine.Pause(task.ID, agent.AbortPausedByUser)
			} else {
				engine.BeginDelete(task.ID)
				go func() { engine.StopTask(task.ID); close(stopped) }()
			}
			select {
			case <-next.canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("任务控制没有取消真实模型请求")
			}
			if plannerPersistentCount(t, m, task.ID) != 1 {
				t.Fatal("模型尚未退出即丢失持久活动引用")
			}
			if control == "delete" {
				select {
				case <-stopped:
					t.Fatal("删除未等待实际执行退出")
				default:
				}
			}
			next.finish()
			awaitPlannerState(t, "真实退出后活动引用未释放", func() bool { return plannerPersistentCount(t, m, task.ID) == 0 })
			if control == "pause" {
				node, err := task.Store.GetNode(nextID)
				if err != nil || node.State != "open" {
					t.Fatalf("暂停丢失可恢复意图: %+v %v", node, err)
				}
			} else {
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Fatal("删除没有完成排空")
				}
			}
		})
	}
}

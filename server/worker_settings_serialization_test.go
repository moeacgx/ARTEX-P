package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkerSettingsSerializePersistenceAndAllPoolsPG(t *testing.T) {
	m := intentStopPGManager(t)
	e := NewEngine(m)
	s := &Server{m: m, engine: e}
	var runtimes []*taskRuntime
	for i := 0; i < 2; i++ {
		task := &Task{ID: fmt.Sprintf("settings-serialization-%d", i), Status: "done"}
		m.mu.Lock()
		m.tasks[task.ID] = task
		m.mu.Unlock()
		ctx, cancel := context.WithCancel(t.Context())
		rt := &taskRuntime{ctx: ctx, cancel: cancel, workerCancels: map[string]context.CancelFunc{}}
		e.runtimes[task.ID] = rt
		e.started.Store(task.ID, true)
		runtimes = append(runtimes, rt)
		t.Cleanup(func() { cancel(); rt.wg.Wait() })
	}
	// 锁住整个池创建区，确保第一请求已持久化但尚未完成扩缩容。
	e.workerPoolMu.Lock()
	locked := true
	defer func() {
		if locked {
			e.workerPoolMu.Unlock()
		}
	}()
	request := func(n int, done chan<- int) {
		r := httptest.NewRequest("PUT", "/api/settings", strings.NewReader(fmt.Sprintf(`{"workers":%d}`, n)))
		w := httptest.NewRecorder()
		s.putSettings(w, r)
		done <- w.Code
	}
	first, second := make(chan int, 1), make(chan int, 1)
	go request(2, first)
	deadline := time.Now().Add(3 * time.Second)
	for m.Workers() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("第一请求未持久化")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go request(4, second)
	time.Sleep(180 * time.Millisecond)
	if got := m.Workers(); got != 2 {
		t.Errorf("第一请求扩缩容未完成，第二请求已覆盖存储: %d", got)
	}
	e.workerPoolMu.Unlock()
	locked = false
	for _, done := range []chan int{first, second} {
		select {
		case code := <-done:
			if code != 200 {
				t.Errorf("HTTP状态=%d", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("设置请求阻塞")
		}
	}
	if got := m.Workers(); got != 4 {
		t.Errorf("最终存储=%d", got)
	}
	for _, rt := range runtimes {
		if got := liveWorkerCount(rt); got != 4 {
			t.Errorf("最终活池=%d", got)
		}
	}
}

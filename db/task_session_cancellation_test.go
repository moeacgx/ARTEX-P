package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSharedTaskSessionCallerCancellationPG(t *testing.T) {
	for _, mode := range []string{"already-canceled", "canceled-while-waiting", "wait-over-probe-timeout"} {
		t.Run(mode, func(t *testing.T) {
			d, _, task := plannerRunTestDBs(t)
			state, err := d.AcquireTaskOperation(task.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := d.ReleaseTaskOperationToken(task.ID, state.OperationToken); err != nil {
					t.Errorf("活操作释放失败: %v", err)
				}
			}()
			key := taskOperationLeaseKey{database: d.DB, taskID: task.ID}
			taskOperationLeases.Lock()
			lease := taskOperationLeases.leases[key]
			taskOperationLeases.Unlock()
			var pid int
			if err := lease.conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "already-canceled" {
				cancel()
			}
			lease.txMu.Lock()
			type result struct {
				lease *taskOperationLease
				err   error
			}
			resultCh := make(chan result, 1)
			go func() {
				got, _, err := d.acquireTaskOperationSessionContext(ctx, task.ID)
				resultCh <- result{got, err}
			}()
			if mode == "canceled-while-waiting" {
				time.Sleep(80 * time.Millisecond)
				cancel()
			}
			if mode == "wait-over-probe-timeout" {
				time.Sleep(5200 * time.Millisecond)
			} else {
				time.Sleep(80 * time.Millisecond)
			}
			lease.txMu.Unlock()
			got := <-resultCh
			if mode == "wait-over-probe-timeout" {
				if got.err != nil || got.lease != lease {
					t.Fatalf("排队超过探测时限破坏共享会话: %v", got.err)
				}
				defer d.releaseTaskOperationSession(task.ID)
			} else if !errors.Is(got.err, context.Canceled) {
				t.Errorf("取消错误=%v", got.err)
			}
			var held bool
			if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ShareLock' AND granted)`, pid).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if !held {
				t.Fatal("调用者取消或排队导致另一活操作丢失共享锁")
			}
		})
	}
}

func TestSharedTaskSessionDisconnectInvalidatesGroupPG(t *testing.T) {
	d, _, firstTask := plannerRunTestDBs(t)
	secondTask, err := d.CreateTask("共享断连分组", "仅本地 fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(secondTask.ID, false, false) })
	if _, err := d.AcquireTaskOperation(firstTask.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AcquireTaskOperation(secondTask.ID); err != nil {
		t.Fatal(err)
	}
	taskOperationLeases.Lock()
	old := taskOperationLeases.sessions[d.DB]
	taskOperationLeases.Unlock()
	var pid int
	if err := old.conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := d.QueryRow(`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("终止 fixture 会话: %v", err)
	}
	fresh, err := d.AcquireTaskOperation(firstTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.ReleaseTaskOperationToken(firstTask.ID, fresh.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	taskOperationLeases.Lock()
	current := taskOperationLeases.sessions[d.DB]
	stale := taskOperationLeases.leases[taskOperationLeaseKey{database: d.DB, taskID: secondTask.ID}]
	taskOperationLeases.Unlock()
	if current == old || stale != nil {
		t.Fatal("真实断连未使共享 session 整组失效")
	}
	second, err := d.AcquireTaskOperation(secondTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReleaseTaskOperationToken(secondTask.ID, second.OperationToken); err != nil {
		t.Fatal(err)
	}
}

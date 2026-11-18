package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// 每个入口都借出真实独占连接；Begin 与收尾共用连接，只有新增占用需要准入。
type executorAdmissionFixture struct {
	d, other *DB
	task     *Task
	x        *TaskProxySwitchExecutor
	begin    func() error
	finish   func() error
	pid      int
}

func newExecutorAdmissionFixture(t *testing.T, kind string, gate ...*closeCommitConnector) executorAdmissionFixture {
	t.Helper()
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, err := other.DeleteTaskCascade(task.ID, false, false); err != nil {
			t.Error(err)
		}
	})
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if kind == "idle" || kind == "idle_revision" {
		oldPath := proxySecretsPath
		t.Cleanup(func() { proxySecretsPath = oldPath })
		if err := ConfigureProxySecrets(filepath.Join(t.TempDir(), "proxy.key")); err != nil {
			t.Fatal(err)
		}
		p, err := d.CreateProxy(&Proxy{Name: "关闭边界合成代理", Kind: ProxyKindHTTP, URL: "http://127.0.0.1:19", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := other.DeleteTaskCascade(task.ID, false, false); err != nil {
				t.Error(err)
			}
			if _, err := other.Exec(`DELETE FROM outbound_proxies WHERE id=$1`, p.ID); err != nil {
				t.Error(err)
			}
		})
		snapshot, err := p.SnapshotEncrypted()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec(`UPDATE tasks SET proxy_mode='fixed',proxy_id=$2,proxy_snapshot=$3 WHERE id=$1`, task.ID, p.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
	f := executorAdmissionFixture{d: d, other: other, task: task}
	if len(gate) != 0 {
		pool := sql.OpenDB(retryingPostgresConnector{base: gate[0]})
		old, exists := taskOperationLockPools.Load(d.DB)
		taskOperationLockPools.Store(d.DB, pool)
		t.Cleanup(func() {
			_ = pool.Close()
			if exists {
				_ = old.(*sql.DB).Close()
			}
		})
	}
	var token string
	if kind == "policy" {
		subject := ProxySubject{Kind: "task", ID: task.ID}
		policy, err := d.GetProxyPolicy(subject)
		if err != nil {
			t.Fatal(err)
		}
		x, err := d.AcquireProxyPolicyExecutor(t.Context(), subject)
		if err != nil {
			t.Fatal(err)
		}
		f.x = x.x
		f.begin = func() error {
			_, got, err := x.Begin(policy.State.Revision, ProxyPolicyPatch{Mode: TaskProxyModeDirect})
			if err == nil {
				token = got
			}
			return err
		}
		f.finish = func() error { _, err := x.Complete(policy.State.Revision, token); return err }
	} else {
		x, err := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.x = x
		switch kind {
		case "task":
			f.begin = func() error {
				_, got, err := x.Begin(0, TaskProxyModeDirect, nil)
				if err == nil {
					token = got
				}
				return err
			}
			f.finish = func() error { _, err := x.Complete(0, token); return err }
		case "idle":
			f.begin = x.AcquireIdleTaskProxyActivity
			f.finish = x.ReleaseTaskProxyActivity
		case "idle_revision":
			f.begin = func() error { return x.AcquireIdleTaskProxyActivityRevision(0) }
			f.finish = x.ReleaseTaskProxyActivity
		default:
			t.Fatal("未知测试入口", kind)
		}
	}
	t.Cleanup(f.x.Close)
	if err := f.x.conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&f.pid); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExecutorAdmissionIncludesCommitReturnPG(t *testing.T) {
	for _, kind := range []string{"task", "policy", "idle", "idle_revision"} {
		t.Run(kind, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			gate := &closeCommitConnector{base: stdlib.GetConnector(*cfg), entered: make(chan struct{}), resume: make(chan struct{})}
			f := newExecutorAdmissionFixture(t, kind, gate)
			gate.armed.Store(true)
			done := make(chan error, 1)
			go func() { done <- f.begin() }()
			select {
			case <-gate.entered:
			case <-time.After(3 * time.Second):
				close(gate.resume)
				t.Fatal("未进入真实提交已落而回执尚未返回的窗口")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err = f.d.closeContext(ctx)
			cancel()
			n := executorInflight(f.d)
			close(gate.resume)
			if beginErr := <-done; beginErr != nil {
				t.Fatal(beginErr)
			}
			if !errors.Is(err, context.DeadlineExceeded) || n != 1 {
				t.Errorf("提交回执未返回就退出准入: close=%v inflight=%d", err, n)
			}
			if err := f.d.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.finish(); err != nil {
				t.Fatal(err)
			}
			f.assertEmpty(t)
		})
	}
}

func executorInflight(d *DB) int {
	d.operationLifecycle.mu.Lock()
	defer d.operationLifecycle.mu.Unlock()
	return d.operationLifecycle.inflight
}

func (f executorAdmissionFixture) assertEmpty(t *testing.T) {
	t.Helper()
	var count int
	err := f.other.QueryRow(`SELECT
 (SELECT count(*) FROM task_operation_activity WHERE task_id=$1 AND (active_count<>0 OR switch_token<>'' OR operation_owners<>'{}'::jsonb))+
 (SELECT count(*) FROM task_proxy_activity WHERE task_id=$1)+
 (SELECT count(*) FROM proxy_subject_policies WHERE task_id=$1 AND switch_token<>'')`, f.task.ID).Scan(&count)
	if err != nil || count != 0 || executorInflight(f.d) != 0 {
		t.Fatalf("入口退出遗留活动 count=%d inflight=%d err=%v", count, executorInflight(f.d), err)
	}
}

func (f executorAdmissionFixture) assertSessionLock(t *testing.T, want int) {
	t.Helper()
	var count int
	err := f.other.QueryRow(`SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ExclusiveLock' AND granted`, f.pid).Scan(&count)
	if err != nil || count != want {
		t.Fatalf("旧执行器真实会话锁=%d，期望=%d err=%v", count, want, err)
	}
}

// 用另一真实事务的 lifecycle 锁固定准入内部窗口，不依赖调度睡眠猜测。
func (f executorAdmissionFixture) blockBegin(t *testing.T) (*sql.Tx, <-chan error) {
	t.Helper()
	tx, err := f.other.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if err = lockProxyLifecycle(tx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.begin() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		if err = f.other.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND wait_event='advisory')`, f.pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return tx, done
		}
		if time.Now().After(deadline) {
			t.Fatal("执行器未进入真实 lifecycle 锁等待")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestExecutorAdmissionCloseWaitsAndAllowsExistingCompletionPG(t *testing.T) {
	for _, kind := range []string{"task", "policy", "idle", "idle_revision"} {
		t.Run(kind, func(t *testing.T) {
			f := newExecutorAdmissionFixture(t, kind)
			tx, done := f.blockBegin(t)
			if n := executorInflight(f.d); n != 1 {
				_ = tx.Rollback()
				if err := <-done; err == nil {
					_ = f.finish()
				}
				t.Fatalf("真实执行入口不在准入计数内: %d", n)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			err := f.d.closeContext(ctx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("关闭未等待在途入口: %v", err)
			}
			f.assertSessionLock(t, 1)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if n := executorInflight(f.d); n != 0 {
				t.Fatalf("成功后准入计数未释放: %d", n)
			}
			if err := f.d.Close(); err != nil {
				t.Fatal(err)
			}
			f.assertSessionLock(t, 1)
			// Close 已完成，先前取得的精确 token/独占引用仍可通过旧连接收尾。
			if err := f.finish(); err != nil {
				t.Fatal(err)
			}
			if err := f.begin(); !errors.Is(err, ErrDBClosing) {
				t.Errorf("旧入口关闭后仍可再次准入: %v", err)
			}
			f.assertEmpty(t)
			f.x.Close()
			f.assertSessionLock(t, 0)
			var exists bool
			if err := f.other.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, f.pid).Scan(&exists); err != nil || exists {
				t.Fatalf("关闭池的旧执行器收尾后 backend 未退出: exists=%v err=%v", exists, err)
			}
		})
	}
}

func TestExecutorAdmissionCancellationAndFailureReleaseCountPG(t *testing.T) {
	for _, kind := range []string{"task", "policy", "idle", "idle_revision"} {
		t.Run(kind, func(t *testing.T) {
			f := newExecutorAdmissionFixture(t, kind)
			tx, done := f.blockBegin(t)
			var canceled bool
			if err := f.other.QueryRow(`SELECT pg_cancel_backend($1)`, f.pid).Scan(&canceled); err != nil || !canceled {
				t.Fatalf("不能取消自有测试查询: %v", err)
			}
			var pgErr *pgconn.PgError
			if err := <-done; !errors.As(err, &pgErr) || pgErr.Code != "57014" {
				t.Fatalf("未保留原始取消错误: %v", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			f.assertEmpty(t)
			f.assertSessionLock(t, 1)
			// 业务校验失败也必须释放计数，不能影响下一次合法执行。
			if kind == "task" {
				if _, _, err := f.x.Begin(99, TaskProxyModeDirect, nil); !errors.Is(err, ErrProxyRevisionConflict) {
					t.Fatal(err)
				}
			} else if kind == "idle_revision" {
				if err := f.x.AcquireIdleTaskProxyActivityRevision(99); !errors.Is(err, ErrProxyRevisionConflict) {
					t.Fatal(err)
				}
			}
			f.assertEmpty(t)
			if err := f.begin(); err != nil {
				t.Fatal(err)
			}
			if err := f.begin(); err == nil {
				t.Error("已有屏障或独占引用时重复准入未失败")
			}
			if n := executorInflight(f.d); n != 0 {
				t.Fatalf("业务拒绝后准入计数泄漏: %d", n)
			}
			if err := f.finish(); err != nil {
				t.Fatal(err)
			}
			f.assertEmpty(t)
		})
	}
}

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type admissionCancelConnector struct {
	base   driver.Connector
	cancel context.CancelFunc
	stage  string
	armed  atomic.Bool
}

func (c *admissionCancelConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *admissionCancelConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &admissionCancelConn{lostCommitConn: &lostCommitConn{underlyingDriverConn: conn}, owner: c}, nil
}

type admissionCancelConn struct {
	*lostCommitConn
	owner *admissionCancelConnector
}

func (c *admissionCancelConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.owner.stage == "begin" && c.owner.armed.Swap(false) {
		c.owner.cancel()
	}
	tx, err := c.underlyingDriverConn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &admissionCancelTx{Tx: tx, owner: c.owner}, nil
}
func (c *admissionCancelConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.underlyingDriverConn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err == nil && c.owner.stage == "write" && strings.Contains(q, "INSERT INTO proxy_subject_activity") && c.owner.armed.Swap(false) {
		c.owner.cancel()
	}
	return r, err
}

func (c *admissionCancelConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.underlyingDriverConn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err == nil && c.owner.stage == "advisory" && strings.Contains(q, "pg_try_advisory_lock_shared") && c.owner.armed.Swap(false) {
		c.owner.cancel()
	}
	return rows, err
}

type admissionCancelTx struct {
	driver.Tx
	owner *admissionCancelConnector
}

func (tx *admissionCancelTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.owner.stage == "commit" && tx.owner.armed.Swap(false) {
		tx.owner.cancel()
	}
	return err
}

func TestTaskAdmissionCancellationPreservesOtherSessionPG(t *testing.T) {
	for _, stage := range []string{"advisory", "begin", "write", "commit"} {
		t.Run(stage, func(t *testing.T) {
			d, _, firstTask := plannerRunTestDBs(t)
			cfg, err := pgx.ParseConfig(testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			inject := &admissionCancelConnector{base: stdlib.GetConnector(*cfg), cancel: cancel, stage: stage}
			pool := sql.OpenDB(retryingPostgresConnector{base: inject})
			old, _ := taskOperationLockPools.Load(d.DB)
			taskOperationLockPools.Store(d.DB, pool)
			defer func() { taskOperationLockPools.Store(d.DB, old); pool.Close() }()
			first, err := d.AcquireTaskOperation(firstTask.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := d.ReleaseTaskOperationToken(firstTask.ID, first.OperationToken); err != nil {
					t.Error(err)
				}
			}()
			second, err := d.CreateTask("取消事务窗口", "仅本地fixture", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := d.DeleteTaskCascade(second.ID, false, false); err != nil {
					t.Error(err)
				}
			}()
			inject.armed.Store(true)
			_, err = d.AcquireTaskOperationContext(ctx, second.ID)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("取消未返回caller错误: %v", err)
			}
			var count int
			if err := d.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, firstTask.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("其他真实owner计数=%d err=%v", count, err)
			}
			if err := d.QueryRow(`SELECT COALESCE(sum(active_count),0) FROM task_operation_activity WHERE task_id=$1`, second.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("已取消准入残留计数=%d err=%v", count, err)
			}
			taskOperationLeases.Lock()
			lease := taskOperationLeases.leases[taskOperationLeaseKey{database: d.DB, taskID: firstTask.ID}]
			taskOperationLeases.Unlock()
			if lease == nil {
				t.Fatal("其他活操作session丢失")
			}
			var held bool
			if err := lease.conn.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND mode='ShareLock' AND granted AND objid=($1::bigint & 4294967295)::oid)`, taskProxySessionKey(firstTask.ID)).Scan(&held); err != nil || !held {
				t.Fatalf("其他活操作共享锁丢失: %v", err)
			}
			if err := lease.conn.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND mode='ShareLock' AND granted AND objid=($1::bigint & 4294967295)::oid)`, taskProxySessionKey(second.ID)).Scan(&held); err != nil || held {
				t.Fatalf("已取消准入仍保留新共享锁: %v", err)
			}
		})
	}
}

func TestPendingAdmissionReclaimsAfterPoolRecoveryWithoutNewAdmissionPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	inject := &lostCommitConnector{base: stdlib.GetConnector(*cfg), committed: true}
	pool := sql.OpenDB(retryingPostgresConnector{base: inject})
	old, _ := taskOperationLockPools.Load(d.DB)
	taskOperationLockPools.Store(d.DB, pool)
	defer func() { taskOperationLockPools.Store(d.DB, old); pool.Close() }()
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.ReleaseTaskOperationToken(task.ID, first.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	foreign, err := other.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.ReleaseTaskOperationToken(task.ID, foreign.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	d.SetMaxOpenConns(1)
	held, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	failed := make([]TaskProxyState, 0, 2)
	for i := 0; i < 2; i++ {
		inject.armed.Store(true)
		state, err := d.AcquireTaskOperation(task.ID)
		if !errors.Is(err, errCommitAcknowledgmentLost) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("第%d次歧义准入错误=%v", i, err)
		}
		failed = append(failed, state)
	}
	pending := d.pendingTaskOperationReclaims()
	for _, state := range failed {
		if pending[state.OperationToken] != task.ID {
			t.Fatal("暂不可回收时丢失了精确待回收凭据")
		}
	}
	d.operationReclaims.mu.Lock()
	done := d.operationReclaims.done
	d.operationReclaims.mu.Unlock()
	if done == nil {
		t.Fatal("失败凭据没有活动协调循环")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	// 不再调用任何准入或释放；后台协调必须自行收敛，且保留两个真实PID的活引用。
	deadline := time.Now().Add(8 * time.Second)
	for {
		var count, tokens int
		if err := other.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if err := other.QueryRow(`SELECT count(*) FROM proxy_subject_activity WHERE token=$1 OR token=$2`, failed[0].OperationToken, failed[1].OperationToken).Scan(&tokens); err != nil {
			t.Fatal(err)
		}
		if count == 2 && tokens == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("恢复后没有自动精确回收: count=%d orphan_tokens=%d", count, tokens)
		}
		time.Sleep(40 * time.Millisecond)
	}
	for _, state := range failed {
		for i := 0; i < 2; i++ {
			if err := d.reclaimUnadmittedTaskOperation(task.ID, state.OperationToken); err != nil {
				t.Fatal(err)
			}
		}
	}
	var count int
	if err := other.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("重复回收扣了真实owner: count=%d err=%v", count, err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("队列排空后协调循环未退出")
	}
	if len(d.pendingTaskOperationReclaims()) != 0 {
		t.Fatal("已确认回收仍保留旧凭据")
	}
}

func TestPendingAdmissionCoordinatorCloseCancelsWaitAndDrainsPG(t *testing.T) {
	d, _, task := plannerRunTestDBs(t)
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	inject := &lostCommitConnector{base: stdlib.GetConnector(*cfg), committed: true}
	pool := sql.OpenDB(retryingPostgresConnector{base: inject})
	old, _ := taskOperationLockPools.Load(d.DB)
	taskOperationLockPools.Store(d.DB, pool)
	defer func() { taskOperationLockPools.Store(d.DB, old); pool.Close() }()
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.ReleaseTaskOperationToken(task.ID, first.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	d.SetMaxOpenConns(1)
	held, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	inject.armed.Store(true)
	failed, err := d.AcquireTaskOperation(task.ID)
	if !errors.Is(err, errCommitAcknowledgmentLost) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("歧义准入错误=%v", err)
	}
	d.operationReclaims.mu.Lock()
	done := d.operationReclaims.done
	d.operationReclaims.mu.Unlock()
	if done == nil {
		t.Fatal("没有后台协调循环")
	}
	closed := make(chan error, 1)
	go func() { closed <- d.closeTaskOperationReclaims() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭未取消并等待后台循环")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("关闭后的精确排空失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("数据库恢复后关闭排空未完成")
	}
	if len(d.pendingTaskOperationReclaims()) != 0 {
		t.Fatal("关闭未排空失败token")
	}
	if err := d.queueAndReclaimTaskOperation(task.ID, failed.OperationToken); !errors.Is(err, ErrProxySwitchRecovery) {
		t.Fatalf("关闭后仍接受新凭据: %v", err)
	}
	var count int
	if err := d.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("关闭扣了真实活owner: count=%d err=%v", count, err)
	}
}

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// 仅耗尽本审查的业务连接池，锁会话继续服务另一真实活操作。
func TestReviewCommitReclaimUnavailablePG(t *testing.T) {
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
		t.Fatalf("错误=%v", err)
	}
	held.Close()
	next, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ReleaseTaskOperationToken(task.ID, next.OperationToken); err != nil {
		t.Fatal(err)
	}
	var count int
	var orphan bool
	if err := d.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM proxy_subject_activity WHERE token=$1)`, failed.OperationToken).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	t.Logf("业务池恢复并完成下一次准入/释放后 count=%d orphan=%v；仅有 first 为真实活操作", count, orphan)
	if count != 1 || orphan {
		t.Fatalf("失败准入的精确 token 没有重试回收，仍归属健康共享会话")
	}
}

type reviewCancelConnector struct {
	base   driver.Connector
	cancel context.CancelFunc
	armed  atomic.Bool
}

func (c *reviewCancelConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *reviewCancelConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &reviewCancelConn{lostCommitConn: &lostCommitConn{underlyingDriverConn: conn}, owner: c}, nil
}

type reviewCancelConn struct {
	*lostCommitConn
	owner *reviewCancelConnector
}

func (c *reviewCancelConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.underlyingDriverConn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *reviewCancelConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.underlyingDriverConn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err == nil && strings.Contains(q, "pg_try_advisory_lock_shared") && c.owner.armed.Swap(false) {
		c.owner.cancel()
	}
	return rows, err
}
func TestReviewCancelDuringNewAdvisoryLockPG(t *testing.T) {
	d, _, firstTask := plannerRunTestDBs(t)
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inject := &reviewCancelConnector{base: stdlib.GetConnector(*cfg), cancel: cancel}
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
	second, err := d.CreateTask("独立取消边界", "仅本地审查", nil, 0, 0)
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
		t.Fatalf("取消错误=%v", err)
	}
	taskOperationLeases.Lock()
	lease := taskOperationLeases.leases[taskOperationLeaseKey{database: d.DB, taskID: firstTask.ID}]
	secondLease := taskOperationLeases.leases[taskOperationLeaseKey{database: d.DB, taskID: second.ID}]
	taskOperationLeases.Unlock()
	if lease == nil || secondLease != nil {
		t.Fatalf("原租约=%v 新租约残留=%v", lease, secondLease)
	}
	var held bool
	if err := lease.conn.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND mode='ShareLock' AND granted AND objid=($1::bigint & 4294967295)::oid)`, taskProxySessionKey(firstTask.ID)).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("另一活操作共享锁丢失")
	}
}

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"sync/atomic"
	"testing"
	"time"
)

type independentGateConnector struct {
	base             driver.Connector
	armed            atomic.Bool
	entered, proceed chan struct{}
}

func (c *independentGateConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *independentGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &independentGateConn{lostCommitConn: &lostCommitConn{underlyingDriverConn: conn}, owner: c}, nil
}

type independentGateConn struct {
	*lostCommitConn
	owner *independentGateConnector
}

func (c *independentGateConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.underlyingDriverConn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &independentGateTx{Tx: tx, owner: c.owner}, nil
}

type independentGateTx struct {
	driver.Tx
	owner *independentGateConnector
}

func (tx *independentGateTx) Commit() error {
	if !tx.owner.armed.Swap(false) {
		return tx.Tx.Commit()
	}
	close(tx.owner.entered)
	<-tx.owner.proceed
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return errCommitAcknowledgmentLost
}

// 请求在Close之前已开始；以闸门固定COMMIT与Close之间的交错，无时间猜测。
func TestIndependentCloseRacesInflightCommitPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	inject := &independentGateConnector{base: stdlib.GetConnector(*cfg), entered: make(chan struct{}), proceed: make(chan struct{})}
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
	type result struct {
		s   TaskProxyState
		err error
	}
	finished := make(chan result, 1)
	inject.armed.Store(true)
	go func() { s, err := d.AcquireTaskOperation(task.ID); finished <- result{s, err} }()
	select {
	case <-inject.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("COMMIT闸门未进入")
	}
	closeErr := d.Close()
	close(inject.proceed)
	got := <-finished
	if !errors.Is(got.err, errCommitAcknowledgmentLost) {
		t.Fatalf("COMMIT错误丢失=%v", got.err)
	}
	var count int
	var orphan bool
	if err := other.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := other.QueryRow(`SELECT EXISTS(SELECT 1 FROM proxy_subject_activity WHERE token=$1)`, got.s.OperationToken).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	t.Logf("Close先返回=%v，已启动请求后落盘但回执丢失，count=%d orphan=%v pending=%d admissionErr=%v", closeErr, count, orphan, len(d.pendingTaskOperationReclaims()), got.err)
	if closeErr == nil && (count != 1 || orphan) {
		t.Fatal("Close未等待已启动准入的token登记，成功返回但精确凭据随后被closed队列拒绝")
	}
}

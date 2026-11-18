package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var errCommitAcknowledgmentLost = errors.New("测试：COMMIT 应答丢失")

// 仅替换独占 fixture 的锁连接；真实事务提交/回滚后丢失应答。
type lostCommitConnector struct {
	base      driver.Connector
	armed     atomic.Bool
	committed bool
}

func (c *lostCommitConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *lostCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &lostCommitConn{underlyingDriverConn: conn, owner: c}, nil
}

type underlyingDriverConn interface{ driver.Conn }
type lostCommitConn struct {
	underlyingDriverConn
	owner *lostCommitConnector
}

func (c *lostCommitConn) Conn() *pgx.Conn { return c.underlyingDriverConn.(*stdlib.Conn).Conn() }
func (c *lostCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.underlyingDriverConn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lostCommitTx{Tx: tx, owner: c.owner}, nil
}
func (c *lostCommitConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.underlyingDriverConn.(driver.ExecerContext).ExecContext(ctx, q, args)
}
func (c *lostCommitConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.underlyingDriverConn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *lostCommitConn) Ping(ctx context.Context) error {
	return c.underlyingDriverConn.(driver.Pinger).Ping(ctx)
}
func (c *lostCommitConn) CheckNamedValue(v *driver.NamedValue) error {
	return c.underlyingDriverConn.(driver.NamedValueChecker).CheckNamedValue(v)
}

type lostCommitTx struct {
	driver.Tx
	owner *lostCommitConnector
}

func (tx *lostCommitTx) Commit() error {
	if !tx.owner.armed.Swap(false) {
		return tx.Tx.Commit()
	}
	var err error
	if tx.owner.committed {
		err = tx.Tx.Commit()
	} else {
		err = tx.Tx.Rollback()
	}
	if err != nil {
		return err
	}
	return errCommitAcknowledgmentLost
}

func TestTaskOperationCommitAmbiguityReclaimsOnlyOwnTokenPG(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "not-committed"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			d, _, task := plannerRunTestDBs(t)
			cfg, err := pgx.ParseConfig(testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			inject := &lostCommitConnector{base: stdlib.GetConnector(*cfg), committed: committed}
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
					t.Errorf("另一活操作不能释放: %v", err)
				}
			}()
			inject.armed.Store(true)
			failed, err := d.AcquireTaskOperation(task.ID)
			if !errors.Is(err, errCommitAcknowledgmentLost) || failed.OperationToken == "" {
				t.Fatalf("歧义错误或本次 token 丢失: %+v %v", failed, err)
			}
			var count, tokens int
			if err := d.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM proxy_subject_activity a JOIN proxy_subject_policies p ON p.id=a.policy_id WHERE p.task_id=$1`, task.ID).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if count != 1 || tokens != 1 {
				t.Fatalf("歧义准入遗留引用或扣除了活操作: count=%d tokens=%d", count, tokens)
			}
			for i := 0; i < 2; i++ {
				if err := d.reclaimUnadmittedTaskOperation(task.ID, failed.OperationToken); err != nil {
					t.Fatalf("重复精确回收不幂等: %v", err)
				}
			}
			if err := d.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("重复回收扣了活引用: count=%d err=%v", count, err)
			}
			var firstPresent bool
			if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM proxy_subject_activity WHERE token=$1)`, first.OperationToken).Scan(&firstPresent); err != nil || !firstPresent {
				t.Fatalf("另一活 token 消失: %v", err)
			}
		})
	}
}

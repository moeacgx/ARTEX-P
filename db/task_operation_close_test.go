package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func waitTaskOperationCount(t *testing.T, d *DB, taskID int64, count, tokens int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var gotCount, gotTokens int
		if err := d.QueryRow(`SELECT active_count,
(SELECT count(*) FROM proxy_subject_activity a JOIN proxy_subject_policies p ON p.id=a.policy_id WHERE p.task_id=$1)
FROM task_operation_activity WHERE task_id=$1`, taskID).Scan(&gotCount, &gotTokens); err != nil {
			t.Fatal(err)
		}
		if gotCount == count && gotTokens == tokens {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("活动未收敛 count=%d tokens=%d，预期 %d/%d", gotCount, gotTokens, count, tokens)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDBCloseFailureKeepsCoordinatorAndLiveLeasePG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() { _, _ = other.DeleteTaskCascade(task.ID, false, false) })
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.ReleaseTaskOperationToken(task.ID, first.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	failed, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟结果尚未交给业务的精确 token；不猜 owner、不改活动计数。
	if err := d.releaseTaskOperationSession(task.ID); err != nil {
		t.Fatal(err)
	}
	d.SetMaxOpenConns(1)
	held, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	d.operationReclaims.mu.Lock()
	d.operationReclaims.pending = map[string]int64{failed.OperationToken: task.ID}
	d.operationReclaims.mu.Unlock()
	d.startTaskOperationReclaims()
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	start := time.Now()
	closeErr := d.closeContext(ctx)
	cancel()
	if !errors.Is(closeErr, context.DeadlineExceeded) || !errors.Is(closeErr, ErrProxySwitchRecovery) || time.Since(start) > 2*time.Second {
		t.Fatalf("关闭失败未有界保留: %v elapsed=%v", closeErr, time.Since(start))
	}
	if len(d.pendingTaskOperationReclaims()) != 1 {
		t.Fatal("关闭失败丢失精确凭据")
	}
	if _, ok := closingTaskOperationDBs.Load(d); !ok {
		t.Fatal("关闭失败对象未登记为可达恢复所有者")
	}
	if _, ok := taskOperationLockPools.Load(d.DB); !ok {
		t.Fatal("关闭失败关闭了活会话所属池")
	}
	if _, err := d.AcquireTaskOperation(task.ID); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新准入: %v", err)
	}
	if _, err := d.AcquireProxyIOOperation(ProxySubject{Kind: "task", ID: task.ID}, 0, nil); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新 IO 准入: %v", err)
	}
	if _, _, err := d.AcquireConversationOperation(1); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新对话准入: %v", err)
	}
	if _, err := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新切换租约: %v", err)
	}
	if _, err := d.AcquirePlannerRun(t.Context(), task.ID, true); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新 Planner 租约: %v", err)
	}
	if _, err := d.AcquireFOFARequest(t.Context(), "只验证门禁", time.Second); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新 FOFA 租约: %v", err)
	}
	if err := d.AcquireTaskProxyActivity(task.ID); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新任务代理活动: %v", err)
	}
	if err := d.AcquireWarpHealthActivity(t.Context(), 1, "只验证门禁"); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭中允许新健康活动: %v", err)
	}
	rebuilt, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if err := rebuilt.RepairStaleTaskOperations(); err != nil {
		t.Fatal(err)
	}
	// 旧池仍被占满时，新对象的正式恢复也可利用原精确凭据安全续办。
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	// 不调用旧 DB 的准入、释放或 Close，验证重建对象不影响原协调者恢复。
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	if err := rebuilt.RepairStaleTaskOperations(); err != nil {
		t.Fatal(err)
	}
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, ok := closingTaskOperationDBs.Load(d); ok {
		t.Fatal("关闭成功后未注销恢复所有者")
	}
	if n := len(d.pendingTaskOperationReclaims()); n != 0 {
		t.Fatalf("重复关闭后仍有待回收=%d", n)
	}
	waitTaskOperationCount(t, other, task.ID, 1, 1)
}

type closeCommitConnector struct {
	base    driver.Connector
	armed   atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (c *closeCommitConnector) Driver() driver.Driver { return c.base.Driver() }
func (c *closeCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &closeCommitConn{lostCommitConn: &lostCommitConn{underlyingDriverConn: conn}, owner: c}, nil
}

type closeCommitConn struct {
	*lostCommitConn
	owner *closeCommitConnector
}

func (c *closeCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.underlyingDriverConn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &closeCommitTx{Tx: tx, owner: c.owner}, nil
}

type closeCommitTx struct {
	driver.Tx
	owner *closeCommitConnector
}

func (tx *closeCommitTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.owner.armed.Swap(false) {
		close(tx.owner.entered)
		<-tx.owner.resume
	}
	return err
}

func TestDBCloseWaitsForAdmissionAndKeepsExactCanceledCommitPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() { _, _ = other.DeleteTaskCascade(task.ID, false, false) })
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	inject := &closeCommitConnector{base: stdlib.GetConnector(*cfg), entered: make(chan struct{}), resume: make(chan struct{})}
	pool := sql.OpenDB(retryingPostgresConnector{base: inject})
	old, _ := taskOperationLockPools.Load(d.DB)
	taskOperationLockPools.Store(d.DB, pool)
	defer func() {
		pool.Close()
		old.(*sql.DB).Close()
	}()
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.ReleaseTaskOperationToken(task.ID, first.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	caller, cancelCaller := context.WithCancel(t.Context())
	defer cancelCaller()
	inject.armed.Store(true)
	result := make(chan error, 1)
	go func() {
		_, err := d.AcquireTaskOperationContext(caller, task.ID)
		result <- err
	}()
	select {
	case <-inject.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("未进入真实 COMMIT 后窗口")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	closeErr := d.closeContext(ctx)
	cancel()
	if !errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatalf("关闭越过尚未处理提交结果的准入: %v", closeErr)
	}
	if _, err := d.AcquireTaskOperation(task.ID); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭后仍新准入: %v", err)
	}
	cancelCaller()
	close(inject.resume)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("提交后的取消未保留: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关闭失败后原准入不能完成精确回收")
	}
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AcquireTaskOperation(task.ID); !errors.Is(err, ErrDBClosing) {
		t.Fatalf("关闭成功后仍新准入: %v", err)
	}
}

func TestDBCloseConcurrentReleaseEndsSharedBackendPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() { _, _ = other.DeleteTaskCascade(task.ID, false, false) })
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := other.QueryRow(`SELECT backend_pid FROM proxy_subject_activity WHERE token=$1`, first.OperationToken).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := d.ReleaseTaskOperationToken(task.ID, second.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	var locks int
	if err := other.QueryRow(`SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND mode='ShareLock' AND granted`, pid).Scan(&locks); err != nil || locks != 1 {
		t.Fatalf("真实操作未退出就丢锁 locks=%d err=%v", locks, err)
	}
	if err := d.ReleaseTaskOperationToken(task.ID, first.OperationToken); err != nil {
		t.Fatal(err)
	}
	waitTaskOperationCount(t, other, task.ID, 0, 0)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var exists bool
		if err := other.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("最后真实操作 Release 后旧 backend 未真实退出")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDBCloseSucceedsOnlyAfterInflightTokenReclaimedPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() { _, _ = other.DeleteTaskCascade(task.ID, false, false) })
	cfg, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	inject := &closeCommitConnector{base: stdlib.GetConnector(*cfg), entered: make(chan struct{}), resume: make(chan struct{})}
	pool := sql.OpenDB(retryingPostgresConnector{base: inject})
	old, _ := taskOperationLockPools.Load(d.DB)
	taskOperationLockPools.Store(d.DB, pool)
	defer func() { pool.Close(); old.(*sql.DB).Close() }()
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.ReleaseTaskOperationToken(task.ID, first.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	caller, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	inject.armed.Store(true)
	go func() {
		_, err := d.AcquireTaskOperationContext(caller, task.ID)
		result <- err
	}()
	select {
	case <-inject.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("未进入在途提交窗口")
	}
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	deadline := time.Now().Add(time.Second)
	for {
		d.operationLifecycle.mu.Lock()
		closing := d.operationLifecycle.closing
		d.operationLifecycle.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("关闭门禁未开启")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatalf("尚未接收提交结果就完成关闭: %v", err)
	default:
	}
	cancel()
	close(inject.resume)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("未保留提交后取消错误: %v", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("在途 token 回收后未完成关闭")
	}
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	if len(d.pendingTaskOperationReclaims()) != 0 {
		t.Fatal("关闭成功但还有未处理的精确凭据")
	}
}

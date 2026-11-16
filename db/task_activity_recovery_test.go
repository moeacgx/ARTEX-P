package db

import (
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTaskOperationLeaseReconnectsAndClearsDeadOwner(t *testing.T) {
	d, _, task := plannerRunTestDBs(t)
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ioToken, err := d.AcquireProxyIOOperation(ProxySubject{Kind: "task", ID: task.ID}, first.Revision, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ioToken == "" {
		t.Fatal("proxy I/O token missing")
	}

	key := taskOperationLeaseKey{database: d.DB, taskID: task.ID}
	taskOperationLeases.Lock()
	lease := taskOperationLeases.leases[key]
	if lease == nil {
		taskOperationLeases.Unlock()
		t.Fatal("task operation lease missing")
	}
	lease.txMu.Lock()
	var backendPID int64
	err = lease.conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&backendPID)
	lease.txMu.Unlock()
	taskOperationLeases.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	var terminated bool
	if err = d.QueryRow(`SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil {
		t.Skipf("cannot terminate test backend: %v", err)
	}
	if !terminated {
		t.Skip("test backend was already gone")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var lockAlive bool
		if err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1)`, backendPID).Scan(&lockAlive); err != nil {
			t.Fatal(err)
		}
		if !lockAlive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminated backend %d still holds locks", backendPID)
		}
		time.Sleep(25 * time.Millisecond)
	}

	second, err := d.AcquireTaskOperationContext(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("reconnect after dead lease: %v", err)
	}
	if second.OperationToken == "" || second.OperationToken == first.OperationToken {
		t.Fatalf("expected a new operation token, first=%q second=%q", first.OperationToken, second.OperationToken)
	}
	var staleActivities int
	if err = d.QueryRow(`SELECT count(*) FROM proxy_subject_activity a JOIN proxy_subject_policies p ON p.id=a.policy_id WHERE p.task_id=$1 AND a.backend_pid=$2`, task.ID, backendPID).Scan(&staleActivities); err != nil {
		t.Fatal(err)
	}
	if staleActivities != 0 {
		t.Fatalf("dead backend still owns %d proxy activities", staleActivities)
	}
	if err = d.ReleaseTaskOperationToken(task.ID, second.OperationToken); err != nil {
		t.Fatal(err)
	}
}

func seedDeadTaskOperationOwner(t *testing.T, d *DB, task *Task) string {
	t.Helper()
	const deadPID int64 = 2147483647
	deadPIDText := strconv.FormatInt(deadPID, 10)
	var policyID int64
	if err := d.QueryRow(`SELECT id FROM proxy_subject_policies WHERE task_id=$1`, task.ID).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO task_operation_activity(task_id,active_count,operation_owners)
VALUES($1,1,jsonb_build_object($2::text,1))
ON CONFLICT(task_id) DO UPDATE SET
active_count=task_operation_activity.active_count+1,
 operation_owners=task_operation_activity.operation_owners || jsonb_build_object($2::text,1)`, task.ID, deadPIDText); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, err := d.Exec(`INSERT INTO proxy_subject_activity(token,policy_id,revision,backend_pid,kind)
VALUES($1,$2,0,$3,'io')`, token, policyID, deadPID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM warp_proxy_health_activity WHERE activity_token::text=$1`, token)
		_, _ = d.Exec(`DELETE FROM proxy_subject_activity WHERE token=$1`, token)
	})
	return token
}

func TestTaskOperationReusedLeaseClearsDeadOwner(t *testing.T) {
	d, _, task := plannerRunTestDBs(t)
	first, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.ReleaseTaskOperationToken(task.ID, first.OperationToken) }()
	staleToken := seedDeadTaskOperationOwner(t, d, task)

	second, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatalf("reused operation lease rejected after stale-owner repair: %v", err)
	}
	defer func() { _ = d.ReleaseTaskOperationToken(task.ID, second.OperationToken) }()

	var ownerCount, staleActivityCount int
	if err = d.QueryRow(`SELECT
COALESCE((SELECT count(*) FROM jsonb_each_text(operation_owners) WHERE key='2147483647'),0),
COALESCE((SELECT count(*) FROM proxy_subject_activity WHERE token=$2),0)
FROM task_operation_activity WHERE task_id=$1`, task.ID, staleToken).Scan(&ownerCount, &staleActivityCount); err != nil {
		t.Fatal(err)
	}
	if ownerCount != 0 || staleActivityCount != 0 {
		t.Fatalf("reused lease kept dead operation owner/activity: owner=%d activity=%d", ownerCount, staleActivityCount)
	}
}

func TestRepairStaleTaskOperationsAtStartup(t *testing.T) {
	d, _, task := plannerRunTestDBs(t)
	seedDeadTaskOperationOwner(t, d, task)
	if err := d.RepairStaleTaskOperations(); err != nil {
		t.Fatal(err)
	}
	var activeCount, ownerCount, activityCount int
	if err := d.QueryRow(`SELECT
COALESCE((SELECT active_count FROM task_operation_activity WHERE task_id=$1),0),
COALESCE((SELECT count(*) FROM task_operation_activity a,jsonb_each_text(a.operation_owners) WHERE a.task_id=$1 AND key='2147483647'),0),
(SELECT count(*) FROM proxy_subject_activity a JOIN proxy_subject_policies p ON p.id=a.policy_id WHERE p.task_id=$1 AND a.backend_pid=2147483647)`, task.ID).Scan(&activeCount, &ownerCount, &activityCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 0 || ownerCount != 0 || activityCount != 0 {
		t.Fatalf("startup repair left stale operation state: active=%d owner=%d activity=%d", activeCount, ownerCount, activityCount)
	}
}

func TestTaskActivityRecoveryReleasesOnlyOwnedWarpTokens(t *testing.T) {
	first, _, task := plannerRunTestDBs(t)
	if err := first.EnsureProxyActivitySchema(); err != nil {
		t.Fatal(err)
	}
	if err := first.MigrateWarpHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	proxyID := healthProxy(t, first)
	t.Cleanup(func() { _, _ = first.Exec(`DELETE FROM outbound_proxies WHERE id=$1`, proxyID) })
	other, err := first.CreateTask("其他任务引用", "保留其他任务占用", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = first.DeleteTaskCascade(other.ID, false, false) })
	seed := func(id int64, kinds ...string) {
		t.Helper()
		policy, err := first.GetProxyPolicy(ProxySubject{Kind: "task", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range kinds {
			token := uuid.NewString()
			if _, err := first.Exec(`INSERT INTO proxy_subject_activity(token,policy_id,revision,proxy_id,backend_pid,kind) VALUES($1,$2,0,$3,2147483647,$4)`, token, policy.ID, proxyID, kind); err != nil {
				t.Fatal(err)
			}
			if _, err := first.Exec(`INSERT INTO warp_proxy_health_activity(activity_token,proxy_id) VALUES($1,$2)`, token, proxyID); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(task.ID, "task", "io")
	seed(other.ID, "io")
	if _, err := first.Exec(`INSERT INTO task_operation_activity(task_id,active_count,operation_owners) VALUES($1,1,'{"2147483647":1}')`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(`INSERT INTO task_proxy_activity(task_id,proxy_id,active_count) VALUES($1,$2,1)`, task.ID, proxyID); err != nil {
		t.Fatal(err)
	}
	clear := func(id int64) {
		t.Helper()
		executor, err := first.AcquireTaskProxySwitchExecutor(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		defer executor.Close()
		if err := executor.ClearStaleTaskOperations(); err != nil {
			t.Fatal(err)
		}
	}
	clear(task.ID)
	var tokens, taskRefs, proxyRefs int
	if err := first.QueryRow(`SELECT (SELECT count(*) FROM warp_proxy_health_activity WHERE proxy_id=$1),
		(SELECT active_count FROM task_operation_activity WHERE task_id=$2),
		(SELECT count(*) FROM task_proxy_activity WHERE task_id=$2)`, proxyID, task.ID).Scan(&tokens, &taskRefs, &proxyRefs); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 || taskRefs != 0 || proxyRefs != 0 {
		t.Fatalf("恢复未精确释放本任务引用：WARP=%d，操作=%d，代理=%d", tokens, taskRefs, proxyRefs)
	}
	checkIdle := func() error {
		return first.WithWarpHealthLifecycle(t.Context(), func(tx *sql.Tx) error {
			return WarpHealthIdleTx(t.Context(), tx, proxyID)
		})
	}
	if err := checkIdle(); !errors.Is(err, ErrWarpHealthBlocked) {
		t.Fatalf("其他任务的活动未继续保护 WARP：%v", err)
	}
	clear(other.ID)
	if err := checkIdle(); err != nil {
		t.Fatalf("正式恢复后 WARP 仍被幽灵引用阻塞：%v", err)
	}
	if row, err := first.GetTask(task.ID); err != nil || row == nil || row.Status != task.Status || row.Paused != task.Paused || row.ProxyRevision != task.ProxyRevision {
		t.Fatalf("引用恢复改变了任务控制状态或出口版本：%+v，%v", row, err)
	}
}

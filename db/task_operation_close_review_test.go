package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 只在本审查 fixture 上验证 Close 后公开准入仍可使用已借出的共享连接。
func TestIndependentAcquireAfterDBClosePG(t *testing.T) {
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
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := d.AcquireTaskOperation(task.ID)
	if err == nil {
		if releaseErr := d.ReleaseTaskOperationToken(task.ID, next.OperationToken); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	var count int
	if e := other.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); e != nil {
		t.Fatal(e)
	}
	t.Logf("DB.Close已返回成功，随后Acquire错误=%v，另一原活操作计数=%d", err, count)
	if err == nil {
		t.Fatal("DB.Close之后仍允许新的业务准入")
	}
}

// token 来自真实准入，但模拟业务尚未收到结果；只持有精确凭据，不猜测或清零活动。
func TestIndependentCloseFailureRebuildLiveOwnerPG(t *testing.T) {
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
	notAdmitted, err := d.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.releaseTaskOperationSession(task.ID); err != nil {
		t.Fatal(err)
	}
	d.operationReclaims.mu.Lock()
	d.operationReclaims.pending = map[string]int64{notAdmitted.OperationToken: task.ID}
	d.operationReclaims.mu.Unlock()
	d.SetMaxOpenConns(1)
	held, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	start := time.Now()
	closeErr := d.Close()
	elapsed := time.Since(start)
	held.Close()
	if !errors.Is(closeErr, ErrProxySwitchRecovery) {
		t.Fatalf("关闭未显式报告待回收: %v", closeErr)
	}
	if elapsed > 23*time.Second {
		t.Errorf("Close超过20秒期限宽限: %v", elapsed)
	}
	rebuilt, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if err := rebuilt.RepairStaleTaskOperations(); err != nil {
		t.Fatal(err)
	}
	var count int
	var orphan, live bool
	if err := other.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := other.QueryRow(`SELECT EXISTS(SELECT 1 FROM proxy_subject_activity WHERE token=$1),EXISTS(SELECT 1 FROM proxy_subject_activity a JOIN pg_locks l ON a.backend_pid=l.pid WHERE a.token=$1 AND l.locktype='advisory' AND l.mode='ShareLock' AND l.granted)`, notAdmitted.OperationToken).Scan(&orphan, &live); err != nil {
		t.Fatal(err)
	}
	// 旧对象的正式Close也已不可重试排空，因为底层业务池已永久关闭。
	retryErr := d.Close()
	t.Logf("首次Close=%v，用时=%v；重建后count=%d orphan=%v owner锁活=%v；再次Close=%v", closeErr, elapsed, count, orphan, live, retryErr)
	if count != 1 || orphan {
		t.Fatal("内存队列所属DB已关闭，重建DB的dead-owner证明无法精确回收仍与真实操作共用活owner的失败token")
	}
}

// 压力覆盖空队列退出附近再入队，以及同步/短重试/后台同时精确回收。
func TestIndependentQueueExitAndConcurrentReclaimPG(t *testing.T) {
	d, _, task := plannerRunTestDBs(t)
	for round := 0; round < 4; round++ {
		states := make([]TaskProxyState, 4)
		for i := range states {
			var err error
			states[i], err = d.AcquireTaskOperation(task.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		d.operationReclaims.mu.Lock()
		if d.operationReclaims.pending == nil {
			d.operationReclaims.pending = map[string]int64{}
		}
		for _, s := range states {
			d.operationReclaims.pending[s.OperationToken] = task.ID
		}
		d.operationReclaims.mu.Unlock()
		d.startTaskOperationReclaims()
		var wg sync.WaitGroup
		failures := make(chan error, 16)
		for _, s := range states {
			for repeat := 0; repeat < 2; repeat++ {
				wg.Add(1)
				go func(token string) {
					defer wg.Done()
					if err := d.reclaimQueuedTaskOperation(t.Context(), task.ID, token); err != nil {
						failures <- err
					}
				}(s.OperationToken)
			}
		}
		wg.Add(1)
		go func() { defer wg.Done(); d.retryTaskOperationReclaimsBriefly(context.Background()) }()
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		for range states {
			if err := d.releaseTaskOperationSession(task.ID); err != nil {
				t.Error(err)
			}
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			d.operationReclaims.mu.Lock()
			done := d.operationReclaims.done
			d.operationReclaims.mu.Unlock()
			if done == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("空队列循环未退出")
			}
			time.Sleep(time.Millisecond)
		}
		if n := len(d.pendingTaskOperationReclaims()); n != 0 {
			t.Fatal(fmt.Sprint("队列未空", n))
		}
	}
	if err := d.closeTaskOperationReclaims(); err != nil {
		t.Fatal(err)
	}
}

// 子进程真实退出让 PostgreSQL 关闭其后台；不通过强清活动或终止其他会话伪造证明。
func init() {
	// 子进程在 TestMain 的整库测试锁之前执行，避免等待父进程自己持有的锁。
	// 仅测试私有环境变量触发；进程本身真实退出，不能用删引用模拟死亡。
	raw := os.Getenv("ARTEX_PR71_CLOSE_CHILD_TASK")
	if raw == "" {
		return
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		fail(err)
	}
	dsn, _, err := DSN()
	if err != nil {
		fail(err)
	}
	d, err := Open(dsn)
	if err != nil {
		fail(err)
	}
	state, err := d.AcquireTaskOperation(id)
	if err != nil {
		fail(err)
	}
	d.operationReclaims.mu.Lock()
	d.operationReclaims.pending = map[string]int64{state.OperationToken: id}
	d.operationReclaims.mu.Unlock()
	fmt.Println("子进程已保留精确token，立即退出且不执行Close")
	os.Exit(0)
}

func TestIndependentProcessExitDeadOwnerRecoveryPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	real, err := other.AcquireTaskOperation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.ReleaseTaskOperationToken(task.ID, real.OperationToken); err != nil {
			t.Error(err)
		}
	}()
	childCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestIndependentProcessExitDeadOwnerRecoveryPG$", "-test.timeout=20s", "-test.v")
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), fmt.Sprintf("ARTEX_PR71_CLOSE_CHILD_TASK=%d", task.ID))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子进程错误=%v output=%s", err, output)
	}
	t.Log(string(output))
	if err := d.RepairStaleTaskOperations(); err != nil {
		t.Fatal(err)
	}
	var count, tokens int
	if err := d.QueryRow(`SELECT active_count FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM proxy_subject_activity a JOIN proxy_subject_policies p ON p.id=a.policy_id WHERE p.task_id=$1`, task.ID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	t.Logf("真实子进程退出后安全修复 count=%d tokens=%d，另一个session真实操作保留", count, tokens)
	if count != 1 || tokens != 1 {
		t.Fatal("dead-owner未能安全收敛")
	}
}

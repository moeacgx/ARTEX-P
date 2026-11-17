package db

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 已借到的 executor 只能释放，关闭后不能经其连接新增持久活动。
func TestRound3OldExecutorAdmissionAfterClosePG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	executor, err := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	_, token, err := executor.Begin(0, TaskProxyModeDirect, nil)
	var persisted string
	if e := other.QueryRow(`SELECT coalesce((SELECT switch_token FROM task_operation_activity WHERE task_id=$1),'')`, task.ID).Scan(&persisted); e != nil {
		t.Fatal(e)
	}
	t.Logf("Close成功后旧executor新切换=%v token=%q 持久token=%q", err, token, persisted)
	if err == nil {
		if _, e := executor.AbortToDirect(0, token); e != nil {
			t.Error(e)
		}
		t.Fatal("旧executor绕过关闭门禁开始新切换并提交屏障")
	}
	if !errors.Is(err, ErrDBClosing) {
		t.Errorf("期望关闭错误，实际=%v", err)
	}
	if token != "" || persisted != "" {
		t.Fatalf("关闭拒绝仍返回或持久化了切换凭据")
	}
}

func TestRound3OldPolicyExecutorBeginAfterClosePG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	if e := d.SetPaused(task.ID, true); e != nil {
		t.Fatal(e)
	}
	subject := ProxySubject{Kind: "task", ID: task.ID}
	policy, e := d.GetProxyPolicy(subject)
	if e != nil {
		t.Fatal(e)
	}
	x, e := d.AcquireProxyPolicyExecutor(t.Context(), subject)
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	_, token, e := x.Begin(policy.State.Revision, ProxyPolicyPatch{Mode: TaskProxyModeDirect})
	var persisted string
	if err := other.QueryRow(`SELECT switch_token FROM proxy_subject_policies WHERE id=$1`, policy.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	t.Logf("Close后policy.Begin错误=%v token存在=%v 持久token匹配=%v", e, token != "", persisted == token)
	if e == nil {
		if _, err := x.Complete(policy.State.Revision, token); err != nil {
			t.Error(err)
		}
		t.Fatal("旧policy executor关闭后新增持久切换")
	}
	if !errors.Is(e, ErrDBClosing) {
		t.Errorf("错误=%v", e)
	}
	if token != "" || persisted != "" {
		t.Fatalf("关闭拒绝仍返回或持久化了策略切换凭据")
	}
	if _, _, err := x.BeginMigration(policy.State.Revision, 1, 2); !errors.Is(err, ErrDBClosing) {
		t.Errorf("策略迁移绕过关闭门禁: %v", err)
	}
}

func TestRound3OldExecutorIdleAfterClosePG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	oldPath := proxySecretsPath
	t.Cleanup(func() { proxySecretsPath = oldPath })
	if err := ConfigureProxySecrets(filepath.Join(t.TempDir(), "proxy.key")); err != nil {
		t.Fatal(err)
	}
	proxy, e := d.CreateProxy(&Proxy{Name: "独立审查合成代理", Kind: ProxyKindHTTP, URL: "http://127.0.0.1:19", Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, err := other.DeleteTaskCascade(task.ID, false, false); err != nil {
			t.Error(err)
		}
		if _, err := other.Exec(`DELETE FROM outbound_proxies WHERE id=$1`, proxy.ID); err != nil {
			t.Error(err)
		}
	})
	snapshot, e := proxy.SnapshotEncrypted()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	// 只配置本任务的合成固定代理，不发起任何网络请求。
	if _, e = d.Exec(`UPDATE tasks SET proxy_mode=$2,proxy_id=$3,proxy_snapshot=$4 WHERE id=$1`, task.ID, TaskProxyModeFixed, proxy.ID, raw); e != nil {
		t.Fatal(e)
	}
	x, e := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	e = x.AcquireIdleTaskProxyActivity()
	var count int
	if err := other.QueryRow(`SELECT coalesce((SELECT active_count FROM task_proxy_activity WHERE task_id=$1),0)`, task.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	t.Logf("Close后AcquireIdle错误=%v 持久活动=%d", e, count)
	if e == nil {
		if err := x.ReleaseTaskProxyActivity(); err != nil {
			t.Error(err)
		}
		t.Fatal("旧executor关闭后新增持久代理引用")
	}
	if !errors.Is(e, ErrDBClosing) {
		t.Errorf("错误=%v", e)
	}
	if count != 0 {
		t.Fatalf("关闭拒绝仍持久化了代理引用: %d", count)
	}
	if err := x.AcquireIdleTaskProxyActivityRevision(0); !errors.Is(err, ErrDBClosing) {
		t.Errorf("带版本的独占引用绕过关闭门禁: %v", err)
	}
}

func TestRound3ClosedGateNoInflightLeakPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	if e := d.Close(); e != nil {
		t.Fatal(e)
	}
	probes := []struct {
		name string
		call func() error
	}{
		{"task", func() error { _, e := d.AcquireTaskOperation(task.ID); return e }},
		{"planner", func() error { _, e := d.AcquirePlannerRun(t.Context(), task.ID, true); return e }},
		{"fofa", func() error { _, e := d.AcquireFOFARequest(t.Context(), "review-only", time.Second); return e }},
		{"io", func() error {
			_, e := d.AcquireProxyIOOperation(ProxySubject{Kind: "task", ID: task.ID}, 0, nil)
			return e
		}},
		{"conversation", func() error { _, _, e := d.AcquireConversationOperation(1); return e }},
		{"switch", func() error { _, e := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID); return e }},
		{"switch_begin", func() error { _, _, e := d.BeginTaskProxySwitch(task.ID, 0, TaskProxyModeDirect, nil); return e }},
		{"activity", func() error { return d.AcquireTaskProxyActivity(task.ID) }},
		{"health", func() error { return d.AcquireWarpHealthActivity(context.Background(), 1, "review-only") }},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			if e := p.call(); !errors.Is(e, ErrDBClosing) {
				t.Errorf("错误=%v", e)
			}
		})
	}
	d.operationLifecycle.mu.Lock()
	inflight := d.operationLifecycle.inflight
	d.operationLifecycle.mu.Unlock()
	if inflight != 0 {
		t.Fatalf("关闭拒绝导致计数泄漏=%d", inflight)
	}
}

func TestRound3BeginRacesClosePG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	if e := d.SetPaused(task.ID, true); e != nil {
		t.Fatal(e)
	}
	x, e := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	var pid int
	if e := x.conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
		t.Fatal(e)
	}
	blocker, e := other.BeginTx(t.Context(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if e := lockProxyLifecycle(blocker); e != nil {
		t.Fatal(e)
	}
	type result struct {
		token string
		err   error
	}
	resultCh := make(chan result, 1)
	go func() { _, token, e := x.Begin(0, TaskProxyModeDirect, nil); resultCh <- result{token, e} }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var blocked bool
		if e := other.QueryRow(`SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("未观察到旧executor Begin等待真实事务锁")
		}
		time.Sleep(time.Millisecond)
	}
	closeErr := d.Close()
	if e := blocker.Rollback(); e != nil {
		t.Fatal(e)
	}
	got := <-resultCh
	var persisted string
	if e := other.QueryRow(`SELECT coalesce((SELECT switch_token FROM task_operation_activity WHERE task_id=$1),'')`, task.ID).Scan(&persisted); e != nil {
		t.Fatal(e)
	}
	t.Logf("Begin先等待锁，Close=%v，随后Begin=%v 持久token匹配=%v", closeErr, got.err, persisted != "" && persisted == got.token)
	if got.err == nil {
		if _, e := x.AbortToDirect(0, got.token); e != nil {
			t.Error(e)
		}
	}
	if closeErr == nil && got.err == nil {
		t.Fatal("Close未计入旧executor已开始的Begin，关闭成功后仍落盘")
	}
}

func TestRound3ExistingSwitchCompleteAfterClosePG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	if e := d.SetPaused(task.ID, true); e != nil {
		t.Fatal(e)
	}
	x, e := d.AcquireTaskProxySwitchExecutor(t.Context(), task.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer x.Close()
	_, token, e := x.Begin(0, TaskProxyModeDirect, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if _, e = x.Complete(0, token); e != nil {
		t.Fatal(e)
	}
	var pending string
	if e = other.QueryRow(`SELECT switch_token FROM task_operation_activity WHERE task_id=$1`, task.ID).Scan(&pending); e != nil {
		t.Fatal(e)
	}
	if pending != "" {
		t.Fatal("已开始切换未完成")
	}
}

func TestRound3RecoveryIdentityAndConcurrentReclaimPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	live, e := d.AcquireTaskOperation(task.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := d.ReleaseTaskOperationToken(task.ID, live.OperationToken); e != nil {
			t.Error(e)
		}
	}()
	failed, e := d.AcquireTaskOperation(task.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.releaseTaskOperationSession(task.ID); e != nil {
		t.Fatal(e)
	}
	d.operationReclaims.mu.Lock()
	d.operationReclaims.pending = map[string]int64{failed.OperationToken: task.ID}
	d.operationReclaims.mu.Unlock()
	d.SetMaxOpenConns(1)
	held, e := d.Conn(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	closeErr := d.closeContext(ctx)
	cancel()
	if !errors.Is(closeErr, ErrProxySwitchRecovery) {
		t.Fatalf("关闭未失败=%v", closeErr)
	}
	different, e := Open(testDSN(t) + "&application_name=review-different-identity")
	if e != nil {
		t.Fatal(e)
	}
	defer different.Close()
	if e = different.RepairStaleTaskOperations(); e != nil {
		t.Fatal(e)
	}
	if len(d.pendingTaskOperationReclaims()) != 1 {
		t.Fatal("不同配置错误匹配精确恢复owner")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := other.RepairStaleTaskOperations(); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if len(d.pendingTaskOperationReclaims()) != 0 {
		t.Fatal("相同Open配置跨对象未回收")
	}
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	held.Close()
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	if _, ok := closingTaskOperationDBs.Load(d); ok {
		t.Fatal("Close成功仍在恢复集合")
	}
}

func TestRound3RejectedCanceledAfterCloseNoNewTokenPG(t *testing.T) {
	d, other, task := plannerRunTestDBs(t)
	t.Cleanup(func() {
		if _, e := other.DeleteTaskCascade(task.ID, false, false); e != nil {
			t.Error(e)
		}
	})
	live, e := d.AcquireTaskOperation(task.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := d.ReleaseTaskOperationToken(task.ID, live.OperationToken); e != nil {
			t.Error(e)
		}
	}()
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, e := d.AcquireTaskOperationContext(ctx, task.ID)
	if !errors.Is(e, ErrDBClosing) || s.OperationToken != "" {
		t.Fatalf("未提前拒绝=%v token存在=%v", e, s.OperationToken != "")
	}
	waitTaskOperationCount(t, other, task.ID, 1, 1)
	if len(d.pendingTaskOperationReclaims()) != 0 {
		t.Fatal("拒绝后新增pending")
	}
	var locked bool
	if e := other.QueryRow(`SELECT EXISTS(SELECT 1 FROM proxy_subject_activity a JOIN pg_locks l ON l.pid=a.backend_pid WHERE a.token=$1 AND l.mode='ShareLock' AND l.granted)`, live.OperationToken).Scan(&locked); e != nil || !locked {
		t.Fatalf("旧锁丢失=%v %v", locked, e)
	}
}

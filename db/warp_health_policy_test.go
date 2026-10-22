package db

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func globalWarpPolicyFixture(t *testing.T) *DB {
	t.Helper()
	d := catalogDBFixture(t)
	if err := d.EnsureProxyActivitySchema(); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateWarpCreateRequests(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateWarpHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGlobalWarpPolicyMigrationAndNewNodes(t *testing.T) {
	d := globalWarpPolicyFixture(t)
	a, b := healthProxy(t, d), healthProxy(t, d)
	healthIntent(t, d, a)
	healthIntent(t, d, b)
	// 模拟升级前节点设置不同；重建仅位于本测试隔离 schema 的新全局表。
	if _, err := d.Exec(`UPDATE warp_proxy_health SET auto_recover_enabled=(proxy_id=$1)`, a); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DROP TABLE warp_health_policy`); err != nil {
		t.Fatal(err)
	}
	if err := d.MigrateWarpHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, err := d.GlobalWarpHealthPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.AutoRecoverEnabled || p.MigratedDisabledNodes != 1 || p.Revision != 1 {
		t.Fatalf("迁移未保守关闭：%+v", p)
	}
	for _, id := range []int64{a, b} {
		s, err := d.WarpHealth(t.Context(), id)
		if err != nil || s.AutoRecoverEnabled {
			t.Fatalf("旧节点绕过全局关闭：%+v %v", s, err)
		}
	}
	p, err = d.SetGlobalWarpHealthPolicy(t.Context(), p.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	c := healthProxy(t, d)
	healthIntent(t, d, c)
	for _, id := range []int64{a, b, c} {
		s, err := d.WarpHealth(t.Context(), id)
		if err != nil || !s.AutoRecoverEnabled {
			t.Fatalf("旧字段或新节点未服从全局：%+v %v", s, err)
		}
	}
	// 再次启动迁移不使用旧节点 disabled 覆盖已保存策略。
	if err = d.MigrateWarpHealth(t.Context()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := d.GlobalWarpHealthPolicy(t.Context())
	if err != nil || reloaded != p {
		t.Fatalf("重启迁移覆盖全局设置：%+v %v", reloaded, err)
	}
	// 旧节点关闭位不能再成为全局开启后的隐形恢复门槛。
	now := time.Now()
	var previous WarpHealthClaim
	for i := 0; i < 3; i++ {
		previous, err = d.BeginWarpHealthProbe(t.Context(), b, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.CompleteWarpHealthProbe(t.Context(), previous, "", false, now, DefaultWarpHealthPolicy()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	if _, err = d.BeginWarpHealthRepair(t.Context(), previous, now, DefaultWarpHealthPolicy()); err != nil {
		t.Fatalf("旧节点关闭位阻止了全局恢复：%v", err)
	}
}

func TestGlobalWarpPolicyCASAndClosedProbe(t *testing.T) {
	d := globalWarpPolicyFixture(t)
	p, err := d.GlobalWarpHealthPolicy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := d.SetGlobalWarpHealthPolicy(t.Context(), p.Revision, false)
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	good, stale := 0, 0
	for err := range results {
		if err == nil {
			good++
		} else if errors.Is(err, ErrWarpHealthStale) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if good != 1 || stale != 1 {
		t.Fatalf("CAS接受数=%d 冲突数=%d", good, stale)
	}
	id := healthProxy(t, d)
	healthIntent(t, d, id)
	before, _ := d.WarpHealth(t.Context(), id)
	claim, err := d.BeginWarpHealthProbe(t.Context(), id, time.Now())
	if err != nil {
		t.Fatalf("全局关闭不应停探测：%v", err)
	}
	if _, err = d.CompleteWarpHealthProbe(t.Context(), claim, "8.8.8.8", true, time.Now(), DefaultWarpHealthPolicy()); err != nil {
		t.Fatal(err)
	}
	after, _ := d.WarpHealth(t.Context(), id)
	if after.AutoRecoverEnabled || !after.Desired || after.Revision != before.Revision {
		t.Fatalf("全局策略误改节点运行意图：%+v", after)
	}
	err = d.WithWarpHealthLifecycle(t.Context(), func(tx *sql.Tx) error { return CheckWarpHealthAutoRecoveryEligibleTx(t.Context(), tx, id) })
	if !errors.Is(err, ErrWarpHealthBlocked) {
		t.Fatalf("全局关闭仍允许自动恢复：%v", err)
	}
	if _, err = d.SetGlobalWarpHealthPolicy(t.Context(), 0, true); !errors.Is(err, ErrWarpHealthStale) {
		t.Fatal("非法版本未拒绝")
	}
}

func TestGlobalWarpPolicyCannotClearUnknownOrResourceReferences(t *testing.T) {
	d := globalWarpPolicyFixture(t)
	id := healthProxy(t, d)
	healthIntent(t, d, id)
	claim, err := d.BeginWarpHealthProbe(t.Context(), id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = d.MarkWarpHealthUnknown(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	p, _ := d.GlobalWarpHealthPolicy(t.Context())
	p, err = d.SetGlobalWarpHealthPolicy(t.Context(), p.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.SetGlobalWarpHealthPolicy(t.Context(), p.Revision, true); err != nil {
		t.Fatal(err)
	}
	s, _ := d.WarpHealth(t.Context(), id)
	if s.Claim.Token != claim.Token || s.ClaimState != "unknown" || !s.Desired {
		t.Fatalf("开关绕过未知执行屏障：%+v", s)
	}
	if _, err = d.BeginWarpHealthProbe(t.Context(), id, time.Now().Add(time.Hour)); !errors.Is(err, ErrWarpHealthBlocked) {
		t.Fatalf("未知claim仍被抢占：%v", err)
	}
	// 没有未知执行的另一个节点仍必须通过真实资源空闲检查。
	other := healthProxy(t, d)
	healthIntent(t, d, other)
	token := "00000000-0000-4000-8000-000000000001"
	if err = d.AcquireWarpHealthActivity(t.Context(), other, token); err != nil {
		t.Fatal(err)
	}
	err = d.WithWarpHealthLifecycle(t.Context(), func(tx *sql.Tx) error { return WarpHealthIdleTx(t.Context(), tx, other) })
	if !errors.Is(err, ErrWarpHealthBlocked) {
		t.Fatalf("开关清除了资源占用：%v", err)
	}
}

func TestGlobalWarpPolicyMissingNeverDefaultsToEnabled(t *testing.T) {
	d := globalWarpPolicyFixture(t)
	id := healthProxy(t, d)
	healthIntent(t, d, id)
	if _, err := d.Exec(`DELETE FROM warp_health_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GlobalWarpHealthPolicy(t.Context()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("缺少策略应明确失败：%v", err)
	}
	err := d.WithWarpHealthLifecycle(t.Context(), func(tx *sql.Tx) error { return CheckWarpHealthAutoRecoveryEligibleTx(t.Context(), tx, id) })
	if err == nil {
		t.Fatal("策略读取失败时仍允许自动恢复")
	}
	if _, err = d.BeginWarpHealthProbe(t.Context(), id, time.Now()); err == nil {
		t.Fatal("缺少策略时未拒绝未知状态")
	}
}

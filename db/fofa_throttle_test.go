package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFOFAThrottleSerializesAndCancelsWaiters(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	first, err := d.AcquireFOFARequest(t.Context(), "shared-synthetic-key", 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Finish(0)
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	if second, err := d.AcquireFOFARequest(ctx, "shared-synthetic-key", time.Millisecond); !errors.Is(err, context.DeadlineExceeded) || second != nil {
		t.Fatalf("同Key并发请求没有等待或取消: lease=%v err=%v", second, err)
	}
	other, err := d.AcquireFOFARequest(t.Context(), "different-synthetic-key", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Finish(0); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(0); err != nil {
		t.Fatal(err)
	}
	if err := first.Finish(time.Hour); err != nil {
		t.Fatal("重复完成必须幂等")
	}
}

func TestFOFAThrottlePersistsSpacingAndCooldown(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	const key = "synthetic-key-never-in-persistent-state"
	first, err := d.AcquireFOFARequest(t.Context(), key, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := first.Finish(0); err != nil {
		t.Fatal(err)
	}
	// 新建门禁调用实例，不能靠进程内客户端缓存来维持间隔和冷却。
	reopened := &DB{DB: d.DB}
	defer reopened.closeFOFAThrottlePool()
	second, err := reopened.AcquireFOFARequest(t.Context(), key, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Fatal("重新创建客户端绕过了请求间隔")
	}
	if err := second.Finish(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var limited *FOFACooldownError
	if lease, err := d.AcquireFOFARequest(t.Context(), key, time.Millisecond); lease != nil || !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Fatalf("共享冷却未阻止新请求: lease=%v err=%v", lease, err)
	}
	var stored string
	if err := d.QueryRow(`SELECT string_agg(key||value,'') FROM settings`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, key) {
		t.Fatal("节流状态泄露了Key")
	}
	time.Sleep(320 * time.Millisecond)
	third, err := d.AcquireFOFARequest(t.Context(), key, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Finish(0); err != nil {
		t.Fatal(err)
	}
}

func TestFOFAThrottleRejectsCorruptStateAndReleasesLock(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key := "synthetic-corrupt-state"
	if _, err := d.Exec(`INSERT INTO settings(key,value) VALUES($1,'not-json')`, fofaThrottleScope(key)); err != nil {
		t.Fatal(err)
	}
	if lease, err := d.AcquireFOFARequest(t.Context(), key, time.Millisecond); lease != nil || !errors.Is(err, ErrFOFAThrottle) {
		t.Fatalf("损坏状态被放行: %v", err)
	}
	if _, err := d.FOFARequestCooldown(t.Context(), key); !errors.Is(err, ErrFOFAThrottle) {
		t.Fatalf("损坏状态被报告为无冷却: %v", err)
	}
	if _, err := d.Exec(`UPDATE settings SET value='{}' WHERE key=$1`, fofaThrottleScope(key)); err != nil {
		t.Fatal(err)
	}
	lease, err := d.AcquireFOFARequest(t.Context(), key, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Finish(0); err != nil {
		t.Fatal(err)
	}
}

func TestFOFAThrottleLostConnectionCannotLeavePooledLock(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	key := "synthetic-lost-connection"
	lease, err := d.AcquireFOFARequest(t.Context(), key, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := lease.conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	if err := lease.Finish(time.Minute); err != nil {
		t.Fatalf("新连接未补写冷却: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var limited *FOFACooldownError
	next, err := d.AcquireFOFARequest(ctx, key, time.Millisecond)
	if next != nil || !errors.As(err, &limited) || limited.RetryAfter < 50*time.Second {
		t.Fatalf("断连后绕过已补写的冷却: %v", err)
	}
}

func TestFOFAThrottleFailedSettlementRemainsBlocked(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	lease, err := d.AcquireFOFARequest(t.Context(), "synthetic-write-failed", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE FUNCTION reject_fofa_settle() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF COALESCE(NEW.value::jsonb->>'pending_token','')='' THEN RAISE EXCEPTION 'synthetic settlement failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_fofa_settle BEFORE UPDATE ON settings FOR EACH ROW EXECUTE FUNCTION reject_fofa_settle()`); err != nil {
		t.Fatal(err)
	}
	if err := lease.Finish(time.Minute); !errors.Is(err, ErrFOFAThrottleUnsettled) {
		t.Fatalf("未上报未结算状态: %v", err)
	}
	if next, err := d.AcquireFOFARequest(t.Context(), "synthetic-write-failed", time.Millisecond); next != nil || !errors.Is(err, ErrFOFAThrottleUnsettled) {
		t.Fatalf("冷却保存失败后继续放行: %v", err)
	}
}

func TestFOFAThrottleDedicatedPoolPreservesBusinessConnection(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	d.SetMaxOpenConns(1)
	lease, err := d.AcquireFOFARequest(t.Context(), "synthetic-one-business-connection", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Finish(0)
	if _, err := d.FOFARequestCooldown(t.Context(), "synthetic-one-business-connection"); !errors.Is(err, ErrFOFARequestPending) {
		t.Fatalf("正常执行被误报为未结算故障: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var value int
	if err := d.QueryRowContext(ctx, `SELECT 1`).Scan(&value); err != nil || value != 1 {
		t.Fatalf("请求许可占满业务连接池: %v", err)
	}
}

func TestFOFAThrottleCloseDoesNotRecreatePool(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	lease, err := d.AcquireFOFARequest(t.Context(), "synthetic-close-pool", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Finish(0); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if lease, err := d.AcquireFOFARequest(t.Context(), "synthetic-close-pool", time.Millisecond); lease != nil || err == nil {
		t.Fatal("关闭后重新创建了许可池")
	}
}

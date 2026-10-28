package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/egress"
)

// 运行器边界不暴露注册命令，健康控制器无法重新注册或生成材料。
type WarpHealthRuntime interface {
	HealthMaterial(string) (egress.WarpProfile, string, error)
	Get(string) (egress.WarpProfile, error)
	Probe(context.Context, string) (egress.WarpProfile, error)
	Reconnect(context.Context, string, string) (egress.WarpProfile, error)
	ConfirmStopped(context.Context, string) error
}

var _ WarpHealthRuntime = (*egress.WarpManager)(nil)

// 即使数据库连接中断，当前宿主内仍以真实函数退出作为判据。
var warpHealthExecutions sync.Map

type WarpHealthDTO struct {
	ProxyID             string     `json:"proxy_id"`
	DesiredRunning      bool       `json:"desired_running"`
	AutoRecoverEnabled  bool       `json:"auto_recover_enabled"`
	Revision            int64      `json:"revision"`
	State               string     `json:"state"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	RecoveryAttempts    int        `json:"recovery_attempts"`
	LastCheckedAt       *time.Time `json:"last_checked_at,omitempty"`
	NextProbeAt         *time.Time `json:"next_probe_at,omitempty"`
	NextRecoveryAt      *time.Time `json:"next_recovery_at,omitempty"`
	EgressIP            string     `json:"egress_ip,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

type WarpHealthController struct {
	store           *db.DB
	runtime         WarpHealthRuntime
	policy          db.WarpHealthPolicy
	mu              sync.Mutex
	started, closed bool
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	jobs            sync.WaitGroup
	inflight        map[int64]bool
	allowed         func(context.Context, int64) (bool, error)
	guard           db.WarpHealthRepairGuard
}

func NewWarpHealthController(store *db.DB, runtime WarpHealthRuntime, policy db.WarpHealthPolicy) (*WarpHealthController, error) {
	if store == nil || runtime == nil {
		return nil, errors.New("WARP 健康控制器缺少存储或运行器")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &WarpHealthController{store: store, runtime: runtime, policy: policy.Normalized(), ctx: ctx, cancel: cancel, done: make(chan struct{}), inflight: make(map[int64]bool)}, nil
}
func (c *WarpHealthController) SetAutoRecoveryAllowed(fn func(context.Context, int64) (bool, error)) {
	c.mu.Lock()
	c.allowed = fn
	c.mu.Unlock()
}

// RepairGuard 必须仅使用传入事务；用于与健康策略 CAS 及新增业务占用原子互斥。
func (c *WarpHealthController) SetRepairGuard(fn func(context.Context, *sql.Tx, int64) error) {
	c.mu.Lock()
	c.guard = fn
	c.mu.Unlock()
}

func (c *WarpHealthController) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("WARP 健康控制器已关闭")
	}
	if c.started {
		return nil
	}
	stopParent := context.AfterFunc(ctx, c.cancel)
	ctx = c.ctx
	c.started = true
	go func() {
		defer stopParent()
		defer close(c.done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				ids, err := c.store.WarpHealthProxyIDs(ctx)
				if err == nil {
					for _, id := range ids {
						c.mu.Lock()
						busy := c.inflight[id]
						if !busy && !c.closed {
							c.inflight[id] = true
							c.jobs.Add(1)
						} else {
							busy = true
						}
						c.mu.Unlock()
						if busy {
							continue
						}
						go func(id int64) {
							defer c.jobs.Done()
							defer func() { c.mu.Lock(); delete(c.inflight, id); c.mu.Unlock() }()
							_ = c.RunOnce(ctx, id)
						}(id)
					}
				}
				timer.Reset(c.policy.ProbeInterval)
			}
		}
	}()
	return nil
}
func (c *WarpHealthController) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	started := c.started
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	if started {
		select {
		case <-c.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	go func() { c.jobs.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *WarpHealthController) ImportProfile(ctx context.Context, id int64) error {
	// 原子检查 imported，避免重复启动导入旧磁盘覆盖新 DB 意图。
	p, material, err := c.runtime.HealthMaterial(warpProfileID(id))
	if err != nil {
		material = ""
		p.Desired = false
	}
	return c.store.WithWarpHealthLifecycle(ctx, func(tx *sql.Tx) error {
		var imported bool
		err := tx.QueryRowContext(ctx, `SELECT imported FROM warp_proxy_health WHERE proxy_id=$1`, id).Scan(&imported)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if imported {
			return nil
		}
		return db.SyncWarpHealthIntentTx(ctx, tx, id, db.WarpHealthIntent{Desired: p.Desired, Material: material, ProfileRevision: p.Revision})
	})
}
func (c *WarpHealthController) SyncProfile(ctx context.Context, id int64) error {
	p, material, err := c.runtime.HealthMaterial(warpProfileID(id))
	if err != nil {
		material = ""
		p.Desired = false
	}
	return c.store.SyncWarpHealthIntent(ctx, id, db.WarpHealthIntent{Desired: p.Desired, Material: material, ProfileRevision: p.Revision})
}

// cancel 仅建立 unknown 屏障，不代表执行已返回。调用者仍同步等到真实边界退出。
func (c *WarpHealthController) execute(ctx context.Context, execution *db.WarpHealthExecution, claim db.WarpHealthClaim, fn func(context.Context) (egress.WarpProfile, error)) (egress.WarpProfile, error, bool) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	marked := make(chan struct{})
	stop := context.AfterFunc(bounded, func() {
		defer close(marked)
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = execution.MarkWarpHealthUnknown(cleanup, claim)
	})
	p, err := fn(bounded)
	stopped := stop()
	if !stopped {
		<-marked
	}
	return p, err, bounded.Err() != nil || !stopped
}
func (c *WarpHealthController) RunOnce(ctx context.Context, id int64) error {
	return c.runOnce(ctx, id, false, true)
}

// ProbeNow 供管理端立即检测使用；未准入不能被解释为本次检测成功。
// 只清除周期门槛，不接管遗留 claim，也不改变节点意图或全局恢复策略。
func (c *WarpHealthController) ProbeNow(ctx context.Context, id int64) error {
	return c.runOnce(ctx, id, true, true)
}

// ProbeOnlyNow 供本地 Agent 检测使用，失败只保存结果，绝不触发恢复或接管遗留执行。
func (c *WarpHealthController) ProbeOnlyNow(ctx context.Context, id int64) error {
	return c.runOnce(ctx, id, true, false)
}

func (c *WarpHealthController) runOnce(ctx context.Context, id int64, immediate, allowRecovery bool) error {
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.jobs.Add(1)
	}
	c.mu.Unlock()
	if closed {
		return errors.New("WARP 健康控制器已关闭")
	}
	defer c.jobs.Done()
	ctx, cancel := context.WithCancel(ctx)
	stopClose := context.AfterFunc(c.ctx, cancel)
	defer func() { stopClose(); cancel() }()
	// 独立 session 锁覆盖网络真实退出；不同档案的慢操作不持全局生命周期锁。
	execution, err := c.store.TryWarpHealthExecution(ctx, id)
	if errors.Is(err, db.ErrWarpHealthBlocked) {
		if immediate {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer func() {
		if execution != nil {
			_ = execution.Close()
		}
	}()
	if immediate {
		// 先持有实际执行锁，再清除周期门槛，避免后台抢先执行后前台静默跳过。
		if err = c.store.PrepareWarpHealthManualProbe(ctx, id); err != nil {
			return err
		}
	}
	// 遗留 claim 不能只凭超时清除。持有旧执行的独占锁并核对真实退出后，
	// 才能解除屏障并重新走正常探测；未知注册或仍在使用的节点继续阻塞。
	previous, err := execution.WarpHealth(ctx, id)
	if err != nil {
		return err
	}
	if previous.Claim.Token != "" {
		if immediate {
			return db.ErrWarpHealthBlocked
		}
		if err = c.autoResolveUnknown(ctx, execution, previous); errors.Is(err, db.ErrWarpHealthBlocked) {
			return nil
		} else if err != nil {
			return err
		}
	}
	claim, err := execution.BeginWarpHealthProbe(ctx, id, time.Now())
	if errors.Is(err, db.ErrWarpHealthBlocked) {
		if immediate {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	warpHealthExecutions.Store(claim.Token, true)
	defer warpHealthExecutions.Delete(claim.Token)
	activeClaim := claim
	defer func() {
		if execution == nil {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// 完成回写后返回 stale；未完成的同 token 才会保留 unknown 屏障。
		_ = execution.MarkWarpHealthUnknown(cleanup, activeClaim)
	}()
	state, err := execution.WarpHealth(ctx, id)
	if err != nil {
		return err
	}
	p, probeErr, canceled := c.execute(ctx, execution, claim, func(ctx context.Context) (egress.WarpProfile, error) {
		profile, material, e := c.runtime.HealthMaterial(warpProfileID(id))
		if e != nil || material != state.Material || !profile.Desired {
			return profile, errors.New("WARP 意图或注册材料不匹配")
		}
		return c.runtime.Probe(ctx, warpProfileID(id))
	})
	if canceled {
		return context.Canceled
	}
	success := probeErr == nil && p.Phase == "ready" && p.Desired && egress.ValidWarpPublicIP(p.EgressIP)
	state, err = execution.CompleteWarpHealthProbe(ctx, claim, p.EgressIP, success, time.Now(), c.policy)
	if err != nil {
		return err
	}
	_ = execution.Close()
	execution = nil
	if !allowRecovery || success || state.Failures < c.policy.FailureThreshold {
		return nil
	}
	c.mu.Lock()
	allowed, guard := c.allowed, c.guard
	c.mu.Unlock()
	if allowed != nil {
		ok, e := allowed(ctx, id)
		if e != nil || !ok {
			return c.store.MarkWarpHealthDeferred(ctx, claim)
		}
	}
	execution, err = c.store.TryWarpHealthExecution(ctx, id)
	if errors.Is(err, db.ErrWarpHealthBlocked) {
		return c.store.MarkWarpHealthDeferred(ctx, claim)
	}
	if err != nil {
		return err
	}
	repair, err := execution.BeginWarpHealthRepairWithGuard(ctx, claim, time.Now(), c.policy, guard)
	if errors.Is(err, db.ErrWarpHealthBlocked) {
		_ = execution.Close()
		execution = nil
		return c.store.MarkWarpHealthDeferred(ctx, claim)
	}
	if err != nil {
		return err
	}
	warpHealthExecutions.Store(repair.Token, true)
	defer warpHealthExecutions.Delete(repair.Token)
	activeClaim = repair
	// 重连之前再次原地复探。避免仅凭旧失败计数终止已恢复的出口。
	p, repairErr, canceled := c.execute(ctx, execution, repair, func(ctx context.Context) (egress.WarpProfile, error) {
		current, material, e := c.runtime.HealthMaterial(warpProfileID(id))
		if e != nil || material != state.Material || !current.Desired {
			return current, errors.New("WARP 材料变化，拒绝重连")
		}
		if current.Phase == "ready" || current.Phase == "failed" {
			if current, e = c.runtime.Probe(ctx, warpProfileID(id)); e == nil && current.Phase == "ready" {
				return current, nil
			}
		}
		if err := ctx.Err(); err != nil {
			return current, err
		}
		return c.runtime.Reconnect(ctx, warpProfileID(id), state.Material)
	})
	if canceled {
		return context.Canceled
	}
	if errors.Is(repairErr, egress.ErrWarpExitUnknown) {
		return execution.MarkWarpHealthUnknown(ctx, repair)
	}
	_, err = execution.CompleteWarpHealthRepair(ctx, repair, p.EgressIP, repairErr == nil && p.Phase == "ready" && p.Desired && egress.ValidWarpPublicIP(p.EgressIP), time.Now(), c.policy)
	return err
}

func (c *WarpHealthController) ResolveUnknown(ctx context.Context, id int64) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("WARP 健康控制器已关闭")
	}
	c.jobs.Add(1)
	c.mu.Unlock()
	defer c.jobs.Done()
	ctx, cancel := context.WithCancel(ctx)
	stopClose := context.AfterFunc(c.ctx, cancel)
	defer func() { stopClose(); cancel() }()
	execution, err := c.store.TryWarpHealthExecution(ctx, id)
	if err != nil {
		return err
	}
	defer execution.Close()
	s, err := execution.WarpHealth(ctx, id)
	if err != nil {
		return err
	}
	c.mu.Lock()
	guard := c.guard
	c.mu.Unlock()
	return c.resolveUnknownWithExecution(ctx, execution, s, guard, false)
}

func (c *WarpHealthController) autoResolveUnknown(ctx context.Context, execution *db.WarpHealthExecution, s db.WarpHealthState) error {
	c.mu.Lock()
	allowed, guard := c.allowed, c.guard
	c.mu.Unlock()
	if allowed != nil {
		ok, err := allowed(ctx, s.ProxyID)
		if err != nil {
			return err
		}
		if !ok {
			return db.ErrWarpHealthBlocked
		}
	}
	// 全局策略、节点意图及业务占用在停止前与清屏障前各作同事务复核。
	// 策略关闭不强杀已准入的 Stop/Wait，但关闭后不能再清屏障或发起重连。
	autoGuard := func(ctx context.Context, tx *sql.Tx, id int64) error {
		if err := db.CheckWarpHealthAutoRecoveryEligibleTx(ctx, tx, id); err != nil {
			return err
		}
		if guard != nil {
			return guard(ctx, tx, id)
		}
		return nil
	}
	return c.resolveUnknownWithExecution(ctx, execution, s, autoGuard, true)
}

func (c *WarpHealthController) resolveUnknownWithExecution(ctx context.Context, execution *db.WarpHealthExecution, s db.WarpHealthState, guard db.WarpHealthRepairGuard, automatic bool) error {
	if s.Claim.Token == "" {
		return db.ErrWarpHealthBlocked
	}
	// 独占连接意外中断时仍要等当前宿主中的真实停止函数返回。
	if _, running := warpHealthExecutions.LoadOrStore(s.Claim.Token, true); running {
		return db.ErrWarpHealthBlocked
	}
	defer warpHealthExecutions.Delete(s.Claim.Token)
	localHost, hostErr := os.Hostname()
	if hostErr != nil {
		return egress.ErrWarpExitUnknown
	}
	if s.OwnerPID != os.Getpid() || s.OwnerHost != localHost {
		if err := egress.ConfirmWarpOwnerExited(s.OwnerHost, s.OwnerPID); err != nil {
			return err
		}
	}
	if err := execution.CheckIdle(ctx, s.ProxyID, guard); err != nil {
		return err
	}
	if automatic {
		p, err := c.runtime.Get(warpProfileID(s.ProxyID))
		if err != nil || !p.Desired || !p.RegistrationAttempted || !p.Registered || !p.Generated || p.Phase == "registering" || p.Phase == "generating" {
			return db.ErrWarpHealthBlocked
		}
		// unknown 仅在注册和生成均已完成时允许核对旧进程；核对后仍必须校验全部材料。
		if p.Phase != "unknown" {
			p, material, err := c.runtime.HealthMaterial(warpProfileID(s.ProxyID))
			if err != nil || material != s.Material || !p.Desired {
				return db.ErrWarpHealthBlocked
			}
		}
	}
	// 崩溃可能来不及写 unknown；取得旧执行独占锁并核对 owner 后才提升为核对状态。
	if s.ClaimState != "unknown" {
		if err := execution.MarkWarpHealthUnknown(ctx, s.Claim); err != nil {
			return err
		}
	}
	if err := c.runtime.ConfirmStopped(ctx, warpProfileID(s.ProxyID)); err != nil {
		return err
	}
	if automatic {
		p, material, err := c.runtime.HealthMaterial(warpProfileID(s.ProxyID))
		if err != nil || material != s.Material || !p.Desired {
			return db.ErrWarpHealthBlocked
		}
	}
	return execution.ResolveWarpHealthUnknownWithGuard(ctx, s.Claim, guard)
}

func (c *WarpHealthController) DTO(ctx context.Context, id int64) (*WarpHealthDTO, error) {
	var out *WarpHealthDTO
	err := c.store.WithWarpHealthLifecycle(ctx, func(tx *sql.Tx) error {
		s, proxy, pending, err := db.WarpHealthViewTx(ctx, tx, id)
		if err != nil {
			return err
		}
		out = &WarpHealthDTO{ProxyID: i64s(id), DesiredRunning: s.Desired, AutoRecoverEnabled: s.AutoRecoverEnabled, Revision: s.Revision, State: s.State, ConsecutiveFailures: s.Failures, RecoveryAttempts: s.RecoveryAttempts, LastCheckedAt: s.LastCheckedAt, NextProbeAt: s.NextProbeAt, NextRecoveryAt: s.NextRecoveryAt, LastError: s.LastError}
		switch {
		case pending:
			out.State = "pending"
		case !proxy.Enabled:
			out.State = "disabled"
		case !s.Desired || proxy.Status == db.ProxyStatusStopped:
			out.State = "stopped"
		case s.ClaimState == "unknown":
			out.State = "unknown"
		case s.Claim.Kind == "repair":
			out.State = "repairing"
		case s.State == "ready":
			// Get 只能读取本地状态，禁止在此接口中加入网络或另借数据库连接。
			p, e := c.runtime.Get(warpProfileID(id))
			if e == nil && p.Desired && p.Phase == "ready" && egress.ValidWarpPublicIP(p.EgressIP) && p.EgressIP == s.EgressIP && proxy.Status == db.ProxyStatusHealthy {
				out.EgressIP = p.EgressIP
			} else {
				out.State = "failed"
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

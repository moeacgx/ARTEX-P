package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// 关闭门禁覆盖完整准入（含提交结果核实和凭据入队），不能只检查底层连接池。
// 真实操作已经持有的会话留到正式 Release，关闭失败则保留原池及协调者供恢复重试。
type taskOperationLifecycle struct {
	mu               sync.Mutex
	closing          bool
	closed           bool
	inflight         int
	idle             chan struct{}
	attempt          *taskOperationCloseAttempt
	recoveryIdentity [32]byte
}

// 关闭失败对象的强引用与凭据保留到成功关闭；重建对象只能按 Open 时相同配置续办。
// 键仅存连接配置摘要，不读取后来变化的环境变量，也不输出连接串。
var closingTaskOperationDBs sync.Map // *DB -> struct{}

func (d *DB) initOperationRecoveryIdentity(dsn string) {
	d.operationLifecycle.recoveryIdentity = sha256.Sum256([]byte(dsn))
}

type taskOperationCloseAttempt struct {
	done chan struct{}
	err  error
}

var ErrDBClosing = errors.New("数据库正在关闭或已经关闭，拒绝新操作准入")

func (d *DB) beginOperationAdmission() error {
	l := &d.operationLifecycle
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return ErrDBClosing
	}
	if l.inflight == 0 {
		l.idle = make(chan struct{})
	}
	l.inflight++
	return nil
}

func (d *DB) endOperationAdmission() {
	l := &d.operationLifecycle
	l.mu.Lock()
	l.inflight--
	if l.inflight == 0 {
		close(l.idle)
	}
	l.mu.Unlock()
}

func (d *DB) operationAdmissionAllowed() error {
	l := &d.operationLifecycle
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing {
		return ErrDBClosing
	}
	return nil
}

func (d *DB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresLifecycleTimeout)
	defer cancel()
	return d.closeContext(ctx)
}

func (d *DB) closeContext(ctx context.Context) error {
	l := &d.operationLifecycle
	l.mu.Lock()
	l.closing = true
	if previous := l.attempt; previous != nil {
		l.mu.Unlock()
		select {
		case <-previous.done:
			return previous.err
		case <-ctx.Done():
			return errors.Join(ErrProxySwitchRecovery, ctx.Err())
		}
	}
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	attempt := &taskOperationCloseAttempt{done: make(chan struct{})}
	l.attempt = attempt
	idle := l.idle
	inflight := l.inflight
	closingTaskOperationDBs.Store(d, struct{}{})
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.attempt = nil
		close(attempt.done)
		l.mu.Unlock()
	}()

	if inflight != 0 {
		// 只给准入结果一个短收尾窗口；未结束则显式拒绝完成关闭，保留全部资源。
		// 不等到原十五秒事务过期，更不能为完成 Close 取消已启动的共享事务。
		waitCtx, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		defer stop()
		select {
		case <-idle:
		case <-waitCtx.Done():
			attempt.err = errors.Join(ErrProxySwitchRecovery, waitCtx.Err())
			return attempt.err
		}
	}
	// 排空失败绝不能继续关闭业务池：精确 token 与真实操作可能仍共用活 owner。
	// 协调 goroutine 自身持有 d，调用方重建 DB 或丢弃旧引用也不会丢失恢复路径。
	if err := d.closeTaskOperationReclaimsContext(ctx); err != nil {
		attempt.err = err
		return err
	}
	var lockErr error
	lockErr = d.closeFOFAThrottlePool()
	if pool, ok := taskOperationLockPools.LoadAndDelete(d.DB); ok {
		lockErr = errors.Join(lockErr, pool.(*sql.DB).Close())
	}
	attempt.err = errors.Join(lockErr, d.DB.Close())
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	closingTaskOperationDBs.Delete(d)
	return attempt.err
}

func (d *DB) reclaimClosingTaskOperationDBs(ctx context.Context) error {
	var reclaimErr error
	closingTaskOperationDBs.Range(func(key, _ any) bool {
		owner := key.(*DB)
		identity := d.operationLifecycle.recoveryIdentity
		if identity == [32]byte{} || identity != owner.operationLifecycle.recoveryIdentity {
			return true
		}
		for token, id := range owner.pendingTaskOperationReclaims() {
			// 新对象可用池与旧对象拥有的精确凭据联合核实；只清理从未交给业务的 token。
			// 与旧后台并行时仍由同一数据库 lifecycle 事务锁保证幂等。
			if err := d.reclaimUnadmittedTaskOperationContext(ctx, id, token); err != nil {
				reclaimErr = errors.Join(reclaimErr, err)
				continue
			}
			owner.operationReclaims.mu.Lock()
			delete(owner.operationReclaims.pending, token)
			owner.operationReclaims.mu.Unlock()
		}
		return ctx.Err() == nil
	})
	if reclaimErr != nil {
		return errors.Join(ErrProxySwitchRecovery, reclaimErr)
	}
	return ctx.Err()
}

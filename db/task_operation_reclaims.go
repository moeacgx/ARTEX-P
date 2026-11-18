package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// 这里只保存已确定未交给业务执行的精确 token；不能由活动计数推断或补造凭据。
// 每个 DB 的单一协调循环在空队列时退出，关闭时取消并等待；失败凭据在成功核实前不丢弃。
type taskOperationReclaims struct {
	mu      sync.Mutex
	pending map[string]int64
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
}

func (d *DB) queueAndReclaimTaskOperation(id int64, token string) error {
	r := &d.operationReclaims
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrProxySwitchRecovery
	}
	if r.pending == nil {
		r.pending = make(map[string]int64)
	}
	r.pending[token] = id
	r.mu.Unlock()
	err := d.reclaimQueuedTaskOperation(context.Background(), id, token)
	if err != nil {
		d.startTaskOperationReclaims()
	}
	return err
}

func (d *DB) reclaimQueuedTaskOperation(ctx context.Context, id int64, token string) error {
	if err := d.reclaimUnadmittedTaskOperationContext(ctx, id, token); err != nil {
		return err
	}
	r := &d.operationReclaims
	r.mu.Lock()
	delete(r.pending, token)
	r.mu.Unlock()
	return nil
}

func (d *DB) pendingTaskOperationReclaims() map[string]int64 {
	r := &d.operationReclaims
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		return nil
	}
	out := make(map[string]int64, len(r.pending))
	for token, id := range r.pending {
		out[token] = id
	}
	return out
}

func (d *DB) retryTaskOperationReclaimsBriefly(parent context.Context) {
	pending := d.pendingTaskOperationReclaims()
	if len(pending) == 0 || parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
	defer cancel()
	for token, id := range pending {
		if ctx.Err() != nil {
			break
		}
		// 事务锁使并发协调幂等；失败不删除凭据，后台重试保留原有保护屏障。
		if err := d.reclaimQueuedTaskOperation(ctx, id, token); err != nil {
			d.startTaskOperationReclaims()
		}
	}
}

func (d *DB) startTaskOperationReclaims() {
	r := &d.operationReclaims
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.done != nil || len(r.pending) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	go func() {
		defer cancel()
		delay := 250 * time.Millisecond
		for {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
				for token, id := range d.pendingTaskOperationReclaims() {
					if ctx.Err() != nil {
						break
					}
					if err := d.reclaimQueuedTaskOperation(ctx, id, token); err != nil && ctx.Err() == nil {
						log.Printf("[proxy] 未准入操作精确回收待重试 task=%d token=%s: %v", id, token, err)
					}
				}
			}
			// 空队列退出和新凭据入队使用同一把锁，避免消息落在退出窗口。
			r.mu.Lock()
			if ctx.Err() != nil || len(r.pending) == 0 {
				r.cancel, r.done = nil, nil
				close(done)
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
		}
	}()
}

func (d *DB) closeTaskOperationReclaims() error {
	ctx, stop := context.WithTimeout(context.Background(), postgresLifecycleTimeout)
	defer stop()
	return d.closeTaskOperationReclaimsContext(ctx)
}

func (d *DB) closeTaskOperationReclaimsContext(ctx context.Context) (err error) {
	r := &d.operationReclaims
	r.mu.Lock()
	r.closed = true
	cancel, done := r.cancel, r.done
	if cancel != nil {
		cancel()
	}
	r.mu.Unlock()
	defer func() {
		if err != nil {
			r.mu.Lock()
			r.closed = false
			r.mu.Unlock()
			d.startTaskOperationReclaims()
		}
	}()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			// 已取消的循环可能尚在退出；结束后重新启动，避免丢失重试机会。
			go func() {
				<-done
				d.startTaskOperationReclaims()
			}()
			return errors.Join(ErrProxySwitchRecovery, ctx.Err())
		}
	}
	// 正常关闭仍尝试一次有界精确回收；未完成必须向调用者显式报告，不能静默丢弃。
	var reclaimErr error
	for token, id := range d.pendingTaskOperationReclaims() {
		reclaimErr = errors.Join(reclaimErr, d.reclaimQueuedTaskOperation(ctx, id, token))
	}
	if pending := len(d.pendingTaskOperationReclaims()); pending != 0 {
		return errors.Join(ErrProxySwitchRecovery, fmt.Errorf("关闭时仍有 %d 个未准入操作待精确回收", pending), reclaimErr)
	}
	return nil
}

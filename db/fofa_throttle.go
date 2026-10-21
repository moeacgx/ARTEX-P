package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var ErrFOFAThrottle = errors.New("FOFA 节流状态不可用，未放行请求")
var ErrFOFAThrottleUnsettled = errors.New("FOFA 上次请求状态未能安全保存，已停止自动请求，请检查数据库后恢复")
var ErrFOFARequestPending = errors.New("FOFA 当前 Key 正有请求执行中，请等待该请求结束")

// 许可使用独立且有上限的连接池，给业务校验和代理准入保留连接。
func (d *DB) fofaThrottleConn(ctx context.Context) (*sql.Conn, error) {
	d.fofaPoolMu.Lock()
	if d.fofaPoolClosed {
		d.fofaPoolMu.Unlock()
		return nil, ErrFOFAThrottle
	}
	if d.fofaPool != nil {
		pool := d.fofaPool
		d.fofaPoolMu.Unlock()
		return pool.Conn(ctx)
	}
	source, err := d.Conn(ctx)
	if err != nil {
		d.fofaPoolMu.Unlock()
		return nil, err
	}
	var config *pgx.ConnConfig
	err = source.Raw(func(raw any) error {
		conn, ok := raw.(*stdlib.Conn)
		if !ok {
			return ErrFOFAThrottle
		}
		config = conn.Conn().Config().Copy()
		return nil
	})
	_ = source.Close()
	if err != nil {
		d.fofaPoolMu.Unlock()
		return nil, err
	}
	pool := stdlib.OpenDB(*config)
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(time.Minute)
	d.fofaPool = pool
	d.fofaPoolMu.Unlock()
	return pool.Conn(ctx)
}

func (d *DB) closeFOFAThrottlePool() error {
	d.fofaPoolMu.Lock()
	d.fofaPoolClosed = true
	pool := d.fofaPool
	d.fofaPool = nil
	d.fofaPoolMu.Unlock()
	if pool != nil {
		return pool.Close()
	}
	return nil
}

type FOFACooldownError struct{ RetryAfter time.Duration }

func (e *FOFACooldownError) Error() string { return "FOFA 正在共享冷却中，未发起请求" }

type fofaThrottleState struct {
	LastStartedAt time.Time `json:"last_started_at"`
	NextAllowedAt time.Time `json:"next_allowed_at"`
	CooldownUntil time.Time `json:"cooldown_until"`
	PendingToken  string    `json:"pending_token,omitempty"`
}

// 一个 Key 的所有 Worker、会话和出口共享数据库锁，不能以更换代理绕过冷却。
type FOFARequestLease struct {
	conn  *sql.Conn
	db    *DB
	scope string
	state fofaThrottleState
	once  sync.Once
	err   error
}

func fofaThrottleScope(key string) string {
	return fmt.Sprintf("integration.fofa.throttle.v1:%x", sha256.Sum256([]byte(key)))
}

func discardFOFAConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func releaseFOFAConn(conn *sql.Conn, scope string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, scope).Scan(&unlocked); err != nil || !unlocked {
		// Close 只归还池连接；未确认解锁时必须弃用物理连接。
		discardFOFAConn(conn)
		return ErrFOFAThrottle
	}
	return conn.Close()
}

func waitFOFA(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (d *DB) AcquireFOFARequest(ctx context.Context, key string, interval time.Duration) (*FOFARequestLease, error) {
	if !ValidFOFAKey(key) || interval <= 0 {
		return nil, ErrFOFAConfig
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	scope := fofaThrottleScope(key)
	for {
		conn, err := d.fofaThrottleConn(ctx)
		if err != nil {
			return nil, errors.Join(ErrFOFAThrottle, ctx.Err())
		}
		var locked bool
		if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, scope).Scan(&locked); err != nil {
			// 查询取消也可能发生在服务端已加锁之后，不能把未知锁状态还池。
			discardFOFAConn(conn)
			return nil, errors.Join(ErrFOFAThrottle, ctx.Err())
		}
		if !locked {
			_ = conn.Close()
			if err := waitFOFA(ctx, 50*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		lease := &FOFARequestLease{conn: conn, scope: scope, db: d}
		if _, err = conn.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES($1,'{}') ON CONFLICT(key) DO NOTHING`, scope); err != nil {
			_ = releaseFOFAConn(conn, scope)
			return nil, errors.Join(ErrFOFAThrottle, ctx.Err())
		}
		var raw string
		var now time.Time
		if err = conn.QueryRowContext(ctx, `SELECT value,clock_timestamp() FROM settings WHERE key=$1`, scope).Scan(&raw, &now); err != nil || json.Unmarshal([]byte(raw), &lease.state) != nil {
			_ = releaseFOFAConn(conn, scope)
			return nil, errors.Join(ErrFOFAThrottle, ctx.Err())
		}
		if lease.state.PendingToken != "" {
			_ = releaseFOFAConn(conn, scope)
			return nil, ErrFOFAThrottleUnsettled
		}
		if delay := lease.state.CooldownUntil.Sub(now); delay > 0 {
			if err := releaseFOFAConn(conn, scope); err != nil {
				return nil, err
			}
			return nil, &FOFACooldownError{RetryAfter: delay}
		}
		next := lease.state.NextAllowedAt
		if configured := lease.state.LastStartedAt.Add(interval); configured.After(next) {
			next = configured
		}
		if delay := next.Sub(now); delay > 0 {
			if err := releaseFOFAConn(conn, scope); err != nil {
				return nil, err
			}
			if err := waitFOFA(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}
		lease.state.LastStartedAt = now
		lease.state.NextAllowedAt = now.Add(interval)
		// 发送前持久保护，只有可靠完成写回才清除；断电或写回失败不能默认为成功。
		lease.state.PendingToken = uuid.NewString()
		b, _ := json.Marshal(lease.state)
		if _, err := conn.ExecContext(ctx, `UPDATE settings SET value=$2,updated_at=clock_timestamp() WHERE key=$1`, scope, string(b)); err != nil {
			_ = lease.Finish(0)
			return nil, errors.Join(ErrFOFAThrottle, ctx.Err())
		}
		return lease, nil
	}
}

// 即使请求已取消，也用独立短期限保存冷却和释放许可；重复调用不会改写结果。
func (l *FOFARequestLease) Finish(cooldown time.Duration) error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := l.settle(ctx, l.conn, cooldown)
		cancel()
		unlockErr := releaseFOFAConn(l.conn, l.scope)
		if err == nil && unlockErr == nil {
			return
		}
		// 原连接失效时，在新的独立会话中补写；未结算标记阻止其他 Worker 抢先发送。
		recovery, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for recovery.Err() == nil {
			conn, err := l.db.fofaThrottleConn(recovery)
			if err != nil {
				break
			}
			var locked bool
			if err = conn.QueryRowContext(recovery, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, l.scope).Scan(&locked); err != nil {
				discardFOFAConn(conn)
				break
			}
			if !locked {
				_ = conn.Close()
				if waitFOFA(recovery, 50*time.Millisecond) != nil {
					break
				}
				continue
			}
			err = l.settle(recovery, conn, cooldown)
			unlockErr = releaseFOFAConn(conn, l.scope)
			if err == nil && unlockErr == nil {
				return
			}
			break
		}
		l.err = ErrFOFAThrottleUnsettled
	})
	return l.err
}

func (l *FOFARequestLease) settle(ctx context.Context, conn *sql.Conn, cooldown time.Duration) error {
	var raw string
	var now time.Time
	if err := conn.QueryRowContext(ctx, `SELECT value,clock_timestamp() FROM settings WHERE key=$1`, l.scope).Scan(&raw, &now); err != nil {
		return err
	}
	var state fofaThrottleState
	if json.Unmarshal([]byte(raw), &state) != nil {
		return ErrFOFAThrottleUnsettled
	}
	// 写入已成功但确认丢失时，允许识别已完成；不能覆盖另一请求的新许可。
	if state.PendingToken == "" && !state.CooldownUntil.Before(l.state.CooldownUntil) {
		return nil
	}
	if state.PendingToken != l.state.PendingToken {
		return ErrFOFAThrottleUnsettled
	}
	if cooldown > 0 {
		until := now.Add(cooldown)
		if until.After(state.CooldownUntil) {
			state.CooldownUntil = until
		}
	}
	l.state.CooldownUntil = state.CooldownUntil
	state.PendingToken = ""
	b, _ := json.Marshal(state)
	_, err := conn.ExecContext(ctx, `UPDATE settings SET value=$2,updated_at=clock_timestamp() WHERE key=$1`, l.scope, string(b))
	return err
}

// 只读状态与发送门禁共用数据库时间，读取失败不假装已解除冷却。
func (d *DB) FOFARequestCooldown(ctx context.Context, key string) (time.Duration, error) {
	if key == "" {
		return 0, nil
	}
	var raw string
	var now time.Time
	err := d.QueryRowContext(ctx, `SELECT value,clock_timestamp() FROM settings WHERE key=$1`, fofaThrottleScope(key)).Scan(&raw, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	var state fofaThrottleState
	if err != nil || json.Unmarshal([]byte(raw), &state) != nil {
		return 0, ErrFOFAThrottle
	}
	if state.PendingToken != "" {
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		conn, err := d.fofaThrottleConn(bounded)
		if err != nil {
			return 0, ErrFOFAThrottle
		}
		var locked bool
		scope := fofaThrottleScope(key)
		if err := conn.QueryRowContext(bounded, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, scope).Scan(&locked); err != nil {
			discardFOFAConn(conn)
			return 0, ErrFOFAThrottle
		}
		if !locked {
			_ = conn.Close()
			return 0, ErrFOFARequestPending
		}
		defer releaseFOFAConn(conn, scope)
		// 状态读取和拿锁之间可能刚好完成；锁内重读避免把正常完成误报为故障。
		if err := conn.QueryRowContext(bounded, `SELECT value,clock_timestamp() FROM settings WHERE key=$1`, scope).Scan(&raw, &now); err != nil {
			return 0, ErrFOFAThrottle
		}
		state = fofaThrottleState{}
		if json.Unmarshal([]byte(raw), &state) != nil {
			return 0, ErrFOFAThrottle
		}
		if state.PendingToken != "" {
			return 0, ErrFOFAThrottleUnsettled
		}
	}
	return max(state.CooldownUntil.Sub(now), 0), nil
}

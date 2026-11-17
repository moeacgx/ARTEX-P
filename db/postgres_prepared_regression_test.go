package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPreparedShortOperationsBeyondPrepareDeadlinePG(t *testing.T) {
	pool, err := openPostgresDB(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	conn, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stmt, err := conn.PrepareContext(context.Background(), `SELECT pg_sleep(1)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	start := time.Now()
	for i := 0; i < 32; i++ {
		if _, err := stmt.Exec(); err != nil {
			t.Fatalf("第 %d 次短执行，累计 %v: %v", i+1, time.Since(start), err)
		}
	}
	if time.Since(start) <= 30*time.Second {
		t.Fatal("没有跨过 prepare 的 30 秒期限")
	}
}

func TestPreparedCancellationPG(t *testing.T) {
	for _, query := range []bool{false, true} {
		name := "exec"
		if query {
			name = "query"
		}
		t.Run(name, func(t *testing.T) {
			pool, err := openPostgresDB(testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			stmt, err := pool.PrepareContext(t.Context(), `SELECT pg_sleep(5)`)
			if err != nil {
				t.Fatal(err)
			}
			defer stmt.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			if query {
				var n any
				err = stmt.QueryRowContext(ctx).Scan(&n)
			} else {
				_, err = stmt.ExecContext(ctx)
			}
			var pgErr *pgconn.PgError
			if ctx.Err() != context.DeadlineExceeded || !(errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &pgErr) && pgErr.Code == "57014")) {
				t.Fatalf("取消错误=%v", err)
			}
		})
	}
}

// 故障只注入真实 PG 已确认的 prepared 写入之后，验证不会扩大安全重试边界。
type preparedLostConnector struct {
	base  driver.Connector
	query bool
}

func (c preparedLostConnector) Driver() driver.Driver { return c.base.Driver() }
func (c preparedLostConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &preparedLostConn{Conn: conn, query: c.query}, nil
}

type preparedLostConn struct {
	driver.Conn
	query bool
}

func (c *preparedLostConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	s, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return &preparedLostStmt{Stmt: s, query: c.query}, nil
}

type preparedLostStmt struct {
	driver.Stmt
	query bool
}

func (s *preparedLostStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	_, err := s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	if err != nil {
		return nil, err
	}
	return nil, io.EOF
}
func (s *preparedLostStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	values := make([]driver.Value, len(rows.Columns()))
	for {
		err = rows.Next(values)
		if err != nil {
			break
		}
	}
	closeErr := rows.Close()
	if err != io.EOF {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return nil, io.EOF
}

func TestPreparedCommittedResponseLostPG(t *testing.T) {
	for _, query := range []bool{false, true} {
		name := "exec"
		if query {
			name = "query"
		}
		t.Run(name, func(t *testing.T) {
			d, _ := fofaSettingsTestDB(t)
			if _, err := d.Exec(`CREATE TABLE prepared_writes(id bigserial PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			var schema string
			if err := d.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			cfg, err := pgx.ParseConfig(testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			cfg.RuntimeParams["search_path"] = schema
			pool := sql.OpenDB(retryingPostgresConnector{base: preparedLostConnector{base: stdlib.GetConnector(*cfg), query: query}})
			defer pool.Close()
			stmt, err := pool.PrepareContext(t.Context(), `INSERT INTO prepared_writes DEFAULT VALUES RETURNING id`)
			if err != nil {
				t.Fatal(err)
			}
			defer stmt.Close()
			if query {
				var id int64
				err = stmt.QueryRowContext(t.Context()).Scan(&id)
			} else {
				_, err = stmt.ExecContext(t.Context())
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("错误未原样保留: %v", err)
			}
			var count int
			if err := d.QueryRow(`SELECT count(*) FROM prepared_writes`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("已提交写入被重放: %d", count)
			}
			if pool.Stats().Idle != 0 {
				t.Fatal("故障 prepared 连接未淘汰")
			}
		})
	}
}

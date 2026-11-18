package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type committedQueryConnector struct {
	commits atomic.Int32
	closes  atomic.Int32
}

type committedPGConnector struct{ base driver.Connector }

func (c committedPGConnector) Driver() driver.Driver { return c.base.Driver() }
func (c committedPGConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &committedPGConn{Conn: conn}, nil
}

type committedPGConn struct{ driver.Conn }

func (c *committedPGConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil || !strings.HasPrefix(query, "INSERT INTO mcp_servers") {
		return rows, err
	}
	// 消耗真实 PG 的提交确认，但向 database/sql 注入首响应丢失。
	values := make([]driver.Value, len(rows.Columns()))
	for {
		if err = rows.Next(values); err != nil {
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

func TestPostgresCommittedInsertReturningResponseLost(t *testing.T) {
	d, _ := fofaSettingsTestDB(t)
	var schema string
	if err := d.QueryRow("SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("CREATE TABLE mcp_servers(id bigserial PRIMARY KEY, name text NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	dsn, _, err := DSN()
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	pool := sql.OpenDB(retryingPostgresConnector{base: committedPGConnector{base: stdlib.GetConnector(*config)}})
	defer pool.Close()
	var id int64
	err = pool.QueryRowContext(t.Context(), "INSERT INTO mcp_servers(name) VALUES('response-lost') RETURNING id").Scan(&id)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("响应错误=%v", err)
	}
	var count int
	if err := d.QueryRow("SELECT count(*) FROM mcp_servers").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("真实 PG 已提交 %d 条，期望仅 1 条", count)
	}
	if err := pool.QueryRowContext(t.Context(), "SELECT 1").Scan(&count); err != nil || count != 1 {
		t.Fatalf("新连接不可用: %v", err)
	}
}

func (c *committedQueryConnector) Connect(context.Context) (driver.Conn, error) {
	return &committedQueryConn{owner: c}, nil
}

func (c *committedQueryConnector) Driver() driver.Driver { return retryTestDriver{} }

type committedQueryConn struct {
	retryTestConn
	owner *committedQueryConnector
}

func (c *committedQueryConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	c.owner.commits.Add(1)
	return nil, io.EOF
}

func (c *committedQueryConn) Close() error { c.owner.closes.Add(1); return nil }

func TestSQLDoesNotReplayCommittedInsertReturning(t *testing.T) {
	base := &committedQueryConnector{}
	pool := sql.OpenDB(retryingPostgresConnector{base: base})
	defer pool.Close()
	var id int64
	err := pool.QueryRowContext(t.Context(), "INSERT INTO mcp_servers(name) VALUES('probe') RETURNING id").Scan(&id)
	if !errors.Is(err, io.EOF) {
		t.Errorf("error=%v, want EOF", err)
	}
	if got := base.commits.Load(); got != 1 {
		t.Errorf("committed writes=%d, want 1", got)
	}
	if got := base.closes.Load(); got != 1 {
		t.Errorf("discarded connections=%d, want 1", got)
	}
}

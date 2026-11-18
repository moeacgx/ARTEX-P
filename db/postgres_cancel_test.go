package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestPostgresCancelKeepsCheckedOutConnectionUsable(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	conn, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err = conn.ExecContext(ctx, `SELECT pg_sleep(5)`); err == nil {
		t.Fatal("expected canceled query to fail")
	}
	var value int
	if err = conn.QueryRowContext(context.Background(), `SELECT 1`).Scan(&value); err != nil {
		t.Fatalf("same checked-out connection was unusable after cancel: %v", err)
	}
	if value != 1 {
		t.Fatalf("unexpected value after cancel: %d", value)
	}
}

func TestPostgresTerminatedConnectionIsEvictedAndReplaced(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	conn, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var firstPID int64
	if err := conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var terminated bool
	if err := admin.QueryRowContext(t.Context(), `SELECT pg_terminate_backend($1)`, firstPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("终止测试连接失败：terminated=%v err=%v", terminated, err)
	}
	if _, err := conn.ExecContext(t.Context(), `SELECT 1`); err == nil {
		t.Fatal("已终止连接意外执行成功")
	}
	if err := conn.Close(); err != nil && err != sql.ErrConnDone {
		t.Fatal(err)
	}
	var nextPID int64
	if err := d.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&nextPID); err != nil {
		t.Fatalf("池未提供可用新连接：%v", err)
	}
	if nextPID == firstPID {
		t.Fatalf("故障会话仍被复用：%d", firstPID)
	}
}

func TestPostgresCanceledQueryPreservesBackendAcrossPoolReuse(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	conn, err := d.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var originalPID int64
	if err := conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&originalPID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var ignored any
	if err := conn.QueryRowContext(ctx, `SELECT pg_sleep(5)`).Scan(&ignored); err == nil {
		t.Fatal("超时查询意外成功")
	}
	var reusedPID int64
	if err := conn.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&reusedPID); err != nil || reusedPID != originalPID {
		t.Fatalf("取消查询未保留原会话：original=%d reused=%d err=%v", originalPID, reusedPID, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&reusedPID); err != nil || reusedPID != originalPID {
		t.Fatalf("归池后未复用已恢复会话：original=%d reused=%d err=%v", originalPID, reusedPID, err)
	}
}

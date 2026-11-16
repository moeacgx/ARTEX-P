package db

import (
	"context"
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

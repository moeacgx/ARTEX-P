package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type retryTestConnector struct {
	errs     []error
	attempts int
}

func (c *retryTestConnector) Connect(context.Context) (driver.Conn, error) {
	c.attempts++
	if c.attempts <= len(c.errs) {
		return nil, c.errs[c.attempts-1]
	}
	return retryTestConn{}, nil
}

func (*retryTestConnector) Driver() driver.Driver {
	return retryTestDriver{}
}

type retryTestDriver struct{}

func (retryTestDriver) Open(string) (driver.Conn, error) {
	return retryTestConn{}, nil
}

type retryTestConn struct {
	queryErr error
	execErr  error
}

func (retryTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}

func (retryTestConn) Close() error {
	return nil
}

func (retryTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("not implemented")
}

func (c retryTestConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, c.queryErr
}

func (c retryTestConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, c.execErr
}

type resetBlockingConn struct {
	retryTestConn
}

func (c resetBlockingConn) ResetSession(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestRetryingPostgresConnectorRetriesTransientFailures(t *testing.T) {
	base := &retryTestConnector{errs: []error{
		errors.New("dial tcp 127.0.0.1:5432: i/o timeout"),
		errors.New("failed SASL auth: wsarecv: forcibly closed"),
	}}
	connector := retryingPostgresConnector{base: base}

	conn, err := connector.Connect(t.Context())
	if err != nil {
		t.Fatalf("connect after transient failures: %v", err)
	}
	_ = conn.Close()
	if base.attempts != 3 {
		t.Fatalf("attempts=%d, want 3", base.attempts)
	}
}

func TestRetryingPostgresConnectorDoesNotRetryDeterministicFailure(t *testing.T) {
	base := &retryTestConnector{errs: []error{
		errors.New(`failed to connect: password authentication failed for user "artex"`),
	}}
	connector := retryingPostgresConnector{base: base}

	if _, err := connector.Connect(t.Context()); err == nil {
		t.Fatal("expected deterministic failure")
	}
	if base.attempts != 1 {
		t.Fatalf("attempts=%d, want 1", base.attempts)
	}
}

func TestRetryingPostgresConnMarksQueryConnectionLossBad(t *testing.T) {
	conn := &retryingPostgresConn{base: retryTestConn{
		queryErr: errors.New("read tcp 127.0.0.1:5432: wsarecv: forcibly closed"),
	}}

	if _, err := conn.QueryContext(t.Context(), "SELECT 1", nil); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("query error=%v, want driver.ErrBadConn", err)
	}
}

func TestRetryingPostgresConnKeepsServerErrors(t *testing.T) {
	serverErr := &pgconn.PgError{Code: "42601", Message: "syntax error"}
	conn := &retryingPostgresConn{base: retryTestConn{queryErr: serverErr}}

	if _, err := conn.QueryContext(t.Context(), "SELECT broken", nil); !errors.Is(err, serverErr) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("query error=%v, want original PostgreSQL error", err)
	}
}

func TestRetryingPostgresConnDoesNotReplayAmbiguousExec(t *testing.T) {
	networkErr := errors.New("write tcp 127.0.0.1:5432: connection reset by peer")
	conn := &retryingPostgresConn{base: retryTestConn{execErr: networkErr}}

	if _, err := conn.ExecContext(t.Context(), "UPDATE tasks SET paused=true", nil); !errors.Is(err, networkErr) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("exec error=%v, want original network error", err)
	}
}

func TestRetryingPostgresConnResetSessionHonorsShorterContext(t *testing.T) {
	conn := &retryingPostgresConn{base: resetBlockingConn{}}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := conn.ResetSession(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reset error=%v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("reset took %s, want bound by caller context", elapsed)
	}
}

func TestConfiguredPostgresConnectStress(t *testing.T) {
	if os.Getenv("ARTEX_PG_STABILITY_TEST") != "1" {
		t.Skip("set ARTEX_PG_STABILITY_TEST=1 to run against the configured PostgreSQL")
	}
	dsn, _, err := DSN()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 30; i++ {
		db, err := openPostgresDB(dsn)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		connectCtx, cancelConnect := context.WithTimeout(t.Context(), 20*time.Second)
		conn, err := db.Conn(connectCtx)
		cancelConnect()
		if err != nil {
			db.Close()
			t.Fatalf("connection %d: %v", i+1, err)
		}
		queryCtx, cancelQuery := context.WithTimeout(t.Context(), 5*time.Second)
		var one int
		err = conn.QueryRowContext(queryCtx, `SELECT 1`).Scan(&one)
		cancelQuery()
		_ = conn.Close()
		_ = db.Close()
		if err != nil {
			t.Fatalf("query %d: %v", i+1, err)
		}
		if one != 1 {
			t.Fatalf("query %d returned %d", i+1, one)
		}
	}
}

package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// AC15: the deadlock retry of migration transactions, against a fake database/sql driver that
// records every transaction event and fails the statement "SELECT inject" from a script. No
// PostgreSQL is involved.

type migrationFake struct {
	mu     sync.Mutex
	events []string
	script []error // one entry per execution of "SELECT inject"; nil = success
	next   int

	beginErr, setErr, commitErr error // failures of the transaction steps; nil = success
}

func (f *migrationFake) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *migrationFake) scripted() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next >= len(f.script) {
		return nil
	}
	err := f.script[f.next]
	f.next++
	return err
}

func (f *migrationFake) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

type migrationFakeConnector struct{ f *migrationFake }

func (c migrationFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &migrationFakeConn{c.f}, nil
}
func (c migrationFakeConnector) Driver() driver.Driver { return migrationFakeDriver{c.f} }

type migrationFakeDriver struct{ f *migrationFake }

func (d migrationFakeDriver) Open(string) (driver.Conn, error) { return &migrationFakeConn{d.f}, nil }

type migrationFakeConn struct{ f *migrationFake }

func (c *migrationFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the fake driver does not prepare")
}
func (c *migrationFakeConn) Close() error { return nil }
func (c *migrationFakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *migrationFakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if c.f.beginErr != nil {
		return nil, c.f.beginErr
	}
	c.f.record("begin")
	return &migrationFakeTx{c.f}, nil
}
func (c *migrationFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.HasPrefix(query, "SET LOCAL lock_timeout") && c.f.setErr != nil:
		return nil, c.f.setErr
	case strings.HasPrefix(query, "SET LOCAL lock_timeout"):
		c.f.record("set " + strings.TrimPrefix(query, "SET LOCAL "))
	case query == "SELECT inject":
		if err := c.f.scripted(); err != nil {
			c.f.record("inject failed")
			return nil, err
		}
		c.f.record("inject ok")
	default:
		c.f.record("exec " + query)
	}
	return driver.RowsAffected(0), nil
}

type migrationFakeTx struct{ f *migrationFake }

func (tx *migrationFakeTx) Commit() error {
	tx.f.record("commit")
	return tx.f.commitErr
}
func (tx *migrationFakeTx) Rollback() error { tx.f.record("rollback"); return nil }

func migrationFakeConnection(t *testing.T, script ...error) (*sql.Conn, *migrationFake) {
	t.Helper()
	f := &migrationFake{script: script}
	db := sql.OpenDB(migrationFakeConnector{f})
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, f
}

func migrationInject(tx *sql.Tx) error {
	_, err := tx.ExecContext(context.Background(), "SELECT inject")
	return err
}

func TestMigrationDeadlockIsRetriedWithRollbackAndDelays(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	conn, fake := migrationFakeConnection(t, deadlock, deadlock, nil)
	var slept []time.Duration
	err := withMigrationRetry(context.Background(), conn, func(d time.Duration) { slept = append(slept, d) }, migrationInject)
	if err != nil {
		t.Fatalf("want success after two deadlocks, got: %v", err)
	}
	if want := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond}; len(slept) != 2 || slept[0] != want[0] || slept[1] != want[1] {
		t.Errorf("sleeps = %v, want %v", slept, want)
	}
	lock := "set lock_timeout = '" + migrationLockTimeoutText() + "'"
	want := []string{
		"begin", lock, "inject failed", "rollback",
		"begin", lock, "inject failed", "rollback",
		"begin", lock, "inject ok", "commit",
	}
	if got := fake.log(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events:\n got  %v\n want %v", got, want)
	}
	for _, line := range []string{"[migrate] deadlock, retry 1/4", "[migrate] deadlock, retry 2/4"} {
		if !strings.Contains(logs(), line) {
			t.Errorf("log has no %q:\n%s", line, logs())
		}
	}
}

func TestMigrationDeadlockFailsAfterFourRetries(t *testing.T) {
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	conn, fake := migrationFakeConnection(t, deadlock, deadlock, deadlock, deadlock, deadlock, deadlock)
	var slept []time.Duration
	err := withMigrationRetry(context.Background(), conn, func(d time.Duration) { slept = append(slept, d) }, migrationInject)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
		t.Fatalf("want the deadlock error after the retries, got: %v", err)
	}
	want := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second}
	if len(slept) != len(want) {
		t.Fatalf("sleeps = %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, slept[i], want[i])
		}
	}
	counts := map[string]int{}
	for _, e := range fake.log() {
		counts[e]++
	}
	if counts["begin"] != 5 || counts["rollback"] != 5 || counts["commit"] != 0 {
		t.Errorf("events: %d begins, %d rollbacks, %d commits; want 5, 5, 0", counts["begin"], counts["rollback"], counts["commit"])
	}
}

func TestMigrationNonDeadlockErrorIsNotRetried(t *testing.T) {
	failure := &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	conn, fake := migrationFakeConnection(t, failure)
	err := withMigrationRetry(context.Background(), conn, func(time.Duration) { t.Error("a non-deadlock error must not be retried") }, migrationInject)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("want the original error, got: %v", err)
	}
	counts := map[string]int{}
	for _, e := range fake.log() {
		counts[e]++
	}
	if counts["begin"] != 1 || counts["rollback"] != 1 || counts["commit"] != 0 {
		t.Errorf("events: %v", fake.log())
	}
}

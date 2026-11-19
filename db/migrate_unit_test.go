package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Unit tests of the runner that need no database.

// AC21: the loader rejects CONCURRENTLY and VACUUM outside comments (a migration runs inside a
// transaction, where neither is allowed) and accepts the words in comments, as upstream's 0004 has them.
func TestLoadMigrationsLint(t *testing.T) {
	upstream0004Comment := `-- This is a plain (non-concurrent) CREATE INDEX: migrations run in a
-- transaction, where CONCURRENTLY is not allowed.
--   CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_llm_usage_ts ON llm_usage(ts);
CREATE INDEX IF NOT EXISTS idx_llm_usage_ts ON llm_usage(ts);
`
	accepted := map[string]string{
		"upstream 0004 comment":     upstream0004Comment,
		"block comment":             "/* VACUUM FULL llm_usage; */ SELECT 1;",
		"nested block comment":      "/* a /* CONCURRENTLY */ VACUUM */ SELECT 1;",
		"trailing line comment":     "SELECT 1; -- vacuum later\n",
		"word inside a word":        "ALTER TABLE t SET (autovacuum_enabled = false);",
		"comment marker in text":    "SELECT '-- not a comment'; SELECT 2;",
		"blank file":                "",
		"unterminated line comment": "SELECT 1; -- CONCURRENTLY",
	}
	for name, text := range accepted {
		if _, err := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(text)}}); err != nil {
			t.Errorf("%s: want the file accepted, got: %v", name, err)
		}
	}
	rejected := map[string]string{
		"create index concurrently": "CREATE INDEX CONCURRENTLY idx_a ON t(a);",
		"lower case":                "create index concurrently idx_a on t(a);",
		"vacuum":                    "VACUUM llm_usage;",
		"vacuum analyze":            "SELECT 1;\nvacuum analyze llm_usage;",
		"after a comment line":      "-- ok\nDROP INDEX CONCURRENTLY idx_a;",
		"after a block comment":     "/* ok */ VACUUM;",
		"after a string with --":    "SELECT '--'; VACUUM;",
	}
	for name, text := range rejected {
		_, err := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(text)}})
		if err == nil {
			t.Errorf("%s: want the file rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), "0001_a.sql") {
			t.Errorf("%s: error %q does not name the file", name, err)
		}
	}
}

// AC27 (unit): which parts of a PostgreSQL error reach the text.
func TestMigrationErrorText(t *testing.T) {
	canary := dbtest.RowCanary
	cases := []struct {
		name    string
		err     error
		want    []string
		notWant []string
	}{
		{"class 22 quotes input: no message, no detail", &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "` + canary + `"`,
			Detail: "detail " + canary, Where: "where " + canary, TableName: "t", ColumnName: "c"},
			[]string{"SQLSTATE 22P02", "table t", "column c"}, []string{canary, "invalid input"}},
		{"class 23 names the constraint, never the failing row", &pgconn.PgError{Code: "23514", Message: `new row for relation "t" violates check constraint "t_x_check"`,
			Detail: "Failing row contains (" + canary + ")", ConstraintName: "t_x_check", TableName: "t", SchemaName: "public"},
			[]string{"SQLSTATE 23514", "schema public", "table t", "constraint t_x_check", `violates check constraint "t_x_check"`}, []string{canary, "Failing row"}},
		{"class 42 names the object", &pgconn.PgError{Code: "42P01", Message: `relation "nope" does not exist`},
			[]string{"SQLSTATE 42P01", `relation "nope" does not exist`}, nil},
		{"class 40 deadlock without the process detail", &pgconn.PgError{Code: "40P01", Message: "deadlock detected", Detail: "Process 1 waits for ShareLock on " + canary},
			[]string{"SQLSTATE 40P01", "deadlock detected"}, []string{canary}},
		{"class 55 lock timeout", &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"},
			[]string{"SQLSTATE 55P03", "lock timeout"}, nil},
		{"class 57 cancel", &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"},
			[]string{"SQLSTATE 57014", "statement timeout"}, nil},
		{"another class: code only", &pgconn.PgError{Code: "XX000", Message: "internal " + canary}, []string{"SQLSTATE XX000"}, []string{canary, "internal"}},
		{"class 08: code only", &pgconn.PgError{Code: "08006", Message: "connection failure host=db.internal"}, []string{"SQLSTATE 08006"}, []string{"db.internal"}},
		{"connection error", &net.OpError{Op: "dial", Err: errors.New("dial tcp 10.0.0.5:5432 password=" + canary)}, []string{"connection error"}, []string{"10.0.0.5", canary}},
		{"other error", errors.New("driver said " + canary), []string{"details withheld"}, []string{canary}},
		{"wrapped PostgreSQL error", fmt.Errorf("record: %w", &pgconn.PgError{Code: "22012", Message: "division by zero"}), []string{"SQLSTATE 22012"}, []string{"division"}},
		{"context error", context.DeadlineExceeded, []string{"context deadline exceeded"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := migrationErrorText(c.err)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("text %q does not contain %q", got, w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("text %q must not contain %q", got, w)
				}
			}
		})
	}

	// The wrapper keeps the cause for errors.As and prints only the text.
	cause := &pgconn.PgError{Code: "22P02", Message: "quotes " + canary}
	wrapped := newMigrationError("migration 5002 (5002_x.sql)", cause)
	var pgErr *pgconn.PgError
	if !errors.As(wrapped, &pgErr) || pgErr != cause {
		t.Error("the migration error does not unwrap to the PostgreSQL error")
	}
	if strings.Contains(wrapped.Error(), canary) || !strings.HasPrefix(wrapped.Error(), "migration 5002 (5002_x.sql): SQLSTATE 22P02") {
		t.Errorf("wrapped text = %q", wrapped.Error())
	}
}

func TestAllowNewerVersion(t *testing.T) {
	cases := []struct {
		env     string
		version int
		set     bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"   ", 0, false, false},
		{"5999", 5999, true, false},
		{"0004", 4, true, false},
		{" 5002 ", 5002, true, false},
		{"0000", 0, false, true},
		{"4", 0, false, true},
		{"59999", 0, false, true},
		{"5002x", 0, false, true},
		{"-5002", 0, false, true},
		{"newest", 0, false, true},
	}
	for _, c := range cases {
		t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", c.env)
		v, set, err := allowNewerVersion()
		if (err != nil) != c.wantErr || v != c.version || set != c.set {
			t.Errorf("ARTEX_SCHEMA_ALLOW_NEWER=%q: got (%d, %v, %v), want (%d, %v, error=%v)", c.env, v, set, err, c.version, c.set, c.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "ARTEX_SCHEMA_ALLOW_NEWER") {
			t.Errorf("ARTEX_SCHEMA_ALLOW_NEWER=%q: error %q does not name the variable", c.env, err)
		}
	}
}

// An invalid override value stops the start before the database is touched.
func TestOpenRejectsInvalidAllowNewerBeforeConnecting(t *testing.T) {
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "soon")
	// The DSN is unreachable on purpose: the error must come from the variable, not the connection.
	d, err := openWithMigrations("postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1", embeddedMigrations())
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded with an invalid ARTEX_SCHEMA_ALLOW_NEWER")
	}
	if !strings.Contains(err.Error(), "ARTEX_SCHEMA_ALLOW_NEWER") {
		t.Errorf("error %q does not name the variable", err)
	}
}

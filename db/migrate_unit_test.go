package db

import (
	"context"
	"database/sql"
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

// A migration file runs as one simple-protocol Exec inside the runner's transaction, so a statement
// that controls the transaction would end it. The loader rejects each such statement outside comments,
// string literals and dollar-quoted bodies, and accepts BEGIN and END inside PL/pgSQL bodies.
func TestLoadMigrationsRejectsTransactionControl(t *testing.T) {
	rejected := map[string]string{
		"COMMIT":                      "CREATE TABLE a(x int);\nCOMMIT;",
		"COMMIT lower case":           "create table a(x int); commit;",
		"COMMIT without semicolon":    "SELECT 1; COMMIT",
		"COMMIT PREPARED":             "COMMIT PREPARED 'x';",
		"BEGIN":                       "BEGIN;\nCREATE TABLE a(x int);",
		"BEGIN TRANSACTION":           "BEGIN TRANSACTION ISOLATION LEVEL SERIALIZABLE;",
		"START TRANSACTION":           "SELECT 1; START   TRANSACTION;",
		"ROLLBACK":                    "SELECT 1; ROLLBACK;",
		"ROLLBACK TO":                 "SELECT 1; ROLLBACK TO SAVEPOINT s;",
		"SAVEPOINT":                   "SAVEPOINT s;",
		"RELEASE":                     "SAVEPOINT s; RELEASE s;",
		"PREPARE TRANSACTION":         "SELECT 1; PREPARE TRANSACTION 'x';",
		"END":                         "SELECT 1;\nEND;",
		"END TRANSACTION":             "SELECT 1; END TRANSACTION;",
		"ABORT":                       "SELECT 1; ABORT;",
		"after a block comment":       "/* c */ COMMIT;",
		"after a line comment":        "-- c\nCOMMIT;",
		"after a string with a quote": "SELECT 'it''s'; COMMIT;",
		"after an escape string":      "SELECT E'it\\'s -- x'; COMMIT;",
		"after a dollar body with --": "SELECT $$a -- b$$; COMMIT;",
		"after a quoted identifier":   `SELECT 1 AS "a;b"; COMMIT;`,
		"after a tagged body":         "DO $b$ BEGIN PERFORM 1; END $b$;\nCOMMIT;",
		"after BEGIN ATOMIC function": "CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; END; COMMIT;",
	}
	for name, text := range rejected {
		_, err := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(text)}})
		if err == nil {
			t.Errorf("%s: want the file rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), "0001_a.sql") || !strings.Contains(err.Error(), "transaction") {
			t.Errorf("%s: error %q must name the file and the transaction", name, err)
		}
	}
	accepted := map[string]string{
		"DO block":                    "DO $$ BEGIN PERFORM 1; END $$;",
		"DO block with a tag":         "DO $body$\nBEGIN\n  PERFORM 1;\n  COMMIT;\nEND\n$body$;",
		"function body":               "CREATE OR REPLACE FUNCTION f() RETURNS trigger LANGUAGE plpgsql AS $fn$\nBEGIN\n  RETURN NEW;\nEND;\n$fn$;",
		"function body in a string":   "CREATE FUNCTION f() RETURNS int LANGUAGE plpgsql AS 'BEGIN RETURN 1; END;';",
		"BEGIN ATOMIC function":       "CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; END;\nSELECT 2;",
		"keywords in comments":        "-- COMMIT;\n/* ROLLBACK; */ SELECT 1;",
		"keywords in strings":         "INSERT INTO t VALUES ('COMMIT; BEGIN; END');",
		"words inside identifiers":    "ALTER TABLE t ADD COLUMN begin_at timestamptz, ADD COLUMN commit_id int;",
		"column named like a keyword": "SELECT 1 AS \"commit\";",
		"CASE ... END":                "SELECT CASE WHEN true THEN 1 ELSE 2 END;",
		"PREPARE a statement":         "PREPARE p AS SELECT 1; DEALLOCATE p;",
		"dollar parameter":            "PREPARE p(int) AS SELECT $1; DEALLOCATE p;",
		"blank file":                  "",
	}
	for name, text := range accepted {
		if _, err := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(text)}}); err != nil {
			t.Errorf("%s: want the file accepted, got: %v", name, err)
		}
	}
}

// A '--' inside a dollar-quoted body is not a comment marker for the lint: it must not hide a
// forbidden word after it.
func TestLoadMigrationsLintSeesPastDashesInDollarBody(t *testing.T) {
	text := "SELECT $$a -- b$$; VACUUM t;"
	if _, err := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(text)}}); err == nil {
		t.Error("want VACUUM after a dollar body with -- rejected")
	}
	text = "SELECT $t$-- x$t$; CREATE INDEX CONCURRENTLY i ON t(a);"
	if _, err := loadMigrations(fstest.MapFS{"0001_a.sql": {Data: []byte(text)}}); err == nil {
		t.Error("want CONCURRENTLY after a tagged dollar body with -- rejected")
	}
}

// Every shipped migration passes the lint (upstream 0001-0003, the fork's 5001 and any later file).
func TestLoadMigrationsAcceptsShippedFiles(t *testing.T) {
	ms, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
}

// Errors of BeginTx, SET LOCAL and Commit are content-free like the others: a commit-time error can
// carry the failing row (a deferred constraint, a serialization failure).
func TestMigrationTxErrorsAreContentFree(t *testing.T) {
	canary := dbtest.RowCanary
	pgErr := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax: "` + canary + `"`, Detail: "detail " + canary}
	cases := []struct {
		name  string
		fake  func(f *migrationFake)
		want  string
		cause error
	}{
		{"commit", func(f *migrationFake) { f.commitErr = pgErr }, "commit", pgErr},
		{"begin", func(f *migrationFake) { f.beginErr = pgErr }, "begin", pgErr},
		{"set lock_timeout", func(f *migrationFake) { f.setErr = pgErr }, "lock_timeout", pgErr},
		{"commit, plain error", func(f *migrationFake) { f.commitErr = errors.New("driver said " + canary) }, "commit", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := migrationFakeConnection(t)
			c.fake(fake)
			err := runMigrationTx(context.Background(), conn, func(*sql.Tx) error { return nil })
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "detail "+canary) {
				t.Errorf("error text %q carries row content", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error text %q does not say which step failed (%q)", err, c.want)
			}
			if c.cause != nil {
				var got *pgconn.PgError
				if !errors.As(err, &got) || got != c.cause {
					t.Error("the error does not unwrap to the PostgreSQL error")
				}
			}
		})
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

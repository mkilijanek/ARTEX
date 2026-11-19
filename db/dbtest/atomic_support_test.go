package dbtest

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The summary is what the new tests print on failure (AC12): it must not carry the
// message of a PostgreSQL error (it can quote a row), a connection error (host, user,
// database) or any text with a canary.
func TestUpgradeErrorSummaryNeverCarriesRowOrConnectionText(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://upg:realpassword1@db.invalid/artextest_dbtest")
	pgErr := &pgconn.PgError{
		Code: "23514", SchemaName: "public", TableName: "task_scope", ConstraintName: "task_scope_kind_check",
		Message: "new row violates " + RowCanary, Detail: "Failing row contains (" + RowCanary + ")",
	}
	got := AtomicErrorSummary(fmt.Errorf("apply schema: %w", pgErr))
	for _, want := range []string{"SQLSTATE 23514", "table task_scope", "constraint task_scope_kind_check"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	if _, leaked := FindSecret(got); leaked {
		t.Errorf("summary of a PostgreSQL error carries a secret: %q", got)
	}
	if !AtomicErrorMentions(pgErr, "task_scope_kind_check") || AtomicErrorMentions(errors.New("x"), "x") {
		t.Error("AtomicErrorMentions did not identify the constraint of a PostgreSQL error only")
	}

	dial := fmt.Errorf("ping postgres (postgres://upg@db.invalid/x): %w", &net.OpError{Op: "dial", Err: errors.New("refused")})
	if got := AtomicErrorSummary(dial); strings.Contains(got, "db.invalid") || strings.Contains(got, "upg") {
		t.Errorf("summary of a connection error carries the connection string: %q", got)
	}
	if got := AtomicErrorSummary(errors.New("bad value " + RowCanary)); strings.Contains(got, RowCanary) {
		t.Errorf("summary carries the row canary: %q", got)
	}
	if got := AtomicErrorSummary(errors.New("restore finding traffic: evidence snapshot metadata hash mismatch")); !strings.Contains(got, "hash mismatch") {
		t.Errorf("summary dropped the application's own message: %q", got)
	}
}

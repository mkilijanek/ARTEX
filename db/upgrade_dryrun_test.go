package db

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// AC15 (Stage 1 delivers the guarded check, not a run on real data). The dry run is a
// runbook step (docs/schema-upgrade.md): it starts its own PostgreSQL container with
// its own volume and port, restores a dump into a database whose name ends in
// _upgradecheck, and runs TestDryRunRestoredCopy against it with
// ARTEX_UPGRADECHECK_DSN. Without that variable the run does not happen.
const dryRunEnv = "ARTEX_UPGRADECHECK_DSN"

// failRecorder keeps the message of a Fatalf and stops the goroutine, as a real testing.TB does.
type failRecorder struct {
	testing.TB
	msg string
}

func (f *failRecorder) Helper()        {}
func (f *failRecorder) Cleanup(func()) {}
func (f *failRecorder) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// startupFailure runs startup against dsn and returns the failure message it reports.
func startupFailure(t *testing.T, dsn string) string {
	t.Helper()
	rec := &failRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		startup(rec, dsn)
	}()
	<-done
	if rec.msg == "" {
		t.Fatal("startup did not fail")
	}
	return rec.msg
}

// The dry run runs startup on a copy of a real database: a PostgreSQL message can quote a row
// value, a connection error names host and user. startup reports SQLSTATE and object names, or
// a fixed text for a connection error, and nothing the driver wrote.
func TestUpgradeStartupFailureIsReportedWithoutDriverText(t *testing.T) {
	// A server that is not there; the DSN carries the fixed password canary.
	msg := startupFailure(t, "postgres://upg:"+dbtest.PasswordCanary+"@127.0.0.1:1/artextest_db?sslmode=disable&connect_timeout=2")
	if !strings.Contains(msg, "startup: Open failed (no SQLSTATE: connection error (details withheld))") {
		t.Errorf("a connection error is not reported as the fixed text: %q", msg)
	}
	for _, leak := range []string{"127.0.0.1", "upg", dbtest.PasswordCanary, "refused"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the connection failure message holds %q", leak)
		}
	}

	// A row that violates a re-added CHECK: the PostgreSQL message names the constraint and the
	// relation, which the summary keeps; the message text itself is dropped.
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "startupfail")
	tmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
	tmp.Load(t, "violation base rows", atomicFixture(t, "violation-base.sql"))
	tmp.Load(t, "violating row", atomicFixture(t, "violation-task-scope-kind.sql"))
	msg = startupFailure(t, tmp.DSN)
	if !strings.Contains(msg, "startup: Open failed (SQLSTATE 23514, schema public, table task_scope, constraint task_scope_kind_check)") {
		t.Errorf("a PostgreSQL error is not reported as SQLSTATE and object names: %q", msg)
	}
	for _, leak := range []string{"violated by some row", dbtest.RowCanary, "apply schema"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the failure message holds %q", leak)
		}
	}
	dbtest.AssertNoSecret(t, "startup failure message", msg)
}

// The server of ARTEX_PG_DSN is shared: it holds other databases, so the inventory
// rule must refuse it.
func TestUpgradeDryRunRefusesAServerThatHoldsOtherDatabases(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "dryrun") // a database the dry run did not create
	admin, err := dbtest.AdminQueryer(os.Getenv(dbtest.EnvDSN))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	err = dbtest.CheckDryRun(context.Background(), admin, "restored_upgradecheck")
	if err == nil {
		t.Fatal("the dry run accepted a server that holds other databases")
	}
	if !strings.Contains(err.Error(), tmp.Name) {
		t.Fatalf("refusal does not name the offending database %s: %v", tmp.Name, err)
	}
	dbtest.AssertNoSecret(t, "dry-run refusal", err.Error())
	if err := dbtest.CheckDryRun(context.Background(), admin, tmp.Name); err == nil {
		t.Fatal("the dry run accepted a target that does not end in _upgradecheck")
	}
}

// TestDryRunRestoredCopy runs startup twice on a restored copy and prints a content-free
// report: row counts per table before and after, the state of the migration ledger before and after
// (absent or its versions: numbers only, never a name, a checksum or a row), catalog differences against
// the reference by object name. Not part of the gate (it needs the user's approval and its
// own server); it does nothing without ARTEX_UPGRADECHECK_DSN.
func TestDryRunRestoredCopy(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(dryRunEnv))
	if dsn == "" {
		t.Skipf("dry run not requested: %s is unset (runbook step, see docs/schema-upgrade.md)", dryRunEnv)
	}
	name, err := dbtest.ParsedDatabase(dsn)
	if err != nil {
		t.Fatalf("%s: %v", dryRunEnv, err)
	}
	ctx := context.Background()
	admin, err := dbtest.AdminQueryer(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := dbtest.CheckDryRun(ctx, admin, name); err != nil {
		t.Fatal(err)
	}

	conn, err := dbtest.OpenDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before, err := dbtest.RowCounts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	// Every start deletes the prompt variables that were renamed away (db/db.go, "cleanup
	// renamed vars"). That deletion is intended, so the rows present before startup are
	// counted and exactly that many may disappear from agent_prompt_vars; nothing else may.
	var obsoleteVars int64
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle', 'CoverageGaps', 'Now')`).Scan(&obsoleteVars); err != nil {
		t.Fatalf("count obsolete prompt variables: %s", dbtest.AtomicErrorSummary(err))
	}
	ledgerBefore, err := dbtest.LedgerStateOf(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	startup(t, dsn)
	ledgerAfterFirst, err := dbtest.LedgerStateOf(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	afterFirst, err := dbtest.RowCounts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	startup(t, dsn)
	afterSecond, err := dbtest.RowCounts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	ledgerAfter, err := dbtest.LedgerStateOf(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := dbtest.Catalog(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	report := dbtest.Report{
		Database: name,
		Before:   before,
		After:    afterSecond,
		Catalog:  dbtest.DiffLines(loadReference(t), dbtest.Lines(entries)),
	}
	text := report.String() + fmt.Sprintf("ledger schema_migrations: %s -> %s\n", ledgerBefore, ledgerAfter)
	dbtest.AssertNoSecret(t, "dry-run report", text)
	t.Log("\n" + text)
	var unexpected []string
	for _, table := range report.RemovedRows() {
		if table == "agent_prompt_vars" && before[table]-afterSecond[table] == obsoleteVars {
			t.Logf("expected: startup removed %d obsolete prompt variable row(s) from agent_prompt_vars", obsoleteVars)
			continue
		}
		unexpected = append(unexpected, table)
	}
	if len(unexpected) > 0 {
		t.Errorf("startup removed rows from: %s", strings.Join(unexpected, ", "))
	}
	if d := dbtest.DiffMaps(afterFirst, afterSecond); len(d) > 0 {
		t.Errorf("the second startup changed row counts: %s", strings.Join(d, ", "))
	}
	// The ledger: a copy of a database from before the runner has none and ends with exactly the versions this
	// binary ships (the baseline set, adopted, then every later migration, applied); a copy that already has one
	// keeps its versions and holds at least the baseline set. The second start changes nothing.
	shipped, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	shippedVersions := make([]int, 0, len(shipped))
	for _, m := range shipped {
		shippedVersions = append(shippedVersions, m.version)
	}
	switch {
	case !ledgerBefore.Present && !ledgerAfter.LedgerIs(shippedVersions...):
		t.Errorf("the ledger was absent before startup and is %s after it, want exactly the shipped versions %v", ledgerAfter, shippedVersions)
	case ledgerBefore.Present && !ledgerAfter.LedgerHas(ledgerBefore.Versions...):
		t.Errorf("the ledger was %s before startup and is %s after it: a version disappeared", ledgerBefore, ledgerAfter)
	case !ledgerAfter.LedgerHas(baselineVersions...):
		t.Errorf("the ledger is %s after startup, it lacks a baseline version", ledgerAfter)
	}
	if ledgerAfter.String() != ledgerAfterFirst.String() {
		t.Errorf("the second startup changed the ledger: %s -> %s", ledgerAfterFirst, ledgerAfter)
	}
}

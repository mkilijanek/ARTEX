package db

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mkilijanek/artex/db/dbtest"
)

// The Stage 1 suite on the migration runner (docs/phase-d2-stage2-plan.md AC3, AC4, AC16): what the
// first start of a binary with the ledger does to a database that has none, from every recorded shape.
// Like every test of the suite, none of these skips: a missing or refused server is a failure.

// ledgerCatalogLines is the number of reference lines that belong to the ledger table: the table, its four
// columns, its primary key constraint and the index behind it.
const ledgerCatalogLines = 7

// sameLedgerRows compares two ledger reads field by field, times by instant.
func sameLedgerRows(a, b []appliedMigration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].version != b[i].version || a[i].name != b[i].name || a[i].checksum != b[i].checksum || !a[i].appliedAt.Equal(b[i].appliedAt) {
			return false
		}
	}
	return true
}

func isLedgerLine(l string) bool { return strings.Contains(l, "schema_migrations") }

// referenceWithoutLedger is the reference catalog minus the lines of the ledger table: the catalog of a
// database that a release without the ledger left behind.
func referenceWithoutLedger(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, l := range loadReference(t) {
		if !isLedgerLine(l) {
			out = append(out, l)
		}
	}
	return out
}

// AC3: the reference catalog holds the ledger table as its only addition to the catalog of the releases
// before the runner: the table, four columns, the primary key and its index, and no other schema_migrations
// line.
func TestUpgradeLedgerTableIsInTheReference(t *testing.T) {
	var kinds []string
	for _, l := range loadReference(t) {
		if !isLedgerLine(l) {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			t.Fatalf("a reference line has no kind and name: %q", l)
		}
		kinds = append(kinds, f[0])
	}
	count := map[string]int{}
	for _, k := range kinds {
		count[k]++
	}
	if len(kinds) != ledgerCatalogLines || count["table"] != 1 || count["column"] != 4 || count["constraint"] != 1 || count["index"] != 1 {
		t.Errorf("the reference has %d schema_migrations lines %v, want %d: one table, four columns, one constraint, one index",
			len(kinds), count, ledgerCatalogLines)
	}
	if n := len(referenceWithoutLedger(t)); n != len(loadReference(t))-ledgerCatalogLines {
		t.Errorf("filtering the ledger leaves %d lines of %d", n, len(loadReference(t)))
	}
}

// AC3: a fresh Open, with no Ensure call and no helper, leaves exactly the ledger versions 0001, 0002, 0003, 0004
// and 5001 with the names and sha256 of the embedded files, and the catalog of the reference.
func TestUpgradeLedgerFreshOpenAdoptsTheBaselineWithoutEnsure(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "ledgerfresh")
	d, err := Open(tmp.DSN)
	if err != nil {
		t.Fatalf("Open on an empty database failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer d.Close()

	requireEmbeddedLedger(t, readLedger(t, d.DB))
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("the catalog after Open alone differs from %s (%d objects):\n  %s", referenceFile, len(diff), strings.Join(diff, "\n  "))
	}
	for _, table := range []string{"llm_records", "llm_usage", "schema_migrations"} {
		if !migrationTableExists(t, d.DB, table) {
			t.Errorf("Open alone did not create %s", table)
		}
	}
	for _, want := range []string{"[migrate] ledger absent: adopting baseline (4)", "[migrate] adopted baseline in", "[migrate] up to date (upstream=0004, fork=5002)"} {
		if !strings.Contains(logs(), want) {
			t.Errorf("the log has no %q line", want)
		}
	}
	dbtest.AssertNoSecret(t, "log output", logs())
}

// AC4: every shape of an older database (the three recorded releases without the runner and the synthetic
// legacy one) has no ledger before its first start and the baseline plus 0004 after it; the shape of the
// release with the runner (s2a) has the baseline ledger before and keeps those four rows as they were. A
// second start leaves the ledger as it was, applied_at included. Rows, catalog and the rest of the second
// start are the business of TestUpgradeRowsPreserved, TestUpgradeConvergesWithFreshCatalog and
// TestUpgradeRepeatedStartupChangesNothing.
func TestUpgradeLedgerAdoptsEveryShape(t *testing.T) {
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			srv := dbtest.RequireServer(t)
			tmp := buildShape(t, srv, sh)
			ctx := context.Background()
			before, err := dbtest.LedgerStateOf(ctx, tmp.Conn(t))
			if err != nil {
				t.Fatal(err)
			}
			ledgered := len(recordedShapeNamed(t, sh.recorded).migrations) > 0
			if before.Present != ledgered {
				t.Fatalf("shape %s: the ledger is present before its first start: %v, want %v (%s)", sh.name, before.Present, ledgered, before)
			}
			var baselineRows []appliedMigration
			if ledgered {
				baselineRows = readLedger(t, tmp.Conn(t))
				requireBaselineLedger(t, baselineRows)
			}

			d1 := startup(t, tmp.DSN)
			first := readLedger(t, d1.DB)
			requireEmbeddedLedger(t, first)
			if ledgered {
				var kept []appliedMigration
				for _, row := range first {
					if slices.Contains(baselineVersions, row.version) {
						kept = append(kept, row)
					}
				}
				if !sameLedgerRows(baselineRows, kept) {
					t.Errorf("shape %s: the first start changed a row of the ledger it found", sh.name)
				}
			}

			d2 := startup(t, tmp.DSN)
			second := readLedger(t, d2.DB)
			requireEmbeddedLedger(t, second)
			if !sameLedgerRows(first, second) {
				t.Errorf("shape %s: the second start changed the ledger (a row, applied_at included)", sh.name)
			}
		})
	}
}

// waitForLocks polls the advisory lock key until ok accepts the granted and waiting counts, for up to a
// minute (a loaded host only takes longer). The poll runs on its own pool connection.
func waitForLocks(t *testing.T, q dbtest.Queryer, what string, ok func(granted, waiting int) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		granted, waiting, err := dbtest.AtomicAdvisoryLocks(context.Background(), q, schemaMigrationLockKey)
		if err != nil {
			t.Fatal(err)
		}
		if ok(granted, waiting) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not seen within a minute (%d granted and %d waiting locks on the key)", what, granted, waiting)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// requireStillRunning fails when ch has a value: the operation under test returned although it must wait.
func requireStillRunning[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s returned while it had to wait", what)
	case <-time.After(time.Second):
	}
}

// AC16: an archive restore (the real restoreTaskArchive, which takes pg_advisory_xact_lock(7337741001) in
// its transaction) and a starting Open exclude each other, in both directions, on a database with the runner:
//
//	(a) the restore holds the key, blocked on the row lock of its first statement: Open waits for the key and
//	    finishes after the restore ended (the restore finds no archive: sql.ErrNoRows, which is what it does
//	    on an empty database; the point is that it ran in its transaction);
//	(b) Open holds the key, blocked on a table lock in schema.sql: the restore waits and runs after Open.
//
// Nothing is guessed with sleeps: the test polls pg_locks for the holder and the waiter.
func TestUpgradeLedgerArchiveRestoreAndStartupExclude(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "ledgerarchive")
	startup(t, tmp.DSN)
	restorer := &DB{tmp.Conn(t)}
	poll, blocker := tmp.Conn(t), tmp.Conn(t)
	ctx := context.Background()
	snapshot := &TaskArchiveSnapshot{FormatVersion: 3}

	hold := func(table, mode string) *sql.Tx {
		t.Helper()
		tx, err := blocker.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin failed (%s)", dbtest.AtomicErrorSummary(err))
		}
		t.Cleanup(func() { tx.Rollback() }) //nolint:errcheck // no-op once released
		if _, err := tx.ExecContext(ctx, `LOCK TABLE `+table+` IN `+mode+` MODE`); err != nil {
			t.Fatalf("lock %s failed (%s)", table, dbtest.AtomicErrorSummary(err))
		}
		return tx
	}
	type result struct {
		d   *DB
		err error
	}
	open := func() <-chan result {
		ch := make(chan result, 1)
		go func() {
			d, err := Open(tmp.DSN)
			ch <- result{d, err}
		}()
		return ch
	}
	restore := func() <-chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := restorer.RestoreTaskArchive(1, snapshot, 0)
			ch <- err
		}()
		return ch
	}

	// (a) restore first.
	tx := hold("task_archives", "ACCESS EXCLUSIVE")
	restored := restore()
	waitForLocks(t, poll, "the restore holding the key", func(granted, _ int) bool { return granted == 1 })
	opened := open()
	waitForLocks(t, poll, "Open waiting for the key held by the restore", func(granted, waiting int) bool { return granted == 1 && waiting == 1 })
	requireStillRunning(t, opened, "Open")
	if err := tx.Rollback(); err != nil {
		t.Fatalf("release the table lock failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	select {
	case err := <-restored:
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("the restore ended with %s, want sql.ErrNoRows (no archive row)", dbtest.AtomicErrorSummary(err))
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the restore did not finish after the table lock was released")
	}
	select {
	case r := <-opened:
		if r.err != nil {
			t.Fatalf("Open failed after the restore ended: %s", dbtest.AtomicErrorSummary(r.err))
		}
		r.d.Close()
	case <-time.After(2 * time.Minute):
		t.Fatal("Open did not finish after the restore ended")
	}

	// (b) Open first. Its schema.sql ALTERs findings, which waits for the ACCESS SHARE lock held here.
	tx = hold("findings", "ACCESS SHARE")
	opened = open()
	waitForLocks(t, poll, "Open holding the key", func(granted, _ int) bool { return granted == 1 })
	restored = restore()
	waitForLocks(t, poll, "the restore waiting for the key held by Open", func(granted, waiting int) bool { return granted == 1 && waiting == 1 })
	requireStillRunning(t, restored, "the restore")
	if err := tx.Rollback(); err != nil {
		t.Fatalf("release the table lock failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	select {
	case r := <-opened:
		if r.err != nil {
			t.Fatalf("Open failed: %s", dbtest.AtomicErrorSummary(r.err))
		}
		defer r.d.Close()
	case <-time.After(2 * time.Minute):
		t.Fatal("Open did not finish after the table lock was released")
	}
	select {
	case err := <-restored:
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("the restore ended with %s, want sql.ErrNoRows (no archive row)", dbtest.AtomicErrorSummary(err))
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the restore did not finish after Open ended")
	}
	requireEmbeddedLedger(t, readLedger(t, restorer.DB))
}

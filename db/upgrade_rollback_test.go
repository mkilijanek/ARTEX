package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Rollback of the first release with the migration ledger (docs/phase-d2-stage2-plan.md 3.8, AC22, AC24).
//
// Redeploying 0.5.13m on a database that this runner adopted needs no step: 0.5.13m never reads
// schema_migrations. The one optional step, that restores the exact catalog of a database that never had the
// ledger, is the guarded cleanup in testdata/upgrade/rollback-cleanup-ledger.sql, which docs/schema-upgrade.md
// quotes. It drops the ledger only when the ledger holds exactly the baseline set.

const rollbackCleanupFile = "rollback-cleanup-ledger.sql"

// rollbackRunCleanup runs the cleanup file the way psql -v ON_ERROR_STOP=1 would: one script on one session;
// on an error the session is left in an aborted transaction, which is rolled back here (psql would end the
// session). The error is returned as it came.
func rollbackRunCleanup(t *testing.T, pool *sql.DB) error {
	t.Helper()
	return rollbackRunScript(t, pool, rollbackCleanupFile)
}

// rollbackRunScript runs one rollback file of testdata/upgrade as rollbackRunCleanup describes.
func rollbackRunScript(t *testing.T, pool *sql.DB, file string) error {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("take a connection failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer conn.Close()
	_, err = conn.ExecContext(ctx, readUpgradeFile(t, file))
	if err != nil {
		if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil {
			t.Fatalf("roll back the failed cleanup failed (%s)", dbtest.AtomicErrorSummary(rbErr))
		}
	}
	return err
}

// rollbackRowsWithoutLedger is the snapshot without the ledger table: its rows are new after every adoption.
func rollbackRowsWithoutLedger(s *dbtest.RowSnapshot) *dbtest.RowSnapshot {
	out := &dbtest.RowSnapshot{Tables: map[string]*dbtest.TableRows{}}
	for name, rows := range s.Tables {
		if name != "schema_migrations" {
			out.Tables[name] = rows
		}
	}
	return out
}

// AC22 (first half): on a database whose ledger is exactly the baseline set (the database of the release before
// 0004), the cleanup drops the ledger and nothing else (the catalog is the reference without the ledger lines
// and without 0004's index, the rows are those of before), and the next start adopts the baseline again,
// applies 0004 and converges to the reference.
func TestUpgradeRollbackCleanupDropsABaselineLedger(t *testing.T) {
	srv := dbtest.RequireServer(t)
	var r0513 matrixShape
	for _, sh := range matrixShapes {
		if sh.name == "r0513" {
			r0513 = sh
		}
	}
	tmp := buildShape(t, srv, r0513)
	d := startupPrevious(t, tmp.DSN)
	requireBaselineLedger(t, readLedger(t, d.DB))
	rowsBefore, digestsBefore := takeRows(t, d.DB), takeDigests(t, d.DB)

	if err := rollbackRunCleanup(t, d.DB); err != nil {
		t.Fatalf("the cleanup failed on a baseline-only ledger (%s)", dbtest.AtomicErrorSummary(err))
	}
	if migrationTableExists(t, d.DB, "schema_migrations") {
		t.Fatal("the cleanup left the ledger table")
	}
	if diff := dbtest.DiffLines(referenceWithoutLlmUsageTsIndex(referenceWithoutLedger(t)), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("after the cleanup the catalog is not the reference without the ledger (and without the index of 0004, which the release before it does not create) (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if p := dbtest.RepeatProblems(rollbackRowsWithoutLedger(rowsBefore), rollbackRowsWithoutLedger(takeRows(t, d.DB))); len(p) > 0 {
		t.Errorf("the cleanup changed rows (%d):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(context.Background(), d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after the cleanup: %d granted and %d waiting locks on the key (err %v), want 0 and 0", granted, waiting, err)
	}

	// The next start re-adopts, applies 0004 and converges.
	d2 := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d2.DB))
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d2.DB))); len(diff) > 0 {
		t.Errorf("after the cleanup and a start the catalog differs from the reference (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if p := dbtest.RepeatProblems(rollbackRowsWithoutLedger(rowsBefore), rollbackRowsWithoutLedger(takeRows(t, d2.DB))); len(p) > 0 {
		t.Errorf("the start after the cleanup changed rows (%d):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if fell := sequencesThatFell(digestsBefore, takeDigests(t, d2.DB)); len(fell) > 0 {
		t.Errorf("a sequence moved backwards: %s", strings.Join(fell, "; "))
	}
}

// AC22 (second half), negative control (g): on a ledger that holds anything but exactly the baseline set, the
// cleanup RAISES and changes nothing: the ledger table, its rows, the catalog, every row and every sequence
// stay as they were, and the session holds no lock afterwards. The ledgers tried are one with a later migration
// (0004, a synthetic 5002), one that lacks a baseline version, and an empty one.
func TestUpgradeRollbackCleanupRefusesAnyOtherLedger(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "rollbackguard")
	d := startupPrevious(t, tmp.DSN)
	baseline := readLedger(t, d.DB)
	requireBaselineLedger(t, baseline)
	ctx := context.Background()

	// setLedger makes the ledger exactly the given rows.
	setLedger := func(what string, rows []appliedMigration) {
		t.Helper()
		if _, err := d.ExecContext(ctx, `DELETE FROM schema_migrations`); err != nil {
			t.Fatalf("%s: clear the ledger failed (%s)", what, dbtest.AtomicErrorSummary(err))
		}
		for _, row := range rows {
			if _, err := d.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES ($1, $2, $3, $4)`,
				row.version, row.name, row.checksum, row.appliedAt); err != nil {
				t.Fatalf("%s: write the ledger failed (%s)", what, dbtest.AtomicErrorSummary(err))
			}
		}
	}
	extra := func(version int, name string) appliedMigration {
		return appliedMigration{version: version, name: name, checksum: "synthetic", appliedAt: baseline[0].appliedAt}
	}
	for _, tc := range []struct {
		name string
		rows []appliedMigration
	}{
		{"0004 applied", append(append([]appliedMigration{}, baseline...), extra(4, "0004_llm_usage_ts_index.sql"))},
		{"5002 applied", append(append([]appliedMigration{}, baseline...), extra(5002, "5002_synthetic.sql"))},
		{"0003 missing", append(append([]appliedMigration{}, baseline[:2]...), baseline[3])},
		{"ledger empty", nil},
	} {
		setLedger(tc.name, tc.rows)
		before := captureMigrationState(t, tmp)
		err := rollbackRunCleanup(t, d.DB)
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			t.Errorf("%s: the cleanup succeeded and dropped the ledger", tc.name)
		case !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "schema_migrations is not the baseline set"):
			t.Errorf("%s: the cleanup failed with %s, want the guard's exception (P0001, \"not the baseline set\")", tc.name, dbtest.AtomicErrorSummary(err))
		}
		if !migrationTableExists(t, d.DB, "schema_migrations") {
			t.Errorf("%s: the ledger table is gone", tc.name)
		}
		requireMigrationStateUnchanged(t, tc.name+": after the refused cleanup", before, captureMigrationState(t, tmp))
		if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
			t.Errorf("%s: after the refused cleanup: %d granted and %d waiting locks on the key (err %v), want 0 and 0", tc.name, granted, waiting, err)
		}
		if t.Failed() {
			return // a dropped ledger makes the next case meaningless
		}
	}
	setLedger("restore", baseline)
	requireBaselineLedger(t, readLedger(t, d.DB))
}

// AC24: a database the runner adopted (the release before 0004), started by a simulated 0.5.13m binary, keeps
// its rows and gains nothing; after the guarded cleanup the new binary starts, adopts the baseline again,
// applies 0004 and converges to the reference.
//
// The simulated 0.5.13m start is what that binary's Open and server start did, ignoring the ledger: the
// schema.sql of the release (the recorded schema fixture of r0513, which is the frozen db/schema.sql), then
// seedBuiltins (unchanged since 0.5.13m: db/db.go differs only in Open), then the Go DDL of the release (the
// historical goddl fixture; 0.5.13m called EnsureLLMRecordsTable and EnsureLLMUsageTable after Open). It is
// not the 0.5.13m binary: AC25 runs that binary on a copy of a real database.
func TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward(t *testing.T) {
	srv := dbtest.RequireServer(t)
	var r0513 matrixShape
	for _, sh := range matrixShapes {
		if sh.name == "r0513" {
			r0513 = sh
		}
	}
	tmp := buildShape(t, srv, r0513)
	d := startupPrevious(t, tmp.DSN)
	adopted := readLedger(t, d.DB)
	requireBaselineLedger(t, adopted)
	rowsAdopted, digestsAdopted := takeRows(t, d.DB), takeDigests(t, d.DB)

	// The release without the ledger starts on the adopted database.
	tmp.Load(t, "schema.sql of 0.5.13m", readUpgradeFile(t, recordedSchemaFile(t, "r0513")))
	if err := (&DB{tmp.Conn(t)}).seedBuiltins(); err != nil {
		t.Fatalf("seedBuiltins of the simulated 0.5.13m start failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	tmp.Load(t, "Go DDL of 0.5.13m", readUpgradeFile(t, recordedGoDDLFile(t, "r0513")))

	rowsOld, digestsOld := takeRows(t, d.DB), takeDigests(t, d.DB)
	if p := dbtest.RepeatProblems(rowsAdopted, rowsOld); len(p) > 0 {
		t.Errorf("the simulated 0.5.13m start changed rows (%d; only agents.updated_at of built-in agents may change):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if fell := sequencesThatFell(digestsAdopted, digestsOld); len(fell) > 0 {
		t.Errorf("the simulated 0.5.13m start moved a sequence backwards: %s", strings.Join(fell, "; "))
	}
	if diff := dbtest.DiffLines(referenceWithoutLlmUsageTsIndex(loadReference(t)), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("the simulated 0.5.13m start changed the catalog (%d objects; the ledger table stays as an extra table that it never reads):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if !sameLedgerRows(readLedger(t, d.DB), adopted) {
		t.Fatal("the simulated 0.5.13m start changed the ledger")
	}
	if t.Failed() {
		return
	}

	// The optional cleanup restores the catalog of a database that never had the ledger.
	if err := rollbackRunCleanup(t, d.DB); err != nil {
		t.Fatalf("the cleanup failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if diff := dbtest.DiffLines(referenceWithoutLlmUsageTsIndex(referenceWithoutLedger(t)), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("after the cleanup the catalog is not the reference without the ledger and 0004's index (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}

	// The new binary starts again: it adopts the baseline and applies 0004.
	d2 := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d2.DB))
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d2.DB))); len(diff) > 0 {
		t.Errorf("the start after the rollback does not converge to the reference (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if p := dbtest.RepeatProblems(rollbackRowsWithoutLedger(rowsOld), rollbackRowsWithoutLedger(takeRows(t, d2.DB))); len(p) > 0 {
		t.Errorf("the start after the rollback changed rows (%d):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if fell := sequencesThatFell(digestsOld, takeDigests(t, d2.DB)); len(fell) > 0 {
		t.Errorf("the start after the rollback moved a sequence backwards: %s", strings.Join(fell, "; "))
	}
}

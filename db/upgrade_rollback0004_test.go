package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Rollback of migration 0004 (docs/phase-d2-stage2-plan.md 3.8, AC29): the guarded recipe in
// testdata/upgrade/rollback-0004-index.sql, which docs/schema-upgrade.md quotes. Like the ledger cleanup of
// upgrade_rollback_test.go it runs only when the ledger is exactly what it is written for.

const rollback0004File = "rollback-0004-index.sql"

// rollbackRun0004 runs the guarded recipe for 0004 the way psql -v ON_ERROR_STOP=1 would (see rollbackRunCleanup).
func rollbackRun0004(t *testing.T, tmp *dbtest.Temp) error {
	t.Helper()
	return rollbackRunScript(t, tmp.Conn(t), rollback0004File)
}

// AC29 (6): the guarded recipe of docs/phase-d2-stage2-plan.md 3.8 for 0004, on the ledger it is written for
// (baseline and 0004; a database that this release started has 5002 as well, so the drop of 5002 runs first and
// this recipe second): the index and the ledger row go, nothing else changes, the previous binary starts
// without the override, and the new binary applies 0004 and 5002 again.
func TestUpgradeRollback0004RecipeDropsTheIndexAndTheLedgerRow(t *testing.T) {
	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s2a"))
	conn := tmp.Conn(t)
	baseline := readLedger(t, conn)
	requireBaselineLedger(t, baseline)
	d := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d.DB))
	requireLlmUsageTsIndex(t, "after the start", d.DB)
	rowsBefore, digestsBefore := takeRows(t, d.DB), takeDigests(t, d.DB)

	// With 5002 on record the recipe for 0004 refuses: the drop of 5002 comes first.
	var pgErr *pgconn.PgError
	if err := rollbackRun0004(t, tmp); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "schema_migrations is not baseline + 0004") {
		t.Fatalf("the recipe on the ledger baseline + 0004 + 5002 did not raise the guard's exception (%v)", err)
	}
	if err := rollbackRunScript(t, d.DB, rollbackDropToolRagFile); err != nil {
		t.Fatalf("the 5002 drop failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := rollbackRun0004(t, tmp); err != nil {
		t.Fatalf("the recipe failed on the ledger baseline + 0004 (%s)", dbtest.AtomicErrorSummary(err))
	}
	if got := readLedger(t, d.DB); !sameLedgerRows(baseline, got) {
		t.Errorf("after the recipe the ledger is %v, want the baseline rows as they were", ledgerVersions(got))
	}
	if def := llmUsageTsIndexDef(t, d.DB); def != "" {
		t.Errorf("after the recipe %s still exists (%s)", llmUsageTsIndex, def)
	}
	if diff := dbtest.DiffLines(referenceWithoutToolRag(referenceWithoutLlmUsageTsIndex(loadReference(t))), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("after the recipes the catalog is not the reference without the index and 5002's table (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if p := dbtest.RepeatProblems(rollbackRowsWithoutLedger(rowsBefore), rollbackRowsWithoutLedger(takeRows(t, d.DB))); len(p) > 0 {
		t.Errorf("the recipe changed rows (%d):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(context.Background(), d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after the recipe: %d granted and %d waiting locks on the key (err %v), want 0 and 0", granted, waiting, err)
	}

	// A second run on the rolled-back database raises and changes nothing.
	before := captureMigrationState(t, tmp)
	if err := rollbackRun0004(t, tmp); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "schema_migrations is not baseline + 0004") {
		t.Errorf("a second run of the recipe did not raise the guard's exception (%v)", err)
	}
	requireMigrationStateUnchanged(t, "after the second run of the recipe", before, captureMigrationState(t, tmp))

	// The previous binary starts without the override, and leaves the database as the recipe did.
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	startPrevious(t, tmp)
	if got := readLedger(t, d.DB); !sameLedgerRows(baseline, got) {
		t.Errorf("after the start of the previous binary the ledger is %v, want the baseline rows", ledgerVersions(got))
	}
	if def := llmUsageTsIndexDef(t, d.DB); def != "" {
		t.Errorf("the previous binary's start created %s: its set has no 0004", llmUsageTsIndex)
	}

	// The new binary applies 0004 again and converges.
	d2 := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d2.DB))
	requireLlmUsageTsIndex(t, "after the new binary's start", d2.DB)
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d2.DB))); len(diff) > 0 {
		t.Errorf("after the recipe and a start the catalog differs from the reference (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if p := dbtest.RepeatProblems(rollbackRowsWithoutLedger(rowsBefore), rollbackRowsWithoutLedger(takeRows(t, d2.DB))); len(p) > 0 {
		t.Errorf("the starts after the recipe changed rows (%d):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if fell := sequencesThatFell(digestsBefore, takeDigests(t, d2.DB)); len(fell) > 0 {
		t.Errorf("a sequence moved backwards: %s", strings.Join(fell, "; "))
	}
}

// AC29 (6), negative control: on a ledger that is anything but exactly the baseline and 0004 the recipe RAISES
// and changes nothing (the index included), and the session holds no lock afterwards.
func TestUpgradeRollback0004RecipeRefusesAnyOtherLedger(t *testing.T) {
	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s2a"))
	d := startup(t, tmp.DSN)
	full := readLedger(t, d.DB)
	requireEmbeddedLedger(t, full)
	ctx := context.Background()

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
	without := func(version int) []appliedMigration {
		var out []appliedMigration
		for _, r := range full {
			if r.version != version {
				out = append(out, r)
			}
		}
		return out
	}
	renamed := func(version int, name string) []appliedMigration {
		var out []appliedMigration
		for _, r := range full {
			if r.version == version {
				r.name = name
			}
			out = append(out, r)
		}
		return out
	}
	extra := appliedMigration{version: 5003, name: "5003_synthetic.sql", checksum: "synthetic", appliedAt: full[0].appliedAt}
	for _, tc := range []struct {
		name string
		rows []appliedMigration
	}{
		{"baseline only (0004 not recorded)", without(4)},
		{"5002 applied as well (the drop of 5002 comes first)", full},
		{"a later fork migration as well", append(append([]appliedMigration{}, full...), extra)},
		{"0003 missing", without(3)},
		{"5001 missing", without(5001)},
		{"0004 recorded under another name", renamed(4, "0004_other_name.sql")},
		{"ledger empty", nil},
	} {
		setLedger(tc.name, tc.rows)
		before := captureMigrationState(t, tmp)
		err := rollbackRun0004(t, tmp)
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			t.Errorf("%s: the recipe succeeded", tc.name)
		case !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "schema_migrations is not baseline + 0004"):
			t.Errorf("%s: the recipe failed with %s, want the guard's exception (P0001, \"not baseline + 0004\")", tc.name, dbtest.AtomicErrorSummary(err))
		}
		requireLlmUsageTsIndex(t, tc.name+": after the refused recipe", d.DB)
		requireMigrationStateUnchanged(t, tc.name+": after the refused recipe", before, captureMigrationState(t, tmp))
		if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
			t.Errorf("%s: after the refused recipe: %d granted and %d waiting locks on the key (err %v), want 0 and 0", tc.name, granted, waiting, err)
		}
		if t.Failed() {
			return // a dropped index or ledger makes the next case meaningless
		}
	}
}

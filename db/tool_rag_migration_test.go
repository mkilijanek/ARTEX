package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

const migrationToolRag = "5002_tool_rag_observations.sql"

// AC20: a binary that does not ship 5002 (the release before it) refuses a database that applied it, and
// starts with ARTEX_SCHEMA_ALLOW_NEWER=5002, leaving the table and the ledger as they are.
func TestMigrationToolRagOlderBinaryRefusesUnlessAllowed(t *testing.T) {
	tmp := migrationTemp(t, "ragolder")
	startMigrated(t, tmp, embeddedMigrations())
	older := migrationOverlay(migrationSet(t, nil), fstest.MapFS{migrationToolRag: nil})

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	requireRefusal(t, tmp, older, "unknown", "5002", migrationToolRag, "ARTEX_SCHEMA_ALLOW_NEWER")

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "5002")
	startMigrated(t, tmp, older)
	if !migrationTableExists(t, tmp.Conn(t), "tool_rag_observations") {
		t.Error("the older binary start removed tool_rag_observations")
	}
	requireEmbeddedLedger(t, readLedger(t, tmp.Conn(t)))
}

// AC20: the guarded drop of 5002 (docs/phase-d3-plan.md section 10) removes the table and its ledger row, and
// only then; the next start of this binary applies 5002 again. A database that this release started has 5003 on
// record as well, so the recipe refuses it until the drop of 5003 has run (docs/detector-plan.md section 5).
func TestMigrationToolRagDropRecipeIsGuardedAndReversible(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "ragdrop")
	d := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d.DB))
	ctx := context.Background()

	// 5003 is on record and newest: the recipe raises and changes nothing; the drop of 5003 comes first.
	before5003 := captureMigrationState(t, tmp)
	var pgErr *pgconn.PgError
	err := rollbackRunScript(t, d.DB, rollbackDropToolRagFile)
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "5002 is not the newest fork migration") {
		t.Fatalf("the recipe with 5003 on record: err = %v, want the guard's exception (P0001)", err)
	}
	requireMigrationStateUnchanged(t, "after the recipe refused by 5003", before5003, captureMigrationState(t, tmp))
	dropSignalSidecarMigration(t, d)
	if err := rollbackRunScript(t, d.DB, rollbackDropDetectionsFile); err != nil {
		t.Fatalf("the 5003 drop failed (%s)", dbtest.AtomicErrorSummary(err))
	}

	// A newer fork migration is on record: the recipe raises and changes nothing.
	embedded := readLedger(t, d.DB)
	if _, err := d.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (5999, '5999_future.sql', 'synthetic', $1)`, embedded[0].appliedAt); err != nil {
		t.Fatal(err)
	}
	before := captureMigrationState(t, tmp)
	err = rollbackRunScript(t, d.DB, rollbackDropToolRagFile)
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "5002 is not the newest fork migration") {
		t.Fatalf("the recipe with 5999 on record: err = %v, want the guard's exception (P0001)", err)
	}
	if !migrationTableExists(t, d.DB, "tool_rag_observations") {
		t.Fatal("the refused recipe dropped the table")
	}
	requireMigrationStateUnchanged(t, "after the refused recipe", before, captureMigrationState(t, tmp))
	if _, err := d.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 5999`); err != nil {
		t.Fatal(err)
	}

	// 5002 is the newest: the recipe drops the table, the row, and holds no lock afterwards.
	if err := rollbackRunScript(t, d.DB, rollbackDropToolRagFile); err != nil {
		t.Fatalf("the recipe failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if migrationTableExists(t, d.DB, "tool_rag_observations") {
		t.Error("the recipe left tool_rag_observations")
	}
	// The ledger is what the release before 5002 leaves: the baseline and 0004 (the recipe for 0004 comes after this one).
	requireLedgerOf(t, readLedger(t, d.DB), 1, 2, 3, 4, 5001)
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after the recipe: %d granted and %d waiting locks (err %v), want 0 and 0", granted, waiting, err)
	}
	// It is idempotent against a missing table only through the ledger guard: a second run now raises (no 5002 row).
	if err := rollbackRunScript(t, d.DB, rollbackDropToolRagFile); err == nil {
		t.Error("a second run of the recipe succeeded on a ledger without 5002")
	}

	// This binary applies 5002 again and the table works.
	d2 := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d2.DB))
	if !migrationTableExists(t, d2.DB, "tool_rag_observations") {
		t.Error("the start after the recipe did not re-create tool_rag_observations")
	}
}

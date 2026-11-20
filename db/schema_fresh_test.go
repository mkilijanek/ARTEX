package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// startup is what a running server does to the schema: Open (the migration preflight, schema.sql,
// the baseline adoption of the migration runner with its ledger, seedBuiltins). The llm tables come
// from the embedded migrations, so there is no Ensure call after it. It never skips: a database that
// cannot be opened is a failure. An error is reported through
// dbtest.AtomicErrorSummary (SQLSTATE and object names, a fixed text for a connection
// error): a PostgreSQL message can quote the row that violated a constraint, which matters
// when the database is a copy of a real one (the dry run).
func startup(t testing.TB, dsn string) *DB {
	t.Helper()
	d, err := Open(dsn)
	if err != nil {
		t.Fatalf("startup: Open failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func loadReference(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(upgradeDir, referenceFile))
	if err != nil {
		t.Fatalf("reference catalog: %v\n%s", err, schemaChangeProcedure)
	}
	defer f.Close()
	lines, err := dbtest.ReadReference(f)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// expectedTables is 55 CREATE TABLE statements in schema.sql, llm_records and llm_usage (migrations
// 0001 and 0002), the ledger schema_migrations (docs/phase-d2-plan.md 2.1, docs/phase-d2-stage2-plan.md 3.2) and
// tool_rag_observations (migration 5002, docs/phase-d3-plan.md 3.6).
const expectedTables = 59

// AC2: startup on an empty database succeeds and the catalog listing equals the
// committed reference.
func TestUpgradeFreshDatabaseMatchesReference(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "fresh")
	d := startup(t, tmp.DSN)
	entries := tmp.Catalog(t, d.DB)

	if n := dbtest.Count(entries, "table"); n != expectedTables {
		t.Errorf("fresh database has %d tables, want %d", n, expectedTables)
	}
	for _, kind := range []string{"extension", "schema", "relation"} {
		if n := dbtest.Count(entries, kind); n != 0 {
			t.Errorf("fresh database has %d %s entries, want none (extensions other than plpgsql, schemas other than public and other relation kinds are not part of the schema)", n, kind)
		}
	}
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(entries)); len(diff) > 0 {
		t.Fatalf("catalog of a fresh database differs from %s (%d objects):\n  %s\nregenerate only after the schema-change procedure:\n%s",
			referenceFile, len(diff), strings.Join(diff, "\n  "), schemaChangeProcedure)
	}
}

// AC12: a failed comparison names objects and nothing else, with rows that carry the
// row canary in the database and the password canary in the role's password.
func TestUpgradeFailedComparisonDoesNotLeakRowsOrPassword(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "canary")
	d := startup(t, tmp.DSN)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `INSERT INTO companies(name, nkey) VALUES ($1, $1)`, dbtest.RowCanary+"-company"); err != nil {
		t.Fatalf("insert canary row: %v", err)
	}
	before, err := dbtest.TakeDigests(ctx, d.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE companies SET name = $1, logo = $1`, dbtest.RowCanary+"-changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `ALTER TABLE companies ADD COLUMN canary_probe TEXT DEFAULT '`+dbtest.RowCanary+`'`); err != nil {
		t.Fatal(err)
	}
	after, err := dbtest.TakeDigests(ctx, d.DB)
	if err != nil {
		t.Fatal(err)
	}
	entries := tmp.Catalog(t, d.DB)

	digestDiff := strings.Join(dbtest.DiffDigests(before, after), "\n")
	catalogDiff := strings.Join(dbtest.DiffLines(loadReference(t), dbtest.Lines(entries)), "\n")
	if !strings.Contains(digestDiff, "table companies: column logo values changed") || !strings.Contains(catalogDiff, "unexpected column companies.canary_probe") {
		t.Fatalf("the deliberate differences were not reported:\n%s\n%s", digestDiff, catalogDiff)
	}
	dbtest.AssertNoSecret(t, "digest difference", digestDiff)
	dbtest.AssertNoSecret(t, "catalog difference", catalogDiff)
	dbtest.AssertNoSecret(t, "log output", logs())
}

const referenceHeader = `# ARTEX schema reference catalog, PostgreSQL 16 only (docs/phase-d2-plan.md section 5.2).
# State listed: a fresh database after Open alone (schema.sql, the embedded migrations 0001, 0002, 0003, 0004, 5001, 5002
# and the ledger table schema_migrations; no Ensure call).
# One line per object: kind name sha256(definition). Sorted. Column order is not listed on purpose
# (restore uses the same row type on both sides of INSERT ... SELECT *).
# Listed: tables (kind, persistence), columns (type, nullability, default, collation, identity,
# generated), constraints (definition, validity), indexes (definition, validity, readiness),
# non-internal triggers (definition, enabled state), functions, sequences (owner column, parameters).
# Asserted absent: extensions other than plpgsql, schemas other than public, views and other relations.
# Not listed: owners, grants, comments, views, user types, tablespaces.
# Regenerate only through the schema-change procedure (docs/schema-upgrade.md):
#   ARTEX_UPGRADE_REGENERATE=1 go test ./db -run TestRegenerateUpgradeReference
`

// TestRegenerateUpgradeReference rewrites the reference catalog from the current code, on the
// PostgreSQL server of ARTEX_PG_DSN, and prints the sha256 values to put into
// schema_gate_test.go and manifest.json. The historical Go DDL fixture is never regenerated. It does nothing unless
// ARTEX_UPGRADE_REGENERATE=1; its name does not start with TestUpgrade, so the skip
// gate does not count it.
func TestRegenerateUpgradeReference(t *testing.T) {
	if os.Getenv("ARTEX_UPGRADE_REGENERATE") != "1" {
		t.Skip("set ARTEX_UPGRADE_REGENERATE=1 to regenerate the reference catalog")
	}
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "regen")
	d := startup(t, tmp.DSN)
	entries := tmp.Catalog(t, d.DB)

	var b strings.Builder
	b.WriteString(referenceHeader)
	for _, l := range dbtest.Lines(entries) {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(filepath.Join(upgradeDir, referenceFile), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("schema.sql   sha256 %s", sha256Hex([]byte(schemaSQL)))
	t.Logf("reference    sha256 %s  (%s, %d objects)", sha256Hex([]byte(b.String())), referenceFile, len(entries))
}

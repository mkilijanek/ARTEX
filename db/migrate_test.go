package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Tests of the versioned migration runner (docs/phase-d2-stage2-plan.md, slice A1). All of them
// run on temporary databases made by dbtest.TempDatabase and fail, never skip, when the server is
// unreachable. The first eight (the loader, the embedded set, adoption of existing tables, the
// order, the closed failures, the rollback and the out-of-order refusal) are the eight tests of
// the upstream runner, rewritten for the adapted runner: every synthetic set carries a fork stub
// (5000_fork_stub.sql), because a ledger without a fork-range row is refused as foreign.

// migrationForkStub is the fork-range file of every synthetic set. It is outside the embedded set.
const migrationForkStub = "5000_fork_stub.sql"

func migrationTemp(t *testing.T, label string) *dbtest.Temp {
	t.Helper()
	return dbtest.TempDatabase(t, dbtest.RequireServer(t), "mig_"+label)
}

// withBaseline replaces the frozen baseline set for one test.
func withBaseline(t *testing.T, versions ...int) {
	t.Helper()
	prev := baselineVersions
	baselineVersions = versions
	t.Cleanup(func() { baselineVersions = prev })
}

// withMigrationLockTimeout shortens the lock timeout for one test.
func withMigrationLockTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := migrationLockTimeout
	migrationLockTimeout = d
	t.Cleanup(func() { migrationLockTimeout = prev })
}

// migrationOverlay adds extra to base; a nil entry removes the file.
func migrationOverlay(base, extra fstest.MapFS) fstest.MapFS {
	for name, f := range extra {
		if f == nil {
			delete(base, name)
		} else {
			base[name] = f
		}
	}
	return base
}

func migrationFile(text string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(text)} }

// migrationSet is the embedded set plus extra files.
func migrationSet(t *testing.T, extra fstest.MapFS) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(embeddedMigrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		content, err := fs.ReadFile(embeddedMigrations(), e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: content}
	}
	return migrationOverlay(out, extra)
}

// syntheticSet is 0001, 0002 and the fork stub plus extra files, with the baseline set
// {0001, 0002, 5000} for the rest of the test.
func syntheticSet(t *testing.T, extra fstest.MapFS) fstest.MapFS {
	t.Helper()
	withBaseline(t, 1, 2, 5000)
	out := fstest.MapFS{migrationForkStub: migrationFile("SELECT 1;")}
	for _, name := range []string{migrationLLMRecords, migrationLLMUsage} {
		content, err := fs.ReadFile(embeddedMigrations(), name)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = &fstest.MapFile{Data: content}
	}
	return migrationOverlay(out, extra)
}

type appliedMigration struct {
	version        int
	name, checksum string
	appliedAt      time.Time
}

func readLedger(t *testing.T, q *sql.DB) []appliedMigration {
	t.Helper()
	rows, err := q.Query(`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer rows.Close()
	var out []appliedMigration
	for rows.Next() {
		var m appliedMigration
		if err := rows.Scan(&m.version, &m.name, &m.checksum, &m.appliedAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func ledgerVersions(rows []appliedMigration) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.version
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func migrationTableExists(t *testing.T, q *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := q.QueryRow(`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("look up table %s failed (%s)", name, dbtest.AtomicErrorSummary(err))
	}
	return exists
}

// requireLedgerOf checks that rows are exactly the given versions, with the name and the sha256 of the
// embedded file of each.
func requireLedgerOf(t *testing.T, rows []appliedMigration, versions ...int) {
	t.Helper()
	embedded, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	byVersion := map[int]migration{}
	for _, m := range embedded {
		byVersion[m.version] = m
	}
	if !sameInts(ledgerVersions(rows), versions) {
		t.Fatalf("schema_migrations versions = %v, want %v", ledgerVersions(rows), versions)
	}
	for i, v := range versions {
		m, ok := byVersion[v]
		if !ok {
			t.Fatalf("version %04d is not embedded", v)
		}
		if rows[i].name != m.name || rows[i].checksum != m.checksum {
			t.Errorf("schema_migrations row %d = %s %s, want %s %s", i, rows[i].name, rows[i].checksum, m.name, m.checksum)
		}
	}
}

// requireBaselineLedger checks that rows are exactly the four baseline migrations: what a start of the
// release before 0004 (see previousReleaseMigrations) leaves.
func requireBaselineLedger(t *testing.T, rows []appliedMigration) {
	t.Helper()
	requireLedgerOf(t, rows, 1, 2, 3, 5001)
}

// requireEmbeddedLedger checks that rows are exactly the migrations this binary embeds: the baseline set and
// 0004, which is applied on its own after the baseline (in its own transaction, so with its own applied_at).
func requireEmbeddedLedger(t *testing.T, rows []appliedMigration) {
	t.Helper()
	requireLedgerOf(t, rows, 1, 2, 3, 4, 5001)
}

// startMigrated runs one successful start on the database and closes it.
func startMigrated(t *testing.T, tmp *dbtest.Temp, fsys fs.FS) {
	t.Helper()
	d, err := openWithMigrations(tmp.DSN, fsys)
	if err != nil {
		t.Fatalf("start failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

// migrationState is the content-free state of a database: the catalog listing, the table digests
// (every column, timestamps and sequences included) and three markers that prove where a refused
// start stopped (docs/phase-d2-stage2-plan.md AC7):
//   - the agents.updated_at values: seedBuiltins rewrites them on every start;
//   - the xmin of the function set_updated_at(): schema.sql replaces it on every start;
//   - the oid of the constraint task_scope_kind_check: schema.sql drops and re-adds it.
type migrationState struct {
	lines   []string
	digests *dbtest.Digests
	markers map[string]string
}

func captureMigrationState(t *testing.T, tmp *dbtest.Temp) migrationState {
	t.Helper()
	conn := tmp.Conn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	digests, err := dbtest.TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	st := migrationState{lines: dbtest.Lines(tmp.Catalog(t, conn)), digests: digests, markers: map[string]string{}}
	for name, query := range map[string]string{
		"agents.updated_at":         `SELECT coalesce(string_agg(updated_at::text, ',' ORDER BY id), '') FROM agents`,
		"set_updated_at xmin":       `SELECT coalesce((SELECT xmin::text FROM pg_proc WHERE proname = 'set_updated_at' ORDER BY oid LIMIT 1), '')`,
		"task_scope_kind_check oid": `SELECT coalesce((SELECT oid::text FROM pg_constraint WHERE conname = 'task_scope_kind_check'), '')`,
	} {
		var v string
		if err := conn.QueryRowContext(ctx, query).Scan(&v); err != nil {
			t.Fatalf("marker %s failed (%s)", name, dbtest.AtomicErrorSummary(err))
		}
		st.markers[name] = v
	}
	return st
}

func requireMigrationStateUnchanged(t *testing.T, what string, before, after migrationState) {
	t.Helper()
	if diff := dbtest.DiffLines(before.lines, after.lines); len(diff) > 0 {
		t.Errorf("%s: the catalog changed (%d objects):\n  %s", what, len(diff), strings.Join(diff, "\n  "))
	}
	if diff := dbtest.DiffDigests(before.digests, after.digests); len(diff) > 0 {
		t.Errorf("%s: row digests or sequences changed (%d differences):\n  %s", what, len(diff), strings.Join(diff, "\n  "))
	}
	for name, b := range before.markers {
		if a := after.markers[name]; a != b {
			t.Errorf("%s: marker %q changed: the start wrote past the refusal point", what, name)
		}
	}
}

// requireRefusal runs one start that must be refused in the preflight: a *migrationRefusal of the
// given kind whose text holds every want string, a refusal log line, nothing written (state and
// markers unchanged) and no secret in the error or the log.
func requireRefusal(t *testing.T, tmp *dbtest.Temp, fsys fs.FS, kind string, want ...string) error {
	t.Helper()
	logs := dbtest.CaptureLog(t)
	before := captureMigrationState(t, tmp)
	for _, name := range []string{"agents.updated_at", "set_updated_at xmin", "task_scope_kind_check oid"} {
		if before.markers[name] == "" {
			t.Fatalf("precondition: marker %q is empty, the database was never started", name)
		}
	}
	d, err := openWithMigrations(tmp.DSN, fsys)
	if err == nil {
		d.Close()
		t.Fatalf("start succeeded, want a %q refusal", kind)
	}
	var ref *migrationRefusal
	if !errors.As(err, &ref) || ref.kind != kind {
		t.Fatalf("want a %q refusal, got: %s", kind, dbtest.AtomicErrorSummary(err))
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q does not mention %q", err, w)
		}
	}
	if !strings.Contains(logs(), "[migrate] refused: ") {
		t.Errorf("no refusal log line; log:\n%s", logs())
	}
	dbtest.AssertNoSecret(t, "refusal error", err.Error())
	dbtest.AssertNoSecret(t, "log", logs())
	requireMigrationStateUnchanged(t, "after the refusal", before, captureMigrationState(t, tmp))
	return err
}

// migrationShapeSchema returns the recorded schema text of a shape.
func migrationShapeSchema(t *testing.T, shape string) string {
	t.Helper()
	for _, s := range recordedShapes {
		if s.shape == shape {
			b, err := os.ReadFile(filepath.Join(upgradeDir, s.file))
			if err != nil {
				t.Fatalf("fixture %s: %v", s.file, err)
			}
			return string(b)
		}
	}
	t.Fatalf("no recorded shape %q", shape)
	return ""
}

// ---- the eight tests of the upstream runner, adapted ----

func TestLoadMigrationsValidation(t *testing.T) {
	ms, err := loadMigrations(fstest.MapFS{
		"0010_b.sql": {Data: []byte("SELECT 10;")},
		"0002_a.sql": {Data: []byte("SELECT 2;")},
		"README.md":  {Data: []byte("not a migration")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].version != 2 || ms[1].version != 10 {
		t.Fatalf("want versions [2 10], got %+v", ms)
	}
	sum := sha256.Sum256([]byte("SELECT 2;"))
	if ms[0].checksum != hex.EncodeToString(sum[:]) || ms[0].name != "0002_a.sql" {
		t.Fatalf("migration 0002 = %+v", ms[0])
	}

	for name, fsys := range map[string]fstest.MapFS{
		"bad name":          {"2_a.sql": {Data: []byte("SELECT 1;")}},
		"duplicate version": {"0001_a.sql": {Data: []byte("SELECT 1;")}, "0001_b.sql": {Data: []byte("SELECT 1;")}},
		"version zero":      {"0000_a.sql": {Data: []byte("SELECT 1;")}},
	} {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// The package-level Open on a new database: the ledger holds exactly the four baseline migrations
// and a second Open changes nothing.
func TestOpenRecordsEmbeddedMigrations(t *testing.T) {
	tmp := migrationTemp(t, "embedded")
	d, err := Open(tmp.DSN)
	if err != nil {
		t.Fatalf("Open failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	before := readLedger(t, d.DB)
	requireEmbeddedLedger(t, before)
	d.Close()
	assertReopenIsNoop(t, tmp, before)
}

// assertReopenIsNoop opens the database again and checks that no migration row was added or
// re-applied and that both Ensure wrappers stay idempotent and leave the ledger alone.
func assertReopenIsNoop(t *testing.T, tmp *dbtest.Temp, before []appliedMigration) {
	t.Helper()
	d, err := Open(tmp.DSN)
	if err != nil {
		t.Fatalf("second Open failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer d.Close()
	after := readLedger(t, d.DB)
	requireEmbeddedLedger(t, after)
	for i := range before {
		if !after[i].appliedAt.Equal(before[i].appliedAt) {
			t.Errorf("migration %04d applied_at changed: %v -> %v", before[i].version, before[i].appliedAt, after[i].appliedAt)
		}
	}
	for range 2 {
		if err := d.EnsureLLMRecordsTable(); err != nil {
			t.Fatalf("EnsureLLMRecordsTable after Open failed (%s)", dbtest.AtomicErrorSummary(err))
		}
		if err := d.EnsureLLMUsageTable(); err != nil {
			t.Fatalf("EnsureLLMUsageTable after Open failed (%s)", dbtest.AtomicErrorSummary(err))
		}
	}
	if got := readLedger(t, d.DB); len(got) != len(before) {
		t.Errorf("the Ensure wrappers changed schema_migrations: %v", ledgerVersions(got))
	}
}

func TestOpenMigratesFreshDatabase(t *testing.T) {
	tmp := migrationTemp(t, "fresh")
	logs := dbtest.CaptureLog(t)
	d, err := openWithMigrations(tmp.DSN, embeddedMigrations())
	if err != nil {
		t.Fatalf("Open failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	for _, table := range []string{"llm_records", "llm_usage", "schema_migrations"} {
		if !migrationTableExists(t, d.DB, table) {
			t.Errorf("table %s missing after Open", table)
		}
	}
	applied := readLedger(t, d.DB)
	requireEmbeddedLedger(t, applied)
	d.Close()
	first := logs()
	for _, want := range []string{
		"[schema] schema.sql sha256=",
		"[migrate] ledger absent: adopting baseline (4)",
		"[migrate] adopted baseline in ",
		"[migrate] up to date (upstream=0004, fork=5001)",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("first start: log has no %q:\n%s", want, first)
		}
	}
	assertReopenIsNoop(t, tmp, applied)
	if second := strings.TrimPrefix(logs(), first); strings.Contains(second, "adopting baseline") || !strings.Contains(second, "[migrate] up to date (upstream=0004, fork=5001)") {
		t.Errorf("second start: want an up-to-date line and no adoption; log:\n%s", second)
	}
}

// An install that predates schema_migrations already has both tables, maybe without later columns.
// Open adopts them: it runs the baseline files over them and records the four.
func TestOpenAdoptsExistingTablesWithoutSchemaMigrations(t *testing.T) {
	tmp := migrationTemp(t, "legacy")
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	for _, q := range []string{
		`ALTER TABLE llm_records DROP COLUMN raw_request, DROP COLUMN raw_response`,
		`ALTER TABLE llm_usage DROP COLUMN provider`,
		`INSERT INTO llm_records(model) VALUES ('legacy')`,
		`INSERT INTO llm_usage(model, status) VALUES ('legacy', 'ok')`,
		// The state of a database last started by a release without the ledger.
		`DROP TABLE schema_migrations`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("setup failed (%s)", dbtest.AtomicErrorSummary(err))
		}
	}

	d, err := Open(tmp.DSN)
	if err != nil {
		t.Fatalf("Open on existing tables failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	applied := readLedger(t, d.DB)
	requireEmbeddedLedger(t, applied)
	var records, usage, cols int
	if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE model = 'legacy'`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM llm_usage WHERE model = 'legacy'`).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'llm_records' AND column_name IN ('raw_request', 'raw_response'))
		   OR (table_name = 'llm_usage' AND column_name = 'provider')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if records != 1 || usage != 1 || cols != 3 {
		t.Errorf("after adoption: legacy llm_records rows=%d llm_usage rows=%d restored columns=%d, want 1 1 3", records, usage, cols)
	}
	d.Close()
	assertReopenIsNoop(t, tmp, applied)
}

// probeMigration records how many migrations were applied when it ran and fails if it ever runs
// twice (CREATE TABLE without IF NOT EXISTS).
var probeMigration = migrationFile(`
CREATE TABLE migration_probe (applied_before INTEGER);
INSERT INTO migration_probe SELECT count(*) FROM schema_migrations;
`)

func TestMigrationsApplyNewVersionOnceInOrder(t *testing.T) {
	tmp := migrationTemp(t, "order")
	fsys := syntheticSet(t, fstest.MapFS{"0003_probe.sql": probeMigration})
	for pass := 1; pass <= 2; pass++ {
		d, err := openWithMigrations(tmp.DSN, fsys)
		if err != nil {
			t.Fatalf("pass %d failed (%s)", pass, dbtest.AtomicErrorSummary(err))
		}
		applied := readLedger(t, d.DB)
		if !sameInts(ledgerVersions(applied), []int{1, 2, 3, 5000}) || applied[2].name != "0003_probe.sql" {
			t.Fatalf("pass %d: schema_migrations = %+v", pass, applied)
		}
		var runs, before int
		if err := d.QueryRow(`SELECT count(*), max(applied_before) FROM migration_probe`).Scan(&runs, &before); err != nil {
			t.Fatal(err)
		}
		// The baseline rows (1, 2, 5000) were committed before the probe ran.
		if runs != 1 || before != 3 {
			t.Fatalf("pass %d: probe ran %d times with %d migrations before it, want 1 and 3", pass, runs, before)
		}
		d.Close()
	}
}

func TestMigrationsFailClosed(t *testing.T) {
	tmp := migrationTemp(t, "closed")
	startMigrated(t, tmp, syntheticSet(t, fstest.MapFS{"0003_probe.sql": probeMigration}))

	cases := []struct {
		name string
		fsys fs.FS
		kind string
		want []string
	}{
		{"checksum changed", syntheticSet(t, fstest.MapFS{"0003_probe.sql": migrationFile("-- edited\n" + string(probeMigration.Data))}),
			"changed", []string{"0003", "0003_probe.sql", "checksum"}},
		{"version missing from binary", syntheticSet(t, fstest.MapFS{}),
			"unknown", []string{"0003", "0003_probe.sql", "missing from this binary"}},
		{"name changed", syntheticSet(t, fstest.MapFS{"0003_renamed.sql": probeMigration}),
			"changed", []string{"0003", "0003_probe.sql", "0003_renamed.sql"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			requireRefusal(t, tmp, c.fsys, c.kind, c.want...)
		})
	}
}

func TestFailingMigrationRollsBackAndBlocksOpen(t *testing.T) {
	tmp := migrationTemp(t, "fail")
	fsys := syntheticSet(t, fstest.MapFS{
		"0003_probe.sql": probeMigration,
		"0004_broken.sql": migrationFile(`
CREATE TABLE migration_broken (x INTEGER);
SELECT 1/0;
`),
	})
	d, err := openWithMigrations(tmp.DSN, fsys)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded with a failing migration")
	}
	if !strings.Contains(err.Error(), "0004") || !strings.Contains(err.Error(), "0004_broken.sql") {
		t.Errorf("error %q does not name the failing migration", err)
	}

	// Inspect with the migrations that succeeded: earlier ones stay applied, the failing one left
	// nothing behind.
	d, err = openWithMigrations(tmp.DSN, syntheticSet(t, fstest.MapFS{"0003_probe.sql": probeMigration}))
	if err != nil {
		t.Fatalf("start without the broken migration failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer d.Close()
	applied := readLedger(t, d.DB)
	if !sameInts(ledgerVersions(applied), []int{1, 2, 3, 5000}) {
		t.Fatalf("schema_migrations = %+v, want versions 1, 2, 3, 5000", applied)
	}
	if migrationTableExists(t, d.DB, "migration_broken") {
		t.Error("migration_broken exists: the failed migration was not rolled back")
	}
}

// A database that has 0001, 0003 and the stub must refuse a binary that adds 0002 below 0003, before
// writing anything.
func TestMigrationsRefuseOutOfOrderVersion(t *testing.T) {
	tmp := migrationTemp(t, "outoforder")
	setup := syntheticSet(t, fstest.MapFS{migrationLLMUsage: nil, "0003_probe.sql": probeMigration})
	withBaseline(t, 1, 5000)
	startMigrated(t, tmp, setup)
	before := readLedger(t, tmp.Conn(t))
	if !sameInts(ledgerVersions(before), []int{1, 3, 5000}) {
		t.Fatalf("setup schema_migrations = %+v, want versions 1, 3, 5000", before)
	}

	err := requireRefusal(t, tmp, syntheticSet(t, fstest.MapFS{"0003_probe.sql": probeMigration}), "order",
		migrationLLMUsage, "(0002)", "0003_probe.sql", "(0003)", "never inserted")
	_ = err
	conn := tmp.Conn(t)
	after := readLedger(t, conn)
	if len(after) != len(before) {
		t.Fatalf("schema_migrations changed: before %+v, after %+v", before, after)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("schema_migrations row %d changed: before %+v, after %+v", i, before[i], after[i])
		}
	}
	if migrationTableExists(t, conn, "llm_usage") {
		t.Error("llm_usage exists: the refused 0002 migration ran")
	}
}

// ---- AC2: the order of the refusals ----

// setLedger replaces schema_migrations by a table holding exactly rows.
func setLedger(t *testing.T, tmp *dbtest.Temp, rows ...ledgerRow) {
	t.Helper()
	stmt := `DROP TABLE IF EXISTS schema_migrations;
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, checksum TEXT, applied_at TIMESTAMPTZ DEFAULT now());`
	for _, r := range rows {
		stmt += "\nINSERT INTO schema_migrations(version, name, checksum) VALUES (" +
			strconv.Itoa(r.version) + ", '" + r.name + "', '" + r.checksum + "');"
	}
	tmp.Load(t, "ledger rows", stmt)
}

func TestLedgerRefusalOrder(t *testing.T) {
	tmp := migrationTemp(t, "refusalorder")
	set := migrationSet(t, fstest.MapFS{"5002_next.sql": migrationFile("SELECT 1;")})
	ms, err := loadMigrations(set)
	if err != nil {
		t.Fatal(err)
	}
	byVersion := map[int]migration{}
	for _, m := range ms {
		byVersion[m.version] = m
	}
	good := func(v int) ledgerRow { return ledgerRow{v, byVersion[v].name, byVersion[v].checksum} }
	badSum := func(v int) ledgerRow { return ledgerRow{v, byVersion[v].name, "0000bad0"} }
	unknown := func(v int) ledgerRow { return ledgerRow{v, fmt.Sprintf("%04d_future.sql", v), "1234abcd"} }
	conn := tmp.Conn(t)

	cases := []struct {
		name       string
		rows       []ledgerRow
		absent     bool
		allowNewer int
		kind       string // "" = accepted
		version    int    // version the refusal names (0 = none)
	}{
		{name: "no table", absent: true},
		{name: "empty ledger", rows: nil, kind: "empty"},
		{name: "foreign beats changed, unknown and order", rows: []ledgerRow{badSum(1), good(3), unknown(4), unknown(5)}, allowNewer: 9999, kind: "foreign"},
		{name: "foreign beats unknown without the override", rows: []ledgerRow{good(1), good(3), unknown(4)}, kind: "foreign"},
		{name: "foreign with the override set", rows: []ledgerRow{good(1), good(2), good(3), unknown(4), unknown(5)}, allowNewer: 9999, kind: "foreign"},
		{name: "changed beats unknown and order", rows: []ledgerRow{good(1), badSum(2), good(3), good(5001), unknown(5999)}, kind: "changed", version: 2},
		{name: "unknown beats order", rows: []ledgerRow{good(1), good(2), good(3), good(5001), unknown(5999)}, kind: "unknown", version: 5999},
		{name: "unknown above the allowed version", rows: []ledgerRow{good(1), good(2), good(3), good(5001), unknown(5999)}, allowNewer: 5998, kind: "unknown", version: 5999},
		{name: "order after an allowed unknown", rows: []ledgerRow{good(1), good(3), good(5001), unknown(5999)}, allowNewer: 5999, kind: "order", version: 2},
		{name: "order alone, upstream range", rows: []ledgerRow{good(1), good(3), good(5001)}, kind: "order", version: 2},
		{name: "order alone, fork range", rows: []ledgerRow{good(1), good(2), good(3), {5002, "5002_next.sql", byVersion[5002].checksum}, {5003, "5003_later.sql", "abcd1234"}}, allowNewer: 5003, kind: "order", version: 5001},
		{name: "fresh baseline with a pending fork migration", rows: []ledgerRow{good(1), good(2), good(3), good(5001)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.absent {
				tmp.Load(t, "no ledger", `DROP TABLE IF EXISTS schema_migrations`)
			} else {
				setLedger(t, tmp, c.rows...)
			}
			plan, err := checkLedger(context.Background(), conn, ms, c.allowNewer)
			if c.kind == "" {
				if err != nil {
					t.Fatalf("want the ledger accepted, got: %v", err)
				}
				if plan.absent != c.absent {
					t.Errorf("plan.absent = %v, want %v", plan.absent, c.absent)
				}
				return
			}
			var ref *migrationRefusal
			if !errors.As(err, &ref) {
				t.Fatalf("want a %q refusal, got: %v", c.kind, err)
			}
			if ref.kind != c.kind || ref.version != c.version {
				t.Errorf("refusal = %s version %d (%q), want %s version %d", ref.kind, ref.version, ref.reason, c.kind, c.version)
			}
		})
	}
}

// ---- AC5, AC6, AC12: adoption and later migrations ----

// AC5: adoption is one transaction. With the last baseline file failing, startup on a database of
// the r0512 shape fails, the first three baseline files leave nothing behind, and the state is
// that of a plain schema.sql start.
func TestMigrationAdoptionIsAtomic(t *testing.T) {
	srv := dbtest.RequireServer(t)
	shape := migrationShapeSchema(t, "r0512")
	tmp := dbtest.TempDatabase(t, srv, "mig_atomic")
	tmp.Load(t, "r0512 schema", shape)
	control := dbtest.TempDatabase(t, srv, "mig_atomicctl")
	control.Load(t, "r0512 schema", shape)
	control.Load(t, "schema.sql", schemaSQL)

	failing := migrationSet(t, fstest.MapFS{migrationLLMUsageFork: migrationFile("SELECT 1/0;\n")})
	d, err := openWithMigrations(tmp.DSN, failing)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although the last baseline file fails")
	}
	if got := dbtest.AtomicErrorSummary(err); !strings.Contains(got, "SQLSTATE 22012") || !strings.Contains(err.Error(), migrationLLMUsageFork) {
		t.Fatalf("Open failed, but not in 5001 by the appended statement: %s / %v", got, err)
	}

	conn := tmp.Conn(t)
	for _, table := range []string{"schema_migrations", "llm_records", "llm_usage"} {
		if migrationTableExists(t, conn, table) {
			t.Errorf("table %s exists after the failed adoption", table)
		}
	}
	ctx := context.Background()
	got, err := dbtest.AtomicDataDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	want, err := dbtest.AtomicDataDigests(ctx, control.Conn(t))
	if err != nil {
		t.Fatal(err)
	}
	if diff := dbtest.DiffDigests(want, got); len(diff) > 0 {
		t.Errorf("row digests differ from a plain schema.sql start:\n  %s", strings.Join(diff, "\n  "))
	}
	if diff := dbtest.DiffLines(dbtest.Lines(control.Catalog(t, control.Conn(t))), dbtest.Lines(tmp.Catalog(t, conn))); len(diff) > 0 {
		t.Errorf("catalog differs from a plain schema.sql start:\n  %s", strings.Join(diff, "\n  "))
	}

	// A restart with the embedded set adopts, and applies 0004 after it.
	startMigrated(t, tmp, embeddedMigrations())
	requireEmbeddedLedger(t, readLedger(t, tmp.Conn(t)))
}

// AC6: the adoption set is frozen. A ledger-less database whose binary also ships a failing 5002
// ends with the four baseline rows and the ledger row of 0004, which is an ordinary pending migration applied
// on its own before 5002: the extra file is not part of the adoption, and neither is 0004.
func TestMigrationAdoptionSetIsFrozen(t *testing.T) {
	tmp := migrationTemp(t, "frozen")
	fsys := migrationSet(t, fstest.MapFS{"5002_extra_fails.sql": migrationFile("CREATE TABLE migration_extra (x integer);\nSELECT 1/0;\n")})
	d, err := openWithMigrations(tmp.DSN, fsys)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although 5002 fails")
	}
	if !strings.Contains(err.Error(), "5002") || !strings.Contains(err.Error(), "5002_extra_fails.sql") {
		t.Errorf("error %q does not name 5002", err)
	}
	conn := tmp.Conn(t)
	requireEmbeddedLedger(t, readLedger(t, conn))
	if migrationTableExists(t, conn, "migration_extra") {
		t.Error("migration_extra exists: the failed 5002 was not rolled back")
	}
}

// AC12: a failure after adoption keeps the migrations before it, names the failing one, repeats
// until the file is corrected, and then applies.
func TestMigrationFailureAfterAdoption(t *testing.T) {
	tmp := migrationTemp(t, "afteradopt")
	ok := migrationFile("CREATE TABLE migration_ok (x integer);\n")
	broken := migrationFile("CREATE TABLE migration_bad (x integer);\nSELECT 1/0;\n")
	failing := migrationSet(t, fstest.MapFS{"5002_ok.sql": ok, "5003_bad.sql": broken})

	var first string
	for attempt := 1; attempt <= 2; attempt++ {
		d, err := openWithMigrations(tmp.DSN, failing)
		if err == nil {
			d.Close()
			t.Fatalf("attempt %d: Open succeeded although 5003 fails", attempt)
		}
		if !strings.Contains(err.Error(), "5003") || !strings.Contains(err.Error(), "5003_bad.sql") || strings.Contains(err.Error(), "5002") {
			t.Errorf("attempt %d: error %q must name 5003 only", attempt, err)
		}
		if attempt == 1 {
			first = err.Error()
		} else if err.Error() != first {
			t.Errorf("attempt 2: error %q differs from the first %q", err, first)
		}
		conn := tmp.Conn(t)
		if got := ledgerVersions(readLedger(t, conn)); !sameInts(got, []int{1, 2, 3, 4, 5001, 5002}) {
			t.Fatalf("attempt %d: ledger versions = %v, want 1 2 3 4 5001 5002", attempt, got)
		}
		if !migrationTableExists(t, conn, "migration_ok") || migrationTableExists(t, conn, "migration_bad") {
			t.Fatalf("attempt %d: want migration_ok present and migration_bad absent", attempt)
		}
	}

	corrected := migrationSet(t, fstest.MapFS{"5002_ok.sql": ok, "5003_bad.sql": migrationFile("CREATE TABLE migration_bad (x integer);\n")})
	startMigrated(t, tmp, corrected)
	conn := tmp.Conn(t)
	if got := ledgerVersions(readLedger(t, conn)); !sameInts(got, []int{1, 2, 3, 4, 5001, 5002, 5003}) {
		t.Errorf("after the correction: ledger versions = %v", got)
	}
	if !migrationTableExists(t, conn, "migration_bad") {
		t.Error("the corrected 5003 did not run")
	}
}

// ---- AC7, AC8, AC9, AC10, AC11, AC37: refusals leave the database unchanged ----

func TestMigrationChecksumMismatchIsRefusedBeforeAnyWrite(t *testing.T) {
	tmp := migrationTemp(t, "checksum")
	startMigrated(t, tmp, embeddedMigrations())
	edited := migrationFile("-- edited\n" + string(mustReadEmbedded(t, migrationLLMUsage)))
	requireRefusal(t, tmp, migrationSet(t, fstest.MapFS{migrationLLMUsage: edited}), "changed", "0002", migrationLLMUsage, "checksum")
}

func TestMigrationNameMismatchIsRefusedBeforeAnyWrite(t *testing.T) {
	tmp := migrationTemp(t, "name")
	startMigrated(t, tmp, embeddedMigrations())
	renamed := migrationSet(t, fstest.MapFS{
		migrationLLMUsage:        nil,
		"0002_llm_usage_new.sql": &fstest.MapFile{Data: mustReadEmbedded(t, migrationLLMUsage)},
	})
	requireRefusal(t, tmp, renamed, "changed", "0002", migrationLLMUsage, "0002_llm_usage_new.sql")
}

func mustReadEmbedded(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(embeddedMigrations(), name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMigrationUnknownVersionAndOverride(t *testing.T) {
	tmp := migrationTemp(t, "unknown")
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	if _, err := conn.Exec(`INSERT INTO schema_migrations(version, name, checksum) VALUES (5999, '5999_future.sql', '0123abcd')`); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	requireRefusal(t, tmp, embeddedMigrations(), "unknown", "5999", "5999_future.sql", "ARTEX_SCHEMA_ALLOW_NEWER")
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "5998")
	requireRefusal(t, tmp, embeddedMigrations(), "unknown", "5999", "5999_future.sql")

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "5999")
	logs := dbtest.CaptureLog(t)
	before := readLedger(t, conn)
	startMigrated(t, tmp, embeddedMigrations())
	if n := strings.Count(logs(), "WARNING"); n != 1 || !strings.Contains(logs(), "[migrate] WARNING: ARTEX_SCHEMA_ALLOW_NEWER=5999; unknown migration 5999 (\"5999_future.sql\")") {
		t.Errorf("want exactly one warning for 5999; log:\n%s", logs())
	}
	after := readLedger(t, conn)
	if len(after) != len(before) || after[len(after)-1] != before[len(before)-1] {
		t.Errorf("the unknown row was touched: before %+v after %+v", before, after)
	}
}

func TestMigrationForeignLedgerIsRefusedEvenWithOverride(t *testing.T) {
	tmp := migrationTemp(t, "foreign")
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	// The ledger an upstream binary leaves: 0001 to 0005 and no fork row (0004 is in the ledger already).
	for _, q := range []string{
		`DELETE FROM schema_migrations WHERE version = 5001`,
		`INSERT INTO schema_migrations(version, name, checksum) VALUES (5, '0005_traffic_rewrite_rules.sql', 'f00d0005')`,
	} {
		if _, err := conn.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "9999")
	requireRefusal(t, tmp, embeddedMigrations(), "foreign", "another binary")
}

func TestMigrationOrderPerRange(t *testing.T) {
	t.Run("fork: applied 5002, pending 5001", func(t *testing.T) {
		tmp := migrationTemp(t, "ordera")
		set := migrationSet(t, fstest.MapFS{"5002_next.sql": migrationFile("SELECT 1;")})
		startMigrated(t, tmp, set)
		if _, err := tmp.Conn(t).Exec(`DELETE FROM schema_migrations WHERE version = 5001`); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, tmp, set, "order", migrationLLMUsageFork, "(5001)", "5002_next.sql", "(5002)", "never inserted")
	})
	t.Run("upstream: applied baseline, pending 0004 runs after 5001", func(t *testing.T) {
		tmp := migrationTemp(t, "orderb")
		startPrevious(t, tmp)
		requireBaselineLedger(t, readLedger(t, tmp.Conn(t)))
		startMigrated(t, tmp, embeddedMigrations())
		rows := readLedger(t, tmp.Conn(t))
		if !sameInts(ledgerVersions(rows), []int{1, 2, 3, 4, 5001}) {
			t.Fatalf("ledger versions = %v", ledgerVersions(rows))
		}
		if !rows[3].appliedAt.After(rows[4].appliedAt) {
			t.Errorf("0004 applied_at %v is not after 5001 applied_at %v", rows[3].appliedAt, rows[4].appliedAt)
		}
	})
	t.Run("upstream: applied 0001, 0003, 5001, pending 0002", func(t *testing.T) {
		tmp := migrationTemp(t, "orderc")
		startMigrated(t, tmp, embeddedMigrations())
		// 0004 is not applied either, so that 0003 is the newest applied version of the upstream range.
		if _, err := tmp.Conn(t).Exec(`DELETE FROM schema_migrations WHERE version IN (2, 4)`); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, tmp, embeddedMigrations(), "order", migrationLLMUsage, "(0002)", "(0003)", "never inserted")
	})
}

// AC37: a ledger table without rows means somebody emptied it; adopting it would re-run every later
// migration. It is refused and no override applies.
func TestMigrationEmptyLedgerIsRefused(t *testing.T) {
	tmp := migrationTemp(t, "empty")
	startMigrated(t, tmp, embeddedMigrations())
	if _, err := tmp.Conn(t).Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "9999")
	requireRefusal(t, tmp, embeddedMigrations(), "empty", "ledger was edited", "restore the pre-upgrade dump")
}

// ---- AC14: the lock timeout ----

func TestMigrationLockTimeoutNamesTheTable(t *testing.T) {
	tmp := migrationTemp(t, "locktimeout")
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	if _, err := conn.Exec(`DROP TABLE schema_migrations`); err != nil { // the state of a database from a release without the ledger
		t.Fatal(err)
	}
	holder, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	release := func() {
		if held {
			held = false
			if err := holder.Rollback(); err != nil {
				t.Errorf("release the lock: %v", err)
			}
		}
	}
	t.Cleanup(release)
	if _, err := holder.Exec(`LOCK TABLE llm_usage IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	withMigrationLockTimeout(t, 300*time.Millisecond)
	logs := dbtest.CaptureLog(t)
	start := time.Now()
	d, err := openWithMigrations(tmp.DSN, embeddedMigrations())
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although another session holds a lock on llm_usage")
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Errorf("Open took %v: the lock timeout did not bound the wait", took)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("want SQLSTATE 55P03 in the chain, got: %s", dbtest.AtomicErrorSummary(err))
	}
	for _, w := range []string{"5001", migrationLLMUsageFork, "llm_usage", "pg_dump", "stop it and restart"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not mention %q", err, w)
		}
	}
	if !strings.Contains(logs(), "[migrate] lock timeout on llm_usage after 0.3s: ") {
		t.Errorf("no lock timeout log line; log:\n%s", logs())
	}
	if migrationTableExists(t, conn, "schema_migrations") {
		t.Error("schema_migrations exists after the failed adoption")
	}

	release()
	startMigrated(t, tmp, embeddedMigrations())
	requireEmbeddedLedger(t, readLedger(t, conn))
}

// ---- AC17 (db part): a failing llm_usage migration fails Open ----

func TestFailingLLMUsageMigrationFailsOpen(t *testing.T) {
	tmp := migrationTemp(t, "usagefail")
	failing := migrationSet(t, fstest.MapFS{migrationLLMUsage: migrationFile("SELECT 1/0;\n")})
	d, err := openWithMigrations(tmp.DSN, failing)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although the llm_usage migration fails")
	}
	if !strings.HasPrefix(err.Error(), "apply migrations: ") || !strings.Contains(err.Error(), migrationLLMUsage) {
		t.Errorf("error %q must start with 'apply migrations: ' and name %s", err, migrationLLMUsage)
	}
	if migrationTableExists(t, tmp.Conn(t), "schema_migrations") {
		t.Error("schema_migrations exists although the adoption failed")
	}
}

// ---- AC19: a migration that schema.sql reverts is caught by the repeated-start comparison ----

// repeatedStartDiff starts the database twice with fsys and returns the catalog and row differences
// between the two states (timestamps and sequences left out: every start rewrites them).
func repeatedStartDiff(t *testing.T, tmp *dbtest.Temp, fsys fs.FS) []string {
	t.Helper()
	ctx := context.Background()
	capture := func() ([]string, *dbtest.Digests) {
		conn := tmp.Conn(t)
		digests, err := dbtest.AtomicDataDigests(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		return dbtest.Lines(tmp.Catalog(t, conn)), digests
	}
	startMigrated(t, tmp, fsys)
	l1, d1 := capture()
	startMigrated(t, tmp, fsys)
	l2, d2 := capture()
	return append(dbtest.DiffLines(l1, l2), dbtest.DiffDigests(d1, d2)...)
}

func TestMigrationRepeatedStartChangesNothing(t *testing.T) {
	tmp := migrationTemp(t, "repeat")
	if diff := repeatedStartDiff(t, tmp, embeddedMigrations()); len(diff) > 0 {
		t.Errorf("a second start changed the database:\n  %s", strings.Join(diff, "\n  "))
	}
}

// The control of AC19: a migration that changes task_scope_kind_check, which db/schema.sql re-adds
// on every start, is reverted by the next start, and the repeated-start comparison sees it.
func TestMigrationRevertedBySchemaSQLIsDetected(t *testing.T) {
	tmp := migrationTemp(t, "revert")
	reverting := migrationSet(t, fstest.MapFS{"5002_scope_check.sql": migrationFile(`
ALTER TABLE task_scope DROP CONSTRAINT IF EXISTS task_scope_kind_check;
ALTER TABLE task_scope ADD CONSTRAINT task_scope_kind_check CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword','asn'));
`)})
	diff := repeatedStartDiff(t, tmp, reverting)
	if len(diff) == 0 {
		t.Fatal("the repeated-start comparison did not see schema.sql revert the migration")
	}
	found := false
	for _, line := range diff {
		found = found || strings.Contains(line, "task_scope_kind_check")
	}
	if !found {
		t.Errorf("the difference does not name task_scope_kind_check:\n  %s", strings.Join(diff, "\n  "))
	}
}

// ---- AC27: errors and logs are content-free ----

func TestMigrationFailureErrorAndLogsAreContentFree(t *testing.T) {
	tmp := migrationTemp(t, "canary")
	logs := dbtest.CaptureLog(t)
	failing := migrationSet(t, fstest.MapFS{"5002_canary.sql": migrationFile("SELECT CAST('" + dbtest.RowCanary + "' AS integer);\n")})
	d, err := openWithMigrations(tmp.DSN, failing)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although 5002 casts a canary to an integer")
	}
	// The driver's own message quotes the value: the sanitising is what keeps it out.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" || !strings.Contains(pgErr.Message, dbtest.RowCanary) {
		t.Fatalf("precondition: want SQLSTATE 22P02 quoting the canary in the driver error, got: %s", dbtest.AtomicErrorSummary(err))
	}
	if !strings.Contains(err.Error(), "SQLSTATE 22P02") || !strings.Contains(err.Error(), "5002_canary.sql") {
		t.Errorf("error %q must show the SQLSTATE and the file", err)
	}
	dbtest.AssertNoSecret(t, "error", err.Error())
	dbtest.AssertNoSecret(t, "log", logs())
	if strings.Contains(err.Error(), "postgres://") || strings.Contains(logs(), "postgres://") {
		t.Error("a DSN appears in the error or the log")
	}
}

// ---- the Ensure wrappers ----

// AC18: on a database with only schema.sql applied, the wrappers produce the full fork shape of
// llm_records and llm_usage (every column, including the 14 fork columns of llm_usage, and every
// index), equal to what a full Open produces for those two tables. The comparison is over the
// catalog lines (kind, name, hash of the definition) of every object that belongs to the two
// tables; the wrappers write no ledger row.
func TestEnsureWrappersRunEmbeddedMigrations(t *testing.T) {
	llmLines := func(t *testing.T, tmp *dbtest.Temp, q dbtest.Queryer) []string {
		t.Helper()
		var picked []dbtest.Entry
		for _, e := range tmp.Catalog(t, q) {
			if strings.Contains(e.Name, "llm_records") || strings.Contains(e.Name, "llm_usage") {
				picked = append(picked, e)
			}
		}
		return dbtest.Lines(picked)
	}

	full := migrationTemp(t, "wrappers_full")
	fd, err := Open(full.DSN)
	if err != nil {
		t.Fatalf("Open failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	t.Cleanup(func() { fd.Close() })
	want := llmLines(t, full, fd.DB)

	tmp := migrationTemp(t, "wrappers")
	tmp.Load(t, "schema.sql", schemaSQL)
	d := &DB{tmp.Conn(t)}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("EnsureLLMRecordsTable failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatalf("EnsureLLMUsageTable failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	got := llmLines(t, tmp, d.DB)

	// Guard against an empty comparison: the shape must contain the 14 fork columns and the five
	// llm_usage indexes by name.
	forkColumns := []string{"provider", "intent_id", "attempt", "reason", "request_bytes", "system_bytes", "message_bytes",
		"tool_schema_bytes", "tool_result_bytes", "size_known", "call_id", "round_no", "purpose", "tool_bytes"}
	joined := "\n" + strings.Join(want, "\n")
	for _, c := range forkColumns {
		if !strings.Contains(joined, "column llm_usage."+c+" ") {
			t.Errorf("the full Open shape has no column llm_usage.%s", c)
		}
	}
	for _, i := range []string{"idx_llm_usage_call", "idx_llm_usage_purpose", "idx_llm_usage_task", "idx_llm_usage_model", "idx_llm_usage_exp",
		"idx_llm_records_ts", "idx_llm_records_session"} {
		if !strings.Contains(joined, i+" ") {
			t.Errorf("the full Open shape has no index %s", i)
		}
	}
	// The wrappers re-run the baseline files (0001, 0002, 5001) only: 0004's index is a migration of its own, which
	// only Open applies, so the shape the wrappers produce is the full Open's minus idx_llm_usage_ts.
	if !strings.Contains(joined, "idx_llm_usage_ts ") {
		t.Error("the full Open shape has no index idx_llm_usage_ts")
	}
	if diff := dbtest.DiffLines(referenceWithoutLlmUsageTsIndex(want), got); len(diff) > 0 {
		t.Errorf("the wrappers do not produce the shape of a full Open:\n%s", strings.Join(diff, "\n"))
	}
	if migrationTableExists(t, d.DB, "schema_migrations") {
		t.Error("the wrappers must not write a ledger")
	}
}

// Ledger names come from the database: a row cannot inject a line break into an error or a log line.
func TestMigrationLedgerNamesArePrintedQuoted(t *testing.T) {
	tmp := migrationTemp(t, "quoted")
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	forged := "5999_x.sql\n[migrate] FORGED line"
	quoted := `"5999_x.sql\n[migrate] FORGED line"`
	if _, err := conn.Exec(`INSERT INTO schema_migrations(version, name, checksum) VALUES (5999, $1, 'abc')`, forged); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	err := requireRefusal(t, tmp, embeddedMigrations(), "unknown", "5999", quoted)
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal text contains a line break: %q", err)
	}
	var ref *migrationRefusal
	if !errors.As(err, &ref) || strings.Contains(ref.logLine(), "\n") || !strings.Contains(ref.logLine(), quoted) {
		t.Errorf("the refusal log line is not quoted: %q", ref.logLine())
	}

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "5999")
	logs := dbtest.CaptureLog(t)
	startMigrated(t, tmp, embeddedMigrations())
	if strings.Contains(logs(), "\n[migrate] FORGED") || !strings.Contains(logs(), quoted) {
		t.Errorf("the warning is not quoted:\n%s", logs())
	}
}

func TestMigrationLedgerRenamedRowIsPrintedQuoted(t *testing.T) {
	tmp := migrationTemp(t, "quotedname")
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	if _, err := conn.Exec(`UPDATE schema_migrations SET name = E'0001_x.sql\n[migrate] FORGED', checksum = E'ab\ncd' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	err := requireRefusal(t, tmp, embeddedMigrations(), "changed", "0001", `"0001_x.sql\n[migrate] FORGED"`)
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the refusal text contains a line break: %q", err)
	}
	if _, err := conn.Exec(`UPDATE schema_migrations SET name = '0001_llm_records.sql' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	err = requireRefusal(t, tmp, embeddedMigrations(), "changed", "0001", `"ab\ncd"`)
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the checksum refusal text contains a line break: %q", err)
	}
}

// Defence in depth behind the load-time lint: a file that ended the runner's transaction (built here
// without the loader) fails before its ledger row is written, instead of recording it in autocommit.
func TestMigrationExecFailsWhenTheFileEndedTheTransaction(t *testing.T) {
	tmp := migrationTemp(t, "txended")
	conn := tmp.Conn(t)
	if _, err := conn.Exec(migrationLedgerTable); err != nil {
		t.Fatal(err)
	}
	m := migration{version: 5998, name: "5998_ends_tx.sql", checksum: "x", sql: "CREATE TABLE tx_ended_probe (x int); COMMIT;"}
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = execMigration(context.Background(), tx, m)
	tx.Rollback() //nolint:errcheck // the transaction is already over on the server
	if err == nil || !strings.Contains(err.Error(), "5998_ends_tx.sql") || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("want an error naming the file and the transaction, got: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ledger rows = %d (err %v), want 0", n, err)
	}
}

// A well-behaved migration still records its row through execMigration.
func TestMigrationExecRecordsTheRowInsideTheTransaction(t *testing.T) {
	tmp := migrationTemp(t, "txok")
	conn := tmp.Conn(t)
	if _, err := conn.Exec(migrationLedgerTable); err != nil {
		t.Fatal(err)
	}
	m := migration{version: 5997, name: "5997_ok.sql", checksum: "x", sql: "CREATE TABLE tx_ok_probe (x int);"}
	if err := runMigrationTx(context.Background(), conn, func(tx *sql.Tx) error { return execMigration(context.Background(), tx, m) }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 5997`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ledger rows = %d (err %v), want 1", n, err)
	}
}

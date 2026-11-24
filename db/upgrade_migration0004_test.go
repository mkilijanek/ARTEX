package db

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mkilijanek/artex/db/dbtest"
)

// The first real migration (docs/phase-d2-stage2-plan.md, pull request S2-C, AC29): upstream 0004, one
// CREATE INDEX IF NOT EXISTS idx_llm_usage_ts on llm_usage(ts), byte-identical to upstream, applied by the
// runner on its own after the baseline, and on an existing database after the fork migration 5001 (the order
// rule is per range). Like every test of the suite, none of these skips.

const (
	migration0004       = "0004_llm_usage_ts_index.sql"
	migration0004SHA256 = "1f27b2411ebb064b7b7cd4d0d8fdfc2021ad74082019e6c72e772f706a00d028"
	migration0004Blob   = "f9c66d0019f2b3a9b991628b830b33cb49c3082b"
	migration0004Size   = 904
	llmUsageTsIndex     = "idx_llm_usage_ts"
)

// previousReleaseMigrations is the migration set of the release before 0004 (0.5.15m): the embedded set
// without 0004 and without the fork migrations 5002, 5003 and 5004, which came after it.
func previousReleaseMigrations(t *testing.T) fstest.MapFS {
	t.Helper()
	return migrationSet(t, fstest.MapFS{migration0004: nil, migrationToolRag: nil, migrationDetections: nil, sidecarMigration: nil})
}

// startPrevious runs one start of the release before 0004 on the database and closes it.
func startPrevious(t *testing.T, tmp *dbtest.Temp) {
	t.Helper()
	startMigrated(t, tmp, previousReleaseMigrations(t))
}

// startupPrevious is startup for the release before 0004: the database it leaves has the baseline ledger.
func startupPrevious(t *testing.T, dsn string) *DB {
	t.Helper()
	d, err := openWithMigrations(dsn, previousReleaseMigrations(t))
	if err != nil {
		t.Fatalf("start of the release before 0004 failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func matrixShapeNamed(t *testing.T, name string) matrixShape {
	t.Helper()
	for _, sh := range matrixShapes {
		if sh.name == name {
			return sh
		}
	}
	t.Fatalf("no matrix shape %q", name)
	return matrixShape{}
}

// llmUsageTsIndexDef is the definition of idx_llm_usage_ts, "" when the index does not exist.
func llmUsageTsIndexDef(t *testing.T, q dbtest.Queryer) string {
	t.Helper()
	rows, err := q.QueryContext(context.Background(), `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, llmUsageTsIndex)
	if err != nil {
		t.Fatalf("read pg_indexes failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer rows.Close()
	var def string
	if rows.Next() {
		if err := rows.Scan(&def); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_indexes failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	return def
}

func requireLlmUsageTsIndex(t *testing.T, what string, q dbtest.Queryer) {
	t.Helper()
	if def := llmUsageTsIndexDef(t, q); !strings.Contains(def, "ON public.llm_usage") || !strings.Contains(def, "(ts)") {
		t.Errorf("%s: %s is %q, want an index on llm_usage(ts)", what, llmUsageTsIndex, def)
	}
}

// referenceWithoutLlmUsageTsIndex is the reference catalog minus the line of 0004's index: the catalog of a
// database that the release before 0004 left behind.
func referenceWithoutLlmUsageTsIndex(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "index "+llmUsageTsIndex+" ") {
			out = append(out, l)
		}
	}
	return out
}

// AC29 (1): the file is upstream's, byte for byte (sha256, git blob id, size, no CR), it is pinned as a
// downgrade-safe upstream-range file, and it is not part of the frozen baseline set: a database without a
// ledger gets it as an ordinary pending migration, on its own.
func TestMigration0004IsByteIdenticalToUpstreamAndNotBaseline(t *testing.T) {
	b := mustReadEmbedded(t, migration0004)
	if got := sha256Hex(b); got != migration0004SHA256 {
		t.Errorf("%s has sha256 %s, want %s (upstream's file, byte-identical)", migration0004, got, migration0004SHA256)
	}
	if got := migrationGitBlobID(b); got != migration0004Blob {
		t.Errorf("%s has git blob id %s, want %s (upstream %s)", migration0004, got, migration0004Blob, migrationUpstreamCommit[:12])
	}
	if len(b) != migration0004Size {
		t.Errorf("%s has %d bytes, want %d", migration0004, len(b), migration0004Size)
	}
	if strings.Contains(string(b), "\r") {
		t.Errorf("%s contains a CR byte", migration0004)
	}

	var pin *migrationPin
	for i := range migrationPins {
		if migrationPins[i].name == migration0004 {
			pin = &migrationPins[i]
		}
	}
	switch {
	case pin == nil:
		t.Errorf("%s is not in migrationPins", migration0004)
	case pin.sha256 != migration0004SHA256 || pin.blob != migration0004Blob || pin.downgradeSafe != "yes":
		t.Errorf("pin of %s = %+v, want sha256 %s, blob %s, downgrade-safe yes", migration0004, *pin, migration0004SHA256, migration0004Blob)
	}

	for _, v := range baselineVersions {
		if v == 4 {
			t.Errorf("baselineVersions %v holds 4: the baseline set is frozen at 1, 2, 3, 5001", baselineVersions)
		}
	}
	all, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := baselineMigrations(all)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range baseline {
		if m.name == migration0004 {
			t.Errorf("the baseline migrations hold %s", migration0004)
		}
	}
	if len(baseline) != 4 || len(all) != 8 {
		t.Errorf("%d baseline and %d embedded migrations, want 4 and 8 (the baseline, 0004, 5002, 5003 and 5004)", len(baseline), len(all))
	}
}

// AC29 (2): the shape s2a is what it says: schema.sql of the frozen bytes, a ledger with exactly the four
// baseline rows, and the files of those rows are the baseline set (so the shape is built from files that are
// never edited).
func TestUpgradeShapeS2aRecordsTheBaselineLedger(t *testing.T) {
	rec := recordedShapeNamed(t, "s2a")
	if rec.goddl != "" || len(rec.releases) != 1 || rec.releases[0].tag != "v0.5.15m" {
		t.Fatalf("shape s2a = %+v, want release v0.5.15m and no Go DDL", rec)
	}
	var versions []int
	for _, m := range rec.migrations {
		versions = append(versions, m.version)
		var pin *migrationPin
		for i := range migrationPins {
			if migrationPins[i].name == m.name {
				pin = &migrationPins[i]
			}
		}
		if pin == nil || pin.sha256 != m.sha256 {
			t.Errorf("s2a migration %s: recorded sha256 %s, pinned %+v", m.name, m.sha256, pin)
		}
	}
	if !sameInts(versions, baselineVersions) {
		t.Errorf("shape s2a records the versions %v, want the baseline set %v", versions, baselineVersions)
	}

	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s2a"))
	conn := tmp.Conn(t)
	requireBaselineLedger(t, readLedger(t, conn))
	for _, table := range []string{"llm_records", "llm_usage", "schema_migrations"} {
		if !migrationTableExists(t, conn, table) {
			t.Errorf("shape s2a has no table %s", table)
		}
	}
	if def := llmUsageTsIndexDef(t, conn); def != "" {
		t.Errorf("shape s2a already has %s (%s): it is the database before 0004", llmUsageTsIndex, def)
	}
}

// AC29 (4), (8): on s2a the start applies 0004 after 5001 (an upstream-range version below an applied fork
// version is accepted: the order rule is per range), leaves the four baseline rows as they were, ends with the
// ledger 0001 0002 0003 0004 5001 5002 5003 5004 and the index, and logs how long the migration took (the line, not
// its value).
func TestUpgradeS2aAppliesTheIndexAfterTheForkMigration(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s2a"))
	before := readLedger(t, tmp.Conn(t))
	requireBaselineLedger(t, before)

	d := startup(t, tmp.DSN)
	after := readLedger(t, d.DB)
	requireEmbeddedLedger(t, after)
	if len(after) != 8 || !sameLedgerRows(before[:3], after[:3]) || !sameLedgerRows(before[3:], after[4:5]) {
		t.Fatalf("the start changed a baseline row of the ledger: before %v, after %v", ledgerVersions(before), ledgerVersions(after))
	}
	if after[3].version != 4 || after[4].version != 5001 || !after[3].appliedAt.After(after[4].appliedAt) {
		t.Errorf("0004 (applied %v) was not applied after 5001 (applied %v)", after[3].appliedAt, after[4].appliedAt)
	}
	requireLlmUsageTsIndex(t, "after the start", d.DB)

	log := logs()
	if !regexp.MustCompile(`\[migrate\] applied ` + regexp.QuoteMeta(migration0004) + ` in [0-9]+ms`).MatchString(log) {
		t.Errorf("the log has no \"[migrate] applied %s in <n>ms\" line:\n%s", migration0004, log)
	}
	if strings.Contains(log, "ledger absent") || !strings.Contains(log, "[migrate] up to date (upstream=0004, fork=5004)") {
		t.Errorf("the log of the start on s2a is not \"up to date (upstream=0004, fork=5004)\" without an adoption:\n%s", log)
	}
	dbtest.AssertNoSecret(t, "log output", log)

	// A second start applies nothing.
	startup(t, tmp.DSN)
	if second := strings.TrimPrefix(logs(), log); strings.Contains(second, "[migrate] applied") {
		t.Errorf("the second start applied a migration:\n%s", second)
	}
	if !sameLedgerRows(after, readLedger(t, d.DB)) {
		t.Error("the second start changed the ledger")
	}
}

// AC29 (5): a binary without 0004 (the embedded set minus the file, as the release before it) refuses a
// database that has it, with the documented message; ARTEX_SCHEMA_ALLOW_NEWER=0004 starts it with one
// warning per start and leaves the row and the index; =0003 still refuses.
func TestUpgradeBinaryWithout0004RefusesADatabaseThatHasIt(t *testing.T) {
	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s2a"))
	startMigrated(t, tmp, embeddedMigrations())
	conn := tmp.Conn(t)
	applied := readLedger(t, conn)
	requireEmbeddedLedger(t, applied)
	// The embedded set without 0004 only: the fork migration 5002 stays shipped, so 0004 is the one unknown row.
	previous := migrationSet(t, fstest.MapFS{migration0004: nil})

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	err := requireRefusal(t, tmp, previous, "unknown", "0004", migration0004,
		"is applied in the database but missing from this binary", "ARTEX_SCHEMA_ALLOW_NEWER=0004", "docs/schema-upgrade.md")
	if !strings.HasPrefix(err.Error(), "migration preflight: ") {
		t.Errorf("refusal %q does not start with \"migration preflight: \"", err)
	}
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "0003")
	requireRefusal(t, tmp, previous, "unknown", "0004", migration0004)

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "0004")
	logs := dbtest.CaptureLog(t)
	warning := `[migrate] WARNING: ARTEX_SCHEMA_ALLOW_NEWER=0004; unknown migration 0004 ("` + migration0004 + `")`
	for start := 1; start <= 2; start++ {
		startMigrated(t, tmp, previous)
		if n := strings.Count(logs(), warning); n != start {
			t.Errorf("after start %d the log holds the warning %d times, want %d (once per start); log:\n%s", start, n, start, logs())
		}
	}
	if !sameLedgerRows(applied, readLedger(t, conn)) {
		t.Error("the starts under the override changed the ledger: the unknown row must stay")
	}
	requireLlmUsageTsIndex(t, "after the starts under the override", conn)
	dbtest.AssertNoSecret(t, "log output", logs())
}

// AC29 (7): a database that has 0004 and is started by the simulated 0.5.13m binary (a binary that ignores the
// ledger: see TestUpgradeRollbackToTheReleaseWithoutTheLedgerAndForward for what the simulation is) keeps its
// rows, its ledger and the index, which that release neither knows nor drops; the new binary then starts
// again and applies nothing.
func TestUpgradeSimulated0513StartWorksOnADatabaseWith0004(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s2a"))
	d := startup(t, tmp.DSN)
	applied := readLedger(t, d.DB)
	requireEmbeddedLedger(t, applied)
	rowsApplied, digestsApplied := takeRows(t, d.DB), takeDigests(t, d.DB)

	tmp.Load(t, "schema.sql of 0.5.13m", readUpgradeFile(t, recordedSchemaFile(t, "r0513")))
	if err := (&DB{tmp.Conn(t)}).seedBuiltins(); err != nil {
		t.Fatalf("seedBuiltins of the simulated 0.5.13m start failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	tmp.Load(t, "Go DDL of 0.5.13m", readUpgradeFile(t, recordedGoDDLFile(t, "r0513")))

	if p := dbtest.RepeatProblems(rowsApplied, takeRows(t, d.DB)); len(p) > 0 {
		t.Errorf("the simulated 0.5.13m start changed rows (%d; only agents.updated_at of built-in agents may change):\n  %s", len(p), strings.Join(p, "\n  "))
	}
	if fell := sequencesThatFell(digestsApplied, takeDigests(t, d.DB)); len(fell) > 0 {
		t.Errorf("the simulated 0.5.13m start moved a sequence backwards: %s", strings.Join(fell, "; "))
	}
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("the simulated 0.5.13m start changed the catalog (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if !sameLedgerRows(readLedger(t, d.DB), applied) {
		t.Error("the simulated 0.5.13m start changed the ledger")
	}
	requireLlmUsageTsIndex(t, "after the simulated 0.5.13m start", d.DB)

	before := logs()
	startup(t, tmp.DSN)
	if second := strings.TrimPrefix(logs(), before); strings.Contains(second, "[migrate] applied") {
		t.Errorf("the new binary's start after the simulated 0.5.13m start applied a migration:\n%s", second)
	}
	if !sameLedgerRows(readLedger(t, d.DB), applied) {
		t.Error("the start after the simulated 0.5.13m start changed the ledger")
	}
}

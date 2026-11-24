package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Migration 5003 (docs/detector-plan.md section 5, criteria 1a-1 to 1a-5 of 12.1): the four detection tables, the
// function and the trigger that stops a run when its conversation is deleted. These tests need no Go code of the
// database layer: the schema is exercised with plain SQL. The names start with TestMigration so that
// scripts/upgrade-gate.sh counts them and forbids a skip.

const migrationDetections = "5003_finding_detections.sql"

// detectionPreviousSHA256 and detectionNewLines pin the reference catalog: without the lines of the objects of 5003 it
// is, line for line, the reference of the release 0.5.17m (the sha256 of its sorted lines joined by "\n"), and
// the migration adds exactly 146 lines.
const (
	detectionPreviousSHA256 = "a66a34b4489cb337cbc01dc6638968b20f5ba1f12fcd4dd0c7669d1eeba6a8ea"
	detectionNewLines       = 146
)

// releaseBefore5003Migrations is the migration set of the release 0.5.17m: the embedded set without 5003 and 5004.
func releaseBefore5003Migrations(t *testing.T) fstest.MapFS {
	t.Helper()
	return migrationSet(t, fstest.MapFS{migrationDetections: nil, sidecarMigration: nil})
}

// requireGuardRefusal checks that err is the exception a rollback recipe raises: P0001 with the message.
func requireGuardRefusal(t *testing.T, what string, err error, message string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, message) {
		t.Errorf("%s: err = %s, want the guard's exception (P0001, %q)", what, dbtest.AtomicErrorSummary(err), message)
	}
}

// detectionObjects reports which objects of 5003 exist: the four tables, the function and the trigger.
func detectionObjects(t *testing.T, q *sql.DB) (tables []string, function, trigger bool) {
	t.Helper()
	for _, name := range detectionTables {
		if migrationTableExists(t, q, name) {
			tables = append(tables, name)
		}
	}
	if err := q.QueryRow(`SELECT to_regprocedure('stop_deleted_conversation_detection()') IS NOT NULL`).Scan(&function); err != nil {
		t.Fatalf("look up the function failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'conversations'::regclass
		AND tgname = 'trg_conversation_detection_delete' AND NOT tgisinternal)`).Scan(&trigger); err != nil {
		t.Fatalf("look up the trigger failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	return tables, function, trigger
}

func requireDetectionObjects(t *testing.T, what string, q *sql.DB) {
	t.Helper()
	tables, function, trigger := detectionObjects(t, q)
	if len(tables) != len(detectionTables) || !function || !trigger {
		t.Errorf("%s: tables %v, function %v, trigger %v; want all four tables, the function and the trigger", what, tables, function, trigger)
	}
}

func requireNoDetectionObjects(t *testing.T, what string, q *sql.DB) {
	t.Helper()
	if tables, function, trigger := detectionObjects(t, q); len(tables) != 0 || function || trigger {
		t.Errorf("%s: tables %v, function %v, trigger %v; want none of the objects of 5003", what, tables, function, trigger)
	}
}

// ---- 1a-2: the loader ----

// 1a-2: loadMigrations accepts the real file (its PL/pgSQL BEGIN ... END lies inside the dollar-quoted body, which
// the statement-level view of the lint does not see), and rejects the same bytes with a COMMIT appended and with
// the word CONCURRENTLY inside the function body, with the documented messages.
func TestMigrationDetectionLoaderAcceptsTheFileAndRejectsTheNegativeControls(t *testing.T) {
	ms, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatalf("the embedded set is rejected: %v", err)
	}
	found := false
	for _, m := range ms {
		if m.version == 5003 && m.name == migrationDetections {
			found = true
		}
	}
	if !found {
		t.Fatalf("the embedded set has no migration 5003 (%s)", migrationDetections)
	}

	orig := mustReadEmbedded(t, migrationDetections)
	if !bytes.Contains(orig, []byte("\nBEGIN\n")) || !bytes.Contains(orig, []byte("\nEND;\n$$;")) {
		t.Fatal("the file has no PL/pgSQL BEGIN ... END body: the test does not cover the lint of a dollar-quoted body")
	}
	withCommit := append(append([]byte(nil), orig...), []byte("COMMIT;\n")...)
	_, err = loadMigrations(migrationOverlay(migrationSet(t, nil), fstest.MapFS{migrationDetections: {Data: withCommit}}))
	if err == nil || !strings.Contains(err.Error(), "migration file "+migrationDetections+": COMMIT is not allowed (a migration runs inside the runner's transaction and must not control the transaction)") {
		t.Errorf("the file with a COMMIT appended: err = %v, want the documented transaction-control message", err)
	}

	inBody := bytes.Replace(orig, []byte("    RETURN OLD;"), []byte("    PERFORM 'CONCURRENTLY';\n    RETURN OLD;"), 1)
	if bytes.Equal(inBody, orig) {
		t.Fatal("the negative control did not change the file")
	}
	_, err = loadMigrations(migrationOverlay(migrationSet(t, nil), fstest.MapFS{migrationDetections: {Data: inBody}}))
	if err == nil || !strings.Contains(err.Error(), "migration file "+migrationDetections+": CONCURRENTLY is not allowed (a migration runs inside a transaction)") {
		t.Errorf("the file with CONCURRENTLY in the function body: err = %v, want the documented message", err)
	}
}

// ---- 1a-1: the reference ----

// 1a-1: the reference catalog differs from the one of the release before only by the objects of 5003: without the
// lines of the four tables (columns, constraints, indexes, sequences), the function and the trigger it is, line
// for line, the previous one; and the migration adds exactly the pinned number of lines. The previous file is not
// kept: its content is identified by the sha256 of its lines.
func TestMigrationDetectionReferenceDiffersFromThePreviousOnlyByTheNewObjects(t *testing.T) {
	all := loadReference(t)
	old := referenceWithoutDetections(all) // also without the objects of 5004
	if n := len(referenceWithoutSignalSidecar(all)) - len(old); n != detectionNewLines {
		t.Errorf("the reference holds %d lines of the objects of 5003, want %d", n, detectionNewLines)
	}
	sum := sha256.Sum256([]byte(strings.Join(old, "\n")))
	if got := hex.EncodeToString(sum[:]); got != detectionPreviousSHA256 {
		t.Errorf("the reference without the objects of 5003 has sha256 %s, the reference of 0.5.17m had %s: a line that is not of 5003 changed", got, detectionPreviousSHA256)
	}
	if n := len(referenceTables(t)); n != expectedTables || expectedTables != 65 {
		t.Errorf("the reference lists %d tables and expectedTables is %d, want 65", n, expectedTables)
	}
}

// ---- 1a-1, 1a-5: every shape and a fresh database ----

// 1a-1: the migration applies on every recorded shape (the three releases without the runner, 0.5.15m, the new
// 0.5.17m, and the synthetic legacy one) and on a fresh database; the ledger ends with 5003 and is the embedded
// set; the second start applies nothing and changes no ledger row; the file itself is idempotent (run again on a
// started database it changes nothing).
func TestMigrationDetectionAppliesOnEveryShapeAndAFreshDatabase(t *testing.T) {
	srv := dbtest.RequireServer(t)
	check := func(t *testing.T, tmp *dbtest.Temp) {
		logs := dbtest.CaptureLog(t)
		d := startup(t, tmp.DSN)
		requireDetectionObjects(t, "after the first start", d.DB)
		first := readLedger(t, d.DB)
		requireEmbeddedLedger(t, first)
		if row := first[len(first)-2]; row.version != 5003 || row.name != migrationDetections {
			t.Errorf("the ledger has %d %s before its last row, want 5003 %s", row.version, row.name, migrationDetections)
		}
		if last := first[len(first)-1]; last.version != 5004 || last.name != sidecarMigration {
			t.Errorf("the ledger ends with %d %s, want 5004 %s", last.version, last.name, sidecarMigration)
		}
		if !strings.Contains(logs(), "[migrate] applied "+migrationDetections+" in ") {
			t.Errorf("the first start did not log the application of 5003:\n%s", logs())
		}
		for _, name := range detectionTables {
			var n int
			if err := d.QueryRow(`SELECT count(*) FROM ` + name).Scan(&n); err != nil || n != 0 {
				t.Errorf("table %s has %d rows after the start (err %v), want 0", name, n, err)
			}
		}

		seen := len(logs())
		startup(t, tmp.DSN)
		if second := logs()[seen:]; strings.Contains(second, "[migrate] applied") {
			t.Errorf("the second start applied a migration:\n%s", second)
		}
		if !sameLedgerRows(first, readLedger(t, d.DB)) {
			t.Error("the second start changed the ledger")
		}

		before := dbtest.Lines(tmp.Catalog(t, d.DB))
		if _, err := d.Exec(string(mustReadEmbedded(t, migrationDetections))); err != nil {
			t.Fatalf("the file run a second time on a started database failed (%s)", dbtest.AtomicErrorSummary(err))
		}
		if diff := dbtest.DiffLines(before, dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
			t.Errorf("the file run a second time changed the catalog (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
		}
	}
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			tmp := buildShape(t, srv, sh)
			requireNoDetectionObjects(t, "the shape "+sh.name+" before the start", tmp.Conn(t))
			check(t, tmp)
		})
	}
	t.Run("fresh", func(t *testing.T) {
		check(t, dbtest.TempDatabase(t, srv, "detfresh"))
	})
}

// 1a-5: the shape of release 0.5.17m is what it claims: its manifest entry (tag, commit, ledger list), its flags
// file, the pins of its ledger files and the fixture checks exist (TestGitHistoryFixtures compares the schema
// fixture with `git show` where there is git history); the database built from it has the ledger 0001 0002 0003
// 0004 5001 5002, the index of 0004 and the table of 5002 and none of the objects of 5003; a start adds exactly
// one ledger row, 5003, and leaves the six rows as they were.
func TestMigrationDetectionShapeS3aIsWhatItClaims(t *testing.T) {
	rec := recordedShapeNamed(t, "s3a")
	if rec.goddl != "" || len(rec.releases) != 1 || rec.releases[0] != (releaseCommit{"v0.5.17m", "024abf634efb0f48f13493b40fb4185c9fa1fc49"}) {
		t.Fatalf("shape s3a = %+v, want the release v0.5.17m at 024abf634efb and no Go DDL", rec)
	}
	if rec.sha256 != currentSchemaSQLSHA256 {
		t.Errorf("shape s3a pins schema sha256 %s, the frozen db/schema.sql is %s", rec.sha256, currentSchemaSQLSHA256)
	}
	wantVersions := []int{1, 2, 3, 4, 5001, 5002}
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
			t.Errorf("s3a migration %s: recorded sha256 %s, pinned %+v", m.name, m.sha256, pin)
		}
	}
	if !sameInts(versions, wantVersions) {
		t.Errorf("shape s3a records the versions %v, want %v", versions, wantVersions)
	}
	if _, err := os.Stat(filepath.Join(upgradeDir, "flags-v0.5.17m.txt")); err != nil {
		t.Errorf("no flag list for v0.5.17m: %v", err)
	}
	m := readManifest(t)
	listed := false
	for _, s := range m.Shapes {
		if s.Shape == "s3a" {
			listed = len(s.Releases) == 1 && s.Releases[0].Tag == "v0.5.17m" && s.Releases[0].Commit == "024abf634efb0f48f13493b40fb4185c9fa1fc49" && len(s.Migrations) == len(wantVersions)
		}
	}
	if !listed {
		t.Error("manifest.json has no entry of shape s3a with the tag, the commit and the six ledger rows of v0.5.17m")
	}

	tmp := buildShape(t, dbtest.RequireServer(t), matrixShapeNamed(t, "s3a"))
	conn := tmp.Conn(t)
	before := readLedger(t, conn)
	requireLedgerOf(t, before, wantVersions...)
	requireNoDetectionObjects(t, "shape s3a", conn)
	if def := llmUsageTsIndexDef(t, conn); def == "" {
		t.Errorf("shape s3a has no %s: it is the database after 0004", llmUsageTsIndex)
	}
	if !migrationTableExists(t, conn, "tool_rag_observations") {
		t.Error("shape s3a has no tool_rag_observations: it is the database after 5002")
	}

	d := startup(t, tmp.DSN)
	after := readLedger(t, d.DB)
	requireEmbeddedLedger(t, after)
	if len(after) != len(before)+2 || after[len(after)-2].version != 5003 || after[len(after)-1].version != 5004 || !sameLedgerRows(before, after[:len(before)]) {
		t.Errorf("the start on s3a: ledger %v -> %v, want exactly two new rows (5003, 5004) and the six as they were", ledgerVersions(before), ledgerVersions(after))
	}
}

// ---- 1a-3: locks ----

// detectionRunRunner is what a start does to a database where schema.sql already ran, without Open: the pending
// migrations are applied by the runner on one connection (Open's schema.sql step has no lock timeout).
func detectionRunRunner(t *testing.T, pool *sql.DB) error {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("take a connection failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer conn.Close()
	ms, err := loadMigrations(embeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := baselineMigrations(ms)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := checkLedger(ctx, conn, ms, 0)
	if err != nil {
		t.Fatalf("the preflight failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	return applyMigrations(ctx, conn, ms, baseline, plan)
}

// 1a-3: the runner called directly (not through Open), with the timeout shortened, while another session holds an
// open transaction that wrote a row to conversations (or to findings): the migration fails with a lock timeout
// that names the table and leaves no object and no ledger row; after the session ends it applies. A session that
// holds only SELECT ... FOR UPDATE rows does not block it.
func TestMigrationDetectionLockTimeoutNamesTheTableAndLeavesNothing(t *testing.T) {
	cases := []struct {
		name    string
		hold    string // statement of the other session, in an open transaction
		blocked string // table the error must name; "" when the migration must not be blocked
	}{
		{"an insert into conversations", `INSERT INTO conversations(agent_key) VALUES ('lock probe')`, "conversations"},
		{"an insert into findings", `INSERT INTO findings(name) VALUES ('lock probe')`, "findings"},
		{"select for update on both tables", `SELECT (SELECT count(*) FROM (SELECT id FROM conversations FOR UPDATE) c) + (SELECT count(*) FROM (SELECT id FROM findings FOR UPDATE) f)`, ""},
	}
	for i, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tmp := migrationTemp(t, fmt.Sprintf("detlock%d", i))
			startMigrated(t, tmp, releaseBefore5003Migrations(t))
			conn, runner := tmp.Conn(t), tmp.Conn(t)
			if _, err := conn.Exec(`INSERT INTO conversations(agent_key) VALUES ('seed'); INSERT INTO findings(name) VALUES ('seed')`); err != nil {
				t.Fatalf("seed failed (%s)", dbtest.AtomicErrorSummary(err))
			}
			requireLedgerOf(t, readLedger(t, conn), 1, 2, 3, 4, 5001, 5002)

			ctx := context.Background()
			holder, err := tmp.Conn(t).BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin failed (%s)", dbtest.AtomicErrorSummary(err))
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
			if _, err := holder.ExecContext(ctx, tc.hold); err != nil {
				t.Fatalf("the holding statement failed (%s)", dbtest.AtomicErrorSummary(err))
			}

			withMigrationLockTimeout(t, 300*time.Millisecond)
			logs := dbtest.CaptureLog(t)
			if tc.blocked == "" {
				if err := detectionRunRunner(t, runner); err != nil {
					t.Fatalf("a session holding only FOR UPDATE rows blocked the migration (%s)", dbtest.AtomicErrorSummary(err))
				}
				requireEmbeddedLedger(t, readLedger(t, conn))
				requireDetectionObjects(t, "after the migration beside a FOR UPDATE session", conn)
				return
			}

			start := time.Now()
			err = detectionRunRunner(t, runner)
			if err == nil {
				t.Fatalf("the migration succeeded although another session holds a write on %s", tc.blocked)
			}
			if took := time.Since(start); took > 30*time.Second {
				t.Errorf("the migration took %v: the lock timeout did not bound the wait", took)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("want SQLSTATE 55P03 in the chain, got: %s", dbtest.AtomicErrorSummary(err))
			}
			for _, w := range []string{"5003", migrationDetections, tc.blocked, "pg_dump", "stop it and restart"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			if !strings.Contains(logs(), "[migrate] lock timeout on "+tc.blocked+" after 0.3s: ") {
				t.Errorf("no lock timeout log line naming %s; log:\n%s", tc.blocked, logs())
			}
			requireNoDetectionObjects(t, "after the lock timeout", conn)
			requireLedgerOf(t, readLedger(t, conn), 1, 2, 3, 4, 5001, 5002)

			release()
			if err := detectionRunRunner(t, runner); err != nil {
				t.Fatalf("the migration failed after the other session ended (%s)", dbtest.AtomicErrorSummary(err))
			}
			requireEmbeddedLedger(t, readLedger(t, conn))
			requireDetectionObjects(t, "after the migration", conn)
		})
	}
}

// ---- 1a-4: the recipe ----

// seedDetectionRows writes one finding with a run, a rule, a step and a query result, so that the drop recipe has
// rows to destroy (the recipe must leave the finding).
func seedDetectionRows(t *testing.T, q *sql.DB) {
	t.Helper()
	var finding, run int64
	if err := q.QueryRow(`INSERT INTO findings(name) VALUES ('recipe probe') RETURNING id`).Scan(&finding); err != nil {
		t.Fatalf("insert a finding failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := q.QueryRow(`INSERT INTO finding_detections(finding_id, path, snapshot) VALUES ($1, 'engine', '{}') RETURNING id`, finding).Scan(&run); err != nil {
		t.Fatalf("insert a run failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	for _, stmt := range []string{
		`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body) VALUES ($1, $2, 'attempt', 'sigma', 'title: probe')`,
		`INSERT INTO detection_steps(detection_id, seq, step, status) VALUES ($1, 1, 'context', 'ok')`,
		`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, repeat('a', 64), 'attack', 'ok')`,
	} {
		args := []any{run, finding}
		if strings.Count(stmt, "$") == 1 {
			args = args[:1]
		}
		if _, err := q.Exec(stmt, args...); err != nil {
			t.Fatalf("seed failed (%s)", dbtest.AtomicErrorSummary(err))
		}
	}
}

// 1a-4: the recipe drops the four tables, the function, the trigger and the ledger row; it raises, changing
// nothing, on a ledger without 5003, with a newer fork version, or empty; a second run raises; the binary of the
// release before (its migration set) starts on the result without the override; the next start of this binary
// applies 5003 again.
func TestMigrationDetectionDropRecipeIsGuardedAndReversible(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "detdrop")
	d := startup(t, tmp.DSN)
	ctx := context.Background()
	requireEmbeddedLedger(t, readLedger(t, d.DB))
	dropSignalSidecarMigration(t, d)
	seedDetectionRows(t, d.DB)
	full := readLedger(t, d.DB)
	last := full[len(full)-1]

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
	future := appliedMigration{version: 5903, name: "5903_synthetic.sql", checksum: "synthetic", appliedAt: last.appliedAt}
	for _, tc := range []struct {
		name string
		rows []appliedMigration
	}{
		{"5003 not recorded", full[:len(full)-1]},
		{"a newer fork migration on record", append(append([]appliedMigration{}, full...), future)},
		{"ledger empty", nil},
	} {
		setLedger(tc.name, tc.rows)
		before := captureMigrationState(t, tmp)
		requireGuardRefusal(t, tc.name, rollbackRunScript(t, d.DB, rollbackDropDetectionsFile), "5003 is not the newest fork migration")
		requireDetectionObjects(t, tc.name+": after the refused recipe", d.DB)
		requireMigrationStateUnchanged(t, tc.name+": after the refused recipe", before, captureMigrationState(t, tmp))
		if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
			t.Errorf("%s: after the refused recipe: %d granted and %d waiting locks (err %v), want 0 and 0", tc.name, granted, waiting, err)
		}
		if t.Failed() {
			return // a dropped table makes the next case meaningless
		}
	}
	setLedger("restore", full)

	var findings int
	if err := d.QueryRow(`SELECT count(*) FROM findings`).Scan(&findings); err != nil || findings != 1 {
		t.Fatalf("findings before the recipe: %d (err %v), want the one of the seed", findings, err)
	}
	if err := rollbackRunScript(t, d.DB, rollbackDropDetectionsFile); err != nil {
		t.Fatalf("the recipe failed on the ledger it is written for (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireNoDetectionObjects(t, "after the recipe", d.DB)
	requireLedgerOf(t, readLedger(t, d.DB), 1, 2, 3, 4, 5001, 5002)
	if err := d.QueryRow(`SELECT count(*) FROM findings`).Scan(&findings); err != nil || findings != 1 {
		t.Errorf("findings after the recipe: %d (err %v), want the recipe to leave them", findings, err)
	}
	if diff := dbtest.DiffLines(referenceWithoutDetections(loadReference(t)), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("after the recipe the catalog is not the reference without the objects of 5003 (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after the recipe: %d granted and %d waiting locks (err %v), want 0 and 0", granted, waiting, err)
	}

	// A second run raises and changes nothing.
	before := captureMigrationState(t, tmp)
	requireGuardRefusal(t, "a second run", rollbackRunScript(t, d.DB, rollbackDropDetectionsFile), "5003 is not the newest fork migration")
	requireMigrationStateUnchanged(t, "after the second run", before, captureMigrationState(t, tmp))

	// The release before starts on the result without the override, and leaves it as it is.
	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	startMigrated(t, tmp, releaseBefore5003Migrations(t))
	requireLedgerOf(t, readLedger(t, d.DB), 1, 2, 3, 4, 5001, 5002)
	requireNoDetectionObjects(t, "after the start of the release before", d.DB)

	// This binary applies 5003 again.
	d2 := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d2.DB))
	requireDetectionObjects(t, "after the next start", d2.DB)
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d2.DB))); len(diff) > 0 {
		t.Errorf("after the recipe and a start the catalog differs from the reference (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
}

// 1a-4: the four recipes run in the documented order on a database this release started: drop 5003, drop 5002,
// the 0004 recipe, the ledger cleanup. Each of the other three refuses while a newer migration is recorded, and
// changes nothing; each accepts its turn. Afterwards the catalog is that of a database that never had a ledger,
// and the next start applies everything again.
func TestMigrationDetectionRecipesRunInTheDocumentedOrder(t *testing.T) {
	tmp := dbtest.TempDatabase(t, dbtest.RequireServer(t), "detorder")
	d := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d.DB))

	const (
		msg5004    = "5004 is not the newest fork migration"
		msg5003    = "5003 is not the newest fork migration"
		msg5002    = "5002 is not the newest fork migration"
		msg0004    = "schema_migrations is not baseline + 0004"
		msgCleanup = "schema_migrations is not the baseline set"
	)
	type step struct {
		name   string
		run    func() error
		refuse string // message of the guard while the recipe is not yet its turn
	}
	drop5004 := step{"drop 5004", func() error { return rollbackRunScript(t, d.DB, rollbackDropSignalSidecarFile) }, msg5004}
	drop5003 := step{"drop 5003", func() error { return rollbackRunScript(t, d.DB, rollbackDropDetectionsFile) }, msg5003}
	drop5002 := step{"drop 5002", func() error { return rollbackRunScript(t, d.DB, rollbackDropToolRagFile) }, msg5002}
	recipe0004 := step{"the 0004 recipe", func() error { return rollbackRun0004(t, tmp) }, msg0004}
	cleanup := step{"the ledger cleanup", func() error { return rollbackRunCleanup(t, d.DB) }, msgCleanup}

	order := []step{drop5004, drop5003, drop5002, recipe0004, cleanup}
	for i, turn := range order {
		// Every recipe after this one is refused now.
		for _, later := range order[i+1:] {
			before := captureMigrationState(t, tmp)
			requireGuardRefusal(t, later.name+" before "+turn.name, later.run(), later.refuse)
			requireMigrationStateUnchanged(t, later.name+" before "+turn.name+": after the refusal", before, captureMigrationState(t, tmp))
		}
		if err := turn.run(); err != nil {
			t.Fatalf("%s failed on its turn (%s)", turn.name, dbtest.AtomicErrorSummary(err))
		}
		// A second run of the same recipe is refused too (its row or table is gone).
		if turn.name != "the ledger cleanup" {
			requireGuardRefusal(t, turn.name+" a second time", turn.run(), turn.refuse)
		}
	}
	if migrationTableExists(t, d.DB, "schema_migrations") {
		t.Error("the ledger table exists after the cleanup")
	}
	requireNoDetectionObjects(t, "after the four recipes", d.DB)
	if diff := dbtest.DiffLines(referenceWithoutDetections(referenceWithoutToolRag(referenceWithoutLlmUsageTsIndex(referenceWithoutLedger(t)))), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
		t.Errorf("after the four recipes the catalog is not the reference without the ledger, 5003, 5002 and 0004 (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}

	d2 := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d2.DB))
	requireDetectionObjects(t, "after the next start", d2.DB)
	if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d2.DB))); len(diff) > 0 {
		t.Errorf("after the recipes and a start the catalog differs from the reference (%d objects):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
}

// docs/detector-plan.md section 16 (2): the release before 5003 refuses a database that applied it and starts with
// ARTEX_SCHEMA_ALLOW_NEWER=5003, leaving the tables and the ledger as they are; the trigger keeps working.
func TestMigrationDetectionOlderBinaryRefusesUnlessAllowed(t *testing.T) {
	tmp := migrationTemp(t, "detolder")
	startMigrated(t, tmp, embeddedMigrations())
	// The embedded set without 5003 only: 5004 stays shipped, so 5003 is the one unknown row.
	older := migrationSet(t, fstest.MapFS{migrationDetections: nil})

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "")
	requireRefusal(t, tmp, older, "unknown", "5003", migrationDetections, "ARTEX_SCHEMA_ALLOW_NEWER")

	t.Setenv("ARTEX_SCHEMA_ALLOW_NEWER", "5003")
	startMigrated(t, tmp, older)
	conn := tmp.Conn(t)
	requireDetectionObjects(t, "after the older binary's start", conn)
	requireEmbeddedLedger(t, readLedger(t, conn))

	var finding, conversation, run int64
	if err := conn.QueryRow(`INSERT INTO findings(name) VALUES ('older binary') RETURNING id`).Scan(&finding); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`INSERT INTO conversations(agent_key) VALUES ('main') RETURNING id`).Scan(&conversation); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`INSERT INTO finding_detections(finding_id, conversation_id, path, status, snapshot) VALUES ($1, $2, 'agent', 'running', '{}') RETURNING id`, finding, conversation).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`DELETE FROM conversations WHERE id = $1`, conversation); err != nil {
		t.Fatalf("delete the conversation failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	var status, endCode string
	if err := conn.QueryRow(`SELECT status, end_code FROM finding_detections WHERE id = $1`, run).Scan(&status, &endCode); err != nil || status != "stopped" || endCode != "conversation_deleted" {
		t.Errorf("after the older binary's start the trigger left the run %q / %q (err %v), want stopped / conversation_deleted", status, endCode, err)
	}
}

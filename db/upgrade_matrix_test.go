package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// The upgrade matrix (docs/phase-d2-plan.md AC3, AC4, AC5, AC12): for each shape of an older
// database, load its recorded schema and a synthetic data fixture into a throw-away database,
// run startup (Open: schema.sql, the baseline adoption of the migration runner, seedBuiltins), and
// compare. Shapes:
//
//	r0509   releases 0.5.9m to 0.5.11m (one recorded schema)
//	r0512   release 0.5.12m
//	r0513   release 0.5.13m, whose schema.sql is the frozen one
//	s2a     release 0.5.15m, the first with the migration ledger: the frozen schema.sql, the llm tables of the
//	        baseline migrations and the ledger with the four baseline rows
//	legacy  SYNTHETIC: the r0509 schema with the older forms of legacy-mutations.sql put back
//
// None of these tests skips: a missing ARTEX_PG_DSN, a refused DSN or a failed Open is a failure.

type matrixShape struct {
	name      string
	recorded  string // name of the recordedShapes entry whose schema fixture is loaded
	mutations string // file applied after the schema fixture, "" for none
	data      string
	// exempt lists the tables that have no row in the data fixture on purpose, with the reason.
	exempt map[string]string
}

const noFindingComments = "the r0509 DDL (0.5.9m to 0.5.11m) has no finding_comments table (added after 0.5.11m, issue #65); startup creates it empty"

// noLedger: every shape is a database that a release WITHOUT the migration runner left behind, so it has no
// ledger; the first start creates schema_migrations and adopts the baseline set (four rows, listed per shape
// as `added` entries in expected-changes.json).
const noLedger = "the releases of every shape predate the migration runner and have no ledger table; the first start creates it and adopts the baseline (docs/phase-d2-stage2-plan.md 3.2)"

var matrixShapes = []matrixShape{
	{name: "r0509", recorded: "r0509", data: "data-r0509.sql",
		exempt: map[string]string{"finding_comments": noFindingComments, "schema_migrations": noLedger}},
	{name: "r0512", recorded: "r0512", data: "data-r0512.sql",
		exempt: map[string]string{"schema_migrations": noLedger}},
	{name: "r0513", recorded: "r0513", data: "data-r0513.sql",
		exempt: map[string]string{"schema_migrations": noLedger}},
	// s2a has a ledger before startup, so its table schema_migrations has rows and no exemption.
	{name: "s2a", recorded: "s2a", data: "data-r0513.sql"},
	{name: "legacy", recorded: "r0509", mutations: "legacy-mutations.sql", data: "data-legacy.sql",
		exempt: map[string]string{"finding_comments": noFindingComments, "schema_migrations": noLedger}},
}

func readUpgradeFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(upgradeDir, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

func recordedSchemaFile(t *testing.T, shape string) string {
	t.Helper()
	for _, s := range recordedShapes {
		if s.shape == shape {
			return s.file
		}
	}
	t.Fatalf("no recorded shape %q", shape)
	return ""
}

// recordedShapeNamed returns the recordedShapes entry of a shape.
func recordedShapeNamed(t *testing.T, shape string) recordedShape {
	t.Helper()
	for _, s := range recordedShapes {
		if s.shape == shape {
			return s
		}
	}
	t.Fatalf("no recorded shape %q", shape)
	return recordedShape{}
}

// recordedGoDDLFile is the historical Go DDL fixture of a recorded shape: old releases created the
// llm tables with it, not with migrations. It is "" for a shape of a release with the runner.
func recordedGoDDLFile(t *testing.T, shape string) string {
	t.Helper()
	for _, s := range recordedShapes {
		if s.shape == shape {
			return s.goddl
		}
	}
	t.Fatalf("no recorded shape %q", shape)
	return ""
}

// loadLedgeredShape is what a release with the migration runner leaves besides schema.sql: the files of its
// migrations, run in order, and the ledger table with one row per migration, as the runner writes them. The
// migration files are the embedded ones, which are never edited: the recorded sha256 of each must still be the
// sha256 of the embedded file, or the shape could no longer be rebuilt.
func loadLedgeredShape(t *testing.T, tmp *dbtest.Temp, rec recordedShape) {
	t.Helper()
	var rows []string
	for _, m := range rec.migrations {
		content := mustReadEmbedded(t, m.name)
		if got := sha256Hex(content); got != m.sha256 {
			t.Fatalf("shape %s: embedded %s has sha256 %s, the recorded ledger row says %s (a migration file is never edited)", rec.shape, m.name, got, m.sha256)
		}
		tmp.Load(t, "migration "+m.name+" of "+rec.shape, string(content))
		rows = append(rows, fmt.Sprintf("(%d, '%s', '%s')", m.version, m.name, m.sha256))
	}
	tmp.Load(t, "ledger of "+rec.shape, migrationLedgerTable+"INSERT INTO schema_migrations(version, name, checksum) VALUES "+strings.Join(rows, ", ")+";")
}

// buildShape creates the temporary database of a shape and loads, in this order, the
// recorded schema, the historical Go DDL (llm_records, llm_usage) or, for a release with the runner, its
// migrations and ledger, the legacy mutations when the shape has them, and the data fixture. It is the
// state of a database a release left behind.
func buildShape(t *testing.T, srv *dbtest.Server, sh matrixShape) *dbtest.Temp {
	t.Helper()
	tmp := dbtest.TempDatabase(t, srv, "mx_"+sh.name)
	tmp.Load(t, "schema fixture of "+sh.name, readUpgradeFile(t, recordedSchemaFile(t, sh.recorded)))
	if goddl := recordedGoDDLFile(t, sh.recorded); goddl != "" {
		tmp.Load(t, "Go DDL fixture of "+sh.recorded, readUpgradeFile(t, goddl))
	} else {
		loadLedgeredShape(t, tmp, recordedShapeNamed(t, sh.recorded))
	}
	if sh.mutations != "" {
		tmp.Load(t, sh.mutations, readUpgradeFile(t, sh.mutations))
	}
	tmp.Load(t, sh.data, readUpgradeFile(t, sh.data))
	return tmp
}

func takeRows(t *testing.T, q dbtest.Queryer) *dbtest.RowSnapshot {
	t.Helper()
	s, err := dbtest.TakeRows(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func takeDigests(t *testing.T, q dbtest.Queryer) *dbtest.Digests {
	t.Helper()
	d, err := dbtest.TakeDigests(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// sequencesThatFell names the sequences whose last_value is lower after than before.
func sequencesThatFell(before, after *dbtest.Digests) []string {
	var out []string
	for name, b := range before.Sequences {
		if a, ok := after.Sequences[name]; !ok {
			out = append(out, fmt.Sprintf("sequence %s: removed", name))
		} else if a < b {
			out = append(out, fmt.Sprintf("sequence %s: last_value fell %d -> %d", name, b, a))
		}
	}
	sort.Strings(out)
	return out
}

// sequencesThatAdvanced names the sequences whose last_value is higher after than before.
func sequencesThatAdvanced(before, after *dbtest.Digests) []string {
	var out []string
	for name, b := range before.Sequences {
		if a, ok := after.Sequences[name]; ok && a > b {
			out = append(out, fmt.Sprintf("%s %d->%d", name, b, a))
		}
	}
	sort.Strings(out)
	return out
}

func readExpectedChanges(t *testing.T) *dbtest.ExpectedFile {
	t.Helper()
	f, err := os.Open(filepath.Join(upgradeDir, "expected-changes.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	exp, err := dbtest.ReadExpectedChanges(f)
	if err != nil {
		t.Fatal(err)
	}
	return exp
}

// sqlStateOf reduces a driver error to its SQLSTATE: the message of a failed cast carries
// the value.
func sqlStateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "SQLSTATE " + pgErr.Code
	}
	return "no SQLSTATE: connection or driver error"
}

// startupSlack is how far after the database clock, read once startup is over, a timestamp
// may lie and still count as written by startup (clock_timestamp() is read again for each
// comparison, which is after startup).
const startupSlack = "1 minute"

// startupClock reads the database clock before startup and returns the check that a
// timestamp text lies between that moment and the database clock read after startup plus a
// small slack (the "@startup" after value).
func startupClock(t *testing.T, conn *sql.DB) dbtest.StartupCheck {
	t.Helper()
	var t0 string
	if err := conn.QueryRow(`SELECT clock_timestamp()::text`).Scan(&t0); err != nil {
		t.Fatalf("read the database clock failed (%s)", sqlStateOf(err))
	}
	return func(after string) (bool, error) {
		var fresh bool
		if err := conn.QueryRow(`SELECT $1::timestamptz >= $2::timestamptz
			AND $1::timestamptz <= clock_timestamp() + interval '`+startupSlack+`'`, after, t0).Scan(&fresh); err != nil {
			return false, fmt.Errorf("compare timestamps failed (%s)", sqlStateOf(err))
		}
		return fresh, nil
	}
}

// The "@startup" marker has both bounds: a timestamp from before startup, and one from the
// far future, are not a timestamp startup wrote.
func TestUpgradeStartupMarkerBoundsTheTimestampOnBothSides(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "startupclock")
	conn := tmp.Conn(t)
	check := startupClock(t, conn)
	at := func(expr string) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(`SELECT (` + expr + `)::text`).Scan(&s); err != nil {
			t.Fatalf("read the database clock failed (%s)", sqlStateOf(err))
		}
		return s
	}
	for name, c := range map[string]struct {
		ts   string
		want bool
	}{
		"now":               {at(`clock_timestamp()`), true},
		"before startup":    {at(`clock_timestamp() - interval '1 hour'`), false},
		"far in the future": {at(`clock_timestamp() + interval '1 hour'`), false},
		"infinity":          {"infinity", false},
	} {
		got, err := check(c.ts)
		if err != nil || got != c.want {
			t.Errorf("%s: check = %v, %v; want %v", name, got, err, c.want)
		}
	}
}

// to_char of an infinite timestamp is NULL; the row snapshot must keep infinity and NULL apart,
// or a change from one to the other would not be a difference.
func TestUpgradeRowSnapshotKeepsInfinityAndNullApart(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "rowsinf")
	tmp.Load(t, "probe table", `CREATE TABLE probe (id int PRIMARY KEY, ts timestamptz, note text);
		INSERT INTO probe VALUES (1, NULL, NULL), (2, 'infinity', 'infinity'), (3, '-infinity', ''),
		  (4, '2026-01-02 03:04:05.678901+02', 'x');`)
	conn := tmp.Conn(t)
	snap := takeRows(t, conn)
	rows := snap.Tables["probe"]
	if rows == nil || !rows.IsTimestamp("ts") || rows.IsTimestamp("note") {
		t.Fatal("the probe table or its column types were not read")
	}
	val := func(id, col string) string {
		v := rows.Rows[id][col]
		if v == nil {
			return "<NULL>"
		}
		return *v
	}
	want := map[string]string{"1": "<NULL>", "2": "infinity", "3": "-infinity", "4": "2026-01-02T01:04:05.678901Z"}
	for id, w := range want {
		if got := val(id, "ts"); got != w {
			t.Errorf("row %s: ts reads %q, want %q", id, got, w)
		}
	}
	if val("1", "note") != "<NULL>" || val("2", "note") != "infinity" {
		t.Error("a text column is not read as its text")
	}
	// A change from NULL to infinity is a difference.
	before := takeRows(t, conn)
	if _, err := conn.Exec(`UPDATE probe SET ts = 'infinity' WHERE id = 1`); err != nil {
		t.Fatalf("update failed (%s)", sqlStateOf(err))
	}
	after := takeRows(t, conn)
	if d := dbtest.DiffRows(before, after); len(d) != 1 || d[0].Column != "ts" {
		t.Errorf("NULL -> infinity is not reported as one difference in ts: %d difference(s)", len(d))
	}
}

// AC3, AC4: startup on a database a release left behind loses no row and changes no value, except
// exactly the entries of expected-changes.json (among them the four rows of the migration ledger, which
// the baseline adoption adds), and moves no sequence backwards. An entry of
// class defect is a difference nobody has decided on: the test fails and says so.
func TestUpgradeRowsPreserved(t *testing.T) {
	exp := readExpectedChanges(t)
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			changes, ok := exp.Shapes[sh.name]
			if !ok {
				t.Fatalf("expected-changes.json has no entry list for shape %s (an empty list is [])", sh.name)
			}
			srv := dbtest.RequireServer(t)
			tmp := buildShape(t, srv, sh)
			conn := tmp.Conn(t)
			before, digestsBefore := takeRows(t, conn), takeDigests(t, conn)
			startupCheck := startupClock(t, conn)

			d := startup(t, tmp.DSN)
			after, digestsAfter := takeRows(t, d.DB), takeDigests(t, d.DB)

			if res := dbtest.CompareRows(before, after, changes, startupCheck); !res.OK() {
				t.Errorf("upgrade %s:\n%s", sh.name, res.Report())
			}
			if fell := sequencesThatFell(digestsBefore, digestsAfter); len(fell) > 0 {
				t.Errorf("upgrade %s: a sequence moved backwards:\n  %s", sh.name, strings.Join(fell, "\n  "))
			}
		})
	}
}

// AC4: after startup the catalog of an upgraded database equals the fresh one (the committed
// reference, which TestUpgradeFreshDatabaseMatchesReference ties to a fresh database), column
// order ignored: same tables, columns, constraints (and their validity), indexes (and their
// validity), triggers (and their enabled state), functions and sequences.
func TestUpgradeConvergesWithFreshCatalog(t *testing.T) {
	ref := loadReference(t)
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			srv := dbtest.RequireServer(t)
			tmp := buildShape(t, srv, sh)
			d := startup(t, tmp.DSN)
			entries := tmp.Catalog(t, d.DB)
			if n := dbtest.Count(entries, "table"); n != expectedTables {
				t.Errorf("upgrade %s: %d tables after startup, want %d", sh.name, n, expectedTables)
			}
			if diff := dbtest.DiffLines(ref, dbtest.Lines(entries)); len(diff) > 0 {
				t.Errorf("upgrade %s: the catalog after startup differs from the fresh catalog (%s) in %d object(s):\n  %s",
					sh.name, referenceFile, len(diff), strings.Join(diff, "\n  "))
			}
		})
	}
}

// AC5: a further startup changes no catalog object and no row. The only permitted difference
// is agents.updated_at of a built-in agent (db/db.go:258 with the trigger of
// db/schema.sql:830-832), with every other column of those rows equal. Sequences may only
// move forward: every start consumes ids of the upserts that conflict (nextval runs before
// ON CONFLICT), which the test reports with -v and does not count as a change.
func TestUpgradeRepeatedStartupChangesNothing(t *testing.T) {
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			srv := dbtest.RequireServer(t)
			tmp := buildShape(t, srv, sh)

			d1 := startup(t, tmp.DSN)
			rows1, cat1, dig1 := takeRows(t, d1.DB), dbtest.Lines(tmp.Catalog(t, d1.DB)), takeDigests(t, d1.DB)

			d2 := startup(t, tmp.DSN)
			rows2, cat2, dig2 := takeRows(t, d2.DB), dbtest.Lines(tmp.Catalog(t, d2.DB)), takeDigests(t, d2.DB)

			if p := dbtest.RepeatProblems(rows1, rows2); len(p) > 0 {
				t.Errorf("upgrade %s: a repeated startup changed %d row(s) (only agents.updated_at of built-in agents may change):\n  %s",
					sh.name, len(p), strings.Join(p, "\n  "))
			}
			if diff := dbtest.DiffLines(cat1, cat2); len(diff) > 0 {
				t.Errorf("upgrade %s: a repeated startup changed the catalog in %d object(s):\n  %s", sh.name, len(diff), strings.Join(diff, "\n  "))
			}
			if fell := sequencesThatFell(dig1, dig2); len(fell) > 0 {
				t.Errorf("upgrade %s: a sequence moved backwards:\n  %s", sh.name, strings.Join(fell, "\n  "))
			}
			t.Logf("upgrade %s: sequences advanced by the repeated startup: %s", sh.name, strings.Join(sequencesThatAdvanced(dig1, dig2), ", "))
		})
	}
}

// referenceTables are the table names of the committed reference listing.
func referenceTables(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, l := range loadReference(t) {
		if f := strings.Fields(l); len(f) >= 3 && f[0] == "table" {
			out = append(out, f[1])
		}
	}
	return out
}

// AC3, fixture coverage: every one of the 58 tables has at least one row in the data fixture,
// or an exemption with a reason in matrixShapes; an exemption for a table that has rows is
// stale and fails too.
func TestUpgradeFixtureHasRowsInEveryTable(t *testing.T) {
	tables := referenceTables(t)
	if len(tables) != expectedTables {
		t.Fatalf("the reference lists %d tables, want %d", len(tables), expectedTables)
	}
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			srv := dbtest.RequireServer(t)
			tmp := buildShape(t, srv, sh)
			rows := takeRows(t, tmp.Conn(t))
			known := map[string]bool{}
			for _, tbl := range tables {
				known[tbl] = true
				if rows.RowCount(tbl) > 0 {
					if reason, exempt := sh.exempt[tbl]; exempt {
						t.Errorf("shape %s: table %s has rows but is listed as exempt (%s): drop the exemption", sh.name, tbl, reason)
					}
					continue
				}
				if reason, exempt := sh.exempt[tbl]; !exempt || strings.TrimSpace(reason) == "" {
					t.Errorf("shape %s: the fixture has no row in table %s and there is no exemption with a reason", sh.name, tbl)
				}
			}
			for tbl := range sh.exempt {
				if !known[tbl] {
					t.Errorf("shape %s: exemption for %s, which is not a table of the schema", sh.name, tbl)
				}
			}
		})
	}
}

// startupKeyCount is the number of settings keys startup writes: the 19 `const flag` keys of
// the flags files and the six that flags-README.txt lists.
const startupKeyCount = 25

// operatorSettingKeys are the settings rows of the data fixtures that no startup writer owns.
var operatorSettingKeys = []string{"traffic_capture", "canary.operator_note"}

var readmeKeyLine = regexp.MustCompile(`^([a-z0-9_]+) ([A-Za-z0-9_./-]+):([0-9]+)$`)

// startupSettingsKeys returns the 25 startup keys for a flags file: its keys, and the six
// listed in flags-README.txt. The README line of each of the six must point at a line that
// holds the key.
func startupSettingsKeys(t *testing.T, flagsFile string) []string {
	t.Helper()
	keys := strings.Fields(readUpgradeFile(t, flagsFile))
	for _, line := range strings.Split(readUpgradeFile(t, "flags-README.txt"), "\n") {
		m := readmeKeyLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		key, file, lineNo := m[1], m[2], m[3]
		b, err := os.ReadFile(filepath.Join("..", file))
		if err != nil {
			t.Errorf("flags-README.txt: %s: %v", file, err)
			continue
		}
		n, _ := strconv.Atoi(lineNo)
		lines := strings.Split(string(b), "\n")
		if n < 1 || n > len(lines) || !strings.Contains(lines[n-1], `"`+key+`"`) {
			t.Errorf("flags-README.txt: %s:%d does not hold the key %s", file, n, key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// AC3, fixture coverage: the settings rows of a data fixture are exactly the 25 startup keys
// (the flag rows of every one-time seeder: a missing one would let that seeder run on the
// fixture as a first-time run) and the two operator rows, nothing else.
func TestUpgradeFixtureSettingsAreTheStartupKeys(t *testing.T) {
	for _, sh := range matrixShapes {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			var flagsFile string
			for _, rs := range recordedShapes {
				if rs.shape == sh.recorded {
					flagsFile = "flags-" + rs.releases[len(rs.releases)-1].tag + ".txt"
				}
			}
			want := startupSettingsKeys(t, flagsFile)
			if len(want) != startupKeyCount {
				t.Fatalf("%s and flags-README.txt give %d startup keys, want %d", flagsFile, len(want), startupKeyCount)
			}
			want = append(want, operatorSettingKeys...)
			sort.Strings(want)

			srv := dbtest.RequireServer(t)
			tmp := buildShape(t, srv, sh)
			r, err := tmp.Conn(t).Query(`SELECT key FROM settings ORDER BY key`)
			if err != nil {
				t.Fatalf("read settings failed (%s)", sqlStateOf(err))
			}
			defer r.Close()
			var got []string
			for r.Next() {
				var k string
				if err := r.Scan(&k); err != nil {
					t.Fatal("settings scan failed")
				}
				got = append(got, k)
			}
			if err := r.Err(); err != nil {
				t.Fatalf("read settings failed (%s)", sqlStateOf(err))
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				have := map[string]bool{}
				for _, k := range got {
					have[k] = true
				}
				var missing, extra []string
				for _, k := range want {
					if !have[k] {
						missing = append(missing, k)
					}
					delete(have, k)
				}
				for k := range have {
					extra = append(extra, k)
				}
				sort.Strings(extra)
				t.Errorf("shape %s: the fixture settings rows are not the %d startup keys and the operator rows.\nmissing: %s\nunexpected: %s",
					sh.name, startupKeyCount, strings.Join(missing, ", "), strings.Join(extra, ", "))
			}
		})
	}
}

// The expected-changes file is checked without a database: shape of every entry (done by the
// reader), a list for every shape, a cause that points at a line that exists.
func TestUpgradeExpectedChangesFile(t *testing.T) {
	exp := readExpectedChanges(t)
	for _, sh := range matrixShapes {
		if _, ok := exp.Shapes[sh.name]; !ok {
			t.Errorf("no entry list for shape %s", sh.name)
		}
	}
	for shape := range exp.Shapes {
		known := false
		for _, sh := range matrixShapes {
			known = known || sh.name == shape
		}
		if !known {
			t.Errorf("entry list for unknown shape %s", shape)
		}
	}
	lineCounts := map[string]int{}
	for shape, list := range exp.Shapes {
		for i, c := range list {
			for _, ref := range c.Causes() {
				file, lines, _ := strings.Cut(ref, ":")
				first, last, _ := strings.Cut(lines, "-")
				lo, err1 := strconv.Atoi(first)
				hi, err2 := lo, error(nil)
				if last != "" {
					hi, err2 = strconv.Atoi(last)
				}
				if err1 != nil || err2 != nil || lo < 1 || hi < lo {
					t.Errorf("shape %s entry %d: cause %q has an invalid line range", shape, i+1, ref)
					continue
				}
				n, ok := lineCounts[file]
				if !ok {
					b, err := os.ReadFile(filepath.Join("..", file))
					if err != nil {
						t.Errorf("shape %s entry %d: cause %q: %v", shape, i+1, ref, err)
						continue
					}
					n = strings.Count(string(b), "\n")
					lineCounts[file] = n
				}
				if hi > n {
					t.Errorf("shape %s entry %d: cause %q is beyond the end of %s (%d lines)", shape, i+1, ref, file, n)
				}
			}
		}
	}
}

// R3 (docs/phase-d2-plan.md): the legacy shape must really enter the upgrade branches of
// db/schema.sql. Each condition below is the condition of a branch (or of a data statement);
// it must hold before startup and not after it, and the recorded r0509 schema with its own
// data must not hold it, so that the mutations, not the DDL, are what reaches the branch.
type legacyCondition struct {
	name, where, query string // query returns one boolean: the branch condition holds
}

var legacyConditions = []legacyCondition{
	{"ck_node_kind lacks 'digest'", "db/schema.sql:212-224", `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid = 'exploration_nodes'::regclass AND conname = 'ck_node_kind'
		  AND pg_get_constraintdef(oid) NOT LIKE '%digest%')`},
	{"ck_node_state lacks paused, superseded or deleted", "db/schema.sql:227-247", `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid = 'exploration_nodes'::regclass AND conname = 'ck_node_state'
		  AND (pg_get_constraintdef(oid) NOT LIKE '%paused%' OR pg_get_constraintdef(oid) NOT LIKE '%superseded%'
		       OR pg_get_constraintdef(oid) NOT LIKE '%deleted%'))`},
	{"exploration_edges has a rel CHECK without 'covers'", "db/schema.sql:270-282", `SELECT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid = 'exploration_edges'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%rel%' AND pg_get_constraintdef(oid) NOT LIKE '%covers%')`},
	{"llm_profiles has no thinking_type column", "db/schema.sql:478-490", `SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'llm_profiles' AND column_name = 'thinking_type')`},
	{"agents has no fk_agents_curprompt", "db/schema.sql:845-848", `SELECT NOT EXISTS (SELECT 1 FROM pg_constraint
		WHERE conrelid = 'agents'::regclass AND conname = 'fk_agents_curprompt')`},
	{"the old uq_task_scope index exists", "db/schema.sql:781", `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'uq_task_scope')`},
	{"uq_task_scope_v2 does not exist", "db/schema.sql:782-783", `SELECT NOT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'uq_task_scope_v2')`},
	{"assets.company_source is nullable", "db/schema.sql:105-108", `SELECT is_nullable = 'YES' FROM information_schema.columns
		WHERE table_name = 'assets' AND column_name = 'company_source'`},
	{"an asset has company_source NULL", "db/schema.sql:106", `SELECT EXISTS (SELECT 1 FROM assets WHERE company_source IS NULL)`},
	{"an intercept_pending row has decision_source ''", "db/schema.sql:1115-1116", `SELECT EXISTS (SELECT 1 FROM intercept_pending WHERE decision_source = '')`},
}

func evaluateConditions(t *testing.T, q *sql.DB) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, c := range legacyConditions {
		var v bool
		if err := q.QueryRow(c.query).Scan(&v); err != nil {
			t.Fatalf("condition %q (%s) failed (%s)", c.name, c.where, sqlStateOf(err))
		}
		out[c.name] = v
	}
	return out
}

func TestUpgradeLegacyBranchesWereEntered(t *testing.T) {
	srv := dbtest.RequireServer(t)
	for _, sh := range matrixShapes {
		sh := sh
		if sh.name == "r0512" || sh.name == "r0513" || sh.name == "s2a" {
			continue // identical DDL to r0509 apart from finding_comments and findings.ticket_id; not a legacy candidate
		}
		t.Run(sh.name, func(t *testing.T) {
			tmp := buildShape(t, srv, sh)
			before := evaluateConditions(t, tmp.Conn(t))
			d := startup(t, tmp.DSN)
			after := evaluateConditions(t, d.DB)
			for _, c := range legacyConditions {
				switch sh.name {
				case "legacy":
					if !before[c.name] {
						t.Errorf("legacy: before startup the branch condition %q (%s) does not hold: the mutation does not reach the branch", c.name, c.where)
					}
					if after[c.name] {
						t.Errorf("legacy: after startup the branch condition %q (%s) still holds: startup did not enter the branch", c.name, c.where)
					}
				default:
					if before[c.name] {
						t.Errorf("%s: the recorded shape already satisfies %q (%s): it would enter a legacy branch by itself", sh.name, c.name, c.where)
					}
				}
			}
		})
	}
}

// AC12: rows carry the row canary, the test server's password is the password canary
// (TestUpgradeCanaryPasswordDoesNotReachOutput covers the role); neither may appear in a log
// line or in the failure message of a deliberately failed comparison. The fixture must hold
// the canary, or the check proves nothing.
func TestUpgradeMatrixOutputIsContentFree(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	srv := dbtest.RequireServer(t)
	var legacy matrixShape
	for _, sh := range matrixShapes {
		if sh.name == "legacy" {
			legacy = sh
		}
	}
	tmp := buildShape(t, srv, legacy)
	conn := tmp.Conn(t)
	before, digestsBefore := takeRows(t, conn), takeDigests(t, conn)
	if n := before.CountValuesContaining(dbtest.RowCanary); n < 100 {
		t.Fatalf("the fixture holds the row canary in only %d value(s): the output check would prove little", n)
	}
	d := startup(t, tmp.DSN)
	after, digestsAfter := takeRows(t, d.DB), takeDigests(t, d.DB)

	// Deliberate failure 1: no expected changes at all, so every difference is reported.
	res := dbtest.CompareRows(before, after, nil, nil)
	if len(res.Problems) == 0 {
		t.Fatal("the legacy upgrade changed nothing: the deliberate failure did not happen")
	}
	dbtest.AssertNoSecret(t, "report of a comparison with no expected changes", res.Report())

	// Deliberate failure 2: entries whose exact values are wrong. The canary is in the file
	// values, and must not come out in the message.
	wrong := func(s string) json.RawMessage { b, _ := json.Marshal(s); return b }
	bad := []dbtest.ExpectedChange{
		{Kind: dbtest.KindValue, Table: "agents", PK: map[string]string{"id": "4"}, Column: "name",
			Before: wrong(dbtest.RowCanary + " wrong before"), After: wrong(dbtest.RowCanary + " wrong after"),
			Cause: "db/db.go:258", Class: dbtest.ClassDefect},
		{Kind: dbtest.KindDeleted, Table: "settings", PK: map[string]string{"key": dbtest.RowCanary + "-key"},
			Cause: "db/db.go:280", Class: dbtest.ClassDefect},
	}
	res = dbtest.CompareRows(before, after, bad, nil)
	if len(res.Problems) == 0 || len(res.Awaiting) != 0 {
		t.Fatalf("the wrong entries were not reported as problems (problems %d, awaiting %d)", len(res.Problems), len(res.Awaiting))
	}
	dbtest.AssertNoSecret(t, "report of a comparison with wrong entries", res.Report())

	// Deliberate failure 3: a table whose rows are changed by hand, then the digest and row
	// differences.
	if _, err := d.Exec(`UPDATE companies SET logo = $1`, dbtest.RowCanary+"-changed"); err != nil {
		t.Fatalf("change a row: %v", sqlStateOf(err))
	}
	changed, digestsChanged := takeRows(t, d.DB), takeDigests(t, d.DB)
	p := dbtest.RepeatProblems(after, changed)
	if len(p) == 0 {
		t.Fatal("the deliberate change of a row was not reported")
	}
	dbtest.AssertNoSecret(t, "repeat-startup problems", strings.Join(p, "\n"))
	dbtest.AssertNoSecret(t, "digest difference", strings.Join(dbtest.DiffDigests(digestsAfter, digestsChanged), "\n"))
	dbtest.AssertNoSecret(t, "digest difference of the upgrade", strings.Join(dbtest.DiffDigests(digestsBefore, digestsAfter), "\n"))
	dbtest.AssertNoSecret(t, "log output", logs())
}

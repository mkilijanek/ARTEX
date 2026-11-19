package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// AC8 (docs/phase-d2-plan.md): task archives are a second old schema that startup never
// touches. A package written by an older binary is restored by the current one.

// archiveTaskID is the task of testdata/upgrade/archive-r0509.sql.
const archiveTaskID = 701

// archiveRestoreDefaults names, per "table.column", the normaliser of the restore path
// that supplies a value for a column that is NOT NULL now and absent from packages
// written before it existed. json_populate_recordset inserts NULL for an absent key
// and ignores column defaults, so every such column needs one (db/finding_comments_
// archive.go and db/task_archives_restore.go carry them). Empty today: since the r0509
// shape no column was added to an archived table (the only difference is the new
// table finding_comments, which an old package does not have a key for). A column
// added later without an entry here fails TestUpgradeArchiveColumnsSurviveUpgrade.
var archiveRestoreDefaults = map[string]string{}

// archiveNewTableRestore documents why a table that is absent from the old shape needs
// no column defaults: the old package has no key for it and the restore path treats a
// missing key as zero rows (db/finding_comments_archive.go). TestUpgradeArchiveRestores
// AfterSchemaUpgrade proves it with a real package.
var archiveNewTableRestore = map[string]string{
	"finding_comments": "a package without the key restores zero comments",
}

// archiveOldDB opens a pool on an r0509 database without calling Open: Open would
// upgrade it. The Go DDL steps of startup (llm_records, llm_usage) run as a 0.5.9m
// server did, because the snapshot reads both tables.
func archiveOldDB(t *testing.T, tmp *dbtest.Temp) *DB {
	t.Helper()
	d := &DB{tmp.Conn(t)}
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("EnsureLLMRecordsTable on r0509 failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatalf("EnsureLLMUsageTable on r0509 failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	return d
}

// archiveKnownRestoreDiffs is the complete set of differences that archive and restore
// leave in the archived tables, with their cause. RestoreTaskArchive updates the task and
// exploration stub rows and writes updated_at = archived.updated_at
// (db/task_archives_restore.go, restoreTaskStub and restoreExplorationStub), but the
// BEFORE UPDATE triggers trg_tasks_upd and trg_exp_upd (set_updated_at,
// db/schema.sql:7-10, :168, :558) overwrite it with now(). The difference exists with or
// without a schema upgrade; it is accepted by the owner on 2026-11-19 (backlog item: the
// restore keeps the archived timestamps). The test pins it exactly: any other difference
// fails, and so does the disappearance of these two (then the entries must be removed).
var archiveKnownRestoreDiffs = []string{
	"table explorations: column updated_at values changed",
	"table tasks: column updated_at values changed",
}

// archiveTableDigests keeps the tables of the snapshot (minus the new table
// finding_comments) and drops sequences: restore keeps ids, so no sequence is involved.
func archiveTableDigests(all *dbtest.Digests, tables []string, skip map[string]bool) *dbtest.Digests {
	out := &dbtest.Digests{Tables: map[string]dbtest.TableDigest{}, Sequences: map[string]int64{}}
	for _, name := range tables {
		if skip[name] {
			continue
		}
		if td, ok := all.Tables[name]; ok {
			out.Tables[name] = td
		}
	}
	return out
}

// AC8(a): a format-3 package written on r0509 (it has no finding_comments key), taken
// with the real snapshot function and compacted with the real archive function, is
// restored by the real restore function after Open upgraded the database to the
// current schema. The digests of every archived table equal those before archiving.
//
// The snapshot function of the current code reads finding_comments, a table the r0509
// shape lacks, so the test creates that one table (with the definition of the current
// schema.sql, extracted from the r0512 fixture) for the duration of the snapshot and
// removes it again, then deletes its key from the package, which is what a package of
// an older binary looks like. Every other table is read in its r0509 shape.
func TestUpgradeArchiveRestoresAfterSchemaUpgrade(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "archive")
	tmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
	old := archiveOldDB(t, tmp)
	tmp.Load(t, "archive rows", atomicFixture(t, "archive-r0509.sql"))
	if atomicTableExists(t, tmp, "finding_comments") {
		t.Fatal("precondition: the r0509 shape must not have finding_comments")
	}
	ctx := context.Background()

	// A task of the fixture must be eligible and have rows in the archived tables.
	archive, err := old.QueueTaskArchive(archiveTaskID)
	if err != nil {
		t.Fatalf("QueueTaskArchive failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if claimed, err := old.ClaimTaskArchiveJob(ctx); err != nil || claimed == nil || claimed.ID != archive.ID || claimed.State != Archiving {
		t.Fatalf("ClaimTaskArchiveJob: %+v, err %v", claimed, err)
	}

	// Snapshot with a temporary finding_comments (see the comment above).
	conn := tmp.Conn(t)
	if _, err := conn.ExecContext(ctx, archiveFindingCommentsDDL(t)); err != nil {
		t.Fatalf("create the temporary finding_comments failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	var llmRecords bytes.Buffer
	snapshot, err := old.SnapshotTaskArchiveWithLLMRecords(archiveTaskID, &llmRecords)
	if _, dropErr := conn.ExecContext(ctx, `DROP TABLE finding_comments`); dropErr != nil {
		t.Fatalf("drop the temporary finding_comments failed (%s)", dbtest.AtomicErrorSummary(dropErr))
	}
	if err != nil {
		t.Fatalf("SnapshotTaskArchiveWithLLMRecords failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if snapshot.FormatVersion != 3 {
		t.Fatalf("snapshot format %d, want 3", snapshot.FormatVersion)
	}
	if rawRowCount(snapshot.Tables["finding_comments"]) != 0 {
		t.Fatal("the temporary finding_comments table was not empty")
	}
	delete(snapshot.Tables, "finding_comments")
	delete(snapshot.DataCounts, "finding_comments")
	tables := make([]string, 0, len(snapshot.Tables))
	for name := range snapshot.Tables {
		tables = append(tables, name)
	}
	// The package must really carry rows: every table of the snapshot except the ones the
	// fixture documents as empty, and the streamed llm_records.
	for _, name := range tables {
		if snapshot.DataCounts[name] == 0 {
			t.Errorf("the fixture has no rows in archived table %s", name)
		}
	}
	if snapshot.StreamedTables["llm_records"] != TaskArchiveLLMRecordsPath || llmRecords.Len() == 0 {
		t.Fatal("llm_records was not streamed into the package")
	}
	// A package on disk is JSON: restore from what a reader of the file would decode.
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk TaskArchiveSnapshot
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}

	// The digests that restore must reproduce: the archived tables as they were
	// before compaction (finding_comments is not among them).
	beforeAll, err := dbtest.TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	skip := map[string]bool{"finding_comments": true}
	before := archiveTableDigests(beforeAll, tables, skip)
	if len(before.Tables) != len(tables) {
		t.Fatalf("digests cover %d of %d archived tables", len(before.Tables), len(tables))
	}

	if err := old.CompleteTaskArchive(archive.ID, &onDisk, "/nonexistent/upgrade-test.tar.zst", "0", 1, 1); err != nil {
		t.Fatalf("CompleteTaskArchive failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	compactedAll, err := dbtest.TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if diff := dbtest.DiffDigests(before, archiveTableDigests(compactedAll, tables, skip)); len(diff) == 0 {
		t.Fatal("archiving removed nothing: the comparison after restore would prove nothing")
	}

	// Upgrade: the current schema over the r0509 database holding the compacted stub.
	upgraded := startup(t, tmp.DSN)
	if !atomicTableExists(t, tmp, "finding_comments") {
		t.Fatal("the upgrade did not create finding_comments")
	}

	if _, err := upgraded.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatalf("QueueTaskArchiveRestore failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if claimed, err := upgraded.ClaimTaskArchiveJob(ctx); err != nil || claimed == nil || claimed.ID != archive.ID || claimed.State != Restoring {
		t.Fatalf("ClaimTaskArchiveJob for the restore: %+v, err %v", claimed, err)
	}
	warnings, err := upgraded.RestoreTaskArchiveWithLLMRecords(archive.ID, &onDisk, 0, bytes.NewReader(llmRecords.Bytes()))
	if err != nil {
		t.Fatalf("RestoreTaskArchiveWithLLMRecords on the upgraded database failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if len(warnings) != 0 {
		t.Errorf("restore returned %d warnings (a warning means something was skipped or changed)", len(warnings))
	}
	if n := atomicCount(t, tmp, `SELECT count(*) FROM finding_comments`); n != 0 {
		t.Errorf("a package without comments restored %d comments", n)
	}

	afterAll, err := dbtest.TakeDigests(ctx, tmp.Conn(t))
	if err != nil {
		t.Fatal(err)
	}
	after := archiveTableDigests(afterAll, tables, skip)
	diff := dbtest.DiffDigests(before, after)
	for _, known := range archiveKnownRestoreDiffs {
		found := false
		for i, line := range diff {
			if line == known {
				diff = append(diff[:i], diff[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the known restore difference %q is gone: remove it from archiveKnownRestoreDiffs", known)
		}
	}
	if len(diff) > 0 {
		t.Errorf("the archived tables differ after archive, upgrade and restore (%d differences):\n  %s", len(diff), strings.Join(diff, "\n  "))
	}
	dbtest.AssertNoSecret(t, "log output", logs())
	var live sql.NullTime
	if err := tmp.Conn(t).QueryRow(`SELECT deleted_at FROM tasks WHERE id=$1`, archiveTaskID).Scan(&live); err != nil || live.Valid {
		t.Errorf("the restored task is still deleted (err %v)", err)
	}
}

var findingCommentsDDL = regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS finding_comments \(.*?\n\);`)

// archiveFindingCommentsDDL extracts the table definition from the r0512 fixture.
func archiveFindingCommentsDDL(t *testing.T) string {
	t.Helper()
	ddl := findingCommentsDDL.FindString(atomicSchema(t, "r0512"))
	if ddl == "" {
		t.Fatal("the r0512 fixture has no finding_comments definition")
	}
	return ddl
}

// AC8(b): every column of every archived table in the old shape exists in the current
// schema with the same type and nullability, and every NOT NULL column that is new since
// then has a restore default (archiveRestoreDefaults). The list of archived tables is
// the key set of a real snapshot, so it cannot drift from the code.
func TestUpgradeArchiveColumnsSurviveUpgrade(t *testing.T) {
	srv := dbtest.RequireServer(t)
	oldTmp := dbtest.TempDatabase(t, srv, "archcolold")
	oldTmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
	archiveOldDB(t, oldTmp)

	curTmp := dbtest.TempDatabase(t, srv, "archcolnew")
	cur := startup(t, curTmp.DSN)
	task, err := cur.CreateTaskWithOptions("CANARY-D2-ROW archive columns", "CANARY-D2-ROW goal", TaskCreateOptions{Name: "CANARY-D2-ROW"})
	if err != nil {
		t.Fatalf("create task failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	snapshot, err := cur.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatalf("SnapshotTaskArchive failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	tables := make([]string, 0, len(snapshot.Tables))
	for name := range snapshot.Tables {
		tables = append(tables, name)
	}
	if len(tables) < 23 {
		t.Fatalf("the snapshot lists %d tables, the plan counted 23 and the list only grows", len(tables))
	}

	ctx := context.Background()
	oldCols, err := dbtest.ArchiveColumns(ctx, oldTmp.Conn(t), tables)
	if err != nil {
		t.Fatal(err)
	}
	curCols, err := dbtest.ArchiveColumns(ctx, curTmp.Conn(t), tables)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range tables {
		if _, present := oldCols[name]; !present {
			if _, documented := archiveNewTableRestore[name]; !documented {
				t.Errorf("table %s is new since r0509 and has no documented restore behaviour", name)
			}
		}
	}
	if violations := dbtest.ArchiveColumnGuard(oldCols, curCols, archiveRestoreDefaults); len(violations) > 0 {
		t.Errorf("archived columns are not compatible with packages of the r0509 shape:\n  %s", strings.Join(violations, "\n  "))
	}
}

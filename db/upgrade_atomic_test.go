package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mkilijanek/artex/db/dbtest"
)

// AC6 (docs/phase-d2-plan.md): the schema apply of Open is all-or-nothing.
//
// LIMIT, stated on purpose: these tests cover the DDL transaction only, that is the
// one implicit transaction in which pgx runs schema.sql as a multi-statement
// simple-protocol query (db/db.go, applySchemaWithRetry). seedBuiltins is NOT covered:
// after a failed apply it is not reached, and when it does run it is not
// transactional (every statement commits by itself on another pooled connection).
//
// What "unchanged" means: the catalog listing and the table digests (row counts and
// one hash per column) are equal before and after, and no sequence is lower.
//
// Sequences are the one exception to equality, and it is PostgreSQL's, not the
// schema's: nextval is never rolled back. When the apply runs to its last statement
// (case a), "INSERT INTO mcp_servers ... ON CONFLICT (name) DO NOTHING"
// (db/schema.sql:899-901) consumes a value of mcp_servers_id_seq before it finds the
// conflicting row, and the rollback does not give it back. The test allows exactly that
// sequence to rise and no other; a rise anywhere else, or any fall, fails.

// atomicSchema returns the recorded schema text of a shape (db/upgrade_fixture_test.go).
func atomicSchema(t *testing.T, shape string) string {
	t.Helper()
	for _, s := range recordedShapes {
		if s.shape == shape {
			return atomicFixture(t, s.file)
		}
	}
	t.Fatalf("no recorded shape %q", shape)
	return ""
}

// atomicFixture reads one file of testdata/upgrade.
func atomicFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(upgradeDir, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// atomicState is the content-free state of a database.
type atomicState struct {
	lines   []string
	digests *dbtest.Digests
}

func atomicCapture(t *testing.T, tmp *dbtest.Temp) atomicState {
	t.Helper()
	conn := tmp.Conn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	digests, err := dbtest.TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	return atomicState{lines: dbtest.Lines(tmp.Catalog(t, conn)), digests: digests}
}

// atomicRequireUnchanged fails with object names, counts and sequence values only. A
// sequence named in mayRise may be higher after than before (see the comment at the
// top of this file); it must not be lower.
func atomicRequireUnchanged(t *testing.T, what string, before, after atomicState, mayRise ...string) {
	t.Helper()
	for _, name := range mayRise {
		b, a := before.digests.Sequences[name], after.digests.Sequences[name]
		if a > b {
			t.Logf("%s: sequence %s rose %d -> %d (nextval is not transactional)", what, name, b, a)
			after.digests.Sequences[name] = b
		}
	}
	if diff := dbtest.DiffLines(before.lines, after.lines); len(diff) > 0 {
		t.Errorf("%s: the catalog changed (%d objects):\n  %s", what, len(diff), strings.Join(diff, "\n  "))
	}
	if diff := dbtest.DiffDigests(before.digests, after.digests); len(diff) > 0 {
		t.Errorf("%s: row digests or sequences changed (%d differences):\n  %s", what, len(diff), strings.Join(diff, "\n  "))
	}
}

func atomicTableExists(t *testing.T, tmp *dbtest.Temp, name string) bool {
	t.Helper()
	var exists bool
	if err := tmp.Conn(t).QueryRow(`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("look up table %s failed (%s)", name, dbtest.AtomicErrorSummary(err))
	}
	return exists
}

func atomicCount(t *testing.T, tmp *dbtest.Temp, query string) int {
	t.Helper()
	var n int
	if err := tmp.Conn(t).QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	return n
}

// AC6(a): a schema text whose LAST statement fails. schemaSQL is a package variable
// (db/db.go embeds it), so the test appends one failing statement to it for the
// duration of the test; db/schema.sql itself is untouched. Every statement before it
// succeeds, so a non-atomic apply would leave all of its effects behind: the
// finding_comments table (new since the recorded r0509 shape), the task-asset link and
// the task LLM chain that the backfills create, and recreated triggers.
func TestUpgradeAtomicApplyRollsBackWhenLastStatementFails(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "atomica")
	tmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
	tmp.Load(t, "violation base rows", atomicFixture(t, "violation-base.sql"))
	if atomicTableExists(t, tmp, "finding_comments") {
		t.Fatal("precondition: the r0509 shape must not have finding_comments")
	}
	if n := atomicCount(t, tmp, `SELECT count(*) FROM task_asset_links`); n != 0 {
		t.Fatalf("precondition: the fixture must leave the task-asset link missing, found %d links", n)
	}
	before := atomicCapture(t, tmp)

	original := schemaSQL
	schemaSQL = original + "\nSELECT 1 / 0;\n"
	restored := false
	restore := func() {
		if !restored {
			schemaSQL = original
			restored = true
		}
	}
	t.Cleanup(restore)

	d, err := Open(tmp.DSN)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although the last statement of the schema fails")
	}
	if got := dbtest.AtomicErrorSummary(err); !strings.Contains(got, "SQLSTATE 22012") || !strings.HasPrefix(err.Error(), "apply schema: ") {
		t.Fatalf("Open failed, but not in the schema apply by the appended statement: %s", got)
	}
	restore()

	after := atomicCapture(t, tmp)
	atomicRequireUnchanged(t, "after the failed apply", before, after, "mcp_servers_id_seq")
	if atomicTableExists(t, tmp, "finding_comments") {
		t.Error("finding_comments exists after the failed apply: the apply is not atomic")
	}

	// Control: the same database upgrades with the real schema, so the rolled-back
	// apply had real work to undo and the comparison above is not vacuous.
	d, err = Open(tmp.DSN)
	if err != nil {
		t.Fatalf("control: Open with the real schema failed: %s", dbtest.AtomicErrorSummary(err))
	}
	d.Close()
	if !atomicTableExists(t, tmp, "finding_comments") {
		t.Error("control: finding_comments was not created by the real schema")
	}
	if n := atomicCount(t, tmp, `SELECT count(*) FROM task_asset_links`); n != 1 {
		t.Errorf("control: the backfill created %d links, want 1", n)
	}
}

// AC6(b): the real schema against an r0509 database that holds one row violating a
// CHECK the schema drops and re-adds on every start (task_scope_kind_check,
// db/schema.sql:777-779). The statements before the CHECK already did work when the
// ADD CONSTRAINT fails: the task-asset link backfill and the task LLM chain backfill
// (db/schema.sql:712-717, :745-755). A non-atomic apply would leave both behind.
func TestUpgradeAtomicApplyRollsBackOnCheckViolation(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "atomicb")
	tmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
	tmp.Load(t, "violation base rows", atomicFixture(t, "violation-base.sql"))
	tmp.Load(t, "violating row", atomicFixture(t, "violation-task-scope-kind.sql"))
	linksBefore := atomicCount(t, tmp, `SELECT count(*) FROM task_asset_links`)
	chainBefore := atomicCount(t, tmp, `SELECT count(*) FROM task_llm_profiles`)
	if linksBefore != 0 || chainBefore != 0 {
		t.Fatalf("precondition: backfill targets must be missing, found %d links and %d chain entries", linksBefore, chainBefore)
	}
	before := atomicCapture(t, tmp)

	d, err := Open(tmp.DSN)
	if err == nil {
		d.Close()
		t.Fatal("Open succeeded although a row violates a re-added CHECK")
	}
	errViolation := err
	if got := dbtest.AtomicErrorSummary(err); !strings.Contains(got, "SQLSTATE 23514") || !dbtest.AtomicErrorMentions(err, "task_scope_kind_check") || !strings.HasPrefix(err.Error(), "apply schema: ") {
		t.Fatalf("Open failed, but not by the task_scope_kind_check violation in the schema apply: %s", got)
	}

	after := atomicCapture(t, tmp)
	atomicRequireUnchanged(t, "after the failed apply", before, after)
	if n := atomicCount(t, tmp, `SELECT count(*) FROM task_asset_links`); n != 0 {
		t.Errorf("%d task-asset links exist after the failed apply: the apply is not atomic", n)
	}
	if n := atomicCount(t, tmp, `SELECT count(*) FROM task_llm_profiles`); n != 0 {
		t.Errorf("%d task LLM chain entries exist after the failed apply: the apply is not atomic", n)
	}
	if atomicTableExists(t, tmp, "finding_comments") {
		t.Error("finding_comments exists after the failed apply: the apply is not atomic")
	}

	// Control: with the row removed the same database upgrades and the backfills run.
	if _, err := tmp.Conn(t).Exec(`DELETE FROM task_scope WHERE kind NOT IN ('company','root_domain','subdomain','ip','cidr','icp','keyword')`); err != nil {
		t.Fatalf("control: remove the violating row failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	d, err = Open(tmp.DSN)
	if err != nil {
		t.Fatalf("control: Open after removing the violating row failed: %s", dbtest.AtomicErrorSummary(err))
	}
	d.Close()
	if n := atomicCount(t, tmp, `SELECT count(*) FROM task_asset_links`); n != 1 {
		t.Errorf("control: the backfill created %d links, want 1", n)
	}
	if n := atomicCount(t, tmp, `SELECT count(*) FROM task_llm_profiles`); n != 1 {
		t.Errorf("control: the backfill created %d chain entries, want 1", n)
	}
	// AC12: the violating row carries the row canary; neither the failure summary nor
	// the log may.
	dbtest.AssertNoSecret(t, "log output", logs())
	dbtest.AssertNoSecret(t, "failure summary", dbtest.AtomicErrorSummary(errViolation))
}

// AC7: lock coordination on the advisory key schemaMigrationLockKey.
//
// Session-level (pg_advisory_lock) and transaction-level (pg_advisory_xact_lock)
// advisory locks share ONE key space in PostgreSQL: a transaction that holds the key
// blocks a session that asks for it, and the other way round. That is why archive
// transactions (coordinateWithSchemaMigration, transaction level) exclude startup DDL
// (withSchemaMigrationLock, session level), and the blocking test below asserts it.

// AC7(c): the key is part of the contract between startup and the archive paths.
func TestUpgradeLockKeyIsUnchanged(t *testing.T) {
	if schemaMigrationLockKey != 7337741001 {
		t.Fatalf("schemaMigrationLockKey is %d, want 7337741001: another binary of the same database may still use the old key", schemaMigrationLockKey)
	}
	// The archive paths take the same key as a transaction lock.
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "lockkey")
	conn := tmp.Conn(t)
	tx, err := conn.Begin()
	if err != nil {
		t.Fatalf("begin failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		t.Fatalf("coordinateWithSchemaMigration failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	granted, waiting, err := dbtest.AtomicAdvisoryLocks(context.Background(), conn, 7337741001)
	if err != nil {
		t.Fatal(err)
	}
	if granted != 1 || waiting != 0 {
		t.Fatalf("coordinateWithSchemaMigration holds %d and waits for %d locks on key 7337741001, want 1 and 0", granted, waiting)
	}
}

// AC7(a): four concurrent Open calls on one r0509 database all succeed and leave
// the catalog and the row digests (timestamp columns excluded, they are set by now())
// that one Open leaves. Without the lock, concurrent CREATE TABLE IF NOT EXISTS on the
// objects new since r0509 (finding_comments and its index) collide in the catalog.
//
// Sequences are compared separately: every Open runs upserts that advance sequences
// without adding rows (INSERT ... ON CONFLICT consumes a nextval), so four Opens
// leave the sequences higher than one. They must not be lower.
func TestUpgradeConcurrentOpenMatchesSingleOpen(t *testing.T) {
	srv := dbtest.RequireServer(t)
	single := dbtest.TempDatabase(t, srv, "conc1")
	multi := dbtest.TempDatabase(t, srv, "conc4")
	for _, tmp := range []*dbtest.Temp{single, multi} {
		tmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
		tmp.Load(t, "violation base rows", atomicFixture(t, "violation-base.sql"))
	}
	startup(t, single.DSN)

	const workers = 4
	type result struct {
		d   *DB
		err error
	}
	results := make([]result, workers)
	release := make(chan struct{})
	done := make(chan int, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			<-release
			d, err := Open(multi.DSN)
			results[i] = result{d, err}
			done <- i
		}(i)
	}
	close(release)
	timeout := time.After(3 * time.Minute)
	for i := 0; i < workers; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatalf("only %d of %d concurrent Open calls returned within 3 minutes", i, workers)
		}
	}
	for i, r := range results {
		if r.err != nil {
			t.Errorf("concurrent Open %d failed: %s", i, dbtest.AtomicErrorSummary(r.err))
			continue
		}
		defer r.d.Close()
	}
	if t.Failed() {
		return
	}
	if err := results[0].d.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("EnsureLLMRecordsTable failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if err := results[0].d.EnsureLLMUsageTable(); err != nil {
		t.Fatalf("EnsureLLMUsageTable failed (%s)", dbtest.AtomicErrorSummary(err))
	}

	ctx := context.Background()
	if diff := dbtest.DiffLines(dbtest.Lines(single.Catalog(t, single.Conn(t))), dbtest.Lines(multi.Catalog(t, multi.Conn(t)))); len(diff) > 0 {
		t.Errorf("the catalog after %d concurrent Opens differs from one Open (%d objects):\n  %s", workers, len(diff), strings.Join(diff, "\n  "))
	}
	one, err := dbtest.AtomicDataDigests(ctx, single.Conn(t))
	if err != nil {
		t.Fatal(err)
	}
	many, err := dbtest.AtomicDataDigests(ctx, multi.Conn(t))
	if err != nil {
		t.Fatal(err)
	}
	if diff := dbtest.DiffDigests(one, many); len(diff) > 0 {
		t.Errorf("the rows after %d concurrent Opens differ from one Open (%d differences):\n  %s", workers, len(diff), strings.Join(diff, "\n  "))
	}
	seqOne, err := dbtest.TakeDigests(ctx, single.Conn(t))
	if err != nil {
		t.Fatal(err)
	}
	seqMany, err := dbtest.TakeDigests(ctx, multi.Conn(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range seqOne.Sequences {
		if got, ok := seqMany.Sequences[name]; !ok || got < v {
			t.Errorf("sequence %s is %d after %d concurrent Opens, below %d after one Open", name, got, workers, v)
		}
	}
}

// AC7(b): Open waits while another transaction holds pg_advisory_xact_lock(key) and
// finishes after that transaction ends.
//
// The test does not guess with sleeps. It (1) holds the lock in an open transaction,
// (2) polls pg_locks until Open's session shows up as a waiter for the same key (up
// to a minute; on a loaded host this just takes longer), (3) confirms that Open has
// not returned, which cannot be flaky: it is the property under test, and (4) commits
// and waits, with a generous bound, for Open to return without error.
func TestUpgradeLockBlocksOpenUntilTransactionEnds(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "lockwait")
	tmp.Load(t, "r0509 schema", atomicSchema(t, "r0509"))
	holder := tmp.Conn(t)
	ctx := context.Background()

	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		t.Fatalf("take the transaction lock failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, holder, schemaMigrationLockKey); err != nil || granted != 1 || waiting != 0 {
		t.Fatalf("before Open: %d granted and %d waiting locks on the key (err %v), want 1 and 0", granted, waiting, err)
	}

	type result struct {
		d   *DB
		err error
	}
	finished := make(chan result, 1)
	collected := false // true once a result was taken from finished
	go func() {
		d, err := Open(tmp.DSN)
		finished <- result{d, err}
	}()
	t.Cleanup(func() {
		// Never leave Open's pool behind, whichever way the test ends.
		if collected {
			return
		}
		select {
		case r := <-finished:
			if r.d != nil {
				r.d.Close()
			}
		case <-time.After(30 * time.Second):
		}
	})

	deadline := time.Now().Add(time.Minute)
	for {
		_, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, holder, schemaMigrationLockKey)
		if err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		select {
		case r := <-finished:
			collected = true
			if r.d != nil {
				r.d.Close()
			}
			t.Fatalf("Open returned (error: %v) while the key was held and before it waited for it", r.err != nil)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Open did not wait for the advisory lock within a minute")
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case r := <-finished:
		collected = true
		if r.d != nil {
			r.d.Close()
		}
		t.Fatal("Open returned while another transaction held the advisory lock")
	case <-time.After(time.Second):
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	select {
	case r := <-finished:
		collected = true
		if r.err != nil {
			t.Fatalf("Open failed after the lock was released: %s", dbtest.AtomicErrorSummary(r.err))
		}
		defer r.d.Close()
	case <-time.After(2 * time.Minute):
		t.Fatal("Open did not finish within two minutes after the lock was released")
	}
	// Open released its own session lock again.
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(ctx, holder, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after Open: %d granted and %d waiting locks on the key (err %v), want 0 and 0", granted, waiting, err)
	}
	if !atomicTableExists(t, tmp, "finding_comments") {
		t.Error("Open returned without applying the schema")
	}
}

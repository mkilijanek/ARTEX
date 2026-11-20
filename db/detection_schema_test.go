package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// The constraints of migration 5003 (docs/detector-plan.md section 5), exercised with plain SQL: the composite keys,
// the automatic-attempt and active-run indexes, the tombstone check, the enumerations and the size caps. The
// database layer that uses them is a later slice; these tests state what the schema itself guarantees. Every
// test starts its own database and every case uses its own finding, so no case depends on another.

// detDB is a started database with the helpers of these tests.
type detDB struct {
	t *testing.T
	q *sql.DB
}

func newDetDB(t *testing.T, label string) detDB {
	t.Helper()
	tmp := dbtest.TempDatabase(t, dbtest.RequireServer(t), label)
	return detDB{t, startup(t, tmp.DSN).DB}
}

// id runs a statement that returns one id and fails the test when it does not.
func (x detDB) id(query string, args ...any) int64 {
	x.t.Helper()
	var id int64
	if err := x.q.QueryRow(query, args...).Scan(&id); err != nil {
		x.t.Fatalf("%s failed (%s)", firstWords(query), dbtest.AtomicErrorSummary(err))
	}
	return id
}

// exec runs a statement and returns its error.
func (x detDB) exec(query string, args ...any) error {
	x.t.Helper()
	_, err := x.q.Exec(query, args...)
	return err
}

// ok runs a statement that must succeed.
func (x detDB) ok(query string, args ...any) {
	x.t.Helper()
	if err := x.exec(query, args...); err != nil {
		x.t.Fatalf("%s failed (%s)", firstWords(query), dbtest.AtomicErrorSummary(err))
	}
}

func (x detDB) count(query string, args ...any) int {
	x.t.Helper()
	var n int
	if err := x.q.QueryRow(query, args...).Scan(&n); err != nil {
		x.t.Fatalf("%s failed (%s)", firstWords(query), dbtest.AtomicErrorSummary(err))
	}
	return n
}

func (x detDB) finding() int64 {
	x.t.Helper()
	return x.id(`INSERT INTO findings(name) VALUES ('detection schema probe') RETURNING id`)
}

// run inserts a run for finding with the given columns and values (path is always among them).
func (x detDB) run(finding int64, cols, vals string) (int64, error) {
	x.t.Helper()
	var id int64
	err := x.q.QueryRow(`INSERT INTO finding_detections(finding_id, snapshot, `+cols+`) VALUES ($1, '{}', `+vals+`) RETURNING id`, finding).Scan(&id)
	return id, err
}

// mustRun inserts a run that must be accepted.
func (x detDB) mustRun(finding int64, cols, vals string) int64 {
	x.t.Helper()
	id, err := x.run(finding, cols, vals)
	if err != nil {
		x.t.Fatalf("insert a run (%s) failed (%s)", cols, dbtest.AtomicErrorSummary(err))
	}
	return id
}

// completedRun is a finished engine run of finding.
func (x detDB) completedRun(finding int64) int64 {
	x.t.Helper()
	return x.mustRun(finding, `path, status`, `'engine', 'completed'`)
}

func firstWords(query string) string {
	f := strings.Fields(query)
	if len(f) > 4 {
		f = f[:4]
	}
	return strings.Join(f, " ")
}

// requireViolation checks that err is a PostgreSQL error with the SQLSTATE and, when constraints are given, one of
// the constraints or indexes by name (the message of the error is not printed: it may hold the rejected value).
func requireViolation(t *testing.T, what string, err error, code string, constraints ...string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if err == nil {
		t.Errorf("%s: accepted, want SQLSTATE %s %v", what, code, constraints)
		return
	}
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: %s, want SQLSTATE %s %v", what, dbtest.AtomicErrorSummary(err), code, constraints)
		return
	}
	if len(constraints) == 0 {
		return
	}
	for _, c := range constraints {
		if pgErr.ConstraintName == c {
			return
		}
	}
	t.Errorf("%s: SQLSTATE %s on constraint %q, want one of %v", what, code, pgErr.ConstraintName, constraints)
}

// Defaults, NOT NULL columns and the enumerations of every table. Each accepted value is stored, any other is
// refused by the check of its column.
func TestMigrationDetectionSchemaDefaultsAndEnumerations(t *testing.T) {
	x := newDetDB(t, "detenum")

	// A run with only the required columns holds the documented defaults.
	f := x.finding()
	run := x.mustRun(f, `path`, `'engine'`)
	var trigger, byKind, byWho, status, phase, outcome, endCode string
	var attempt, attempts, queries int
	if err := x.q.QueryRow(`SELECT trigger_kind, auto_attempt, started_by_kind, started_by_subject, status, phase, outcome, end_code, attempts, queries_used
		FROM finding_detections WHERE id = $1`, run).Scan(&trigger, &attempt, &byKind, &byWho, &status, &phase, &outcome, &endCode, &attempts, &queries); err != nil {
		t.Fatal(err)
	}
	if trigger != "manual" || attempt != 0 || byKind != "operator" || byWho != "" || status != "pending" || phase != "generate" || outcome != "" || endCode != "" || attempts != 0 || queries != 0 {
		t.Errorf("defaults: %q %d %q %q %q %q %q %q %d %d", trigger, attempt, byKind, byWho, status, phase, outcome, endCode, attempts, queries)
	}
	requireViolation(t, "a run with no snapshot", x.exec(`INSERT INTO finding_detections(finding_id, path) VALUES ($1, 'engine')`, x.finding()), "23502")
	requireViolation(t, "a run with no path", x.exec(`INSERT INTO finding_detections(finding_id, snapshot) VALUES ($1, '{}')`, x.finding()), "23502")
	requireViolation(t, "a run of a finding that does not exist", x.exec(`INSERT INTO finding_detections(finding_id, path, snapshot) VALUES (-1, 'engine', '{}')`), "23503", "finding_detections_finding_id_fkey")

	// Enumerations of finding_detections. Each value gets a finding of its own (a pending run is unique per finding).
	runEnum := func(column string, good, bad []string) {
		t.Helper()
		for _, v := range good {
			cols, vals := `path, `+column, `'engine', '`+v+`'`
			if column == "path" {
				cols, vals = `path`, `'`+v+`'`
			}
			if _, err := x.run(x.finding(), cols, vals); err != nil {
				t.Errorf("%s = %q refused (%s)", column, v, dbtest.AtomicErrorSummary(err))
			}
		}
		for _, v := range bad {
			cols, vals := `path, `+column, `'engine', '`+v+`'`
			if column == "path" {
				cols, vals = `path`, `'`+v+`'`
			}
			_, err := x.run(x.finding(), cols, vals)
			requireViolation(t, fmt.Sprintf("%s = %q", column, v), err, "23514", "finding_detections_"+column+"_check")
		}
	}
	runEnum("path", []string{"engine", "agent"}, []string{"", "other", "ENGINE"})
	runEnum("status", []string{"pending", "running", "completed", "failed", "stopped"}, []string{"", "done", "Pending"})
	runEnum("phase", []string{"generate", "validate"}, []string{"", "compile"})
	runEnum("outcome", []string{"", "validated", "partial", "telemetry_gap", "unvalidated", "no_rules"}, []string{"ok", "Validated"})
	runEnum("trigger_kind", []string{"manual"}, []string{"", "scheduled", "Auto"})
	if _, err := x.run(x.finding(), `path, trigger_kind, auto_attempt`, `'engine', 'auto', 1`); err != nil {
		t.Errorf("trigger_kind = auto with attempt 1 refused (%s)", dbtest.AtomicErrorSummary(err))
	}

	// Rules.
	rf := x.finding()
	rrun := x.completedRun(rf)
	rule := func(cols, vals string) error {
		return x.exec(`INSERT INTO detection_rules(detection_id, finding_id, body, `+cols+`) VALUES ($1, $2, 'rule text', `+vals+`)`, rrun, rf)
	}
	for _, s := range []string{"attempt", "success", "post_exploitation", "agent_behavior"} {
		if err := rule(`stage, format`, `'`+s+`', 'kql'`); err != nil {
			t.Errorf("stage %q refused (%s)", s, dbtest.AtomicErrorSummary(err))
		}
	}
	for _, s := range []string{"", "persistence"} {
		requireViolation(t, "stage "+s, rule(`stage, format`, `'`+s+`', 'kql'`), "23514", "detection_rules_stage_check")
	}
	for _, s := range []string{"sigma", "kql", "suricata", "waf_imperva", "waf_f5", "yara"} {
		if err := rule(`stage, format`, `'attempt', '`+s+`'`); err != nil {
			t.Errorf("format %q refused (%s)", s, dbtest.AtomicErrorSummary(err))
		}
	}
	for _, s := range []string{"", "snort"} {
		requireViolation(t, "format "+s, rule(`stage, format`, `'attempt', '`+s+`'`), "23514", "detection_rules_format_check")
	}
	for _, s := range []string{"unvalidated", "validated", "noisy", "no_hit", "telemetry_gap", "syntax_error", "inconclusive"} {
		if err := rule(`stage, format, validation`, `'attempt', 'kql', '`+s+`'`); err != nil {
			t.Errorf("validation %q refused (%s)", s, dbtest.AtomicErrorSummary(err))
		}
	}
	for _, s := range []string{"", "good"} {
		requireViolation(t, "validation "+s, rule(`stage, format, validation`, `'attempt', 'kql', '`+s+`'`), "23514", "detection_rules_validation_check")
	}
	requireViolation(t, "negative positive_hits", rule(`stage, format, positive_hits`, `'attempt', 'kql', -1`), "23514", "detection_rules_positive_hits_check")
	requireViolation(t, "negative baseline_hits", rule(`stage, format, baseline_hits`, `'attempt', 'kql', -1`), "23514", "detection_rules_baseline_hits_check")
	if err := rule(`stage, format, positive_hits, baseline_hits`, `'attempt', 'kql', 0, NULL`); err != nil {
		t.Errorf("zero and unknown hits refused (%s)", dbtest.AtomicErrorSummary(err))
	}

	// Steps: every step name and status is accepted, any other refused.
	seq := 0
	step := func(name, status string) error {
		seq++
		return x.exec(`INSERT INTO detection_steps(detection_id, seq, step, status) VALUES ($1, $2, $3, $4)`, rrun, seq, name, status)
	}
	for _, s := range []string{"context", "reconstruct", "synthesise", "compile", "validate", "repair", "record"} {
		if err := step(s, "ok"); err != nil {
			t.Errorf("step %q refused (%s)", s, dbtest.AtomicErrorSummary(err))
		}
	}
	for _, s := range []string{"ok", "failed", "skipped"} {
		if err := step("record", s); err != nil {
			t.Errorf("step status %q refused (%s)", s, dbtest.AtomicErrorSummary(err))
		}
	}
	requireViolation(t, "unknown step", step("plan", "ok"), "23514", "detection_steps_step_check")
	requireViolation(t, "unknown step status", step("record", "done"), "23514", "detection_steps_status_check")
	requireViolation(t, "a repeated seq", x.exec(`INSERT INTO detection_steps(detection_id, seq, step, status) VALUES ($1, 1, 'record', 'ok')`, rrun), "23505", "uq_detection_steps_seq")

	// Query results.
	n := 0
	result := func(window, state string) error {
		n++
		return x.exec(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, lpad(($2::int)::text, 64, '0'), $3, $4)`, rrun, n, window, state)
	}
	for _, w := range []string{"attack", "baseline_1d", "baseline_7d", "liveness"} {
		if err := result(w, "ok"); err != nil {
			t.Errorf("window %q refused (%s)", w, dbtest.AtomicErrorSummary(err))
		}
	}
	for _, s := range []string{"ok", "error", "rejected"} {
		if err := result("attack", s); err != nil {
			t.Errorf("state %q refused (%s)", s, dbtest.AtomicErrorSummary(err))
		}
	}
	requireViolation(t, "unknown window", result("baseline_30d", "ok"), "23514", "detection_query_results_window_kind_check")
	requireViolation(t, "unknown state", result("attack", "pending"), "23514", "detection_query_results_state_check")
	requireViolation(t, "a hash of 63 characters", x.exec(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, repeat('a', 63), 'attack', 'ok')`, rrun), "23514", "detection_query_results_body_sha256_check")
	requireViolation(t, "negative hits", x.exec(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state, hits) VALUES ($1, repeat('b', 64), 'attack', 'ok', -1)`, rrun), "23514", "detection_query_results_hits_check")
	x.ok(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, repeat('c', 64), 'attack', 'ok')`, rrun)
	requireViolation(t, "the same (run, hash, window) twice", x.exec(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, repeat('c', 64), 'attack', 'error')`, rrun), "23505", "uq_detection_query_results")
	x.ok(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, repeat('c', 64), 'baseline_1d', 'ok')`, rrun)
}

// 1a-6, 1a-7: a rule belongs to its run's finding and a parent to its run, by the schema. A rule with run A's id
// and finding B's id violates the composite key; a parent from another run violates the parent key; a sigma rule
// with a parent and a rule that is its own parent violate their checks; a kql child of a sigma rule in the same
// run is accepted and goes with its parent; the validation columns can be updated.
func TestMigrationDetectionSchemaRuleKeysAndTree(t *testing.T) {
	x := newDetDB(t, "detkeys")
	fa, fb := x.finding(), x.finding()
	runA, runB := x.completedRun(fa), x.completedRun(fb)
	insert := func(detection, finding int64, format string, parent any) (int64, error) {
		var id int64
		err := x.q.QueryRow(`INSERT INTO detection_rules(detection_id, finding_id, parent_id, stage, format, body) VALUES ($1, $2, $3, 'attempt', $4, 'rule text') RETURNING id`,
			detection, finding, parent, format).Scan(&id)
		return id, err
	}

	if _, err := insert(runA, fb, "sigma", nil); err == nil {
		t.Error("a rule with run A's id and finding B's id was accepted")
	} else {
		requireViolation(t, "a rule with run A's id and finding B's id", err, "23503", "fk_detection_rules_run")
	}
	if _, err := insert(-1, fa, "sigma", nil); err == nil {
		t.Error("a rule of a run that does not exist was accepted")
	}
	sigmaA, err := insert(runA, fa, "sigma", nil)
	if err != nil {
		t.Fatalf("a rule with its run's finding was refused (%s)", dbtest.AtomicErrorSummary(err))
	}

	_, err = insert(runB, fb, "kql", sigmaA)
	requireViolation(t, "a parent from another run", err, "23503", "fk_detection_rules_parent")
	_, err = insert(runA, fa, "sigma", sigmaA)
	requireViolation(t, "a sigma rule with a parent", err, "23514", "ck_detection_rules_sigma_root")
	err = x.exec(`INSERT INTO detection_rules(id, detection_id, finding_id, parent_id, stage, format, body) VALUES (987654, $1, $2, 987654, 'attempt', 'kql', 'rule text')`, runA, fa)
	requireViolation(t, "a rule that is its own parent", err, "23514", "ck_detection_rules_not_own_parent")

	kql, err := insert(runA, fa, "kql", sigmaA)
	if err != nil {
		t.Fatalf("a kql child of a sigma rule in the same run was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	grandchild, err := insert(runA, fa, "kql", kql)
	if err != nil {
		t.Fatalf("a child of a child was refused (%s)", dbtest.AtomicErrorSummary(err))
	}

	x.ok(`UPDATE detection_rules SET validation = 'validated', positive_hits = 3, baseline_hits = 0, validation_detail = '{"hits": 3}' WHERE id = $1`, kql)
	var validation string
	var positive, baseline int
	if err := x.q.QueryRow(`SELECT validation, positive_hits, baseline_hits FROM detection_rules WHERE id = $1`, kql).Scan(&validation, &positive, &baseline); err != nil || validation != "validated" || positive != 3 || baseline != 0 {
		t.Errorf("the validation columns after the update: %q %d %d (err %v)", validation, positive, baseline, err)
	}
	requireViolation(t, "an update to a validation that does not exist", x.exec(`UPDATE detection_rules SET validation = 'perfect' WHERE id = $1`, kql), "23514", "detection_rules_validation_check")
	requireViolation(t, "an update that moves a rule to another run", x.exec(`UPDATE detection_rules SET detection_id = $1 WHERE id = $2`, runB, kql), "23503")

	x.ok(`DELETE FROM detection_rules WHERE id = $1`, sigmaA)
	if n := x.count(`SELECT count(*) FROM detection_rules WHERE id IN ($1, $2, $3)`, sigmaA, kql, grandchild); n != 0 {
		t.Errorf("deleting the parent left %d of its descendants", n)
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE id = $1`, runA); n != 1 {
		t.Error("deleting a rule deleted its run")
	}
}

// 1a-9: the indexes that keep one active run per finding and one automatic run per attempt number. The schema allows
// both behaviours of the retry decision: attempt 2 is accepted after attempt 1 failed and refused while an
// automatic run is pending, running or completed; the attempt number exists once per finding; 11 attempts and a
// manual run with an attempt number violate their checks; the author kind 'system' is stored.
func TestMigrationDetectionSchemaActiveRunAndAutomaticAttempts(t *testing.T) {
	x := newDetDB(t, "detauto")
	const (
		active  = "idx_finding_detections_active"
		attempt = "idx_finding_detections_auto_attempt"
		done    = "idx_finding_detections_auto_done"
	)
	auto := func(finding int64, n int, status string) (int64, error) {
		return x.run(finding, `path, trigger_kind, auto_attempt, status, started_by_kind, started_by_subject`,
			fmt.Sprintf(`'engine', 'auto', %d, '%s', 'system', 'detector'`, n, status))
	}

	// One active run per finding, of either path; finished runs are unlimited.
	f := x.finding()
	first := x.mustRun(f, `path, status`, `'engine', 'pending'`)
	_, err := x.run(f, `path, status`, `'engine', 'pending'`)
	requireViolation(t, "a second pending engine run", err, "23505", active)
	_, err = x.run(f, `path, status`, `'agent', 'running'`)
	requireViolation(t, "a running agent run beside a pending engine run", err, "23505", active)
	x.ok(`UPDATE finding_detections SET status = 'running' WHERE id = $1`, first)
	_, err = x.run(f, `path, status`, `'engine', 'pending'`)
	requireViolation(t, "a pending run beside a running one", err, "23505", active)
	x.ok(`UPDATE finding_detections SET status = 'completed' WHERE id = $1`, first)
	x.mustRun(f, `path, status`, `'engine', 'completed'`)
	x.mustRun(f, `path, status`, `'engine', 'failed'`)
	x.mustRun(f, `path, status`, `'agent', 'stopped'`)
	second := x.mustRun(f, `path, status`, `'engine', 'pending'`)
	if second == 0 {
		t.Fatal("no run")
	}
	// Another finding is independent.
	x.mustRun(x.finding(), `path, status`, `'engine', 'pending'`)

	// Attempt 1 exists once per finding, whatever its status.
	f = x.finding()
	if _, err := auto(f, 1, "failed"); err != nil {
		t.Fatalf("automatic attempt 1 refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	_, err = auto(f, 1, "failed")
	requireViolation(t, "attempt 1 twice", err, "23505", attempt)
	_, err = auto(f, 1, "pending")
	requireViolation(t, "attempt 1 twice (pending)", err, "23505", attempt)
	// Attempt 2 is accepted after attempt 1 failed.
	a2, err := auto(f, 2, "pending")
	if err != nil {
		t.Fatalf("attempt 2 after attempt 1 failed was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	// ... and refused while an automatic run is active.
	_, err = auto(f, 3, "pending")
	requireViolation(t, "attempt 3 beside the pending attempt 2", err, "23505", active, done)
	x.ok(`UPDATE finding_detections SET status = 'running' WHERE id = $1`, a2)
	_, err = auto(f, 3, "pending")
	requireViolation(t, "attempt 3 beside the running attempt 2", err, "23505", active, done)
	// A manual run is refused beside it too (one active run), but an automatic finished run is not.
	_, err = x.run(f, `path`, `'engine'`)
	requireViolation(t, "a manual run beside the active automatic one", err, "23505", active)
	// ... and refused while an automatic run is completed, although nothing is active.
	x.ok(`UPDATE finding_detections SET status = 'completed' WHERE id = $1`, a2)
	_, err = auto(f, 3, "pending")
	requireViolation(t, "attempt 3 after the completed attempt 2", err, "23505", done)
	_, err = auto(f, 3, "running")
	requireViolation(t, "a running attempt 3 after the completed attempt 2", err, "23505", done)
	if _, err := auto(f, 3, "completed"); err == nil || !strings.Contains(err.Error(), done) {
		t.Errorf("a second completed automatic run was accepted or refused by another index: %v", err)
	}
	if _, err := auto(f, 3, "failed"); err != nil {
		t.Errorf("a failed automatic run beside a completed one was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id = $1 AND trigger_kind = 'auto'`, f); n != 3 {
		t.Errorf("%d automatic runs of the finding, want 3", n)
	}

	// The caps of the attempt number and its pairing with the trigger.
	f = x.finding()
	_, err = auto(f, 11, "failed")
	requireViolation(t, "attempt 11", err, "23514", "finding_detections_auto_attempt_check")
	_, err = auto(f, -1, "failed")
	requireViolation(t, "attempt -1", err, "23514", "finding_detections_auto_attempt_check", "ck_finding_detections_auto_attempt")
	if _, err := auto(f, 10, "failed"); err != nil {
		t.Errorf("attempt 10 refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	_, err = auto(f, 0, "failed")
	requireViolation(t, "an automatic run with attempt 0", err, "23514", "ck_finding_detections_auto_attempt")
	_, err = x.run(f, `path, trigger_kind, auto_attempt, status`, `'engine', 'manual', 1, 'failed'`)
	requireViolation(t, "a manual run with attempt 1", err, "23514", "ck_finding_detections_auto_attempt")
	var kind, subject string
	run, err := auto(x.finding(), 1, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.q.QueryRow(`SELECT started_by_kind, started_by_subject FROM finding_detections WHERE id = $1`, run).Scan(&kind, &subject); err != nil || kind != "system" || subject != "detector" {
		t.Errorf("author of the automatic run: %q %q (err %v), want system / detector", kind, subject, err)
	}
}

// 1a-10 (schema part): a tombstone is a finished automatic run with deleted_at; a manual run, or an active
// automatic run, with deleted_at violates the check; the tombstone keeps its attempt number and its place in the
// indexes; an engine run has no conversation; deleting the conversation of an agent run stops it with
// end_code 'conversation_deleted', keeps its rules and clears the reference.
func TestMigrationDetectionSchemaTombstoneAndConversationDelete(t *testing.T) {
	x := newDetDB(t, "dettomb")
	const tomb = "ck_finding_detections_tombstone"

	for _, tc := range []struct {
		name, cols, vals string
		ok               bool
	}{
		{"a manual completed run", `path, status`, `'engine', 'completed'`, false},
		{"a manual failed run", `path, status`, `'engine', 'failed'`, false},
		{"an active automatic run", `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 1, 'pending'`, false},
		{"a running automatic run", `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 1, 'running'`, false},
		{"a completed automatic run", `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 1, 'completed'`, true},
		{"a failed automatic run", `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 1, 'failed'`, true},
		{"a stopped automatic run", `path, trigger_kind, auto_attempt, status`, `'agent', 'auto', 1, 'stopped'`, true},
	} {
		_, err := x.run(x.finding(), tc.cols+`, deleted_at`, tc.vals+`, now()`)
		if tc.ok && err != nil {
			t.Errorf("%s with deleted_at refused (%s)", tc.name, dbtest.AtomicErrorSummary(err))
		}
		if !tc.ok {
			requireViolation(t, tc.name+" with deleted_at", err, "23514", tomb)
		}
	}
	// An update of a live run to a tombstone is checked the same way.
	f := x.finding()
	manual := x.completedRun(f)
	requireViolation(t, "marking a manual run deleted", x.exec(`UPDATE finding_detections SET deleted_at = now() WHERE id = $1`, manual), "23514", tomb)
	live := x.mustRun(f, `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 1, 'running'`)
	requireViolation(t, "marking a running automatic run deleted", x.exec(`UPDATE finding_detections SET deleted_at = now() WHERE id = $1`, live), "23514", tomb)
	x.ok(`UPDATE finding_detections SET status = 'failed' WHERE id = $1`, live)
	x.ok(`UPDATE finding_detections SET deleted_at = now(), snapshot = '{}', summary = '' WHERE id = $1`, live)

	// The tombstone still counts as attempt 1 and, when completed, blocks the next attempt.
	_, err := x.run(f, `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 1, 'failed'`)
	requireViolation(t, "attempt 1 beside its tombstone", err, "23505", "idx_finding_detections_auto_attempt")
	if _, err := x.run(f, `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 2, 'pending'`); err != nil {
		t.Errorf("attempt 2 after a failed tombstone was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	g := x.finding()
	x.mustRun(g, `path, trigger_kind, auto_attempt, status, deleted_at`, `'engine', 'auto', 1, 'completed', now()`)
	_, err = x.run(g, `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 2, 'pending'`)
	requireViolation(t, "attempt 2 beside a completed tombstone", err, "23505", "idx_finding_detections_auto_done")
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id = $1 AND trigger_kind = 'auto' AND deleted_at IS NOT NULL`, g); n != 1 {
		t.Errorf("%d tombstones of the finding, want 1", n)
	}

	// Conversations.
	conv := x.id(`INSERT INTO conversations(agent_key) VALUES ('main') RETURNING id`)
	_, err = x.run(x.finding(), `path, conversation_id`, fmt.Sprintf(`'engine', %d`, conv))
	requireViolation(t, "an engine run with a conversation", err, "23514", "ck_finding_detections_engine_conv")
	af := x.finding()
	agentRun := x.mustRun(af, `path, status, conversation_id`, fmt.Sprintf(`'agent', 'running', %d`, conv))
	_, err = x.run(x.finding(), `path, status, conversation_id`, fmt.Sprintf(`'agent', 'completed', %d`, conv))
	requireViolation(t, "two runs of one conversation", err, "23505", "finding_detections_conversation_id_key")
	x.ok(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body) VALUES ($1, $2, 'attempt', 'kql', 'kept')`, agentRun, af)

	// A finished agent run, a pending engine run and a run of another conversation are not touched by the trigger.
	otherConv := x.id(`INSERT INTO conversations(agent_key) VALUES ('main') RETURNING id`)
	doneRun := x.mustRun(x.finding(), `path, status, conversation_id`, fmt.Sprintf(`'agent', 'completed', %d`, otherConv))
	engine := x.mustRun(x.finding(), `path`, `'engine'`)

	x.ok(`DELETE FROM conversations WHERE id = $1`, conv)
	var status, endCode, errText string
	var finished, convID sql.NullString
	if err := x.q.QueryRow(`SELECT status, end_code, error, finished_at::text, conversation_id::text FROM finding_detections WHERE id = $1`, agentRun).Scan(&status, &endCode, &errText, &finished, &convID); err != nil {
		t.Fatal(err)
	}
	if status != "stopped" || endCode != "conversation_deleted" || errText != "detection session deleted" || !finished.Valid || convID.Valid {
		t.Errorf("the run after its conversation was deleted: %q %q %q finished=%v conversation=%v, want stopped / conversation_deleted / detection session deleted, finished and cleared", status, endCode, errText, finished.Valid, convID.Valid)
	}
	if n := x.count(`SELECT count(*) FROM detection_rules WHERE detection_id = $1`, agentRun); n != 1 {
		t.Errorf("the run keeps %d rules after its conversation was deleted, want 1", n)
	}
	x.ok(`DELETE FROM conversations WHERE id = $1`, otherConv)
	if err := x.q.QueryRow(`SELECT status, end_code, conversation_id::text FROM finding_detections WHERE id = $1`, doneRun).Scan(&status, &endCode, &convID); err != nil || status != "completed" || endCode != "" || convID.Valid {
		t.Errorf("a finished agent run after its conversation was deleted: %q %q conversation=%v (err %v), want completed / empty, conversation cleared", status, endCode, convID.Valid, err)
	}
	if err := x.q.QueryRow(`SELECT status FROM finding_detections WHERE id = $1`, engine).Scan(&status); err != nil || status != "pending" {
		t.Errorf("a pending engine run after a conversation was deleted: %q (err %v), want pending", status, err)
	}
}

// 1a-11 (schema part): the size caps. For the five JSON columns a payload whose jsonb::text length is exactly the cap
// is stored and one byte more violates the CHECK (jsonb::text is longer than compact JSON: a space after ':'); the
// text caps count characters or bytes as their column says, so a string of four-byte characters is judged by
// the right unit; the minimum of the rule body and the cardinality of the technique list are checked.
func TestMigrationDetectionSchemaSizeCaps(t *testing.T) {
	x := newDetDB(t, "detcaps")
	rf := x.finding()
	rrun := x.completedRun(rf)
	seq := 0

	// jsonb::text of {"a": "<n x>"} has n+9 bytes.
	jsonCases := []struct {
		column, constraint string
		cap                int
		insert             func(n int) error
	}{
		{"snapshot", "finding_detections_snapshot_check", 1048576, func(n int) error {
			return x.exec(`INSERT INTO finding_detections(finding_id, path, snapshot) VALUES ($1, 'engine', jsonb_build_object('a', repeat('x', $2::int)))`, x.finding(), n)
		}},
		{"input_manifest", "finding_detections_input_manifest_check", 65536, func(n int) error {
			return x.exec(`INSERT INTO finding_detections(finding_id, path, snapshot, input_manifest) VALUES ($1, 'engine', '{}', jsonb_build_object('a', repeat('x', $2::int)))`, x.finding(), n)
		}},
		{"stage_report", "finding_detections_stage_report_check", 16384, func(n int) error {
			return x.exec(`INSERT INTO finding_detections(finding_id, path, snapshot, stage_report) VALUES ($1, 'engine', '{}', jsonb_build_object('a', repeat('x', $2::int)))`, x.finding(), n)
		}},
		{"validation_detail", "detection_rules_validation_detail_check", 65536, func(n int) error {
			return x.exec(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body, validation_detail) VALUES ($1, $2, 'attempt', 'kql', 'b', jsonb_build_object('a', repeat('x', $3::int)))`, rrun, rf, n)
		}},
		{"detail", "detection_query_results_detail_check", 65536, func(n int) error {
			seq++
			return x.exec(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state, detail) VALUES ($1, lpad(($2::int)::text, 64, '0'), 'attack', 'ok', jsonb_build_object('a', repeat('x', $3::int)))`, rrun, seq, n)
		}},
	}
	for _, c := range jsonCases {
		atCap := c.cap - 9
		if got := x.count(`SELECT octet_length(jsonb_build_object('a', repeat('x', $1::int))::text)`, atCap); got != c.cap {
			t.Fatalf("%s: the probe payload has %d bytes as jsonb::text, want %d", c.column, got, c.cap)
		}
		if err := c.insert(atCap); err != nil {
			t.Errorf("%s: a payload of exactly %d bytes was refused (%s)", c.column, c.cap, dbtest.AtomicErrorSummary(err))
		}
		requireViolation(t, fmt.Sprintf("%s one byte over %d", c.column, c.cap), c.insert(atCap+1), "23514", c.constraint)
	}

	// Text columns: exactly the cap is stored, one more is refused. Units: characters or octets as in the schema.
	textCases := []struct {
		table, column, constraint string
		cap                       int
	}{
		{"finding_detections", "started_by_subject", "finding_detections_started_by_subject_check", 200},
		{"finding_detections", "end_code", "finding_detections_end_code_check", 64},
		{"finding_detections", "notes", "finding_detections_notes_check", 4000},
		{"finding_detections", "summary", "finding_detections_summary_check", 16384},
		{"finding_detections", "error", "finding_detections_error_check", 2000},
		{"detection_rules", "data_source", "detection_rules_data_source_check", 64},
		{"detection_rules", "technology", "detection_rules_technology_check", 128},
		{"detection_rules", "title", "detection_rules_title_check", 300},
		{"detection_rules", "coverage", "detection_rules_coverage_check", 2048},
		{"detection_rules", "body", "detection_rules_body_check", 65536},
		{"detection_steps", "code", "detection_steps_code_check", 64},
		{"detection_steps", "call_id", "detection_steps_call_id_check", 64},
	}
	insertText := func(table, column string, value any) error {
		switch table {
		case "finding_detections":
			return x.exec(`INSERT INTO finding_detections(finding_id, path, snapshot, `+column+`) VALUES ($1, 'engine', '{}', $2)`, x.finding(), value)
		case "detection_rules":
			if column == "body" {
				return x.exec(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body) VALUES ($1, $2, 'attempt', 'kql', $3)`, rrun, rf, value)
			}
			return x.exec(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body, `+column+`) VALUES ($1, $2, 'attempt', 'kql', 'b', $3)`, rrun, rf, value)
		default:
			seq++
			return x.exec(`INSERT INTO detection_steps(detection_id, seq, step, status, `+column+`) VALUES ($1, $2, 'record', 'ok', $3)`, rrun, 1000+seq, value)
		}
	}
	for _, c := range textCases {
		fill := func(n int) string { return strings.Repeat("x", n) } // ASCII: one byte and one character each
		if err := insertText(c.table, c.column, fill(c.cap)); err != nil {
			t.Errorf("%s.%s: exactly %d accepted characters refused (%s)", c.table, c.column, c.cap, dbtest.AtomicErrorSummary(err))
		}
		requireViolation(t, fmt.Sprintf("%s.%s one over %d", c.table, c.column, c.cap), insertText(c.table, c.column, fill(c.cap+1)), "23514", c.constraint)
	}
	// Four-byte characters: the character caps count characters, the byte caps count bytes.
	wide := func(n int) string { return strings.Repeat("\U0001F600", n) }
	if err := insertText("detection_rules", "title", wide(300)); err != nil {
		t.Errorf("a title of 300 four-byte characters was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireViolation(t, "a title of 301 four-byte characters", insertText("detection_rules", "title", wide(301)), "23514", "detection_rules_title_check")
	if err := insertText("detection_rules", "body", wide(16384)); err != nil {
		t.Errorf("a body of 65536 bytes in four-byte characters was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireViolation(t, "a body of 65540 bytes in four-byte characters", insertText("detection_rules", "body", wide(16385)), "23514", "detection_rules_body_check")
	if err := insertText("finding_detections", "summary", wide(4096)); err != nil {
		t.Errorf("a summary of 16384 bytes in four-byte characters was refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireViolation(t, "a summary of 16388 bytes in four-byte characters", insertText("finding_detections", "summary", wide(4097)), "23514", "finding_detections_summary_check")
	if err := insertText("finding_detections", "notes", wide(4000)); err != nil {
		t.Errorf("notes of 4000 four-byte characters were refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireViolation(t, "an empty rule body", insertText("detection_rules", "body", ""), "23514", "detection_rules_body_check")

	// Other numeric and list caps.
	requireViolation(t, "negative queries_used", x.exec(`INSERT INTO finding_detections(finding_id, path, snapshot, queries_used) VALUES ($1, 'engine', '{}', -1)`, x.finding()), "23514", "finding_detections_queries_used_check")
	if err := x.exec(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body, attack_techniques) VALUES ($1, $2, 'attempt', 'kql', 'b', ARRAY(SELECT 'T' || i FROM generate_series(1, 16) i))`, rrun, rf); err != nil {
		t.Errorf("16 techniques refused (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireViolation(t, "17 techniques", x.exec(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body, attack_techniques) VALUES ($1, $2, 'attempt', 'kql', 'b', ARRAY(SELECT 'T' || i FROM generate_series(1, 17) i))`, rrun, rf), "23514", "detection_rules_attack_techniques_check")
}

// 1a-10 (schema part): deleting a finding removes its runs (finished, active and tombstones), their rules, steps and
// query results; a deleted manual run takes its rules, steps and results with it; nothing of another finding is
// touched.
func TestMigrationDetectionSchemaCascadeFromTheFinding(t *testing.T) {
	x := newDetDB(t, "detcascade")
	populate := func(finding int64) []int64 {
		t.Helper()
		manual := x.completedRun(finding)
		tomb := x.mustRun(finding, `path, trigger_kind, auto_attempt, status, deleted_at`, `'engine', 'auto', 1, 'failed', now()`)
		live := x.mustRun(finding, `path, trigger_kind, auto_attempt, status`, `'engine', 'auto', 2, 'running'`)
		for _, run := range []int64{manual, tomb, live} {
			x.ok(`INSERT INTO detection_rules(detection_id, finding_id, stage, format, body) VALUES ($1, $2, 'attempt', 'sigma', 'rule')`, run, finding)
			x.ok(`INSERT INTO detection_steps(detection_id, seq, step, status) VALUES ($1, 1, 'record', 'ok')`, run)
			x.ok(`INSERT INTO detection_query_results(detection_id, body_sha256, window_kind, state) VALUES ($1, repeat('d', 64), 'attack', 'ok')`, run)
		}
		return []int64{manual, tomb, live}
	}
	counts := func(runs []int64) (n int) {
		for _, run := range runs {
			n += x.count(`SELECT (SELECT count(*) FROM finding_detections WHERE id = $1) + (SELECT count(*) FROM detection_rules WHERE detection_id = $1)
				+ (SELECT count(*) FROM detection_steps WHERE detection_id = $1) + (SELECT count(*) FROM detection_query_results WHERE detection_id = $1)`, run)
		}
		return n
	}
	gone, kept := x.finding(), x.finding()
	goneRuns, keptRuns := populate(gone), populate(kept)
	if n := counts(goneRuns); n != 12 {
		t.Fatalf("the seed holds %d rows for the finding to delete, want 12", n)
	}
	x.ok(`DELETE FROM findings WHERE id = $1`, gone)
	if n := counts(goneRuns); n != 0 {
		t.Errorf("deleting the finding left %d rows of its runs", n)
	}
	if n := counts(keptRuns); n != 12 {
		t.Errorf("deleting one finding changed the rows of another: %d, want 12", n)
	}

	// A manual run: its row, rules, steps and results go together.
	manual := keptRuns[0]
	x.ok(`DELETE FROM finding_detections WHERE id = $1`, manual)
	if n := counts(keptRuns[:1]); n != 0 {
		t.Errorf("deleting a manual run left %d rows", n)
	}
	if n := counts(keptRuns[1:]); n != 8 {
		t.Errorf("deleting a manual run changed the other runs: %d rows, want 8", n)
	}
}

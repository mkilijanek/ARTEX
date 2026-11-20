package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mkilijanek/artex/db/dbtest"
)

// Tests of the detection database layer (db/finding_detections.go, docs/detector-plan.md sections 6.8 and 12.1,
// criteria 1a-6 to 1a-15) and of the name check of the two older drop recipes. Every test starts its own
// database (a temporary one, started the way the product starts) and every case uses its own finding, so no test
// depends on another.

// detLayer is a started database with the helpers of these tests.
type detLayer struct {
	t   *testing.T
	d   *DB
	ctx context.Context
}

func newDetLayer(t *testing.T, label string) *detLayer {
	t.Helper()
	tmp := dbtest.TempDatabase(t, dbtest.RequireServer(t), label)
	return &detLayer{t: t, d: startup(t, tmp.DSN), ctx: context.Background()}
}

func (x *detLayer) finding() int64 {
	x.t.Helper()
	id, err := x.d.AddFinding(0, 0, "sqli", "detection layer probe", "high", "the summary", "the evidence", "worker", nil)
	if err != nil {
		x.t.Fatalf("add finding failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	return id
}

func (x *detLayer) exec(query string, args ...any) {
	x.t.Helper()
	if _, err := x.d.ExecContext(x.ctx, query, args...); err != nil {
		x.t.Fatalf("%s failed (%s)", firstWords(query), dbtest.AtomicErrorSummary(err))
	}
}

func (x *detLayer) count(query string, args ...any) int {
	x.t.Helper()
	var n int
	if err := x.d.QueryRowContext(x.ctx, query, args...).Scan(&n); err != nil {
		x.t.Fatalf("%s failed (%s)", firstWords(query), dbtest.AtomicErrorSummary(err))
	}
	return n
}

func (x *detLayer) text(query string, args ...any) string {
	x.t.Helper()
	var s string
	if err := x.d.QueryRowContext(x.ctx, query, args...).Scan(&s); err != nil {
		x.t.Fatalf("%s failed (%s)", firstWords(query), dbtest.AtomicErrorSummary(err))
	}
	return s
}

// create starts a manual run that must be created.
func (x *detLayer) create(finding int64, path string) *FindingDetection {
	x.t.Helper()
	run, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: finding, Path: path})
	if err != nil || !created {
		x.t.Fatalf("create a %s run: created=%t err=%v", path, created, err)
	}
	return run
}

// running makes a pending run running the way a claim does, for the tests that are not about the claim.
func (x *detLayer) running(id int64) {
	x.t.Helper()
	x.exec(`UPDATE finding_detections SET status='running', started_at=now(), claimed_at=now() WHERE id=$1 AND status='pending'`, id)
}

// runningRun is a new engine run that is running.
func (x *detLayer) runningRun(finding int64) int64 {
	x.t.Helper()
	run := x.create(finding, "engine")
	x.running(run.ID)
	return run.ID
}

func (x *detLayer) status(id int64) string {
	return x.text(`SELECT status FROM finding_detections WHERE id=$1`, id)
}

func sigmaRule(title string) DetectionRuleInput {
	return DetectionRuleInput{Stage: "attempt", Format: "sigma", DataSource: "http_access", Technology: "nginx",
		AttackTechniques: []string{"T1190"}, Title: title, Body: "title: " + title + "\n", Coverage: "covers the attempt"}
}

// densePayload is a JSON object of many short keys, padded so that its jsonb::text length (which the database
// measures, and which is larger than compact JSON) is exactly size bytes.
func (x *detLayer) densePayload(size int) string {
	x.t.Helper()
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, `"k%03d":%d,`, i, i)
	}
	mk := func(pad int) string { return b.String() + `"z":"` + strings.Repeat("x", pad) + `"}` }
	base := x.count(`SELECT octet_length($1::jsonb::text)`, mk(0))
	if size < base {
		x.t.Fatalf("payload of %d bytes is smaller than its %d byte skeleton", size, base)
	}
	p := mk(size - base)
	if got := x.count(`SELECT octet_length($1::jsonb::text)`, p); got != size {
		x.t.Fatalf("dense payload measures %d bytes, want %d", got, size)
	}
	return p
}

func requireLimit(t *testing.T, what string, err error, field string) {
	t.Helper()
	var le *DetectionLimitError
	if !errors.As(err, &le) || le.Field != field {
		t.Errorf("%s: err = %v, want a limit error naming %q", what, err, field)
	}
}

// ---- the name check of the older drop recipes ------------------------------------------------------------------

// The recipe for 5003 acts only on the ledger row it was written for: a 5003 recorded under another name raises,
// and nothing changes (the 0004 recipe has always checked its row's name).
func TestMigrationDetectionDropRecipeRefusesAVersionUnderAnotherName(t *testing.T) {
	tmp := dbtest.TempDatabase(t, dbtest.RequireServer(t), "detname")
	d := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d.DB))
	if _, err := d.Exec(`UPDATE schema_migrations SET name = '5003_renamed.sql' WHERE version = 5003`); err != nil {
		t.Fatal(err)
	}
	before := captureMigrationState(t, tmp)
	requireGuardRefusal(t, "5003 under another name", rollbackRunScript(t, d.DB, rollbackDropDetectionsFile), "5003 is recorded under another name")
	requireDetectionObjects(t, "after the refused recipe", d.DB)
	requireMigrationStateUnchanged(t, "after the refused recipe", before, captureMigrationState(t, tmp))
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(context.Background(), d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after the refused recipe: %d granted and %d waiting locks (err %v), want 0 and 0", granted, waiting, err)
	}

	// With the shipped name the same recipe acts.
	if _, err := d.Exec(`UPDATE schema_migrations SET name = '5003_finding_detections.sql' WHERE version = 5003`); err != nil {
		t.Fatal(err)
	}
	if err := rollbackRunScript(t, d.DB, rollbackDropDetectionsFile); err != nil {
		t.Fatalf("the recipe failed on the shipped name (%s)", dbtest.AtomicErrorSummary(err))
	}
	requireNoDetectionObjects(t, "after the recipe", d.DB)
}

// The same for the recipe of 5002, which runs once 5003 is gone.
func TestMigrationToolRagDropRecipeRefusesAVersionUnderAnotherName(t *testing.T) {
	tmp := dbtest.TempDatabase(t, dbtest.RequireServer(t), "ragname")
	d := startup(t, tmp.DSN)
	requireEmbeddedLedger(t, readLedger(t, d.DB))
	if err := rollbackRunScript(t, d.DB, rollbackDropDetectionsFile); err != nil {
		t.Fatalf("the 5003 drop failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	if _, err := d.Exec(`UPDATE schema_migrations SET name = '5002_renamed.sql' WHERE version = 5002`); err != nil {
		t.Fatal(err)
	}
	before := captureMigrationState(t, tmp)
	requireGuardRefusal(t, "5002 under another name", rollbackRunScript(t, d.DB, rollbackDropToolRagFile), "5002 is recorded under another name")
	if !migrationTableExists(t, d.DB, "tool_rag_observations") {
		t.Error("the refused recipe dropped the table")
	}
	requireMigrationStateUnchanged(t, "after the refused recipe", before, captureMigrationState(t, tmp))
	if granted, waiting, err := dbtest.AtomicAdvisoryLocks(context.Background(), d.DB, schemaMigrationLockKey); err != nil || granted != 0 || waiting != 0 {
		t.Errorf("after the refused recipe: %d granted and %d waiting locks (err %v), want 0 and 0", granted, waiting, err)
	}

	if _, err := d.Exec(`UPDATE schema_migrations SET name = '5002_tool_rag_observations.sql' WHERE version = 5002`); err != nil {
		t.Fatal(err)
	}
	if err := rollbackRunScript(t, d.DB, rollbackDropToolRagFile); err != nil {
		t.Fatalf("the recipe failed on the shipped name (%s)", dbtest.AtomicErrorSummary(err))
	}
	if migrationTableExists(t, d.DB, "tool_rag_observations") {
		t.Error("the recipe left tool_rag_observations")
	}
}

// ---- 1a-6, 1a-7: ownership and the rule tree ---------------------------------------------------------------------

// 1a-6: a rule inserted with run A's id and finding B's id violates the key; RecordDetectionRule takes the finding
// from the run, has no finding parameter, and refuses a run that is not running.
func TestFindingDetectionRuleTakesItsFindingFromTheRun(t *testing.T) {
	x := newDetLayer(t, "detown")
	fa, fb := x.finding(), x.finding()
	runA, runB := x.runningRun(fa), x.runningRun(fb)

	// The key of the schema.
	_, err := x.d.ExecContext(x.ctx, `INSERT INTO detection_rules(detection_id, finding_id, stage, format, body) VALUES ($1, $2, 'attempt', 'sigma', 'x')`, runA, fb)
	requireViolation(t, "a rule of run A with the finding of B", err, "23503", "fk_detection_rules_run")

	// The function: the finding comes from the run.
	id, err := x.d.RecordDetectionRule(x.ctx, runA, sigmaRule("owned"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := x.d.ListDetectionRules(x.ctx, runA)
	if err != nil || len(rules) != 1 || rules[0].ID != id || rules[0].FindingID != fa || rules[0].DetectionID != runA {
		t.Fatalf("rules of run A = %+v (err %v), want the one rule with finding %d", rules, err, fa)
	}
	if other, _ := x.d.ListDetectionRules(x.ctx, runB); len(other) != 0 {
		t.Errorf("run B holds %d rules, want none", len(other))
	}
	// There is no way to name a finding: not in the call, not in the input.
	if n := reflect.TypeOf(x.d.RecordDetectionRule).NumIn(); n != 3 {
		t.Errorf("RecordDetectionRule takes %d parameters, want context, run and rule", n)
	}
	ty := reflect.TypeOf(DetectionRuleInput{})
	for i := 0; i < ty.NumField(); i++ {
		if strings.Contains(strings.ToLower(ty.Field(i).Name), "finding") || strings.Contains(strings.ToLower(ty.Field(i).Name), "detection") {
			t.Errorf("DetectionRuleInput has the field %s: the run and the finding must not be input", ty.Field(i).Name)
		}
	}

	// A run that is not running takes no rule: pending, finished, missing.
	pending := x.create(x.finding(), "engine")
	for name, run := range map[string]int64{"pending": pending.ID, "missing": 1 << 40} {
		if _, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule("late")); !errors.Is(err, ErrDetectionNotRunning) {
			t.Errorf("a rule for a %s run: err = %v, want ErrDetectionNotRunning", name, err)
		}
	}
	if err := x.d.FinishFindingDetection(x.ctx, runB, DetectionFinish{Status: "completed", Outcome: "no_rules", EndCode: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.d.RecordDetectionRule(x.ctx, runB, sigmaRule("late")); !errors.Is(err, ErrDetectionNotRunning) {
		t.Errorf("a rule for a finished run: err = %v, want ErrDetectionNotRunning", err)
	}
	// An unknown stage, format or validation is refused before the database is asked.
	bad := sigmaRule("bad")
	bad.Stage = "exfiltration"
	if _, err := x.d.RecordDetectionRule(x.ctx, runA, bad); err == nil {
		t.Error("an unknown stage was accepted")
	}
}

// 1a-7: the tree of rules inside one run.
func TestFindingDetectionRuleTree(t *testing.T) {
	x := newDetLayer(t, "dettree")
	runA, runB := x.runningRun(x.finding()), x.runningRun(x.finding())
	parentA, err := x.d.RecordDetectionRule(x.ctx, runA, sigmaRule("parent"))
	if err != nil {
		t.Fatal(err)
	}

	// A kql child of a sigma rule of the same run is accepted.
	kql := DetectionRuleInput{ParentID: &parentA, Stage: "attempt", Format: "kql", Body: "Table | where x", Title: "child"}
	child, err := x.d.RecordDetectionRule(x.ctx, runA, kql)
	if err != nil {
		t.Fatalf("a kql child of a sigma rule of the same run: %v", err)
	}
	// A parent from another run violates the key.
	if _, err := x.d.RecordDetectionRule(x.ctx, runB, kql); err == nil {
		t.Error("a parent from another run was accepted")
	} else {
		requireViolation(t, "a parent from another run", errors.Unwrap(err), "23503", "fk_detection_rules_parent")
	}
	// A sigma rule with a parent: refused by the function, and by the check when written directly.
	sigmaChild := sigmaRule("sigma child")
	sigmaChild.ParentID = &parentA
	if _, err := x.d.RecordDetectionRule(x.ctx, runA, sigmaChild); err == nil {
		t.Error("a sigma rule with a parent was accepted")
	}
	_, err = x.d.ExecContext(x.ctx, `INSERT INTO detection_rules(detection_id, finding_id, parent_id, stage, format, body)
		SELECT detection_id, finding_id, id, 'attempt', 'sigma', 'x' FROM detection_rules WHERE id=$1`, parentA)
	requireViolation(t, "a sigma rule with a parent (direct)", err, "23514", "ck_detection_rules_sigma_root")
	// A rule that is its own parent.
	_, err = x.d.ExecContext(x.ctx, `UPDATE detection_rules SET parent_id=id WHERE id=$1`, child)
	requireViolation(t, "a rule that is its own parent", err, "23514", "ck_detection_rules_not_own_parent")

	// The validation columns can be updated on an existing rule, and are read back.
	x.exec(`UPDATE detection_rules SET validation='validated', positive_hits=3, baseline_hits=0, validation_detail='{"windows":2}' WHERE id=$1`, parentA)
	rules, err := x.d.ListDetectionRules(x.ctx, runA)
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules = %d (err %v), want 2", len(rules), err)
	}
	p := rules[0]
	if p.ID != parentA || p.Validation != "validated" || p.PositiveHits == nil || *p.PositiveHits != 3 || p.BaselineHits == nil || *p.BaselineHits != 0 ||
		string(p.ValidationDetail) != `{"windows": 2}` || len(p.AttackTechniques) != 1 || p.AttackTechniques[0] != "T1190" {
		t.Errorf("the updated parent reads back as %+v", p)
	}
	if rules[1].ParentID == nil || *rules[1].ParentID != parentA {
		t.Errorf("the child has parent %v, want %d", rules[1].ParentID, parentA)
	}
	// Deleting the parent removes the child.
	x.exec(`DELETE FROM detection_rules WHERE id=$1`, parentA)
	if n := x.count(`SELECT count(*) FROM detection_rules WHERE detection_id=$1`, runA); n != 0 {
		t.Errorf("after deleting the parent %d rules of the run remain, want 0", n)
	}
}

// ---- 1a-8: create, both paths -----------------------------------------------------------------------------------

func TestFindingDetectionCreateReturnsTheActiveRunOfEitherPath(t *testing.T) {
	x := newDetLayer(t, "detcreate")
	f := x.finding()
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: 1 << 40, Path: "engine"}); !errors.Is(err, ErrFindingNotFound) {
		t.Errorf("create for a missing finding: err = %v, want ErrFindingNotFound", err)
	}
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "cloud"}); err == nil {
		t.Error("an unknown path was accepted")
	}

	first, conv, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", Notes: "  look at the login  "})
	if err != nil || !created || conv != nil {
		t.Fatalf("first create: created=%t conv=%v err=%v", created, conv, err)
	}
	if first.Status != "pending" || first.Path != "engine" || first.TriggerKind != "manual" || first.StartedByKind != "operator" || first.Notes != "look at the login" || first.ConversationID != nil {
		t.Errorf("the new run is %+v", first)
	}
	for _, path := range []string{"engine", "agent"} {
		again, c, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: path})
		if err != nil || created || c != nil || again.ID != first.ID {
			t.Errorf("a %s create with an active run: id=%v created=%t conv=%v err=%v, want the first run unchanged", path, again, created, c, err)
		}
	}
	if n := x.count(`SELECT count(*) FROM conversations WHERE agent_key=$1`, FindingDetectionAgentKey); n != 0 {
		t.Errorf("the refused agent create left %d conversations", n)
	}
	// A running run is active as well.
	x.running(first.ID)
	if again, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine"}); err != nil || created || again.ID != first.ID {
		t.Errorf("create with a running run: created=%t err=%v", created, err)
	}
	if stopped, err := x.d.StopFindingDetection(x.ctx, first.ID); err != nil || !stopped {
		t.Fatalf("stop: %t %v", stopped, err)
	}

	// The agent path creates the conversation, the run and the first message together.
	run, conv, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "agent", Notes: "focus on auth"})
	if err != nil || !created || conv == nil || run.ConversationID == nil || *run.ConversationID != conv.ID {
		t.Fatalf("agent create: run=%+v conv=%+v created=%t err=%v", run, conv, created, err)
	}
	if conv.AgentKey != FindingDetectionAgentKey || !strings.HasPrefix(conv.Title, fmt.Sprintf("Detection #%d", f)) {
		t.Errorf("the conversation is %+v", conv)
	}
	if n := x.count(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind='user' AND detail LIKE '%focus on auth%'`, conv.ID); n != 1 {
		t.Errorf("the conversation holds %d first messages with the notes, want 1", n)
	}
}

// 1a-8: eight concurrent creates, of both paths, make one row; when the agent path won, one conversation exists.
func TestFindingDetectionConcurrentCreatesMakeOneRow(t *testing.T) {
	x := newDetLayer(t, "detconc")
	f := x.finding()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ids []int64
	createdBy := []string{}
	for i := range 8 {
		path := "engine"
		if i%2 == 1 {
			path = "agent"
		}
		wg.Go(func() {
			run, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: path})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids = append(ids, run.ID)
			if created {
				createdBy = append(createdBy, path)
			}
		})
	}
	wg.Wait()
	if len(ids) != 8 || len(createdBy) != 1 {
		t.Fatalf("ids=%v created by %v, want 8 ids and one creator", ids, createdBy)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different runs returned: %v", ids)
		}
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id=$1`, f); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	wantConvs := 0
	if createdBy[0] == "agent" {
		wantConvs = 1
	}
	if n := x.count(`SELECT count(*) FROM conversations WHERE agent_key=$1`, FindingDetectionAgentKey); n != wantConvs {
		t.Errorf("%d conversations after an %s create, want %d", n, createdBy[0], wantConvs)
	}
}

// An agent-path create that fails after the conversation was inserted leaves no conversation behind.
func TestFindingDetectionAgentCreateRollsBackTheConversation(t *testing.T) {
	x := newDetLayer(t, "detroll")
	f := x.finding()
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: 1}); err != nil {
		t.Fatal(err)
	}
	x.exec(`UPDATE finding_detections SET status='completed', finished_at=now() WHERE finding_id=$1`, f)
	_, conv, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "agent", TriggerKind: "auto", AutoAttempt: 2})
	if !errors.Is(err, ErrDetectionAutoRunExists) || created || conv != nil {
		t.Fatalf("create: created=%t conv=%v err=%v, want ErrDetectionAutoRunExists", created, conv, err)
	}
	if n := x.count(`SELECT count(*) FROM conversations WHERE agent_key=$1`, FindingDetectionAgentKey); n != 0 {
		t.Errorf("%d conversations left after the refused create, want 0", n)
	}
	if n := x.count(`SELECT count(*) FROM conversation_activities`); n != 0 {
		t.Errorf("%d messages left after the refused create, want 0", n)
	}
}

// ---- 1a-9: automatic runs ---------------------------------------------------------------------------------------

func TestFindingDetectionAutomaticAttempts(t *testing.T) {
	x := newDetLayer(t, "detauto")
	f := x.finding()
	auto := func(attempt int) (*FindingDetection, bool, error) {
		run, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{
			FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: attempt, StartedByKind: "system", StartedBySubject: "watermark"})
		return run, created, err
	}

	// Two concurrent creates of attempt 1 make one row.
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range 2 {
		wg.Go(func() {
			run, c, err := auto(1)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if c {
				created++
			}
			if run.AutoAttempt != 1 || run.TriggerKind != "auto" || run.StartedByKind != "system" || run.StartedBySubject != "watermark" {
				t.Errorf("the automatic run is %+v", run)
			}
		})
	}
	wg.Wait()
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id=$1`, f); n != 1 || created != 1 {
		t.Fatalf("%d rows and %d creators for two concurrent creates of attempt 1, want 1 and 1", n, created)
	}
	run1 := x.latestRun(f)

	// Attempt 1 failed: attempt 2 is accepted; attempt 1 again is refused (its number exists).
	x.running(run1)
	if err := x.d.FinishFindingDetection(x.ctx, run1, DetectionFinish{Status: "failed", EndCode: "provider_error", Error: "The model provider failed."}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auto(1); !errors.Is(err, ErrDetectionAutoRunExists) {
		t.Errorf("attempt 1 again: err = %v, want ErrDetectionAutoRunExists", err)
	}
	run2, c, err := auto(2)
	if err != nil || !c {
		t.Fatalf("attempt 2 after a failed attempt 1: created=%t err=%v", c, err)
	}
	// While it is active the index refuses a third active or completed automatic run (the function returns the
	// active run instead, so the index is asked directly).
	_, err = x.d.ExecContext(x.ctx, `INSERT INTO finding_detections(finding_id, path, trigger_kind, auto_attempt, status, snapshot) VALUES ($1,'engine','auto',3,'completed','{}')`, f)
	requireViolation(t, "attempt 3 while attempt 2 is active", err, "23505", "idx_finding_detections_auto_done")
	// Completed: refused as well, through the function.
	x.running(run2.ID)
	if err := x.d.FinishFindingDetection(x.ctx, run2.ID, DetectionFinish{Status: "completed", Outcome: "no_rules"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auto(3); !errors.Is(err, ErrDetectionAutoRunExists) {
		t.Errorf("attempt 3 after a completed attempt: err = %v, want ErrDetectionAutoRunExists", err)
	}

	// Out of range or inconsistent: refused by the function, and by the checks when written directly.
	for _, attempt := range []int{0, 11, -1} {
		if _, _, err := auto(attempt); err == nil {
			t.Errorf("automatic attempt %d was accepted", attempt)
		}
	}
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", AutoAttempt: 1}); err == nil {
		t.Error("a manual run with attempt 1 was accepted")
	}
	f2 := x.finding()
	_, err = x.d.ExecContext(x.ctx, `INSERT INTO finding_detections(finding_id, path, trigger_kind, auto_attempt, snapshot) VALUES ($1,'engine','auto',11,'{}')`, f2)
	requireViolation(t, "auto_attempt 11", err, "23514")
	_, err = x.d.ExecContext(x.ctx, `INSERT INTO finding_detections(finding_id, path, trigger_kind, auto_attempt, snapshot) VALUES ($1,'engine','manual',1,'{}')`, f2)
	requireViolation(t, "a manual run with attempt 1", err, "23514", "ck_finding_detections_auto_attempt")
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- 1a-10: delete ----------------------------------------------------------------------------------------------

// Deleting a finding, or its task's findings, removes runs, rules, steps, query results and tombstones; deleting a
// task whose findings have runs succeeds.
func TestFindingDetectionDeletesWithTheFindingAndTheTask(t *testing.T) {
	x := newDetLayer(t, "detcascade")
	build := func(f int64) {
		// A finished manual run with a rule, a step and a query result, and a tombstone.
		run := x.runningRun(f)
		if _, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule("cascade")); err != nil {
			t.Fatal(err)
		}
		if err := x.d.AppendDetectionStep(x.ctx, run, DetectionStep{Step: "context", Status: "ok"}); err != nil {
			t.Fatal(err)
		}
		if err := x.d.RecordDetectionQueryResult(x.ctx, run, DetectionQueryResult{BodySHA256: strings.Repeat("a", 64), WindowKind: "attack", State: "ok"}); err != nil {
			t.Fatal(err)
		}
		if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "unvalidated"}); err != nil {
			t.Fatal(err)
		}
		auto, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: 1})
		if err != nil {
			t.Fatal(err)
		}
		x.exec(`UPDATE finding_detections SET status='failed', finished_at=now() WHERE id=$1`, auto.ID)
		if tomb, err := x.d.DeleteFindingDetection(x.ctx, auto.ID); err != nil || !tomb {
			t.Fatalf("tombstone: %t %v", tomb, err)
		}
	}
	rows := func() [4]int {
		return [4]int{x.count(`SELECT count(*) FROM finding_detections`), x.count(`SELECT count(*) FROM detection_rules`),
			x.count(`SELECT count(*) FROM detection_steps`), x.count(`SELECT count(*) FROM detection_query_results`)}
	}

	f1 := x.finding()
	build(f1)
	if got := rows(); got != [4]int{2, 1, 1, 1} {
		t.Fatalf("rows (runs, rules, steps, results) = %v, want [2 1 1 1]", got)
	}
	if n, err := x.d.DeleteFinding(f1); err != nil || n != 1 {
		t.Fatalf("delete finding: %d %v", n, err)
	}
	if got := rows(); got != [4]int{} {
		t.Errorf("after deleting the finding: %v, want all gone (the tombstone included)", got)
	}

	// The task's findings, and the task itself.
	task, err := x.d.CreateTask("task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f2 := x.finding()
	x.exec(`UPDATE findings SET task_id=$1 WHERE id=$2`, task.ID, f2)
	build(f2)
	if n, err := x.d.DeleteFindingsByTask(task.ID); err != nil || n != 1 {
		t.Fatalf("delete the findings of the task: %d %v", n, err)
	}
	if got := rows(); got != [4]int{} {
		t.Errorf("after deleting the task's findings: %v, want all gone", got)
	}

	f3 := x.finding()
	x.exec(`UPDATE findings SET task_id=$1 WHERE id=$2`, task.ID, f3)
	build(f3)
	if err := x.d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if got := rows(); got != [4]int{2, 1, 1, 1} {
		t.Errorf("after deleting the task: %v, want the runs of the finding kept", got)
	}
	if x.count(`SELECT count(*) FROM findings WHERE id=$1 AND task_id IS NULL`, f3) != 1 {
		t.Error("the finding of the deleted task lost its row")
	}
	// A finding whose task is gone can still be run.
	x.create(f3, "engine")
}

func (x *detLayer) latestRun(finding int64) int64 {
	x.t.Helper()
	return mustInt64(x.t, x.text(`SELECT max(id)::text FROM finding_detections WHERE finding_id=$1 AND deleted_at IS NULL`, finding))
}

func TestFindingDetectionDeleteARun(t *testing.T) {
	x := newDetLayer(t, "detdel")
	f := x.finding()

	// A finished manual run: the row goes with its rules, steps and results.
	manual := x.runningRun(f)
	if _, err := x.d.RecordDetectionRule(x.ctx, manual, sigmaRule("gone")); err != nil {
		t.Fatal(err)
	}
	// Active: refused, and nothing is deleted.
	if _, err := x.d.DeleteFindingDetection(x.ctx, manual); !errors.Is(err, ErrDetectionActive) {
		t.Errorf("delete of a running run: err = %v, want ErrDetectionActive", err)
	}
	if err := x.d.FinishFindingDetection(x.ctx, manual, DetectionFinish{Status: "failed", EndCode: "provider_error"}); err != nil {
		t.Fatal(err)
	}
	if tomb, err := x.d.DeleteFindingDetection(x.ctx, manual); err != nil || tomb {
		t.Fatalf("delete of a finished manual run: tombstone=%t err=%v", tomb, err)
	}
	if x.count(`SELECT count(*) FROM finding_detections WHERE id=$1`, manual) != 0 || x.count(`SELECT count(*) FROM detection_rules WHERE detection_id=$1`, manual) != 0 {
		t.Error("the manual run or its rules remain")
	}
	if _, err := x.d.DeleteFindingDetection(x.ctx, manual); !errors.Is(err, ErrDetectionNotFound) {
		t.Errorf("a second delete: err = %v, want ErrDetectionNotFound", err)
	}
	pending := x.create(f, "engine")
	if _, err := x.d.DeleteFindingDetection(x.ctx, pending.ID); !errors.Is(err, ErrDetectionActive) {
		t.Errorf("delete of a pending run: err = %v, want ErrDetectionActive", err)
	}
	if _, err := x.d.StopFindingDetection(x.ctx, pending.ID); err != nil {
		t.Fatal(err)
	}

	// A completed automatic run: its content goes, its row stays as a tombstone that still holds its attempt.
	auto, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: 1, Notes: "private notes"})
	if err != nil {
		t.Fatal(err)
	}
	x.running(auto.ID)
	if _, err := x.d.RecordDetectionRule(x.ctx, auto.ID, sigmaRule("content")); err != nil {
		t.Fatal(err)
	}
	if err := x.d.AppendDetectionStep(x.ctx, auto.ID, DetectionStep{Step: "record", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := x.d.RecordDetectionQueryResult(x.ctx, auto.ID, DetectionQueryResult{BodySHA256: strings.Repeat("b", 64), WindowKind: "attack", State: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := x.d.FinishFindingDetection(x.ctx, auto.ID, DetectionFinish{Status: "completed", Outcome: "unvalidated", Summary: "private summary", StageReport: json.RawMessage(`{"attempt":"rule"}`)}); err != nil {
		t.Fatal(err)
	}
	// The history lists it, until it is deleted.
	if list, _ := x.d.ListFindingDetections(x.ctx, f); len(list) != 2 {
		t.Fatalf("history before the delete holds %d runs, want 2 (the stopped one and the automatic one)", len(list))
	}
	if tomb, err := x.d.DeleteFindingDetection(x.ctx, auto.ID); err != nil || !tomb {
		t.Fatalf("delete of an automatic run: tombstone=%t err=%v", tomb, err)
	}
	var snapshot, manifest, report, summary, notes, errText, status string
	var attempt int
	var deletedAt *time.Time
	if err := x.d.QueryRow(`SELECT snapshot::text, input_manifest::text, stage_report::text, summary, notes, error, status, auto_attempt, deleted_at FROM finding_detections WHERE id=$1`, auto.ID).
		Scan(&snapshot, &manifest, &report, &summary, &notes, &errText, &status, &attempt, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if snapshot != "{}" || manifest != "{}" || report != "{}" || summary != "" || notes != "" || errText != "" || deletedAt == nil || status != "completed" || attempt != 1 {
		t.Errorf("tombstone: snapshot=%q manifest=%q report=%q summary=%q notes=%q error=%q status=%s attempt=%d deleted_at=%v", snapshot, manifest, report, summary, notes, errText, status, attempt, deletedAt)
	}
	for _, table := range []string{"detection_rules", "detection_steps", "detection_query_results"} {
		if n := x.count(`SELECT count(*) FROM `+table+` WHERE detection_id=$1`, auto.ID); n != 0 {
			t.Errorf("%s holds %d rows of the tombstone, want 0", table, n)
		}
	}
	// It still counts: its attempt number is taken, and as a completed run it blocks a later automatic run.
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: 1}); !errors.Is(err, ErrDetectionAutoRunExists) {
		t.Errorf("attempt 1 after its run was deleted: err = %v, want ErrDetectionAutoRunExists", err)
	}
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: 2}); !errors.Is(err, ErrDetectionAutoRunExists) {
		t.Errorf("attempt 2 after a deleted completed run: err = %v, want ErrDetectionAutoRunExists (a deleted run is never retried)", err)
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id=$1 AND trigger_kind='auto'`, f); n != 1 {
		t.Errorf("automatic attempts counted over all rows: %d, want 1 (the tombstone)", n)
	}
	// The lists and the single run do not return it; deleting it again is not found.
	if list, _ := x.d.ListFindingDetections(x.ctx, f); len(list) != 1 || list[0].ID != pending.ID {
		t.Errorf("history after the delete: %d runs, want only the stopped one", len(list))
	}
	if got, err := x.d.GetFindingDetection(x.ctx, auto.ID); err != nil || got != nil {
		t.Errorf("get of a tombstone: %v, %v, want nil", got, err)
	}
	if _, err := x.d.DeleteFindingDetection(x.ctx, auto.ID); !errors.Is(err, ErrDetectionNotFound) {
		t.Errorf("delete of a tombstone: err = %v, want ErrDetectionNotFound", err)
	}
	if _, err := x.d.FindingDetectionSnapshot(x.ctx, auto.ID); !errors.Is(err, ErrDetectionNotFound) {
		t.Errorf("snapshot of a tombstone: err = %v, want ErrDetectionNotFound", err)
	}

	// deleted_at on a manual run or on an active run violates the check.
	_, err = x.d.ExecContext(x.ctx, `UPDATE finding_detections SET deleted_at=now() WHERE id=$1`, pending.ID)
	requireViolation(t, "deleted_at on a manual run", err, "23514", "ck_finding_detections_tombstone")
	f2 := x.finding()
	active, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f2, Path: "engine", TriggerKind: "auto", AutoAttempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.d.ExecContext(x.ctx, `UPDATE finding_detections SET deleted_at=now() WHERE id=$1`, active.ID)
	requireViolation(t, "deleted_at on an active run", err, "23514", "ck_finding_detections_tombstone")
}

// Deleting the conversation of an agent-path run stops it, keeps its rules and clears the conversation.
func TestFindingDetectionConversationDeleteStopsTheAgentRun(t *testing.T) {
	x := newDetLayer(t, "detconv")
	f := x.finding()
	run, conv, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "agent"})
	if err != nil || conv == nil {
		t.Fatalf("agent create: %v", err)
	}
	x.running(run.ID)
	if _, err := x.d.RecordDetectionRule(x.ctx, run.ID, sigmaRule("kept")); err != nil {
		t.Fatal(err)
	}
	if err := x.d.DeleteConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := x.d.GetFindingDetection(x.ctx, run.ID)
	if err != nil || got == nil || got.Status != "stopped" || got.EndCode != "conversation_deleted" || got.ConversationID != nil || got.FinishedAt == nil {
		t.Fatalf("run after the conversation was deleted: %+v (err %v)", got, err)
	}
	if rules, _ := x.d.ListDetectionRules(x.ctx, run.ID); len(rules) != 1 {
		t.Errorf("%d rules after the delete, want the rule kept", len(rules))
	}
	// The finding can be run again.
	if _, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "agent"}); err != nil || !created {
		t.Errorf("a new run after the session was deleted: created=%t err=%v", created, err)
	}
}

// ---- 1a-11: limits ------------------------------------------------------------------------------------------------

// The JSON columns: a dense-key payload whose jsonb::text length is exactly the cap is stored, one byte more is
// refused by the function with a message that names the field, and a direct write over the cap violates the check.
func TestFindingDetectionJSONSizeLimits(t *testing.T) {
	x := newDetLayer(t, "detjson")
	f := x.finding()
	run := x.runningRun(f)
	parent, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule("detail"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("c", 64)

	cases := []struct {
		field  string
		cap    int
		store  func(payload string) error
		direct string // the same column written directly
	}{
		{"input_manifest", 65536, func(p string) error { return x.d.SetDetectionManifest(x.ctx, run, json.RawMessage(p)) },
			`UPDATE finding_detections SET input_manifest=$1::jsonb WHERE id=` + fmt.Sprint(run)},
		{"validation_detail", 65536, func(p string) error {
			in := sigmaRule("detail")
			in.ValidationDetail = json.RawMessage(p)
			_, err := x.d.RecordDetectionRule(x.ctx, run, in)
			return err
		}, `UPDATE detection_rules SET validation_detail=$1::jsonb WHERE id=` + fmt.Sprint(parent)},
		{"query result detail", 65536, func(p string) error {
			return x.d.RecordDetectionQueryResult(x.ctx, run, DetectionQueryResult{BodySHA256: sha, WindowKind: "attack", State: "ok", Detail: json.RawMessage(p)})
		}, `UPDATE detection_query_results SET detail=$1::jsonb WHERE detection_id=` + fmt.Sprint(run)},
	}
	for _, tc := range cases {
		if err := tc.store(x.densePayload(tc.cap)); err != nil {
			t.Errorf("%s at the cap of %d bytes: %v", tc.field, tc.cap, err)
		}
		requireLimit(t, tc.field+" one byte over the cap", tc.store(x.densePayload(tc.cap+1)), tc.field)
		_, err := x.d.ExecContext(x.ctx, tc.direct, x.densePayload(tc.cap+1))
		requireViolation(t, tc.field+" written directly over the cap", err, "23514")
	}
	// The stored manifest and result are the ones at the cap.
	if n := x.count(`SELECT octet_length(input_manifest::text) FROM finding_detections WHERE id=$1`, run); n != 65536 {
		t.Errorf("manifest stores %d bytes, want 65536", n)
	}
	if n := x.count(`SELECT octet_length(detail::text) FROM detection_query_results WHERE detection_id=$1`, run); n != 65536 {
		t.Errorf("query result detail stores %d bytes, want 65536", n)
	}
	// The stage report is written by the finish, which ends the run only when it succeeds.
	requireLimit(t, "stage_report one byte over the cap", x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "failed", EndCode: "x", StageReport: json.RawMessage(x.densePayload(16385))}), "stage_report")
	if x.status(run) != "running" {
		t.Error("a refused finish ended the run")
	}
	_, err = x.d.ExecContext(x.ctx, `UPDATE finding_detections SET stage_report=$1::jsonb WHERE id=$2`, x.densePayload(16385), run)
	requireViolation(t, "stage_report written directly over the cap", err, "23514")
	if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "unvalidated", StageReport: json.RawMessage(x.densePayload(16384))}); err != nil {
		t.Errorf("stage_report at the cap: %v", err)
	}
	if n := x.count(`SELECT octet_length(stage_report::text) FROM finding_detections WHERE id=$1`, run); n != 16384 {
		t.Errorf("stage report stores %d bytes, want 16384", n)
	}

	// The snapshot: the insert measures it in its own statement; exactly 1 MiB is stored, one byte more refused.
	tx, err := x.d.BeginTx(x.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	row := detectionRunRow{FindingID: f, Path: "engine", Trigger: "manual", StartedByKind: "operator"}
	if _, err := insertDetectionRun(x.ctx, tx, row, []byte(x.densePayload(detectionSnapshotCap))); err != nil {
		t.Errorf("snapshot at the cap: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	tx2, err := x.d.BeginTx(x.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	_, err = insertDetectionRun(x.ctx, tx2, row, []byte(x.densePayload(detectionSnapshotCap+1)))
	requireLimit(t, "snapshot one byte over the cap", err, "snapshot")
	_, err = x.d.ExecContext(x.ctx, `UPDATE finding_detections SET snapshot=$1::jsonb WHERE id=$2`, x.densePayload(detectionSnapshotCap+1), run)
	requireViolation(t, "snapshot written directly over the cap", err, "23514")
}

// Text caps, the 25th rule, the 101st run, and every step name and status.
func TestFindingDetectionTextRuleRunAndStepLimits(t *testing.T) {
	x := newDetLayer(t, "detlimits")
	f := x.finding()

	// Notes: 4000 characters are accepted, 4001 refused (characters, not bytes).
	_, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", Notes: strings.Repeat("é", 4001)})
	requireLimit(t, "notes of 4001 characters", err, "notes")
	_, _, _, err = x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", Notes: strings.Repeat("😀", 4000)})
	if err != nil {
		t.Fatalf("notes of 4000 four-byte characters: %v", err)
	}
	run := x.latestRun(f)
	x.running(run)

	// Rule text: body in bytes (1 to 65536), title in characters (300).
	body := func(n int) DetectionRuleInput { r := sigmaRule("t"); r.Body = strings.Repeat("b", n); return r }
	if _, err := x.d.RecordDetectionRule(x.ctx, run, body(65536)); err != nil {
		t.Errorf("body of 65536 bytes: %v", err)
	}
	_, err = x.d.RecordDetectionRule(x.ctx, run, body(65537))
	requireLimit(t, "body of 65537 bytes", err, "body")
	if _, err := x.d.RecordDetectionRule(x.ctx, run, body(0)); err == nil {
		t.Error("an empty body was accepted")
	}
	four := sigmaRule(strings.Repeat("😀", 300))
	if _, err := x.d.RecordDetectionRule(x.ctx, run, four); err != nil {
		t.Errorf("a title of 300 four-byte characters: %v", err)
	}
	_, err = x.d.RecordDetectionRule(x.ctx, run, sigmaRule(strings.Repeat("😀", 301)))
	requireLimit(t, "a title of 301 characters", err, "title")
	long := sigmaRule("t")
	long.Coverage = strings.Repeat("c", 2049)
	_, err = x.d.RecordDetectionRule(x.ctx, run, long)
	requireLimit(t, "coverage of 2049 bytes", err, "coverage")
	many := sigmaRule("t")
	many.AttackTechniques = make([]string, 17)
	_, err = x.d.RecordDetectionRule(x.ctx, run, many)
	requireLimit(t, "17 techniques", err, "attack_techniques")

	// The run text: summary 16384 bytes, error 2000 bytes, end code 64 characters.
	for _, tc := range []struct {
		field string
		f     DetectionFinish
	}{
		{"summary", DetectionFinish{Status: "completed", Summary: strings.Repeat("s", 16385)}},
		{"error", DetectionFinish{Status: "failed", Error: strings.Repeat("e", 2001)}},
		{"end_code", DetectionFinish{Status: "failed", EndCode: strings.Repeat("c", 65)}},
	} {
		requireLimit(t, tc.field+" over its cap", x.d.FinishFindingDetection(x.ctx, run, tc.f), tc.field)
	}
	if x.status(run) != "running" {
		t.Error("a refused finish changed the run")
	}

	// The 25th rule is refused, and a finish with rules over the limit stores nothing.
	have := x.count(`SELECT count(*) FROM detection_rules WHERE detection_id=$1`, run)
	for i := have; i < MaxDetectionRulesPerRun; i++ {
		if _, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule(fmt.Sprint("r", i))); err != nil {
			t.Fatalf("rule %d: %v", i+1, err)
		}
	}
	if _, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule("one too many")); !errors.Is(err, ErrDetectionRuleLimit) {
		t.Errorf("the 25th rule: err = %v, want ErrDetectionRuleLimit", err)
	}
	if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "unvalidated", Rules: []DetectionRuleInput{sigmaRule("extra")}}); !errors.Is(err, ErrDetectionRuleLimit) {
		t.Errorf("a finish with a 25th rule: err = %v, want ErrDetectionRuleLimit", err)
	}
	if n := x.count(`SELECT count(*) FROM detection_rules WHERE detection_id=$1`, run); n != MaxDetectionRulesPerRun || x.status(run) != "running" {
		t.Errorf("after the refused finish: %d rules, status %s, want 24 and running", n, x.status(run))
	}

	// Every step name and status is accepted, any other refused (the database check agrees).
	var seqWant int
	for _, step := range []string{"context", "reconstruct", "synthesise", "compile", "validate", "repair", "record"} {
		for _, status := range []string{"ok", "failed", "skipped"} {
			if err := x.d.AppendDetectionStep(x.ctx, run, DetectionStep{Step: step, Status: status, Code: "c", CallID: "call", InputBytes: 10, OutputBytes: 20, DurationMS: 5}); err != nil {
				t.Errorf("step %s/%s: %v", step, status, err)
			}
			seqWant++
		}
	}
	for _, bad := range []DetectionStep{{Step: "plan", Status: "ok"}, {Step: "context", Status: "done"}, {Step: "context", Status: "ok", Code: strings.Repeat("c", 65)}} {
		if err := x.d.AppendDetectionStep(x.ctx, run, bad); err == nil {
			t.Errorf("step %+v was accepted", bad)
		}
	}
	_, err = x.d.ExecContext(x.ctx, `INSERT INTO detection_steps(detection_id, seq, step, status) VALUES ($1, 999, 'plan', 'ok')`, run)
	requireViolation(t, "a step name written directly", err, "23514")
	steps, err := x.d.ListDetectionSteps(x.ctx, run)
	if err != nil || len(steps) != seqWant {
		t.Fatalf("steps = %d (err %v), want %d", len(steps), err, seqWant)
	}
	for i, s := range steps {
		if s.Seq != i+1 {
			t.Errorf("step %d has seq %d", i, s.Seq)
		}
	}

	// The 101st run: 100 runs are the limit, a tombstone is not counted.
	f2 := x.finding()
	x.exec(`INSERT INTO finding_detections(finding_id, path, status, snapshot) SELECT $1::bigint, 'engine', 'completed', '{}'::jsonb FROM generate_series(1, 95)`, f2)
	x.exec(`INSERT INTO finding_detections(finding_id, path, trigger_kind, auto_attempt, status, snapshot, deleted_at)
		SELECT $1::bigint, 'engine', 'auto', g, 'failed', '{}'::jsonb, now() FROM generate_series(1, 5) g`, f2)
	for i := 95; i < MaxDetectionRunsPerFinding; i++ {
		if _, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f2, Path: "engine"}); err != nil || !created {
			t.Fatalf("run %d: created=%t err=%v (95 runs and 5 tombstones before it)", i+1, created, err)
		}
		x.exec(`UPDATE finding_detections SET status='failed', finished_at=now() WHERE finding_id=$1 AND status='pending'`, f2)
	}
	if _, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f2, Path: "engine"}); !errors.Is(err, ErrDetectionRunLimit) {
		t.Errorf("the 101st run: err = %v, want ErrDetectionRunLimit", err)
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id=$1 AND status='pending'`, f2); n != 0 {
		t.Errorf("the refused create left %d pending runs", n)
	}
}

// ---- 1a-12: the snapshot -------------------------------------------------------------------------------------------

func (x *detLayer) addTraffic(finding int64, n int, urlPrefix string) {
	x.t.Helper()
	idPrefix := fmt.Sprintf("snap%d-", finding)
	x.exec(`INSERT INTO traffic_evidence_snapshots(id, source_traffic_id, captured_at, url, method, status, req_head, resp_head, req_hash, resp_hash, req_len, resp_len)
		SELECT $1::text || g, 'src' || g, 1700000000 + g, $2::text || g, 'GET', 200, 'h', 'h', repeat('a', 64), repeat('b', 64), 10, 20
		FROM generate_series(1, $3::int) g`, idPrefix, urlPrefix, n)
	x.exec(`INSERT INTO finding_traffic_bindings(finding_id, snapshot_id, role, position)
		SELECT $1::bigint, $2::text || g, (ARRAY['baseline','supporting','verification','proof'])[1 + g % 4], g FROM generate_series(1, $3::int) g`, finding, idPrefix, n)
}

func decodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func jsonKeys(m map[string]json.RawMessage) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func requireKeys(t *testing.T, what string, m map[string]json.RawMessage, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := jsonKeys(m); !reflect.DeepEqual(got, want) {
		t.Errorf("%s keys = %v, want %v", what, got, want)
	}
}

func (x *detLayer) snapshotOf(run int64) []byte {
	x.t.Helper()
	snap, err := x.d.FindingDetectionSnapshot(x.ctx, run)
	if err != nil {
		x.t.Fatal(err)
	}
	return snap
}

func TestFindingDetectionSnapshotShape(t *testing.T) {
	x := newDetLayer(t, "detsnap")
	task, err := x.d.CreateTask("task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := x.finding()
	x.exec(`UPDATE findings SET task_id=$1, ticket_id='TICKET-CANARY-7731', report='the report' WHERE id=$2`, task.ID, f)
	x.exec(`INSERT INTO task_constraints(exploration_id, kind, text, origin) SELECT exploration_id, 'deny', 'do not touch /admin', 'operator' FROM tasks WHERE id=$1`, task.ID)
	x.exec(`INSERT INTO assets(type, app_name, url, technologies, service_name, service_type, extra) VALUES ('app', 'snap-app', 'https://app.example/', ARRAY['nginx','php'], 'web', 'http', '{"k":"v"}')`)
	x.exec(`UPDATE findings SET asset_ids=(SELECT jsonb_build_array(id) FROM assets WHERE app_name='snap-app') WHERE id=$1`, f)
	x.addTraffic(f, 250, "https://t.example/p")
	run := x.create(f, "engine")

	snap := x.snapshotOf(run.ID)
	if strings.Contains(string(snap), "TICKET-CANARY-7731") || strings.Contains(string(snap), "ticket") {
		t.Error("the ticket id canary, or the word ticket, is in the snapshot")
	}
	top := decodeObject(t, snap)
	requireKeys(t, "snapshot", top, "finding", "assets", "constraints", "traffic", "traffic_total", "truncated")
	requireKeys(t, "finding", decodeObject(t, top["finding"]), "id", "task_id", "node_id", "vulnclass", "name", "severity", "status", "worker",
		"created_at", "evidence_version", "summary", "evidence", "report")
	var assets []json.RawMessage
	if err := json.Unmarshal(top["assets"], &assets); err != nil || len(assets) != 1 {
		t.Fatalf("assets = %s (err %v), want one", top["assets"], err)
	}
	requireKeys(t, "asset", decodeObject(t, assets[0]), "id", "url", "technologies", "service_name", "service_type", "extra")
	var cons []json.RawMessage
	if err := json.Unmarshal(top["constraints"], &cons); err != nil || len(cons) != 1 {
		t.Fatalf("constraints = %s (err %v), want one", top["constraints"], err)
	}
	requireKeys(t, "constraint", decodeObject(t, cons[0]), "id", "kind", "text", "origin")
	var traffic []json.RawMessage
	if err := json.Unmarshal(top["traffic"], &traffic); err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 200 || string(top["traffic_total"]) != "250" {
		t.Errorf("%d bindings and traffic_total %s, want 200 and 250", len(traffic), top["traffic_total"])
	}
	requireKeys(t, "binding", decodeObject(t, traffic[0]), "binding_id", "snapshot_id", "role", "position", "captured_at", "method", "url", "status", "req_len", "resp_len", "req_hash", "resp_hash")
	var truncated []string
	if err := json.Unmarshal(top["truncated"], &truncated); err != nil || !reflect.DeepEqual(truncated, []string{"traffic"}) {
		t.Errorf("truncated = %v (err %v), want [traffic]", truncated, err)
	}
	// Selection order: the proof bindings come first, so a cut from the end drops the baseline.
	var first snapTraffic
	_ = json.Unmarshal(traffic[0], &first)
	var last snapTraffic
	_ = json.Unmarshal(traffic[199], &last)
	if first.Role != "proof" || last.Role == "proof" {
		t.Errorf("first binding has role %s and the 200th %s, want proof first", first.Role, last.Role)
	}

	// The snapshot is frozen: a change of the finding afterwards does not reach it.
	x.exec(`UPDATE findings SET evidence='changed evidence' WHERE id=$1`, f)
	if strings.Contains(string(x.snapshotOf(run.ID)), "changed evidence") {
		t.Error("the snapshot followed the finding")
	}

	// A finding whose task is gone, and a finding without anything bound, gets a snapshot.
	if err := x.d.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	x.exec(`UPDATE finding_detections SET status='stopped', finished_at=now() WHERE id=$1`, run.ID)
	run2 := x.create(f, "engine")
	finding2 := decodeObject(t, decodeObject(t, x.snapshotOf(run2.ID))["finding"])
	if string(finding2["task_id"]) != "null" || !strings.Contains(string(finding2["evidence"]), "changed evidence") {
		t.Errorf("snapshot of a finding whose task is gone: task_id=%s", finding2["task_id"])
	}
	bare := x.create(x.finding(), "engine")
	if string(decodeObject(t, x.snapshotOf(bare.ID))["traffic"]) != "[]" {
		t.Error("a finding without bindings has no empty traffic list")
	}
}

// A finding with a 1 MiB report gets a snapshot under the budget, and truncated names the field.
func TestFindingDetectionSnapshotCutsTheLargeFields(t *testing.T) {
	x := newDetLayer(t, "detbig")
	f := x.finding()
	x.exec(`UPDATE findings SET report=repeat('r', 1048576), evidence=repeat('e', 100000), summary=repeat('s', 20000) WHERE id=$1`, f)
	run := x.create(f, "engine")
	if n := x.count(`SELECT octet_length(snapshot::text) FROM finding_detections WHERE id=$1`, run.ID); n > DetectionSnapshotBudget {
		t.Errorf("snapshot is %d bytes, over the budget of %d", n, DetectionSnapshotBudget)
	}
	var snap detectionSnapshot
	if err := json.Unmarshal(x.snapshotOf(run.ID), &snap); err != nil {
		t.Fatal(err)
	}
	want := []string{"finding.summary", "finding.evidence", "finding.report"}
	if !reflect.DeepEqual(snap.Truncated, want) || len(snap.Finding.Report) != snapReportCap || len(snap.Finding.Summary) != snapSummaryCap {
		t.Errorf("truncated = %v, report %d bytes, summary %d bytes", snap.Truncated, len(snap.Finding.Report), len(snap.Finding.Summary))
	}
}

// The worst input: every large field made of four-byte characters, of double quotes or of control characters
// (JSON escaping makes a quote two bytes and a control character six), 250 bindings, 60 assets of 200
// technologies each. The stored size is inside the budget, no string is cut inside a character, the shrink steps
// are named in the documented order, and the run is created.
func TestFindingDetectionSnapshotWorstInputStaysInsideTheBudget(t *testing.T) {
	x := newDetLayer(t, "detworst")
	order := map[string]int{}
	for i, st := range snapshotShrinkSteps {
		order[st.name] = i
	}
	units := []struct{ name, unit string }{{"four-byte", "😀"}, {"quote", `"`}, {"control", "\x01"}}
	for ui, u := range units {
		f := x.finding()
		rep := func(bytes int) string { return strings.Repeat(u.unit, bytes/len(u.unit)) }
		x.exec(`UPDATE findings SET report=$2, evidence=$3, summary=$4, name=$5 WHERE id=$1`, f, rep(1<<20), rep(1<<20), rep(1<<20), rep(5000))
		x.addTraffic(f, 250, rep(2400)+"/")
		x.exec(`INSERT INTO assets(type, app_name, url, technologies, service_name, service_type, extra)
			SELECT 'app', $1::text || g, $2::text, array_fill($3::text, ARRAY[200]), $4::text, 'http', jsonb_build_object('k', $5::text)
			FROM generate_series(1, 60) g`, fmt.Sprint("worst", ui, "-"), rep(2400), rep(128), rep(300), rep(8000))
		x.exec(`UPDATE findings SET asset_ids=(SELECT jsonb_agg(id) FROM assets WHERE app_name LIKE $2) WHERE id=$1`, f, fmt.Sprint("worst", ui, "-%"))

		run, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine"})
		if err != nil || !created {
			t.Fatalf("%s: created=%t err=%v", u.name, created, err)
		}
		if n := x.count(`SELECT octet_length(snapshot::text) FROM finding_detections WHERE id=$1`, run.ID); n > DetectionSnapshotBudget {
			t.Errorf("%s: snapshot is %d bytes, over the budget of %d", u.name, n, DetectionSnapshotBudget)
		}
		var snap detectionSnapshot
		if err := json.Unmarshal(x.snapshotOf(run.ID), &snap); err != nil {
			t.Fatal(err)
		}
		// No string is cut inside a character: every string is a whole number of the unit.
		whole := func(what, s string) {
			if !utf8.ValidString(s) || strings.Contains(s, "�") || strings.Trim(s, u.unit) != "" {
				t.Errorf("%s: %s is not made of whole %q characters (%d bytes)", u.name, what, u.unit, len(s))
			}
		}
		whole("report", snap.Finding.Report)
		whole("evidence", snap.Finding.Evidence)
		whole("summary", snap.Finding.Summary)
		whole("name", snap.Finding.Name)
		for _, tr := range snap.Traffic {
			if i := strings.IndexByte(tr.URL, '/'); i >= 0 {
				whole("traffic url", tr.URL[:i])
			} else {
				whole("traffic url", tr.URL)
			}
		}
		for _, a := range snap.Assets {
			whole("asset url", a.URL)
			whole("service name", a.ServiceName)
		}
		if len(snap.Traffic) == 0 || len(snap.Assets) == 0 {
			t.Errorf("%s: the snapshot lost all traffic (%d) or all assets (%d)", u.name, len(snap.Traffic), len(snap.Assets))
		}
		// The shrink steps that were applied are named in the documented order.
		last := -1
		for _, name := range snap.Truncated {
			idx, ok := order[name]
			if !strings.HasPrefix(name, "shrink:") {
				continue
			}
			if !ok || idx <= last {
				t.Errorf("%s: truncated %v: %s is out of order or unknown", u.name, snap.Truncated, name)
			}
			last = idx
		}
		if u.name == "control" {
			// 250 bindings of 2 KiB URLs at six bytes per byte cannot fit: the shrink steps ran.
			if last < 0 {
				t.Errorf("control: no shrink step was needed for the worst input (truncated %v)", snap.Truncated)
			}
		}
	}
}

// Only the newest five runs of a finding keep their snapshot and manifest; the rows and the rules stay.
func TestFindingDetectionOldSnapshotsAreCompacted(t *testing.T) {
	x := newDetLayer(t, "detcompact")
	f := x.finding()
	var ids []int64
	for i := 0; i < 6; i++ {
		id := x.runningRun(f)
		if i == 0 {
			if _, err := x.d.RecordDetectionRule(x.ctx, id, sigmaRule("kept rule")); err != nil {
				t.Fatal(err)
			}
		}
		if err := x.d.SetDetectionManifest(x.ctx, id, json.RawMessage(`{"budget":1}`)); err != nil {
			t.Fatal(err)
		}
		if i < 5 {
			if err := x.d.FinishFindingDetection(x.ctx, id, DetectionFinish{Status: "completed", Outcome: "unvalidated"}); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
		// After the fifth run, all five are full.
		if i == 4 {
			for _, old := range ids {
				if x.text(`SELECT snapshot::text FROM finding_detections WHERE id=$1`, old) == "{}" {
					t.Errorf("run %d lost its snapshot with five runs", old)
				}
			}
		}
	}
	// The sixth run was created: the oldest finished run is empty, the others are not, the rule stays.
	if x.text(`SELECT snapshot::text FROM finding_detections WHERE id=$1`, ids[0]) != "{}" || x.text(`SELECT input_manifest::text FROM finding_detections WHERE id=$1`, ids[0]) != "{}" {
		t.Error("the oldest run kept its snapshot or manifest after the sixth run started")
	}
	for _, id := range ids[1:] {
		if x.text(`SELECT snapshot::text FROM finding_detections WHERE id=$1`, id) == "{}" {
			t.Errorf("run %d lost its snapshot", id)
		}
	}
	if rules, _ := x.d.ListDetectionRules(x.ctx, ids[0]); len(rules) != 1 || rules[0].Title != "kept rule" {
		t.Errorf("the rules of the compacted run = %v, want the one kept", rules)
	}
	if list, _ := x.d.ListFindingDetections(x.ctx, f); len(list) != 6 {
		t.Errorf("history has %d runs, want 6 (compaction keeps the rows)", len(list))
	}
}

// The history, run and rule queries do not select the snapshot: asserted on the returned types, on the statement
// text, and on what a list returns when the snapshot holds a canary.
func TestFindingDetectionListsNeverReadTheSnapshot(t *testing.T) {
	for _, ty := range []reflect.Type{reflect.TypeOf(FindingDetection{}), reflect.TypeOf(DetectionRule{}), reflect.TypeOf(DetectionStep{})} {
		for i := 0; i < ty.NumField(); i++ {
			if strings.Contains(strings.ToLower(ty.Field(i).Name), "snapshot") || strings.Contains(strings.ToLower(string(ty.Field(i).Tag)), "snapshot") {
				t.Errorf("%s has the field %s", ty.Name(), ty.Field(i).Name)
			}
		}
	}
	if strings.Contains(strings.ToLower(detectionListCols), "snapshot") || strings.Contains(strings.ToLower(detectionListCols), "input_manifest") {
		t.Errorf("the list columns name the snapshot or the manifest: %s", detectionListCols)
	}
	if strings.Contains(strings.ToLower(detectionRuleCols), "snapshot") {
		t.Errorf("the rule columns name the snapshot: %s", detectionRuleCols)
	}

	x := newDetLayer(t, "detlists")
	f := x.finding()
	run := x.runningRun(f)
	x.exec(`UPDATE finding_detections SET snapshot='{"canary":"SNAPSHOT-CANARY-5521"}', input_manifest='{"canary":"MANIFEST-CANARY-5521"}' WHERE id=$1`, run)
	if _, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule("listed")); err != nil {
		t.Fatal(err)
	}
	list, err := x.d.ListFindingDetections(x.ctx, f)
	if err != nil || len(list) != 1 {
		t.Fatalf("history: %d runs, err %v", len(list), err)
	}
	one, err := x.d.GetFindingDetection(x.ctx, run)
	if err != nil || one == nil {
		t.Fatal(err)
	}
	rules, _ := x.d.ListDetectionRules(x.ctx, run)
	byFinding, _ := x.d.ListFindingDetectionRules(x.ctx, f)
	listJSON, _ := json.Marshal(list)
	oneJSON, _ := json.Marshal(one)
	ruleJSON, _ := json.Marshal([][]*DetectionRule{rules, byFinding})
	if strings.Contains(string(listJSON), "CANARY") || strings.Contains(string(oneJSON), "SNAPSHOT-CANARY") || strings.Contains(string(ruleJSON), "CANARY") {
		t.Error("a list or a run carries the snapshot canary")
	}
	// The history leaves the manifest out; the single run has it.
	if list[0].InputManifest != nil || !strings.Contains(string(one.InputManifest), "MANIFEST-CANARY-5521") {
		t.Errorf("manifest in the history %q, in the run %q", list[0].InputManifest, one.InputManifest)
	}
	// The snapshot is read only by the worker's functions.
	if !strings.Contains(string(x.snapshotOf(run)), "SNAPSHOT-CANARY-5521") {
		t.Error("FindingDetectionSnapshot does not return the snapshot")
	}
	if len(byFinding) != 1 || byFinding[0].ID != rules[0].ID {
		t.Errorf("rules of the finding = %v", byFinding)
	}
}

// ---- 1a-13: recovery ------------------------------------------------------------------------------------------------

func TestFindingDetectionRecoveryStopsWhatWasRunning(t *testing.T) {
	x := newDetLayer(t, "detrecover")
	engineRunning := x.runningRun(x.finding())
	enginePending := x.create(x.finding(), "engine").ID
	agentPending := x.create(x.finding(), "agent").ID
	agentRunning := x.create(x.finding(), "agent").ID
	x.exec(`UPDATE finding_detections SET status='running', claimed_at=now() WHERE id=$1`, agentRunning)
	finished := x.runningRun(x.finding())
	if err := x.d.FinishFindingDetection(x.ctx, finished, DetectionFinish{Status: "completed", Outcome: "no_rules"}); err != nil {
		t.Fatal(err)
	}

	stopped, queued, err := x.d.RecoverFindingDetections()
	if err != nil || stopped != 3 || queued != 1 {
		t.Fatalf("recovery: stopped=%d queued=%d err=%v, want 3 and 1", stopped, queued, err)
	}
	for _, id := range []int64{engineRunning, agentPending, agentRunning} {
		if got, _ := x.d.GetFindingDetection(x.ctx, id); got == nil || got.Status != "stopped" || got.EndCode != "restart" || got.FinishedAt == nil ||
			!strings.Contains(got.Error, "Server restarted") {
			t.Errorf("run %d after recovery: %+v", id, got)
		}
	}
	if x.status(enginePending) != "pending" || x.status(finished) != "completed" {
		t.Errorf("a pending engine run or a finished run changed: %s, %s", x.status(enginePending), x.status(finished))
	}
	state := func() string {
		return x.text(`SELECT string_agg(id || ':' || status || ':' || end_code || ':' || COALESCE(finished_at::text, ''), ',' ORDER BY id) FROM finding_detections`)
	}
	before := state()
	stopped, queued, err = x.d.RecoverFindingDetections()
	if err != nil || stopped != 0 || queued != 1 {
		t.Errorf("second recovery: stopped=%d queued=%d err=%v, want 0 and 1", stopped, queued, err)
	}
	if after := state(); after != before {
		t.Errorf("a second recovery changed the runs:\n%s\n%s", before, after)
	}
}

// ---- 1a-14: queue, sweep, writes after the end ----------------------------------------------------------------------

func TestFindingDetectionClaimsAreExclusive(t *testing.T) {
	x := newDetLayer(t, "detclaim")
	const runs = 6
	want := map[int64]bool{}
	for range runs {
		want[x.create(x.finding(), "engine").ID] = true
	}
	x.create(x.finding(), "agent") // never claimed

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[int64]int{}
	for range 12 {
		wg.Go(func() {
			c, err := x.d.ClaimFindingDetection(x.ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if c == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			claimed[c.ID]++
			if c.Phase != "generate" || len(c.Snapshot) == 0 {
				t.Errorf("claim returned phase %q and %d snapshot bytes", c.Phase, len(c.Snapshot))
			}
		})
	}
	wg.Wait()
	if len(claimed) != runs {
		t.Fatalf("%d distinct runs claimed by 12 claimers, want %d", len(claimed), runs)
	}
	for id, n := range claimed {
		if n != 1 || !want[id] {
			t.Errorf("run %d claimed %d times (engine run: %t)", id, n, want[id])
		}
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE status='running' AND attempts=1 AND claimed_at IS NOT NULL AND started_at IS NOT NULL`); n != runs {
		t.Errorf("%d runs are running with attempts=1, claimed_at and started_at, want %d", n, runs)
	}
	if c, err := x.d.ClaimFindingDetection(x.ctx); err != nil || c != nil {
		t.Errorf("a claim with nothing queued: %v %v, want nil", c, err)
	}
}

// A claim does not wait for a run whose row another transaction holds: it takes the next one (SKIP LOCKED). Without
// it the claim would block behind the holder and the call would run into its deadline.
func TestFindingDetectionClaimSkipsLockedRows(t *testing.T) {
	x := newDetLayer(t, "detskip")
	first := x.create(x.finding(), "engine")
	second := x.create(x.finding(), "engine")
	tx, err := x.d.BeginTx(x.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var held int64
	if err := tx.QueryRow(`SELECT id FROM finding_detections WHERE id=$1 FOR UPDATE`, first.ID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(x.ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	c, err := x.d.ClaimFindingDetection(ctx)
	if err != nil || c == nil || c.ID != second.ID {
		t.Fatalf("claim while the first run is locked: %+v err=%v, want the second run (%d)", c, err, second.ID)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the claim waited %v for a locked row", time.Since(start))
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if c, err := x.d.ClaimFindingDetection(x.ctx); err != nil || c == nil || c.ID != first.ID {
		t.Errorf("claim after the lock ended: %+v err=%v, want the first run", c, err)
	}
}

func TestFindingDetectionClaimRespectsNotBeforeAndReturnsThePhase(t *testing.T) {
	x := newDetLayer(t, "detnb")
	run := x.create(x.finding(), "engine")
	x.exec(`UPDATE finding_detections SET not_before=now() + interval '1 hour', phase='validate', notes='hello' WHERE id=$1`, run.ID)
	if c, err := x.d.ClaimFindingDetection(x.ctx); err != nil || c != nil {
		t.Fatalf("a run parked until the future was claimed: %v %v", c, err)
	}
	x.exec(`UPDATE finding_detections SET not_before=now() - interval '1 second' WHERE id=$1`, run.ID)
	c, err := x.d.ClaimFindingDetection(x.ctx)
	if err != nil || c == nil || c.ID != run.ID || c.Phase != "validate" || c.Notes != "hello" || c.FindingID != run.FindingID {
		t.Fatalf("claim after not_before passed: %+v err=%v", c, err)
	}

	// claimed_at is set on every claim; started_at is kept.
	var firstClaim, firstStart time.Time
	if err := x.d.QueryRow(`SELECT claimed_at, started_at FROM finding_detections WHERE id=$1`, run.ID).Scan(&firstClaim, &firstStart); err != nil {
		t.Fatal(err)
	}
	x.exec(`UPDATE finding_detections SET status='pending', claimed_at=NULL WHERE id=$1`, run.ID)
	if c, err = x.d.ClaimFindingDetection(x.ctx); err != nil || c == nil || c.ID != run.ID {
		t.Fatalf("second claim: %v %v", c, err)
	}
	var secondClaim, secondStart time.Time
	var attempts int
	if err := x.d.QueryRow(`SELECT claimed_at, started_at, attempts FROM finding_detections WHERE id=$1`, run.ID).Scan(&secondClaim, &secondStart, &attempts); err != nil {
		t.Fatal(err)
	}
	if !secondClaim.After(firstClaim) || !secondStart.Equal(firstStart) || attempts != 2 {
		t.Errorf("second claim: claimed_at %v after %v, started_at %v vs %v, attempts %d", secondClaim, firstClaim, secondStart, firstStart, attempts)
	}
}

func TestFindingDetectionSweepFailsOnlyStaleEngineRuns(t *testing.T) {
	x := newDetLayer(t, "detsweep")
	stale := x.runningRun(x.finding())
	fresh := x.runningRun(x.finding())
	pending := x.create(x.finding(), "engine").ID
	agent := x.create(x.finding(), "agent").ID
	x.exec(`UPDATE finding_detections SET status='running', claimed_at=now() - interval '30 minutes' WHERE id=$1`, agent)
	x.exec(`UPDATE finding_detections SET claimed_at=now() - interval '30 minutes' WHERE id=$1`, stale)
	x.exec(`UPDATE finding_detections SET claimed_at=now() - interval '19 minutes' WHERE id=$1`, fresh)

	n, err := x.d.SweepFindingDetections(x.ctx, DetectionSweepDeadline)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v, want 1", n, err)
	}
	got, _ := x.d.GetFindingDetection(x.ctx, stale)
	if got.Status != "failed" || got.EndCode != "deadline" || got.FinishedAt == nil || got.Error == "" {
		t.Errorf("the stale run: %+v", got)
	}
	if x.status(fresh) != "running" || x.status(pending) != "pending" || x.status(agent) != "running" {
		t.Errorf("a fresh run, a pending run or an agent run changed: %s %s %s", x.status(fresh), x.status(pending), x.status(agent))
	}
	if n, err := x.d.SweepFindingDetections(x.ctx, DetectionSweepDeadline); err != nil || n != 0 {
		t.Errorf("a second sweep: %d %v, want 0", n, err)
	}
	// A worker that finds its run failed by the sweep ends quietly.
	if err := x.d.FinishFindingDetection(x.ctx, stale, DetectionFinish{Status: "completed", Outcome: "unvalidated"}); !errors.Is(err, ErrDetectionNotRunning) {
		t.Errorf("finish after the sweep: err = %v, want ErrDetectionNotRunning", err)
	}
}

// The worker's recover() contract: a panic is recorded as a failed run with end_code worker_panic through
// FinishFindingDetection; the sweep is the net when even that did not happen.
func TestFindingDetectionPanicIsRecordedAndTheSweepIsTheNet(t *testing.T) {
	x := newDetLayer(t, "detpanic")
	run := x.runningRun(x.finding())
	if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "failed", EndCode: "worker_panic", Error: "The detection worker failed unexpectedly."}); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.d.GetFindingDetection(x.ctx, run); got.Status != "failed" || got.EndCode != "worker_panic" || got.Outcome != "" {
		t.Errorf("the panicked run: %+v", got)
	}
	// The worker that could not record its panic leaves a running run; the sweep ends it.
	left := x.runningRun(x.finding())
	x.exec(`UPDATE finding_detections SET claimed_at=now() - interval '21 minutes' WHERE id=$1`, left)
	if n, err := x.d.SweepFindingDetections(x.ctx, DetectionSweepDeadline); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if got, _ := x.d.GetFindingDetection(x.ctx, left); got.Status != "failed" || got.EndCode != "deadline" {
		t.Errorf("the left run: %+v", got)
	}
}

// A write for a run that is no longer running affects no row and returns the "not running" error.
func TestFindingDetectionWritesAfterTheEndAreRefused(t *testing.T) {
	x := newDetLayer(t, "detended")
	for _, how := range []string{"stopped", "finished", "deleted", "pending"} {
		f := x.finding()
		run := x.runningRun(f)
		if _, err := x.d.RecordDetectionRule(x.ctx, run, sigmaRule("before")); err != nil {
			t.Fatal(err)
		}
		switch how {
		case "stopped":
			if ok, err := x.d.StopFindingDetection(x.ctx, run); err != nil || !ok {
				t.Fatal(ok, err)
			}
		case "finished":
			if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "failed", EndCode: "provider_error"}); err != nil {
				t.Fatal(err)
			}
		case "deleted":
			if n, err := x.d.DeleteFinding(f); err != nil || n != 1 {
				t.Fatal(n, err)
			}
		case "pending":
			x.exec(`UPDATE finding_detections SET status='pending' WHERE id=$1`, run)
		}
		before := x.count(`SELECT (SELECT count(*) FROM detection_rules) + (SELECT count(*) FROM detection_steps) + (SELECT count(*) FROM detection_query_results)`)
		writes := map[string]error{
			"step":     x.d.AppendDetectionStep(x.ctx, run, DetectionStep{Step: "context", Status: "ok"}),
			"manifest": x.d.SetDetectionManifest(x.ctx, run, json.RawMessage(`{"a":1}`)),
			"finish":   x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "unvalidated", Rules: []DetectionRuleInput{sigmaRule("late")}}),
			"result":   x.d.RecordDetectionQueryResult(x.ctx, run, DetectionQueryResult{BodySHA256: strings.Repeat("d", 64), WindowKind: "attack", State: "ok"}),
		}
		_, writes["rule"] = x.d.RecordDetectionRule(x.ctx, run, sigmaRule("late"))
		_, writes["increment"] = x.d.IncrementDetectionQueries(x.ctx, run, 8)
		for name, err := range writes {
			if !errors.Is(err, ErrDetectionNotRunning) {
				t.Errorf("%s: a %s write: err = %v, want ErrDetectionNotRunning", how, name, err)
			}
		}
		if after := x.count(`SELECT (SELECT count(*) FROM detection_rules) + (SELECT count(*) FROM detection_steps) + (SELECT count(*) FROM detection_query_results)`); after != before {
			t.Errorf("%s: the refused writes changed the row count from %d to %d", how, before, after)
		}
	}
	// A stop of a finished run is not an error and changes nothing.
	f := x.finding()
	run := x.runningRun(f)
	if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "no_rules"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := x.d.StopFindingDetection(x.ctx, run); err != nil || ok {
		t.Errorf("stop of a finished run: %t %v, want false and no error", ok, err)
	}
}

// The finish writes the rules, the outcome and the report together; a refused rule stores nothing.
func TestFindingDetectionFinishRecordsRulesAndOutcomeTogether(t *testing.T) {
	x := newDetLayer(t, "detfinish")
	f := x.finding()
	run := x.runningRun(f)
	dropped := sigmaRule("dropped")
	dropped.Validation = "syntax_error"
	dropped.ValidationDetail = json.RawMessage(`{"code":"sigma_modifier"}`)
	err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "unvalidated", Summary: "one rule",
		StageReport: json.RawMessage(`{"attempt":{"state":"rule","count":1}}`), Rules: []DetectionRuleInput{sigmaRule("good"), dropped}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := x.d.GetFindingDetection(x.ctx, run)
	if err != nil || got.Status != "completed" || got.Outcome != "unvalidated" || got.Summary != "one rule" || got.FinishedAt == nil ||
		string(got.StageReport) != `{"attempt": {"count": 1, "state": "rule"}}` {
		t.Errorf("run after finish: %+v (err %v)", got, err)
	}
	rules, _ := x.d.ListDetectionRules(x.ctx, run)
	if len(rules) != 2 || rules[0].Validation != "unvalidated" || rules[1].Validation != "syntax_error" || string(rules[1].ValidationDetail) != `{"code": "sigma_modifier"}` {
		t.Errorf("rules after finish: %+v", rules)
	}

	// A rule refused inside the transaction takes the others with it.
	f2 := x.finding()
	run2 := x.runningRun(f2)
	bad := sigmaRule("bad")
	bad.ValidationDetail = json.RawMessage(x.densePayload(65537))
	err = x.d.FinishFindingDetection(x.ctx, run2, DetectionFinish{Status: "completed", Outcome: "unvalidated", Rules: []DetectionRuleInput{sigmaRule("first"), bad}})
	requireLimit(t, "a finish with an oversize rule detail", err, "validation_detail")
	if n := x.count(`SELECT count(*) FROM detection_rules WHERE detection_id=$1`, run2); n != 0 || x.status(run2) != "running" {
		t.Errorf("after the refused finish: %d rules, status %s, want none and running", n, x.status(run2))
	}
	// A finish may only be completed or failed; the stop has its own function.
	if err := x.d.FinishFindingDetection(x.ctx, run2, DetectionFinish{Status: "stopped"}); err == nil {
		t.Error("a finish as stopped was accepted")
	}
}

// ---- 1a-15: query accounting ------------------------------------------------------------------------------------------

func TestFindingDetectionQueryCounterIsExactUnderConcurrency(t *testing.T) {
	x := newDetLayer(t, "detqueries")
	run := x.runningRun(x.finding())
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, capped := 0, 0
	seen := map[int]bool{}
	for range 16 {
		wg.Go(func() {
			used, err := x.d.IncrementDetectionQueries(x.ctx, run, 8)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				if seen[used] {
					t.Errorf("count %d returned twice", used)
				}
				seen[used] = true
			case errors.Is(err, ErrDetectionQueryCap):
				capped++
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok != 8 || capped != 8 || len(seen) != 8 {
		t.Errorf("%d increments succeeded and %d were refused (counts %v), want 8 and 8 with counts 1 to 8", ok, capped, seen)
	}
	if n := x.count(`SELECT queries_used FROM finding_detections WHERE id=$1`, run); n != 8 {
		t.Errorf("queries_used = %d, want 8", n)
	}
	// The check constraint: the counter cannot go negative.
	_, err := x.d.ExecContext(x.ctx, `UPDATE finding_detections SET queries_used=-1 WHERE id=$1`, run)
	requireViolation(t, "a negative counter", err, "23514")
}

func TestFindingDetectionQueryResultsAreKeyedByRunHashAndWindow(t *testing.T) {
	x := newDetLayer(t, "detresults")
	runA, runB := x.runningRun(x.finding()), x.runningRun(x.finding())
	sha := strings.Repeat("e", 64)
	hits := 4
	if err := x.d.RecordDetectionQueryResult(x.ctx, runA, DetectionQueryResult{BodySHA256: sha, WindowKind: "attack", State: "ok", Hits: &hits, Detail: json.RawMessage(`{"rows":4}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := x.d.FindDetectionQueryResult(x.ctx, runA, sha, "attack")
	if err != nil || got == nil || got.State != "ok" || got.Hits == nil || *got.Hits != 4 || string(got.Detail) != `{"rows": 4}` {
		t.Fatalf("stored result: %+v (err %v)", got, err)
	}
	// The same key twice keeps one row, with the newer result.
	if err := x.d.RecordDetectionQueryResult(x.ctx, runA, DetectionQueryResult{BodySHA256: sha, WindowKind: "attack", State: "error"}); err != nil {
		t.Fatal(err)
	}
	if n := x.count(`SELECT count(*) FROM detection_query_results WHERE detection_id=$1`, runA); n != 1 {
		t.Errorf("%d rows for one key, want 1", n)
	}
	if got, _ := x.d.FindDetectionQueryResult(x.ctx, runA, sha, "attack"); got == nil || got.State != "error" || got.Hits != nil {
		t.Errorf("the second write did not replace the first: %+v", got)
	}
	// Another window is another key; another run never sees it.
	if err := x.d.RecordDetectionQueryResult(x.ctx, runA, DetectionQueryResult{BodySHA256: sha, WindowKind: "baseline_1d", State: "ok"}); err != nil {
		t.Fatal(err)
	}
	if n := x.count(`SELECT count(*) FROM detection_query_results WHERE detection_id=$1`, runA); n != 2 {
		t.Errorf("%d rows for two windows, want 2", n)
	}
	for _, c := range []struct {
		run    int64
		sha    string
		window string
	}{{runB, sha, "attack"}, {runA, strings.Repeat("f", 64), "attack"}, {runA, sha, "liveness"}} {
		if got, err := x.d.FindDetectionQueryResult(x.ctx, c.run, c.sha, c.window); err != nil || got != nil {
			t.Errorf("result of run %d for %s/%s: %+v %v, want none", c.run, c.sha[:4], c.window, got, err)
		}
	}
	// Bad keys are refused before the database is asked.
	for _, bad := range []DetectionQueryResult{{BodySHA256: "short", WindowKind: "attack", State: "ok"}, {BodySHA256: sha, WindowKind: "week", State: "ok"}, {BodySHA256: sha, WindowKind: "attack", State: "maybe"}} {
		if err := x.d.RecordDetectionQueryResult(x.ctx, runA, bad); err == nil {
			t.Errorf("result %+v was accepted", bad)
		}
	}
}

// ---- for the archive refusal (slice S3) ---------------------------------------------------------------------------------

// taskHasDetectionRuns is the question the archive refusal asks: runs count, tombstones do not.
func TestFindingDetectionTaskHasRunsIgnoresTombstones(t *testing.T) {
	x := newDetLayer(t, "dettask")
	task, err := x.d.CreateTask("task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := x.finding()
	x.exec(`UPDATE findings SET task_id=$1 WHERE id=$2`, task.ID, f)
	has := func() bool {
		ok, err := taskHasDetectionRuns(x.ctx, x.d, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if has() {
		t.Error("a task without runs has runs")
	}
	auto, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine", TriggerKind: "auto", AutoAttempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !has() {
		t.Error("a task with a run has none")
	}
	x.exec(`UPDATE finding_detections SET status='completed', finished_at=now() WHERE id=$1`, auto.ID)
	if tomb, err := x.d.DeleteFindingDetection(x.ctx, auto.ID); err != nil || !tomb {
		t.Fatal(tomb, err)
	}
	if has() {
		t.Error("a task whose only run is a tombstone has runs")
	}
	other := x.finding() // a finding of another task is not counted
	x.create(other, "engine")
	if has() {
		t.Error("a run of a finding of no task was counted")
	}
}

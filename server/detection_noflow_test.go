package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/intercept"
)

// Guards for "no flow back" (docs/detector-plan.md section 11, criteria 1a-17 and 1a-18): nothing ARTEX hands to
// the planner or a worker may carry detection data. Three kinds of guard: a canary test over the agent-facing
// outputs, source guards (no agent-facing code reads the four detection tables), and a diff guard.

// detectionTables are the four tables of migration 5003.
var detectionTables = []string{"finding_detections", "detection_rules", "detection_steps", "detection_query_results"}

// moduleRoot is the directory above server/ (the tests run in the package directory).
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	return root
}

// goSources returns the non-test Go files under dir (relative to root, slash-separated) with their text.
func goSources(t *testing.T, root string, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "third_party", "web":
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(raw)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no Go sources found under %v: the guard would be vacuous", dirs)
	}
	return out
}

func mentionsDetectionTable(text string) []string {
	var found []string
	for _, name := range detectionTables {
		if strings.Contains(text, name) {
			found = append(found, name)
		}
	}
	return found
}

// ---- the canary test ------------------------------------------------------------------------------------------

// detectionFlowFixture makes a finished detection run whose every free-text field holds the canary: notes, rule
// title, body and coverage, the summary, the stage report and the manifest.
func detectionFlowFixture(t *testing.T, a *ticketAPI, canary string) {
	t.Helper()
	pg := a.s.m.pg
	ctx := context.Background()
	run, _, created, err := pg.CreateFindingDetection(ctx, db.CreateDetectionInput{FindingID: a.finding, Path: "engine", Notes: canary})
	if err != nil || !created {
		t.Fatalf("create the detection run: created=%t err=%v", created, err)
	}
	if _, err := pg.ExecContext(ctx, `UPDATE finding_detections SET status='running', started_at=now(), claimed_at=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(map[string]any{"note": canary})
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetDetectionManifest(ctx, run.ID, manifest); err != nil {
		t.Fatal(err)
	}
	if err := pg.AppendDetectionStep(ctx, run.ID, db.DetectionStep{Step: "context", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	stage, err := json.Marshal(map[string]any{"reason": canary})
	if err != nil {
		t.Fatal(err)
	}
	rule := db.DetectionRuleInput{Stage: "attempt", Format: "sigma", DataSource: "http_access", Technology: "nginx",
		AttackTechniques: []string{"T1190"}, Title: canary, Body: "title: " + canary + "\n", Coverage: canary}
	if err := pg.FinishFindingDetection(ctx, run.ID, db.DetectionFinish{Status: "completed", Outcome: "unvalidated",
		Summary: canary, StageReport: stage, Rules: []db.DetectionRuleInput{rule}}); err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := pg.QueryRowContext(ctx, `SELECT count(*) FROM detection_rules WHERE finding_id=$1 AND body LIKE '%'||$2||'%'`, a.finding, canary).Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("the fixture rule is not stored: %d %v", seen, err)
	}
}

func TestDetectionDataStaysOutOfAgentFacingOutput(t *testing.T) {
	a := newTicketAPI(t)
	pg := a.s.m.pg
	ctx := context.Background()
	const canary = "DETLEAK-8c41f7d2"
	detectionFlowFixture(t, a, canary)

	check := func(name, content string) {
		t.Helper()
		if content == "" {
			t.Fatalf("%s: empty output, the check would be vacuous", name)
		}
		lower := strings.ToLower(content)
		if strings.Contains(content, canary) || strings.Contains(lower, "detection") || len(mentionsDetectionTable(lower)) > 0 {
			t.Fatalf("%s carries detection data: %s", name, content)
		}
	}
	taskID := strconv.FormatInt(a.taskID, 10)

	// A finished intent with its output, so that get_worker_output has an output to return.
	var expID int64
	if err := pg.QueryRow(`SELECT exploration_id FROM tasks WHERE id=$1`, a.taskID).Scan(&expID); err != nil {
		t.Fatal(err)
	}
	store := pg.Exploration(expID)
	intent, err := store.AddNode("intent", map[string]any{"summary": "probe the login form"}, 5, "done", "planner", nil) // done: a task that inherits reads only finished intents
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendActivity(db.Activity{NodeID: &intent, Worker: "w1", Kind: "result", Summary: "login form probed", Detail: "login form probed: no finding"}); err != nil {
		t.Fatal(err)
	}

	// Cross-task orchestration tools.
	list := workflowCall(t, ctx, a.s.toolListTaskFindings(), map[string]any{"task_id": taskID}, false)
	if !strings.Contains(list, strconv.FormatInt(a.finding, 10)) {
		t.Fatalf("list_task_findings does not list the fixture finding: %s", list)
	}
	check("list_task_findings", list)
	check("get_task_node_detail", workflowCall(t, ctx, a.s.toolGetTaskNodeDetail(), map[string]any{"task_id": taskID, "id": a.nodeID}, false))
	check("get_task_graph", workflowCall(t, ctx, a.s.toolGetTaskGraph(), map[string]any{"task_id": taskID}, false))
	check("get_finding_traffic", workflowCall(t, ctx, a.s.toolGetFindingTraffic(), map[string]any{"finding_id": a.finding}, false))

	// The worker tools and the planner's read tools (the planner context is built from graph_overview's data and
	// the same finding reads).
	own := agent.NewToolSet(store, "worker")
	own.SetTaskID(a.taskID)
	workerTools, plannerTools := own.WorkerTools(), own.PlannerTools()
	out := workflowCall(t, ctx, workflowTool(t, workerTools, "list_findings"), map[string]any{}, false)
	if !strings.Contains(out, strconv.FormatInt(a.finding, 10)) {
		t.Fatalf("worker list_findings does not list the fixture finding: %s", out)
	}
	check("worker list_findings", out)
	check("worker node_detail", workflowCall(t, ctx, workflowTool(t, workerTools, "node_detail"), map[string]any{"id": a.nodeID}, false))
	out = workflowCall(t, ctx, workflowTool(t, plannerTools, "get_worker_output"), map[string]any{"intent_id": intent}, false)
	if !strings.Contains(out, "login form probed") {
		t.Fatalf("get_worker_output lost the fixture output: %s", out)
	}
	check("get_worker_output", out)
	check("planner graph_overview", workflowCall(t, ctx, workflowTool(t, plannerTools, "graph_overview"), map[string]any{}, false))
	check("planner list_findings", workflowCall(t, ctx, workflowTool(t, plannerTools, "list_findings"), map[string]any{}, false))
	check("planner list_facts", workflowCall(t, ctx, workflowTool(t, plannerTools, "list_facts"), map[string]any{}, false))
	check("planner node_detail", workflowCall(t, ctx, workflowTool(t, plannerTools, "node_detail"), map[string]any{"id": a.nodeID}, false))

	// The same for a task that inherits the finding: its tools read the source task's rows read-only.
	child, err := a.s.m.CreateTaskWithOptions("inherits the finding", "fixture", db.TaskCreateOptions{SourceTaskIDs: []int64{a.taskID}})
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := strconv.ParseInt(child.ID, 10, 64)
	t.Cleanup(func() { pg.DeleteTask(childID) })
	inh := agent.NewToolSet(pg.Exploration(child.ExpID), "worker")
	inh.SetTaskID(childID)
	inhWorker, inhPlanner := inh.WorkerTools(), inh.PlannerTools()
	out = workflowCall(t, ctx, workflowTool(t, inhWorker, "list_findings"), map[string]any{}, false)
	if !strings.Contains(out, strconv.FormatInt(a.finding, 10)) || !strings.Contains(out, "inherited") {
		t.Fatalf("inherited list_findings does not list the source finding: %s", out)
	}
	check("inherited worker list_findings", out)
	check("inherited worker node_detail", workflowCall(t, ctx, workflowTool(t, inhWorker, "node_detail"), map[string]any{"id": a.nodeID}, false))
	out = workflowCall(t, ctx, workflowTool(t, inhPlanner, "get_worker_output"), map[string]any{"intent_id": intent}, false)
	if !strings.Contains(out, "login form probed") || !strings.Contains(out, "inherited") {
		t.Fatalf("inherited get_worker_output lost the source output: %s", out)
	}
	check("inherited get_worker_output", out)
	check("inherited planner graph_overview", workflowCall(t, ctx, workflowTool(t, inhPlanner, "graph_overview"), map[string]any{}, false))
	check("inherited list_task_findings", workflowCall(t, ctx, a.s.toolListTaskFindings(), map[string]any{"task_id": child.ID}, false))
	check("inherited get_task_node_detail", workflowCall(t, ctx, a.s.toolGetTaskNodeDetail(), map[string]any{"task_id": child.ID, "id": a.nodeID}, false))
	check("inherited get_task_graph", workflowCall(t, ctx, a.s.toolGetTaskGraph(), map[string]any{"task_id": child.ID}, false))

	// Chat mention: the whole finding row is serialised into the prompt; and its search results.
	mention, err := loadChatMention(pg, chatMentionRef{Kind: "finding", ID: a.finding})
	if err != nil || mention == nil {
		t.Fatal(mention, err)
	}
	blob, err := json.Marshal(mention)
	if err != nil {
		t.Fatal(err)
	}
	check("chat mention payload", string(blob))
	page, err := pg.SearchChatMentionsPage(ctx, "finding", strconv.FormatInt(a.finding, 10), "")
	if err != nil {
		t.Fatal(err)
	}
	if blob, err = json.Marshal(page); err != nil {
		t.Fatal(err)
	}
	check("chat mention search", string(blob))
	// The finding as the database layer hands it out, serialised the way a prompt would carry it.
	f, err := pg.GetFinding(a.finding)
	if err != nil || f == nil {
		t.Fatal(f, err)
	}
	if blob, err = json.Marshal(f); err != nil {
		t.Fatal(err)
	}
	check("GetFinding payload", string(blob))

	// Retest: the snapshot the retester agent reads, and the tool it calls.
	r, _, _, err := pg.CreateFindingRetest(ctx, a.finding, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM finding_retests WHERE id=$1`, r.ID)
		pg.Exec(`DELETE FROM conversations WHERE id=$1`, *r.ConversationID)
	})
	var detail string
	if err := pg.QueryRow(`SELECT COALESCE(string_agg(detail||summary,' '),'') FROM conversation_activities WHERE conversation_id=$1`, *r.ConversationID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	check("retest initial message", detail+r.InitialMessage())
	contextTool := workflowTool(t, a.s.findingRetestTools(), "get_finding_retest_context")
	out = workflowCall(t, intercept.WithConvID(ctx, *r.ConversationID), contextTool, map[string]any{}, false)
	if !strings.Contains(out, `"vulnclass"`) {
		t.Fatalf("retest context lost the finding: %s", out)
	}
	check("get_finding_retest_context", out)
}

// ---- the finding row and the catalog -----------------------------------------------------------------------------

// findingColumnsBeforeDetections is the column list of `findings` as it was before migration 5003 (I6): the
// migration adds no column to the table.
var findingColumnsBeforeDetections = []string{"asset_ids", "created_at", "evidence", "evidence_version", "id", "name", "node_id",
	"report", "report_evidence_version", "severity", "status", "summary", "task_id", "ticket_id", "vulnclass", "worker"}

func TestDetectionAddsNoFindingColumnOrField(t *testing.T) {
	a := newTicketAPI(t)
	rows, err := a.s.m.pg.Query(`SELECT column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='findings' ORDER BY column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), findingColumnsBeforeDetections...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("columns of findings = %v, want %v (migration 5003 adds none)", got, want)
	}

	// DBFinding, the struct every finding query fills and chat mentions serialise whole, has no detection field.
	typ := reflect.TypeOf(db.DBFinding{})
	for i := 0; i < typ.NumField(); i++ {
		if name := typ.Field(i).Name; strings.Contains(strings.ToLower(name), "detect") || strings.Contains(strings.ToLower(name), "rule") {
			t.Errorf("DBFinding has the field %s", name)
		}
	}
	// findingSelectCols (unexported: read from the source) names no detection table or column.
	src := goSources(t, moduleRoot(t), "db")["db/findings.go"]
	i := strings.Index(src, "const findingSelectCols")
	if i < 0 {
		t.Fatal("findingSelectCols not found in db/findings.go: the guard needs updating")
	}
	end := strings.Index(src[i:], "`\n\n")
	if end < 0 {
		t.Fatal("end of findingSelectCols not found")
	}
	if block := strings.ToLower(src[i : i+end]); strings.Contains(block, "detect") || len(mentionsDetectionTable(block)) > 0 {
		t.Errorf("findingSelectCols reads detection data:\n%s", block)
	}
}

// ---- source guards -----------------------------------------------------------------------------------------------

// Only the database layer of the feature may name the four tables. The allowlist is closed on purpose: a new reader
// (in 1b the engine's loader, in 3 the validation backend) is a decision that adds its file here, with its reason,
// and that must not be a file an agent can reach.
var detectionTableReaders = map[string]string{
	"db/finding_detections.go": "the detection database layer",
}

func TestDetectionTablesAreNamedOnlyByTheirDatabaseLayer(t *testing.T) {
	root := moduleRoot(t)
	all := goSources(t, root, ".")
	for file, text := range all {
		if names := mentionsDetectionTable(text); len(names) > 0 {
			if _, ok := detectionTableReaders[file]; !ok {
				t.Errorf("%s names %v: only %v may (add a file to the allowlist only after checking that no agent-facing path reaches it)", file, names, keys(detectionTableReaders))
			}
		}
	}
	if _, ok := all["db/finding_detections.go"]; !ok {
		t.Fatal("the allowlisted file is missing: the guard needs updating")
	}
}

// No file under agent/ and no tool file of server/ (a file that defines tools: it imports the tool package) names
// the tables or the feature's database API (rows 1, 3 and 7 of section 11).
func TestDetectionNoAgentOrToolFileTouchesTheTables(t *testing.T) {
	root := moduleRoot(t)
	apiNames := regexp.MustCompile(`\b(CreateFindingDetection|ClaimFindingDetection|FindingDetectionSnapshot|ListFindingDetections|GetFindingDetection|ListDetectionRules|ListFindingDetectionRules|ListDetectionSteps|RecordDetectionRule|FinishFindingDetection|FindDetectionQueryResult|DetectionRule|DetectionStep|FindingDetection)\b`)
	agentFiles := goSources(t, root, "agent")
	toolFiles := 0
	files := map[string]string{}
	for f, text := range agentFiles {
		files[f] = text
	}
	for f, text := range goSources(t, root, "server") {
		if strings.Contains(text, `"github.com/Autumn-27/norma/tool"`) {
			files[f] = text
			toolFiles++
		}
	}
	if len(agentFiles) < 10 || toolFiles < 3 {
		t.Fatalf("the guard sees %d agent files and %d server tool files: it would be vacuous", len(agentFiles), toolFiles)
	}
	for file, text := range files {
		if names := mentionsDetectionTable(text); len(names) > 0 {
			t.Errorf("%s names the detection tables %v", file, names)
		}
		if m := apiNames.FindString(text); m != "" {
			t.Errorf("%s uses the detection database API (%s)", file, m)
		}
	}
}

// The tools and agents tables get no row from starting a run (the detector has no tool row and no agent row in
// this milestone), on either path.
func TestDetectionStartAddsNoToolOrAgentRow(t *testing.T) {
	a := newTicketAPI(t)
	pg := a.s.m.pg
	count := func(table string) int {
		t.Helper()
		var n int
		if err := pg.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := [2]int{count("tools"), count("agents")}
	ctx := context.Background()
	run, _, created, err := pg.CreateFindingDetection(ctx, db.CreateDetectionInput{FindingID: a.finding, Path: "engine"})
	if err != nil || !created {
		t.Fatalf("engine start: created=%t err=%v", created, err)
	}
	if _, err := pg.StopFindingDetection(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, created, err := pg.CreateFindingDetection(ctx, db.CreateDetectionInput{FindingID: a.finding, Path: "agent"}); err != nil || !created {
		t.Fatalf("agent start: created=%t err=%v", created, err)
	}
	if after := [2]int{count("tools"), count("agents")}; after != before {
		t.Errorf("rows in tools and agents: %v before, %v after a start", before, after)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- the diff guard (1a-18) -----------------------------------------------------------------------------------------

// forbiddenInDiff returns the files of a diff that the detection work must not touch (invariant I9): the schema
// files, third_party, agent/ (the planner, the workers and the data envelope), and web/.
func forbiddenInDiff(files []string) []string {
	var bad []string
	for _, f := range files {
		f = filepath.ToSlash(strings.TrimSpace(f))
		switch {
		case f == "db/db.go", f == "db/migrate.go", f == "db/schema.sql",
			strings.HasPrefix(f, "third_party/"), strings.HasPrefix(f, "agent/"), strings.HasPrefix(f, "web/"):
			bad = append(bad, f)
		}
	}
	return bad
}

func TestDetectionDiffGuardNamesTheForbiddenFiles(t *testing.T) {
	ok := []string{"db/finding_detections.go", "server/detection_noflow_test.go", "db/migrations/5003_finding_detections.sql", "db/migrate_test.go", "agentx/x.go", "webhook/a.go"}
	if got := forbiddenInDiff(ok); len(got) != 0 {
		t.Errorf("allowed files flagged: %v", got)
	}
	bad := []string{"db/db.go", "db/migrate.go", "db/schema.sql", "third_party/x/y.go", "agent/planner.go", "agent/data_envelope.go", "web/src/app.js"}
	if got := forbiddenInDiff(append(append([]string{}, ok...), bad...)); !reflect.DeepEqual(got, bad) {
		t.Errorf("forbidden files = %v, want %v", got, bad)
	}
}

// On the branches of the detection work, the diff against main lists none of the forbidden files. Without git
// history (the test container exports the files only) it is skipped, and the gate runs the same command on the
// host: git diff --name-only main...HEAD.
func TestDetectionDiffAgainstMainTouchesNoForbiddenFile(t *testing.T) {
	root := moduleRoot(t)
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	branch, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) || errors.Is(err, exec.ErrNotFound) {
			t.Skipf("no git history here (%v): run `git diff --name-only main...HEAD` on the host", err)
		}
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^feature/detections-(storage|engine)$`).MatchString(branch) {
		t.Skipf("branch %q is not a detection work branch (PR 1a, PR 1b)", branch)
	}
	out, err := git("diff", "--name-only", "main...HEAD")
	if err != nil {
		t.Fatalf("git diff main...HEAD: %v", err)
	}
	files := strings.Split(out, "\n")
	if out == "" || len(files) < 5 {
		t.Fatalf("the diff against main lists %d files: the guard would be vacuous", len(files))
	}
	if bad := forbiddenInDiff(files); len(bad) > 0 {
		t.Errorf("the diff against main touches %s", fmt.Sprint(bad))
	}
}

// ---- the archive error mapping (S3) --------------------------------------------------------------------------------

// The archive handlers answer a task with detection results with 409 and the text of section 6.10, also when the
// error is wrapped, so that the operator sees why (an unmapped error would be a 500).
func TestArchiveHandlersMapTheDetectionRefusalTo409(t *testing.T) {
	const message = "this task has detection results, which task archives do not carry yet; delete them from the findings or upgrade to a release that archives them"
	for name, err := range map[string]error{"plain": db.ErrTaskArchiveDetections, "wrapped": fmt.Errorf("purge: %w", db.ErrTaskArchiveDetections)} {
		w := httptest.NewRecorder()
		writeArchiveError(w, err)
		if w.Code != http.StatusConflict {
			t.Errorf("%s: status %d, want 409", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), message) {
			t.Errorf("%s: body %q lacks the message", name, w.Body.String())
		}
	}
}

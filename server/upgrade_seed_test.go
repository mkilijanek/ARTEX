package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// AC9 (docs/phase-d2-plan.md section 3): the startup writers outside schema.sql, run on a
// database of shape r0509 that holds the state a real 0.5.10m instance has.
//
// How the fixture state is made. The schema fixture of shape r0509 and the Go DDL are loaded
// into a temporary database; then the startup of this commit runs once (it writes the
// agents, prompts, tools, settings flags and intercept rules); then seed-r0509.sql rewinds the
// two things 0.5.10m lacked (finding_comments, the source property) and adds the operator's
// edits. The rows are read per table and key, the startup runs again, and the changes are
// compared with the exact lists below. No value is ever printed.
//
// What this is evidence of: the CURRENT seeders run on a state rewound to 0.5.10m. It is not
// a database created by 0.5.10m. The startup writers of this commit are not those of v0.5.10m
// (seedFactSourceSchema was added in server/orchestration.go, agent/promptcatalog.go changed
// the default prompt texts, besides the module path; see the header of seed-r0509.sql), so the
// rows the first run writes are the rows of this commit's seeders. The real-binary gap is
// closed only by the dry run on a copy of a real database (docs/phase-d2-plan.md AC15).

const (
	upgradeFixtures = "../db/testdata/upgrade"
	r0509Schema     = "schema-b4b9600f5c8f.sql"
	goDDLFixture    = "goddl-4bfc9dbe09c3.sql"
	flagsFixture    = "flags-v0.5.10m.txt"
	seedFixture     = "testdata/upgrade/seed-r0509.sql"
)

// startupFlagKeys is every settings key a startup writer uses as a one-time gate or marker,
// with the line that writes it. flags-<tag>.txt lists only the first 19 (the
// `const flag = "..."` keys of server/); the other six are written by db.Open and by
// seedFindingWorkflowTools.
var startupFlagKeys = map[string]string{
	"tool_schema_refresh_v7_list_facts_paging":          "server/orchestration.go:568",
	"goal_met_unbind_default_v1":                        "server/orchestration.go:606",
	"goals_prompt_constraint_step_v1":                   "server/orchestration.go:623",
	"mainagent_prompt_goalless_intent_v1":               "server/orchestration.go:653",
	"planner_prompt_compact_realistic_v2":               "server/orchestration.go:684",
	"worker_prompt_compact_v4":                          "server/orchestration.go:714",
	"reporter_trigger_evidence_version_v1":              "server/orchestration.go:759",
	"reporter_agent_seed_v1":                            "server/orchestration.go:787",
	"auto_report_finding_v1":                            "server/orchestration.go:836",
	"planner_report_finding_v1":                         "server/orchestration.go:852",
	"planner_list_assets_v1":                            "server/orchestration.go:869",
	"company_scope_rebind_v1":                           "server/orchestration.go:887",
	"worker_readtools_unbind_v1":                        "server/orchestration.go:915",
	"task_credential_bindings_v1":                       "server/orchestration.go:941",
	"worker_readback_rebind_v2":                         "server/orchestration.go:955",
	"auto_default_bindings_v3":                          "server/orchestration.go:973",
	"finding_workflow_tools_v2_reporter":                "server/finding_workflow.go:32",
	"finding_traffic_tools_v1":                          "server/finding_traffic.go:30",
	"finding_retester_seed_v1":                          "server/finding_retests.go:202",
	"finding_workflow_tools_v3_host_search_description": "server/finding_workflow.go:18",
	"interactive_shell_default_v1":                      "db/db.go:286",
	"asset_intercept_default_rules_v1":                  "db/db.go:331",
	"intercept_default_rules_v1":                        "db/db.go:387",
	"intercept_default_rules_v2":                        "db/db.go:582",
	"intercept_default_rules_v3":                        "db/db.go:640",
}

// pythonInterpreterKey is written by seedPythonInterpreter (server/customtool.go:215) only
// when empty; the fixture sets it so the host's interpreter cannot change the result.
const pythonInterpreterKey = "python_interpreter"

func fixtureText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture %s: %v", filepath.Base(path), err)
	}
	return string(b)
}

// runStartup does what the server does at startup, in the order of server/manager.go:379-397
// (Open, RecoverFindingRetests, EnsureLLM*Table) and server/server.go:237-243 (wireTools,
// seedPrompts, seedOrchestrationTools, seedFindingRetester, seedPythonInterpreter). Errors the
// server only logs fail the test here. It uses the production functions only.
func runStartup(t *testing.T, dsn string) {
	t.Helper()
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("startup: Open: %v", err)
	}
	defer pg.Close()
	if err := pg.RecoverFindingRetests(); err != nil {
		t.Fatalf("startup: RecoverFindingRetests: %v", err)
	}
	if err := pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatalf("startup: EnsureLLMRecordsTable: %v", err)
	}
	if err := pg.EnsureLLMUsageTable(); err != nil {
		t.Fatalf("startup: EnsureLLMUsageTable: %v", err)
	}
	m := &Manager{pg: pg, assets: pg.Assets()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{m: m, ctx: ctx}
	prevResolve := agent.ToolResolve()
	defer agent.SetToolResolve(prevResolve)
	wireTools(pg, buildDomainReg(m.Assets()))
	seedPrompts(pg)
	s.seedOrchestrationTools()
	if err := s.seedFindingRetester(); err != nil {
		t.Fatalf("startup: seedFindingRetester: %v", err)
	}
	s.seedPythonInterpreter()
}

// r0509Instance builds the database of an existing 0.5.10m instance (see the file comment).
func r0509Instance(t *testing.T, label string) *dbtest.Temp {
	t.Helper()
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, label)
	tmp.Load(t, "schema fixture "+r0509Schema, fixtureText(t, filepath.Join(upgradeFixtures, r0509Schema)))
	tmp.Load(t, "Go DDL fixture "+goDDLFixture, fixtureText(t, filepath.Join(upgradeFixtures, goDDLFixture)))
	runStartup(t, tmp.DSN) // what the 0.5.10m startup left behind
	tmp.Load(t, "operator state "+seedFixture, fixtureText(t, seedFixture))
	return tmp
}

// observation is the content-free state of a database at one moment: the rows (dbtest.TakeRows,
// the implementation the upgrade matrix uses) and the digests with the sequence values.
type observation struct {
	rows   *dbtest.RowSnapshot
	digest *dbtest.Digests
}

func observe(t *testing.T, tmp *dbtest.Temp) observation {
	t.Helper()
	conn := tmp.Conn(t)
	ctx := context.Background()
	rows, err := dbtest.TakeRows(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	dg, err := dbtest.TakeDigests(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	return observation{rows: rows, digest: dg}
}

// cell is a column of a row as text; "" for NULL or a column the row does not have.
func cell(row map[string]*string, col string) string {
	if v := row[col]; v != nil {
		return *v
	}
	return ""
}

// rowLabel names a row for an entry: agents, their prompts and prompt variables by the agent
// key (a serial id differs from run to run), tools by key, any other row by its primary key
// as dbtest describes it (integers as they are, text as a hash). The label is structure, not
// row content. The row is looked up before, then after.
func rowLabel(before, after *dbtest.RowSnapshot, table, key string) string {
	for _, snap := range []*dbtest.RowSnapshot{before, after} {
		tbl := snap.Tables[table]
		if tbl == nil {
			continue
		}
		row, ok := tbl.Rows[key]
		if !ok {
			continue
		}
		agentKey := func() string {
			if a := snap.Tables["agents"]; a != nil {
				return cell(a.Rows[cell(row, "agent_id")], "key")
			}
			return ""
		}
		switch table {
		case "agents":
			return cell(row, "key")
		case "agent_prompt_vars":
			return agentKey() + "/" + cell(row, "var_name")
		case "agent_prompts":
			return agentKey() + "/v" + cell(row, "version")
		case "tools":
			return fmt.Sprintf("key=%q", cell(row, "key"))
		}
		return tbl.DescribeKey(key)
	}
	return "?"
}

// entries renders the changes between two observations as exact strings, one per changed
// column, deleted or added row, and table that appears or disappears. A table that appears
// is "table-added" and, for each row it holds, "added"; a table that disappears is
// "table-gone" only.
func entries(before, after observation) []string {
	b, a := before.rows, after.rows
	var out []string
	for _, n := range a.TableNames() {
		if b.Tables[n] == nil {
			out = append(out, "table-added "+n)
		}
	}
	for _, n := range b.TableNames() {
		if a.Tables[n] == nil {
			out = append(out, "table-gone "+n)
		}
	}
	for _, d := range dbtest.DiffRows(b, a) {
		if a.Tables[d.Table] == nil {
			continue // table-gone above
		}
		switch d.Kind {
		case dbtest.DiffColumnRemoved:
			out = append(out, fmt.Sprintf("column-removed %s %s", d.Table, d.Column))
		case dbtest.DiffAdded, dbtest.DiffDeleted:
			out = append(out, fmt.Sprintf("%s %s [%s]", d.Kind, d.Table, rowLabel(b, a, d.Table, d.Key)))
		default:
			out = append(out, fmt.Sprintf("%s %s [%s] %s", d.Kind, d.Table, rowLabel(b, a, d.Table, d.Key), d.Column))
		}
	}
	sort.Strings(out)
	return out
}

// sequenceMoves names the sequences whose last_value changed (or that appeared) between two
// observations. A sequence gap is not a row change; it is listed so that it is not a surprise.
func sequenceMoves(before, after observation) []string {
	var out []string
	for _, line := range dbtest.DiffDigests(before.digest, after.digest) {
		if rest, ok := strings.CutPrefix(line, "sequence "); ok {
			name, _, _ := strings.Cut(rest, ":")
			out = append(out, name)
		}
	}
	return out
}

func assertEntries(t *testing.T, what string, got, want []string) {
	t.Helper()
	w := append([]string(nil), want...)
	sort.Strings(w)
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	wantSet := map[string]bool{}
	for _, x := range w {
		wantSet[x] = true
	}
	var unexpected, missing []string
	for _, g := range got {
		if !wantSet[g] {
			unexpected = append(unexpected, g)
		}
	}
	for _, x := range w {
		if !have[x] {
			missing = append(missing, x)
		}
	}
	if len(unexpected) > 0 || len(missing) > 0 {
		t.Errorf("%s: %d change(s), want %d\nunexpected:\n  %s\nmissing:\n  %s",
			what, len(got), len(w), strings.Join(unexpected, "\n  "), strings.Join(missing, "\n  "))
	}
}

var builtinAgentKeys = []string{"goals", "planner", "mainagent", "worker", "auto", "pentest"}

// builtinUpdatedAt is one entry per built-in agent: seedBuiltins runs
// `INSERT ... ON CONFLICT (key) DO UPDATE` (db/db.go:256-258), which is an UPDATE of the
// row on every start, and the trigger trg_agents_upd (schema.sql, set_updated_at) stamps
// updated_at. Plan invariant I3 allows exactly this change.
func builtinUpdatedAt() []string {
	var out []string
	for _, k := range builtinAgentKeys {
		out = append(out, fmt.Sprintf("value agents [%s] updated_at", k))
	}
	return out
}

func TestUpgradeSeedFixtureHoldsTheOperatorState(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	tmp := r0509Instance(t, "seedfix")
	conn := tmp.Conn(t)
	ctx := context.Background()

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := conn.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("precondition query failed")
		}
		return n
	}
	// the six built-in agents, one of them renamed and re-described by the operator
	if n := count(`SELECT count(*) FROM agents WHERE key = ANY($1) AND builtin`, builtinAgentKeys); n != 6 {
		t.Errorf("built-in agents in the fixture: %d, want 6", n)
	}
	if n := count(`SELECT count(*) FROM agents WHERE key='planner' AND name LIKE $1 AND description LIKE $1`, dbtest.RowCanary+"%"); n != 1 {
		t.Errorf("the renamed planner is not in the fixture")
	}
	// the stale prompt variables
	for _, v := range []string{"EngagementTitle", "CoverageGaps", "Now"} {
		if n := count(`SELECT count(*) FROM agent_prompt_vars WHERE var_name=$1`, v); n != 1 {
			t.Errorf("agent_prompt_vars rows named %s: %d, want 1", v, n)
		}
	}
	// record_fact: edited description, no source property
	if n := count(`SELECT count(*) FROM tools WHERE key='record_fact' AND description LIKE $1 AND NOT (schema->'properties' ? 'source')`, dbtest.RowCanary+"%"); n != 1 {
		t.Errorf("record_fact is not in its 0.5.10m shape")
	}
	// operator-edited prompts
	if n := count(`SELECT count(*) FROM agent_prompts p JOIN agents a ON a.current_prompt_id = p.id WHERE p.template_text LIKE $1`, dbtest.RowCanary+"%"); n != 3 {
		t.Errorf("operator-edited current prompts: %d, want 3", n)
	}
	// interrupted retests
	for _, st := range []string{"pending", "running"} {
		if n := count(`SELECT count(*) FROM finding_retests WHERE status=$1`, st); n != 1 {
			t.Errorf("finding_retests in %s: %d, want 1", st, n)
		}
	}
	// the r0509 table set: no finding_comments
	if n := count(`SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tablename='finding_comments'`); n != 0 {
		t.Errorf("finding_comments exists in the fixture")
	}

	// settings: exactly the startup flags plus the interpreter path, every flag set
	keys := map[string]string{}
	r, err := conn.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		t.Fatal("settings query failed")
	}
	for r.Next() {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			r.Close()
			t.Fatal("settings scan failed")
		}
		keys[k] = v
	}
	r.Close()
	for k := range startupFlagKeys {
		if v, ok := keys[k]; !ok || (v != "true" && v != "done") {
			t.Errorf("startup flag %s is not set in the fixture", k)
		}
	}
	var extra []string
	for k := range keys {
		if _, ok := startupFlagKeys[k]; !ok && k != pythonInterpreterKey {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("settings keys written by startup that startupFlagKeys does not list: %s", strings.Join(extra, ", "))
	}
	// flags-v0.5.10m.txt is the 19-key server list; all of them are in startupFlagKeys
	for _, line := range strings.Fields(fixtureText(t, filepath.Join(upgradeFixtures, flagsFixture))) {
		if _, ok := startupFlagKeys[line]; !ok {
			t.Errorf("%s lists %s, which startupFlagKeys lacks", flagsFixture, line)
		}
	}
	// startupFlagKeys is the flags file (19 keys) and the six keys flags-README.txt lists, with
	// the same file:line, and nothing else.
	readme := map[string]string{}
	for _, line := range strings.Split(fixtureText(t, filepath.Join(upgradeFixtures, "flags-README.txt")), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.Contains(f[1], ".go:") {
			readme[f[0]] = f[1]
		}
	}
	listed := map[string]bool{}
	for _, k := range strings.Fields(fixtureText(t, filepath.Join(upgradeFixtures, flagsFixture))) {
		listed[k] = true
	}
	for k, where := range readme {
		if startupFlagKeys[k] != where {
			t.Errorf("flags-README.txt lists %s at %s; startupFlagKeys says %q", k, where, startupFlagKeys[k])
		}
		listed[k] = true
	}
	for k := range startupFlagKeys {
		if !listed[k] {
			t.Errorf("startupFlagKeys has %s, which neither %s nor flags-README.txt lists", k, flagsFixture)
		}
	}
	dbtest.AssertNoSecret(t, "startup log", logs())
}

// Existing 0.5.10m instance, flag rows present: the exact changes of the first run, and a
// second run that changes nothing but agents.updated_at.
func TestUpgradeSeedWritersOnExistingInstanceTwice(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	tmp := r0509Instance(t, "seedtwice")

	before := observe(t, tmp)
	runStartup(t, tmp.DSN)
	first := observe(t, tmp)
	runStartup(t, tmp.DSN)
	second := observe(t, tmp)

	// First run. Each entry names its cause.
	want := append(builtinUpdatedAt(),
		// schema.sql: CREATE TABLE IF NOT EXISTS finding_comments (the only table 0.5.10m lacks).
		"table-added finding_comments",
		// db/db.go:256-258: ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description =
		// EXCLUDED.description overwrites the operator's rename and description on every start.
		// OPERATOR-EDITED. Accepted by the owner on 2026-11-19 (backlog item): the product does not
		// let a built-in agent be renamed (db/config.go:610), so only an out-of-band SQL edit is
		// reverted.
		"value agents [planner] name",
		"value agents [planner] description",
		// db/db.go:280: DELETE FROM agent_prompt_vars WHERE var_name IN ('EngagementTitle',
		// 'CoverageGaps','Now'). Stale catalog entries, not operator data in the intended design.
		"deleted agent_prompt_vars [goals/EngagementTitle]",
		"deleted agent_prompt_vars [goals/Now]",
		"deleted agent_prompt_vars [planner/CoverageGaps]",
		// db/finding_retests.go:261 (RecoverFindingRetests): pending and running -> stopped.
		"value finding_retests [id=9101] status",
		"value finding_retests [id=9101] error",
		"value finding_retests [id=9101] finished_at",
		"value finding_retests [id=9102] status",
		"value finding_retests [id=9102] error",
		"value finding_retests [id=9102] finished_at",
		// server/orchestration.go:557-558 (seedFactSourceSchema): jsonb_set adds the source
		// property; the trigger trg_tools_upd stamps updated_at. The operator's description stays.
		"value tools [key=\"record_fact\"] schema",
		"value tools [key=\"record_fact\"] updated_at",
	)
	assertEntries(t, "first run", entries(before, first), want)
	// Sequences: `INSERT ... ON CONFLICT` consumes a value even when it updates or does nothing
	// (db/db.go:256 agents, :264 agent_prompt_vars, :298 mcp_servers), and finding_comments is new.
	moves := []string{"agent_prompt_vars_id_seq", "agents_id_seq", "finding_comments_id_seq", "mcp_servers_id_seq"}
	assertEntries(t, "first run, sequences", sequenceMoves(before, first), moves)

	// Second run: only the updated_at stamps of the built-in agents (db/db.go:256-258 and the
	// trigger), no other row, no other table.
	assertEntries(t, "second run", entries(first, second), builtinUpdatedAt())
	assertEntries(t, "second run, sequences", sequenceMoves(first, second), []string{"agent_prompt_vars_id_seq", "agents_id_seq", "mcp_servers_id_seq"})

	// Operator edits that the plan says must survive, checked by behaviour.
	conn := tmp.Conn(t)
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM tools WHERE key='record_fact' AND description LIKE $1 AND schema->'properties' ? 'source'`, dbtest.RowCanary+"%").Scan(&n); err != nil || n != 1 {
		t.Errorf("record_fact after startup: operator description kept and source added: rows=%d err=%v", n, err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM agent_prompts WHERE template_text LIKE $1`, dbtest.RowCanary+"%").Scan(&n); err != nil || n != 3 {
		t.Errorf("operator-edited prompt rows after startup: %d, want 3 (err=%v)", n, err)
	}
	dbtest.AssertNoSecret(t, "startup log", logs())
}

// NOT what an existing database sees. A database without the settings flag rows is a
// first-time run: every one-time seeder runs. This is what an operator would see only if the
// flag rows were lost (for example a restore that skipped `settings`); the entries below are
// asserted as observed, and none of them is an upgrade behaviour of a real 0.5.10m instance.
func TestUpgradeSeedWritersWithoutFlagRowsFirstTimeBehaviourNotExistingDatabase(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	tmp := r0509Instance(t, "seednoflag")
	conn := tmp.Conn(t)
	keys := make([]string, 0, len(startupFlagKeys))
	for k := range startupFlagKeys {
		keys = append(keys, k)
	}
	res, err := conn.ExecContext(context.Background(), `DELETE FROM settings WHERE key = ANY($1)`, keys)
	if err != nil {
		t.Fatal("deleting the flag rows failed")
	}
	if n, _ := res.RowsAffected(); int(n) != len(startupFlagKeys) {
		t.Fatalf("deleted %d flag rows, want %d", n, len(startupFlagKeys))
	}

	before := observe(t, tmp)
	runStartup(t, tmp.DSN)
	first := observe(t, tmp)
	runStartup(t, tmp.DSN)
	second := observe(t, tmp)

	got := entries(before, first)
	t.Logf("first-time run without flag rows: %d change(s)", len(got))
	for _, e := range got {
		t.Logf("    %s", e)
	}
	for _, line := range summarise(got) {
		t.Logf("  %s", line)
	}
	assertEntries(t, "first-time run summary", summarise(got), unflaggedSummary)
	// Even from this state a second run settles.
	assertEntries(t, "second run after the first-time run", entries(first, second), builtinUpdatedAt())
	dbtest.AssertNoSecret(t, "startup log", logs())
}

// summarise reduces entries to kind, table and column with a count, so the first-time
// behaviour is asserted exactly without listing every tool row.
func summarise(entries []string) []string {
	counts := map[string]int{}
	for _, e := range entries {
		kind, rest, _ := strings.Cut(e, " ")
		table, rest, _ := strings.Cut(rest, " ")
		col := ""
		if i := strings.LastIndex(rest, "] "); i >= 0 {
			col = " " + rest[i+2:]
		}
		counts[kind+" "+table+col]++
	}
	out := make([]string, 0, len(counts))
	for k, n := range counts {
		out = append(out, fmt.Sprintf("%s x%d", k, n))
	}
	sort.Strings(out)
	return out
}

// unflaggedSummary is the observed first-time behaviour on the r0509 instance after the flag
// rows are removed (NOT what an existing database sees). Causes:
//   - added settings x25: the flag rows themselves are written again.
//   - added intercept_rules x19, asset_intercept_rules x4: db/db.go:387, :582, :640, :331 insert
//     the default rules again; their ON CONFLICT DO NOTHING matched none of the rows that are
//     still present, so they are additional rows (an operator's earlier delete of a default
//     rule would be resurrected the same way).
//   - added agent_prompts x3 (goals, planner, worker v2), value agents current_prompt_id x3:
//     server/orchestration.go:623, :684, :714 append a new default version and make it
//     current. OPERATOR-EDITED: the operator's current prompt stops being current (it stays
//     in history). An existing database does not do this.
//   - value tools updated_at x29, schema x1: server/orchestration.go:568-596 refresh and
//     :557 seedFactSourceSchema (schema of record_fact); the other 28 rows get only the
//     trigger stamp (their text already equals the code default).
//   - the remaining entries are the same as in the existing-instance test.
var unflaggedSummary = []string{
	"added agent_prompts x3",
	"added asset_intercept_rules x4",
	"added intercept_rules x19",
	"added settings x25",
	"deleted agent_prompt_vars x3",
	"table-added finding_comments x1",
	"value agents current_prompt_id x3",
	"value agents description x1",
	"value agents name x1",
	"value agents updated_at x6",
	"value finding_retests error x2",
	"value finding_retests finished_at x2",
	"value finding_retests status x2",
	"value tools schema x1",
	"value tools updated_at x29",
}

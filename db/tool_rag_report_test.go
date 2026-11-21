package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Tests of scripts/toolrag/report.sql (Phase D.3, pull request 2). The file is split into its statements on
// the "-- [id] title" comment lines; psql's :'t0' is replaced by a quoted literal (the Go driver does not
// know psql variables) and the statements run in order on one connection, BEGIN READ ONLY and ROLLBACK
// included. The fixture sits in the year 2100 and the window starts in 2099, so rows that other tests leave in
// the shared database (stamped with the real clock) fall outside every query.

const (
	reportFile   = "../scripts/toolrag/report.sql"
	reportT0     = "2099-12-31 00:00:00+00"
	reportBase   = "2100-01-01 00:00:00+00"
	reportMarker = `^-- \[([0-9A-Za-z.]+)\] (.*)$`
)

type reportStmt struct {
	id, title, sql string
}

var reportMarkerRe = regexp.MustCompile(reportMarker)

func readReport(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(reportFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parseReport splits the file into statements. A statement ends at a line that ends with a semicolon; comment
// lines and blank lines are dropped; the last marker comment names the statement. The opening BEGIN READ ONLY
// and the closing ROLLBACK get the empty id.
func parseReport(t *testing.T, src string) []reportStmt {
	t.Helper()
	var stmts []reportStmt
	var id, title string
	var buf []string
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "--"):
			if len(buf) > 0 {
				t.Fatalf("a comment line inside a statement: %q", trim)
			}
			if m := reportMarkerRe.FindStringSubmatch(trim); m != nil {
				id, title = m[1], m[2]
			}
		case trim == "":
		default:
			buf = append(buf, line)
			if strings.HasSuffix(trim, ";") {
				text := strings.Join(buf, "\n")
				buf = nil
				s := reportStmt{id: id, title: title, sql: text}
				switch strings.ToUpper(strings.TrimSuffix(trim, ";")) {
				case "BEGIN READ ONLY", "ROLLBACK":
					if text == line {
						s.id, s.title = "", ""
					}
				}
				stmts = append(stmts, s)
			}
		}
	}
	if len(buf) > 0 {
		t.Fatalf("the file ends inside a statement: %q", buf)
	}
	return stmts
}

// psqlVar is the only psql variable the report uses.
const psqlVar = ":'t0'"

func substituteT0(sqlText, t0 string) string {
	return strings.ReplaceAll(sqlText, psqlVar, "'"+t0+"'")
}

type reportResult struct {
	cols []string
	rows [][]string
}

// runReport executes every statement of the file on one connection, in file order, and returns the result
// rows of the sections by id. NULL is shown as "NULL".
func runReport(t *testing.T, d *DB, t0 string) map[string]reportResult {
	t.Helper()
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	out := map[string]reportResult{}
	stmts := parseReport(t, readReport(t))
	fail := func(s reportStmt, err error) {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		t.Fatalf("section %q (%s): %v", s.id, s.title, err)
	}
	for _, s := range stmts {
		text := substituteT0(s.sql, t0)
		if s.id == "" {
			if _, err := conn.ExecContext(ctx, text); err != nil {
				fail(s, err)
			}
			continue
		}
		if _, dup := out[s.id]; dup {
			t.Fatalf("section id %q is used twice", s.id)
		}
		rows, err := conn.QueryContext(ctx, text)
		if err != nil {
			fail(s, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			fail(s, err)
		}
		res := reportResult{cols: cols, rows: [][]string{}}
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				fail(s, err)
			}
			row := make([]string, len(cols))
			for i, v := range vals {
				row[i] = "NULL"
				if v.Valid {
					row[i] = v.String
				}
			}
			res.rows = append(res.rows, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			fail(s, err)
		}
		rows.Close()
		out[s.id] = res
	}
	return out
}

func wantRows(t *testing.T, got map[string]reportResult, id string, want ...[]string) {
	t.Helper()
	res, ok := got[id]
	if !ok {
		t.Fatalf("section %s did not run", id)
	}
	if want == nil {
		want = [][]string{}
	}
	if !reflect.DeepEqual(res.rows, want) {
		t.Errorf("section %s\ncolumns: %v\n got: %q\nwant: %q", id, res.cols, res.rows, want)
	}
}

func row(cells ...string) []string { return cells }

// Fixture helpers.

func candNames(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("c%02d", i))
	}
	return out
}

func rcall(n int, class string) ToolRAGCall { return ToolRAGCall{N: n, Class: class} }

type reportFixture struct {
	t      *testing.T
	d      *DB
	t1, t2 int64
}

func (f *reportFixture) obs(task int64, agent string, intent int64, outcome string, host, keep, cand, nz, hide, def, lock, unl int,
	ranking []string, called map[string]ToolRAGCall) {
	f.t.Helper()
	o := &ToolRAGObservation{
		TaskID: task, IntentID: intent, AgentKey: agent, Scorer: "bm25-v1", Mode: "observe", Outcome: outcome,
		K: 12, HostListCount: host, KeepCount: keep, CandidateCount: cand, NonzeroCount: nz, WouldHideCount: hide,
		DeferredCount: def, LockedAtStart: lock, UnlockedDuringRun: unl, QueryTokens: 5, DurationUS: 100,
		Ranking: ranking, Called: called,
	}
	if err := f.d.InsertToolRAGObservation(context.Background(), o); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reportFixture) llm(task int64, worker string, intent int64, model string, toolBytes map[string]ToolBytes) {
	f.t.Helper()
	if err := f.d.InsertLLMUsage(&LLMUsage{
		TaskID: strconv.FormatInt(task, 10), Worker: worker, IntentID: intent, Model: model, Status: "ok",
		ToolBytes: toolBytes,
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reportFixture) usage(task int64, agent string, intent int64, tool string, n int, session string) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if err := f.d.InsertToolUsage(&ToolUsage{ToolKey: tool, AgentKey: agent, TaskID: task, IntentID: intent, SessionID: session}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *reportFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// newReportFixture loads the deterministic data set described in the comments of each test: 13 observation
// rows of two tasks (R1..R13, inserted in this order), the llm_usage rows and the tool_usage rows the sections
// join. The rows are stamped one minute apart from 2100-01-01 00:00 UTC (the order of insertion), except two
// rows named "outside" that keep the real clock and must stay outside the window.
func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	d := toolRagDB(t)
	k1 := toolRagTask(t, d, "report one")
	k2 := toolRagTask(t, d, "report two")
	f := &reportFixture{t: t, d: d, t1: k1.ID, t2: k2.ID}
	t.Cleanup(func() {
		task1, task2 := strconv.FormatInt(f.t1, 10), strconv.FormatInt(f.t2, 10)
		if _, err := d.Exec(`DELETE FROM llm_usage WHERE task_id IN ($1,$2)`, task1, task2); err != nil {
			t.Errorf("clean llm_usage: %v", err)
		}
		if _, err := d.Exec(`DELETE FROM tool_usage WHERE task_id IN ($1,$2)`, f.t1, f.t2); err != nil {
			t.Errorf("clean tool_usage: %v", err)
		}
		d.DeleteTask(f.t1)
		d.DeleteTask(f.t2)
	})
	t1, t2 := f.t1, f.t2

	// R1..R13. Names c01.. are candidates; k is 12 in every row.
	f.obs(t1, "worker", 101, "projected", 24, 10, 14, 9, 2, 3, 2, 1, candNames(1, 14), map[string]ToolRAGCall{
		"c03": rcall(2, "candidate"), "c14": rcall(1, "candidate"), "Skill": rcall(1, "keep"), "mcp__x__a": rcall(2, "deferred_global")})
	f.obs(t1, "worker", 102, "projected", 24, 10, 14, 9, 2, 0, 0, 0, candNames(1, 14), map[string]ToolRAGCall{
		"c01": rcall(1, "candidate"), "c05": rcall(3, "candidate")})
	f.obs(t1, "worker", 101, "projected", 24, 10, 14, 9, 2, 3, 2, 2, candNames(1, 14), map[string]ToolRAGCall{
		"c02": rcall(1, "candidate"), "bind_finding_traffic": rcall(1, "other")})
	f.obs(t1, "planner", 0, "projected", 30, 10, 20, 12, 8, 4, 4, 0, candNames(1, 20), map[string]ToolRAGCall{
		"c20": rcall(1, "candidate"), "c13": rcall(4, "candidate")})
	f.obs(t2, "planner", 0, "projected", 16, 8, 8, 5, 0, 2, 2, 0, append([]string{"c02", "c01"}, candNames(3, 8)...), map[string]ToolRAGCall{
		"c02": rcall(2, "candidate"), "Skill": rcall(1, "keep")})
	f.obs(t2, "worker", 201, "no_signal", 24, 10, 14, 0, 0, 1, 1, 0, candNames(1, 14), map[string]ToolRAGCall{
		"c07": rcall(1, "candidate")})
	f.obs(t2, "worker", 202, "small_catalog", 14, 8, 6, 3, 0, 0, 0, 0, candNames(1, 6), nil)
	f.obs(t2, "planner", 0, "no_query", 12, 8, 4, 0, 0, 0, 0, 0, nil, map[string]ToolRAGCall{"Skill": rcall(1, "keep")})
	f.obs(t2, "worker", 203, "scorer_error", 18, 8, 10, 0, 0, 5, 5, 3, nil, nil)
	f.obs(t2, "worker", 204, "observer_panic", 10, 8, 2, 0, 0, 0, 0, 0, nil, map[string]ToolRAGCall{"_overflow": rcall(3, "other")})
	f.obs(t1, "planner", 0, "projected", 24, 10, 14, 14, 2, 4, 4, 0, append(candNames(1, 13), "_unknown"), map[string]ToolRAGCall{
		"_unknown": rcall(1, "other")})
	f.obs(t1, "worker", 103, "projected", 24, 10, 14, 9, 2, 1, 1, 1, append(candNames(1, 13), "c15"), map[string]ToolRAGCall{
		"c14": rcall(1, "candidate"), "c01": rcall(1, "candidate"), "mcp__y__b": rcall(1, "deferred_locked")})
	f.obs(t2, "worker", 205, "projected", 24, 10, 14, 9, 2, 0, 0, 0, candNames(1, 12), map[string]ToolRAGCall{
		"c13": rcall(1, "candidate")})

	// llm_usage: model "fixture" rows are stamped into the window, the one "outside" row keeps the real clock.
	ee := func(result int) map[string]ToolBytes {
		return map[string]ToolBytes{"ExecuteExtraTool": {Schema: 10, Result: result}}
	}
	f.llm(t1, "worker", 101, "fixture", ee(50))
	f.llm(t1, "worker", 101, "fixture", nil)
	f.llm(t1, "worker", 102, "fixture", ee(0))
	f.llm(t1, "worker", 103, "fixture", nil)
	f.llm(t2, "worker", 201, "fixture", nil)
	f.llm(t2, "worker", 202, "fixture", nil)
	f.llm(t2, "worker", 206, "fixture", nil)
	f.llm(t1, "worker", 104, "fixture", nil)
	f.llm(t1, "planner", 0, "fixture", ee(7))
	f.llm(t2, "planner", 0, "fixture", nil)
	f.llm(t2, "worker", 203, "fixture", ee(9))
	f.llm(t2, "worker", 204, "fixture", nil)
	f.llm(t1, "mainagent", 0, "fixture", nil)
	f.llm(t2, "worker", 299, "outside", nil)

	// tool_usage: session "fixture" rows are stamped into the window, "outside" keeps the real clock.
	f.usage(t1, "worker", 101, "c03", 2, "fixture")
	f.usage(t1, "worker", 101, "c02", 1, "fixture")
	f.usage(t1, "worker", 102, "c05", 2, "fixture")
	f.usage(t1, "worker", 102, "c01", 1, "fixture")
	f.usage(t1, "worker", 103, "c01", 2, "fixture")
	f.usage(t1, "planner", 0, "c20", 1, "fixture")
	f.usage(t1, "planner", 0, "c13", 4, "fixture")
	f.usage(t2, "worker", 201, "c07", 1, "fixture")
	f.usage(t2, "worker", 206, "c09", 1, "fixture")
	f.usage(t2, "planner", 0, "c99", 1, "fixture")
	f.usage(t1, "mainagent", 0, "c01", 1, "fixture")
	f.usage(t1, "worker", 101, "c03", 1, "outside")

	var minID int64
	if err := d.QueryRow(`SELECT min(id) FROM tool_rag_observations WHERE task_id IN ($1,$2)`, t1, t2).Scan(&minID); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE tool_rag_observations SET ts = $1::timestamptz + make_interval(mins => (id - $2::bigint)::int)
WHERE task_id IN ($3,$4)`, reportBase, minID, t1, t2)
	f.exec(`UPDATE llm_usage SET ts = $1::timestamptz WHERE task_id IN ($2,$3) AND model = 'fixture'`,
		reportBase, strconv.FormatInt(t1, 10), strconv.FormatInt(t2, 10))
	f.exec(`UPDATE tool_usage SET ts = $1::timestamptz WHERE task_id IN ($2,$3) AND session_id = 'fixture'`, reportBase, t1, t2)
	return f
}

func (f *reportFixture) report(t *testing.T) map[string]reportResult {
	t.Helper()
	return runReport(t, f.d, reportT0)
}

func TestToolRagReportFileIsReadOnly(t *testing.T) {
	src := readReport(t)
	stmts := parseReport(t, src)
	if len(stmts) < 3 {
		t.Fatalf("only %d statements parsed", len(stmts))
	}
	if first := strings.TrimSpace(stmts[0].sql); first != "BEGIN READ ONLY;" {
		t.Errorf("first statement is %q, want BEGIN READ ONLY;", first)
	}
	if last := strings.TrimSpace(stmts[len(stmts)-1].sql); last != "ROLLBACK;" {
		t.Errorf("last statement is %q, want ROLLBACK;", last)
	}
	forbidden := regexp.MustCompile(`(?i)\b(insert|update|delete|truncate|create|alter|drop|grant|revoke|copy|vacuum|commit|set|call|do|execute|prepare|lock|listen|notify|nextval|setval|pg_sleep)\b`)
	seen := map[string]bool{}
	for i, s := range stmts {
		head := strings.ToUpper(strings.Fields(s.sql)[0])
		switch {
		case i == 0 || i == len(stmts)-1:
		case head == "SELECT" || head == "WITH":
			if s.id == "" {
				t.Errorf("statement %d has no [id] comment before it", i)
			}
			if seen[s.id] {
				t.Errorf("section id %s is used twice", s.id)
			}
			seen[s.id] = true
		default:
			t.Errorf("statement %d (%s) starts with %s: only SELECT or WITH may follow BEGIN READ ONLY", i, s.id, head)
		}
		if i != 0 && i != len(stmts)-1 {
			if m := forbidden.FindString(s.sql); m != "" {
				t.Errorf("section %s contains the word %q", s.id, m)
			}
			if strings.Count(s.sql, ";") != 1 {
				t.Errorf("section %s holds more than one statement", s.id)
			}
		}
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) {
			t.Errorf("psql meta command in the file: %q", line)
		}
	}
	// The only psql variable is :'t0' (::casts are not variables, and psql does not interpolate inside quotes).
	literal := regexp.MustCompile(`'[^']*'`)
	variable := regexp.MustCompile(`(^|[^:]):['"A-Za-z_]`)
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if variable.MatchString(literal.ReplaceAllString(strings.ReplaceAll(line, psqlVar, ""), "''")) {
			t.Errorf("a psql variable other than t0: %q", line)
		}
	}
	for _, id := range []string{"0.1", "0.2", "0.3", "0.4", "0.5", "0.6", "A.1", "A.1p", "A.2", "A.3", "A.4", "A.5", "A.6", "A.7",
		"B.1", "B.2", "B.3", "C.1", "C.2", "C.3", "C.4", "D.1", "D.2", "V.1"} {
		if !seen[id] {
			t.Errorf("section %s is missing", id)
		}
	}

	// Run on the fixture: the row counts of the three tables do not change, and the session is read only
	// after the first statement, so a write fails with SQLSTATE 25006.
	f := newReportFixture(t)
	count := func() [3]int {
		var c [3]int
		for i, tbl := range []string{"tool_rag_observations", "llm_usage", "tool_usage"} {
			if err := f.d.QueryRow(`SELECT count(*) FROM ` + tbl).Scan(&c[i]); err != nil {
				t.Fatal(err)
			}
		}
		return c
	}
	before := count()
	got := f.report(t)
	if len(got) != len(seen) {
		t.Errorf("ran %d sections, the file has %d", len(got), len(seen))
	}
	if after := count(); after != before {
		t.Errorf("row counts changed: before %v after %v", before, after)
	}

	ctx := context.Background()
	conn, err := f.d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, stmts[0].sql); err != nil {
		t.Fatal(err)
	}
	_, err = conn.ExecContext(ctx, `DELETE FROM tool_rag_observations`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Errorf("a write inside the report's transaction returned %v, want SQLSTATE 25006", err)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if after := count(); after != before {
		t.Errorf("row counts changed after the injected write: before %v after %v", before, after)
	}
}

// An empty window: every section runs, the aggregates without a grouping return their zero row.
func TestToolRagReportRunsOnAnEmptyWindow(t *testing.T) {
	d := toolRagDB(t)
	got := runReport(t, d, "2200-01-01 00:00:00+00")
	wantRows(t, got, "0.1")
	wantRows(t, got, "0.2", row("0", "0", "0", "NULL", "NULL"))
	wantRows(t, got, "0.4", row("0", "0", "0", "0", "0"))
	wantRows(t, got, "0.5", row("0", "0", "NULL", "0", "0"))
	wantRows(t, got, "0.6")
	wantRows(t, got, "A.1")
	wantRows(t, got, "A.1p")
	wantRows(t, got, "A.2")
	wantRows(t, got, "A.3")
	wantRows(t, got, "A.4", row("01-04", "0", "0", "0"), row("05-08", "0", "0", "0"), row("09-12", "0", "0", "0"),
		row("13-16", "0", "0", "0"), row("17+", "0", "0", "0"))
	wantRows(t, got, "A.5", row("0", "0", "NULL", "0", "0", "NULL"))
	wantRows(t, got, "A.7")
	wantRows(t, got, "B.1", row("0", "0", "0", "0", "0", "0"))
	wantRows(t, got, "B.2")
	wantRows(t, got, "B.3")
	wantRows(t, got, "C.2")
	wantRows(t, got, "C.3")
	wantRows(t, got, "C.4")
	wantRows(t, got, "D.1")
	wantRows(t, got, "D.2")
	wantRows(t, got, "V.1",
		row("observed_rows", "0"), row("projected_rows", "0"), row("rows_would_hide", "0"),
		row("would_hide_pct_of_projected", "NULL"), row("max_candidate_count", "0"),
		row("hiding_runs_calling_ranked_candidate", "0"), row("hiding_runs_with_a_miss", "0"), row("hiding_miss_pct", "NULL"),
		row("no_signal_pct_of_large_catalog_rows", "NULL"), row("fallback_pct_of_rows", "NULL"),
		row("rankable_share_pct", "NULL"), row("worker_coverage_pct", "NULL"))
}

// Hand-calculated from the fixture (13 rows R1..R13; 9 worker rows, 4 planner rows; 8 worker intents).
func TestToolRagReportCoverageAndDataQuality(t *testing.T) {
	f := newReportFixture(t)
	got := f.report(t)
	id1, id2 := strconv.FormatInt(f.t1, 10), strconv.FormatInt(f.t2, 10)

	// Rows per agent and outcome; tasks: planner projected has rows of both tasks, worker projected of both.
	wantRows(t, got, "0.1",
		row("planner", "no_query", "1", "1"),
		row("planner", "projected", "3", "2"),
		row("worker", "no_signal", "1", "1"),
		row("worker", "observer_panic", "1", "1"),
		row("worker", "projected", "5", "2"),
		row("worker", "scorer_error", "1", "1"),
		row("worker", "small_catalog", "1", "1"))
	// 13 rows, 2 tasks, intents 101 102 103 201..205 = 8; first row at minute 0, last at minute 12.
	wantRows(t, got, "0.2", row("13", "2", "8", "2100-01-01 00:00:00", "2100-01-01 00:12:00"))
	// planner: 3 projected of 4, 2 hide (R4, R11): 66.7 and 50.0; max candidates 20, max hide 8.
	// worker: 5 projected of 9, all 5 hide: 100.0 and 55.6; 14 and 2. all: 8 of 13, 7 hide: 87.5, 53.8.
	wantRows(t, got, "0.3",
		row("planner", "4", "3", "2", "66.7", "50.0", "20", "8"),
		row("worker", "9", "5", "5", "100.0", "55.6", "14", "2"),
		row("all", "13", "8", "7", "87.5", "53.8", "20", "8"))
	// _unknown in the ranking and called: R11. _overflow called: R10. Ranking at cap: none.
	// Ranking shorter than candidate_count among rows with a ranking: R13 (12 of 14).
	wantRows(t, got, "0.4", row("1", "1", "1", "0", "1"))
	// Worker intents with calls in the window: 101 102 103 104 (task 1), 201 202 203 204 206 (task 2) = 9 (299 is
	// outside the window; the planner and mainagent rows are not worker rows). Observed: all but 104 and 206 = 7,
	// 77.8 percent. Observed without calls: 205. Worker observation rows: 9 (intent 101 resumed: two rows).
	wantRows(t, got, "0.5", row("9", "7", "77.8", "1", "9"))
	wantRows(t, got, "0.6", row(id1, "104", "1"), row(id2, "206", "1"))
}

// Section A. Hiding runs (projected, would_hide_count > 0): R1 R2 R3 R4 R11 R12 R13.
func TestToolRagReportLexicalRetrieval(t *testing.T) {
	f := newReportFixture(t)
	got := f.report(t)
	id1, id2 := strconv.FormatInt(f.t1, 10), strconv.FormatInt(f.t2, 10)

	// Highest position of a called candidate with a position: R1 14, R2 5, R3 2, R4 20, R5 1 (c02 is ranked first there), R12 1 (c14 has no
	// position), R11 and R13 none. Candidates: R1 R2 R3 R12 R13 14 each, R4 20, R5 8, R11 14.
	// planner k=4: R4 R5 R11 (3 rows), calling 2 (R4 R5), miss R4 (20>4): 1 of 2 = 50.0.
	// planner k=8: R4 R11 (R5 has 8 candidates), calling 1, miss 1 = 100.0. k=12 the same. k=16: R4 only.
	// worker k=4: 5 rows, calling R1 R2 R3 R12 = 4, misses R1 (14) R2 (5) = 2 = 50.0; k=8 and k=12: miss R1 only
	// = 25.0; k=16: no worker row has more than 16 candidates.
	wantRows(t, got, "A.1",
		row("planner", "4", "3", "2", "1", "50.0"),
		row("planner", "8", "2", "1", "1", "100.0"),
		row("planner", "12", "2", "1", "1", "100.0"),
		row("planner", "16", "1", "1", "1", "100.0"),
		row("worker", "4", "5", "4", "2", "50.0"),
		row("worker", "8", "5", "4", "1", "25.0"),
		row("worker", "12", "5", "4", "1", "25.0"))
	// Planner per task at the recorded k=12: task 1 has R4 and R11, both hiding, only R4 calls a ranked
	// candidate, and it misses; task 2 has R5 which hides nothing.
	wantRows(t, got, "A.1p",
		row(id1, "2", "2", "1", "1", "100.0"),
		row(id2, "1", "0", "0", "0", "NULL"))
	// Seven hiding runs. candidate pairs: R1 2, R2 2, R3 1, R4 2, R12 2, R13 1 = 10, calls 3+4+1+5+2+1 = 16;
	// inside the top 12: c03 c01 c05 c02 c01 = 5; outside: c14 (R1), c20, c13 (R4) = 3; unranked: c14 (R12), c13 (R13).
	// deferred_global 1 pair 2 calls; deferred_locked 1 pair 1 call; keep (Skill in R1) 1 pair 1 call;
	// other: bind_finding_traffic (R3) and _unknown (R11) = 2 pairs 2 calls.
	wantRows(t, got, "A.2",
		row("7", "candidate", "10", "16", "5", "3", "2"),
		row("7", "deferred_global", "1", "2", "NULL", "NULL", "NULL"),
		row("7", "deferred_locked", "1", "1", "NULL", "NULL", "NULL"),
		row("7", "keep", "1", "1", "NULL", "NULL", "NULL"),
		row("7", "other", "2", "2", "NULL", "NULL", "NULL"))
	// Outside the top 12 in hiding runs: c13 (planner, 4 calls, position 13), c14 (worker, 14), c20 (planner, 20).
	wantRows(t, got, "A.3",
		row("c13", "planner", "1", "4", "13", "13"),
		row("c14", "worker", "1", "1", "14", "14"),
		row("c20", "planner", "1", "1", "20", "20"))
	// Positions in all projected rows (pairs, calls, pairs in hiding runs):
	// 1-4: c03(3) c01(1) c02(2) R5:c02(1) R12:c01(1) = 5 pairs, calls 2+1+1+2+1 = 7, hiding 4 (R5 hides nothing);
	// 5-8: c05 = 1 pair 3 calls; 9-12: none; 13-16: c14(14) c13(13) = 2 pairs 5 calls; 17+: c20 = 1 pair 1 call.
	wantRows(t, got, "A.4",
		row("01-04", "5", "7", "4"),
		row("05-08", "1", "3", "1"),
		row("09-12", "0", "0", "0"),
		row("13-16", "2", "5", "2"),
		row("17+", "1", "1", "1"))
	// After nonzero_count: R1 c14 (14>9), R4 c20 (20>12) and c13 (13>12): 3 of 9 pairs = 33.3;
	// calls 1+1+4 = 6 of 16 = 37.5.
	wantRows(t, got, "A.5", row("9", "3", "33.3", "16", "6", "37.5"))
	// Large catalog rows: planner 3 projected, worker 5 projected + 1 no_signal (R6) = 6; all 9, 1 no_signal.
	wantRows(t, got, "A.6",
		row("planner", "3", "0", "0.0"),
		row("worker", "6", "1", "16.7"),
		row("all", "9", "1", "11.1"))
	// First ranked in projected rows with a nonzero score: planner R4 and R11 start with c01, R5 with c02 (3 rows:
	// 66.7 and 33.3); worker R1 R2 R3 R12 R13 all start with c01 (5 rows: 100.0).
	wantRows(t, got, "A.7",
		row("planner", "c01", "2", "3", "66.7"),
		row("planner", "c02", "1", "3", "33.3"),
		row("worker", "c01", "5", "5", "100.0"))
}

// Section B. R12 is the deleted-candidate case: its ranking is complete (14 of 14 candidates) and does not hold
// the called candidate c14. R13's ranking is cut (12 of 14), which explains its missing c13.
func TestToolRagReportPermissionBehaviour(t *testing.T) {
	f := newReportFixture(t)
	got := f.report(t)

	// Rows with a ranking: R1 R2 R3 R4 R5 R6 R7 R11 R12 R13 = 10. Called candidate pairs: R1 2, R2 2, R3 1, R4 2,
	// R5 1, R6 1, R7 0, R11 0, R12 2, R13 1 = 12. Not in the ranking: R12 c14 (complete ranking: a violation),
	// R13 c13 (truncated ranking). One row with a violation.
	wantRows(t, got, "B.1", row("10", "12", "2", "1", "1", "1"))
	// Never candidates, by class and agent (any outcome): deferred_global worker R1 (2 calls); deferred_locked
	// worker R12 (1); keep planner R5 R8 (2 runs, 2 calls), keep worker R1 (1); other planner R11 (1),
	// other worker R3 and R10 (2 runs, 1 + 3 calls).
	wantRows(t, got, "B.2",
		row("deferred_global", "worker", "1", "1", "2"),
		row("deferred_locked", "worker", "1", "1", "1"),
		row("keep", "planner", "2", "2", "2"),
		row("keep", "worker", "1", "1", "1"),
		row("other", "planner", "1", "1", "1"),
		row("other", "worker", "2", "2", "4"))
	// By runs, then calls, then name in C order (uppercase before underscore before lowercase).
	wantRows(t, got, "B.3",
		row("Skill", "keep", "planner", "2", "2"),
		row("_overflow", "other", "worker", "1", "3"),
		row("mcp__x__a", "deferred_global", "worker", "1", "2"),
		row("Skill", "keep", "worker", "1", "1"),
		row("_unknown", "other", "planner", "1", "1"),
		row("bind_finding_traffic", "other", "worker", "1", "1"),
		row("mcp__y__b", "deferred_locked", "worker", "1", "1"))
}

// Section C.
func TestToolRagReportLockedAndUnlockable(t *testing.T) {
	f := newReportFixture(t)
	got := f.report(t)

	// planner R4 R5 R8 R11: deferred 4 2 0 4 (3 rows, sum 10, max 4), locked 4 2 0 4 (3 rows, sum 10), unlocked 0;
	// candidates 20+8+4+14 = 46: 46/56 = 82.1.
	// worker R1 R2 R3 R6 R7 R9 R10 R12 R13: deferred 3 0 3 1 0 5 0 1 0 (5 rows, sum 13, max 5), locked 2 0 2 1 0 5 0 1 0
	// (5 rows, sum 11), unlocked 1 0 2 0 0 3 0 1 0 (4 rows, sum 7); candidates 14 14 14 14 6 10 2 14 14 = 102:
	// 102/115 = 88.7. all: 8 rows, 23, 5, 8 rows, 21, 4 rows, 7, 148: 148/171 = 86.5.
	wantRows(t, got, "C.1",
		row("planner", "4", "3", "10", "4", "3", "10", "0", "0", "46", "82.1"),
		row("worker", "9", "5", "13", "5", "5", "11", "4", "7", "102", "88.7"),
		row("all", "13", "8", "23", "5", "8", "21", "4", "7", "148", "86.5"))
	// worker rows with deferred tools: R1 R3 R6 R9 R12 = 5; calling: R1 (mcp__x__a, 2 global) and R12
	// (mcp__y__b, 1 locked). planner: 3 rows with deferred tools, none called.
	wantRows(t, got, "C.2", row("planner", "3", "0", "0", "0"), row("worker", "5", "2", "2", "1"))
	// Run keys. planner: task 1 (R4 R11) and task 2 (R5 R8) = 2 keys, both with llm rows; ExecuteExtraTool in a
	// request of task 1 only (listed 1), with a result there (7 bytes) and deferred calls 0: the suspect column 1.
	// worker keys: 101 (R1 R3), 102, 103, 201, 202, 203, 204, 205 = 8; llm rows for 7 (205 has none).
	// Listed: 101, 102, 203 = 3; result above zero: 101 (50) and 203 (9) = 2 (102 has 0 bytes).
	// With a result and zero deferred calls: 203 = 1. With a result and calls: 101 (2 calls) = 1.
	// Calls without a result: 103 (1 call, llm rows without ExecuteExtraTool) = 1. A result with no deferred
	// tools listed: none.
	wantRows(t, got, "C.3",
		row("planner", "2", "2", "1", "1", "1", "0", "0", "0"),
		row("worker", "8", "7", "3", "2", "1", "1", "1", "0"))
	// tool_usage (window, planner and worker with a task). worker pairs: (101,c03) 2 = 2 equal; (101,c02) 1 = 1
	// equal; (102,c05) 2 against 3 called: counter higher; (102,c01) 1 = 1 equal; (103,c01) 2 against 1 called:
	// counter lower; (201,c07) 1 = 1 equal; (206,c09) has no observation: 7 pairs. planner pairs: (c20) 1 = 1,
	// (c13) 4 = 4 equal, (c99) not in called: 3 pairs. The mainagent row and the outside row are not counted.
	wantRows(t, got, "C.4",
		row("planner", "3", "0", "1", "2", "0", "0"),
		row("worker", "7", "1", "0", "4", "1", "1"))
}

// Section D.
func TestToolRagReportFallback(t *testing.T) {
	f := newReportFixture(t)
	got := f.report(t)

	// planner: 4 rows, one no_query = 25.0. worker: 9 rows, one each of no_signal, observer_panic, scorer_error
	// and small_catalog = 11.1 each.
	wantRows(t, got, "D.1",
		row("planner", "no_query", "1", "25.0"),
		row("worker", "no_signal", "1", "11.1"),
		row("worker", "observer_panic", "1", "11.1"),
		row("worker", "scorer_error", "1", "11.1"),
		row("worker", "small_catalog", "1", "11.1"))
	// planner host 30 16 12 24 (sorted 12 16 24 30, lower median 16), candidates 20 8 4 14 (4 8 14 20: 8).
	// worker host 24 24 24 24 14 18 10 24 24 (sorted 10 14 18 24 x6: 5th = 24), candidates
	// 14 14 14 14 6 10 2 14 14 (sorted 2 6 10 14 x6: 5th = 14).
	wantRows(t, got, "D.2",
		row("planner", "4", "12", "16", "30", "4", "8", "20"),
		row("worker", "9", "10", "24", "24", "2", "14", "14"))
}

// The verdict inputs, from the numbers above.
func TestToolRagReportVerdictInputs(t *testing.T) {
	f := newReportFixture(t)
	got := f.report(t)

	// 13 rows, 8 projected, 7 hide (87.5), most candidates 20. Hiding runs calling a ranked candidate: R1 R2 R3
	// R4 R12 = 5; with a called candidate after k=12: R1 and R4 = 2: 40.0. No signal 1 of 9 large-catalog rows
	// (11.1). Not projected: 5 of 13 (38.5). Rankable share 148/171 (86.5). Coverage 7 of 9 (77.8).
	wantRows(t, got, "V.1",
		row("observed_rows", "13"), row("projected_rows", "8"), row("rows_would_hide", "7"),
		row("would_hide_pct_of_projected", "87.5"), row("max_candidate_count", "20"),
		row("hiding_runs_calling_ranked_candidate", "5"), row("hiding_runs_with_a_miss", "2"),
		row("hiding_miss_pct", "40.0"), row("no_signal_pct_of_large_catalog_rows", "11.1"),
		row("fallback_pct_of_rows", "38.5"), row("rankable_share_pct", "86.5"), row("worker_coverage_pct", "77.8"))
}

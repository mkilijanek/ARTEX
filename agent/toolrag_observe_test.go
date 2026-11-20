package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// ragRunTool is ragTool that can be called: it answers "ok".
func ragRunTool(name, desc string, params ...string) actool.CoreTool {
	props := map[string]any{}
	for _, p := range params {
		props[p] = map[string]any{"type": "string"}
	}
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: map[string]any{"type": "object", "properties": props},
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Text("ok"), nil
		},
	})
}

// ragRig installs the hooks AugmentTools reads (augment returning def and an
// original-cleanup spy, no resolve, finding binding off, a recording observer)
// and restores whatever was there.
type ragRig struct {
	mu       sync.Mutex
	rows     []ToolRAGObservation
	cleanups atomic.Int32
}

func newRagRig(t *testing.T, def DeferredInfo) *ragRig {
	t.Helper()
	oldAug, oldRes, oldFind, oldObs := ToolAugment(), ToolResolve(), FindingTrafficBindingEnabled(), currentToolRAGObserver()
	oldScorer, oldProblems := toolRAGScorer, toolRAGProblems
	r := &ragRig{}
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return nil, def, func() { r.cleanups.Add(1) }
	})
	SetToolResolve(nil)
	SetFindingTrafficBindingEnabled(nil)
	SetToolRAGObserver(func(o ToolRAGObservation) {
		r.mu.Lock()
		r.rows = append(r.rows, o)
		r.mu.Unlock()
	})
	toolRAGScorer, toolRAGProblems = nil, NewToolRAGLog()
	t.Cleanup(func() {
		SetToolAugment(oldAug)
		SetToolResolve(oldRes)
		SetFindingTrafficBindingEnabled(oldFind)
		SetToolRAGObserver(oldObs)
		toolRAGScorer, toolRAGProblems = oldScorer, oldProblems
	})
	return r
}

func (r *ragRig) observed() []ToolRAGObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.rows)
}

func ragCtx(query string) context.Context {
	return WithRunInfo(WithToolQuery(context.Background(), query), RunInfo{TaskID: 7, ExplorationID: 3, IntentID: 5})
}

// underlying strips the panic guard so a test can compare what AugmentTools wrapped.
func underlying(t actool.CoreTool) actool.CoreTool {
	if g, ok := t.(*guardedTool); ok {
		return g.CoreTool
	}
	return t
}

func TestToolRAGModeValues(t *testing.T) {
	for raw, want := range map[string][2]string{
		"": {toolRAGModeObserve, ""}, "observe": {toolRAGModeObserve, ""}, " Observe ": {toolRAGModeObserve, ""},
		"off": {toolRAGModeOff, ""}, "OFF": {toolRAGModeOff, ""}, "0": {toolRAGModeOff, ""}, "false": {toolRAGModeOff, ""}, " No ": {toolRAGModeOff, ""},
		"1": {toolRAGModeObserve, "filter"}, "true": {toolRAGModeObserve, "filter"}, "on": {toolRAGModeObserve, "filter"},
		"yes": {toolRAGModeObserve, "filter"}, "filter": {toolRAGModeObserve, "filter"}, " FILTER ": {toolRAGModeObserve, "filter"},
		"bogus": {toolRAGModeObserve, "unknown"}, "observe,off": {toolRAGModeObserve, "unknown"},
	} {
		mode, note := parseToolRAGMode(raw)
		if mode != want[0] || note != want[1] {
			t.Errorf("%q -> (%s, %q), want (%s, %q)", raw, mode, note, want[0], want[1])
		}
	}
}

func TestToolRAGWarnsOncePerProcess(t *testing.T) {
	for _, v := range []string{"1", "true", "on", "yes", "filter"} {
		t.Run(v, func(t *testing.T) {
			toolRAGWarned.Store(false)
			logs := dbtest.CaptureLog(t)
			t.Setenv(toolRAGEnv, " "+strings.ToUpper(v)+" ")
			for range 3 {
				if got := toolRAGMode(); got != toolRAGModeObserve {
					t.Fatalf("mode = %s", got)
				}
			}
			want := "[tool-rag] ARTEX_TOOL_RAG=" + v + " requests filtering, which this build does not implement; observing only"
			if out := strings.TrimSpace(logs()); out != want {
				t.Fatalf("log = %q, want exactly %q", out, want)
			}
		})
	}
	t.Run("unknown value is not echoed", func(t *testing.T) {
		toolRAGWarned.Store(false)
		logs := dbtest.CaptureLog(t)
		t.Setenv(toolRAGEnv, "s3cr3t-value")
		for range 3 {
			toolRAGMode()
		}
		out := logs()
		if strings.Count(out, "[tool-rag]") != 1 || strings.Contains(out, "s3cr3t") || !strings.Contains(out, "accepted values") {
			t.Fatalf("log = %q", out)
		}
		// One warning per process, not one per kind: the filter value stays quiet now.
		t.Setenv(toolRAGEnv, "1")
		toolRAGMode()
		if strings.Count(logs(), "[tool-rag]") != 1 {
			t.Fatalf("second warning: %q", logs())
		}
	})
	t.Run("accepted values stay quiet", func(t *testing.T) {
		toolRAGWarned.Store(false)
		logs := dbtest.CaptureLog(t)
		for _, v := range []string{"", "observe", "off", "0", "false", "no"} {
			t.Setenv(toolRAGEnv, v)
			toolRAGMode()
		}
		if logs() != "" {
			t.Fatalf("log = %q", logs())
		}
	})
}

func TestToolRAGLogsTheModeOnce(t *testing.T) {
	toolRAGModeLogged.Store(false)
	toolRAGWarned.Store(true)
	logs := dbtest.CaptureLog(t)
	t.Setenv(toolRAGEnv, "")
	t.Setenv(toolRAGTopKEnv, "")
	LogToolRAGMode()
	LogToolRAGMode()
	if got := strings.TrimSpace(logs()); got != "[tool-rag] mode=observe scorer=bm25-v1 k=12" {
		t.Fatalf("log = %q", got)
	}
	toolRAGModeLogged.Store(false)
	t.Setenv(toolRAGEnv, "off")
	LogToolRAGMode()
	if !strings.HasSuffix(strings.TrimSpace(logs()), "[tool-rag] mode=off") {
		t.Fatalf("log = %q", logs())
	}
}

// AC1: whatever the mode, scorer outcome or agent key, AugmentTools hands back the
// same elements in the same order, and neither the incoming nor the resolved slice
// is touched (length, capacity and every element of the backing array).
func TestToolRAGAugmentReturnsTheSameListInEveryMode(t *testing.T) {
	scorers := map[string]ToolScorer{
		"bm25":  nil,
		"error": func(string, []actool.CoreTool) ([]float64, error) { return nil, errors.New("scorer failed") },
		"panic": func(string, []actool.CoreTool) ([]float64, error) { panic("scorer panicked") },
	}
	for _, size := range []int{5, 25, 60} {
		base, def := obsFixture()
		if size != 25 {
			base = append(ragCatalog(size), base[2:6]...) // keep the deferred tools in every fixture
			def.Unlock = actool.NewUnlockSet("mcp__srv__g1", "mcp__srv__g2")
		}
		// Reverse-alphabetical, so any sort of the incoming slice is visible.
		slices.SortFunc(base, func(a, b actool.CoreTool) int { return strings.Compare(b.Name(), a.Name()) })
		newRagRig(t, def)
		for _, key := range []string{"planner", "worker", "mainagent", "chat-assistant"} {
			t.Setenv(toolRAGEnv, "off")
			ref, refDef, refCleanup := AugmentTools(ragCtx(ragQuery), key, base)
			refCleanup()
			for _, mode := range []string{"off", "OFF", " Observe ", "observe", "", "0", "false", "no", "1", "true", "on", "yes", "filter", "bogus"} {
				for sc, scorer := range scorers {
					t.Run(fmt.Sprintf("%d/%s/%q/%s", size, key, mode, sc), func(t *testing.T) {
						t.Setenv(toolRAGEnv, mode)
						toolRAGWarned.Store(true)
						toolRAGScorer = scorer
						snapshot := slices.Clone(base[:cap(base)])
						got, gotDef, cleanup := AugmentTools(ragCtx(ragQuery), key, base)
						cleanup()
						if len(got) != len(ref) {
							t.Fatalf("len = %d, want %d", len(got), len(ref))
						}
						for i := range got {
							if underlying(got[i]) != underlying(ref[i]) {
								t.Fatalf("element %d: %s, want %s", i, got[i].Name(), ref[i].Name())
							}
						}
						if !slices.Equal(gotDef.Deferred, refDef.Deferred) || !slices.Equal(gotDef.GlobalNames, refDef.GlobalNames) ||
							gotDef.Unlock != refDef.Unlock || gotDef.FindingGuidance != refDef.FindingGuidance ||
							(gotDef.UnlockSkill == nil) != (refDef.UnlockSkill == nil) {
							t.Fatalf("DeferredInfo changed: %+v vs %+v", gotDef, refDef)
						}
						if !slices.Equal(snapshot, base[:cap(base)]) {
							t.Fatal("the incoming slice was written")
						}
					})
				}
			}
		}
	}
}

// The observer sees the list ToolResolve returned and must leave that exact slice
// alone: the scorer checks it while it runs, and again afterwards.
func TestToolRAGObserverDoesNotWriteTheResolvedSlice(t *testing.T) {
	base, def := obsFixture()
	newRagRig(t, def)
	t.Setenv(toolRAGEnv, "observe")
	resolved := slices.Clone(base)
	slices.SortFunc(resolved, func(a, b actool.CoreTool) int { return strings.Compare(b.Name(), a.Name()) })
	snapshot := slices.Clone(resolved[:cap(resolved)])
	SetToolResolve(func(context.Context, string, []actool.CoreTool) []actool.CoreTool { return resolved })
	called := false
	toolRAGScorer = func(q string, cands []actool.CoreTool) ([]float64, error) {
		called = true
		if !slices.Equal(snapshot, resolved[:cap(resolved)]) {
			t.Error("resolved slice changed before scoring finished")
		}
		return bm25Score(q, cands)
	}
	out, _, cleanup := AugmentTools(ragCtx("alpha beta"), "worker", base)
	cleanup()
	if !called {
		t.Fatal("scorer not used")
	}
	if !slices.Equal(snapshot, resolved[:cap(resolved)]) || len(out) != len(resolved) {
		t.Fatal("resolved slice changed")
	}
	for i := range out {
		if underlying(out[i]) != resolved[i] {
			t.Fatalf("element %d reordered", i)
		}
	}
}

// AC4: off values compute nothing; main agent, chat and runs without a task id are
// never observed.
func TestToolRAGOffAndUnobservedRunsDoNothing(t *testing.T) {
	base, def := obsFixture()
	rig := newRagRig(t, def)
	var scored atomic.Int32
	toolRAGScorer = func(q string, c []actool.CoreTool) ([]float64, error) { scored.Add(1); return bm25Score(q, c) }
	check := func(name, mode, key string, ctx context.Context) {
		t.Run(name, func(t *testing.T) {
			t.Setenv(toolRAGEnv, mode)
			out, _, cleanup := AugmentTools(ctx, key, base)
			cleanup()
			for _, tool := range out {
				if tool.(*guardedTool).obs != nil {
					t.Fatalf("%s has a call counter", tool.Name())
				}
			}
			if scored.Load() != 0 || len(rig.observed()) != 0 {
				t.Fatalf("scored=%d rows=%d", scored.Load(), len(rig.observed()))
			}
		})
	}
	for _, mode := range []string{"off", "0", "false", "no"} {
		check("planner/"+mode, mode, "planner", ragCtx(ragQuery))
		check("worker/"+mode, mode, "worker", ragCtx(ragQuery))
	}
	for _, key := range []string{"mainagent", "chat-assistant", "reporter"} {
		check(key, "observe", key, ragCtx(ragQuery))
	}
	check("planner without task id", "observe", "planner", WithToolQuery(context.Background(), ragQuery))
	check("worker with task 0", "observe", "worker", WithRunInfo(WithToolQuery(context.Background(), ragQuery), RunInfo{ExplorationID: 3}))
	t.Run("no observer installed", func(t *testing.T) {
		SetToolRAGObserver(nil)
		t.Setenv(toolRAGEnv, "observe")
		out, _, cleanup := AugmentTools(ragCtx(ragQuery), "worker", base)
		cleanup()
		if scored.Load() != 0 || out[0].(*guardedTool).obs != nil {
			t.Fatal("observed without an observer")
		}
	})
}

// AC8: one observation per run, delivered by cleanup and only once; the original
// cleanup runs every time cleanup does.
func TestToolRAGOneObservationPerRunAfterCleanup(t *testing.T) {
	for _, key := range []string{"planner", "worker"} {
		t.Run(key, func(t *testing.T) {
			base, def := obsFixture()
			rig := newRagRig(t, def)
			t.Setenv(toolRAGEnv, "observe")
			_, _, cleanup := AugmentTools(ragCtx("alpha beta"), key, base)
			if len(rig.observed()) != 0 {
				t.Fatal("row delivered before cleanup")
			}
			cleanup()
			rows := rig.observed()
			if len(rows) != 1 || rows[0].AgentKey != key || rig.cleanups.Load() != 1 {
				t.Fatalf("rows=%d cleanups=%d", len(rows), rig.cleanups.Load())
			}
			cleanup()
			if len(rig.observed()) != 1 {
				t.Fatalf("second cleanup added a row")
			}
			if rig.cleanups.Load() != 2 {
				t.Fatal("original cleanup must run on every call, as before")
			}
		})
	}
}

// AC9, AC12, AC13 through AugmentTools, and what a row carries.
func TestToolRAGRowsThroughAugmentTools(t *testing.T) {
	t.Run("outcomes", func(t *testing.T) {
		for name, tc := range map[string]struct {
			tools   []actool.CoreTool
			query   string
			outcome string
			ranked  bool
		}{
			"19 tools":     {ragCatalog(19), ragQuery, toolRAGSmallCatalog, true},
			"no query":     {ragCatalog(25), "", toolRAGNoQuery, false},
			"25 tools":     {ragCatalog(25), ragQuery, toolRAGProjected, true},
			"all zero":     {ragCatalog(25), "zzzz", toolRAGNoSignal, true},
			"blank query ": {ragCatalog(25), " \n ", toolRAGNoQuery, false},
		} {
			t.Run(name, func(t *testing.T) {
				rig := newRagRig(t, DeferredInfo{})
				t.Setenv(toolRAGEnv, "")
				_, _, cleanup := AugmentTools(ragCtx(tc.query), "worker", tc.tools)
				cleanup()
				rows := rig.observed()
				if len(rows) != 1 || rows[0].Outcome != tc.outcome || (len(rows[0].Ranking) > 0) != tc.ranked {
					t.Fatalf("rows = %+v", rows)
				}
			})
		}
	})
	t.Run("identity, counts, locked and unlocked", func(t *testing.T) {
		base, def := obsFixture()
		rig := newRagRig(t, def)
		t.Setenv(toolRAGEnv, "")
		t.Setenv(toolRAGTopKEnv, "3")
		_, _, cleanup := AugmentTools(ragCtx("alpha beta"), "worker", base)
		def.Unlock.Add("mcp__srv__s1", "mcp__srv__s2") // the skill loads during the run
		cleanup()
		rows := rig.observed()
		if len(rows) != 1 {
			t.Fatalf("rows = %d", len(rows))
		}
		r := rows[0]
		if r.TaskID != 7 || r.ExplorationID != 3 || r.IntentID != 5 || r.AgentKey != "worker" || r.Scorer != "bm25-v1" || r.Mode != "observe" {
			t.Errorf("identity = %+v", r)
		}
		if r.DeferredCount != 4 || r.LockedAtStart != 2 || r.UnlockedDuringRun != 2 || r.K != 3 || r.WouldHideCount != 16 || r.CandidateCount != 19 {
			t.Errorf("counts = %+v", r)
		}
		for _, n := range def.Deferred {
			if slices.Contains(r.Ranking, n) {
				t.Errorf("deferred %s ranked", n)
			}
		}
		if r.DurationUS < 0 || r.QueryTokens != 2 {
			t.Errorf("duration=%d tokens=%d", r.DurationUS, r.QueryTokens)
		}
	})
	t.Run("planner rows have no intent", func(t *testing.T) {
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		ctx := WithRunInfo(WithToolQuery(context.Background(), ragQuery), RunInfo{TaskID: 7, ExplorationID: 3})
		_, _, cleanup := AugmentTools(ctx, "planner", ragCatalog(25))
		cleanup()
		if rows := rig.observed(); len(rows) != 1 || rows[0].IntentID != 0 || rows[0].AgentKey != "planner" {
			t.Fatalf("rows = %+v", rows)
		}
	})
}

// AC13: ranked names come from the list ToolResolve returned (never from a tool it
// dropped), never include keep tools, and hold with the finding-binding switch both ways.
func TestToolRAGRankedNamesComeFromTheResolvedList(t *testing.T) {
	for _, binding := range []bool{false, true} {
		t.Run(fmt.Sprint("binding=", binding), func(t *testing.T) {
			base, def := obsFixture()
			base = append(base, ragRunTool("bind_finding_traffic", "alpha beta bind traffic to finding"), ragRunTool("unbound_best", "alpha beta alpha beta"))
			rig := newRagRig(t, def)
			t.Setenv(toolRAGEnv, "")
			SetFindingTrafficBindingEnabled(func() bool { return binding })
			var resolved []actool.CoreTool
			SetToolResolve(func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool {
				resolved = nil
				for _, tool := range in {
					if tool.Name() != "unbound_best" { // dropped by the tools table
						resolved = append(resolved, tool)
					}
				}
				return resolved
			})
			out, _, cleanup := AugmentTools(ragCtx("alpha beta"), "worker", base)
			cleanup()
			r := rig.observed()[0]
			names := map[string]bool{}
			for _, tool := range resolved {
				names[tool.Name()] = true
			}
			if len(r.Ranking) == 0 || slices.Contains(r.Ranking, "unbound_best") {
				t.Fatalf("ranking = %v", r.Ranking)
			}
			for _, n := range r.Ranking {
				if !names[n] || toolRAGKeep(def.Deferred)[n] {
					t.Errorf("ranked %s: not a candidate of the resolved list", n)
				}
			}
			for _, tool := range out {
				if !names[tool.Name()] {
					t.Errorf("%s is not in the resolved list", tool.Name())
				}
			}
			if has := slices.ContainsFunc(out, func(tool actool.CoreTool) bool { return tool.Name() == "bind_finding_traffic" }); has != binding {
				t.Errorf("bind_finding_traffic in the final list = %v with binding=%v", has, binding)
			}
		})
	}
}

// AC14: a scorer error or panic is recorded in the row, never breaks the run, and
// the log line carries the type and nothing of the error text or the panic value.
func TestToolRAGScorerFailureIsContentFree(t *testing.T) {
	for name, tc := range map[string]struct {
		scorer  ToolScorer
		outcome string
		kind    string
		typ     string
	}{
		"error": {func(string, []actool.CoreTool) ([]float64, error) { return nil, errors.New("CANARY goal text") }, toolRAGScorerError, "scorer error", "*errors.errorString"},
		"panic": {func(string, []actool.CoreTool) ([]float64, error) { panic("CANARY goal text") }, toolRAGObserverPanic, "observer panic", "string"},
	} {
		t.Run(name, func(t *testing.T) {
			base, def := obsFixture()
			rig := newRagRig(t, def)
			t.Setenv(toolRAGEnv, "")
			logs := dbtest.CaptureLog(t)
			toolRAGScorer = tc.scorer
			for range 3 {
				out, _, cleanup := AugmentTools(ragCtx("alpha beta"), "planner", base)
				cleanup()
				if len(out) != len(base) {
					t.Fatalf("list changed: %d tools", len(out))
				}
			}
			rows := rig.observed()
			if len(rows) != 3 || rows[0].Outcome != tc.outcome || len(rows[0].Ranking) != 0 {
				t.Fatalf("rows = %+v", rows)
			}
			out := strings.TrimSpace(logs())
			want := "[tool-rag] observation " + tc.kind + "; 1 since start (type " + tc.typ + ")"
			if out != want {
				t.Fatalf("log = %q, want one line %q (rate limit)", out, want)
			}
			if toolRAGProblems.Count(tc.kind) != 3 {
				t.Fatalf("count = %d, want 3", toolRAGProblems.Count(tc.kind))
			}
		})
	}
}

func TestToolRAGLogIsRateLimitedPerKind(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	now := time.Unix(1000, 0)
	l := NewToolRAGLog()
	l.now = func() time.Time { return now }
	for range 5 {
		l.Report("drop", "queue full")
	}
	l.Report("insert error", "SQLSTATE 08006") // another kind has its own budget
	now = now.Add(59 * time.Second)
	l.Report("drop", "queue full")
	now = now.Add(2 * time.Second)
	l.Report("drop", "queue full")
	l.Report("scorer error", "")
	want := []string{
		"[tool-rag] observation drop; 1 since start (queue full)",
		"[tool-rag] observation insert error; 1 since start (SQLSTATE 08006)",
		"[tool-rag] observation drop; 7 since start (queue full)",
		"[tool-rag] observation scorer error; 1 since start",
	}
	if got := strings.Split(strings.TrimSpace(logs()), "\n"); !slices.Equal(got, want) {
		t.Fatalf("log =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if l.Count("drop") != 7 || l.Count("never") != 0 {
		t.Fatalf("counts drop=%d never=%d", l.Count("drop"), l.Count("never"))
	}
}

func TestToolObsNameRefusesEmpty(t *testing.T) {
	if got := toolObsName(""); got != toolRAGUnknownName {
		t.Fatalf("empty name -> %q", got)
	}
}

// calledRows delivers the row (cleanup) and returns what it recorded as called.
func calledRows(t *testing.T, rig *ragRig, cleanup func()) map[string]db.ToolRAGCall {
	t.Helper()
	cleanup()
	rows := rig.observed()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	return rows[0].Called
}

// AC17 (called) and AC11 (counter): the name rule, the 256-name cap with _overflow,
// exact counts under concurrency, and nothing after the flush.
func TestToolRAGCalledCounterRulesAndConcurrency(t *testing.T) {
	t.Run("name rule and cap", func(t *testing.T) {
		var base []actool.CoreTool
		for i := range 300 {
			base = append(base, ragRunTool(fmt.Sprintf("t%03d", i), "tool"))
		}
		base = append(base, ragRunTool(strings.Repeat("x", 129), "long"), ragRunTool("bell\x07", "control"))
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		out, _, cleanup := AugmentTools(ragCtx(""), "planner", base)
		for _, tool := range out {
			if _, err := tool.Call(context.Background(), json.RawMessage(`{}`), nil); err != nil {
				t.Fatal(err)
			}
		}
		called := calledRows(t, rig, cleanup)
		if len(called) != 257 {
			t.Fatalf("entries = %d, want 256 names plus _overflow", len(called))
		}
		// The list order decides which names overflow; what is fixed: _overflow
		// exists, every call is counted once, and no name that breaks the rule is stored.
		total := 0
		for name, c := range called {
			total += c.N
			if name == strings.Repeat("x", 129) || name == "bell\x07" {
				t.Errorf("stored a name that breaks the rule: %q", name)
			}
		}
		if called[toolRAGOverflowName].N == 0 || total != 302 {
			t.Fatalf("overflow=%d total=%d", called[toolRAGOverflowName].N, total)
		}
	})
	t.Run("bad names share _unknown", func(t *testing.T) {
		base := []actool.CoreTool{ragRunTool(strings.Repeat("y", 129), "long"), ragRunTool("a\nb", "control"), ragRunTool("fine", "ok")}
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		out, _, cleanup := AugmentTools(ragCtx(""), "worker", base)
		for _, tool := range out {
			tool.Call(context.Background(), json.RawMessage(`{}`), nil)
		}
		called := calledRows(t, rig, cleanup)
		if called[toolRAGUnknownName].N != 2 || called["fine"].N != 1 || len(called) != 2 || called[toolRAGUnknownName].Class != toolClassOther {
			t.Fatalf("called = %+v", called)
		}
	})
	t.Run("50 concurrent calls are counted exactly, a call after the flush is ignored", func(t *testing.T) {
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		out, _, cleanup := AugmentTools(ragCtx(""), "worker", []actool.CoreTool{ragRunTool("alpha", "x"), ragRunTool("record_fact", "x")})
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() { out[i%2].Call(context.Background(), json.RawMessage(`{}`), nil) })
		}
		wg.Wait()
		called := calledRows(t, rig, cleanup)
		if called["alpha"].N != 25 || called["record_fact"].N != 25 || called["alpha"].Class != toolClassCandidate || called["record_fact"].Class != toolClassKeep {
			t.Fatalf("called = %+v", called)
		}
		out[0].Call(context.Background(), json.RawMessage(`{}`), nil)
		if got := rig.observed()[0].Called["alpha"].N; got != 25 {
			t.Fatalf("a call after the flush was counted: %d", got)
		}
	})
	t.Run("a panicking tool is still counted and still guarded", func(t *testing.T) {
		boom := actool.Build(actool.Spec{Name: "boom", Description: "x", Schema: map[string]any{"type": "object"},
			Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) { panic("boom") }})
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		logs := dbtest.CaptureLog(t)
		out, _, cleanup := AugmentTools(ragCtx(""), "worker", []actool.CoreTool{boom})
		res, err := out[0].Call(context.Background(), json.RawMessage(`{}`), nil)
		if err != nil || !res.IsError {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if calledRows(t, rig, cleanup)["boom"].N != 1 || !strings.Contains(logs(), "[tools] boom panic") {
			t.Fatal("not counted or not guarded")
		}
	})
}

// AC15 (agent side): the flush is non-blocking and its failure never reaches the run;
// the original cleanup (the MCP closer) runs even when the flush panics or the
// goroutine exits inside the observer (a recover alone would not catch that).
func TestToolRAGOriginalCleanupAlwaysRuns(t *testing.T) {
	t.Run("observer panics", func(t *testing.T) {
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		logs := dbtest.CaptureLog(t)
		SetToolRAGObserver(func(ToolRAGObservation) { panic("CANARY") })
		_, _, cleanup := AugmentTools(ragCtx(ragQuery), "worker", ragCatalog(25))
		cleanup()
		if rig.cleanups.Load() != 1 {
			t.Fatal("original cleanup did not run")
		}
		if out := strings.TrimSpace(logs()); out != "[tool-rag] observation flush panic; 1 since start (type string)" {
			t.Fatalf("log = %q", out)
		}
	})
	t.Run("observer exits the goroutine", func(t *testing.T) {
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		SetToolRAGObserver(func(ToolRAGObservation) { runtime.Goexit() })
		_, _, cleanup := AugmentTools(ragCtx(ragQuery), "worker", ragCatalog(25))
		done := make(chan struct{})
		go func() { defer close(done); cleanup() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup goroutine never finished")
		}
		if rig.cleanups.Load() != 1 {
			t.Fatal("original cleanup was skipped when the observer left the goroutine")
		}
	})
	t.Run("run context cancelled before cleanup", func(t *testing.T) {
		rig := newRagRig(t, DeferredInfo{})
		t.Setenv(toolRAGEnv, "")
		ctx, cancel := context.WithCancel(ragCtx(ragQuery))
		_, _, cleanup := AugmentTools(ctx, "worker", ragCatalog(25))
		cancel()
		cleanup()
		if len(rig.observed()) != 1 {
			t.Fatal("cancelled run produced no row")
		}
	})
}

// AC16 (agent side): the row holds names and counts, never the query or a description.
func TestToolRAGRowHoldsNoQueryOrDescriptionText(t *testing.T) {
	base := append(ragCatalog(25), ragRunTool("canary_tool", "DESC-CANARY-7 password=hunter2 https://canary.example/login"))
	rig := newRagRig(t, DeferredInfo{})
	t.Setenv(toolRAGEnv, "")
	_, _, cleanup := AugmentTools(ragCtx("scan canary-host.example with password=hunter2 DESC-CANARY-7"), "worker", base)
	cleanup()
	raw, err := json.Marshal(rig.observed()[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"canary-host.example", "hunter2", "DESC-CANARY-7", "canary.example"} {
		if strings.Contains(string(raw), canary) {
			t.Errorf("row holds %q: %s", canary, raw)
		}
	}
	if !strings.Contains(string(raw), "canary_tool") {
		t.Error("the tool name itself should be stored")
	}
}

// AC11: the counter is on the wrapped instance that ExecuteExtraTool resolves, so a
// deferred MCP tool is counted under its real name; a refused locked call is not.
func TestToolRAGCountsCallsThroughTheSession(t *testing.T) {
	cand, keepTool := ragRunTool("cand_tool", "alpha"), ragRunTool("record_fact", "keep")
	g1, s1 := ragRunTool("mcp__srv__g1", "global"), ragRunTool("mcp__srv__s1", "gated")
	def := DeferredInfo{
		Deferred: []string{"mcp__srv__g1", "mcp__srv__s1"}, GlobalNames: []string{"mcp__srv__g1"},
		Unlock: actool.NewUnlockSet("mcp__srv__g1"),
	}
	skill := actool.Build(actool.Spec{Name: "Skill", Description: "load a skill", Schema: map[string]any{"type": "object"},
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			def.Unlock.Add("mcp__srv__s1")
			return actool.Text("loaded"), nil
		}})
	rig := newRagRig(t, def)
	t.Setenv(toolRAGEnv, "")
	tools, got, cleanup := AugmentTools(ragCtx("alpha"), "worker", []actool.CoreTool{cand, keepTool, g1, s1, skill})
	runScriptedSession(t, tools, got, []scriptedCall{
		{"cand_tool", `{}`}, {"cand_tool", `{}`}, {"record_fact", `{}`},
		{actool.ExecuteExtraToolName, `{"tool_name":"mcp__srv__g1","params":{}}`},
		{actool.ExecuteExtraToolName, `{"tool_name":"mcp__srv__s1","params":{}}`}, // locked: refused, not counted
		{"Skill", `{}`},
		{actool.ExecuteExtraToolName, `{"tool_name":"mcp__srv__s1","params":{}}`},
	})
	called := calledRows(t, rig, cleanup)
	want := map[string]db.ToolRAGCall{
		"cand_tool": {N: 2, Class: toolClassCandidate}, "record_fact": {N: 1, Class: toolClassKeep},
		"mcp__srv__g1": {N: 1, Class: toolClassDeferredGlobal}, "Skill": {N: 1, Class: toolClassKeep},
		"mcp__srv__s1": {N: 1, Class: toolClassDeferredLocked},
	}
	if len(called) != len(want) {
		t.Fatalf("called = %+v", called)
	}
	for name, w := range want {
		if called[name] != w {
			t.Errorf("%s = %+v, want %+v", name, called[name], w)
		}
	}
	if rig.observed()[0].UnlockedDuringRun != 1 {
		t.Errorf("unlocked during run = %d, want 1", rig.observed()[0].UnlockedDuringRun)
	}
}

// BenchmarkToolRAGObserve measures the delay observation adds before the first model
// request of a planner round or worker run (plan 3.9): realistic input, and the capped
// maximum.
func BenchmarkToolRAGObserve(b *testing.B) {
	build := func(n, textBytes int) []actool.CoreTool {
		tools := make([]actool.CoreTool, n)
		for i := range tools {
			text := strings.Repeat(fmt.Sprintf("word%d scanner probe ", i%17), textBytes/20)
			tools[i] = ragTool(fmt.Sprintf("tool_%03d", i), text, "target", "port")
		}
		return tools
	}
	for name, tc := range map[string]struct {
		tools    []actool.CoreTool
		queryLen int
	}{
		"typical":   {build(40, 300), 300}, // real descriptions are a few hundred bytes
		"realistic": {build(40, 1024), 1024},
		"capped":    {build(512, 8192), 4096},
	} {
		query := strings.Repeat("scanner probe word3 target ", tc.queryLen/27+1)[:tc.queryLen]
		ctx := WithToolQuery(context.Background(), query)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				if o := observeToolRAG(ctx, "worker", tc.tools, DeferredInfo{}, nil); o == nil || len(o.Ranking) == 0 {
					b.Fatal("no ranking")
				}
			}
		})
	}
}

// BenchmarkGuardedToolCall measures what the call counter adds to one tool call.
func BenchmarkGuardedToolCall(b *testing.B) {
	inner := ragRunTool("record_fact", "x")
	in := json.RawMessage(`{}`)
	ctx := context.Background()
	b.Run("plain", func(b *testing.B) {
		g := guardPanic(inner)
		b.ReportAllocs()
		for range b.N {
			g.Call(ctx, in, nil)
		}
	})
	b.Run("counted", func(b *testing.B) {
		obs := &toolObservation{Classes: map[string]string{"record_fact": toolClassKeep}}
		g := &guardedTool{CoreTool: inner, obs: obs}
		b.ReportAllocs()
		for range b.N {
			g.Call(ctx, in, nil)
		}
	})
}

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/llmrec"
)

type scriptedCall struct{ name, input string }

// runScriptedSession drives a real agentcore session whose provider is a script:
// one tool call per model turn, then a final text. The tools and deferred wiring are
// exactly what AugmentTools returned, as the planner and worker hand them over.
func runScriptedSession(t *testing.T, tools []actool.CoreTool, def DeferredInfo, calls []scriptedCall) {
	t.Helper()
	var turns [][]llm.StreamEvent
	for i, c := range calls {
		turns = append(turns, []llm.StreamEvent{
			{Type: llm.SEToolUseStart, ToolID: fmt.Sprintf("c%d", i), ToolName: c.name},
			{Type: llm.SEToolInputJSON, Text: c.input},
			{Type: llm.SEMessageDelta, StopReason: "tool_use"},
			{Type: llm.SEMessageStop},
		})
	}
	turns = append(turns, []llm.StreamEvent{{Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}})
	n := 0
	callModel := func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		evs := turns[n]
		n++
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, e := range evs {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
	sess := agentcore.NewSession(agentcore.Options{
		SystemPrompt: []string{"sys"}, Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock,
		PermissionMode: permission.ModeBypass, MaxTurns: len(turns) + 2,
		Deps: harness.QueryDeps{CallModel: callModel},
	})
	defer sess.Close()
	for ev, err := range sess.Prompt(t.Context(), "go") {
		if err != nil {
			t.Fatalf("session: %v", err)
		}
		_ = ev
	}
	if n != len(turns) {
		t.Fatalf("script consumed %d of %d turns", n, len(turns))
	}
}

var wallClock = regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d:\d\d [A-Z]{2,5}`)

// requestKey is a captured model request as bytes, with the wall-clock stamp the
// planner and worker prompts embed ("Now: ...") masked: it moves with the second.
func requestKey(t *testing.T, req llm.CompletionRequest) string {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return wallClock.ReplaceAllString(string(raw), "<now>")
}

// ragRunFixture installs host tools like the server does for a real run: a traffic
// tool, a global and a skill-gated deferred MCP tool, and one tool whose description
// is stuffed with the goal's words (AC18).
func ragRunFixture(t *testing.T) (def DeferredInfo, extra []actool.CoreTool) {
	t.Helper()
	extra = []actool.CoreTool{
		ragTool("traffic_search", "Search recorded traffic", "host"),
		ragTool("mcp__srv__global", "global mcp tool"),
		ragTool("mcp__srv__gated", "gated mcp tool"),
		ragTool("zz_stuffed", strings.Repeat("ssrf login service find ", 20)),
	}
	def = DeferredInfo{
		Deferred: []string{"mcp__srv__global", "mcp__srv__gated"}, GlobalNames: []string{"mcp__srv__global"},
		Unlock: actool.NewUnlockSet("mcp__srv__global"),
	}
	return def, extra
}

const ragRunGoal = "find ssrf in the login service"

// AC2 and AC18, and the pin for llm_usage.worker: a real planner round and a real
// worker run produce the same model request with observation off and on, even when
// a stuffed description ranks first; the usage ledger names them "planner" and
// "worker" (the day-14 SQL relies on that).
func TestToolRAGRequestsAreIdenticalOffAndObserve(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	prev := dataNonceSource
	dataNonceSource = func() string { return "fixednonce" }
	t.Cleanup(func() { dataNonceSource = prev })
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("observe fixture", ragRunGoal)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	intentID, err := ts.AddIntent(map[string]any{"summary": "probe the login service for ssrf"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ts.GetNode(intentID)
	if err != nil {
		t.Fatal(err)
	}
	def, extra := ragRunFixture(t)
	rig := newRagRig(t, def)
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return extra, def, func() { rig.cleanups.Add(1) }
	})
	workDir := t.TempDir()

	type captured struct {
		planner, worker string
		usage           []db.LLMUsage
	}
	run := func(mode string) captured {
		t.Setenv(toolRAGEnv, mode)
		var c captured
		var request llm.CompletionRequest
		provider := llmrec.Wrap(&plannerContextFixtureProvider{receive: func(req llm.CompletionRequest) { request = req }}, nil, "m", "p", "", "", func() bool { return false })
		ctx := llmrec.WithUsageObserver(t.Context(), func(u db.LLMUsage) { c.usage = append(c.usage, u) })
		p := NewPlanner(provider, "fixture", workDir, nil, 100000, 1)
		p.SetNonStreaming(func() bool { return true })
		if _, _, err := p.Plan(ctx, exp, nil, ts, ragRunGoal, nil, nil); err != nil {
			t.Fatal(err)
		}
		c.planner = requestKey(t, request)
		w := NewWorker(provider, "fixture", workDir, nil, 100000, 1)
		w.SetNonStreaming(func() bool { return true })
		if _, _, err := w.Execute(ctx, "fixture", exp, nil, ts, intent, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		c.worker = requestKey(t, request)
		return c
	}
	off := run("off")
	if len(rig.observed()) != 0 {
		t.Fatalf("off produced %d rows", len(rig.observed()))
	}
	on := run("observe")
	if off.planner != on.planner {
		t.Errorf("planner request differs between off and observe")
	}
	if off.worker != on.worker {
		t.Errorf("worker request differs between off and observe")
	}
	if strings.Contains(on.planner, "zz_stuffed") == false || strings.Contains(on.worker, "zz_stuffed") == false {
		t.Error("the stuffed tool must still be offered unchanged")
	}
	rows := rig.observed()
	if len(rows) != 2 || rows[0].AgentKey != "planner" || rows[1].AgentKey != "worker" {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.TaskID != exp || r.ExplorationID != exp || r.DeferredCount != 2 || r.LockedAtStart != 1 || len(r.Ranking) == 0 || r.Ranking[0] != "zz_stuffed" {
			t.Errorf("%s row = %+v", r.AgentKey, r)
		}
	}
	if rows[0].IntentID != 0 || rows[1].IntentID != intentID {
		t.Errorf("intent ids = %d, %d", rows[0].IntentID, rows[1].IntentID)
	}
	// llm_usage.worker: the lane names the day-14 coverage SQL filters on.
	lanes := map[string][]int64{}
	for _, u := range on.usage {
		lanes[u.Worker] = append(lanes[u.Worker], u.IntentID)
	}
	if len(lanes["planner"]) == 0 || len(lanes["worker"]) == 0 || slices.Contains(lanes["worker"], 0) || !slices.Equal(slices.Compact(lanes["worker"]), []int64{intentID}) || slices.Compact(lanes["planner"])[0] != 0 {
		t.Errorf("usage lanes = %v, want planner with intent 0 and worker with intent %d", lanes, intentID)
	}
}

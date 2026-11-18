package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Phase D.1, S3 red tests for fact provenance (D5, AC17), on PostgreSQL. They never touch
// prove_goal: its behaviour is unchanged in D.1 (the refusal is deferred, decision Q8).

var wantFactSources = []string{"target_response", "tool_output", "third_party_intel", "inference"}

// factFixture is a fresh exploration; its rows are removed with it.
func factFixture(t *testing.T) *db.ExplorationStore {
	t.Helper()
	d := requireTestDB(t)
	t.Cleanup(func() { d.Close() })
	exp, err := d.CreateExploration("fact provenance fixture", "fixture goal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	return d.Exploration(exp)
}

func recordFactCall(t *testing.T, tsx *ToolSet, input string) actool.Result {
	t.Helper()
	res, err := tsx.recordFact().Call(t.Context(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("record_fact: %v", err)
	}
	return res
}

func factCount(t *testing.T, ts *db.ExplorationStore) int {
	t.Helper()
	facts, err := ts.ListByKind(db.KindFact, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(facts)
}

// storedFact returns a fact's declared source and its server-set origin.
func storedFact(t *testing.T, ts *db.ExplorationStore, id int64) (source, origin string) {
	t.Helper()
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		t.Fatalf("node %d: %v", id, err)
	}
	var p map[string]any
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatal(err)
	}
	source, _ = p["source"].(string)
	return source, n.Origin
}

func recordedFactID(t *testing.T, res actool.Result) int64 {
	t.Helper()
	out := res.Flatten()
	if res.IsError || !strings.HasPrefix(out, "fact recorded: ") {
		t.Fatalf("record_fact did not record a fact: is_error=%v %q", res.IsError, out)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(out, "fact recorded: "), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// T-FACT-1 (AC17, C4): record_fact accepts source from the closed enum, stores an empty
// source as inference, and refuses operator_input and unknown values for every model
// writer, without writing a fact. The server-set origin is the writer, whatever the model
// declares. The schema offered to the model lists the same four values.
func TestFactSourceEnum(t *testing.T) {
	ts := factFixture(t)

	// The enum the model sees.
	props, _ := NewToolSet(ts, "worker-1").recordFact().InputSchema()["properties"].(map[string]any)
	src, _ := props["source"].(map[string]any)
	var enum []string
	switch v := src["enum"].(type) {
	case []string:
		enum = v
	case []any:
		for _, e := range v {
			enum = append(enum, fmt.Sprint(e))
		}
	}
	if !slices.Equal(enum, wantFactSources) {
		t.Errorf("record_fact schema source enum = %v, want %v (operator_input is not offered to models)", enum, wantFactSources)
	}

	for _, writer := range []string{"worker-3", "planner", "human"} { // human is the main agent's origin (C14)
		tsx := NewToolSet(ts, writer)
		for _, want := range wantFactSources {
			t.Run(writer+"/accepts "+want, func(t *testing.T) {
				id := recordedFactID(t, recordFactCall(t, tsx, `{"summary":"fact from `+want+`","source":"`+want+`"}`))
				if source, origin := storedFact(t, ts, id); source != want || origin != writer {
					t.Fatalf("stored source=%q origin=%q, want %q / %q", source, origin, want, writer)
				}
			})
		}
		t.Run(writer+"/empty becomes inference", func(t *testing.T) {
			for _, in := range []string{`{"summary":"no source"}`, `{"summary":"empty source","source":""}`, `{"summary":"blank source","source":"  "}`} {
				if source, _ := storedFact(t, ts, recordedFactID(t, recordFactCall(t, tsx, in))); source != "inference" {
					t.Errorf("%s stored source %q, want inference", in, source)
				}
			}
		})
		t.Run(writer+"/refuses", func(t *testing.T) {
			for _, bad := range []string{"operator_input", "OPERATOR_INPUT", " operator_input ", "target", "human", "the target told me"} {
				before := factCount(t, ts)
				res := recordFactCall(t, tsx, `{"summary":"claims `+bad+`","source":`+jsonString(bad)+`}`)
				if !res.IsError {
					t.Errorf("source %q was accepted: %q", bad, res.Flatten())
					continue
				}
				if !strings.Contains(res.Flatten(), "target_response") {
					t.Errorf("source %q: the refusal does not list the allowed values: %q", bad, res.Flatten())
				}
				if after := factCount(t, ts); after != before {
					t.Errorf("source %q: a refused call wrote %d fact(s)", bad, after-before)
				}
			}
		})
	}

	t.Run("batch reports the bad item and writes the good one", func(t *testing.T) {
		tsx := NewToolSet(ts, "worker-4")
		before := factCount(t, ts)
		res := recordFactCall(t, tsx, `{"facts":[{"summary":"port 22 open","source":"tool_output"},{"summary":"claims operator","source":"operator_input"}]}`)
		var batch struct {
			IDs    []int64           `json:"ids"`
			Errors map[string]string `json:"errors"`
		}
		if err := json.Unmarshal([]byte(res.Flatten()), &batch); err != nil || len(batch.IDs) != 2 {
			t.Fatalf("batch result: %q err=%v", res.Flatten(), err)
		}
		if batch.IDs[0] == 0 || batch.IDs[1] != 0 || batch.Errors["1"] == "" {
			t.Fatalf("batch: ids=%v errors=%v, want item 0 written and item 1 refused", batch.IDs, batch.Errors)
		}
		if source, origin := storedFact(t, ts, batch.IDs[0]); source != "tool_output" || origin != "worker-4" {
			t.Errorf("stored source=%q origin=%q", source, origin)
		}
		if after := factCount(t, ts); after != before+1 {
			t.Errorf("batch wrote %d facts, want 1", after-before)
		}
	})
}

// T-FACT-2 (AC17, C4): the writer (origin, set by the server) and the declared source
// surface in compactNode, list_facts and the planner's recent_facts; a node without them
// carries neither key.
func TestFactOriginSurfaced(t *testing.T) {
	t.Run("compactNode", func(t *testing.T) {
		n := &db.Node{ID: 7, Kind: db.KindFact, State: "confirmed", Origin: "worker-2", Payload: json.RawMessage(`{"summary":"s","source":"tool_output"}`)}
		if m := compactNode(n); m["origin"] != "worker-2" || m["source"] != "tool_output" {
			t.Fatalf("compactNode dropped the provenance: %#v", m)
		}
		legacy := compactNode(&db.Node{ID: 8, Kind: db.KindFact, State: "confirmed", Payload: json.RawMessage(`{"summary":"legacy"}`)})
		if _, ok := legacy["origin"]; ok {
			t.Errorf("an empty origin was surfaced: %#v", legacy)
		}
		if _, ok := legacy["source"]; ok {
			t.Errorf("an absent source was surfaced: %#v", legacy)
		}
	})

	ts := factFixture(t)
	// Written straight to the store, so this test needs only the display, not record_fact.
	id, err := ts.AddNode(db.KindFact, map[string]any{"summary": "server header is nginx", "confidence": "observed", "source": "tool_output"}, 5, "confirmed", "worker-5", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := func(where string, m map[string]any) {
		t.Helper()
		if m["origin"] != "worker-5" || m["source"] != "tool_output" {
			t.Errorf("%s: origin=%v source=%v in %#v", where, m["origin"], m["source"], m)
		}
	}
	planner := NewToolSet(ts, "planner")

	t.Run("recent_facts", func(t *testing.T) {
		recent, _ := planner.graphOverviewData()["recent_facts"].([]map[string]any)
		for _, m := range recent {
			if m["id"] == id {
				want("recent_facts", m)
				return
			}
		}
		t.Fatalf("fact %d is not in recent_facts: %#v", id, recent)
	})

	t.Run("list_facts", func(t *testing.T) {
		facts, _ := callReadJSON(t, planner.listFacts(), `{}`).(map[string]any)["facts"].([]any)
		for _, raw := range facts {
			if m, _ := raw.(map[string]any); m != nil && int64(m["id"].(float64)) == id {
				want("list_facts", m)
				return
			}
		}
		t.Fatalf("fact %d is not in list_facts: %#v", id, facts)
	})
}

// AC22 (observability; not the deferred T-FACT-3): the refusal log line carries a fixed code only, never the fact text or
// the source value the model supplied.
func TestFactRefusalLogContentFree(t *testing.T) {
	ts := factFixture(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	res := recordFactCall(t, NewToolSet(ts, "worker-9"), `{"summary":"SECRET-SUMMARY-TEXT","source":"SECRET-SOURCE-VALUE"}`)
	if !res.IsError {
		t.Fatalf("a bad source was accepted: %q", res.Flatten())
	}
	out := buf.String()
	if !strings.Contains(out, "[facts] record_refused reason=invalid_source") {
		t.Errorf("no refusal log line: %q", out)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(strings.ToLower(res.Flatten()), "secret-source") {
		t.Errorf("refusal leaks content: log=%q result=%q", out, res.Flatten())
	}
}

// Q8 guard (not the deferred T-FACT-4): prove_goal is untouched in D.1, so a fact of any source still proves a goal.
func TestProveGoalIgnoresFactSource(t *testing.T) {
	ts := factFixture(t)
	tsx := NewToolSet(ts, "planner")
	for _, src := range wantFactSources {
		t.Run(src, func(t *testing.T) {
			fact := recordedFactID(t, recordFactCall(t, tsx, `{"summary":"proof by `+src+`","confidence":"observed","source":"`+src+`"}`))
			goal, err := ts.AddNode(db.KindGoal, map[string]any{"summary": "goal " + src}, 5, "open", "planner", nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := tsx.proveGoal().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"goal_id":%d,"evidence_id":%d}`, goal, fact)), nil)
			if err != nil || res.IsError || !strings.Contains(res.Flatten(), "marked met") {
				t.Fatalf("prove_goal with a %s fact: err=%v %q", src, err, res.Flatten())
			}
		})
	}
}

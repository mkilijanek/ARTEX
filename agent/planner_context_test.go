package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestPlannerJournalReplayRemovalsAndResync(t *testing.T) {
	first, err := advancePlannerJournal(nil, map[string]any{"facts": 1, "hints": []any{"old"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(first)
	second, err := advancePlannerJournal(raw, map[string]any{"facts": 2})
	if err != nil {
		t.Fatal(err)
	}
	if second.BaseVersion != 1 || second.Version != 2 || len(second.Deltas) != 1 {
		t.Fatal(second)
	}
	current, err := second.materialize()
	if err != nil || string(current["facts"]) != "2" || current["hints"] != nil {
		t.Fatalf("replay: %s %v", current, err)
	}
	if string(second.Base["facts"]) != "1" {
		t.Fatal("base was mutated")
	}
	j := second
	for range 7 {
		raw, _ = json.Marshal(j)
		j, err = advancePlannerJournal(raw, map[string]any{"facts": 3})
		if err != nil {
			t.Fatal(err)
		}
	}
	if j.Epoch != first.Epoch || j.BaseVersion != 9 || len(j.Deltas) != 0 || j.ResyncReason != "periodic" {
		t.Fatal(j)
	}
	// A corrupt chain is not silently replayed to the model. Recover from the
	// source graph into a new independently usable snapshot/epoch.
	j.CurrentChecksum = "corrupt"
	raw, _ = json.Marshal(j)
	repaired, err := advancePlannerJournal(raw, map[string]any{"facts": 4})
	if err != nil || repaired.Epoch == j.Epoch || repaired.ResyncReason != "invalid_checkpoint" {
		t.Fatalf("%+v %v", repaired, err)
	}
}

func TestPlannerJournalSizeResyncAndMandatoryOverflow(t *testing.T) {
	first, _ := advancePlannerJournal(nil, map[string]any{"hints": strings.Repeat("a", 24000)})
	raw, _ := json.Marshal(first)
	next, _ := advancePlannerJournal(raw, map[string]any{"hints": strings.Repeat("b", 24000)})
	encoded, _ := json.Marshal(next)
	if len(encoded) > plannerPacketBudget || next.ResyncReason != "byte_budget" || len(next.Deltas) != 0 {
		t.Fatalf("bytes=%d journal=%+v", len(encoded), next)
	}
	large := strings.Repeat("keep mandatory ", 5000)
	next, _ = advancePlannerJournal(nil, map[string]any{"hints": large})
	materialized, err := next.materialize()
	var hint string
	_ = json.Unmarshal(materialized["hints"], &hint)
	if err != nil || hint != large || !next.MandatoryOverflow {
		t.Fatal("mandatory evidence lost/overflow unreported", err)
	}
}

func TestPlannerWireViewHasNoReplayOverhead(t *testing.T) {
	var previous json.RawMessage
	for round := range 12 {
		data := map[string]any{"task": "fixture", "recent_facts": []any{map[string]any{"id": 1, "state": fmt.Sprint(round), "summary": strings.Repeat("evidence ", 40)}}}
		j, err := advancePlannerJournal(previous, data)
		if err != nil {
			t.Fatal(err)
		}
		previous, _ = json.Marshal(j)
		wire := strings.TrimPrefix(renderPlannerJournal(j), plannerSnapshotPrefix)
		expected, _ := json.Marshal(projectOverview(data))
		if wire != string(expected) {
			t.Fatal("model did not receive exact projected current state", wire)
		}
	}
}

func TestPlannerJournalKeepsImmutableBaseBeforeChangingMetadata(t *testing.T) {
	first, _ := advancePlannerJournal(nil, map[string]any{"task": map[string]any{"description": "fixture", "goal": "stable goal"}, "facts": 1})
	raw, _ := json.Marshal(first)
	second, _ := advancePlannerJournal(raw, map[string]any{"task": map[string]any{"description": "fixture", "goal": "stable goal"}, "facts": 2})
	araw, _ := json.Marshal(first)
	braw, _ := json.Marshal(second)
	a, b := string(araw), string(braw)
	if a[:strings.Index(a, `"deltas":`)] != b[:strings.Index(b, `"deltas":`)] {
		t.Fatal("version metadata invalidated stable base prefix")
	}
}

func TestPlannerCredentialsStayLiveAndOutsideCheckpoint(t *testing.T) {
	data := map[string]any{"facts": 1, "task_credentials": []any{map[string]any{"username": "fixture", "password": "fresh-secret"}}, "task_credentials_note": "current only"}
	j, err := advancePlannerJournal(nil, data)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(j)
	if strings.Contains(string(raw), "fresh-secret") || strings.Contains(string(raw), "task_credentials") {
		t.Fatal("secret retained in checkpoint")
	}
	live := renderPlannerCredentials(data)
	if !strings.Contains(live, "fresh-secret") || !strings.Contains(live, "current only") {
		t.Fatal("live credentials dropped")
	}
}

func TestPlannerJournalPersistsAcrossFreshHandlesAndSerializesUpdates(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("context fixture", "preserve all evidence")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	data := map[string]any{"task": map[string]any{"goal": "fixture", "description": "nonempty"}, "facts": json.Number("9007199254740993")}
	first, err := plannerContextCheckpoint(t.Context(), d.Exploration(exp), data)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh DB handle simulates process restart and exercises JSONB reordering.
	dsn, _, _ := db.DSN()
	reopened, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := plannerContextCheckpoint(t.Context(), reopened.Exploration(exp), data)
	if err != nil || second.Epoch != first.Epoch || second.Version != 2 || len(second.Deltas[0].Replace) != 0 {
		t.Fatalf("restart: %+v %v", second, err)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := plannerContextCheckpoint(t.Context(), d.Exploration(exp), data); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	last, err := plannerContextCheckpoint(t.Context(), reopened.Exploration(exp), data)
	if err != nil || last.Version != 15 {
		t.Fatalf("lost concurrent versions: %+v %v", last, err)
	}
	materialized, err := last.materialize()
	if err != nil || string(materialized["facts"]) != "9007199254740993" {
		t.Fatalf("numeric identity lost: %s %v", materialized, err)
	}
	// Failed writes must not consume a version or lose the valid checkpoint.
	_, err = d.Exploration(exp).UpdatePlannerContext(t.Context(), func(json.RawMessage) (json.RawMessage, error) { return nil, fmt.Errorf("fixture failure") })
	if err == nil {
		t.Fatal("callback failure ignored")
	}
	next, err := plannerContextCheckpoint(t.Context(), d.Exploration(exp), data)
	if err != nil || next.Version != 16 {
		t.Fatal(next, err)
	}
}

func TestPlannerJournalMeasuresWholeReplayAndIncrementalBytes(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("measure fixture", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for round := range 3 {
		data := map[string]any{"task": strings.Repeat("stable context ", 300), "facts": round}
		j, err := plannerContextCheckpoint(t.Context(), d.Exploration(exp), data)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(j)
		if !strings.Contains(output.String(), fmt.Sprintf("packet_bytes=%d", len(raw))) {
			t.Fatal(output.String())
		}
	}
	for _, field := range []string{"round=3", "source_tokens_estimate=", "packet_tokens_estimate=", "estimator=utf8_bytes_div4", "update_bytes="} {
		if !strings.Contains(output.String(), field) {
			t.Fatal(output.String())
		}
	}
}

func TestPlannerFreshSessionReceivesReplayAndReportedUsage(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("fresh session fixture", "preserve current state")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	var requests []llm.CompletionRequest
	provider := &plannerContextFixtureProvider{receive: func(req llm.CompletionRequest) { requests = append(requests, req) }}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for round := range 2 {
		if _, err := ts.AddNode(db.KindHint, map[string]any{"text": fmt.Sprintf("round %d fixture", round)}, 0, "active", "fixture", nil); err != nil {
			t.Fatal(err)
		}
		p := NewPlanner(provider, "fixture", t.TempDir(), nil, 100000, 1)
		p.SetNonStreaming(func() bool { return true })
		if _, _, err := p.Plan(t.Context(), exp, nil, ts, "fixture", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(requests) != 2 {
		t.Fatalf("calls=%d", len(requests))
	}
	for i, req := range requests {
		input := ""
		for _, message := range req.Messages {
			if strings.Contains(message.Text(), plannerSnapshotPrefix) {
				input = message.Text()
				break
			}
		}
		start := strings.Index(input, plannerSnapshotPrefix)
		if start < 0 {
			t.Fatal("provider did not receive journal", input)
		}
		var view map[string]json.RawMessage
		if err := json.NewDecoder(strings.NewReader(input[start+len(plannerSnapshotPrefix):])).Decode(&view); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(view["hints"]), fmt.Sprintf("round %d fixture", i)) {
			t.Fatal("stale/missing materialized view", view)
		}
	}
	if !strings.Contains(output.String(), "reported_input_tokens=123") || !strings.Contains(output.String(), "token_source=provider_usage") {
		t.Fatal(output.String())
	}
}

type plannerContextFixtureProvider struct{ receive func(llm.CompletionRequest) }

func (p *plannerContextFixtureProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.receive(req)
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture complete")}}, "end_turn", llm.Usage{InputTokens: 123, OutputTokens: 5}, nil
}
func (p *plannerContextFixtureProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.receive(req)
		if yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "fixture complete"}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{InputTokens: 123, OutputTokens: 5}}, nil)
		}
	}
}

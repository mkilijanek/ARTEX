package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestWorkerLaunchProjectionIsDefaultAndIndependentOfExperiments(t *testing.T) {
	t.Setenv("ARTEX_WORKER_CONTEXT", "")
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	if !compactWorkerLaunchEnabled() || boundedContextEnabled() {
		t.Fatal("worker default changed experimental flag")
	}
	for _, value := range []string{"0", "false", "off"} {
		t.Setenv("ARTEX_WORKER_CONTEXT", value)
		if compactWorkerLaunchEnabled() {
			t.Fatal("rollback ignored", value)
		}
	}
}

func TestRenderedWorkerLaunchLargeTaskBudgetAndEvidenceRetrieval(t *testing.T) {
	facts := []any{}
	for i := range 2000 {
		facts = append(facts, map[string]any{"id": 100 + i, "from_intent": 1, "state": "confirmed", "confidence": "observed", "summary": strings.Repeat("negative observation \x00秘密 ", 100)})
	}
	data := map[string]any{"task": map[string]any{"goal": "preserve full operator goal"}, "hints": []any{"preserve the operator hint"}, "running_intents": []any{map[string]any{"id": 7, "parents": []int64{1}, "summary": "assigned fixture"}}, "recent_facts": facts, "facts": len(facts), "related_tasks": []any{map[string]any{"body": strings.Repeat("unrelated data ", 10000)}}}
	before, _ := json.Marshal(data)
	view := workerLaunchContext(data, &db.Node{ID: 7})
	rendered := renderWorkerGraphOverview(view)
	if len(rendered) > 20*1024 {
		t.Fatalf("rendered launch exceeds documented budget: %d", len(rendered))
	}
	selected := view["recent_facts"].([]any)
	if len(selected) != 20 || fmt.Sprint(view["recent_facts_omitted_items"]) != "1980" {
		t.Fatal("observation limit unreported", len(selected), view["recent_facts_omitted_items"])
	}
	first := selected[0].(map[string]any)
	for _, key := range []string{"id", "from_intent", "state", "confidence", "summary_truncated", "summary_omitted_bytes", "requires_node_detail"} {
		if first[key] == nil {
			t.Fatal("lost evidence identity/detail requirement", key)
		}
	}
	if !strings.Contains(rendered, "preserve full operator goal") || !strings.Contains(rendered, "preserve the operator hint") {
		t.Fatal("operator data lost")
	}
	after, _ := json.Marshal(data)
	if string(before) != string(after) {
		t.Fatal("source graph mutated")
	}
	t.Logf("source=%d rendered=%d", len(before), len(rendered))
}

func TestWorkerDefaultProjectionKeepsDependencyAndAwarenessDistinct(t *testing.T) {
	data := map[string]any{"open_intents": []any{map[string]any{"id": 7, "parents": []int64{1}}}, "recent_facts": []any{map[string]any{"id": 101, "from_intent": 1, "summary": "dependency"}, map[string]any{"id": 102, "from_intent": 2, "source_task_id": 9, "confidence": "inferred", "summary": "other context"}}}
	view := workerLaunchContext(data, &db.Node{ID: 7})
	if len(view["recent_facts"].([]any)) != 1 || len(view["awareness_recent_facts"].([]any)) != 1 {
		t.Fatal("dependency/awareness selection lost", view)
	}
	other := view["awareness_recent_facts"].([]any)[0].(map[string]any)
	if fmt.Sprint(other["source_task_id"]) != "9" || other["confidence"] != "inferred" {
		t.Fatal("provenance lost")
	}
}

func TestRealWorkerUsesDefaultProjectionAndExplicitRollback(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("default worker fixture", "keep assigned scope")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	intentID, err := ts.AddIntent(map[string]any{"summary": "only local fixture"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ts.GetNode(intentID)
	if err != nil {
		t.Fatal(err)
	}
	factID, err := ts.AddNode(db.KindFact, map[string]any{"summary": strings.Repeat("long existing observation ", 1000)}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.Link(factID, db.RelDerivedFrom, intentID); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"", "0"} {
		t.Run("flag="+flag, func(t *testing.T) {
			t.Setenv("ARTEX_WORKER_CONTEXT", flag)
			var request llm.CompletionRequest
			provider := &plannerContextFixtureProvider{receive: func(req llm.CompletionRequest) { request = req }}
			w := NewWorker(provider, "fixture", t.TempDir(), nil, 100000, 1)
			w.SetNonStreaming(func() bool { return true })
			if _, _, err := w.Execute(t.Context(), "fixture", 0, nil, ts, intent, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			input := ""
			for _, message := range request.Messages {
				input += message.Text()
			}
			if flag == "" && (!strings.Contains(input, `"summary_truncated":true`) || len(input) > 20*1024) {
				t.Fatal("default worker bypassed projection", len(input))
			}
			if flag == "0" && !strings.Contains(input, strings.Repeat("long existing observation ", 1000)) {
				t.Fatal("legacy rollback did not preserve raw overview")
			}
			if strings.Contains(strings.Join(request.System, "\n"), contextReferenceRules) {
				t.Fatal("worker projection enabled experimental references")
			}
		})
	}
}

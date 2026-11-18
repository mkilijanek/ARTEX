package agent

import (
	"encoding/json"
	"fmt"
	"github.com/mkilijanek/artex/db"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTraceSlicesReconstructEvidenceAndBoundEscaping(t *testing.T) {
	text := strings.Repeat("秘密\x00\n", 4000)
	var reconstructed strings.Builder
	for offset := 0; offset < len(text); {
		s := traceSlice(text, offset, 4096)
		part := s["detail"].(string)
		if !utf8.ValidString(part) || len(part) == 0 {
			t.Fatal("invalid/empty nonterminal slice")
		}
		encoded, _ := json.Marshal(s)
		if len(encoded) > 4500 {
			t.Fatal("encoded slice exceeds budget")
		}
		reconstructed.WriteString(part)
		next := s["next_offset"].(int)
		if next <= offset {
			t.Fatal("no forward progress")
		}
		offset = next
	}
	if reconstructed.String() != text {
		t.Fatal("evidence was lost while paging")
	}
	part, start, _ := sliceUTF8("a秘密", 2, 8)
	if start != 1 || part != "秘密" {
		t.Fatal("unaligned offset lost bytes")
	}
}

func TestProjectionBoundsAndKeepsProvenanceAndDistinctIDs(t *testing.T) {
	facts := []map[string]any{{"id": 1, "summary": "port closed", "confidence": "observed", "source_task_id": 8}, {"id": 1, "summary": "port closed", "confidence": "observed", "source_task_id": 8}, {"id": 2, "summary": "port closed", "confidence": "inferred"}}
	data := map[string]any{"task": map[string]any{"goal": "keep the full goal"}, "recent_facts": facts, "optional_detail": strings.Repeat("\x00", 100000)}
	out := projectOverview(data)
	b, _ := json.Marshal(out)
	if len(b) > overviewBudget || !utf8.Valid(b) {
		t.Fatal("unbounded projection")
	}
	fs := out["recent_facts"].([]any)
	if len(fs) != 2 || fs[0].(map[string]any)["confidence"] != "observed" || fmt.Sprint(fs[0].(map[string]any)["source_task_id"]) != "8" {
		t.Fatalf("lost negative evidence/provenance: %v", fs)
	}
}

func TestWorkerProjectionSelectsDependenciesAndAssetFields(t *testing.T) {
	data := map[string]any{"running_intents": []map[string]any{{"id": 7, "parents": []int64{1}}}, "recent_facts": []map[string]any{{"id": 101, "from_intent": 1, "summary": "negative", "confidence": "observed"}, {"id": 2, "summary": "unrelated"}}, "scope": []any{"lab.invalid"}}
	out := workerContext(data, &db.Node{ID: 7})
	facts := out["recent_facts"].([]any)
	if len(facts) != 1 || fmt.Sprint(facts[0].(map[string]any)["id"]) != "101" {
		t.Fatalf("dependencies: %v", facts)
	}
	assets := projectAssets([]*db.Asset{{ID: 1, URL: strings.Repeat("x", 5000), Domain: "lab.invalid", PageTitle: "secret headers"}})
	b, _ := json.Marshal(assets)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), strings.Repeat("x", 100)) {
		t.Fatal("raw asset metadata leaked into projection")
	}
	if !assets["assets"].([]any)[0].(map[string]any)["requires_asset_detail"].(bool) {
		t.Fatal("incomplete target lacked detail requirement")
	}
}

func TestMandatoryOverviewKeysAreLossless(t *testing.T) {
	hints := make([]any, 50)
	for i := range hints {
		hints[i] = strings.Repeat("constraint", 200)
	}
	data := map[string]any{"hints": hints, "goals": hints, "stagnation_warning": "stop repeating", "coverage": map[string]any{"tested": 0}, "related_tasks": []any{"source"}, "sites_without_endpoints": 8, "asset_counts": map[string]any{"site": 9}}
	out := projectOverview(data)
	for _, key := range []string{"hints", "goals", "stagnation_warning", "coverage"} {
		before, _ := json.Marshal(data[key])
		after, _ := json.Marshal(out[key])
		if string(before) != string(after) {
			t.Fatalf("mandatory field changed: %s", key)
		}
	}
	if out["mandatory_budget_exceeded"] != true {
		t.Fatal("mandatory overflow must be explicit")
	}
	small := projectOverview(map[string]any{"coverage": data["coverage"], "sites_without_endpoints": 8, "asset_counts": data["asset_counts"]})
	if small["sites_without_endpoints"] == nil || small["asset_counts"] == nil {
		t.Fatal("overview signals dropped")
	}
}
func TestChangedEvidenceAndSourceIdentityAreNotDeduplicated(t *testing.T) {
	data := map[string]any{"recent_facts": []any{
		map[string]any{"id": 1, "source_task_id": 1, "state": "old"},
		map[string]any{"id": 1, "source_task_id": 1, "state": "new"},
		map[string]any{"id": 1, "source_task_id": 2, "state": "new"},
	}}
	if len(projectOverview(data)["recent_facts"].([]any)) != 3 {
		t.Fatal("state transition or provenance lost")
	}
}

func TestTraceSchemaFlagOffAndAggregateBound(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	schema := traceInputSchema(obj(map[string]any{"intent_id": idp("id"), "offset": intp("offset")}, "intent_id"))
	if schema["properties"].(map[string]any)["offset"] != nil || tracePagingDescription() != "" {
		t.Fatal("flag-off schema changed")
	}
	steps := []map[string]any{}
	for range 5 {
		step := traceSlice(strings.Repeat("a", 9000), 0, 4096)
		steps = append(steps, step)
	}
	result := map[string]any{"steps": steps}
	boundTraceResult(result, steps)
	raw, _ := json.Marshal(result)
	if len(raw) > 16*1024 {
		t.Fatalf("aggregate bytes=%d", len(raw))
	}
	for _, step := range steps {
		if step["next_offset"].(int) != len(step["detail"].(string)) || step["has_more"] != true {
			t.Fatal("lost continuation")
		}
	}
}

func TestProjectionOfRealGraphOverview(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("requires isolated PostgreSQL")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("fixture", "mandatory goal")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	factID, err := ts.AddNode(db.KindFact, map[string]any{"summary": "observed negative fixture", "confidence": "observed"}, 0, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	intentID, err := ts.AddIntent(map[string]any{"summary": "dependent intent"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.AddNode(db.KindHint, map[string]any{"text": "mandatory fixture hint"}, 0, "active", "fixture", nil); err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(ts, "fixture")
	data := tools.graphOverviewData()
	if len(data["recent_facts"].([]map[string]any)) == 0 {
		t.Fatal("fixture overview lacks facts")
	}
	worker := workerContext(data, &db.Node{ID: intentID})
	if worker["intent_id"] == nil || worker["intent_context"] == nil {
		t.Fatal("real intent lost")
	}
	direct := workerContext(map[string]any{"running_intents": []map[string]any{{"id": intentID, "parents": []int64{factID}}}, "recent_facts": data["recent_facts"]}, &db.Node{ID: intentID})
	if len(direct["recent_facts"].([]any)) == 0 {
		t.Fatal("direct fact parent lost")
	}
	out := projectOverview(data)
	for key := range data {
		if _, ok := out[key]; !ok {
			if _, marked := out["omitted"].(map[string]any)[key]; !marked {
				t.Fatalf("real overview field silently dropped: %s", key)
			}
		}
	}
}

func TestWorkerPrioritySurvivesRelatedTaskPressure(t *testing.T) {
	data := map[string]any{"hints": []any{"keep instructions"}, "related_tasks": []any{map[string]any{"body": strings.Repeat("x", 40000)}}, "running_intents": []map[string]any{{"id": 7, "parents": []int64{1}}}, "recent_facts": []map[string]any{{"id": 101, "from_intent": 1, "summary": "negative"}}}
	out := workerContext(data, &db.Node{ID: 7})
	for _, k := range []string{"intent_context", "intent_id", "scope_required_before_action", "recent_facts", "recent_facts_filtered_unrelated", "finding_list_filtered_unrelated"} {
		if out[k] == nil {
			t.Fatalf("worker mandatory field dropped: %s", k)
		}
	}
	if len(out["recent_facts"].([]any)) != 1 {
		t.Fatal("dependency evidence dropped")
	}
	if out["mandatory_budget_exceeded"] == true {
		t.Fatal("optional related-task content mislabeled mandatory")
	}
}

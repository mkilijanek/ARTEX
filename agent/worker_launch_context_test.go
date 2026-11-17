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

func TestWorkerAssetProjectionEncodedBudgetAndExactTargets(t *testing.T) {
	assets := []*db.Asset{}
	for i := 0; i < 100; i++ {
		assets = append(assets, &db.Asset{ID: int64(i + 1), Type: "http_service", URL: "https://fixture.invalid/" + strings.Repeat("\x00", 150), Domain: "fixture.invalid", PageTitle: strings.Repeat("unused title", 10000), Extra: map[string]any{"body": strings.Repeat("unused body", 10000)}})
	}
	before, _ := json.Marshal(assets)
	view := projectAssets(assets)
	encoded, _ := json.Marshal(view)
	selected := view["assets"].([]any)
	if len(encoded) > workerAssetBudget || len(selected) > 8 || len(selected) == 0 || view["omitted_assets"] != len(assets)-len(selected) {
		t.Fatal("asset budget/omission mismatch", len(encoded), len(selected), view["omitted_assets"])
	}
	if selected[0].(map[string]any)["url"] != assets[0].URL {
		t.Fatal("target URL altered")
	}
	if strings.Contains(string(encoded), "unused") {
		t.Fatal("raw metadata leaked")
	}
	after, _ := json.Marshal(assets)
	if string(before) != string(after) {
		t.Fatal("source asset changed")
	}
	oversized := projectAssets([]*db.Asset{{ID: 777, Type: "http_service", URL: "https://fixture.invalid/" + strings.Repeat("x", 2000)}})["assets"].([]any)[0].(map[string]any)
	if oversized["url"] != nil || oversized["url_omitted"] != true || oversized["requires_asset_detail"] != true {
		t.Fatal("oversized target must be retrieved, never shortened", oversized)
	}
	t.Logf("raw assets=%d compact assets=%d selected=%d", len(before), len(encoded), len(selected))
}

func TestRealWorkerAssetProjectionDefaultAndRollback(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("asset projection fixture", "assigned assets only")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	as := d.Assets()
	target := fmt.Sprintf("https://asset-fixture-%d.invalid/exact?value=1", exp)
	title := strings.Repeat("fixture title marker ", 2000)
	id, err := as.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: target, PageTitle: title})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM assets WHERE id=$1`, id)
	ts := d.Exploration(exp)
	iid, err := ts.AddIntent(map[string]any{"summary": "inspect assigned fixture only", "asset_ids": []int64{id}}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ts.GetNode(iid)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"", "0"} {
		t.Run("flag="+flag, func(t *testing.T) {
			t.Setenv("ARTEX_WORKER_CONTEXT", flag)
			var request llm.CompletionRequest
			provider := &plannerContextFixtureProvider{receive: func(req llm.CompletionRequest) { request = req }}
			w := NewWorker(provider, "fixture", t.TempDir(), nil, 100000, 1)
			w.SetNonStreaming(func() bool { return true })
			if _, _, err := w.Execute(t.Context(), "fixture", 0, as, ts, intent, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			system := strings.Join(request.System, "\n")
			if !strings.Contains(system, target) || !strings.Contains(system, string(intent.Payload)) {
				t.Fatal("target/intent asset identity lost")
			}
			if flag == "" && (strings.Contains(system, title) || !strings.Contains(system, `"omitted_fields"`)) {
				t.Fatal("default system contains raw assets")
			}
			if flag == "0" && !strings.Contains(system, title) {
				t.Fatal("rollback did not preserve raw assets")
			}
			stored, err := as.GetByIDs([]int64{id})
			if err != nil || len(stored) != 1 || stored[0].PageTitle != title {
				t.Fatal("full detail unavailable", err)
			}
			t.Logf("flag=%q actual system bytes=%d", flag, len(system))
		})
	}
}

package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestWorkerLaunchProjectionIsOptInAndIndependentOfExperiments(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	for _, value := range []string{"", "0", "false", "off", "no", "unknown"} {
		t.Setenv("ARTEX_WORKER_CONTEXT", value)
		if compactWorkerLaunchEnabled() {
			t.Fatalf("compact launch enabled without opt-in: %q", value)
		}
	}
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	for _, value := range []string{"1", "true", "on", " YES "} {
		t.Setenv("ARTEX_WORKER_CONTEXT", value)
		if !compactWorkerLaunchEnabled() || boundedContextEnabled() {
			t.Fatalf("opt-in ignored or coupled to experimental flag: %q", value)
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
	rendered := renderWorkerGraphOverview(view, true)
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

func TestRealWorkerUsesLegacyLaunchByDefaultAndProjectionOnOptIn(t *testing.T) {
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
	tools := NewToolSet(ts, "fixture")
	detail, err := tools.nodeDetail().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"id":%d}`, factID)), nil)
	if err != nil || detail.IsError || !strings.Contains(detail.Flatten(), strings.Repeat("long existing observation ", 1000)) {
		t.Fatal("full evidence not available through node_detail", err)
	}
	for _, flag := range []string{"1", "", "0"} {
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
			if flag == "1" && (!strings.Contains(input, `"summary_truncated":true`) || !strings.Contains(input, "This is a partial view") || len(input) > 20*1024) {
				t.Fatal("opted-in worker bypassed projection", len(input))
			}
			if flag != "1" && (!strings.Contains(input, strings.Repeat("long existing observation ", 1000)) || strings.Contains(input, "partial view") || !strings.Contains(input, "[Global exploration overview") || strings.Contains(input, `"projection":true`)) {
				t.Fatal("legacy launch did not preserve raw overview and its introduction")
			}
			system := strings.Join(request.System, "\n")
			if flag == "1" && strings.Count(system+input, "only local fixture") != 1 {
				t.Fatal("assigned summary duplicated or lost")
			}
			if strings.Count(system, untrustedContentInstructions) != 1 || strings.Count(system, normaTrustRules) != 1 || strings.Count(system, artifactRulesPrefix) != 1 {
				t.Fatal("static safety/artifact rules duplicated or lost")
			}
			if len(system) > 14000 {
				t.Fatalf("actual worker system exceeds reviewed fixture budget: %d", len(system))
			}
			if strings.Contains(system, contextReferenceRules) {
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

func TestRealWorkerAssetProjectionOptInAndLegacyDefault(t *testing.T) {
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
	tools := NewToolSet(ts, "fixture")
	tools.SetAssetStore(as, as.Companies())
	detail, err := tools.listAssets().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"ids":[%d]}`, id)), nil)
	if err != nil || detail.IsError || !strings.Contains(detail.Flatten(), title) {
		t.Fatal("full asset not available through list_assets", err)
	}
	for _, flag := range []string{"1", "", "0"} {
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
			if flag == "1" && (strings.Contains(system, title) || !strings.Contains(system, `"omitted_fields"`)) {
				t.Fatal("opted-in system contains raw assets")
			}
			if flag != "1" && !strings.Contains(system, title) {
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

func TestWorkerLaunchKeepsAssignmentInSystemAndLineageInContext(t *testing.T) {
	summary := strings.Repeat("unique assigned summary ", 1000)
	payload, _ := json.Marshal(map[string]any{"summary": summary, "asset_ids": []int64{91}})
	intent := &db.Node{ID: 7, Payload: payload}
	source := map[string]any{"running_intents": []any{map[string]any{"id": 7, "summary": summary, "parents": []int64{1}, "yields": []int64{9}, "asset_ids": []int64{91}}}}
	view := workerLaunchContext(source, intent)
	assignment := view["intent_context"].(map[string]any)
	if assignment["summary"] != nil || assignment["summary_in_system_intent"] != true || assignment["parents"] == nil || assignment["yields"] == nil {
		t.Fatal("assignment/lineage contract lost", assignment)
	}
	rendered := renderIntentTask(intent) + renderWorkerGraphOverview(view, true)
	if strings.Count(rendered, summary) != 1 {
		t.Fatal("full assigned summary must appear exactly once")
	}
	if !strings.Contains(rendered, `"asset_ids":[91]`) || !strings.Contains(rendered, "only job") {
		t.Fatal("intent mandate/targets lost")
	}
	if len(renderWorkerGraphOverview(map[string]any{}, true)) > 350 {
		t.Fatal("overview boilerplate grew")
	}
	legacy := renderWorkerGraphOverview(map[string]any{}, false)
	if strings.Contains(legacy, "partial view") || !strings.Contains(legacy, "Global exploration overview") || !strings.Contains(legacy, "be sure to write it into a fact for the planner") {
		t.Fatal("legacy launch must keep its own introduction", legacy)
	}
}

func TestWorkerLaunchKeepsEveryWindowFindingVisible(t *testing.T) {
	findings, facts := []any{}, []any{}
	for i := range 6 {
		findings = append(findings, map[string]any{"id": 200 + i, "from_intent": 2, "summary": fmt.Sprintf("finding %d", i)})
		facts = append(facts, map[string]any{"id": 300 + i, "from_intent": 2, "summary": fmt.Sprintf("fact %d", i)})
	}
	data := map[string]any{"running_intents": []any{map[string]any{"id": 7}}, "finding_list": findings, "recent_facts": facts}
	view := workerLaunchContext(data, &db.Node{ID: 7})
	if got := len(view["awareness_finding_list"].([]any)); got != 6 {
		t.Fatal("a worker must see every finding in the window to avoid re-reporting", got)
	}
	if got := len(view["awareness_recent_facts"].([]any)); got != 3 || fmt.Sprint(view["awareness_recent_facts_total"]) != "6" {
		t.Fatal("fact awareness sample changed", got, view["awareness_recent_facts_total"])
	}
}

func TestWorkerLaunchLoadsDependencyEvidenceOutsideOverviewWindow(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("dependency evidence fixture", "busy task")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	must := func(id int64, err error) int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	link := func(from int64, rel string, to int64) {
		t.Helper()
		if err := ts.Link(from, rel, to); err != nil {
			t.Fatal(err)
		}
	}
	parent := must(ts.AddIntent(map[string]any{"summary": "parent direction"}, 1, nil, "fixture"))
	yielded := must(ts.AddNode(db.KindFact, map[string]any{"summary": "old fact yielded by parent", "confidence": "observed"}, 0, "confirmed", "fixture", nil))
	link(parent, db.RelYields, yielded)
	finding := must(ts.AddNode(db.KindFinding, map[string]any{"summary": "old finding yielded by parent"}, 0, "confirmed", "fixture", nil))
	link(parent, db.RelYields, finding)
	direct := must(ts.AddNode(db.KindFact, map[string]any{"summary": "old direct parent fact"}, 0, "confirmed", "fixture", nil))
	for i := range 25 {
		must(ts.AddNode(db.KindFact, map[string]any{"summary": fmt.Sprintf("newer unrelated fact %d", i)}, 0, "confirmed", "fixture", nil))
	}
	for i := range 12 {
		must(ts.AddNode(db.KindFinding, map[string]any{"summary": fmt.Sprintf("newer unrelated finding %d", i)}, 0, "confirmed", "fixture", nil))
	}
	child := must(ts.AddIntent(map[string]any{"summary": "dependent direction"}, 1, nil, "fixture"))
	link(parent, db.RelDerivedFrom, child)
	link(direct, db.RelDerivedFrom, child)

	tools := NewToolSet(ts, "fixture")
	ids := func(view map[string]any, key string) map[string]map[string]any {
		out := map[string]map[string]any{}
		raw, _ := json.Marshal(view[key])
		var items []map[string]any
		_ = json.Unmarshal(raw, &items)
		for _, m := range items {
			out[fmt.Sprint(m["id"])] = m
		}
		return out
	}
	window := tools.graphOverviewData()
	without := workerLaunchContext(window, &db.Node{ID: child})
	if len(ids(without, "recent_facts")) != 0 || len(ids(without, "finding_list")) != 0 {
		t.Fatal("fixture must push dependency evidence out of the overview windows", without["recent_facts"], without["finding_list"])
	}
	data := tools.graphOverviewData()
	tools.addWorkerDependencyEvidence(data, child)
	view := workerLaunchContext(data, &db.Node{ID: child})
	facts, findings := ids(view, "recent_facts"), ids(view, "finding_list")
	if len(facts) != 2 || len(findings) != 1 {
		t.Fatal("dependency evidence not selected by id", view["recent_facts"], view["finding_list"])
	}
	got := facts[fmt.Sprint(yielded)]
	if got == nil || fmt.Sprint(got["from_intent"]) != fmt.Sprint(parent) || got["confidence"] != "observed" || got["summary"] != "old fact yielded by parent" {
		t.Fatal("yielded fact lost provenance", got)
	}
	if facts[fmt.Sprint(direct)] == nil || fmt.Sprint(findings[fmt.Sprint(finding)]["from_intent"]) != fmt.Sprint(parent) {
		t.Fatal("direct parent fact or parent finding missing", facts, findings)
	}
	// Loading is idempotent and never duplicates entries already in the window.
	tools.addWorkerDependencyEvidence(data, child)
	if len(data["recent_facts"].([]map[string]any)) != 22 {
		t.Fatal("dependency evidence duplicated", len(data["recent_facts"].([]map[string]any)))
	}
}

func TestRealWorkerFlagCombinationsKeepProjectionUnderReferences(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("flag combination fixture", "keep assigned scope")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	intentID, err := ts.AddIntent(map[string]any{"summary": "combination fixture"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ts.GetNode(intentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		bounded, worker string
		compactIntro    bool
	}{{"1", "", false}, {"1", "1", true}} {
		t.Run("bounded="+c.bounded+",worker="+c.worker, func(t *testing.T) {
			t.Setenv("ARTEX_BOUNDED_CONTEXT", c.bounded)
			t.Setenv("ARTEX_WORKER_CONTEXT", c.worker)
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
			if !strings.Contains(input, `"projection":true`) {
				t.Fatal("experimental references must never run over the unprojected overview")
			}
			if strings.Contains(input, "This is a partial view") != c.compactIntro || strings.Contains(input, "[Global exploration overview") == c.compactIntro {
				t.Fatal("introduction does not match the selected launch mode")
			}
		})
	}
}

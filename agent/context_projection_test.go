package agent

import (
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/artex/db"
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
	data := map[string]any{"task": map[string]any{"goal": strings.Repeat("秘密", 40000)}, "recent_facts": facts, "hints": []any{strings.Repeat("\x00", 100000)}}
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
	data := map[string]any{"running_intents": []map[string]any{{"id": 7, "parents": []int64{1}}}, "recent_facts": []map[string]any{{"id": 1, "summary": "negative", "confidence": "observed"}, {"id": 2, "summary": "unrelated"}}, "scope": []any{"lab.invalid"}}
	out := workerContext(data, &db.Node{ID: 7})
	facts := out["recent_facts"].([]any)
	if len(facts) != 1 || fmt.Sprint(facts[0].(map[string]any)["id"]) != "1" {
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

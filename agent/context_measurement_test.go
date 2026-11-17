package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// Deterministic payload measurements, without a model/network/cost assumption.
// Provider-reported tokens are separately exercised by TestPlannerFreshSession.
func TestPhaseBContextMeasurements(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	t.Log("scenario,round,snapshot_bytes,journal_bytes,incremental_bytes,request_before_bytes,request_after_bytes,request_before_tokens_estimate,request_after_tokens_estimate")
	for _, scenario := range []string{"steady", "incremental", "changed-evidence"} {
		var checkpoint json.RawMessage
		for round := range 4 {
			facts := []any{}
			for i := range 12 {
				state := "observed"
				if scenario == "changed-evidence" && round > 1 && i == 0 {
					state = "revoked"
				}
				summary := strings.Repeat(fmt.Sprintf("fixture-%d observation ", i), 18)
				facts = append(facts, map[string]any{"id": i + 1, "summary": summary, "state": state})
			}
			count := 12
			if scenario == "incremental" {
				count += round
			}
			data := map[string]any{"task": map[string]any{"goal": "preserve source evidence"}, "recent_facts": facts, "facts": count}
			j, err := advancePlannerJournal(checkpoint, data)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, _ = json.Marshal(j)
			snapshot, _ := json.Marshal(projectOverview(data))
			incremental := len(checkpoint)
			if len(j.Deltas) > 0 {
				raw, _ := json.Marshal(j.Deltas[len(j.Deltas)-1])
				incremental = len(raw)
			}
			literal := renderPlannerJournal(j)
			current, _ := j.materialize()
			ctx := withContextFragment(t.Context(), literal, current, 1)
			p := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
			req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText(literal)}}
			toolJSON, _ := json.Marshal(map[string]any{"facts": facts, "has_more": false, "total": 12})
			for n := range 2 {
				id := fmt.Sprintf("read-%d", n)
				req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "list_facts", Input: json.RawMessage(`{}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: id, Content: []llm.ContentBlock{llm.TextBlock(string(toolJSON))}}}})
			}
			before, _ := json.Marshal(req)
			after, _ := json.Marshal(p.request(ctx, req))
			if len(after) > len(before) {
				t.Fatal("dedup expanded request")
			}
			t.Logf("%s,%d,%d,%d,%d,%d,%d,%d,%d", scenario, round+1, len(snapshot), len(checkpoint), incremental, len(before), len(after), (len(before)+3)/4, (len(after)+3)/4)
		}
	}
}

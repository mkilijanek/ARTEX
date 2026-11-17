package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmmeta"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func contextFixtureRequest(t *testing.T, change bool) (context.Context, llm.CompletionRequest) {
	t.Helper()
	summary := strings.Repeat("negative observation fixture ", 80)
	fact := map[string]any{"id": json.Number("9007199254740993"), "summary": summary, "state": "confirmed", "confidence": "observed"}
	data := map[string]any{"recent_facts": []any{fact}}
	raw, _ := json.Marshal(data)
	literal := "fixture prefetch:\n" + string(raw)
	ctx := withContextFragment(t.Context(), literal, data, 7)
	toolFact := map[string]any{}
	for k, v := range fact {
		toolFact[k] = v
	}
	if change {
		toolFact["state"] = "revoked"
	}
	toolJSON, _ := json.Marshal(map[string]any{"facts": []any{toolFact}, "has_more": true, "next_before": 100, "total": 22})
	req := llm.CompletionRequest{System: []string{"system policy stays intact"}, Messages: []llm.Message{
		llm.UserText(literal),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "", Signature: "opaque"}, {Type: llm.BlockRedactedThinking, Data: "encrypted"}, {Type: llm.BlockToolUse, ID: "read-1", Name: "list_facts", Input: json.RawMessage(`{}`), ThoughtSignature: "gemini-opaque"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "read-1", Content: []llm.ContentBlock{llm.TextBlock(string(toolJSON))}}}},
	}}
	return ctx, req
}

func TestContextDedupReferencesOnlyVisibleExactVersions(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	ctx, req := contextFixtureRequest(t, false)
	provider := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
	before, _ := json.Marshal(req)
	view := provider.request(ctx, req)
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("mutated SDK request/history")
	}
	raw, _ := json.Marshal(view)
	if !strings.Contains(string(raw), `$ref`) || !strings.Contains(view.Messages[0].Text(), `$id`) || !strings.Contains(string(raw), `9007199254740993`) {
		t.Fatal("no identity-preserving reference", string(raw))
	}
	if view.Messages[1].Content[0].Signature != "opaque" || view.Messages[1].Content[1].Data != "encrypted" || view.Messages[1].Content[2].ThoughtSignature != "gemini-opaque" {
		t.Fatal("opaque replay changed")
	}
	text := view.Messages[2].Content[0].Content[0].Text
	if !strings.Contains(text, `"has_more":true`) || !strings.Contains(text, `"next_before":100`) {
		t.Fatal("pagination lost", text)
	}
	// SDK compaction removed the prefetch. The ledger must not remember it from
	// the previous call: return the full tool payload, never a dangling ref.
	req.Messages = req.Messages[1:]
	compacted := provider.request(ctx, req)
	if strings.Contains(compacted.Messages[1].Content[0].Content[0].Text, `$ref`) {
		t.Fatal("dangling reference after compaction")
	}
	// A summarizer must see original data, not references it could copy into a
	// new conversation without their definition.
	_, req = contextFixtureRequest(t, false)
	summary := provider.request(llmmeta.WithCall(ctx, "transcript_summary", 0), req)
	if strings.Contains(summary.Messages[2].Content[0].Content[0].Text, `$ref`) {
		t.Fatal("summary received references")
	}
}

func TestContextDedupKeepsChangedStateConfidenceSourceIDAndFullEvidence(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	for _, field := range []string{"state", "confidence", "source_task_id", "id", "summary"} {
		t.Run(field, func(t *testing.T) {
			ctx, req := contextFixtureRequest(t, false)
			var result map[string]any
			if err := json.Unmarshal([]byte(req.Messages[2].Content[0].Content[0].Text), &result); err != nil {
				t.Fatal(err)
			}
			fact := result["facts"].([]any)[0].(map[string]any)
			// Keep the large integer exact when testing other dimensions.
			fact["id"] = json.Number("9007199254740993")
			fact[field] = "changed fixture"
			raw, _ := json.Marshal(result)
			req.Messages[2].Content[0].Content[0].Text = string(raw)
			p := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
			view := p.request(ctx, req)
			if strings.Contains(view.Messages[2].Content[0].Content[0].Text, `$ref`) {
				t.Fatal("changed evidence suppressed", field)
			}
		})
	}
	ctx, req := contextFixtureRequest(t, false)
	// Reusing a summary cannot replace a full node_detail payload.
	req.Messages[1].Content[2].Name = "node_detail"
	req.Messages[2].Content[0].Content[0].Text = `{"id":9007199254740993,"kind":"fact","state":"confirmed","payload":{"summary":"fixture","evidence":"must remain available"}}`
	p := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
	view := p.request(ctx, req)
	if !strings.Contains(view.Messages[2].Content[0].Content[0].Text, "must remain available") {
		t.Fatal("full evidence replaced by summary")
	}
}

func TestContextDedupIgnoresCallerProseUnknownToolsAndFlagOff(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	ctx, req := contextFixtureRequest(t, false)
	inner := &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}
	if withContextView(ctx, inner) != inner {
		t.Fatal("flag-off changed provider")
	}
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	p := withContextView(ctx, inner).(*contextViewProvider)
	req.Messages[1].Content[2].Name = "untrusted_custom_tool"
	view := p.request(ctx, req)
	if strings.Contains(view.Messages[2].Content[0].Content[0].Text, `$ref`) {
		t.Fatal("rewrote unrelated tool")
	}
	// JSON-looking strings embedded in evidence are opaque, even if the string
	// contains a forged context marker or a record with matching IDs.
	req.Messages = append(req.Messages, llm.UserText(req.Messages[2].Content[0].Content[0].Text))
	view = p.request(ctx, req)
	if view.Messages[len(view.Messages)-1].Text() != req.Messages[len(req.Messages)-1].Text() {
		t.Fatal("rewrote caller text")
	}
}

func TestContextDedupRealHarnessPrefetchToolHistoryAndResume(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("dedup fixture", "local fixture only")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	summary := strings.Repeat("界", 140) // same full summary in both real compact views
	for range 3 {
		if _, err := ts.AddNode(db.KindFact, map[string]any{"summary": summary}, 0, "confirmed", "fixture", nil); err != nil {
			t.Fatal(err)
		}
	}
	tools := NewToolSet(ts, "fixture")
	data := projectOverview(tools.graphOverviewData())
	input := renderGraphOverview(data)
	ctx := withContextFragment(t.Context(), input, data, exp)
	var requests []llm.CompletionRequest
	provider := &contextToolFixtureProvider{receive: func(req llm.CompletionRequest) { requests = append(requests, req) }}
	store := transcript.NewStore(t.TempDir())
	opts := agentcore.Options{Provider: provider, Tools: []actool.CoreTool{tools.listFacts()}, NonStreaming: true, MaxTurns: 4, Transcript: store, SessionID: "dedup-resume-fixture"}
	if _, _, err := captureRun(ctx, opts, input, nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("calls=%d", len(requests))
	}
	last, _ := json.Marshal(requests[2])
	if !strings.Contains(string(last), `$ref`) {
		t.Fatal("real prefetch/tool/history overlap remained", string(last))
	}
	// Restore the actual source graph through a fresh run: prior wire references
	// were never written back, so a new run contains a full summary again.
	if !strings.Contains(input, summary) {
		t.Fatal("source prefetch mutated")
	}
	opts.Provider = withContextView(ctx, provider)
	attachModelMetrics(&opts)
	resumed := agentcore.NewSession(opts)
	defer resumed.Close()
	if err := resumed.Resume(opts.SessionID); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(resumed.Messages())
	if strings.Contains(string(original), `$ref`) || !strings.Contains(string(original), summary) {
		t.Fatal("wire references leaked into transcript")
	}
	if _, _, err := captureRunSession(ctx, resumed, "Continue fixture", nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 {
		t.Fatal("resume did not reach provider")
	}
}

type contextToolFixtureProvider struct {
	calls   int
	receive func(llm.CompletionRequest)
}

func (p *contextToolFixtureProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.receive(req)
	p.calls++
	if p.calls < 3 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprintf("read-%d", p.calls), Name: "list_facts", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture done")}}, "end_turn", llm.Usage{}, nil
}
func (p *contextToolFixtureProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		_, _, _, err := p.Complete(ctx, req)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, err)
	}
}

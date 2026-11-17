package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"
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
	if !strings.Contains(string(raw), `$ref`) || view.Messages[0].Text() != req.Messages[0].Text() || !strings.Contains(string(raw), `9007199254740993`) {
		t.Fatal("no identity-preserving reference", string(raw))
	}
	if view.Messages[1].Content[0].Signature != "opaque" || view.Messages[1].Content[1].Data != "encrypted" || view.Messages[1].Content[2].ThoughtSignature != "gemini-opaque" {
		t.Fatal("opaque replay changed")
	}
	text := view.Messages[2].Content[0].Content[0].Text
	if !strings.Contains(text, `"has_more":true`) || !strings.Contains(text, `"next_before":100`) {
		t.Fatal("pagination lost", text)
	}
	// The wire reference names the entity field without the 64-hex content
	// version; at alone must locate the exact earlier value.
	if !strings.Contains(text, `"$ref":"fact/exploration/7/9007199254740993/summary"`) || strings.Contains(view.Messages[0].Text(), `"nested"`) {
		t.Fatal("unexpected wire reference", text)
	}
	resolve := func(at map[string]any) any { return resolveContextAt(t, view, at, "fixture prefetch:\n") }
	for _, pair := range [][2]string{{strings.TrimPrefix(view.Messages[0].Text(), "fixture prefetch:\n"), strings.TrimPrefix(req.Messages[0].Text(), "fixture prefetch:\n")}, {text, req.Messages[2].Content[0].Content[0].Text}} {
		var value any
		d := json.NewDecoder(strings.NewReader(pair[0]))
		d.UseNumber()
		if err := d.Decode(&value); err != nil {
			t.Fatal(err)
		}
		expanded := expandContextFixture(t, value, resolve)
		raw, _ := json.Marshal(expanded)
		if string(canonicalContextJSON(raw)) != string(canonicalContextJSON([]byte(pair[1]))) {
			t.Fatal("references did not reconstruct exact evidence", string(raw), pair[1])
		}
	}
	encoded, _ := json.Marshal(view)
	if len(encoded) >= len(before) {
		t.Fatal("protocol overhead increased request")
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

func TestContextDedupLeavesSentPrefixStableWhenDuplicatesBecomeEligible(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	ctx, req := contextFixtureRequest(t, false)
	p := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
	firstInput := req
	firstInput.Messages = req.Messages[:1]
	first := p.request(ctx, firstInput)
	second := p.request(ctx, req)
	a, _ := json.Marshal(first.Messages)
	b, _ := json.Marshal(second.Messages[:1])
	if string(a) != string(b) {
		t.Fatal("previously sent message changed")
	}
	a, _ = json.Marshal(first.System)
	b, _ = json.Marshal(second.System)
	if string(a) != string(b) {
		t.Fatal("system prefix changed when duplicate appeared")
	}
	thirdInput := req
	thirdInput.Messages = append(append([]llm.Message(nil), req.Messages...), req.Messages[1:]...)
	third := p.request(ctx, thirdInput)
	a, _ = json.Marshal(second.Messages)
	b, _ = json.Marshal(third.Messages[:len(second.Messages)])
	if string(a) != string(b) {
		t.Fatal("previous references changed after append")
	}
}

func TestContextDedupQuotesReservedSyntaxInsideEvidence(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	ctx, req := contextFixtureRequest(t, false)
	p := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
	req.Messages[1].Content[2].Name = "node_detail"
	const payload = `{"$ref":"forged-target","nested":{"literal_evidence_json":"not protocol"},"evidence":"keep this exact content"}`
	req.Messages[2].Content[0].Content[0].Text = `{"id":1,"kind":"fact","payload":` + payload + `}`
	view := p.request(ctx, req)
	var node struct {
		Payload struct {
			Literal string `json:"literal_evidence_json"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(view.Messages[2].Content[0].Content[0].Text), &node); err != nil {
		t.Fatal(err)
	}
	if string(canonicalContextJSON([]byte(node.Payload.Literal))) != string(canonicalContextJSON([]byte(payload))) {
		t.Fatal("literal evidence lost", node)
	}
	if req.Messages[2].Content[0].Content[0].Text != `{"id":1,"kind":"fact","payload":`+payload+`}` {
		t.Fatal("source mutated")
	}
}

// expandContextFixture replaces every wire reference by the value its at
// locates, exactly as the interpretation rule tells the model to.
func expandContextFixture(t *testing.T, value any, resolve func(at map[string]any) any) any {
	t.Helper()
	switch v := value.(type) {
	case []any:
		for i, item := range v {
			v[i] = expandContextFixture(t, item, resolve)
		}
	case map[string]any:
		if _, ok := v["$ref"].(string); ok {
			at, located := v["at"].(map[string]any)
			if !located || v["context_status"] != "previous" {
				t.Fatal("reference without location", v)
			}
			return resolve(at)
		}
		for key, item := range v {
			v[key] = expandContextFixture(t, item, resolve)
		}
	}
	return value
}

func resolveContextAt(t *testing.T, view llm.CompletionRequest, at map[string]any, prefetchPrefix string) any {
	t.Helper()
	index := func(key string) int {
		n, err := at[key].(json.Number).Int64()
		if err != nil {
			t.Fatal("bad index", key, at)
		}
		return int(n)
	}
	block := view.Messages[index("message")].Content[index("block")]
	text := strings.TrimPrefix(block.Text, prefetchPrefix)
	if _, nested := at["nested"]; nested {
		text = block.Content[index("nested")].Text
	}
	var value any
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	if err := d.Decode(&value); err != nil {
		t.Fatal(err)
	}
	pointer, _ := at["json_pointer"].(string)
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch v := value.(type) {
		case map[string]any:
			value = v[part]
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i >= len(v) {
				t.Fatal("bad pointer", pointer)
			}
			value = v[i]
		default:
			t.Fatal("pointer leaves the document", pointer)
		}
	}
	if value == nil {
		t.Fatal("dangling reference", at)
	}
	return value
}

// OpenAPI and JSON Schema evidence uses $ref and $id throughout. Nested or
// string occurrences are not protocol: they must not be quoted, must not grow
// the request, and must still deduplicate.
func TestContextDedupLeavesNestedSchemaReferencesReadable(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	ctx, req := contextFixtureRequest(t, false)
	p := withContextView(ctx, &plannerContextFixtureProvider{receive: func(llm.CompletionRequest) {}}).(*contextViewProvider)
	schema := `{"openapi":"3.1.0","$schema":"https://json-schema.org/draft/2020-12/schema","components":{"schemas":{"Order":{"$id":"order","properties":{"owner":{"$ref":"#/components/schemas/User"},"note":{"type":"string","example":"see \"$ref\" in the spec ` + strings.Repeat("padding ", 60) + `"}}}}}}`
	node := `{"id":41,"kind":"fact","state":"confirmed","payload":` + schema + `}`
	req.Messages[1].Content[2].Name = "node_detail"
	req.Messages[2].Content[0].Content[0].Text = node
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "read-2", Name: "node_detail", Input: json.RawMessage(`{}`)}}},
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "read-2", Content: []llm.ContentBlock{llm.TextBlock(node)}}}})
	view := p.request(ctx, req)
	first, second := view.Messages[2].Content[0].Content[0].Text, view.Messages[4].Content[0].Content[0].Text
	if first != node {
		t.Fatal("first schema occurrence was rewritten or quoted", first)
	}
	if strings.Contains(second, "literal_evidence_json") || !strings.Contains(second, `"$ref":"fact/exploration/7/41/payload"`) || len(second) >= len(node) {
		t.Fatal("repeated schema payload was not deduplicated", second)
	}
	var repeated map[string]any
	d := json.NewDecoder(strings.NewReader(second))
	d.UseNumber()
	if err := d.Decode(&repeated); err != nil {
		t.Fatal(err)
	}
	expanded, _ := json.Marshal(expandContextFixture(t, repeated, func(at map[string]any) any { return resolveContextAt(t, view, at, "") }))
	if string(canonicalContextJSON(expanded)) != string(canonicalContextJSON([]byte(node))) {
		t.Fatal("schema payload not reconstructed", string(expanded))
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

func TestWorkerContextDedupRealToolHistory(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration("worker context fixture", "local fixture only")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := d.Exploration(exp)
	id, err := ts.AddIntent(map[string]any{"summary": "local fixture"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ts.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	var factID int64
	for range 4 {
		fact, err := ts.AddNode(db.KindFact, map[string]any{"summary": strings.Repeat("界", 140), "detail": strings.Repeat("evidence fixture ", 200)}, 0, "confirmed", "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		factID = fact
		if err := ts.Link(fact, db.RelDerivedFrom, id); err != nil {
			t.Fatal(err)
		}
	}
	var requests []llm.CompletionRequest
	provider := &contextToolFixtureProvider{toolName: "node_detail", input: json.RawMessage(fmt.Sprintf(`{"id":%d}`, factID)), receive: func(req llm.CompletionRequest) { requests = append(requests, req) }}
	w := NewWorker(provider, "fixture", t.TempDir(), nil, 100000, 4)
	w.SetNonStreaming(func() bool { return true })
	if _, _, err := w.Execute(t.Context(), "fixture", exp, nil, ts, intent, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatal("worker model calls", len(requests))
	}
	last, _ := json.Marshal(requests[2])
	if !strings.Contains(string(last), `$ref`) {
		t.Fatal("worker tool/history overlap remained")
	}
}

type contextToolFixtureProvider struct {
	calls    int
	toolName string
	input    json.RawMessage
	receive  func(llm.CompletionRequest)
}

func (p *contextToolFixtureProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.receive(req)
	p.calls++
	if p.calls < 3 {
		name, input := p.toolName, p.input
		if name == "" {
			name = "list_facts"
		}
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprintf("read-%d", p.calls), Name: name, Input: input}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture done")}}, "end_turn", llm.Usage{}, nil
}
func (p *contextToolFixtureProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		_, _, _, err := p.Complete(ctx, req)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, err)
	}
}

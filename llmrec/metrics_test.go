package llmrec

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type telemetryProvider struct {
	events []llm.StreamEvent
	err    error
}

func (p telemetryProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range p.events {
			if !yield(ev, nil) {
				return
			}
		}
		if p.err != nil {
			yield(llm.StreamEvent{}, p.err)
		}
	}
}

func (p telemetryProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "stop", llm.Usage{InputTokens: 100, OutputTokens: 4, CacheReadTokens: 20, CacheWriteTokens: 10}, p.err
}

func TestTelemetryMeasuresRequestWithoutContent(t *testing.T) {
	secret := "SECRET-credentials-秘密"
	req := llm.CompletionRequest{
		System:   []string{secret},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call-secret", secret, false)}}},
		Tools:    []llm.ToolSchema{{Name: "private-tool", Description: secret, InputSchema: map[string]any{"type": "object"}}},
	}
	u := measureRequest(req)
	full, _ := json.Marshal(req)
	results, _ := json.Marshal(req.Messages[0].Content[0])
	if !u.SizeKnown || u.RequestBytes != len(full) || u.SystemBytes != len(secret) || u.ToolResultBytes != len(results) || u.MessageBytes <= u.ToolResultBytes || u.ToolSchemaBytes == 0 {
		t.Fatalf("invalid measurements: %+v", u)
	}
	encoded, _ := json.Marshal(u)
	for _, forbidden := range []string{secret, "call-secret", "private-tool"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("content leaked into telemetry: %s", encoded)
		}
	}
	req.Tools[0].InputSchema["invalid"] = make(chan int)
	if u := measureRequest(req); u.SizeKnown {
		t.Fatal("invalid schema reported as measured")
	}
}

func TestTelemetryOnceAcrossCompletionAndStreamOutcomes(t *testing.T) {
	for _, mode := range []string{"complete", "stream", "early", "cancel", "error", "complete_error"} {
		t.Run(mode, func(t *testing.T) {
			p := telemetryProvider{events: []llm.StreamEvent{
				{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 20, CacheWriteTokens: 10}},
				// Anthropic can echo the input side on the delta.
				{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 100, OutputTokens: 4, CacheReadTokens: 20, CacheWriteTokens: 10}},
			}}
			if strings.Contains(mode, "error") {
				p.err = errors.New("secret provider error")
			}
			var rows []db.LLMUsage
			ctx, cancel := context.WithCancel(transcript.WithSessionID(t.Context(), "exp12-worker-i99"))
			defer cancel()
			ctx = WithAttempt(WithTaskID(WithUsageObserver(ctx, func(u db.LLMUsage) { rows = append(rows, u) }), "42"), 2)
			r := Wrap(p, nil, "fixture", "profile", "", "", func() bool { return false }).ForProvider("anthropic")
			if strings.HasPrefix(mode, "complete") {
				_, _, _, _ = r.Complete(ctx, llm.CompletionRequest{})
			} else {
				for ev, _ := range r.Stream(ctx, llm.CompletionRequest{}) {
					if ev.Type == llm.SEMessageDelta {
						if mode == "cancel" {
							cancel()
							break
						}
						if mode == "early" {
							break
						}
					}
				}
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %d", len(rows))
			}
			u := rows[0]
			wantReason := "completed"
			if mode == "early" {
				wantReason = "consumer_stopped"
			}
			if mode == "cancel" {
				wantReason = "cancelled"
			}
			if strings.Contains(mode, "error") {
				wantReason = "error"
			}
			if u.Reason != wantReason || u.Provider != "anthropic" || u.TaskID != "42" || u.ExplorationID != 12 || u.IntentID != 99 || u.Attempt != 2 || u.Worker != "worker" {
				t.Fatalf("attribution: %+v", u)
			}
			if u.InputTokens != 100 || u.OutputTokens != 4 || u.CacheRead != 20 || u.CacheWrite != 10 {
				t.Fatalf("usage echoed or lost: %+v", u)
			}
		})
	}
}

func TestTelemetryConcurrentRecorderAndUnknownUsage(t *testing.T) {
	r := Wrap(telemetryProvider{}, nil, "", "", "", "", func() bool { return false })
	var mu sync.Mutex
	var rows []db.LLMUsage
	ctx := WithUsageObserver(t.Context(), func(u db.LLMUsage) { mu.Lock(); defer mu.Unlock(); rows = append(rows, u) })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			for range r.Stream(ctx, llm.CompletionRequest{}) {
			}
		})
	}
	wg.Wait()
	if len(rows) != 20 {
		t.Fatalf("zero-usage calls dropped: %d", len(rows))
	}
	for _, u := range rows {
		if u.Reason != "completed" || u.InputTokens != 0 {
			t.Fatalf("unknown usage: %+v", u)
		}
	}
}

func TestIntentSessionParsing(t *testing.T) {
	for session, want := range map[string]int64{"exp12-worker-i99": 99, "exp12-planner": 0, "exp12-worker-invalid": 0, "exp12-btw-i99": 0, "exp12-worker-i-3": 0} {
		if got := intentFromSession(session); got != want {
			t.Fatalf("%q: %d != %d", session, got, want)
		}
	}
}

func TestToolMetricsUseOpaqueKeysAndCountResultBytes(t *testing.T) {
	req := llm.CompletionRequest{
		Tools: []llm.ToolSchema{{Name: "secret-tool", InputSchema: map[string]any{"type": "object"}}},
		Messages: []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "call1", Name: "secret-tool"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("call1", "secret-result", false)}},
		},
	}
	u := measureRequest(req)
	if len(u.ToolBytes) != 1 {
		t.Fatalf("tool groups: %+v", u.ToolBytes)
	}
	for key, b := range u.ToolBytes {
		if len(key) != 64 || b.Schema == 0 || b.Result != u.ToolResultBytes {
			t.Fatalf("tool metric: %q %+v", key, b)
		}
	}
	b, _ := json.Marshal(u)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "call1") {
		t.Fatal("tool metadata leaked")
	}
}

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/llmmeta"
)

func TestModelMetricsDistinguishActualSDKSummary(t *testing.T) {
	var calls []llmmeta.CallInfo
	p := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
		calls = append(calls, llmmeta.From(ctx))
		if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: "summary with evidence reference fixture.txt:7"}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}}
	opts := agentcore.Options{Provider: p, NonStreaming: true}
	attachModelMetrics(&opts)
	for range 2 {
		_, _, _, err := opts.Deps.CallModelSync(t.Context(), llm.CompletionRequest{})
		if err != nil {
			t.Fatal(err)
		}
	}
	c := compaction.New(compaction.Config{KeepRecent: 1}, compaction.ProviderSummarizer(opts.Provider, 1000, true))
	_, ok := c.Reactive(t.Context(), []llm.Message{llm.UserText(strings.Repeat("first ", 2000)), llm.UserText("second"), llm.UserText("tail")})
	if !ok || len(calls) != 3 {
		t.Fatalf("summary=%v calls=%+v", ok, calls)
	}
	for i, info := range calls {
		purpose, round := "agent", i+1
		if i == 2 {
			purpose, round = "transcript_summary", 0
		}
		if info.ID == "" || info.Purpose != purpose || info.Round != round {
			t.Fatalf("call=%+v", info)
		}
		for _, previous := range calls[:i] {
			if previous.ID == info.ID {
				t.Fatal("distinct logical calls share identity")
			}
		}
	}
}

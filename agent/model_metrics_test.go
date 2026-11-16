package agent

import (
	"context"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"testing"
)

func TestModelMetricsDistinguishTurnsAndSummaries(t *testing.T) {
	var rows []db.LLMUsage
	ctx := llmrec.WithUsageObserver(t.Context(), func(u db.LLMUsage) { rows = append(rows, u) })
	// The existing fake is a complete provider; its response is irrelevant here.
	opts := agentcore.Options{Provider: llmrec.Wrap(captureUsageProvider{stream: func(context.Context, func(llm.StreamEvent, error) bool) {}}, nil, "fixture", "profile", "", "", func() bool { return false })}
	attachModelMetrics(&opts)
	for range 2 {
		for range opts.Deps.CallModel(ctx, llm.CompletionRequest{}) {
		}
	}
	_, _, _, _ = opts.Provider.Complete(ctx, llm.CompletionRequest{})
	if len(rows) != 3 {
		t.Fatalf("calls: %d", len(rows))
	}
	for i, u := range rows {
		wantPurpose, wantRound := "agent", i+1
		if i == 2 {
			wantPurpose, wantRound = "transcript_summary", 0
		}
		if u.CallID == "" || u.Purpose != wantPurpose || u.Round != wantRound {
			t.Fatalf("call %d: %+v", i, u)
		}
		for j := 0; j < i; j++ {
			if rows[j].CallID == u.CallID {
				t.Fatal("distinct turns share logical call ID")
			}
		}
	}
}

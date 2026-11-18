package agent

import (
	"context"
	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/llmmeta"
	"testing"
)

func TestGraphSummaryCallSitePurpose(t *testing.T) {
	var got llmmeta.CallInfo
	p := captureUsageProvider{stream: func(ctx context.Context, y func(llm.StreamEvent, error) bool) {
		got = llmmeta.From(ctx)
		if y(llm.StreamEvent{Type: llm.SETextDelta, Text: "summary"}, nil) {
			y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		}
	}}
	c := NewCompactor(p, "fixture")
	if _, err := c.compress(t.Context(), newColdGraph(nil, nil), block{}, nil); err != nil {
		t.Fatal(err)
	}
	if got.Purpose != "graph_summary" || got.Round != 0 || got.ID == "" {
		t.Fatalf("summary metadata: %+v", got)
	}
}

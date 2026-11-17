package llm

import (
	"strings"
	"testing"
)

func TestAnthropicAdditionalPrefixKeepsLegacyEndpointAndText(t *testing.T) {
	system := []string{"stable rules", "volatile goal", "deferred guidance", "runtime trust"}
	for _, points := range [][]int{{1}, {1, 1, 0, -1, 99}, {3, 1, 2}} {
		req := CompletionRequest{System: system, DynamicBoundary: 3, CacheBreakpoints: points}
		blocks := buildAnthropicSystem(req)
		texts := []string{}
		cached := 0
		for _, b := range blocks {
			texts = append(texts, b.Text)
			if b.CacheControl != nil {
				cached++
			}
		}
		if strings.Join(texts, "\n\n") != strings.Join(system, "\n\n") {
			t.Fatal("system instructions reordered/lost")
		}
		if blocks[0].Text != "stable rules" || blocks[0].CacheControl == nil || blocks[len(blocks)-1].CacheControl != nil || cached < 2 || cached > 4 {
			t.Fatal("wrong cache boundaries", blocks)
		}
	}
	// Ignore additional hints without a legacy boundary; keep old behavior.
	if b := buildAnthropicSystem(CompletionRequest{System: system, CacheBreakpoints: []int{1}}); len(b) != 1 || b[0].CacheControl != nil {
		t.Fatal("enabled caching without boundary")
	}
	many := []string{"a", "b", "c", "d", "e", "f"}
	b := buildAnthropicSystem(CompletionRequest{System: many, DynamicBoundary: 6, CacheBreakpoints: []int{1, 2, 3, 4, 5}})
	if len(b) != 4 || b[3].Text != "d\n\ne\n\nf" || b[3].CacheControl == nil {
		t.Fatal("limit lost final endpoint", b)
	}
}

// The maximum cache endpoint count must still leave room for the dynamic tail.
func TestAnthropicMaximumCacheEndpointsWithDynamicTail(t *testing.T) {
	system := []string{"a", "b", "c", "d", "e", "f", "dynamic"}
	blocks := buildAnthropicSystem(CompletionRequest{System: system, DynamicBoundary: 6, CacheBreakpoints: []int{5, 4, 3, 2, 1}})
	want := []string{"a", "b", "c", "d\n\ne\n\nf", "dynamic"}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d", len(blocks), len(want))
	}
	for i, block := range blocks {
		if block.Text != want[i] || (block.CacheControl != nil) != (i < 4) {
			t.Fatalf("block %d: text=%q cache=%v", i, block.Text, block.CacheControl)
		}
	}
}

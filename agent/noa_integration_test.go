package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
)

func TestNoaUsesConfiguredWindowAndExclusiveFallback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		dir := t.TempDir()
		if fail {
			dir = filepath.Join(dir, "file")
			if err := os.WriteFile(dir, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		opts := agentcore.Options{Compaction: compactionConfig(32000)}
		warned := false
		enableNoa(&opts, func() bool { return true }, dir, "fixture", func(string) { warned = true })
		if fail {
			if !warned || opts.Compaction == nil || opts.Compactor != nil {
				t.Fatal("NOA initialization failed without clean fallback")
			}
			continue
		}
		if opts.Compaction != nil || opts.Compactor == nil {
			t.Fatal("multiple context managers")
		}
		p, ok := opts.Provider.(*noaBudgetProvider)
		if !ok || p.window != 32000 {
			t.Fatalf("configured window lost: %#v", opts.Provider)
		}
	}
}

func TestNoaIrreducibleBudgetStopsBeforeProvider(t *testing.T) {
	calls := 0
	p := captureUsageProvider{stream: func(context.Context, func(llm.StreamEvent, error) bool) { calls++ }}
	guard := &noaBudgetProvider{Provider: p, window: 32000}
	req := llm.CompletionRequest{System: []string{strings.Repeat("system ", 20000)}}
	if _, _, _, err := guard.Complete(t.Context(), req); err == nil {
		t.Fatal("oversized atomic request accepted")
	}
	failed := false
	for _, err := range guard.Stream(t.Context(), req) {
		failed = err != nil
	}
	if !failed || calls != 0 {
		t.Fatalf("stream failed=%v provider calls=%d", failed, calls)
	}
}

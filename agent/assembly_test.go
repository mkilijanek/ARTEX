package agent

import (
	"context"
	"fmt"
	"github.com/Autumn-27/norma/llm"
	"sync"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// Server wiring installs the hooks while restored tasks may already be assembling
// tools, rendering prompts and building settlement configs on other goroutines.
// Readers and writers run concurrently here, so under -race any unsynchronised
// hook access is reported regardless of scheduling.
func TestAgentHooksConcurrentSwap(t *testing.T) {
	oldAugment, oldResolve, oldPolicy := ToolAugment(), ToolResolve(), FindingTrafficBindingEnabled()
	oldPrompt, oldWrapup, oldWrapupTurns := PromptOverride(), WrapupOverride(), WrapupMaxTurnsOverride()
	oldTimeout, oldTimeoutTurns := WrapupTaskTimeoutOverride(), WrapupTaskTimeoutTurnsOverride()
	t.Cleanup(func() {
		SetToolAugment(oldAugment)
		SetToolResolve(oldResolve)
		SetFindingTrafficBindingEnabled(oldPolicy)
		SetPromptOverride(oldPrompt)
		SetWrapupOverride(oldWrapup)
		SetWrapupMaxTurnsOverride(oldWrapupTurns)
		SetWrapupTaskTimeoutOverride(oldTimeout)
		SetWrapupTaskTimeoutTurnsOverride(oldTimeoutTurns)
	})
	augment := func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return nil, DeferredInfo{}, nil
	}
	resolve := func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool { return in }
	policy := func() bool { return false }
	text := func(string) (string, bool) { return "", false }
	turns := func(string) (int, bool) { return 0, false }
	setAll := func(on bool) {
		if on {
			SetToolAugment(augment)
			SetToolResolve(resolve)
			SetFindingTrafficBindingEnabled(policy)
			SetPromptOverride(text)
			SetWrapupOverride(text)
			SetWrapupMaxTurnsOverride(turns)
			SetWrapupTaskTimeoutOverride(text)
			SetWrapupTaskTimeoutTurnsOverride(turns)
			return
		}
		SetToolAugment(nil)
		SetToolResolve(nil)
		SetFindingTrafficBindingEnabled(nil)
		SetPromptOverride(nil)
		SetWrapupOverride(nil)
		SetWrapupMaxTurnsOverride(nil)
		SetWrapupTaskTimeoutOverride(nil)
		SetWrapupTaskTimeoutTurnsOverride(nil)
	}

	provider := captureUsageProvider{stream: func(_ context.Context, y func(llm.StreamEvent, error) bool) {
		if y(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		}
	}}
	workdir := t.TempDir()
	const rounds = 200
	var wg sync.WaitGroup
	for reader := range 4 {
		wg.Go(func() {
			for round := range rounds {
				if round%20 == 0 {
					chat := NewChatAgent(provider, "fixture", workdir, nil, 100000)
					if _, err := chat.Chat(t.Context(), "assistant", fmt.Sprintf("swap-%d-%d", reader, round), "hello", 1, 0, false, nil); err != nil {
						t.Error(err)
						return
					}
				}
				out, _, cleanup := AugmentTools(context.Background(), "worker", nil)
				cleanup()
				if len(out) != 0 {
					t.Errorf("AugmentTools(nil base) = %d tools, want 0", len(out))
					return
				}
				if got := renderSystem("planner", "DEFAULT", PlannerVars{}); got != "DEFAULT" {
					t.Errorf("renderSystem = %q, want the default", got)
					return
				}
				if st := wrapupSettlementForTask("worker", nil, true); st.Prompt != WrapupDefault("worker") {
					t.Errorf("settlement prompt = %q, want the default", st.Prompt)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range rounds {
			setAll(true)
			setAll(false)
		}
	})
	wg.Wait()
}

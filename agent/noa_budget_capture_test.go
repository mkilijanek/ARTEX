package agent

import (
	"context"
	"errors"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"strings"
	"testing"
)

func TestNOABudgetTerminalThroughCapture(t *testing.T) {
	for _, atomic := range []bool{false, true} {
		calls := 0
		inner := captureUsageProvider{stream: func(_ context.Context, _ func(llm.StreamEvent, error) bool) { calls++ }}
		provider := &noaBudgetProvider{Provider: inner, window: 1}
		var activities []db.Activity
		_, reason, err := captureRun(t.Context(), agentcore.Options{Provider: provider, NonStreaming: atomic, MaxTurns: 1}, "fixture", func(a db.Activity) { activities = append(activities, a) })
		if reason != harness.ReasonBlockingLimit || !errors.Is(err, ErrNOAContextBudget) || calls != 0 {
			t.Fatalf("atomic=%v reason=%s err=%v calls=%d", atomic, reason, err, calls)
		}
		if len(activities) == 0 || strings.Contains(activities[len(activities)-1].Detail, "network, auth") {
			t.Fatal("misleading terminal activity")
		}
	}
}

package server

import (
	"context"
	"errors"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/llmmeta"
	"github.com/mkilijanek/artex/llmrec"
)

func TestTaskLLMMetricsAttributeEveryRetry(t *testing.T) {
	defer withZeroRetryBackoff()()
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[streaming], func(t *testing.T) {
			inner := &flakyThenOKProvider{failCount: 2, failErr: errors.New("anthropic: overloaded_error"), okEvents: []llm.StreamEvent{{Type: llm.SEMessageStart}, {Type: llm.SETextDelta, Text: "ok"}, {Type: llm.SEMessageStop}}}
			provider := llmrec.Wrap(inner, nil, "fixture", "profile", "", "", func() bool { return false })
			var rows []db.LLMUsage
			ctx := llmrec.WithUsageObserver(llmmeta.WithCall(t.Context(), "agent", 1), func(u db.LLMUsage) { rows = append(rows, u) })
			hooks := taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) {
				return taskLLMSelection{profileID: 11, provider: provider}, nil
			}}
			var err error
			if streaming {
				_, err = collectTaskLLMStream(streamTaskLLM(ctx, "7", llm.CompletionRequest{}, hooks))
			} else {
				_, _, _, err = completeTaskLLM(ctx, "7", llm.CompletionRequest{}, hooks)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("%d rows, expected all 3 physical calls", len(rows))
			}
			for i, u := range rows {
				want := "error"
				if i == 2 {
					want = "completed"
				}
				if u.Attempt != i || u.Reason != want || u.CallID == "" || u.CallID != rows[0].CallID || u.Round != 1 {
					t.Fatalf("attempt %d: %+v", i, u)
				}
			}
		})
	}
}

func TestTaskLLMMetricsContinueAttemptsAcrossFailover(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "stream"}[streaming], func(t *testing.T) {
			failed := llmrec.Wrap(&scriptedLLMProvider{err: errors.New("quota exhausted")}, nil, "first", "one", "", "", func() bool { return false })
			ok := llmrec.Wrap(&scriptedLLMProvider{events: []llm.StreamEvent{{Type: llm.SETextDelta, Text: "ok"}, {Type: llm.SEMessageStop}}}, nil, "second", "two", "", "", func() bool { return false })
			var rows []db.LLMUsage
			ctx := llmrec.WithUsageObserver(llmmeta.WithCall(t.Context(), "agent", 1), func(u db.LLMUsage) { rows = append(rows, u) })
			advanced := false
			hooks := taskLLMStreamHooks{
				current: func(context.Context) (taskLLMSelection, error) {
					if advanced {
						return taskLLMSelection{profileID: 2, provider: ok}, nil
					}
					return taskLLMSelection{profileID: 1, provider: failed}, nil
				},
				exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
					advanced = true
					id := int64(2)
					return db.TaskLLMTransition{Advanced: true, NextProfileID: &id}, nil
				},
			}
			var err error
			if streaming {
				_, err = collectTaskLLMStream(streamTaskLLM(ctx, "7", llm.CompletionRequest{}, hooks))
			} else {
				_, _, _, err = completeTaskLLM(ctx, "7", llm.CompletionRequest{}, hooks)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].Attempt != 0 || rows[1].Attempt != 1 || rows[1].Model != "second" {
				t.Fatalf("failover attribution: %+v", rows)
			}
		})
	}
}

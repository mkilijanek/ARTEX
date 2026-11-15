package server

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type usageRetryProvider struct {
	calls  int
	fail   error
	always bool
	cancel context.CancelFunc
}

func (p *usageRetryProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if p.calls == 1 || p.always {
		if p.cancel != nil {
			time.AfterFunc(5*time.Millisecond, p.cancel)
		}
		return llm.Message{}, "", llm.Usage{InputTokens: 11, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 2}, p.fail
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{InputTokens: 17, OutputTokens: 5, CacheReadTokens: 4, CacheWriteTokens: 2}, nil
}
func (p *usageRetryProvider) Stream(_ context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		p.calls++
		if p.calls == 1 || p.always {
			if !y(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 11, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 2}}, nil) {
				return
			}
			if p.cancel != nil {
				time.AfterFunc(5*time.Millisecond, p.cancel)
			}
			y(llm.StreamEvent{}, p.fail)
			return
		}
		for _, e := range []llm.StreamEvent{{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 13, CacheReadTokens: 3, CacheWriteTokens: 2}}, {Type: llm.SETextDelta, Text: "done"}, {Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{InputTokens: 17, OutputTokens: 5, CacheReadTokens: 4, CacheWriteTokens: 2}}, {Type: llm.SEMessageStop}} {
			if !y(e, nil) {
				return
			}
		}
	}
}

func TestNormaRetryLogicalUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, mode := range []string{"retry", "failover", "exhausted", "cancel"} {
			t.Run(mode+map[bool]string{true: "/stream", false: "/atomic"}[stream], func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				p := &usageRetryProvider{fail: errors.New("connection reset by peer")}
				retry := agent.RetryConfig{StreamAttempts: 1, StreamInterval: time.Millisecond}
				profile := int64(0)
				if mode == "exhausted" {
					p.always = true
				}
				if mode == "cancel" {
					p.cancel = cancel
					retry.StreamInterval = time.Second
				}
				if mode == "failover" {
					profile = 1
					p.fail = errors.New("insufficient_quota")
					retry.StreamAttempts = -1
				}
				hooks := taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) {
					return taskLLMSelection{provider: p, profileID: profile, retry: retry}, nil
				}, exhaust: func(taskLLMSelection, error) (db.TaskLLMTransition, error) {
					next := int64(2)
					profile = next
					return db.TaskLLMTransition{Advanced: true, NextProfileID: &next}, nil
				}}
				var usage llm.Usage
				var err error
				if stream {
					acc := llm.NewAccumulator()
					for ev, e := range streamTaskLLM(ctx, t.Name(), llm.CompletionRequest{}, hooks) {
						if e != nil {
							err = e
							break
						}
						acc.Add(ev)
					}
					usage = acc.Usage
				} else {
					_, _, usage, err = completeTaskLLM(ctx, t.Name(), llm.CompletionRequest{}, hooks)
				}
				want := llm.Usage{InputTokens: 28, OutputTokens: 7, CacheReadTokens: 7, CacheWriteTokens: 4}
				if mode == "exhausted" {
					want = llm.Usage{InputTokens: 22, OutputTokens: 4, CacheReadTokens: 6, CacheWriteTokens: 4}
					if err == nil {
						t.Fatal("expected exhausted error")
					}
				} else if mode == "cancel" {
					want = llm.Usage{InputTokens: 11, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 2}
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancel error=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if usage != want {
					t.Fatalf("logical usage=%+v want %+v (calls %d)", usage, want, p.calls)
				}
			})
		}
	}
}

func TestNormaOpaqueThinkingCommitsStream(t *testing.T) {
	for _, kind := range []llm.StreamEventType{llm.SEThinkingSignature, llm.SERedactedThinking, llm.SEToolUseSignature} {
		if !streamEventCommitsOutput(llm.StreamEvent{Type: kind, Text: "opaque"}) {
			t.Errorf("opaque %s can be replayed", kind)
		}
	}
}

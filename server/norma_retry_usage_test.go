package server

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
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
				want.ContextTokens = 22
				if mode == "exhausted" || mode == "cancel" {
					want.ContextTokens = 13
				}
				wantCalls := 2
				if mode == "cancel" {
					wantCalls = 1
				}
				if p.calls != wantCalls {
					t.Fatalf("physical calls=%d want=%d", p.calls, wantCalls)
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

type gatePhysicalProvider struct{ calls int }

func (p *gatePhysicalProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if p.calls == 1 {
		return llm.Message{}, "", llm.Usage{InputTokens: 11, OutputTokens: 2}, errors.New("connection reset by peer")
	}
	if p.calls == 2 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "fixture-call", Name: "Fixture", Input: []byte(`{}`)}}}, "tool_use", llm.Usage{InputTokens: 17, OutputTokens: 5}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{InputTokens: 7, OutputTokens: 3}, nil
}
func (p *gatePhysicalProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		m, stop, u, err := p.Complete(ctx, req)
		if !y(llm.StreamEvent{Type: llm.SEMessageStart, Usage: u}, nil) {
			return
		}
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				if !y(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
					return
				}
				if !y(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
					return
				}
			} else if !y(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
				return
			}
		}
		if !y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, nil) {
			return
		}
		y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

type normaGateRecorder struct{ seen []int }

func (c *normaGateRecorder) Pre(_ context.Context, m []llm.Message, n int) []llm.Message {
	c.seen = append(c.seen, n)
	return m
}
func (*normaGateRecorder) Reactive(_ context.Context, m []llm.Message) ([]llm.Message, bool) {
	return m, false
}
func (*normaGateRecorder) IsOverflow(error) bool { return false }

type normaRuntimeFixture struct{ hooks taskLLMStreamHooks }

func (p normaRuntimeFixture) Complete(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return completeTaskLLM(ctx, "norma-sdk-fixture", r, p.hooks)
}
func (p normaRuntimeFixture) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return streamTaskLLM(ctx, "norma-sdk-fixture", r, p.hooks)
}

func TestNormaSDKCompactionUsesPhysicalContextInsteadOfRetryBilling(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{true: "stream", false: "atomic"}[stream], func(t *testing.T) {
			physical := &gatePhysicalProvider{}
			provider := normaRuntimeFixture{hooks: taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) {
				return taskLLMSelection{provider: physical, retry: agent.RetryConfig{StreamAttempts: 1, StreamInterval: time.Millisecond}}, nil
			}}}
			gate := &normaGateRecorder{}
			fixture := tool.Build(tool.Spec{Name: "Fixture", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				return tool.Text("fixture evidence"), nil
			}})
			s := agentcore.NewSession(agentcore.Options{Provider: provider, Compactor: gate, NonStreaming: !stream, Tools: []tool.CoreTool{fixture}, PermissionMode: "bypassPermissions", DisableBackgroundTasks: true, MaxTurns: 3})
			defer s.Close()
			for _, err := range s.Prompt(t.Context(), "only the local fixture") {
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(gate.seen) != 2 || gate.seen[1] != 22 {
				t.Fatalf("compaction gate saw %v; physical context is 22, billed retry total is 35", gate.seen)
			}
			if u := s.Usage(); u.InputTokens != 35 || u.OutputTokens != 10 {
				t.Fatalf("terminal billed usage lost: %+v", u)
			}
		})
	}
}

func TestNormaSDKRetainsAtomicErrorUsage(t *testing.T) {
	p := &usageRetryProvider{fail: errors.New("invalid api key"), always: true}
	provider := normaRuntimeFixture{hooks: taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) {
		return taskLLMSelection{provider: p, retry: agent.RetryConfig{StreamAttempts: -1}}, nil
	}}}
	s := agentcore.NewSession(agentcore.Options{Provider: provider, NonStreaming: true, DisableBackgroundTasks: true})
	defer s.Close()
	failed := false
	for ev, err := range s.Prompt(t.Context(), "fixture") {
		if err != nil || (ev.Terminal != nil && ev.Terminal.Err != nil) {
			failed = true
		}
	}
	if u := s.Usage(); !failed || u.InputTokens != 11 || u.OutputTokens != 2 {
		t.Fatalf("atomic failure usage=%+v failed=%v", u, failed)
	}
}

type committedFailureProvider struct {
	calls int
	kind  llm.StreamEventType
}

func (p *committedFailureProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	panic("unexpected atomic")
}
func (p *committedFailureProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		p.calls++
		if !y(llm.StreamEvent{Type: p.kind, Text: "opaque-or-text"}, nil) {
			return
		}
		y(llm.StreamEvent{}, errors.New("connection reset by peer"))
	}
}
func TestCommittedStreamFailureNeverReplays(t *testing.T) {
	for _, kind := range []llm.StreamEventType{llm.SEThinkingSignature, llm.SERedactedThinking, llm.SEToolUseSignature, llm.SETextDelta} {
		p := &committedFailureProvider{kind: kind}
		hooks := taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) {
			return taskLLMSelection{provider: p, retry: agent.RetryConfig{StreamAttempts: 2, StreamInterval: time.Millisecond}}, nil
		}}
		failed := false
		for _, err := range streamTaskLLM(t.Context(), "fixture", llm.CompletionRequest{}, hooks) {
			if err != nil {
				failed = true
			}
		}
		if !failed || p.calls != 1 {
			t.Fatalf("kind=%s failure=%v calls=%d", kind, failed, p.calls)
		}
	}
}
func TestConsumerStopNeverRetries(t *testing.T) {
	p := &committedFailureProvider{kind: llm.SETextDelta}
	hooks := taskLLMStreamHooks{current: func(context.Context) (taskLLMSelection, error) {
		return taskLLMSelection{provider: p, retry: agent.RetryConfig{StreamAttempts: 2}}, nil
	}}
	for range streamTaskLLM(t.Context(), "fixture", llm.CompletionRequest{}, hooks) {
		break
	}
	if p.calls != 1 {
		t.Fatal("consumer stop replayed", p.calls)
	}
}

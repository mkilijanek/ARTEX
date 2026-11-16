package llmpool

import (
	"context"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"testing"
)

type atomicUsageFixture struct {
	fakeProv
	usage   llm.Usage
	failure error
}

func (p *atomicUsageFixture) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	return llm.Message{}, "end_turn", p.usage, p.failure
}
func TestAtomicPoolRetainsFailedMemberUsage(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		first := &atomicUsageFixture{usage: llm.Usage{InputTokens: 11}, failure: errors.New("HTTP status 429")}
		second := &atomicUsageFixture{usage: llm.Usage{InputTokens: 17}}
		if exhausted {
			second.failure = errors.New("HTTP status 429")
		}
		pool := New([]*Member{member(1, "first", 2, first), member(2, "second", 1, second)}, nil)
		_, _, usage, err := pool.Complete(t.Context(), llm.CompletionRequest{})
		if usage.InputTokens != 28 || usage.ContextTokens != 17 || first.calls != 1 || second.calls != 1 || (err != nil) != exhausted {
			t.Fatalf("usage=%+v calls=%d/%d err=%v", usage, first.calls, second.calls, err)
		}
	}
}

package llmmeta

import (
	"context"
	"iter"
	"sync/atomic"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/google/uuid"
)

type callKey struct{}
type CallInfo struct {
	ID, Purpose string
	Round       int
}

// WithCall starts a logical call. Retry decorators preserve this identity;
// physical attempts remain separate ledger rows.
func WithCall(ctx context.Context, purpose string, round int) context.Context {
	switch purpose {
	case "agent", "transcript_summary", "graph_summary", "side_answer", "side_summary", "judge":
	default:
		purpose = "unclassified"
	}
	return context.WithValue(ctx, callKey{}, CallInfo{uuid.NewString(), purpose, max(0, round)})
}

// MeterDeps labels only harness model turns. The provider fallback is separately
// tagged for SDK transcript summaries, so purpose never depends on prompt text.
func MeterDeps(deps harness.QueryDeps, provider llm.Provider) harness.QueryDeps {
	stream, complete := deps.CallModel, deps.CallModelSync
	if stream == nil {
		stream = provider.Stream
	}
	if complete == nil {
		complete = provider.Complete
	}
	var round atomic.Int64
	deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return stream(WithCall(ctx, "agent", int(round.Add(1))), req)
	}
	deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		return complete(WithCall(ctx, "agent", int(round.Add(1))), req)
	}
	return deps
}

type purposeProvider struct {
	inner   llm.Provider
	purpose string
}

func ForPurpose(inner llm.Provider, purpose string) llm.Provider {
	return &purposeProvider{inner, purpose}
}
func (p *purposeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.inner.Stream(WithCall(ctx, p.purpose, 0), req)
}
func (p *purposeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.inner.Complete(WithCall(ctx, p.purpose, 0), req)
}

func From(ctx context.Context) CallInfo { info, _ := ctx.Value(callKey{}).(CallInfo); return info }

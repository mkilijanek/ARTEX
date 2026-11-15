package agent

import (
	"context"
	"fmt"
	"iter"

	"github.com/Autumn-27/artex/sidequestion"
	"github.com/Autumn-27/norma/llm"
)

// NOA's view interface cannot return an error when protected content alone is
// too large. Stop at the final provider boundary, including system/tool overhead,
// rather than submit the same irreducible request or silently discard evidence.
// This is an estimate; the provider remains authoritative about actual tokens.
type noaBudgetProvider struct {
	llm.Provider
	window int
}

func (p *noaBudgetProvider) check(req llm.CompletionRequest) error {
	estimate := sidequestion.EstimateInputTokens(req) + max(req.MaxTokens, 0) + 512
	if estimate > p.window {
		return fmt.Errorf("Norma NOA request estimate %d exceeds configured budget %d; protected context cannot fit, increase the configured window or start a new session", estimate, p.window)
	}
	return nil
}
func (p *noaBudgetProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if err := p.check(req); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	return p.Provider.Complete(ctx, req)
}
func (p *noaBudgetProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		if err := p.check(req); err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for ev, err := range p.Provider.Stream(ctx, req) {
			if !y(ev, err) {
				return
			}
		}
	}
}

package sidequestion

import (
	"context"
	"github.com/Autumn-27/norma/llm"
	"iter"
	"testing"
)

func TestAnswerUsageEchoIsCountedOnce(t *testing.T) {
	p := fakeProvider{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(y func(llm.StreamEvent, error) bool) {
			for _, ev := range []llm.StreamEvent{
				{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 20}},
				{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 100, CacheReadTokens: 20, OutputTokens: 4}},
				{Type: llm.SETextDelta, Text: "answer"}, {Type: llm.SEMessageStop},
			} {
				if !y(ev, nil) {
					return
				}
			}
		}
	}}
	out, err := (SideQuestionService{Provider: p}).Answer(t.Context(), llm.CompletionRequest{}, true, nil)
	if err != nil || out.Usage.InputTokens != 100 || out.Usage.CacheReadTokens != 20 || out.Usage.OutputTokens != 4 {
		t.Fatalf("echoed usage: %+v %v", out, err)
	}
}

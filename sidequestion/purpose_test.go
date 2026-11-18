package sidequestion

import (
	"context"
	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/llmmeta"
	"testing"
)

func TestSideCallSitePurposes(t *testing.T) {
	var got []llmmeta.CallInfo
	p := fakeProvider{complete: func(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		got = append(got, llmmeta.From(ctx))
		return assistant("short summary"), "end_turn", llm.Usage{}, nil
	}}
	s := SideQuestionService{Provider: p}
	if _, err := s.Answer(t.Context(), llm.CompletionRequest{}, false, nil); err != nil {
		t.Fatal(err)
	}
	b := contextBuilder{service: s, window: 200000}
	if _, err := b.summarize(t.Context(), "", "evidence"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatal(got)
	}
	for i, purpose := range []string{"side_answer", "side_summary"} {
		if got[i].ID == "" || got[i].Purpose != purpose || got[i].Round != 0 {
			t.Fatalf("call %d: %+v", i, got[i])
		}
	}
}

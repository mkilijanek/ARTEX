package agent

import (
	"context"
	"errors"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	"iter"
	"testing"
)

type summaryFixture struct {
	calls int
	err   error
}

func (p *summaryFixture) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}
func (p *summaryFixture) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("summary with source references")}}, "stop", llm.Usage{}, p.err
}

func TestCompactionMemoIncludesSourcesAndIgnoresFailures(t *testing.T) {
	t.Setenv("ARTEX_COMPACTION_MEMO", "1")
	p := &summaryFixture{}
	c := NewCompactor(p, "fixture")
	g := newColdGraph([]cgNode{intent(1, "done"), fact(2)}, []cgEdge{yields(1, 2)})
	b := block{Members: []int64{1, 2}}
	nodes := map[int64]*db.Node{1: {ID: 1, Kind: db.KindIntent, State: "done", Payload: []byte(`{"summary":"source"}`)}, 2: {ID: 2, Kind: db.KindFact, Payload: []byte(`{"summary":"negative","confidence":"observed"}`)}}
	for range 2 {
		if _, err := c.compress(t.Context(), g, b, nodes); err != nil {
			t.Fatal(err)
		}
	}
	if calls, hits := c.SummaryStats(); calls != 1 || hits != 1 {
		t.Fatalf("calls/hits=%d/%d", calls, hits)
	}
	nodes[2].Payload = []byte(`{"summary":"changed source"}`)
	if _, err := c.compress(t.Context(), g, b, nodes); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatal("changed source reused stale summary")
	}
	p.err = errors.New("provider failed")
	nodes[2].Payload = []byte(`{"summary":"failure source"}`)
	for range 2 {
		if _, err := c.compress(t.Context(), g, b, nodes); err == nil {
			t.Fatal("failure cached as success")
		}
	}
	if p.calls != 4 {
		t.Fatal("failed summary was memoized")
	}
	if string(nodes[2].Payload) != `{"summary":"failure source"}` {
		t.Fatal("source evidence changed")
	}
}

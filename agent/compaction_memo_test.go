package agent

import (
	"context"
	"errors"
	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/db"
	"iter"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type summaryFixture struct {
	calls int
	err   error
	stop  string
}

func (p *summaryFixture) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}
func (p *summaryFixture) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	stop := p.stop
	if stop == "empty" {
		stop = ""
	} else if stop == "" {
		stop = "stop"
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("summary with source references")}}, stop, llm.Usage{}, p.err
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

func TestCompactionMemoRejectsIncompleteSummaries(t *testing.T) {
	t.Setenv("ARTEX_COMPACTION_MEMO", "1")
	p := &summaryFixture{stop: "max_tokens"}
	c := NewCompactor(p, "fixture")
	g := newColdGraph([]cgNode{intent(1, "done")}, nil)
	for range 2 {
		if _, err := c.compress(t.Context(), g, block{Members: []int64{1}}, map[int64]*db.Node{1: {ID: 1}}); err == nil {
			t.Fatal("accepted truncated summary")
		}
	}
	if p.calls != 2 || len(c.summaryMemo) != 0 {
		t.Fatal("cached incomplete summary")
	}
}
func TestSummaryMemoLRUAndConcurrentReuse(t *testing.T) {
	c := NewCompactor(&summaryFixture{}, "fixture")
	var calls atomic.Int64
	compute := func() (string, error) { calls.Add(1); return "summary", nil }
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := c.memoSummary(t.Context(), [32]byte{1}, compute); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate inference", calls.Load())
	}
	for i := 2; i <= 64; i++ {
		_, _ = c.memoSummary(t.Context(), [32]byte{byte(i)}, compute)
	}
	_, _ = c.memoSummary(t.Context(), [32]byte{1}, compute)
	_, _ = c.memoSummary(t.Context(), [32]byte{65}, compute)
	if _, ok := c.summaryMemo[[32]byte{1}]; !ok {
		t.Fatal("evicted recently used summary")
	}
	if _, ok := c.summaryMemo[[32]byte{2}]; ok {
		t.Fatal("did not evict least recently used summary")
	}
	if len(c.summaryMemo) != 64 {
		t.Fatal("cache flushed rather than evicting one")
	}
}
func TestCompactionFlagOffKeepsReturnedCompletion(t *testing.T) {
	t.Setenv("ARTEX_COMPACTION_MEMO", "0")
	p := &summaryFixture{stop: "max_tokens"}
	c := NewCompactor(p, "fixture")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Provider deliberately returns a paid result despite cancellation, as before memoization.
	if _, err := c.compress(ctx, newColdGraph(nil, nil), block{}, nil); err != nil || p.calls != 1 {
		t.Fatalf("flag-off behavior changed: %v", err)
	}
}

func TestSummaryFlightReleasesOnPanic(t *testing.T) {
	c := NewCompactor(&summaryFixture{}, "fixture")
	if _, err := c.memoSummary(t.Context(), [32]byte{1}, func() (string, error) { panic("fixture") }); err == nil {
		t.Fatal("panic lost")
	}
	if len(c.summaryFlights) != 0 {
		t.Fatal("flight leaked")
	}
	if body, err := c.memoSummary(t.Context(), [32]byte{1}, func() (string, error) { return "retry", nil }); err != nil || body != "retry" {
		t.Fatal(body, err)
	}
}
func TestMemoAcceptsEmptyCompletionReason(t *testing.T) {
	t.Setenv("ARTEX_COMPACTION_MEMO", "1")
	p := &summaryFixture{stop: "empty"}
	c := NewCompactor(p, "fixture")
	for range 2 {
		if _, err := c.compress(t.Context(), newColdGraph(nil, nil), block{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if p.calls != 1 {
		t.Fatal("complete response not reused")
	}
}

func TestSummaryFlightWaiterJoinsInProgressCall(t *testing.T) {
	c := NewCompactor(&summaryFixture{}, "fixture")
	started := make(chan struct{})
	release := make(chan struct{})
	owner := make(chan error, 1)
	waiter := make(chan error, 1)
	compute := func() (string, error) { close(started); <-release; return "shared", nil }
	go func() { _, err := c.memoSummary(t.Context(), [32]byte{9}, compute); owner <- err }()
	<-started
	go func() {
		_, err := c.memoSummary(t.Context(), [32]byte{9}, func() (string, error) { t.Error("second inference started"); return "bad", nil })
		waiter <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	joined := false
	for time.Now().Before(deadline) {
		c.mu.Lock()
		f := c.summaryFlights[[32]byte{9}]
		joined = f != nil && f.waiters == 1
		c.mu.Unlock()
		if joined {
			break
		}
		runtime.Gosched()
	}
	close(release)
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	if !joined {
		t.Fatal("waiter did not join in-flight inference")
	}
}

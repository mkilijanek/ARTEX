package llmrec

import (
	"context"
	"iter"
	"os"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/google/uuid"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/llmmeta"
)

type completeProvider struct{}

func (completeProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (completeProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "reasoning"},
			llm.TextBlock("answer"),
		},
	}, "stop", llm.Usage{InputTokens: 7, OutputTokens: 3}, nil
}

func TestTaskIDContextUsesExplicitRegistryID(t *testing.T) {
	ctx := WithTaskID(context.Background(), " 42 ")
	if got := TaskIDFrom(ctx); got != "42" {
		t.Fatalf("TaskIDFrom()=%q want 42", got)
	}
	if got := TaskIDFrom(WithTaskID(ctx, "   ")); got != "42" {
		t.Fatalf("blank task id should preserve parent context, got %q", got)
	}
	if got := TaskIDFrom(nil); got != "" {
		t.Fatalf("nil context returned %q", got)
	}
}

func TestParseSessionFallbackIsExplorationScoped(t *testing.T) {
	taskID, worker := parseSession("exp12-worker-i99")
	if taskID != "12" || worker != "worker" {
		t.Fatalf("parseSession()=(%q,%q)", taskID, worker)
	}
	if taskID, worker := parseSession("not-a-task"); taskID != "" || worker != "" {
		t.Fatalf("unexpected non-session parse: (%q,%q)", taskID, worker)
	}
}

func TestCompleteForwardsAtomicResponse(t *testing.T) {
	recorder := Wrap(completeProvider{}, nil, "model", "profile", "", "", func() bool { return false })
	msg, stopReason, usage, err := recorder.Complete(context.Background(), llm.CompletionRequest{})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if got := msg.Text(); got != "answer" {
		t.Fatalf("Complete() text=%q want answer", got)
	}
	if stopReason != "stop" {
		t.Fatalf("Complete() stop reason=%q want stop", stopReason)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Fatalf("Complete() usage=%+v", usage)
	}
}

type meteredStreamProvider struct{ completeProvider }

func (meteredStreamProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 23, CacheReadTokens: 5, CacheWriteTokens: 3}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 23, CacheReadTokens: 5, CacheWriteTokens: 3, OutputTokens: 5}}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestNormaUsageEchoLedger(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	profile := "norma-echo-" + uuid.NewString()
	if err := pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	defer pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	recorder := Wrap(meteredStreamProvider{}, pg, "fixture", profile, "", "", func() bool { return false })
	ctx := llmmeta.WithCall(t.Context(), "transcript_summary", 0)
	for _, err := range recorder.Stream(ctx, llm.CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count, input, output, read, write int
	err = pg.QueryRow(`SELECT count(*),sum(input_tokens),sum(output_tokens),sum(cache_read),sum(cache_write) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count, &input, &output, &read, &write)
	if err != nil || count != 1 || input != 23 || output != 5 || read != 5 || write != 3 {
		t.Fatalf("ledger=%d %d/%d/%d/%d err=%v", count, input, output, read, write, err)
	}
	var id, purpose string
	err = pg.QueryRow(`SELECT call_id,purpose FROM llm_usage WHERE profile_name=$1`, profile).Scan(&id, &purpose)
	if err != nil || id != llmmeta.From(ctx).ID || purpose != "transcript_summary" {
		t.Fatalf("persisted attribution=%q/%q err=%v", id, purpose, err)
	}
}

func TestNormaConsumerStopPersistsUsageOnce(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if err := pg.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	profile := "norma-stopped-" + uuid.NewString()
	defer pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	r := Wrap(meteredStreamProvider{}, pg, "fixture", profile, "", "", func() bool { return false })
	for ev, err := range r.Stream(t.Context(), llm.CompletionRequest{}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == llm.SEMessageDelta {
			break
		}
	}
	var count, input, output int
	var status string
	err = pg.QueryRow(`SELECT count(*),sum(input_tokens),sum(output_tokens),max(status) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count, &input, &output, &status)
	if err != nil || count != 1 || input != 23 || output != 5 || status != "error" {
		t.Fatalf("ledger=%d %d/%d %s err=%v", count, input, output, status, err)
	}
}

func TestSideUsageRecordedOnceOnConsumerCancellation(t *testing.T) {
	t.Skip("timing-sensitive: passes locally, flaky on CI runners; tracked in BaaRai/ARTEX#9")
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	for _, early := range []bool{false, true} {
		profile := "btw-metering-" + uuid.NewString()
		ctx, cancel := context.WithCancel(transcript.WithSessionID(t.Context(), "exp0-btw-test"))
		recorder := Wrap(meteredStreamProvider{}, pg, "fixture", profile, "", "", func() bool { return false })
		for event, err := range recorder.Stream(ctx, llm.CompletionRequest{}) {
			if err != nil {
				t.Fatal(err)
			}
			if early && event.Type == llm.SEMessageDelta {
				cancel()
				break // Consumer exits before the provider can return a cancellation event.
			}
		}
		cancel()
		var count, input, output int
		var worker, status string
		err = pg.QueryRow(`SELECT count(*),max(input_tokens),max(output_tokens),max(worker),max(status)
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count, &input, &output, &worker, &status)
		_, _ = pg.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus := "ok"
		if early {
			wantStatus = "error"
		}
		if count != 1 || input != 23 || output != 5 || worker != "btw" || status != wantStatus {
			t.Fatalf("early=%v: count=%d usage=%d/%d worker=%q status=%q", early, count, input, output, worker, status)
		}
	}
}

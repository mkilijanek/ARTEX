package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type auditReplayProvider struct{}

func (auditReplayProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}
func (auditReplayProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	// A deterministic protocol fixture, not a model-quality or billing simulator.
	text := "Fixture summary; observations remain data. " + req.Messages[0].Text()
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(text)}}, "end_turn", llm.Usage{InputTokens: 2000, OutputTokens: 300}, nil
}

func TestCompactionRepresentativeReplay(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	reports := []map[string]any{}
	for _, flag := range []string{"0", "1"} {
		t.Run("memo="+flag, func(t *testing.T) {
			t.Setenv("ARTEX_COMPACTION_MEMO", flag)
			exp, err := d.CreateExploration("Phase C replay", "local evidence only")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
			ts := d.Exploration(exp)
			must := func(id int64, err error) int64 {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			source := map[int64]string{}
			changed := int64(0)
			for direction := range 8 {
				payload := map[string]any{"summary": fmt.Sprintf("settled local direction %d", direction)}
				parent := must(ts.AddNode(db.KindIntent, payload, 0, "done", "fixture", nil))
				node, _ := ts.GetNode(parent)
				source[parent] = string(node.Payload)
				for i := range 12 {
					id := must(ts.AddNode(db.KindFact, map[string]any{"summary": fmt.Sprintf("direction %d local observation %d: no evidence of condition X", direction, i), "confidence": "observed", "evidence": "fixture.txt:7"}, 0, "confirmed", "fixture", nil))
					if err := ts.Link(parent, db.RelYields, id); err != nil {
						t.Fatal(err)
					}
					node, _ := ts.GetNode(id)
					source[id] = string(node.Payload)
					if changed == 0 {
						changed = id
					}
				}
			}
			c := NewCompactor(auditReplayProvider{}, "protocol-fixture")
			wait := func() {
				t.Helper()
				deadline := time.Now().Add(20 * time.Second)
				for {
					c.mu.Lock()
					running := c.running[exp]
					c.mu.Unlock()
					if !running {
						return
					}
					if time.Now().After(deadline) {
						t.Fatal("compaction did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			}
			round := func(advanceCooldown bool) {
				t.Helper()
				if advanceCooldown {
					c.mu.Lock()
					c.lastRun[exp] = time.Now().Add(-61 * time.Second)
					c.mu.Unlock()
				}
				c.OnPlannerRound(t.Context(), ts)
				wait()
			}
			for range 7 {
				round(false)
			}
			// Production cooldown suppresses a major run, even though eight digests
			// meet its threshold. Later replay rounds advance only this test clock.
			c.mu.Lock()
			c.lastRun[exp] = time.Now()
			c.mu.Unlock()
			round(false)
			for range 5 {
				round(true)
			}
			// A changed evidence version must trigger precisely one new source fold.
			if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{summary}',to_jsonb('updated local negative observation'::text)),content_version=content_version+1 WHERE id=$1 AND exploration_id=$2`, changed, exp); err != nil {
				t.Fatal(err)
			}
			updated, _ := ts.GetNode(changed)
			source[changed] = string(updated.Payload)
			round(true)
			round(true)
			stats := c.AuditStats()
			if stats.MaintenanceRounds != 15 || stats.SuppressedRuns != 1 || stats.MinorRuns != 1 || stats.MajorRuns != 8 || stats.ProviderInvocations != 9 || stats.SignatureReuses != 63 || stats.MemoReturns != 0 {
				t.Fatalf("unexpected replay frequency/reuse: %+v", stats)
			}
			if stats.InputTokens != 18000 || stats.OutputTokens != 2700 || stats.ProviderErrors != 0 || stats.InputBytes == 0 || stats.OutputBytes == 0 || stats.ZeroUsageResponses != 0 {
				t.Fatalf("usage telemetry mismatch: %+v", stats)
			}
			for id, want := range source {
				node, err := ts.GetNode(id)
				if err != nil || string(node.Payload) != want {
					t.Fatal("original evidence replaced", id, err)
				}
			}
			digests, err := ts.ActiveDigests()
			if err != nil || len(digests) != 8 {
				t.Fatal("source folds not materialized", len(digests), err)
			}
			report := map[string]any{"mode": "deterministic_protocol_fixture", "memo_flag": flag, "topology": "eight settled directions, twelve observations each; actual OnPlannerRound thresholds and cooldown suppression; accelerated cooldown between five unchanged majors, one changed-source major and final unchanged major", "stats": stats, "example_tariff_eur_per_million": map[string]int{"input": 2, "output": 8}, "example_cost_eur": float64(stats.InputTokens)*2/1e6 + float64(stats.OutputTokens)*8/1e6, "actual_provider_cost_known": false}
			reports = append(reports, report)
			raw, _ := json.Marshal(report)
			t.Log(string(raw))
		})
	}
	if path := os.Getenv("ARTEX_PHASE_C_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(reports, "", "  ")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompactionAuditDoesNotTreatMissingUsageOrTruncationAsFree(t *testing.T) {
	t.Setenv("ARTEX_COMPACTION_MEMO", "1")
	p := &summaryFixture{stop: "max_tokens"}
	c := NewCompactor(p, "fixture")
	for range 2 {
		_, _ = c.compress(t.Context(), newColdGraph(nil, nil), block{}, nil)
	}
	stats := c.AuditStats()
	if stats.ProviderInvocations != 2 || stats.TruncatedResponses != 2 || stats.RejectedSummaries != 2 || stats.ZeroUsageResponses != 2 || stats.MemoReturns != 0 {
		t.Fatal("invalid summary silently cached or usage invented", stats)
	}
	raw, _ := json.Marshal(stats)
	if strings.Contains(string(raw), "summary with source") {
		t.Fatal("evidence leaked into numeric audit")
	}
}

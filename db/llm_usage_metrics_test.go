package db

import (
	"fmt"
	"testing"
	"time"
)

func TestUsageMetricsLegacySchemaIsAdditiveAndRepeatable(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	schema := fmt.Sprintf("usage_metrics_%d", time.Now().UnixNano())
	if _, err = tx.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`SET LOCAL search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	// Original v0.5.0 ledger, with a real historical row before upgrades.
	_, err = tx.Exec(`CREATE TABLE llm_usage (
 id BIGSERIAL PRIMARY KEY, ts TIMESTAMPTZ NOT NULL DEFAULT now(),
 task_id TEXT, exploration_id BIGINT, worker TEXT, model TEXT, profile_name TEXT,
 latency_ms INTEGER, input_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0, cache_read INTEGER NOT NULL DEFAULT 0,
 cache_write INTEGER NOT NULL DEFAULT 0, status TEXT);
 INSERT INTO llm_usage(task_id,input_tokens) VALUES ('legacy',123)`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = tx.Exec(llmUsageSchema); err != nil {
			t.Fatal(err)
		}
	}
	var tokens, attempt, bytes int
	var known bool
	var provider string
	err = tx.QueryRow(`SELECT input_tokens,attempt,request_bytes,size_known,provider FROM llm_usage WHERE task_id='legacy'`).Scan(&tokens, &attempt, &bytes, &known, &provider)
	if err != nil || tokens != 123 || attempt != 0 || bytes != 0 || known || provider != "" {
		t.Fatalf("legacy row changed: %d %d %d %v %q: %v", tokens, attempt, bytes, known, provider, err)
	}
}

func TestUsageMetricsInsertRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for range 2 {
		if err := d.EnsureLLMUsageTable(); err != nil {
			t.Fatal(err)
		}
	}
	profile := fmt.Sprintf("metrics-%d", time.Now().UnixNano())
	defer d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile)
	u := &LLMUsage{ProfileName: profile, Provider: "ollama", IntentID: 99, Attempt: 2, Reason: "consumer_stopped", RequestBytes: 123, SystemBytes: 20, MessageBytes: 40, ToolSchemaBytes: 30, ToolResultBytes: 10, SizeKnown: true}
	if err := d.InsertLLMUsage(u); err != nil {
		t.Fatal(err)
	}
	var got LLMUsage
	err = d.QueryRow(`SELECT provider,intent_id,attempt,reason,request_bytes,system_bytes,message_bytes,tool_schema_bytes,tool_result_bytes,size_known FROM llm_usage WHERE profile_name=$1`, profile).Scan(&got.Provider, &got.IntentID, &got.Attempt, &got.Reason, &got.RequestBytes, &got.SystemBytes, &got.MessageBytes, &got.ToolSchemaBytes, &got.ToolResultBytes, &got.SizeKnown)
	got.ProfileName = profile
	if err != nil || got != *u {
		t.Fatalf("roundtrip: %+v want %+v: %v", got, *u, err)
	}
}


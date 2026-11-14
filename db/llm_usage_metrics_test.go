package db

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
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
	u := &LLMUsage{CallID: "fixture-call", Round: 3, Purpose: "agent", ToolBytes: map[string]ToolBytes{"opaque": {Schema: 12, Result: 5}}, ProfileName: profile, Provider: "ollama", IntentID: 99, Attempt: 2, Reason: "consumer_stopped", RequestBytes: 123, SystemBytes: 20, MessageBytes: 40, ToolSchemaBytes: 30, ToolResultBytes: 10, SizeKnown: true}
	if err := d.InsertLLMUsage(u); err != nil {
		t.Fatal(err)
	}
	var got LLMUsage
	var tools []byte
	err = d.QueryRow(`SELECT provider,intent_id,attempt,reason,request_bytes,system_bytes,message_bytes,tool_schema_bytes,tool_result_bytes,size_known,call_id,round_no,purpose,tool_bytes FROM llm_usage WHERE profile_name=$1`, profile).Scan(&got.Provider, &got.IntentID, &got.Attempt, &got.Reason, &got.RequestBytes, &got.SystemBytes, &got.MessageBytes, &got.ToolSchemaBytes, &got.ToolResultBytes, &got.SizeKnown, &got.CallID, &got.Round, &got.Purpose, &tools)
	if err == nil {
		err = json.Unmarshal(tools, &got.ToolBytes)
	}
	got.ProfileName = profile
	if err != nil || !reflect.DeepEqual(got, *u) {
		t.Fatalf("roundtrip: %+v want %+v: %v", got, *u, err)
	}
}

func TestCostSeparatesFreshReadAndWriteBuckets(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	name := fmt.Sprintf("cache-price-%d", time.Now().UnixNano())
	id, err := d.SaveProfile(&LLMProfile{Name: name, Format: "anthropic", Model: "fixture", PriceInputPerM: 2, PriceOutputPerM: 4, PriceCacheReadPerM: 0.2, PriceCacheWritePerM: 2.5})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	for _, tc := range []struct {
		name            string
		in, read, write int
		want            float64
	}{
		{"fresh", 1000000, 0, 0, 2},
		{"read", 1000000, 1000000, 0, 0.2},
		{"write", 1000000, 0, 1000000, 2.5},
		{"mixed", 1000000, 200000, 300000, 1.79},
		{"inconsistent_gateway", 100000, 200000, 300000, 0.79},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := []ProfileUsage{{ProfileName: name, InputTokens: tc.in, CacheReadTokens: tc.read, CacheWriteTokens: tc.write, OutputTokens: 100000}, {ProfileName: "unknown-" + name}}
			if err := applyCostEur(d, out); err != nil {
				t.Fatal(err)
			}
			if out[0].CostEur == nil || math.Abs(*out[0].CostEur-(tc.want+0.4)) > 1e-9 {
				t.Fatalf("cost %+v, expected %v", out[0].CostEur, tc.want+0.4)
			}
			if out[1].CostEur != nil {
				t.Fatal("unpriced usage must remain unknown")
			}
		})
	}
}

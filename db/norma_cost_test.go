package db

import (
	"fmt"
	"math"
	"testing"
	"time"
)

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

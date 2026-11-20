package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// goldenCatalog is a deterministic synthetic catalog that exercises the scoring hot
// path: repeated and rare words, mixed case, digits, punctuation, non-ASCII text,
// descriptions beyond the 8 KiB scoring cap, parameter names, and ties.
func goldenCatalog() (tools []actool.CoreTool, query string) {
	vocab := []string{"scan", "Probe", "SQL", "injection", "port", "host", "TLS", "cert", "dns", "http2",
		"fuzz", "brute", "login", "ssh", "smb", "ldap", "Ünïcode", "ŞCAN", "İstanbul", "straße", "日本語", "tool_x", "a1b2", "v6"}
	state := uint32(12345)
	next := func(n int) int {
		state = state*1664525 + 1013904223
		return int(state>>8) % n
	}
	for i := 0; i < 64; i++ {
		var sb strings.Builder
		words := 20 + next(60)
		if i%9 == 0 {
			words = 2500 // well past toolRAGMaxToolBytes
		}
		for w := 0; w < words; w++ {
			sb.WriteString(vocab[next(len(vocab))])
			sb.WriteString([]string{" ", ", ", "-", ". ", "/"}[next(5)])
		}
		name := fmt.Sprintf("tool_%02d", i%40) // names repeat from i=40: exercises the name tie-break
		if i%7 == 0 {
			name = fmt.Sprintf("Tool%d", i)
		}
		params := []string{"target", "Port"}
		if i%3 == 0 {
			params = append(params, "sql_query", "x1")
		}
		tools = append(tools, ragTool(name, sb.String(), params...))
	}
	// Two identical tools: equal scores, tie broken by name then input order.
	tools = append(tools, ragTool("dup", "scan probe", "target"), ragTool("dup", "scan probe", "target"))
	return tools, "Scan the HOST for SQL injection; probe port 443 TLS cert, ŞCAN straße 日本語 unknownword scan"
}

func TestToolRAGGoldenScores(t *testing.T) {
	tools, query := goldenCatalog()
	scores, err := bm25Score(query, tools)
	if err != nil {
		t.Fatal(err)
	}
	idx, _, err := rankIndex(query, tools, bm25Score)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, j := range idx {
		fmt.Fprintf(&got, "%d %s %.12f\n", j, tools[j].Name(), scores[j])
	}
	// Same catalog through the observer, which caps each text at 8 KiB first.
	obs := observeToolRAG(WithToolQuery(context.Background(), query), "worker", tools, DeferredInfo{}, nil)
	fmt.Fprintf(&got, "observer %s nonzero=%d qtok=%d\n%s\n", obs.Outcome, obs.NonzeroCount, obs.QueryTokens, strings.Join(obs.Ranking, ","))
	if p := os.Getenv("TOOLRAG_GOLDEN_OUT"); p != "" {
		os.WriteFile(p, []byte(got.String()), 0o644)
	}
	if got.String() != toolRAGGolden {
		t.Errorf("ranking or scores changed.\ngot:\n%s", got.String())
	}
}

// TestToolRAGObserveAllocBudget guards plan 3.9: assembling the capped maximum
// (512 tools x 8 KiB, 4 KiB query) allocates at most 16 MiB. It bounds bytes, not
// time, because bytes per operation do not depend on machine load (testing.Benchmark
// averages over many runs), and the bound sits well above the measured ~5.5 MiB so
// only a real regression trips it.
func TestToolRAGObserveAllocBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a benchmark")
	}
	tools := make([]actool.CoreTool, 512)
	for i := range tools {
		text := strings.Repeat(fmt.Sprintf("word%d scanner probe ", i%17), 8192/20)
		tools[i] = ragTool(fmt.Sprintf("tool_%03d", i), text, "target", "port")
	}
	query := strings.Repeat("scanner probe word3 target ", 4096/27+1)[:4096]
	ctx := WithToolQuery(context.Background(), query)
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if o := observeToolRAG(ctx, "worker", tools, DeferredInfo{}, nil); o == nil || len(o.Ranking) == 0 {
				b.Fatal("no ranking")
			}
		}
	})
	if got := res.AllocedBytesPerOp(); got > 16<<20 {
		t.Errorf("capped assembly allocates %d bytes/op, budget %d", got, 16<<20)
	}
}

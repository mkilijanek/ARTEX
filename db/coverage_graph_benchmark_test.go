package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"testing"
	"time"
)

type coverageQueryMeasurement struct {
	Query  string  `json:"query"`
	Calls  int64   `json:"calls"`
	ExecMS float64 `json:"exec_ms"`
}
type coverageMeasurement struct {
	TaskID         int64                      `json:"task_id"`
	ExpID          int64                      `json:"exploration_id"`
	Iterations     int                        `json:"iterations"`
	Nodes          int                        `json:"nodes"`
	Edges          int                        `json:"edges"`
	JSONBytes      int                        `json:"json_bytes"`
	CanonicalSHA   string                     `json:"canonical_sha256"`
	P50MS          float64                    `json:"p50_ms"`
	P95MS          float64                    `json:"p95_ms"`
	QueriesPerCall float64                    `json:"queries_per_call"`
	Queries        []coverageQueryMeasurement `json:"queries"`
	ICPIndex       bool                       `json:"icp_index"`
	Plan           json.RawMessage            `json:"plan"`
}

func canonicalCoverageGraph(g *CoverageGraphData) []byte {
	copy := CoverageGraphData{Nodes: append([]CoverageGraphNode(nil), g.Nodes...), Edges: append([]CoverageGraphEdge(nil), g.Edges...)}
	sort.Slice(copy.Nodes, func(i, j int) bool { return copy.Nodes[i].Key < copy.Nodes[j].Key })
	sort.Slice(copy.Edges, func(i, j int) bool {
		if copy.Edges[i].Src == copy.Edges[j].Src {
			return copy.Edges[i].Dst < copy.Edges[j].Dst
		}
		return copy.Edges[i].Src < copy.Edges[j].Src
	})
	raw, _ := json.Marshal(copy)
	return raw
}

// With ARTEX_DB_PERF_REPORT set, the benchmark keeps one fixture in a disposable
// database and reuses it across before/after runs. It fails on graph-content or
// encoded-size drift. Query-stat reset is opt-in and requires a dedicated PG
// instance with pg_stat_statements preloaded; no such mutation occurs by default.
func BenchmarkBuildCoverageGraphLarge(b *testing.B) {
	dsn, _, err := DSN()
	if err != nil {
		b.Fatal(err)
	}
	d, err := Open(dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { d.Close() })
	path := os.Getenv("ARTEX_DB_PERF_REPORT")
	stats := os.Getenv("ARTEX_DB_PERF_QUERY_STATS") == "1"
	var previous coverageMeasurement
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(raw, &previous); err != nil {
				b.Fatal(err)
			}
		}
	}
	var f largeCoverageFixture
	if previous.TaskID > 0 {
		task, err := d.GetTask(previous.TaskID)
		if err != nil || task == nil || task.Description != "large coverage graph" {
			b.Fatal("retained workload unavailable", err)
		}
		f = largeCoverageFixture{taskID: task.ID, expID: task.ExplorationID}
	} else {
		f = seedLargeCoverageFixture(b, d, path != "")
	}
	store := d.Assets()
	if _, err := store.BuildCoverageGraph(f.taskID, f.expID); err != nil {
		b.Fatal(err)
	}
	if stats {
		if _, err := d.Exec(`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
			b.Fatal(err)
		}
		if _, err := d.Exec(`SELECT pg_stat_statements_reset()`); err != nil {
			b.Fatal(err)
		}
	}
	times := make([]float64, 0, b.N)
	var graph *CoverageGraphData
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		started := time.Now()
		graph, err = store.BuildCoverageGraph(f.taskID, f.expID)
		if err != nil {
			b.Fatal(err)
		}
		if path != "" {
			times = append(times, float64(time.Since(started).Nanoseconds())/1e6)
		}
	}
	b.StopTimer()
	if len(graph.Nodes) == 0 {
		b.Fatal("empty graph")
	}
	if path == "" {
		return
	}
	sort.Float64s(times)
	percentile := func(n int) float64 { index := (len(times)*n+99)/100 - 1; return times[max(0, index)] }
	raw, _ := json.Marshal(graph)
	hash := sha256.Sum256(canonicalCoverageGraph(graph))
	report := coverageMeasurement{TaskID: f.taskID, ExpID: f.expID, Iterations: b.N, Nodes: len(graph.Nodes), Edges: len(graph.Edges), JSONBytes: len(raw), CanonicalSHA: hex.EncodeToString(hash[:]), P50MS: percentile(50), P95MS: percentile(95)}
	if previous.CanonicalSHA != "" && (report.CanonicalSHA != previous.CanonicalSHA || report.JSONBytes != previous.JSONBytes) {
		b.Fatalf("coverage output changed: previous %s/%d now %s/%d", previous.CanonicalSHA, previous.JSONBytes, report.CanonicalSHA, report.JSONBytes)
	}
	if stats {
		rows, err := d.Query(`SELECT query,calls,total_exec_time FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND query NOT ILIKE '%pg_stat_statements%' ORDER BY query`)
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			var q coverageQueryMeasurement
			if err := rows.Scan(&q.Query, &q.Calls, &q.ExecMS); err != nil {
				b.Fatal(err)
			}
			report.Queries = append(report.Queries, q)
			report.QueriesPerCall += float64(q.Calls) / float64(b.N)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND tablename='task_scope' AND indexname='idx_ts_icp')`).Scan(&report.ICPIndex); err != nil {
		b.Fatal(err)
	}
	if err := d.QueryRow(`EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+coverageGraphSQL, f.taskID).Scan(&report.Plan); err != nil {
		b.Fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		b.Fatal(err)
	}
	b.Logf("retained task=%d nodes=%d edges=%d bytes=%d queries/call=%.0f p50=%.3fms p95=%.3fms index=%v sha=%s", f.taskID, report.Nodes, report.Edges, report.JSONBytes, report.QueriesPerCall, report.P50MS, report.P95MS, report.ICPIndex, report.CanonicalSHA)
}

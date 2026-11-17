# Performance: DB and graph hot paths

Scope: milestone 3 / #10. Port the focused changes from
[BaaRai/ARTEX PR #230](https://github.com/BaaRai/ARTEX/pull/230), source head
`30c787d6905785de51ef086c3a6e653ee5ec3f81`, to fork main `b166efd`.
Branch: `perf-db/coverage-graph-hotpaths`. This branch is independent of Phase C.

## Implementation

- `BuildCoverageGraph` previously queried every missing connector root domain and
  every referenced company separately. It now uses one array query for each set.
  Missing root rows still produce synthetic `r:<domain>` connectors; existing
  roots retain their real asset/company IDs. Company/root additions are sorted
  for deterministic ordering. Nodes, edges, field values and scope/tested flags
  are preserved. Query failures now propagate instead of silently fabricating
  missing context; an absent row still follows the normal fallback.
- Add `idx_ts_icp ON task_scope(value) WHERE kind='icp'`. The fork uses an existing
  idempotent, advisory-locked `schema.sql` startup bootstrap, rather than upstream's
  versioned migration runner. The index is therefore installed through that path
  on fresh and existing stores; repeated startup is tested. No migration-runner
  replacement, destructive schema change or scope-predicate rewrite is included.
- Extract the existing main query verbatim into `coverageGraphSQL`, so the
  benchmark's EXPLAIN uses the actual production SQL. Shared/current/source task
  visibility and authorization predicates remain unchanged.

## Workload and method

The upstream large-graph fixture is adapted with attribution and retained for
before/after comparisons: 20 companies, 200 roots (20 have no asset row), 800
subdomains, 1,000 services, 1,200 endpoints, 2,000 anchored facts, and 800 manual
subdomain-scope entries. Result: **3,220 nodes / 3,180 edges**; 3,000 in-scope assets,
2,000 tested assets, 180 real and 20 synthetic connector roots.

Measurements run against a dedicated disposable PostgreSQL 16 instance with
`pg_stat_statements` preloaded and I/O timing enabled, on Linux amd64 / Go 1.26.10,
Intel Xeon E5-1650 v2. One warm-up precedes ten timed `BuildCoverageGraph` calls;
p50/p95 use nearest-rank percentiles. The same task/assets/IDs remain in the
measurement database across baseline, batch-only and full changes. The benchmark
checks canonical graph SHA256 and encoded-size parity before replacing its report.

`pg_stat_statements` is reset only in explicit measurement mode on this dedicated
instance. Reports retain statement counts and `EXPLAIN (ANALYZE, BUFFERS, FORMAT
JSON)`. Setup, plan collection, graph canonicalization and report writing are
outside the measured Go benchmark interval. These are local DB-method timings,
not HTTP, browser rendering, model/task latency or a production SLA.

| Metric | Fork baseline | Batch only | Batch + ICP index |
| --- | ---: | ---: | ---: |
| SQL statements per call | 221 | 3 | 3 |
| p50 | 224.018 ms | 192.252 ms | 59.521 ms |
| p95 | 238.940 ms | 196.233 ms | 66.756 ms |
| Go ns/op | 226,501,579 | 192,371,487 | 61,183,470 |
| Go B/op | 4,756,711 | 4,628,134 | 4,628,733 |
| Go allocs/op | 51,742 | 48,646 | 48,647 |
| Main-query EXPLAIN execution | 189.482 ms | 186.784 ms | 53.293 ms |
| Main-query shared buffer hits | 70,206 | 70,206 | 35,216 |
| Encoded graph bytes | 890,716 | 890,716 | 890,716 |
| Nodes / edges | 3,220 / 3,180 | 3,220 / 3,180 | 3,220 / 3,180 |

Canonical JSON SHA256 in all three stages:
`32ffcac3f95abe818e6c20ce09bd922cb5c1adbb848074a0c59d1d89ce86f7a1`.
Canonicalization sorts nodes and edges; the batching deliberately stabilizes
connector ordering rather than promising byte-identical array order.

Baseline has 200 root and 20 company lookups plus the main query. Batching removes
218 round trips. The index addresses a separate dominant main-query cost: the old
plan probes `uq_task_scope_v2` 3,180 times (47,700 shared hits). The new plan uses
`idx_ts_icp` for the two ICP/app-ICP branches, each with 3,180 probes / 6,360 hits.
Shared reads are zero in these warm measurements. The p50 is about 3.8 times
faster on this workload; upstream's reported 526 → 24 ms is not reused as a fork
measurement. Small graphs or differently shaped scopes can have different gains.

## Reproduction

Use a disposable PostgreSQL instance with `pg_stat_statements` in
`shared_preload_libraries`. The opt-in report mode retains the fixture between
runs, so keep the same database and report path across implementation variants:

```sh
ARTEX_PG_DSN='postgres://.../db_perf_measurement?sslmode=disable' \
ARTEX_DB_PERF_REPORT=/absolute/private/path/measurement.json \
ARTEX_DB_PERF_QUERY_STATS=1 \
go test ./db -run '^$' -bench '^BenchmarkBuildCoverageGraphLarge$' \
  -benchtime=10x -count=1 -timeout=180s
```

Without report/stat flags, the benchmark cleans up its workload and does not reset
cluster statistics. There is no wall-clock performance assertion. Raw before,
batch-only and after JSON plans plus Go benchmark logs are retained in the external
`db-performance` validation workspace. The functional test runs independently of
pg_stat_statements in normal CI.

## Regression coverage and validation

- Large-graph invariants check exact counts, unique keys/edges, valid parent kinds,
  tested/in-scope signals, company labels and real/synthetic connectors.
- The retained benchmark rejects any change to complete canonical graph content
  or encoded size, using the same IDs/dataset across variants.
- Index upgrade coverage creates a fresh store, removes the index, reopens the
  existing store twice, and checks restored definition/idempotence. Root/app ICP
  case/whitespace normalization, graph equality and exclusion of a nonmatching
  asset remain intact with and without the index.
- Existing direct-source scope and coverage/API tests run with the complete suites.

Validation at implementation head `6c78339`:

- Build, vet and SDK source-provenance verification pass.
- Full repository: **1161 passing test/subtest events, zero failures, five existing skips**.
- Separate complete server race suite: **330 passing events, zero failures, two existing skips**.
- Temporary integration with Phase C PR #61 is conflict-free; joint cache,
  compaction scheduler, large-graph and index-upgrade tests pass under `-race`.
- The complete CI race package set is still running; final results go in the PR.

Logs and JSON plans are retained in the external validation workspace. GitHub
currently supplies no remote checks; these results are local. Production UI and
exploration-graph algorithms are unchanged; no unmeasured frontend or whole-task
speedup is claimed.

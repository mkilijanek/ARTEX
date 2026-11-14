# Token benchmark and verification record

## Baseline A

Source: `fc35dddee8286d584f71ba751a02c48bc60835a3` (untouched `git archive`
snapshot). Go 1.26.10 on linux/amd64, PostgreSQL 16 Alpine in a disposable
container bound to a random localhost port. No live targets or model calls.

| Run | Result |
| --- | --- |
| Unconfigured PostgreSQL, `go test -json -p 1 ./...` | 600 passing test events, 244 skips, 34 failing test events, all failures in server. This is an environment limitation, not a passing baseline. |
| Isolated PostgreSQL, same command against untouched snapshot | 926 passing test events, 5 skips, no failures; 14 packages pass and 2 have no tests. |

Counts include parent tests and subtests emitted by Go's JSON test output.
Five pre-existing skips are not acceptance evidence. No frontend changes are
included; frontend checks require Node 22 (host initially supplies Node 18).

Reproduce using Go 1.26.10 and an isolated PostgreSQL DSN:

```sh
export PATH=/path/to/go1.26.10/bin:$PATH
export GOMODCACHE=/large-volume/artex-gomod GOCACHE=/large-volume/artex-gocache
export TMPDIR=/large-volume/artex-build-tmp
export ARTEX_PG_DSN='postgres://USER:PASSWORD@127.0.0.1:PORT/isolated_baseline?sslmode=disable'
go test -json -count=1 -p 1 -timeout 3m ./... > baseline.json
go build ./...
go vet ./...
```

The package test harness suffixes database names. Never point this command at
production. Tests can delete fixture rows and create databases. Use separate
root database names for baseline, changed code and race runs.

## Comparison protocol for Phases A–F

A = fork before changes; B = dependency-reviewed upstream ports; C = ports plus
context optimizations. B/C live-run results are **not available**. Pin source SHA,
SDK version, prompt version, provider/model (planned model: Ollama `beam:cloud`),
profile configuration, prices/currency, task seed, evidence fixtures, tool catalog,
context window, concurrency and retry policy for every cohort. Do not record
keys, prompts containing credentials, or private endpoint URLs in reports.

Use identical replay inputs for deterministic bounds and failure scenarios,
then compare multiple real runs where permitted. Separate cold/warm cache runs
and adapters that do not report caching. Retain failed/cancelled attempts and
zero-usage calls; all-zero provider counters may indicate absent usage metadata.
A byte measurement is neither a provider token count nor billable usage.

Report input/output/cache read/cache creation separately. The installed SDK's
Anthropic adapter folds cache read and creation into total input; OpenAI's cache
reads are also a subset of input. Price fresh input as
`max(0, input - cache_read - cache_write)`, then price each cache bucket once.
Profile reports use current configured prices, not immutable historical tariffs.
A model round can contain multiple physical provider attempts; do not count
returned usage a second time after the recorder has already persisted it.

Compare calls/retries, request/schema/result bytes, latency, provider usage,
configured-rate cost and outcome quality together. Validate task success,
negative evidence, scope safety, full-evidence retrieval, tool permissions,
restart/resync and Ollama behavior. No savings estimate is justified until these
comparisons exist. See [token-audit.md](token-audit.md) for attribution exclusions
and [upstream-port-matrix.md](upstream-port-matrix.md) for candidate dependencies.

## Acceptance and rollback

The first implementation is additive instrumentation; no context reduction or
live execution behavior is enabled. Old usage rows must survive repeated schema
initialization. Unknown historical request sizes stay explicitly unknown.
Disable deployment/revert code if metering blocks model calls, changes tool
permissions, loses evidence, double counts attempts or changes task outcomes.
Leave additive columns in place on rollback; do not drop usage rows. Historical
pre-fix echoed-token rows cannot be reconstructed safely without provider data.

## Implementation status

Phase 0 is complete in local commits. Phase A now includes logical-call/round identity, per-tool byte buckets, explicit
hidden-call purpose labels, retry attribution and normalized cache accounting.
Reporting exclusions are listed in the audit; subsequent milestones are not
marked complete.

Each source change has its own commit with a description and validation notes.
Phase 0 is published in PR #34; Phase A is published separately against main.
PRs depend on earlier phases until those changes are merged. No deployment or
automatic merge is part of this work.

New schema fields are additive under the existing advisory-lock protocol.
Versioned upstream migrations are still pending review; any later port must
retain these fields in its new migration rather than editing previously applied
migration checksums. Historical usage reports can change with the corrected cost
formula because current prices are applied at query time; original token rows
are not rewritten.

## Inspecting a measured task

Run read-only on the selected cohort (bind `$1` to the task registry ID):

```sql
SELECT provider, model, worker, reason,
       count(*) AS provider_invocations,
       count(*) FILTER (WHERE attempt > 0) AS runtime_retries,
       sum(input_tokens) AS input_tokens,
       sum(output_tokens) AS output_tokens,
       sum(cache_read) AS cache_read_tokens,
       sum(cache_write) AS cache_write_tokens,
       sum(request_bytes) FILTER (WHERE size_known) AS measured_request_bytes,
       sum(tool_schema_bytes) FILTER (WHERE size_known) AS measured_schema_bytes,
       sum(tool_result_bytes) FILTER (WHERE size_known) AS measured_result_bytes,
       count(*) FILTER (WHERE NOT size_known) AS unmeasured_requests
FROM llm_usage
WHERE task_id = $1 AND reason <> ''
GROUP BY provider, model, worker, reason
ORDER BY provider_invocations DESC;
```

Rows with an empty reason predate this instrumentation and are excluded from
this cohort query. `runtime_retries` includes profile failover; it does not count
transport-internal attempts. NULL measured sums mean no known measurements in
that bucket. Keep raw token totals for diagnostics and do not interpret zero
provider counters as proof of zero billing.

## Validation of this implementation batch

Validated after source commit `9580c70` with Go 1.26.10 and separate disposable
PostgreSQL database roots for the plain suite and focused race run.

| Check | Result |
| --- | --- |
| `go test -json -count=1 -p 1 -timeout 3m ./...` | 950 passing test events, 5 existing skips, no failures; 14 packages pass, 2 have no tests. Baseline was 926 passing events with the same skips. |
| `go build ./...` | Pass |
| `go vet ./...` | Pass |
| `go test -race -count=1 -timeout 2m ./llmrec ./db ./server -run 'TestTelemetry|TestIntentSession|TestUsageMetrics|TestCostSeparates|TestTaskLLMMetrics'` | Pass in all three packages. This is focused race coverage, not a claim that the entire repository is race-free. |
| Baseline manifest checksums | All six still match; no SDK or frontend dependency changes. |

The first final-check attempt exhausted the host root filesystem during
compilation. Those build failures are not test results. After moving this task's
Go toolchain, module/build caches and temporary build files to the data volume,
all checks above were rerun successfully.

Existing skips: `TestGraphOverviewExpandsAssociatedCompanyScope`,
`TestCaptureApprovalLifecycle`, `TestSideUsageRecordedOnceOnConsumerCancellation`,
`TestInterceptFilterHTTP`, and `TestLiveContextReview`. New deterministic
telemetry fixtures cover cancellation separately; they do not turn those skips
into passes. No frontend checks or live model benchmarks were run.

## Phase A validation (2026-11-15)

Full isolated PostgreSQL suite: 952 passing test events, the same five existing
skips and no failures (14 packages pass, three have no tests). Focused race tests
pass in agent, llmrec, db, sidequestion and server for purpose/round attribution,
tool privacy, upgraded persistence, retry identity and echoed answer usage.
`go vet ./...` passes. No live-model savings measurement is available.

## Phase B validation (2026-11-15)

Default-mode full suite: 960 passing test events, the same five existing skips,
no failures. Enabled-mode race tests pass for trace reconstruction, byte bounds,
pages, source ownership/reopen, projection provenance, negative facts, planner
resync/restart and concurrent snapshots. Vet passes. ARTEX_BOUNDED_CONTEXT stays
zero by default; no live-model savings or quality equivalence is claimed.

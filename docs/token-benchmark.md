# Token benchmark and verification record

## Baseline A (test suite, not a token baseline)

These are test-suite event counts recorded on 2026-11-14. They are not provider token counts; no token
baseline exists yet (see "Results" below).

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

Reproduce in a `golang:1.26` container next to a disposable `postgres:16-alpine` container whose user is
`artex` (the CI database user):

```sh
export ARTEX_PG_DSN='postgres://artex:PASSWORD@PGHOST:5432/isolated_baseline?sslmode=disable'
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

## Acceptance and rollback of the usage telemetry (Phase A)

This section covers the Phase A usage telemetry only. That implementation was
additive instrumentation; it enabled no context reduction and no live execution
change. Old usage rows must survive repeated schema
initialization. Unknown historical request sizes stay explicitly unknown.
Disable deployment/revert code if metering blocks model calls, changes tool
permissions, loses evidence, double counts attempts or changes task outcomes.
Leave additive columns in place on rollback; do not drop usage rows. Historical
pre-fix echoed-token rows cannot be reconstructed safely without provider data.

## Implementation status (2026-11-26)

Phases 0–C (#34–#37), translation #39, SDK integration #47, follow-ups
#56–#58 and #60, Phase C completion #61 and DB performance #63 are merged into
`main` (the last two on 2026-11-17). The current release is `v0.7.0m`. Later
changes that alter request content: the data envelope and planner goal anchor
(Phase D.1, on by default, `ARTEX_DATA_ENVELOPE`) and the code-owned untrusted
content rules. Tool RAG observation (Phase D.3) leaves requests unchanged.
Implementation and local regression evidence do not constitute live rollout
acceptance. #1 and #2 remain open for the worker comparison/default decision;
#33 remains open for task quality, live Ollama and spend/cache acceptance.
No live token savings have been measured.

## Results

No comparable live measurement exists. Each row stays NOT RUN until a run with
pinned source SHA, SDK version, model, flags and inputs is recorded here with
its command and raw output location. The result fields follow the benchmark
harness report of Phase E (#27–#29) once it exists. Deterministic byte fixtures
are listed in [token-audit.md](token-audit.md) ("Current byte measurements");
they are bytes, not tokens.

| Cohort / comparison | Measure | Status | Issue |
| --- | --- | --- | --- |
| A: fork before changes (`fc35ddd`) | provider tokens, calls, cost, outcome | NOT RUN (only test counts exist) | #32 |
| B: dependency-reviewed upstream ports | as A | NOT RUN | #32 |
| C: ports plus context changes | as A | NOT RUN | #32 |
| `ARTEX_BOUNDED_CONTEXT` 1 vs 0 | as A, plus planner snapshot/resync counts | NOT RUN | #17–#20 |
| `ARTEX_CACHE_PREFIX` 1 vs 0 | cache read/write tokens, cost | NOT RUN | #22 |
| `ARTEX_COMPACTION_MEMO` 1 vs 0 | summary calls, memo hits, cost | NOT RUN | #21 |
| `ARTEX_WORKER_CONTEXT` 1 vs 0 | task success, duplicate findings, facts recorded, turns per intent, tokens | NOT RUN | #1, #2, #33 |
| `ARTEX_DATA_ENVELOPE` 1 vs 0 | tokens per planner round and worker launch | NOT RUN (fixture bytes only) | - |
| `ARTEX_UNTRUSTED_BOUNDARY` 1 vs 0 | tokens per tool result (summary trust rules apply in both modes) | NOT RUN | - |
| Live Ollama | protocol, usage reporting, task outcome | NOT RUN (mock protocol tests only) | #33 |
| Spend and cache | provider-reported input/output/cache read/cache write, retries, cost | NOT RUN | #33 |

## Review acceptance checklist and rollback (#33)

The acceptance criteria, the per-setting enable and rollback steps and the
rollback checklist are in [rollout-acceptance.md](rollout-acceptance.md).

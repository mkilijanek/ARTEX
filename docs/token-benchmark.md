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
export PATH=/opt/data/artex-review-validation-20261114/go/bin:$PATH
export GOMODCACHE=/opt/data/artex-review-validation-20261114/artex-gomod GOCACHE=/opt/data/artex-review-validation-20261114/artex-gocache
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

## Implementation status (review refresh 2026-11-17)

Phases 0–C (#34–#37), translation #39, SDK integration #47 and follow-ups
#56–#58 and #60 are merged into `main`. Release `v0.5.8m` contains those
follow-ups. Phase C completion #61 and DB performance #63 are separate open
PRs; their local measurements are documented in their respective reports.
Implementation and local regression evidence do not constitute live rollout
acceptance. #1 and #2 remain open for the worker comparison/default decision;
#33 remains open for task quality, live Ollama and spend/cache acceptance.
No live token savings have been measured.

## Review acceptance checklist and rollback (#33)

| Criterion | Evidence / gate | Result |
| --- | --- | --- |
| Task success | Identical authorized replay outcomes, no lost constraints/goals | Local regression tests; live comparison NOT RUN |
| Scope and evidence safety | Graph provenance, negative evidence, permission and trace reconstruction fixtures | Regression gate; no production rollout approval |
| Worker launch | Compare legacy and compact launches on the same representative authorized workload: task success, scope safety, evidence quality, duplicate findings, facts recorded and turns per intent; capture model, flags and provider usage | Live comparison NOT RUN; #1/#2 remain open, `ARTEX_WORKER_CONTEXT` defaults to 0 |
| Recovery | Fresh planner snapshot after restart, immutable stored evidence | Local regression gate |
| Existing data | Repeated additive usage migration preserves legacy rows; durable `planner_context_checkpoints` survive restart/flag rollback and resync correctly on re-enable | Local PostgreSQL regression gate |
| Ollama compatibility | `go test ./server -run TestOllama -count=1` with isolated PostgreSQL; mock HTTP server verifies wire protocol/keyless behavior | Automated protocol check; live Ollama NOT RUN |
| Prompt growth | TestRenderedAgentPromptByteInventory enforced per-role ceilings | PASS: byte budgets, not token savings |
| Spend/cache benefits | Provider-reported tokens, retries, cost and memo hit frequency on representative workloads | NOT RUN; required before default-on rollout |

Enable one flag at a time on an isolated replay deployment, restart and capture
source SHA, inputs, outcome and usage before comparing with flag-off:
`ARTEX_BOUNDED_CONTEXT=1`, then independently `ARTEX_CACHE_PREFIX=1`, then
independently `ARTEX_COMPACTION_MEMO=1`, then independently
`ARTEX_WORKER_CONTEXT=1`. Keep the other flags off for each individual comparison;
validate the intended combination separately before rollout. Never infer benefits
from synthetic reuse. All four flags default to off.

Rollback checklist:
- [ ] Set `ARTEX_BOUNDED_CONTEXT`, `ARTEX_CACHE_PREFIX`, `ARTEX_COMPACTION_MEMO`
      and `ARTEX_WORKER_CONTEXT` to `0` (or unset), restart all ARTEX processes,
      and verify the effective deployment environment. Restart clears in-memory
      memo/cache state. Durable `planner_context_checkpoints` rows remain in
      PostgreSQL and are read/resynchronized when bounded context is re-enabled.
- [ ] Retry a known replay and confirm full overview, original cache boundary and
      uncached summarization. Preserve transcripts and graph/evidence rows.
- [ ] If metering itself causes failures, deploy the previous code version; retain
      additive telemetry columns, historical rows and `planner_context_checkpoints`.
      Do not run destructive DDL.
- [ ] Confirm task state, scope constraints, evidence retrieval and Ollama mock
      compatibility after rollback. Record pass/fail and source SHA per criterion.

Unchecked rollback boxes are operator actions, not a claim they were executed.

# Token and cost audit

Baseline: `fc35dddee8286d584f71ba751a02c48bc60835a3`, reviewed 2026-11-14.
References below are baseline line numbers; use function names after edits.
No live model measurements or token savings have been established.

| Path | Observation | Risk / priority | Planned issue |
| --- | --- | --- | --- |
| `agent/planner.go:299` `renderGraphOverview` | Serializes shared overview each planner round; fresh sessions cannot consume standalone deltas safely. | High input repetition; versioned snapshot, durable base and resync needed. | #17 |
| `agent/worker.go:362` `renderWorkerGraphOverview`; `:435` initial context | Full overview repeated for each intent; assets also marshaled into system context. | Bound by intent relevance while preserving goals, scope and negative observations. | #18, #1, #2 |
| `agent/tools.go:441` `graphOverviewData`; `:484–613` fact/digest selection | Cold members are already suppressed; do not assume every recent fact is duplicated. Inherited sources add another context path. | Measure ID overlap before changing selection. | #20 |
| `agent/tools.go:1966` get_worker_trace; `:2037` step response | Five-step limit bounds count but not full content bytes. | Bounded excerpts and read-slice retrieval must retain original evidence in storage. | #19 |
| `agent/compaction.go:516` `compress`; `:96` compactor session | Atomic model call returns usage; attribution depends on decorated provider, not the discarded return variable. | Count summary spend and preserve evidence source IDs. | #15, #21 |
| `agent/provider.go:191` `compactionConfig` | SDK transcript compaction is a distinct path from cold-graph compaction. | Shared session can conceal summary cost within worker/main lane. Explicitly exclude purpose-level comparisons until tagged. | #15, #21 |
| `llmrec/llmrec.go:121` `Stream`; `:223` `Complete` | Existing ledger runs with debug recording off. Stream input/cache deltas were summed even when SDK adapters echo totals. | Correctness before optimization; compare SDK `Usage.MergeEcho` contract. | #14, #16 |
| `db/llm_usage.go:346` `applyCostEur` | Fresh input subtracts cache reads but originally not cache creation, although SDK Anthropic input includes both. | Cache-write tokens charged at both fresh and cache-write prices. | #16 |
| `server/task_llm.go:319` `completeTaskLLM`; `:389` `streamTaskLLM` | Runtime retries and profile switches invoke decorated providers repeatedly. | Count physical invocations separately from logical rounds; do not add return usage to ledger totals. | #14, #15 |
| `server/server.go:465`, `:600` recorder wrapping | Both environment and persisted-profile provider paths share recorder. | Label actual configured adapter; do not retain endpoint/key in metrics. Pool member attempts are recorded; SDK-internal HTTP retries remain below the recorder. | #14, #15 |
| `server/intercept.go:51`, `:85` judge lane | Judge uses explicit worker override and decorated provider. | Retain judgement and safety context; no prompt trimming here. | #15 |
| `sidequestion/context.go:111` summary completion; `sidequestion/service.go:25`, `:53` answers | Both invoke bound provider; summary and answer currently share lane. | Purpose-level breakdown is unavailable; zero reported tokens are not proof of zero spend. | #15 |
| `agent/mainagent.go`, `agent/planner.go`, `agent/worker.go` prompt assembly | Shared instructions and dynamic intent tails require priority-aware review. | Stable prefix must preserve untrusted boundaries and current intent through compaction. | #22, #3 |

## Implemented instrumentation contract

The Phase A foundation extends `llm_usage` with provider, intent ID, runtime
attempt, completion reason and request byte counts. It keeps existing task,
exploration, model, profile, agent lane, provider tokens, latency and status.
`COUNT(*)` measures physical provider-decorator invocations, not logical rounds
or transport-internal HTTP attempts. Runtime attempt numbers start at zero per
logical request and continue across profile failover.

Byte fields use the provider-neutral SDK representation, not HTTP wire payloads:
`system_bytes` counts raw UTF-8 in system segments; `message_bytes` and
`tool_schema_bytes` count their JSON arrays; `tool_result_bytes` counts tool-result
blocks and is a subset of message bytes. `request_bytes` includes neutral request
fields and JSON framing. These fields overlap and must not be summed together.
`size_known=false` means serialization was unavailable or the historical row
predates instrumentation. JSON is transiently serialized for counting; no new
prompt, answer, tool arguments, URL, credential or error-text field is stored.
Debug body recording remains a separate existing operator-controlled feature.

Observer callbacks receive value copies synchronously after each invocation and
must synchronize if shared by concurrent calls. Consumer stops, cancellation,
provider errors and normal completion are distinct reasons. The observer is a
measurement hook, not a second usage ledger to add to database counts.

## Phase A completion and reporting boundaries

Logical model calls carry a random `call_id`, a session-local round number and a
code-selected purpose. Physical runtime retries share that identity and have
separate attempt numbers. Harness turns are `agent`; SDK transcript compaction
uses a separately wrapped provider tagged `transcript_summary`. Cold-graph
compaction is `graph_summary`, side-question answers/summaries are `side_answer`
and `side_summary`, and approval judgement is `judge`. Findings/review/chat and
goal decomposition use the same instrumented harness path. Purpose never comes
from prompt inspection. Summaries without a harness round use round zero.

Per-tool byte buckets use full catalog tool-name keys and contain only schema and
result sizes. They link tool context to the owning model call; provider token
usage and cost belong to the whole call and are not spuriously divided among
tools. A result whose originating tool use is outside retained history goes in
the hash-of-empty-name bucket. Byte buckets exclude JSON array framing.

Phase A acceptance includes the following explicit exclusions and limits:

- Transport/pool retries beneath a decorated provider are excluded from physical
  attempt reports; SDK summarizer retries are independently metered calls, since
  the SDK owns that loop. These are not reconstructed from prompt similarity.
- Third-party MCP services and SDK DeepSeek web-search inference bypass the
  configured provider boundary. Their external billing is excluded from ARTEX
  token/cost reports. Bash subprocess model clients are likewise external.
- Uninstrumented/direct provider callers have purpose `unclassified`, empty call
  identity and round zero; purpose/round reports must exclude that bucket and
  report its count. They still contribute to total provider usage.
- All-zero SDK usage is ambiguous, not proof of free inference. Provider metadata
  lacks an availability flag. Keep those rows and report unknown billing.
- Prices are configured current EUR-per-million rates joined by profile name,
  not historical invoices. Unknown prices remain NULL. Inclusive SDK input counts
  are priced once across fresh/cache-read/cache-write buckets. Raw gateway usage
  with different semantics needs its own adapter, not another subtraction here.
- Historical echoed-token rows are not rewritten; compare post-fix cohorts.

Core dimensions and hidden configured-provider paths are implemented and tested.
Reports must retain these exclusions rather than claiming universal billing or
per-tool token precision. Security and context changes remain later phases.

## Review correction: dimensions and call boundaries (#14)

`GET /api/tokens/dimensions?task=ID&purpose=agent&after=0&limit=100` returns
physical-attempt rows with call ID, purpose, round, attempt and per-tool bytes;
follow `next_after` until rows is empty. Limit is bounded to 500. Call and purpose
indexes support inspection. This is authenticated by the normal API middleware.
Tool keys now retain configured catalog names (not descriptions/arguments/results);
unknown model-invented names use `_unknown`. Historical hashed keys remain opaque.

Attempt ordinals are shared across recorded providers within a logical call,
including pool-internal failover and outer router retries. SDK-internal HTTP
retries below the recorder remain excluded. `round` means the model invocation
ordinal within one session instance, **not** a durable conversation turn number:
SDK re-issues get a fresh call ID and ordinal, and restoring a session resets the
ordinal. Group by call ID for physical retries; never group solely by round across
resumes. Unexpected stream EOF without a stop event is an error, not completion.

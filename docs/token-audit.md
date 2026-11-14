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
| `server/server.go:465`, `:600` recorder wrapping | Both environment and persisted-profile provider paths share recorder. | Label actual configured adapter; do not retain endpoint/key in metrics. Pool-internal retries require separate audit. | #14, #15 |
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

## Remaining instrumentation gaps

- Logical round IDs and tool-call IDs are not yet propagated through all harness
  paths. Intent and agent-lane attribution is available, not a tool cost split.
- SDK transcript compaction and side-question summarization need explicit purpose
  tags; current rows contribute to lane totals but cannot support purpose totals.
- Transport/pool retries below the recorder are not individual ledger rows.
- Provider-supplied all-zero usage remains ambiguous because the SDK usage type
  lacks an availability flag. Report it as unknown billing, never free inference.
- Profiles are joined by mutable name and current price. Costs are estimates at
  current configured rates, not immutable invoices or historical tariff snapshots.
- Token echo correction affects new rows only; historical doubled counters cannot
  be repaired safely without provider evidence. Keep pre/post cohorts separate.

Phase A issues remain open until these acceptance gaps are resolved. The next
implementation is explicit hidden-path attribution, followed by security
prerequisites and bounded trace retrieval. See the baseline for the full order.

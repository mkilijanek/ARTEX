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


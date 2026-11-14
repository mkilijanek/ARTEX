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

## Phase A completion and reporting boundaries

Logical model calls carry a random `call_id`, a session-local round number and a
code-selected purpose. Physical runtime retries share that identity and have
separate attempt numbers. Harness turns are `agent`; SDK transcript compaction
uses a separately wrapped provider tagged `transcript_summary`. Cold-graph
compaction is `graph_summary`, side-question answers/summaries are `side_answer`
and `side_summary`, and approval judgement is `judge`. Findings/review/chat and
goal decomposition use the same instrumented harness path. Purpose never comes
from prompt inspection. Summaries without a harness round use round zero.

Per-tool byte buckets use full SHA-256 tool-name keys and contain only schema and
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

## Phase B context contract

`ARTEX_BOUNDED_CONTEXT=1` opts into projected launch context and trace slices.
Default remains zero pending security/rollout validation. Mandatory system
constraints and full intent text are unchanged. Projection never changes stored
graphs, evidence or permissions; omitted evidence cannot establish a negative.

Planner packets carry a 16 KiB projected base plus cumulative top-level replace
and remove sets (40 KiB packet ceiling). Every request includes its base, so
fresh planner sessions can reconstruct current state without remembering prior
turns. Resync occurs every eight rounds or on size pressure. Restart/eviction
starts a new epoch. Projection metadata requires full scope retrieval before
acting; truncated scope text never authorizes targets.

Worker launches select dependency/own-intent observations, preserve IDs,
confidence and source identity, and omit unrelated frontier/digest material.
Asset launch records contain at most eight compact targets. Oversized target
strings are withheld with a detail requirement, never used as shortened URLs.
`list_assets` and `node_detail` retrieve full records on demand. Exact duplicate
records are suppressed; equal summaries with different IDs/provenance remain.
Existing hot/cold covered-member suppression and revival behavior are retained.

Trace detail is capped at five steps, 4096 encoded detail bytes per step, with
byte offsets, next offsets, total/returned bytes and truncation flags. Metadata
is bounded too; responses stay below 64 KiB for supported inputs. Summary/search
pages cap at 20 entries and use next_before_step_id. UTF-8 slices reconstruct
original evidence; database contents remain complete. Inherited paging checks
direct-source ownership and terminal state in SQL, and excludes thinking/usage.
Custom stored tool-schema overrides may need optional paging fields added by the
operator; default code definitions include them without changing custom policy.

## Phase C compaction and prefix audit

Cold-graph major compaction already reuses unchanged block signatures; it is not
an unconditional repeated-summary path. SDK transcript compaction and side
summaries are different paths, now separately attributed by Phase A. Original
node payloads/evidence and transcript history remain authoritative.

Optional `ARTEX_COMPACTION_MEMO=1` adds a 64-entry, per-Compactor successful-summary
memo keyed by configured model, full request and full member/anchor source
records. Source changes invalidate it. Errors, empty results and cancellation are
not cached. It resets on restart and never replaces the database source records.
`SummaryStats` separates inference calls from memo hits; provider token/cost rows
are written only for real calls. Routing-model identity changes are not exposed
by the SDK: turn this memo off when comparing rotated model configurations.

Optional `ARTEX_CACHE_PREFIX=1` splits the rendered prompt only at an existing
paragraph before a timestamp, per-run directory or intent tail. Joining segments
recreates the original text exactly. The provider cache boundary covers only the
stable prefix. Custom templates without a safe split keep their existing shape.
Shared artifact-rule literals are factored once in source; their emitted text is
unchanged. The audit found no evidence justifying deletion of safety instructions
merely because similar rules appear in different agent roles.

Default-template byte inventory (Go test fixture, not token or billing data):
worker 9437 total / 9062 prefix; planner 12259 / 11937; mainagent 4051 / 3729;
chat 470 / 211. Actual prompts vary with stored templates, tools and runtime
configuration. These byte counts do not establish cache hits or token savings.
Both options default off and are forwarded through Compose.

# Token and cost audit

Baseline: `fc35dddee8286d584f71ba751a02c48bc60835a3`, reviewed 2026-11-14.
References below are baseline line numbers; use function names after edits.
No live model measurements or token savings have been established.

| Path | Observation | Risk | Priority | Planned issue |
| --- | --- | --- | --- | --- |
| `agent/planner.go:299` `renderGraphOverview` | Serializes shared overview each planner round; fresh sessions cannot consume standalone deltas safely. | High input repetition; versioned snapshot, durable base and resync needed. | P1 | #17 |
| `agent/worker.go:362` `renderWorkerGraphOverview`; `:435` initial context | Full overview repeated for each intent; assets also marshaled into system context. | Bound by intent relevance while preserving goals, scope and negative observations. | P1 | #18, #1, #2 |
| `agent/tools.go:441` `graphOverviewData`; `:484–613` fact/digest selection | Cold members are already suppressed; do not assume every recent fact is duplicated. Inherited sources add another context path. | Measure ID overlap before changing selection. | P1 | #20 |
| `agent/tools.go:1966` get_worker_trace; `:2037` step response | Five-step limit bounds count but not full content bytes. | Bounded excerpts and read-slice retrieval must retain original evidence in storage. | P1 | #19 |
| `agent/compaction.go:516` `compress`; `:96` compactor session | Atomic model call returns usage; attribution depends on decorated provider, not the discarded return variable. | Count summary spend and preserve evidence source IDs. | P1 | #15, #21 |
| `agent/provider.go:191` `compactionConfig` | SDK transcript compaction is a distinct path from cold-graph compaction. | Shared session can conceal summary cost within worker/main lane. Explicitly exclude purpose-level comparisons until tagged. | P1 | #15, #21 |
| `llmrec/llmrec.go:121` `Stream`; `:223` `Complete` | Existing ledger runs with debug recording off. Stream input/cache deltas were summed even when SDK adapters echo totals. | Correctness before optimization; compare SDK `Usage.MergeEcho` contract. | P1 | #14, #16 |
| `db/llm_usage.go:346` `applyCostEur` | Fresh input subtracts cache reads but originally not cache creation, although SDK Anthropic input includes both. | Cache-write tokens charged at both fresh and cache-write prices. | P1 | #16 |
| `server/task_llm.go:319` `completeTaskLLM`; `:389` `streamTaskLLM` | Runtime retries and profile switches invoke decorated providers repeatedly. | Count physical invocations separately from logical rounds; do not add return usage to ledger totals. | P1 | #14, #15 |
| `server/server.go:465`, `:600` recorder wrapping | Both environment and persisted-profile provider paths share recorder. | Label actual configured adapter; do not retain endpoint/key in metrics. Pool-internal retries require separate audit. | P1 | #14, #15 |
| `server/intercept.go:51`, `:85` judge lane | Judge uses explicit worker override and decorated provider. | Retain judgement and safety context; no prompt trimming here. | P1 | #15 |
| `sidequestion/context.go:111` summary completion; `sidequestion/service.go:25`, `:53` answers | Both invoke bound provider; summary and answer currently share lane. | Purpose-level breakdown is unavailable; zero reported tokens are not proof of zero spend. | P1 | #15 |
| `agent/mainagent.go`, `agent/planner.go`, `agent/worker.go` prompt assembly | Shared instructions and dynamic intent tails require priority-aware review. | Stable prefix must preserve untrusted boundaries and current intent through compaction. | P1 | #22, #3 |

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

Per-tool byte buckets use configured catalog names and contain only schema and
result sizes. Unknown originating names use `_unknown`; historical SHA-256 keys
remain opaque. They link tool context to its owning call; provider usage/cost
belong to the whole call and are not divided among tools. JSON array framing is
excluded. Tool names are identifiers, not prompt or result content.

Phase A acceptance includes the following explicit exclusions and limits:

- SDK transport retries beneath a decorated provider are excluded from physical
  attempt reports; recorded pool members share physical-attempt ordinals; SDK summarizer retries are independently metered calls, since
  the SDK owns that loop. These are not reconstructed from prompt similarity.
- Third-party MCP services and SDK DeepSeek web-search inference bypass the
  configured provider boundary. Their external billing is excluded from ARTEX
  token/cost reports. Bash subprocess model clients are likewise external.
- Uninstrumented/direct provider callers have purpose `unclassified`, a fresh fallback call
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

`ARTEX_BOUNDED_CONTEXT=1` opts into planner context, transcript references and
trace slices; its default remains zero. Worker launch projection now has its own
default-on `ARTEX_WORKER_CONTEXT` switch (see milestone completion below). Mandatory system
constraints and full intent text are unchanged. Projection never changes stored
graphs, evidence or permissions; omitted evidence cannot establish a negative.

Planner context uses a durable PostgreSQL checkpoint: one immutable base plus
versioned replace/remove deltas, replayed and materialized on the server. Fresh
SDK sessions receive only the current projected snapshot. Restart
retains the epoch/version; corrupt checkpoints rebuild from the source graph in a
new epoch. Resync occurs every eight updates or before replay exceeds 40 KiB.
Optional projection targets 16 KiB; mandatory instructions/signals remain lossless
above the soft targets with explicit `mandatory_budget_exceeded`. See
[Phase B completion](phase-b-completion.md) for protocol, tests and measurements.

Worker launches select dependency/own-intent observations, preserve IDs,
confidence and source identity, and omit unrelated frontier/digest material.
Asset launch records contain at most eight compact targets. Oversized target
strings are withheld with a detail requirement, never used as shortened URLs.
`list_assets` and `node_detail` retrieve full records on demand. Exact duplicate
records are suppressed; equal summaries with different IDs/provenance remain.
Existing hot/cold covered-member suppression and revival behavior are retained.

Trace detail is capped at five steps, 4096 encoded detail bytes per step, with
byte offsets, next offsets, total/returned bytes and truncation flags. Metadata
is bounded too; detail batches are reduced to a 16 KiB encoded budget including notices. Summary/search
pages cap at 20 entries and use next_before_step_id. UTF-8 slices reconstruct
original evidence; database contents remain complete. Inherited paging checks
direct-source ownership and terminal state in SQL, and excludes thinking/usage.
Custom stored tool-schema overrides may need optional paging fields added by the
operator; default code definitions include them without changing custom policy.

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

## Phase B review corrections (#1, #2, #17–20)

At this review stage the feature remained opt-in; the later worker-only rollout
is documented below. Goals, hints, task identity, coverage, stagnation,
credentials and worker identity/dependency evidence are preserved without truncation. Optional
fields carry per-path omission metadata. The 16 KiB target is a **soft** budget:
mandatory data may exceed it, explicitly marked `mandatory_budget_exceeded`;
correctness takes precedence over savings. Do not claim a hard bound on all prompts.

Planner checkpoint recovery replays a persisted base plus ordered deltas; the
model receives a materialized snapshot, not a delta chain. A checkpoint error
falls back to a one-off snapshot unless cancelled. Logs distinguish source/replay/update bytes,
explicit UTF-8/4 token estimates and provider-reported input/output/cache usage.
They do not assert model-cost savings. Dependency filtering precedes optional
trimming and uses producing intent IDs; direct fact/finding parents are retained
as well, because graph edges can point from those node kinds to an intent.

Asset omissions are explicit at field level. Trace pagination parameters exist
only with the flag enabled, unavailable step IDs are reported, and detail batches
have an aggregate 16 KiB encoded budget with continuation offsets.

#17/#20 are completed by the follow-up protocol in `agent/planner_context.go`
and `agent/context_dedup.go`: durable replay plus request-local, versioned field
references across registered prefetch and known graph read tools/history. Source,
node ID, kind, state, confidence and exact content determine identity. Full
transcripts/evidence stay unchanged; after compaction, missing source values become
full data again. First occurrences stay unchanged; later references use natural
keys and locations. Fixed rules have an upfront cost; field substitutions are
applied only when smaller, and reserved evidence syntax is quoted literally.
Representative deterministic
measurements are in [phase-b-completion.md](phase-b-completion.md). Live model
quality/cost acceptance remains Phase E/F work. The subsequent milestone #1
completion below makes only worker launch projection default-on; planner replay,
references and trace paging remain opt-in.

## Phase C review: prompt inventory and memo acceptance

| Shared block | Previous copies | Current source | Behavior |
| --- | --- | --- | --- |
| Untrusted-content defense | mainAgentDefaultTmpl, workerDefaultTmpl, DefaultAssistantPrompt, pentestDefaultTmpl | `untrustedContentInstructions` in prompt_shared.go | Identical bytes at the same instruction priority and position |
| Artifact storage prefix | worker artifactSpec and chatWorkDirSpec | shared artifact prefix | Dynamic directory remains in the tail |
| Role instructions and finding gates | Role-specific templates | Kept separate | Different role permissions and workflows must not be merged by text similarity |

Rendered fixture byte counts / enforced ceilings: worker 9437 / 10400, planner
12259 / 13500, mainagent 4051 / 4500, chat 470 / 600. These are UTF-8 byte budgets,
not billed tokens. Prompt changes above those ceilings require an explicit review.
Real planner/main prompts are tested across changed goals/directories. Existing
cache boundaries covering deferred guidance are preserved; a single-segment system
can use the stable prefix. No live cache-hit improvement is claimed.

Memoization rejects only explicit max_tokens/length truncation, retains flag-off behavior,
uses a 64-entry LRU, and coalesces identical concurrent calls with cancellable
waiters. Deterministic fixture: two unchanged requests cause one inference and one
memo hit; changed source forces another inference; failed/truncated summaries are
not cached. This measures correctness and synthetic reuse, not production savings.
#21 remains open for representative frequency/cost measurement; keep the flag off
until that evidence exists.

## Review corrections, 2026-11-16

Connection/model-listing probes are administrative validation traffic outside the
recorded agent provider path and are excluded from task spend, as are dedicated
search-provider calls. All non-harness inference has round zero; each recorded
unclassified invocation receives a fallback UUID. Purpose fixtures assert the
actual judge, graph-summary, side-answer and side-summary call sites.

Profile fresh-input pricing sums `max(0,input-cache_read-cache_write)` **per call**,
then applies current rates. New archives retain this subtotal. Older archives
without it have unknown cost rather than a reconstructed estimate. A zero/missing
cache tariff with nonzero usage also leaves cost unknown (`cost_unknown=true`):
the legacy tariff schema cannot distinguish an intentional free tariff from an
unset one. Token totals remain available.

Related-task detail is optional and lowest priority; current evidence, running and
open intents precede it. Worker coverage is removed before budgeting, matching its
rendered view. Selection reports unrelated counts. Dependencies outside the source
overview windows (including cold digests and transitive ancestors not represented
there) still require explicit node/detail retrieval; this is not a claim of a full
transitive graph closure. Worker mandatory data may exceed the soft size target.

The built-in planner goal moves behind static rules only with
`ARTEX_CACHE_PREFIX=1`; the disabled path retains its original goal-first layout.
Stored custom planner templates keep their own layout in either mode. If a custom
template places the goal near the top, its stable prefix can remain short; no
cache improvement is promised for that template without measurement.


## Token Efficiency milestone completion, 2026-11-17

The remaining review gaps in #1, #2 and #3 are addressed independently of the
experimental planner/transcript features. `ARTEX_WORKER_CONTEXT` defaults to `1`
in code, `.env.example` and Compose; `0`, `false` or `off` restores the legacy full
worker overview and raw asset records. `ARTEX_BOUNDED_CONTEXT=0` continues to keep
planner packets, reference substitution and trace paging off. Stored custom
prompt bodies, operator constraints, goals, hints and credentials are unchanged.

### Default launch data (#1)

`workerLaunchContext` selects observations produced by the assigned intent or its
parents represented in the source overview, plus up to three separately labelled
awareness observations per kind. Source windows are newest-first. Each selected
observation list has a 20-entry ceiling and explicit omitted counts. Summary
excerpts use at most 384 encoded JSON bytes, preserving UTF-8; truncated entries
keep IDs/provenance/state/confidence and require `node_detail(id)` before treating
the excerpt as complete evidence. Related tasks and other optional fields share
the existing 16 KiB soft projection budget. This is launch context, not a full
transitive dependency closure or a new persistent summary.

Full operator instructions and mandatory assignment metadata remain lossless. An
oversize mandatory section reports `mandatory_budget_exceeded`; arbitrary operator
text is deliberately not subject to a hard total prompt cap. Original evidence
and stored graph records are never shortened. Omitted observations are not
negative evidence. The rendered large-graph regression fixture enforces a 20 KiB
launch-message ceiling and measures **7,119,318 source bytes → 9,322 rendered
bytes**. This synthetic case stresses 2,000 long observations; production source
windows may already be smaller. Real `Worker.Execute` tests exercise default and
rollback paths with PostgreSQL, and call the actual `node_detail` tool to verify
full evidence retrieval.

### Default target assets (#2)

The system tail now uses `projectAssets` by default: at most eight records and
**8 KiB aggregate encoded JSON**, including omission metadata. IDs, type, exact
URL/domain/IP, port and status are retained when they fit. A URL/domain/IP over
1,024 bytes is omitted with a field-specific marker and `requires_asset_detail`;
a shortened address is never offered as an executable target. The complete intent
`asset_ids` remains pinned in the system prompt. All raw metadata is fetched using
`list_assets(ids)` when needed; it is not interpreted as absent evidence.

A 100-record escaped-string/large-metadata fixture measures **23,105,893 raw bytes
→ 7,375 projected bytes** (seven records fit). A real Worker SDK request with one
long page title measures about **53.2 kB legacy → 11.2 kB default system**. Tests
also verify rollback, unchanged stored records, exact short targets and successful
full retrieval through the actual `list_assets` tool.

### Prompt inventory and actual reductions (#3)

| Block / location | Rendering and decision |
| --- | --- |
| `untrustedContentInstructions`, prompt_shared.go | One shared source, included once per applicable role. Security policy is retained; source factoring alone does not save model bytes. |
| Artifact rules, worker.go (`artifactSpec`, `workerArtifactSpec`), chat directory rules | Shared prefix; one role-specific directory tail. Each independent session still needs its storage instruction. |
| Worker traffic note, `workerTrafficBlock` | One code-owned block, included only while recording is enabled; egress-only proxy mode does not advertise absent tools. |
| Finding guidance, finding_workflow.go / deferred.go | Derived once from the permitted tools. Distinct ID/scope rules retained. |
| Deferred-tool catalogue, deferred.go | One final segment for globally available tools; existing cache boundary retained. |
| SDK trust and output framing, norma_trust.go | Separate trust contracts for runtime/summary data, retained. Provider-facing tests check each block occurs once. |
| Settlement guidance, wrapup.go / worker.go | Budget-exhaustion guidance is supplied through SDK settlement, not copied into every ordinary launch. Role-specific writeback rules retained. |
| Assigned intent in system and launch overview | The default launch omits its duplicate summary, marks `summary_in_system_intent`, and keeps lineage. Full payload appears once in the system tail and survives compaction. |
| Overview introduction, renderWorkerGraphOverview | Condensed to read-only awareness, assigned-duty boundary, recording useful related clues, and evidence-retrieval/omission rules: **555 fewer rendered bytes per launch**. |

`TestRenderedAgentPromptByteInventory` keeps the existing per-role template
ceilings (worker 10,400; planner 13,500; main 4,500; chat 600 bytes).
`TestProviderFacingPromptBudgetsWithTrafficAndDeferredTools` additionally captures
actual Norma SDK provider requests with cache segmentation on/off, traffic
recording on/off, deferred guidance and SDK trust framing. Enforced system
ceilings: worker 14,000; planner 16,000; main 7,000; chat 3,500 bytes. Representative
measurements are 11,387/12,077 worker (recording off/on), 14,212 planner, 6,004 main,
2,420 chat. An actual worker execution checks the full assigned summary appears
only once across the system and launch input, without duplicating safety rules.

These are deterministic UTF-8 byte measurements, not provider tokenizer counts,
live model-quality acceptance, billed-token savings or cache-hit claims. Phase E/F
representative workloads remain separate from this milestone.


Validation on Go 1.26.10 and disposable PostgreSQL 16 databases:

- `go test -json -p 1 ./... -count=1 -timeout=600s`: 1,143 passing test/subtest
  events, zero failures, five existing skips (four tracked upstream/timing cases
  plus the opt-in live-model test).
- CI race package set (`agent`, `cmd/artex`, `config`, `db`, `evidence`, `guard`,
  `intercept`, `llmpool`, `llmrec`, `mcphttp`, `notify`, `sidequestion`, `traffic`):
  813 passing test/subtest events, zero failures, three existing skips.
- `go build ./...`, `go vet ./...` and `python3 scripts/verify-norma-source.py` pass.
- No new skips; DB-backed default/rollback and full read-tool tests ran against
  PostgreSQL. Local JSON logs are retained in the external validation workspace.

GitHub Actions currently rejects dispatch with “Actions has been disabled for
this repository” and CodeQL remains queued. The checks above are local validation,
not a remote CI/CodeQL result. SDK, UI and release workflow source are unchanged.

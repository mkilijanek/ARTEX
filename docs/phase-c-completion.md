# Phase C — compaction audit and cache-friendly prompt delivery

Scope: #21 and #22, on `token-opt/phase-c-completion`, based on released main
`b166efd` (0.5.8m). Experimental flags remain off. This delivers the implementation,
repeatable scheduler audit and physical provider-wire verification; production
quality, actual cache hits and billed-cost acceptance remain Phase E/F (#33).
No model credentials or representative production archive were supplied in this
session; no live inference or deployment comparison was run.

## #21 — frequency, overlap, usage and evidence

`Compactor.AuditStats()` and `[compaction_audit]` logs expose **numeric lifetime
counters for one Compactor**, not source text, credentials, inferred invoices or
an additional billing ledger:

- maintenance rounds/errors, minor/major attempts and cooldown/running suppression;
- candidate block/member visits and source signature reuse (before inference);
- provider `Complete` invocations, successful memo/shared returns and joined flights;
- request/response UTF-8 bytes, elapsed inference milliseconds, reported token/cache
  usage, provider errors, rejected/truncated responses and zero-usage responses.

A provider invocation can include several backend retries/failovers; it is **not**
a physical HTTP-attempt count. Use the existing `llm_usage` ledger for that, with
`purpose=graph_summary`. SDK transcript summarization is a separate path tagged
`transcript_summary` (`provider.go`, `model_metrics.go`, `norma_trust.go`); graph
counters do not claim to cover SDK/NOA compaction. Missing/zero response usage does
not establish free work. Apply configured current tariffs through the existing
usage/cost reporting; unknown prices stay unknown, and never charge memo returns
as extra model calls.

Replay through actual `OnPlannerRound`, maintenance, default thresholds N=20/M=8,
background fold, commit-time liveness and signature reconciliation:

- eight settled directions, twelve source observations each (104 source nodes);
- seven initial rounds make the graph eligible; initial minor produces eight
  digests and triggers a major reconciliation;
- one immediate round demonstrates cooldown suppression; subsequent rounds advance
  only the test's cooldown clock to exercise five unchanged major passes;
- one source payload/content version changes, then a final unchanged pass follows;
- full original source payloads remain retrievable and unchanged except the
  explicitly edited test observation. Summaries never replace source records.

| Counter | Memo off | Memo on |
| --- | ---: | ---: |
| Maintenance rounds | 15 | 15 |
| Cooldown suppression | 1 | 1 |
| Minor / major runs | 1 / 8 | 1 / 8 |
| Candidate block visits | 72 | 72 |
| Signature reuses before inference | 63 | 63 |
| Source member visits / reused visits | 936 / 819 | 936 / 819 |
| Provider invocations | 9 | 9 |
| Memo returns | 0 | 0 |

**Finding:** unchanged signatures already avoid repeated work in this workload.
The content memo adds no hits here; do not enable it on an assumption of universal
savings. Existing repeated-request/LRU/shared-flight fixtures exercise the cases
where it can help, plus invalidation, cancellation, panic and incomplete output.
New audit tests count rejected/truncated/zero-usage replies without caching them.
Flag-off completion behavior stays unchanged.

The deterministic provider reports 2,000 input and 300 output tokens per call:
18,000 / 2,700 in each replay. At **example fixture rates only** of EUR 2/M input
and EUR 8/M output, that is EUR 0.0576 in each mode. These are controlled protocol
usage/tariffs, **not tokenization, a live model bill, production hit rate or proof
of semantic summary quality**. Latency with this fixture is not model latency.

Reproduce (requires disposable PostgreSQL, matching the normal Go tests):

```sh
ARTEX_PG_DSN='postgres://.../phase_c_replay?sslmode=disable' \
ARTEX_PHASE_C_REPORT=/absolute/private/path/replay.json \
go test ./agent -run '^TestCompactionRepresentativeReplay$' -count=1 -v
```

The report contains the topology, flag, counters and explicit fixture-cost label.
For production, collect log snapshots and `graph_summary`/`transcript_summary`
usage on the same authorized workload/model in both modes. Compare invocation
frequency, signature versus memo/shared reuse, canonical usage, configured prices,
source/summary overlap and evidence quality. Keep raw archives unchanged.

## #22 — stable prefix reaches the actual provider

The previous host split preserved text and deferred-tool cache coverage, but the
single final cache endpoint still included volatile date/goal data whenever
MCP guidance existed. A new **optional** Norma hint adds an earlier system-prefix
endpoint while retaining that final endpoint. No instruction is moved or removed.

`ARTEX_CACHE_PREFIX=1` activates the extra hint only after a safe host-side split.
Default/flag-off serialization remains unchanged. Anthropic endpoints are sorted,
deduplicated, bounded to the existing static region and limited to four including
the legacy endpoint. Invalid hints do not enable caching without a boundary.
Non-Anthropic adapters ignore the hint. Original role/custom-template ordering,
artifact rules, trust framing, current intent and deferred guidance stay intact.

Physical HTTP test through ARTEX Session → Norma harness → Anthropic adapter:

| Planner configuration | Cached system blocks | First cached block bytes |
| --- | ---: | ---: |
| Flag off, goal one | 1 | 13,289 (includes volatile goal/date) |
| Flag off, different goal | 1 | 13,295 (different prefix) |
| Flag on, either goal | 2 | 11,915 (byte-identical across goals) |

The second endpoint still covers deferred guidance. Full joined system sizes are
14,208 / 14,214 bytes, unchanged by adding the endpoint. Existing real-role byte
budgets and goal/directory tests cover main/planner/worker/chat; a custom template
without a safe boundary remains unsplit. Small prefixes (e.g. the chat fixture)
need provider-specific eligibility measurement; a marker does not promise a hit.

The local SDK snapshot is `v0.4.8+artex.2`; original upstream hashes are unchanged,
patched production/test hashes are recorded in `SOURCE.json`. Original one-boundary
SDK tests still pass alongside the endpoint-bound/serialization regressions.

Reference: [Anthropic explicit cache breakpoints](https://platform.claude.com/docs/en/build-with-claude/prompt-caching).
The documentation permits multiple endpoints and makes cache eligibility model
dependent. Local wire tests validate requested boundaries, not provider cache-hit
rate, billing reduction or live task quality. Those rollout gates remain #33.

## Validation

Focused scheduler/memo/failure and physical HTTP/SDK endpoint tests pass. Full
repository, nested SDK and race results are recorded in the delivery PR after the
complete checks finish. No new skipped test or weakened security check is used
to claim coverage.

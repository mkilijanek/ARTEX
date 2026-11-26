# Rollout acceptance and rollback

This page is the acceptance checklist for the settings that change what ARTEX sends to a model (#33). It also
holds the gate for the default of the compact worker launch (#1, #2). Measurements are recorded in
[token-benchmark.md](token-benchmark.md) ("Results"); the token paths are listed in [token-audit.md](token-audit.md).

Rules:
- A live row stays **NOT RUN** until a comparison with pinned source SHA, SDK version, model, flags and inputs is
  recorded in the benchmark results. Mock protocol tests and byte fixtures validate the implementation only.
- No setting below changes its default without a recorded comparison and an owner decision.
- Checked boxes record operator actions that were carried out. An unchecked box is not a claim either way.

## Acceptance criteria

Results as of `v0.7.0m` (`b462710`).

| Criterion | Evidence / gate | Result |
| --- | --- | --- |
| Task success | Identical authorized replay outcomes; no lost constraints or goals | Local regression tests; live comparison NOT RUN |
| Evidence quality | Full records stay retrievable (`node_detail`, `list_assets`, trace paging); provenance and confidence are kept; omitted data is never treated as negative evidence | Local regression tests; live comparison NOT RUN |
| Scope safety | Scope constraints, destination checks and fact provenance are the same in every flag combination | Regression gate; no production rollout approval |
| Tool and schema permission model | Each role's tool set and schemas are unchanged by the flags (`TestWebFetchToolSetPerRole`); Tool RAG observation never filters the tool list | Regression gate |
| Ollama support | `go test ./server -v -run TestOllama -count=1` with an isolated PostgreSQL (`ARTEX_PG_DSN`); a mock HTTP server checks the wire protocol and keyless profiles | PASS at `b462710` (6 tests, 0 skipped, mock only); live Ollama NOT RUN |
| Recovery | Planner snapshot is rebuilt after a restart; stored evidence is immutable | Local regression gate |
| Data state | Additive migrations keep legacy rows; `planner_context_checkpoints` and `tool_rag_observations` rows survive a flag rollback | Local PostgreSQL regression gate |
| Worker launch (#1, #2) | Same representative authorized workload in legacy and compact mode: task success, scope safety, evidence quality, duplicate findings, facts recorded, turns per intent; record model, flags and provider usage | NOT RUN; `ARTEX_WORKER_CONTEXT` defaults to 0 |
| Prompt growth | `TestRenderedAgentPromptByteInventory` and `TestProviderFacingPromptBudgetsWithTrafficAndDeferredTools` enforce per-role byte ceilings | PASS at `b462710`: byte budgets, not tokens |
| Spend and cache | Provider-reported input, output, cache read and cache write tokens, retries, cost and memo hit frequency on representative workloads | NOT RUN; required before any default-on rollout of an opt-in flag |

## Settings

All are environment variables read by the ARTEX server; restart all ARTEX processes after a change and check the
effective environment of the deployment.

| Setting | Default | What it changes | Enable | Rollback | State left behind | Status |
| --- | --- | --- | --- | --- | --- | --- |
| `ARTEX_BOUNDED_CONTEXT` | `0` | Planner snapshot and deltas, request-local references, trace byte caps and paging | `1` | `0` or unset | `planner_context_checkpoints` rows stay and are read and resynchronized when the flag is enabled again | Opt-in; comparison NOT RUN |
| `ARTEX_CACHE_PREFIX` | `0` | Code-owned rules move into a stable cached prefix; planner goal placed after the static rules | `1` | `0` or unset | None | Opt-in; comparison NOT RUN |
| `ARTEX_COMPACTION_MEMO` | `0` | In-memory memo (64 entries) for source-identical cold-graph summaries | `1` | `0` or unset; restart clears the memo | None | Opt-in; comparison NOT RUN |
| `ARTEX_WORKER_CONTEXT` | `0` | Compact worker launch data and projected target assets | `1`, `true`, `on` or `yes` | `0` or unset | None | Opt-in; comparison NOT RUN (#1, #2) |
| `ARTEX_DATA_ENVELOPE` | `1` | Data envelope around derived text and the planner goal anchor; adds bytes per planner round and worker launch | on by default | `0`, `false`, `off` or `no` | None; the code-owned prompt rules stay | Default on; token effect NOT RUN |
| `ARTEX_UNTRUSTED_BOUNDARY` | `1` | SDK tool-output framing; adds bytes per tool result | on by default | `0`; summary trust rules still apply | None | Default on; token effect NOT RUN |
| `ARTEX_TOOL_RAG` | observe | Writes one observation row per planner round and worker run; requests are unchanged | on by default | `off` | `tool_rag_observations` rows stay until their task is deleted | Measurement only; see [tool-rag-observation.md](tool-rag-observation.md) |

`ARTEX_DATA_ENVELOPE` and `ARTEX_UNTRUSTED_BOUNDARY` are security controls against content from targets and
agents ([ADR 0002](adr/0002-data-envelope-and-code-owned-rules.md)). Turning them off to reduce request size is a
security decision, not a token rollback.

## Comparison method

Enable one opt-in flag at a time on an isolated replay deployment, restart, and record source SHA, inputs,
outcome and provider usage; compare with the same run with the flag off. Order: `ARTEX_BOUNDED_CONTEXT=1`, then
independently `ARTEX_CACHE_PREFIX=1`, then independently `ARTEX_COMPACTION_MEMO=1`, then independently
`ARTEX_WORKER_CONTEXT=1`. Keep the other flags at their defaults for each comparison; validate the intended
combination separately before a rollout. Never infer benefits from synthetic reuse. The measurement protocol
(cohorts, usage buckets, pricing) is in [token-benchmark.md](token-benchmark.md); the Phase E benchmark harness
(#27-#29) is the intended way to produce comparable runs.

## Rollback checklist

- [ ] Set `ARTEX_BOUNDED_CONTEXT`, `ARTEX_CACHE_PREFIX`, `ARTEX_COMPACTION_MEMO` and `ARTEX_WORKER_CONTEXT` to `0`
      (or unset), restart all ARTEX processes, and verify the effective deployment environment. Restart clears
      in-memory memo and cache state. Durable `planner_context_checkpoints` rows remain in PostgreSQL and are read
      and resynchronized when bounded context is enabled again.
- [ ] Retry a known replay and confirm the full overview, the original cache boundary and uncached
      summarization. Preserve transcripts and graph/evidence rows.
- [ ] If metering itself causes failures, deploy the previous code version; keep the additive telemetry
      columns, historical rows and `planner_context_checkpoints`. Do not run destructive DDL.
- [ ] Confirm task state, scope constraints, evidence retrieval and Ollama mock compatibility after rollback.
      Record pass/fail and source SHA per criterion.

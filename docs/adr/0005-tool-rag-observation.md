# ADR 0005: Tool RAG in observation mode

- **Status:** Accepted for the part built (pull request 1: the port, the recording, migration `5002`). Not built, and
  not accepted here: the report that computes the four comparisons (pull request 2), the measured conclusion, and any
  filter mode.
- **Date:** 2026-11-20
- **Depends on:** [0004-versioned-migrations-stage-2.md](0004-versioned-migrations-stage-2.md) (the migration)
- **Plan:** `docs/phase-d3-plan.md` (not tracked), revision 3
- **Runbook:** [../tool-rag-observation.md](../tool-rag-observation.md)
- **Issue:** 25. Upstream: `f3a20ef` (Tool RAG), `e640c1b` (its keep-set test).

## Context

Issue 25 asks to compare lexical retrieval, permission behaviour, locked and unlockable tools and fallback, not to
enable blocking behaviour until it is measured, and to default to observe only.

Upstream `f3a20ef` adds `agent/toolrag.go`: a BM25 ranking of an agent's bound tool list after tool resolution, a keep
set, a top-k projection that only removes tools, and a fallback to the full list. It is off by default and
`ARTEX_TOOL_RAG=1` filters. The fork had none of it. Upstream has no observe mode, no stored decision and no
measure of a miss, so it cannot answer the issue.

Facts of the fork that shape the design:

- Planner and worker assemblies are the only ones with a query (the goal and the triggers, or the intent summary).
- With the default catalogs the rankable set is small (by estimate 8 tools for the worker, 11 for the planner, under
  the default k of 12), so the data may show that a filter would hide nothing. That is a valid result.
- MCP tools are deferred and skill-gated ones are locked until the skill loads. Upstream keeps all deferred names.
- Every `log.Printf` line is persisted to `server_logs` without retention, so the query must never be logged.

## Decision

1. **Port the ranking, never filter.** `rankTools`, `bm25Score`, `tokenize`, `projectTools`, `toolRAGKeep`, the core
   names, `WithToolQuery`, the planner query and the worker query (`nodeSummary(intent)`) are ported with their logic.
   `AugmentTools` returns the same list, in the same order, in every mode. `projectTools` stays in the code for parity
   and tests; no production path calls it.
2. **Record content-free rows.** One row per planner round and per worker run, in a new table
   `tool_rag_observations` (migration `5002_tool_rag_observations.sql`, first line `downgrade-safe: yes`). A row holds
   counts and tool names: never the goal, query, description, argument, result or a hash. Names pass one rule (128
   bytes, valid UTF-8, no control character, else `_unknown`), and MCP and custom tool names are stored as they are. The
   owner accepted that.
3. **Default observe.** `ARTEX_TOOL_RAG=off` (also `0`, `false`, `no`) switches it off. Upstream's enabling values and
   `filter` mean observe plus one warning; an unknown value means observe plus one warning that does not echo it.
   `ARTEX_TOOL_RAG_TOPK` is ported (default 12).
4. **Count the calls in the existing panic guard.** Every returned tool is wrapped in `guardedTool`; it counts a call
   by name and class before delegating. Deferred MCP calls go through the same wrapped instances, which a test proves
   end to end with a real `agentcore` session. `third_party/norma` is not touched.
5. **Write asynchronously and never fail a run.** The end-of-run flush is a non-blocking send into a queue of 256. One
   writer inserts each row with a 2 second timeout on its own context. The writer belongs to the `Manager` and stops
   first in `Manager.Close`: a 3 second drain, then drop and count.
6. **Delete with the task.** `task_id` references `tasks(id) ON DELETE CASCADE`. Task archiving keeps the `tasks` row,
   so the table is added to the explicit delete list in `db/task_archives_restore.go`. Rows are not in the archive and
   are not restored.

### Definitions fixed in the build

The plan left these open; the implementers fixed them, and they are part of the contract.

- **`host_list_count`**: the tools handed in minus the names in the deferred list. It equals `keep_count +
  candidate_count`. It is the list at the hook point, not the list the model sees (the SDK adds and hides tools).
- **Keep:** a non-deferred tool whose name is one of the 12 core names (`record_fact`, `insert_assets`,
  `report_finding`, `list_findings`, `graph_overview`, `node_detail`, `add_intent`, `steer_work`, `ask_operator`,
  `prove_goal`, `goal_met`, `list_assets`), `Skill`, or a name in the SDK's default or shell-session tools. `Skill` is
  class `keep`. Deferred names are never keep: they have their own classes (`deferred_global`, `deferred_locked`) and
  are not counted in `host_list_count`.
- **Candidates:** the non-keep, non-deferred tools. `candidate_count` is the true count; at most 512 are scored, at
  most 256 names are stored in `ranking`.
- **`deferred_count`** is the number of distinct deferred names; **`locked_at_start`** is those not unlocked when the
  run starts; **`unlocked_during_run`** is read at flush and includes unlocks replayed from a resumed session.
- **Outcome precedence:** `no_query` (blank query) first; then `scorer_error` or `observer_panic`; then
  `small_catalog` (fewer than 20 tools in the list handed in, deferred included, as upstream counts); then `no_signal`
  (candidates exist and all scores are zero); otherwise `projected`. `would_hide_count` is `max(0, candidate_count - k)`
  for `projected` only.
- **Caps:** query 4096 bytes on a rune boundary (`query_truncated`); scoring text per tool 8192 bytes; 512 candidates
  scored; 256 names in `ranking` and in `called`, the rest of `called` counted under `_overflow`. The database layer
  refuses a row that breaks a cap; it never truncates.
- **Ordering change versus upstream:** the scoring text lists a tool's **parameter names sorted**. Upstream builds it
  from map iteration order. Scores are the same, except that under the 8192 byte cap the cut is now deterministic.
  The tokenizer has an ASCII fast path with the same tokens; the scores are pinned by a golden test.
- **Copy rule:** the observer copies the name and scoring text of each tool into its own slice, ranks that copy, and
  never sorts, re-slices, appends to or writes the incoming list (checked on the backing array).
- **Position:** the ranking runs after tool resolution and before `findingWorkflowTools`, as upstream does. It does
  not see tools the SDK adds later.
- Planner and worker runs **without a task id** are not observed at all: the table requires one.

## Invariant for any later filter mode

A filter mode is not part of this decision. It is named here so that observation data cannot be mistaken for a licence.
When it is built (new plan, after the measurement), it must satisfy all of:

1. Its output is a **subset of the post-resolve tool list**. It never adds, re-binds or unlocks a tool.
2. It is **never a permission boundary**. Binding, the unlock gate, the target gate and permission checks stay where
   they are.
3. Deferred and locked names are untouched.
4. **Any error, empty query or no-signal ranking yields the full list.**
5. The decision **may not rest on description text alone** (descriptions can come from untrusted custom tools, database
   overrides and MCP metadata and can be written to rank first).

The value would be `filter`, default off. Until then `1`, `true`, `on`, `yes` and `filter` mean observe.

## Alternatives rejected

| Alternative | Why rejected |
|---|---|
| Port upstream as is (filter, off by default) | The issue forbids filtering before measurement, and nothing is recorded |
| Log lines only | Persisted to `server_logs` forever and shown in the UI; fragile; no join with called tools |
| A column on `llm_usage` | Wrong granularity (per physical call); history replay; `task_id` is text |
| `tool_usage` alone for calls | Lacks keep-set SDK tools, `Skill` and deferred MCP calls. Kept as a cross-check for the report |
| A new counting decorator | `guardedTool` already wraps every returned tool |
| A synchronous write in the cleanup | Delays the caller and the MCP client close |
| Asynchronous scoring, off the model path | The ranking would describe a list the run may already have changed, and it needs its own lifecycle; the realistic delay is small |
| A query hash, a catalog fingerprint, `would_hide_bytes` | A hash of low-entropy text is guessable; the others are needed by no criterion of pull request 1 |
| Rows for the main agent and chat | No query, nothing to compare |
| No foreign key, like `tool_usage` | Rows would outlive a deleted task |
| A stored `invariant_violations` column | At the hook point it is a tautology; against the final list it false-alarms because of `findingWorkflowTools`. Unit tests cover the invariant |
| Offline replay only; embedding retrieval; ranking deferred tools | Out of scope; in the backlog |

## Consequences

- **Default on adds one database write per planner round and worker run**, and a scoring delay before the first model
  request of each (0.32 ms for 40 tools of 1 KiB; 24 ms at the caps; one laptop-class machine). `ARTEX_TOOL_RAG=off`
  removes both. The model request is byte-identical in every mode (test criterion).
- **Shutdown can take up to 3 seconds longer** (the drain). Worst case about 8 seconds including the 5 second HTTP
  shutdown, against the 10 seconds `docker stop` allows. Rows are lost, uncounted, if the process is killed or
  crashes, and counted when the queue is full or at the drain deadline.
- **Stored tool names reveal engagement words in MCP and custom tool names.** Accepted by the owner: same database as
  the MCP and tool configuration, deleted with the task, read by nothing in this release.
- **Access rule.** No route, export, archive or UI reads the table. A later reader must be admin-only once roles exist
  and must be named in its own plan.
- **The first fork migration after the baseline.** `5002` is the first fork migration that changes the catalog (upstream
  `0004` added an index before it). The ledger of a started database now ends with `5002`, the reference catalog has one more table, and rolling back needs
  `ARTEX_SCHEMA_ALLOW_NEWER=5002` or the guarded drop `db/testdata/upgrade/rollback-drop-5002.sql`, run first, before the
  `0004` recipe and the older ledger cleanup.
- **The fork diverges from upstream's `toolrag.go`** by the adaptations above. The pure functions stay close, and the
  upstream tests are ported with their assertions unchanged (one adapted).
- **A known gap:** a panic in the scoring step outside the observer's own recover (for example a tool whose name lookup
  panics) yields one log line and no row, not a row with outcome `observer_panic`.

## Deferred

- The report that computes the four comparisons (retrieval miss rate at k, permission behaviour, locked versus
  unlockable tools, fallback), with the `tool_usage` cross-check: pull request 2.
- The measured conclusion and any bar for a filter mode, after the 14 day observation window.
- Shadow ranking of deferred MCP tools; counting SDK refusals of locked tools; observations for the main agent and
  chat; offline replay of historical data.
- A filter mode (see the invariant above), under a new plan.

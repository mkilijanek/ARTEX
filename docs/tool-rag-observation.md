# Tool RAG observation mode

Phase D.3, pull request 1. Design: [adr/0005-tool-rag-observation.md](adr/0005-tool-rag-observation.md). Issue 25.

This page is for the operator. It says what the feature does, what it stores, how to switch it off, how to check
that it is healthy, and how to roll it back. The report that computes the comparisons of the issue ships in the next
pull request; this page is extended then.

## What it is, and what it is not

Upstream's Tool RAG ranks an agent's tools against the task text (BM25, lexical) and can hide the low-ranked ones
from the model. This release ports the ranking and **only observes**:

- For every **planner round** and every **worker run**, the ranking is computed next to the real tool list and one
  row is written: what the ranking would have kept, and which tools the model actually called.
- **The tool list the model receives is never changed.** Same tools, same order, same descriptions and schemas, same
  request bytes. This is a test criterion, not only a design aim.
- **Nothing is blocked.** The ranking is never used for a decision.
- **There is no filter mode in this release.** Upstream's enabling values (`1`, `true`, `on`, `yes`) and the word
  `filter` are accepted and mean observe, with one warning (see "The switch").
- The main agent, chat, the reporter and any run without a task id are not observed.

The purpose is to measure, before anything is ever filtered, how often a filter would have hidden a tool that the
model then used.

## What is recorded

One row in the table `tool_rag_observations` per planner round and per worker run, written when the run's tools are
cleaned up. The row holds **names and counts only**:

| Column | Meaning |
|---|---|
| `task_id`, `exploration_id`, `intent_id` | Whose run it was. `intent_id` is empty (NULL) for the planner |
| `agent_key` | `planner` or `worker` |
| `scorer`, `mode` | `bm25-v1`, `observe` |
| `outcome` | What upstream's projection would have done: `projected`, `small_catalog`, `no_query`, `no_signal`, `scorer_error`, `observer_panic` (below) |
| `k` | Candidates the projection keeps (`ARTEX_TOOL_RAG_TOPK`, default 12) |
| `host_list_count` | Tools the host handed in, minus deferred tools. It is `keep_count + candidate_count`. It is **not** the list the model sees: the SDK adds tools of its own and may hide some |
| `keep_count` | Of those, tools that the projection always keeps (12 core names, `Skill`, the SDK's default and shell-session tools) |
| `candidate_count` | Of those, tools that could be ranked and hidden (counted before the cap of 512 scored) |
| `nonzero_count` | Candidates with a score above zero |
| `would_hide_count` | `max(0, candidate_count - k)` when the outcome is `projected`, else 0 |
| `deferred_count`, `locked_at_start`, `unlocked_during_run` | Deferred tools (MCP and custom), how many were locked when the run started, how many of those were unlocked at its end. On a resumed session the last number includes unlocks replayed from history |
| `query_tokens`, `query_truncated` | How many tokens the query had (a count), and whether it was cut at 4096 bytes |
| `duration_us` | Scoring time in microseconds |
| `ranking` | Candidate names in rank order, at most 256 |
| `called` | The tools the model called in the run: `{"name": {"n": 3, "class": "candidate"}}`. Classes: `candidate`, `keep`, `deferred_global`, `deferred_locked` (locked when the run started), `other`. At most 256 names; the rest count under `_overflow` |
| `ts` | Insert time |

Outcomes, in the order they are decided:

1. `no_query`: the goal or intent text is blank. Nothing is ranked.
2. `scorer_error` or `observer_panic`: scoring failed. The row says so; the run is not affected.
3. `small_catalog`: fewer than 20 tools in the list handed in (deferred tools included). Upstream would pass the whole
   list. The ranking is still stored.
4. `no_signal`: there are candidates and every score is zero, so the order is alphabetical and means nothing.
5. `projected`: upstream would have filtered.

**Never recorded:** the goal, the intent text, the query, a hash of any of them, tool descriptions, schemas, arguments,
results, error messages. The query is used for scoring in memory and only its token count is kept.

**Tool names are recorded, after one rule.** A name that is empty, longer than 128 bytes, not valid UTF-8 or that
contains a control character is stored as `_unknown`. Every other name is stored as it is, **including MCP tool names
(`mcp__<server>__<tool>`) and custom tool names**, which can carry words that identify an engagement or a server.
This is a data point that the owner accepted. The names sit in the same database as the MCP server and tool
configuration and go with the task (see "Deletion").

## The switch

`ARTEX_TOOL_RAG` is read on every planner and worker assembly. The value is trimmed and lowercased.

| Value | What happens |
|---|---|
| unset, empty, `observe` | Observe. This is the default |
| `off`, `0`, `false`, `no` | Nothing is scored, counted or written |
| `1`, `true`, `on`, `yes`, `filter` | Observe, plus one warning per process: `[tool-rag] ARTEX_TOOL_RAG=<value> requests filtering, which this build does not implement; observing only` |
| anything else | Observe, plus one warning per process: `[tool-rag] ARTEX_TOOL_RAG has an unrecognised value (not shown); accepted values: observe, off, 0, false, no, 1, true, on, yes, filter; observing only` |

At most one of the two warnings is printed per process. A typo therefore does not switch the feature off: it observes.

`ARTEX_TOOL_RAG_TOPK` sets `k`. A positive integer; anything else means 12. It changes only what the row says would
have been hidden.

At start the server logs `[tool-rag] mode=observe scorer=bm25-v1 k=12` or `[tool-rag] mode=off`, once per process.

**Docker Compose.** `docker-compose.yml` passes `ARTEX_TOOL_RAG` and `ARTEX_TOOL_RAG_TOPK` from `.env` to the
container; an empty value means the default. After a change, check the `mode=` line in the log.

## Cost

Measured once, on one laptop-class linux/amd64 machine, Go 1.26, three runs each. Treat as an order of magnitude.

| What | Measured | Budget |
|---|---|---|
| Scoring delay before the first model request of a planner round or worker run, 40 tools of 1 KiB each | 0.32 ms per assembly | 0.5 ms |
| Same, at the caps (512 tools of 8 KiB each, 4 KiB query), which only a hostile or huge catalog reaches | 24 ms and 5.5 MB per assembly | 100 ms and 16 MiB |
| Counting one tool call | about 80 ns | 2 us |

Scoring is synchronous, inside the tool assembly, once per planner round or worker run. Later requests of the same
run are not delayed. The row is written afterwards by a background writer. `duration_us` in the table shows the real
figure on your instance; the target is a 95th percentile of at most 5 ms.

Space is not measured. By estimate a typical row is about 1 KiB, and a row at every cap about 75 KiB, so a task with
300 rows takes about 300 KiB typically.

## When something fails

**An observation failure never fails a task.** The tool list is returned before anything is written, and the work
after it is wrapped, counted and logged. Each kind below is counted in memory from process start; a log line is
printed for the first occurrence and then at most once per minute per kind, with the running count
(`<n> since start`). The lines carry a Go type or an SQLSTATE and never text from an error, a query or a tool.

| Situation | What happens | Log line |
|---|---|---|
| The write queue is full (256 rows) | The new row is dropped | `[tool-rag] observation drop; <n> since start (queue full)` |
| Database down or any other insert error | The row is lost | `[tool-rag] observation insert error; <n> since start (SQLSTATE <code>)`, or `(type <Go type>)` when the error has no SQLSTATE |
| An insert takes longer than 2 seconds | The row is lost | `[tool-rag] observation timeout; <n> since start` |
| The database layer refuses the row (a name or list over its cap) | The row is lost. A defect of the sender | `[tool-rag] observation invalid row; <n> since start` |
| The writer panics inside an insert | The panic is recovered, the row is lost, the writer goes on | `[tool-rag] observation writer panic; <n> since start (type <Go type>)` |
| The task was deleted or archived while the row waited (SQLSTATE `23503`) | The row is not needed. Not a failure | none; counted as `task_gone` in the shutdown line |
| The scorer returns an error | A row with outcome `scorer_error` is written | `[tool-rag] observation scorer error; <n> since start (type <Go type>)` |
| Scoring panics | A row with outcome `observer_panic` is written | `[tool-rag] observation observer panic; <n> since start (type <Go type>)` |
| Anything else in the scoring step panics (for example a tool whose name lookup panics) | No row | `[tool-rag] observation observer panic; ...` |
| Handing the row over at the end of the run panics | No row; the MCP clients are still closed | `[tool-rag] observation flush panic; <n> since start (type <Go type>)` |

The counters live in memory and start again at zero when the process restarts.

## Where the counters are visible

- **The shutdown line**, once, when the manager closes:
  `[tool-rag] writer stopped: written=<n> dropped_shutdown=<n> task_gone=<n> invalid_row=<n> writer_panic=<n>`.
  It is printed even when no row was ever written.
- **The rate-limited lines above**, while the process runs.
- Every line goes to the log and, like all log lines, to the table `server_logs` and the console.

A run that ends after the writer has stopped still counts its row in `dropped_shutdown`, but the line was already
printed, so that late count is **not printed**.

## Shutdown

The writer is stopped first in the manager's close, before the database closes:

1. Rows from runs that end later are dropped and counted as `dropped_shutdown`, without a log line.
2. Queued rows are written for at most **3 seconds**. The insert in flight is cancelled at the deadline and the rest
   are dropped and counted as `dropped_shutdown`.
3. The close waits for the writer to end, so no insert runs against a closed database, and prints the line above.

Runs that the shutdown signal aborts end during the HTTP shutdown, before this step, so their rows are normally
written. The worst case is up to 5 seconds of HTTP shutdown plus up to 3 seconds of drain, about 8 seconds, which is
read from the code and not measured. `docker stop` kills the process after 10 seconds (`start.sh`; the compose file
sets no longer grace period). If the process is killed, or crashes, queued rows are lost and not counted, the same
as any in-memory data.

## Deletion

- **Task deleted:** the rows go with it (`ON DELETE CASCADE`).
- **Task archived:** archiving deletes the task's observation rows, together with its `skill_usage` and `tool_usage`
  rows. The task itself stays.
- **Archive restored:** the rows are **not** in the archive and **not** brought back. A restored task starts without
  observations; new runs add rows.
- There is no time-based pruning. Rows live as long as their task.

## Who can read the table

No API route, UI view, export or task archive reads `tool_rag_observations`. It is read by **database access only**.
The rule for later: any future reader must be admin-only and must be named in its own plan. A report that prints tool
names must quote a name that starts with `=`, `+`, `-` or `@` before the output goes into a spreadsheet, because the
names come from operators and MCP servers.

## Check after 14 days: the read-only SQL

Run it on the operator's instance after the release has been running with real tasks for 14 days. `t0` is the time
the release was deployed. It reads only; it begins with `BEGIN READ ONLY` and ends with `ROLLBACK`. It was written
against the table definitions in the code and has not been run against a database at the time of writing.

Save it as `tool-rag-day14.sql`, then (with Docker Compose):

```bash
docker compose exec -T postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
  -v t0='2026-11-20 00:00:00+00' < tool-rag-day14.sql
```

```sql
BEGIN READ ONLY;
-- 1. Coverage: distinct worker intents with model calls versus those with an observation row.
WITH calls AS (
  SELECT DISTINCT intent_id FROM llm_usage
  WHERE worker = 'worker' AND intent_id <> 0 AND ts >= :'t0'
), obs AS (
  SELECT DISTINCT intent_id FROM tool_rag_observations
  WHERE agent_key = 'worker' AND ts >= :'t0'
)
SELECT (SELECT count(*) FROM calls) AS worker_intents,
       (SELECT count(*) FROM calls JOIN obs USING (intent_id)) AS observed_intents;
-- 2. Scoring delay and outcomes per agent.
SELECT agent_key, outcome, count(*) AS rows,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_us) AS p95_us,
       max(duration_us) AS max_us
FROM tool_rag_observations WHERE ts >= :'t0'
GROUP BY ROLLUP (agent_key, outcome) ORDER BY 1, 2;
-- 3. [tool-rag] log lines by kind (failure lines exclude 23503 by design; the summary line is info).
SELECT CASE
         WHEN text LIKE '%writer stopped%' THEN 'shutdown summary'
         WHEN text LIKE '%mode=%' THEN 'mode'
         WHEN text LIKE '%requests filtering%' OR text LIKE '%accepted values%' THEN 'mode warning'
         WHEN text LIKE '%panic%' THEN 'panic'
         WHEN text LIKE '%drop%' THEN 'drop'
         ELSE 'failure' END AS kind,
       count(*) AS lines
FROM server_logs
WHERE created_at >= :'t0' AND (tag = 'tool-rag' OR text LIKE '%[tool-rag]%')
GROUP BY 1 ORDER BY 1;
-- 4. Task error rate: share of tasks reaching failed/timeout, this window versus the 14 days before.
SELECT CASE WHEN updated_at >= :'t0' THEN 'window' ELSE 'before' END AS period,
       count(*) FILTER (WHERE status IN ('failed','timeout')) AS failed,
       count(*) FILTER (WHERE status IN ('done','failed','timeout')) AS terminal
FROM tasks
WHERE updated_at >= :'t0'::timestamptz - interval '14 days'
GROUP BY 1;
ROLLBACK;
```

Every column it uses exists in the code as built: `llm_usage.worker`, `intent_id` and `ts`; the columns of
`tool_rag_observations`; `server_logs.created_at`, `tag` and `text`; `tasks.status` and `updated_at`. In `llm_usage`,
`worker` is `worker` for a worker run and `planner` for a planner round; `intent_id` is the intent for a worker run and
0 for the planner, which is why query 1 selects `worker = 'worker' AND intent_id <> 0`.

How the lines of query 3 fall into kinds: `drop` is the queue-full line; `panic` is `observer panic`, `flush panic`
and `writer panic`; `failure` is `insert error`, `timeout`, `invalid row` and `scorer error`; `shutdown summary` is the
stop line.

Reading the result:

- **HEALTHY:**
  - query 1: `observed_intents` is at least 95% of `worker_intents`;
  - query 3: no `failure`, `drop` or `panic` lines;
  - query 2: the 95th percentile of `duration_us` is at most 5000 (5 ms);
  - query 4: the share `failed / terminal` of the window is not above the share of `before`.
- **DEGRADED:** coverage between 80% and 95%, or any `failure` or `drop` line, or a 95th percentile between 5 and
  20 ms.
- **Roll back (see below):** a task failure that you can attribute to the observer, a 95th percentile above 20 ms, or
  query text or canary text found in the table or in the logs.
- **UNKNOWN:** no rows, or no real tasks in the window. Do not read this as healthy.

Limits of the reading. Coverage counts distinct intents, so a resumed intent does not push it above 100%. A task that
was deleted in the window can remove its observation rows while its `llm_usage` rows remain, which makes coverage read
low; set such tasks aside. The task error rate is a proxy: a failure belongs to the observer only if a `[tool-rag]`
line or a stack in `server_logs` says so.

## Rollback

1. **Switch it off:** set `ARTEX_TOOL_RAG=off` and restart. No scoring,
   no counter, no rows. Rows already written stay until their task goes.
2. **Go back to the previous binary.** The table is additive, so the migration is marked `downgrade-safe: yes`, but an
   older binary refuses a ledger that holds a migration it does not ship (`5002`). Either start it with
   `ARTEX_SCHEMA_ALLOW_NEWER=5002` (the table is then ignored; read the risk in
   [schema-upgrade.md](schema-upgrade.md), "The override"), or drop the table with this guarded recipe, with no ARTEX
   instance running (the text is `db/testdata/upgrade/rollback-drop-5002.sql`):

   ```sql
   BEGIN;
   SELECT pg_advisory_xact_lock(7337741001);
   DO $$
   BEGIN
     IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5002 THEN
       RAISE EXCEPTION '5002 is not the newest fork migration: restore the pre-upgrade dump instead';
     END IF;
   END $$;
   DELETE FROM schema_migrations WHERE version = 5002 AND name = '5002_tool_rag_observations.sql';
   DROP TABLE IF EXISTS tool_rag_observations;
   COMMIT;
   ```

   It **refuses on any other ledger**: when `5002` is not the newest fork migration it raises and changes nothing, and
   the way back is the pre-upgrade dump.
3. **Order with the ledger cleanup.** The older recipe `rollback-cleanup-ledger.sql` drops `schema_migrations` only when
   the ledger holds exactly the baseline versions, so it refuses while `5002` is recorded. Run the `5002` drop
   **first**, then the cleanup if you want it.

What is lost on a rollback is the measurements, nothing else.

## Limits

- Only planner rounds and worker runs with a task id are observed.
- The ranking sees the list as the host assembles it after tool binding and before the finding-workflow step
  (`findingWorkflowTools`, which can remove `bind_finding_traffic` and rewrite descriptions). It does not see the tools
  the SDK adds afterwards (for example `SearchExtraTools`, `ExecuteExtraTool`, WebFetch, background-task tools).
- Deferred tools (MCP and custom) are never ranked; they are counted as deferred.
- The call counter does not count the SDK-added tools above, and does not count a call that the SDK refused because the
  tool was locked.
- Rows are lost when the process crashes or is killed, when the queue is full and at the shutdown deadline.
- There is **no counterfactual**. The data says "the model used a tool that a filter would have removed", not what the
  model would have done with the filtered list.
- On a typical default setup the rankable set is small (by estimate 8 to 11 tools, under `k` = 12), so the first thing
  the data must show is how often anything would be hidden at all. That estimate is from the default catalogs, not
  from a measurement.
- The four comparisons of the issue (retrieval quality, permission behaviour, locked versus unlockable tools,
  fallback) are computed by a report that ships in the next pull request. Until then, the SQL above is the check.

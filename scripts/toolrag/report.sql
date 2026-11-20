-- Tool RAG observation report (Phase D.3, docs/phase-d3-plan.md 3.7). READ ONLY: one transaction that is
-- rolled back, plain SELECTs, no temporary table, no function, no dynamic SQL.
--
--   psql -v ON_ERROR_STOP=1 -v t0='2026-11-20 00:00:00+00' -f scripts/toolrag/report.sql
--
-- t0 is the start of the window (the time the release was deployed). Every table is read from t0 on.
-- Each statement is preceded by a comment line "-- [id] title". Names are printed as stored: a tool name can
-- start with = + - or @, so quote the output before it goes into a spreadsheet.
--
-- A "run" below is one row of tool_rag_observations: one planner round or one worker run. A resumed worker
-- intent writes more than one row. There is no counterfactual anywhere in this report: it says what the
-- model used, not what it would have done with a filtered list.
BEGIN READ ONLY;

-- Coverage and data quality. Read the would-hide share first: with the default tool bindings the plan expects
-- the rankable set (candidates) to stay under k = 12, which makes the share near zero and every miss rate
-- below empty by construction.

-- [0.1] Coverage: rows and distinct tasks per agent and outcome.
SELECT agent_key, outcome, count(*) AS n_rows, count(DISTINCT task_id) AS tasks
FROM tool_rag_observations
WHERE ts >= :'t0'
GROUP BY agent_key, outcome
ORDER BY agent_key COLLATE "C", outcome COLLATE "C";

-- [0.2] Coverage: size and time span of the window (UTC).
SELECT count(*) AS n_rows,
       count(DISTINCT task_id) AS tasks,
       count(DISTINCT intent_id) FILTER (WHERE agent_key = 'worker') AS worker_intents,
       to_char(min(ts) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') AS first_row_utc,
       to_char(max(ts) AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') AS last_row_utc
FROM tool_rag_observations
WHERE ts >= :'t0';

-- [0.3] Would-hide share: runs where upstream's projection would have hidden anything (would_hide_count > 0).
-- Percent of projected rows (the rows where a filter would have acted) and percent of all rows. The row "all"
-- is the total. A near-zero share ends the question: nothing to filter.
SELECT coalesce(agent_key, 'all') AS agent_key,
       count(*) AS n_rows,
       count(*) FILTER (WHERE outcome = 'projected') AS projected_rows,
       count(*) FILTER (WHERE outcome = 'projected' AND would_hide_count > 0) AS rows_would_hide,
       round(100.0 * count(*) FILTER (WHERE outcome = 'projected' AND would_hide_count > 0)
             / nullif(count(*) FILTER (WHERE outcome = 'projected'), 0), 1) AS would_hide_pct_of_projected,
       round(100.0 * count(*) FILTER (WHERE outcome = 'projected' AND would_hide_count > 0)
             / nullif(count(*), 0), 1) AS would_hide_pct_of_all_rows,
       max(candidate_count) AS max_candidate_count,
       max(would_hide_count) AS max_would_hide_count
FROM tool_rag_observations
WHERE ts >= :'t0'
GROUP BY ROLLUP (agent_key)
ORDER BY grouping(agent_key), agent_key;

-- [0.4] Data quality: rows with names the writer replaced or counted. _unknown stands for a name that was
-- empty, over 128 bytes or had a control character, _overflow counts the calls beyond 256 distinct names.
-- A ranking at 256 names, or shorter than candidate_count in a row that has a ranking, was cut at the caps
-- (scoring covers 512 candidates, the stored ranking 256): positions past the cut are not known.
SELECT count(*) FILTER (WHERE ranking @> ARRAY['_unknown']) AS rows_unknown_in_ranking,
       count(*) FILTER (WHERE called ? '_unknown') AS rows_unknown_called,
       count(*) FILTER (WHERE called ? '_overflow') AS rows_overflow_called,
       count(*) FILTER (WHERE cardinality(ranking) = 256) AS rows_ranking_at_cap,
       count(*) FILTER (WHERE outcome IN ('projected', 'small_catalog', 'no_signal')
                          AND cardinality(ranking) < candidate_count) AS rows_ranking_shorter_than_candidates
FROM tool_rag_observations
WHERE ts >= :'t0';

-- [0.5] Coverage: worker intents with model calls in llm_usage versus those with an observation row.
-- Distinct intents on both sides (a resumed intent has several rows of each kind), and the numerator counts the
-- intents that have a row, so it can never exceed the denominator: coverage is at most 100 percent by
-- construction. llm_usage.task_id is TEXT: the join compares text and never casts it, so a task id that is not a
-- number cannot stop the report. An intent that appears here as unobserved can also be one whose task was
-- deleted (the rows cascade with the task, llm_usage stays), or whose row was lost (queue full, crash, shutdown).
-- observed_without_model_calls: intents with a row but no worker model call in the window (the run failed
-- before a call, or llm_usage lost it). worker_observation_rows exceeds worker_intents when intents resumed.
WITH calls AS (
  SELECT DISTINCT task_id, intent_id
  FROM llm_usage
  WHERE worker = 'worker' AND intent_id <> 0 AND ts >= :'t0'
), obs AS (
  SELECT DISTINCT task_id::text AS task_id, intent_id
  FROM tool_rag_observations
  WHERE agent_key = 'worker' AND intent_id IS NOT NULL AND ts >= :'t0'
), cov AS (
  SELECT (SELECT count(*) FROM calls) AS with_calls,
         (SELECT count(*) FROM calls c
           WHERE EXISTS (SELECT 1 FROM obs o WHERE o.task_id = c.task_id AND o.intent_id = c.intent_id)) AS observed,
         (SELECT count(*) FROM obs o
           WHERE NOT EXISTS (SELECT 1 FROM calls c WHERE o.task_id = c.task_id AND o.intent_id = c.intent_id)) AS no_calls,
         (SELECT count(*) FROM tool_rag_observations WHERE agent_key = 'worker' AND ts >= :'t0') AS obs_rows
)
SELECT with_calls AS worker_intents_with_calls,
       observed AS observed_intents,
       round(100.0 * observed / nullif(with_calls, 0), 1) AS coverage_pct,
       no_calls AS observed_without_model_calls,
       obs_rows AS worker_observation_rows
FROM cov;

-- [0.6] Coverage: the worker intents with model calls and no observation row (first 50, by intent).
SELECT u.task_id, u.intent_id, count(*) AS llm_rows
FROM llm_usage u
WHERE u.worker = 'worker' AND u.intent_id <> 0 AND u.ts >= :'t0'
  AND NOT EXISTS (SELECT 1 FROM tool_rag_observations o
                  WHERE o.agent_key = 'worker' AND o.ts >= :'t0'
                    AND o.task_id::text = u.task_id AND o.intent_id = u.intent_id)
GROUP BY u.task_id, u.intent_id
ORDER BY u.intent_id, u.task_id
LIMIT 50;

-- A. Lexical retrieval. Population: rows with outcome projected. Nothing can be hidden when candidate_count
-- is not above k, so a miss is impossible there and those rows are left out of every miss rate. A miss means:
-- the run called a candidate that the ranking puts after position k. Calls to keep and deferred tools are never
-- hidden and are not part of a miss. A called candidate that is not in the stored ranking has no position: it is
-- left out of the denominator here and shows in B.1. There is no counterfactual: a model given the filtered list
-- might have used other tools. The numbers say "the model used a tool a filter would have removed".

-- [A.1] Miss rate at k = 4, 8, 12, 16, per agent. runs_hiding_at_k: projected rows whose candidate_count is
-- above k. runs_calling_ranked_candidate: of those, rows that called at least one candidate that has a
-- position (the denominator of the rate). miss_runs: of those, rows with a called candidate after position k.
WITH flat AS (
  SELECT o.id, o.outcome, c.value ->> 'class' AS tool_class, array_position(o.ranking, c.key) AS pos
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0'
), called_ranked AS (
  SELECT id, max(pos) AS max_pos
  FROM flat
  WHERE outcome = 'projected' AND tool_class = 'candidate'
  GROUP BY id
), pop AS (
  SELECT o.agent_key, o.candidate_count, r.max_pos
  FROM tool_rag_observations o
  LEFT JOIN called_ranked r ON r.id = o.id
  WHERE o.ts >= :'t0' AND o.outcome = 'projected'
)
SELECT pop.agent_key, ks.k,
       count(*) AS runs_hiding_at_k,
       count(pop.max_pos) AS runs_calling_ranked_candidate,
       count(*) FILTER (WHERE pop.max_pos > ks.k) AS miss_runs,
       round(100.0 * count(*) FILTER (WHERE pop.max_pos > ks.k) / nullif(count(pop.max_pos), 0), 1) AS miss_pct
FROM pop
JOIN (VALUES (4), (8), (12), (16)) AS ks(k) ON pop.candidate_count > ks.k
GROUP BY pop.agent_key, ks.k
ORDER BY pop.agent_key COLLATE "C", ks.k;

-- [A.1p] Planner rounds per task, at the k each row recorded (planner queries are near constant inside one
-- task, so rounds of a task are not independent samples and are not pooled). rounds_hiding: would_hide_count > 0.
WITH flat AS (
  SELECT o.id, c.value ->> 'class' AS tool_class, array_position(o.ranking, c.key) AS pos
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0' AND o.agent_key = 'planner' AND o.outcome = 'projected'
), called_ranked AS (
  SELECT id, max(pos) AS max_pos
  FROM flat
  WHERE tool_class = 'candidate'
  GROUP BY id
)
SELECT o.task_id,
       count(*) AS projected_rounds,
       count(*) FILTER (WHERE o.would_hide_count > 0) AS rounds_hiding,
       count(r.max_pos) FILTER (WHERE o.would_hide_count > 0) AS rounds_calling_ranked_candidate,
       count(*) FILTER (WHERE o.would_hide_count > 0 AND r.max_pos > o.k) AS miss_rounds,
       round(100.0 * count(*) FILTER (WHERE o.would_hide_count > 0 AND r.max_pos > o.k)
             / nullif(count(r.max_pos) FILTER (WHERE o.would_hide_count > 0), 0), 1) AS miss_pct
FROM tool_rag_observations o
LEFT JOIN called_ranked r ON r.id = o.id
WHERE o.ts >= :'t0' AND o.agent_key = 'planner' AND o.outcome = 'projected'
GROUP BY o.task_id
ORDER BY o.task_id;

-- [A.2] Called tools by class, in the runs where something would be hidden (projected, would_hide_count > 0),
-- at the k each row recorded. population_runs is the number of such runs. Only candidates can be hidden: for
-- them the pairs (run, name) are split into inside the top k, outside it, and without a position. The other
-- classes are shown with their counts and NULL in the split.
WITH hiding AS (
  SELECT id, k, ranking, called
  FROM tool_rag_observations
  WHERE ts >= :'t0' AND outcome = 'projected' AND would_hide_count > 0
), pairs AS (
  SELECT c.value ->> 'class' AS tool_class, (c.value ->> 'n')::bigint AS n,
         array_position(h.ranking, c.key) AS pos, h.k
  FROM hiding h
  CROSS JOIN LATERAL jsonb_each(h.called) AS c(key, value)
)
SELECT (SELECT count(*) FROM hiding) AS population_runs,
       tool_class,
       count(*) AS run_name_pairs,
       sum(n) AS calls,
       CASE WHEN tool_class = 'candidate' THEN count(*) FILTER (WHERE pos <= k) END AS pairs_inside_top_k,
       CASE WHEN tool_class = 'candidate' THEN count(*) FILTER (WHERE pos > k) END AS pairs_outside_top_k,
       CASE WHEN tool_class = 'candidate' THEN count(*) FILTER (WHERE pos IS NULL) END AS pairs_unranked
FROM pairs
GROUP BY tool_class
ORDER BY tool_class COLLATE "C";

-- [A.3] The names most often called while ranked outside the top k (same population as A.2, first 20).
-- runs: runs that called the name outside the top k. min_pos and max_pos: its positions in those runs.
SELECT c.key AS tool_name, o.agent_key,
       count(*) AS runs,
       sum((c.value ->> 'n')::bigint) AS calls,
       min(array_position(o.ranking, c.key)) AS min_pos,
       max(array_position(o.ranking, c.key)) AS max_pos
FROM tool_rag_observations o
CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
WHERE o.ts >= :'t0' AND o.outcome = 'projected' AND o.would_hide_count > 0
  AND c.value ->> 'class' = 'candidate'
  AND array_position(o.ranking, c.key) > o.k
GROUP BY c.key, o.agent_key
ORDER BY count(*) DESC, sum((c.value ->> 'n')::bigint) DESC, c.key COLLATE "C", o.agent_key COLLATE "C"
LIMIT 20;

-- [A.4] Rank position of called candidates, in all projected rows (not only the hiding ones, so that the
-- ranker can be judged even when nothing would be hidden). A pair is (run, name). Names without a position are
-- left out. pairs_in_hiding_runs: the same count, limited to rows with would_hide_count > 0.
WITH pairs AS (
  SELECT array_position(o.ranking, c.key) AS pos, (c.value ->> 'n')::bigint AS n,
         o.would_hide_count > 0 AS hiding
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0' AND o.outcome = 'projected' AND c.value ->> 'class' = 'candidate'
)
SELECT b.bucket AS rank_bucket,
       count(p.pos) AS pairs_all_projected,
       coalesce(sum(p.n), 0) AS calls_all_projected,
       count(p.pos) FILTER (WHERE p.hiding) AS pairs_in_hiding_runs
FROM (VALUES ('01-04', 1, 4), ('05-08', 5, 8), ('09-12', 9, 12), ('13-16', 13, 16), ('17+', 17, 1000000))
     AS b(bucket, lo, hi)
LEFT JOIN pairs p ON p.pos BETWEEN b.lo AND b.hi
GROUP BY b.bucket, b.lo
ORDER BY b.lo;

-- [A.5] Called candidates ranked after nonzero_count, that is inside the part of the ranking where every score
-- is zero and the order is alphabetical (all projected rows, names with a position only).
WITH pairs AS (
  SELECT array_position(o.ranking, c.key) AS pos, o.nonzero_count AS nz, (c.value ->> 'n')::bigint AS n
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0' AND o.outcome = 'projected' AND c.value ->> 'class' = 'candidate'
)
SELECT count(pos) AS ranked_pairs,
       count(pos) FILTER (WHERE pos > nz) AS pairs_after_nonzero,
       round(100.0 * count(pos) FILTER (WHERE pos > nz) / nullif(count(pos), 0), 1) AS pairs_pct,
       coalesce(sum(n) FILTER (WHERE pos IS NOT NULL), 0) AS ranked_calls,
       coalesce(sum(n) FILTER (WHERE pos > nz), 0) AS calls_after_nonzero,
       round(100.0 * coalesce(sum(n) FILTER (WHERE pos > nz), 0)
             / nullif(sum(n) FILTER (WHERE pos IS NOT NULL), 0), 1) AS calls_pct
FROM pairs;

-- [A.6] The no_signal share: rows where the catalog was large enough to filter (outcome projected or
-- no_signal; a small catalog is decided first and counted in D) and every score was zero.
SELECT coalesce(agent_key, 'all') AS agent_key,
       count(*) FILTER (WHERE outcome IN ('projected', 'no_signal')) AS large_catalog_rows,
       count(*) FILTER (WHERE outcome = 'no_signal') AS no_signal_rows,
       round(100.0 * count(*) FILTER (WHERE outcome = 'no_signal')
             / nullif(count(*) FILTER (WHERE outcome IN ('projected', 'no_signal')), 0), 1) AS no_signal_pct
FROM tool_rag_observations
WHERE ts >= :'t0'
GROUP BY ROLLUP (agent_key)
ORDER BY grouping(agent_key), agent_key;

-- B. Permission behaviour. The ranking runs on the list after the permission step, so a ranked tool is a
-- permitted tool: that is proven by unit tests, not by a column. "Hides a permitted tool that was needed" is
-- the miss rate of A. What the stored rows can still check is the subset relation below.

-- [B.1] Subset check: every called name of class candidate must be in the stored ranking of its row (rows
-- with outcome projected, small_catalog or no_signal, the outcomes that store a ranking). Violations should be
-- zero. violations_complete_ranking counts the misses in rows whose ranking holds all candidates; a non-zero
-- value means the class or the ranking was recorded from a different list than the call saw, for example a
-- tool removed or added after the point where the ranking is taken (the finding-workflow step rewrites the list
-- afterwards) or a defect of the recorder, and every position-based number above is then suspect for those
-- rows. missing_in_truncated_ranking: the row's ranking was cut at the caps (see 0.4), which explains the miss.
WITH checked AS (
  SELECT o.id,
         cardinality(o.ranking) < o.candidate_count AS truncated,
         c.key = ANY (o.ranking) AS in_ranking
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0' AND o.outcome IN ('projected', 'small_catalog', 'no_signal')
    AND c.value ->> 'class' = 'candidate'
)
SELECT (SELECT count(*) FROM tool_rag_observations
         WHERE ts >= :'t0' AND outcome IN ('projected', 'small_catalog', 'no_signal')) AS rows_checked,
       count(*) AS called_candidate_pairs,
       count(*) FILTER (WHERE NOT in_ranking) AS pairs_not_in_ranking,
       count(*) FILTER (WHERE NOT in_ranking AND NOT truncated) AS violations_complete_ranking,
       count(*) FILTER (WHERE NOT in_ranking AND truncated) AS missing_in_truncated_ranking,
       count(DISTINCT id) FILTER (WHERE NOT in_ranking AND NOT truncated) AS rows_with_violation
FROM checked;

-- [B.2] Calls to names that were never candidates, by class and agent (all rows, any outcome). keep: tools the
-- projection always keeps. deferred_global and deferred_locked: deferred tools (unlocked or locked at the
-- start of the run). other: names not in the list the ranking saw (added later by the host or the SDK, or the
-- replaced names _unknown and _overflow).
SELECT c.value ->> 'class' AS tool_class, o.agent_key,
       count(DISTINCT o.id) AS runs,
       count(*) AS run_name_pairs,
       sum((c.value ->> 'n')::bigint) AS calls
FROM tool_rag_observations o
CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
WHERE o.ts >= :'t0' AND c.value ->> 'class' <> 'candidate'
GROUP BY c.value ->> 'class', o.agent_key
ORDER BY (c.value ->> 'class') COLLATE "C", o.agent_key COLLATE "C";

-- [B.3] The names behind B.2, most runs first (first 25). Printed as stored.
SELECT c.key AS tool_name, c.value ->> 'class' AS tool_class, o.agent_key,
       count(*) AS runs,
       sum((c.value ->> 'n')::bigint) AS calls
FROM tool_rag_observations o
CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
WHERE o.ts >= :'t0' AND c.value ->> 'class' <> 'candidate'
GROUP BY c.key, c.value ->> 'class', o.agent_key
ORDER BY count(*) DESC, sum((c.value ->> 'n')::bigint) DESC, c.key COLLATE "C",
         (c.value ->> 'class') COLLATE "C", o.agent_key COLLATE "C"
LIMIT 25;

-- C. Locked versus unlockable tools. Deferred tools are never ranked: Tool RAG has nothing to do for them.

-- [C.1] Deferred surface per agent (the row "all" is the total). locked_at_start: deferred names not unlocked
-- when the run began. unlocked_during_run: of those, names unlocked at the end; on a resumed session this
-- includes unlocks replayed from the history. rankable_share_pct: candidates / (candidates + deferred) summed
-- over the rows: how much of the surface Tool RAG could act on at all.
SELECT coalesce(agent_key, 'all') AS agent_key,
       count(*) AS n_rows,
       count(*) FILTER (WHERE deferred_count > 0) AS rows_with_deferred,
       sum(deferred_count) AS sum_deferred,
       max(deferred_count) AS max_deferred,
       count(*) FILTER (WHERE locked_at_start > 0) AS rows_with_locked,
       sum(locked_at_start) AS sum_locked,
       count(*) FILTER (WHERE unlocked_during_run > 0) AS rows_with_unlocked,
       sum(unlocked_during_run) AS sum_unlocked,
       sum(candidate_count) AS sum_candidates,
       round(100.0 * sum(candidate_count) / nullif(sum(candidate_count) + sum(deferred_count), 0), 1) AS rankable_share_pct
FROM tool_rag_observations
WHERE ts >= :'t0'
GROUP BY ROLLUP (agent_key)
ORDER BY grouping(agent_key), agent_key;

-- [C.2] Calls to deferred tools per agent: rows that had deferred tools, rows that called one, and the calls
-- by class (deferred_global: unlocked at the start, deferred_locked: locked at the start of the run).
SELECT o.agent_key,
       count(*) FILTER (WHERE o.deferred_count > 0) AS rows_with_deferred,
       count(*) FILTER (WHERE d.global_calls + d.locked_calls > 0) AS rows_calling_deferred,
       coalesce(sum(d.global_calls), 0) AS calls_deferred_global,
       coalesce(sum(d.locked_calls), 0) AS calls_deferred_locked
FROM tool_rag_observations o
CROSS JOIN LATERAL (
  SELECT coalesce(sum((c.value ->> 'n')::bigint) FILTER (WHERE c.value ->> 'class' = 'deferred_global'), 0) AS global_calls,
         coalesce(sum((c.value ->> 'n')::bigint) FILTER (WHERE c.value ->> 'class' = 'deferred_locked'), 0) AS locked_calls
  FROM jsonb_each(o.called) AS c(key, value)
) d
WHERE o.ts >= :'t0'
GROUP BY o.agent_key
ORDER BY o.agent_key COLLATE "C";

-- [C.3] Bypass check for the call counter. Calls to deferred tools reach the counter only if the SDK's
-- ExecuteExtraTool calls the wrapped tool; a bypass would read as zero deferred calls. The stored data can
-- support only an indicator, per run key (task, agent, intent; the planner has intent 0 and its rounds cannot
-- be told apart, so a planner key pools the rounds of one task). For each key: deferred calls recorded (sum over
-- its rows) against llm_usage.tool_bytes, where the key ExecuteExtraTool with result bytes above zero in any
-- request of the key means a result of that tool was in the conversation.
-- ee_result_deferred_calls_zero is the suspect column. It is NOT proof of a bypass: a call the SDK refused for a
-- locked tool never reaches the wrapper and still leaves a result, and result bytes stay in the history of later
-- requests. It cannot be answered here: which tool an ExecuteExtraTool call named, how many calls there were
-- (tool_bytes holds sizes, not counts, and tool_usage does not record deferred tools), and runs without
-- llm_usage rows (runs_with_llm_rows counts the keys that have some). deferred_calls_positive_no_ee_result
-- should be zero: calls were counted while no ExecuteExtraTool result was seen.
WITH run_calls AS (
  SELECT o.task_id, o.agent_key, coalesce(o.intent_id, 0) AS intent_id,
         max(o.deferred_count) AS max_deferred,
         sum((SELECT coalesce(sum((c.value ->> 'n')::bigint), 0)
              FROM jsonb_each(o.called) AS c(key, value)
              WHERE c.value ->> 'class' IN ('deferred_global', 'deferred_locked'))) AS deferred_calls
  FROM tool_rag_observations o
  WHERE o.ts >= :'t0'
  GROUP BY o.task_id, o.agent_key, coalesce(o.intent_id, 0)
), run_llm AS (
  SELECT u.task_id, u.worker AS agent_key, u.intent_id,
         bool_or(u.tool_bytes ? 'ExecuteExtraTool') AS ee_listed,
         bool_or(coalesce((u.tool_bytes #>> '{ExecuteExtraTool,result}')::bigint, 0) > 0) AS ee_result
  FROM llm_usage u
  WHERE u.ts >= :'t0' AND u.worker IN ('worker', 'planner')
  GROUP BY u.task_id, u.worker, u.intent_id
)
SELECT r.agent_key,
       count(*) AS observed_runs,
       count(l.task_id) AS runs_with_llm_rows,
       count(*) FILTER (WHERE l.ee_listed) AS runs_ee_listed,
       count(*) FILTER (WHERE l.ee_result) AS runs_ee_result,
       count(*) FILTER (WHERE l.ee_result AND r.deferred_calls = 0) AS ee_result_deferred_calls_zero,
       count(*) FILTER (WHERE l.ee_result AND r.deferred_calls > 0) AS ee_result_deferred_calls_positive,
       count(*) FILTER (WHERE l.task_id IS NOT NULL AND NOT l.ee_result AND r.deferred_calls > 0)
         AS deferred_calls_positive_no_ee_result,
       count(*) FILTER (WHERE l.ee_result AND r.max_deferred = 0) AS ee_result_no_deferred_listed
FROM run_calls r
LEFT JOIN run_llm l ON l.task_id = r.task_id::text AND l.agent_key = r.agent_key AND l.intent_id = r.intent_id
GROUP BY r.agent_key
ORDER BY r.agent_key COLLATE "C";

-- [C.4] Cross-check of the call counter against tool_usage for catalog tools (planner and worker rows with a
-- task id), per (task, agent, intent, tool). tool_usage records a call of a catalog tool when it starts; the
-- observation counts every call that reaches the guard, so for the same tool the two should be equal.
-- called_lower_than_tool_usage means the counter missed calls (or the row was lost, or a run straddles t0:
-- tool_usage is stamped at the call, the observation at the end of the run). tool_usage does not record
-- keep-set SDK tools, Skill or deferred tools, so those cannot be compared.
WITH obs_runs AS (
  SELECT DISTINCT task_id, agent_key, coalesce(intent_id, 0) AS intent_id
  FROM tool_rag_observations
  WHERE ts >= :'t0'
), obs_calls AS (
  SELECT o.task_id, o.agent_key, coalesce(o.intent_id, 0) AS intent_id, c.key AS tool_key,
         sum((c.value ->> 'n')::bigint) AS n
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0'
  GROUP BY o.task_id, o.agent_key, coalesce(o.intent_id, 0), c.key
), tu AS (
  SELECT task_id, agent_key, coalesce(intent_id, 0) AS intent_id, tool_key, count(*) AS n
  FROM tool_usage
  WHERE ts >= :'t0' AND task_id IS NOT NULL AND agent_key IN ('planner', 'worker')
  GROUP BY task_id, agent_key, coalesce(intent_id, 0), tool_key
)
SELECT u.agent_key,
       count(*) AS tool_usage_pairs,
       count(*) FILTER (WHERE r.task_id IS NULL) AS run_without_observation,
       count(*) FILTER (WHERE r.task_id IS NOT NULL AND c.n IS NULL) AS tool_not_in_called,
       count(*) FILTER (WHERE c.n = u.n) AS equal,
       count(*) FILTER (WHERE c.n < u.n) AS called_lower_than_tool_usage,
       count(*) FILTER (WHERE c.n > u.n) AS called_higher_than_tool_usage
FROM tu u
LEFT JOIN obs_runs r ON r.task_id = u.task_id AND r.agent_key = u.agent_key AND r.intent_id = u.intent_id
LEFT JOIN obs_calls c ON c.task_id = u.task_id AND c.agent_key = u.agent_key AND c.intent_id = u.intent_id
                     AND c.tool_key = u.tool_key
GROUP BY u.agent_key
ORDER BY u.agent_key COLLATE "C";

-- D. Fallback: what upstream's projection would have done instead of filtering. The log lines of the writer
-- ([tool-rag] drops and failures) are not in this report: the day-14 query in docs/tool-rag-observation.md
-- reads them.

-- [D.1] Outcomes other than projected, per agent, with the share of the agent's rows.
SELECT agent_key, outcome, n_rows, pct_of_agent_rows
FROM (
  SELECT agent_key, outcome, count(*) AS n_rows,
         round(100.0 * count(*) / sum(count(*)) OVER (PARTITION BY agent_key), 1) AS pct_of_agent_rows
  FROM tool_rag_observations
  WHERE ts >= :'t0'
  GROUP BY agent_key, outcome
) s
WHERE outcome <> 'projected'
ORDER BY agent_key COLLATE "C", outcome COLLATE "C";

-- [D.2] Catalog size per agent: host_list_count (keep plus candidates) and candidate_count, minimum, median
-- (the lower median, percentile_disc 0.5) and maximum, over all rows. Is 20 the right small-catalog threshold:
-- the threshold counts the deferred tools too, which host_list_count does not.
SELECT agent_key, count(*) AS n_rows,
       min(host_list_count) AS host_min,
       percentile_disc(0.5) WITHIN GROUP (ORDER BY host_list_count) AS host_p50,
       max(host_list_count) AS host_max,
       min(candidate_count) AS cand_min,
       percentile_disc(0.5) WITHIN GROUP (ORDER BY candidate_count) AS cand_p50,
       max(candidate_count) AS cand_max
FROM tool_rag_observations
WHERE ts >= :'t0'
GROUP BY agent_key
ORDER BY agent_key COLLATE "C";

-- Verdict inputs.

-- [V.1] The numbers that decide whether a filter mode is worth building. One line each, text values, at the
-- recorded k. would_hide_pct_of_projected near zero: stop here. hiding_miss_pct: of the runs where something
-- would be hidden and a ranked candidate was called, the share that called one outside the top k (A). A
-- worker_coverage_pct below 95 means the rows are not a safe sample (0.5). rankable_share_pct is C.1's total.
WITH flat AS (
  SELECT o.id, c.value ->> 'class' AS tool_class, array_position(o.ranking, c.key) AS pos
  FROM tool_rag_observations o
  CROSS JOIN LATERAL jsonb_each(o.called) AS c(key, value)
  WHERE o.ts >= :'t0'
), hiding_runs AS (
  SELECT o.id, o.k, max(f.pos) AS max_pos
  FROM tool_rag_observations o
  LEFT JOIN flat f ON f.id = o.id AND f.tool_class = 'candidate'
  WHERE o.ts >= :'t0' AND o.outcome = 'projected' AND o.would_hide_count > 0
  GROUP BY o.id, o.k
), obs AS (
  SELECT count(*) AS n_rows,
         count(*) FILTER (WHERE outcome = 'projected') AS projected,
         count(*) FILTER (WHERE outcome = 'projected' AND would_hide_count > 0) AS hiding,
         count(*) FILTER (WHERE outcome IN ('projected', 'no_signal')) AS large,
         count(*) FILTER (WHERE outcome = 'no_signal') AS no_signal,
         count(*) FILTER (WHERE outcome <> 'projected') AS fallback,
         coalesce(max(candidate_count), 0) AS max_candidates,
         coalesce(sum(candidate_count), 0) AS sum_candidates,
         coalesce(sum(deferred_count), 0) AS sum_deferred
  FROM tool_rag_observations
  WHERE ts >= :'t0'
), miss AS (
  SELECT count(max_pos) AS calling, count(*) FILTER (WHERE max_pos > k) AS missed
  FROM hiding_runs
), calls AS (
  SELECT DISTINCT task_id, intent_id
  FROM llm_usage
  WHERE worker = 'worker' AND intent_id <> 0 AND ts >= :'t0'
), cov AS (
  SELECT count(*) AS with_calls,
         count(*) FILTER (WHERE EXISTS (SELECT 1 FROM tool_rag_observations o
                                         WHERE o.agent_key = 'worker' AND o.ts >= :'t0'
                                           AND o.task_id::text = c.task_id AND o.intent_id = c.intent_id)) AS observed
  FROM calls c
), s AS (
  SELECT obs.*, miss.calling, miss.missed, cov.with_calls, cov.observed
  FROM obs CROSS JOIN miss CROSS JOIN cov
)
SELECT v.input, v.value
FROM s
CROSS JOIN LATERAL (VALUES
  (1, 'observed_rows', s.n_rows::text),
  (2, 'projected_rows', s.projected::text),
  (3, 'rows_would_hide', s.hiding::text),
  (4, 'would_hide_pct_of_projected', round(100.0 * s.hiding / nullif(s.projected, 0), 1)::text),
  (5, 'max_candidate_count', s.max_candidates::text),
  (6, 'hiding_runs_calling_ranked_candidate', s.calling::text),
  (7, 'hiding_runs_with_a_miss', s.missed::text),
  (8, 'hiding_miss_pct', round(100.0 * s.missed / nullif(s.calling, 0), 1)::text),
  (9, 'no_signal_pct_of_large_catalog_rows', round(100.0 * s.no_signal / nullif(s.large, 0), 1)::text),
  (10, 'fallback_pct_of_rows', round(100.0 * s.fallback / nullif(s.n_rows, 0), 1)::text),
  (11, 'rankable_share_pct', round(100.0 * s.sum_candidates / nullif(s.sum_candidates + s.sum_deferred, 0), 1)::text),
  (12, 'worker_coverage_pct', round(100.0 * s.observed / nullif(s.with_calls, 0), 1)::text)
) AS v(ord, input, value)
ORDER BY v.ord;

ROLLBACK;

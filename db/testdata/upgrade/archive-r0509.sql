-- One archivable task with rows in every archived table (the 23 tables of the snapshot
-- list in db/task_archives.go, minus finding_comments, which the r0509 shape lacks),
-- for the archive compatibility test (docs/phase-d2-plan.md AC8). Synthetic. Loaded
-- after the r0509 schema AND after the Go DDL steps (llm_records, llm_usage exist).
-- Text columns carry the row canary. Ids are explicit (701...), sequences are set
-- after them. Task 702 is a live task that task 701 inherits from.

INSERT INTO companies(id, name, nkey) VALUES (701, 'CANARY-D2-ROW company', 'canary-d2-row-company');
INSERT INTO task_categories(id, name, nkey) VALUES (701, 'CANARY-D2-ROW category', 'canary-d2-row-category');
INSERT INTO llm_profiles(id, name, format, model) VALUES (701, 'CANARY-D2-ROW profile', 'openai', 'canary-d2-row-model');

INSERT INTO explorations(id, description, goal, status, created_at, updated_at) VALUES
  (701, 'CANARY-D2-ROW exploration', 'CANARY-D2-ROW goal', 'open', '2026-01-02 03:04:05.123456+00', '2026-01-02 03:04:06.654321+00'),
  (702, 'CANARY-D2-ROW source exploration', 'CANARY-D2-ROW source goal', 'achieved', '2026-01-01 00:00:00+00', '2026-01-01 00:00:01+00');

INSERT INTO tasks(id, name, category_id, description, goal, exploration_id, status, paused, queue_mode,
                  llm_profile_id, active_llm_profile_id, llm_chain_revision, llm_rotate, company_id, parent_ref,
                  timeout_seconds, plan_heartbeat_seconds, plan_quiet_mode, coverage_enabled,
                  first_run_at, deadline_at, created_at, updated_at) VALUES
  (701, 'CANARY-D2-ROW task', 701, 'CANARY-D2-ROW description', 'CANARY-D2-ROW goal', 701, 'paused', true, '',
   701, 701, 3, true, 701, 'CANARY-D2-ROW parent',
   3600, 120, false, true,
   '2026-01-02 03:04:07.111111+00', '2026-01-03 00:00:00+00', '2026-01-02 03:04:05.123456+00', '2026-01-02 03:04:08.222222+00'),
  (702, 'CANARY-D2-ROW source task', NULL, 'CANARY-D2-ROW source description', 'CANARY-D2-ROW source goal', 702, 'done', false, '',
   NULL, NULL, 0, false, NULL, NULL,
   0, 300, true, true,
   NULL, NULL, '2026-01-01 00:00:00+00', '2026-01-01 00:00:01+00');

INSERT INTO task_relations(task_id, source_task_id, created_at) VALUES (701, 702, '2026-01-02 03:04:09.333333+00');

INSERT INTO exploration_nodes(id, exploration_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, created_at, updated_at, completed_at) VALUES
  (701, 701, 'begin',   '{"note": "CANARY-D2-ROW begin"}', 0, 'open', 'system', NULL, NULL, NULL, '2026-01-02 03:04:05+00', '2026-01-02 03:04:05+00', NULL),
  (702, 701, 'fact',    '{"summary": "CANARY-D2-ROW fact", "asset_ids": [701]}', 1, 'confirmed', 'worker', 'worker-1', NULL, NULL, '2026-01-02 03:04:10.444444+00', '2026-01-02 03:04:11.555555+00', NULL),
  (703, 701, 'intent',  '{"title": "CANARY-D2-ROW intent"}', 5, 'open', 'planner', NULL, NULL, NULL, '2026-01-02 03:04:12+00', '2026-01-02 03:04:12+00', NULL),
  (704, 701, 'finding', '{"summary": "CANARY-D2-ROW finding node"}', 2, 'confirmed', 'worker', NULL, NULL, NULL, '2026-01-02 03:04:13+00', '2026-01-02 03:04:13+00', '2026-01-02 03:04:14+00');
INSERT INTO exploration_edges(exploration_id, src_id, dst_id, rel, created_at) VALUES
  (701, 701, 703, 'spawns', '2026-01-02 03:04:15.666666+00'),
  (701, 703, 702, 'yields', '2026-01-02 03:04:16+00');

-- The asset belongs to this task only and has no company: it is exclusive and is
-- removed by compaction and re-created by restore under its own id.
INSERT INTO assets(id, type, domain, root_domain, task_ids, bound_domains, technologies, created_at, last_seen, updated_at) VALUES
  (701, 'root_domain', 'canary-d2-row.example', 'canary-d2-row.example', ARRAY[701]::bigint[],
   ARRAY['CANARY-D2-ROW.bound']::text[], ARRAY['CANARY-D2-ROW tech']::text[],
   '2026-01-02 03:04:17.777777+00', '2026-01-02 03:04:18.888888+00', '2026-01-02 03:04:19.999999+00');
INSERT INTO exploration_anchors(node_id, asset_id) VALUES (702, 701);
-- trg_assets_task_links created the (701, 701) link with source 'system'; give it provenance.
UPDATE task_asset_links SET source = 'worker', source_summary = 'CANARY-D2-ROW link', source_node_id = 702,
       created_at = '2026-01-02 03:04:20+00', updated_at = '2026-01-02 03:04:21+00'
 WHERE task_id = 701 AND asset_id = 701;

INSERT INTO task_constraints(id, exploration_id, kind, text, origin, created_at, updated_at) VALUES
  (701, 701, 'allow', 'CANARY-D2-ROW allow', 'operator', '2026-01-02 03:04:22+00', '2026-01-02 03:04:22+00'),
  (702, 701, 'deny',  'CANARY-D2-ROW deny',  'operator', '2026-01-02 03:04:23+00', '2026-01-02 03:04:23+00');
INSERT INTO activity(id, exploration_id, node_id, worker, kind, tool, tool_use_id, is_error, summary, detail, metadata,
                     input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, created_at) VALUES
  (701, 701, 702, 'worker-1', 'tool', 'CANARY-D2-ROW tool', 'toolu_canary', false, 'CANARY-D2-ROW summary', 'CANARY-D2-ROW detail',
   '{"k": "CANARY-D2-ROW"}', 11, 7, 3, 1, '2026-01-02 03:04:24.123456+00');

INSERT INTO task_llm_profiles(task_id, profile_id, position, status, last_error, exhausted_at, created_at, updated_at) VALUES
  (701, 701, 0, 'quota_exhausted', 'CANARY-D2-ROW balance exhausted', '2026-01-02 02:00:00+00', '2026-01-02 01:00:00+00', '2026-01-02 02:00:00+00');
INSERT INTO task_scope(id, task_id, kind, company_id, domain, net, value, source, reason, created_at) VALUES
  (701, 701, 'company',     701, NULL, NULL, NULL, 'manual', 'CANARY-D2-ROW company scope', '2026-01-02 03:04:25+00'),
  (702, 701, 'root_domain', NULL, 'canary-d2-row.example', NULL, NULL, 'auto', NULL, '2026-01-02 03:04:26+00'),
  (703, 701, 'cidr',        NULL, NULL, '192.0.2.0/24', NULL, 'agent', 'CANARY-D2-ROW cidr', '2026-01-02 03:04:27+00'),
  (704, 701, 'icp',         NULL, NULL, NULL, 'CANARY-D2-ROW icp', 'manual', NULL, '2026-01-02 03:04:28+00'),
  (705, 701, 'keyword',     NULL, NULL, NULL, 'CANARY-D2-ROW keyword', 'manual', NULL, '2026-01-02 03:04:29+00');

INSERT INTO findings(id, task_id, node_id, vulnclass, name, severity, summary, evidence, worker, asset_ids, status, report, created_at) VALUES
  (701, 701, 704, 'CANARY-D2-ROW class', 'CANARY-D2-ROW finding', 'high', 'CANARY-D2-ROW summary', 'CANARY-D2-ROW evidence', 'worker-1',
   '[701]', 'confirmed', 'CANARY-D2-ROW report', '2026-01-02 03:04:30.121212+00');
-- The snapshot id is the sha256 of its metadata (TrafficSnapshotID in db/finding_traffic.go);
-- restore re-checks it, so the value below is computed, not chosen.
INSERT INTO traffic_evidence_snapshots(id, source_traffic_id, captured_at, url, method, status, content_type,
                                       req_head, resp_head, req_hash, resp_hash, req_len, resp_len, unreferenced_at, created_at) VALUES
  ('d57857cf83632ceae74fe6c61e2b5131f910e178d4cb1c13084c5a48f88da559', 'canary-d2-row-traffic', 1767323070000, 'https://canary-d2-row.example/', 'GET', 200, 'text/html',
   'CANARY-D2-ROW req head', 'CANARY-D2-ROW resp head', 'canary-d2-row-req-hash', 'canary-d2-row-resp-hash', 120, 4096, NULL, '2026-01-02 03:04:31+00');
INSERT INTO finding_traffic_bindings(id, finding_id, snapshot_id, role, note, position, created_at) VALUES
  (701, 701, 'd57857cf83632ceae74fe6c61e2b5131f910e178d4cb1c13084c5a48f88da559', 'proof', 'CANARY-D2-ROW note', 0, '2026-01-02 03:04:32+00');

-- llm_records and llm_usage are created by the Go DDL steps, not by schema.sql.
INSERT INTO llm_records(id, ts, model, profile_name, session_id, task_id, worker, latency_ms, input_tokens, output_tokens,
                        cache_read, cache_write, status, error, request_body, response_body, raw_request, raw_response) VALUES
  (701, '2026-01-02 03:04:33.131313+00', 'canary-d2-row-model', 'CANARY-D2-ROW profile', 'canary-d2-row-session', '701', 'worker-1',
   250, 11, 7, 3, 1, 'ok', NULL, 'CANARY-D2-ROW request body', 'CANARY-D2-ROW response body', 'CANARY-D2-ROW raw request', 'CANARY-D2-ROW raw response');
INSERT INTO llm_usage(id, ts, task_id, exploration_id, worker, model, profile_name, latency_ms, input_tokens, output_tokens,
                      cache_read, cache_write, status, provider, intent_id, attempt, reason, request_bytes, system_bytes,
                      message_bytes, tool_schema_bytes, tool_result_bytes, size_known, call_id, round_no, purpose, tool_bytes) VALUES
  (701, '2026-01-02 03:04:34.141414+00', '701', 701, 'worker-1', 'canary-d2-row-model', 'CANARY-D2-ROW profile', 250, 11, 7,
   3, 1, 'ok', 'canary-d2-row-provider', 703, 1, 'CANARY-D2-ROW reason', 1000, 100, 800, 50, 50, true, 'canary-d2-row-call', 2,
   'CANARY-D2-ROW purpose', '{"k": 1}');
INSERT INTO skill_usage(id, ts, skill, agent_key, task_id, exploration_id, intent_id, session_id, args_len, found) VALUES
  (701, '2026-01-02 03:04:35+00', 'canary-d2-row-skill', 'worker', 701, 701, 703, 'canary-d2-row-session', 12, true);
INSERT INTO tool_usage(id, ts, tool_key, agent_key, task_id, exploration_id, intent_id, session_id) VALUES
  (701, '2026-01-02 03:04:36+00', 'canary-d2-row-tool', 'worker', 701, 701, 703, 'canary-d2-row-session');

-- decision_source is set as a 0.5.9m server wrote it (the column is in the r0509 shape);
-- an empty value would be rewritten by the restore normaliser and not round-trip.
INSERT INTO intercept_pending(id, rule_id, conversation_id, task_id, agent_name, tool_name, tool_input, status, reason, decided_at,
                              created_at, audit, decision_source) VALUES
  (701, NULL, NULL, '701', 'worker', 'canary-d2-row-tool', '{"k": "CANARY-D2-ROW"}', 'denied', '[Model] CANARY-D2-ROW reason',
   '2026-01-02 03:04:38+00', '2026-01-02 03:04:37+00', '{"effective_action": "deny", "execution_status": "not_executed"}', 'model');

INSERT INTO side_question_sessions(session_key, conversation_id, task_id, exploration_id, intent_id, run_id, version, snapshot, generation) VALUES
  ('canary-d2-row-session', NULL, 701, 701, 703, 1, 1, '{"k": "CANARY-D2-ROW"}', 0);
INSERT INTO side_question_requests(id, session_key, generation, client_id, question, answer, status, error, model, snapshot_at, created_at, sequence, usage) VALUES
  ('canary-d2-row-request', 'canary-d2-row-session', 0, 'canary-d2-row-client', 'CANARY-D2-ROW question', 'CANARY-D2-ROW answer', 'completed', '',
   '{"name": "CANARY-D2-ROW"}', '2026-01-02 03:04:39+00', '2026-01-02 03:04:40+00', 1, '{"input": 11}');

-- Sequences follow the explicit ids.
SELECT setval(pg_get_serial_sequence('companies', 'id'), 701);
SELECT setval(pg_get_serial_sequence('task_categories', 'id'), 701);
SELECT setval(pg_get_serial_sequence('llm_profiles', 'id'), 701);
SELECT setval(pg_get_serial_sequence('explorations', 'id'), 702);
SELECT setval(pg_get_serial_sequence('tasks', 'id'), 702);
SELECT setval(pg_get_serial_sequence('exploration_nodes', 'id'), 704);
SELECT setval(pg_get_serial_sequence('assets', 'id'), 701);
SELECT setval(pg_get_serial_sequence('task_constraints', 'id'), 702);
SELECT setval(pg_get_serial_sequence('activity', 'id'), 701);
SELECT setval(pg_get_serial_sequence('task_scope', 'id'), 705);
SELECT setval(pg_get_serial_sequence('findings', 'id'), 701);
SELECT setval(pg_get_serial_sequence('finding_traffic_bindings', 'id'), 701);
SELECT setval(pg_get_serial_sequence('llm_records', 'id'), 701);
SELECT setval(pg_get_serial_sequence('llm_usage', 'id'), 701);
SELECT setval(pg_get_serial_sequence('skill_usage', 'id'), 701);
SELECT setval(pg_get_serial_sequence('tool_usage', 'id'), 701);
SELECT setval(pg_get_serial_sequence('intercept_pending', 'id'), 701);
SELECT setval(pg_get_serial_sequence('side_question_requests', 'ordinal'), 1);

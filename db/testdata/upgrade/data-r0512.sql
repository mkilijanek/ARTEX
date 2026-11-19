-- Synthetic data for shape r0512 (release 0.5.12m and main), loaded by db/upgrade_matrix_test.go
-- after schema-7996d35963a0.sql and goddl-4bfc9dbe09c3.sql, in one implicit transaction.
-- Same rows as data-r0509.sql plus finding_comments (a table the r0509 DDL does not have).
-- docs/phase-d2-plan.md section 5.1. Every text column of a row that startup does not touch
-- carries CANARY-D2-ROW (AC12): the string must never reach a log line or a failure message.
--
-- The rows are what a database looks like after a started release: the six built-in agents,
-- their variable catalog (with the three leftover names that db/db.go:280 deletes), the
-- browser MCP row, the one-time seed flags of db.Open (db/db.go:286-290, 331-352, 387-571,
-- 582-620, 640-654) and the server seeders (flags-v0.5.12m.txt), the built-in skill visibility.
-- The schema fixture itself already inserted the ScopeSentry row (mcp_servers id 1).
--
-- Order: parents before children. The circular key agents.current_prompt_id is filled
-- after agent_prompts. session_replication_role is not used: the row "asset lists a task but
-- the link is missing" is made by inserting the asset (the trigger creates the link) and
-- deleting the link afterwards.

-- ---------------------------------------------------------------- companies
INSERT INTO companies (id, name, nkey, logo) VALUES
 (1, 'CANARY-D2-ROW company one', 'canary-d2-row-company-one', 'CANARY-D2-ROW-logo'),
 (2, 'CANARY-D2-ROW company two', 'canary-d2-row-company-two', NULL);

-- ------------------------------------------------------------- llm_profiles
-- Boundary rows of the re-added CHECKs: format 'ollama', max_tokens_field
-- 'max_completion_tokens', retry attempts -1 (the floor), endpoint_mode 'fixed'.
INSERT INTO llm_profiles (id, name, format, base_url, proxy, model, api_key, api_key_hint,
    rate_per_second, rate_per_minute, context_window_k, reasoning_effort, thinking_type,
    is_default, priority, pool_exclude, streaming, max_tokens, max_tokens_field,
    session_header_key, user_agent, endpoint_mode,
    retry_connect_attempts, retry_connect_interval_ms, retry_empty_attempts,
    retry_empty_interval_ms, retry_stream_attempts, retry_stream_interval_ms,
    price_input_per_m, price_output_per_m, price_cache_read_per_m, price_cache_write_per_m) VALUES
 (1, 'CANARY-D2-ROW profile openai', 'openai', 'https://llm.invalid/v1', 'http://proxy.invalid:3128',
    'canary-model-1', 'CANARY-D2-ROW-api-key', 'CANARY-D2-ROW-hint',
    1.5, 60, 128, 'high', 'enabled',
    true, 10, false, true, 4096, 'max_completion_tokens',
    'X-CANARY-D2-ROW', 'CANARY-D2-ROW-agent', 'fixed',
    -1, 0, 2, 500, 3, 1000,
    1.250000, 5.000000, 0.125000, 1.500000),
 (2, 'CANARY-D2-ROW profile ollama', 'ollama', 'http://ollama.invalid:11434', NULL,
    'canary-model-2', NULL, NULL,
    0, 0, 0, '', '',
    false, 0, true, false, 0, '',
    '', '', 'base',
    0, 0, 0, 0, 0, 0,
    NULL, NULL, NULL, NULL),
 (3, 'CANARY-D2-ROW profile responses', 'openai-responses', NULL, NULL,
    'canary-model-3', 'CANARY-D2-ROW-api-key-3', NULL,
    0, 0, 0, 'low', 'disabled',
    false, 5, false, true, 0, '',
    '', '', '',
    0, 0, 0, 0, 0, 0,
    NULL, NULL, NULL, NULL);

INSERT INTO llm_profile_health (profile_id, fails, trips, open_until, last_error) VALUES
 (1, 2, 1, NULL, 'CANARY-D2-ROW last error');

-- ---------------------------------------------------------- task_categories
INSERT INTO task_categories (id, name, nkey) VALUES
 (1, 'CANARY-D2-ROW category one', 'canary-d2-row-category-one'),
 (2, 'CANARY-D2-ROW category two', 'canary-d2-row-category-two');

-- ------------------------------------------------------------- explorations
INSERT INTO explorations (id, description, goal, status, round_no) VALUES
 (1, 'CANARY-D2-ROW exploration one', 'CANARY-D2-ROW goal one', 'open', 3),
 (2, 'CANARY-D2-ROW exploration two', 'CANARY-D2-ROW goal two', 'achieved', 1),
 (3, 'CANARY-D2-ROW exploration three', 'CANARY-D2-ROW goal three', 'failed', 0),
 (4, 'CANARY-D2-ROW exploration four', 'CANARY-D2-ROW goal four', 'open', 0),
 (5, 'CANARY-D2-ROW exploration five', 'CANARY-D2-ROW goal five', 'open', 0);

-- -------------------------------------------------------------------- tasks
-- t1 pinned profile with a chain, t2 plain, t3 archived, t4 pinned profile WITHOUT a chain
-- (db/schema.sql:745-755 backfills a one-entry chain and sets active_llm_profile_id),
-- t5 soft-deleted (the link backfill ignores it).
INSERT INTO tasks (id, name, category_id, description, goal, exploration_id, status, paused, queued,
    queued_at, queue_mode, llm_profile_id, active_llm_profile_id, llm_chain_revision, llm_rotate,
    company_id, parent_ref, timeout_seconds, plan_heartbeat_seconds, plan_quiet_mode,
    coverage_enabled, pinned_at, first_run_at, deadline_at, archived_at, deleted_at,
    completed_at, paused_at, updated_at) VALUES
 (1, 'CANARY-D2-ROW task one', 1, 'CANARY-D2-ROW description one', 'CANARY-D2-ROW goal one', 1, 'running', false, false,
    NULL, '', 1, 1, 2, true,
    1, 'CANARY-D2-ROW-ref', 3600, 300, true,
    true, '2026-01-02 03:04:05+00', '2026-01-02 03:04:06+00', '2026-01-03 03:04:05+00', NULL, NULL,
    NULL, NULL, '2026-01-01 00:00:00+00'),
 (2, 'CANARY-D2-ROW task two', 2, 'CANARY-D2-ROW description two', 'CANARY-D2-ROW goal two', 2, 'done', false, false,
    NULL, '', NULL, NULL, 0, false,
    2, NULL, 0, 300, true,
    true, NULL, '2026-01-02 03:04:05+00', NULL, NULL, NULL,
    '2026-01-02 04:04:05+00', NULL, '2026-01-01 00:00:00+00'),
 (3, 'CANARY-D2-ROW task three', NULL, 'CANARY-D2-ROW description three', 'CANARY-D2-ROW goal three', 3, 'failed', false, false,
    NULL, '', NULL, NULL, 0, false,
    NULL, NULL, 0, 300, false,
    false, NULL, NULL, NULL, '2026-02-01 00:00:00+00', NULL,
    NULL, NULL, '2026-01-01 00:00:00+00'),
 (4, 'CANARY-D2-ROW task four', 1, 'CANARY-D2-ROW description four', 'CANARY-D2-ROW goal four', 4, 'paused', true, true,
    '2026-03-01 00:00:00+00', 'resume', 2, NULL, 0, false,
    NULL, NULL, 0, 300, true,
    true, NULL, NULL, NULL, NULL, NULL,
    NULL, '2026-03-01 00:00:00+00', '2026-01-01 00:00:00+00'),
 (5, 'CANARY-D2-ROW task five', NULL, 'CANARY-D2-ROW description five', 'CANARY-D2-ROW goal five', 5, 'timeout', false, false,
    NULL, '', NULL, NULL, 0, false,
    NULL, NULL, 60, 300, true,
    true, NULL, NULL, NULL, NULL, '2026-02-02 00:00:00+00',
    NULL, NULL, '2026-01-01 00:00:00+00');

INSERT INTO task_llm_profiles (task_id, profile_id, position, status, last_error, exhausted_at) VALUES
 (1, 1, 0, 'ready', NULL, NULL),
 (1, 2, 1, 'quota_exhausted', 'CANARY-D2-ROW quota error', '2026-03-02 00:00:00+00');

INSERT INTO task_archives (id, task_id, state, phase, progress, error, warnings, format_version, archive_path,
    sha256, original_size, compressed_size, task_name, task_description, task_goal, original_status,
    category_id_snapshot, category_name_snapshot, source_task_ids, remaining_timeout_seconds,
    data_counts, aggregate_stats, archived_at) VALUES
 (1, 3, 'ready', 'done', 100, '', '["CANARY-D2-ROW warning"]', 3, 'CANARY-D2-ROW/archive/task-3.tar.zst',
    'canary-d2-row-sha256', 2048, 512, 'CANARY-D2-ROW task three', 'CANARY-D2-ROW description three', 'CANARY-D2-ROW goal three', 'failed',
    1, 'CANARY-D2-ROW category one', '{1}', 0,
    '{"assets": 1}', '{"tokens": 10}', '2026-02-01 00:00:00+00');

INSERT INTO task_templates (id, name, nkey, description, goal, category_id, intercept_rules) VALUES
 (1, 'CANARY-D2-ROW template', 'canary-d2-row-template', 'CANARY-D2-ROW template description', 'CANARY-D2-ROW template goal',
    1, '[{"kind":"exact_domain","pattern":"canary-d2-row.example"}]');

INSERT INTO task_relations (task_id, source_task_id) VALUES (2, 1);

-- ------------------------------------------------------------------- assets
-- Each insert fires trg_assets_task_links (db/schema.sql:692-708), which creates the link rows.
-- a1 root_domain (tasks 1 and 2), a2 ip (task 1, plus the soft-deleted task 5, which gets no
-- link), a3 subdomain (task 1; its link is deleted below), a4 http service on the scope
-- boundary company_source='scope', a5 app, a6 endpoint, a7 other service.
INSERT INTO assets (id, type, company_id, company_source, task_ids, domain, root_domain, ip, c_segment, port,
    icp, bound_domains, open_ports, record_type, record_value, bundle_id, app_name, category, app_description,
    app_icp, url, service_type, service_name, favicon_mmh3, status_code, content_length, page_title,
    technologies, auth, method, params, extra) VALUES
 (1, 'root_domain', 1, 'explicit', '{1,2}', 'canary-d2-row.example', 'canary-d2-row.example', NULL, NULL, NULL,
    'ICP-CANARY-D2-ROW', '{}', '{}', NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'CANARY-D2-ROW title',
    '{}', '{}', NULL, '{}', '{"note":"CANARY-D2-ROW"}'),
 (2, 'ip', 1, 'explicit', '{1,5}', NULL, NULL, '203.0.113.10', '203.0.113.0/24', 443,
    NULL, '{canary-d2-row.example}', ARRAY['{"port":443,"proto":"tcp","banner":"CANARY-D2-ROW"}']::jsonb[], NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
    '{}', '{}', NULL, '{}', '{}'),
 (3, 'subdomain', 1, 'explicit', '{1}', 'www.canary-d2-row.example', 'canary-d2-row.example', NULL, NULL, NULL,
    NULL, '{}', '{}', 'CNAME', '{canary-d2-row.example}', NULL, NULL, NULL, NULL,
    NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL,
    '{}', '{}', NULL, '{}', '{}'),
 (4, 'service', 2, 'scope', '{}', 'app.canary-d2-row.example', 'canary-d2-row.example', '203.0.113.10', NULL, 443,
    NULL, '{}', '{}', NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, 'https://app.canary-d2-row.example/', 'http', 'CANARY-D2-ROW http', 'CANARY-D2-ROW-mmh3', 200, 1234, 'CANARY-D2-ROW page',
    '{nginx,CANARY-D2-ROW}', ARRAY['{"scheme":"basic","user":"CANARY-D2-ROW"}']::jsonb[], NULL, '{}', '{}'),
 (5, 'app', NULL, 'explicit', '{}', NULL, NULL, NULL, NULL, NULL,
    NULL, '{}', '{}', NULL, NULL, 'org.canary.d2.row', 'CANARY-D2-ROW app', 'tools', 'CANARY-D2-ROW app description',
    'ICP-CANARY-D2-ROW-APP', NULL, NULL, NULL, NULL, NULL, NULL, NULL,
    '{}', '{}', NULL, '{}', '{}'),
 (6, 'endpoint', NULL, 'explicit', '{}', 'app.canary-d2-row.example', NULL, NULL, NULL, NULL,
    NULL, '{}', '{}', NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, 'https://app.canary-d2-row.example/api/canary', NULL, NULL, NULL, NULL, NULL, NULL,
    '{}', '{}', 'POST', ARRAY['{"name":"q","in":"query"}']::jsonb[], '{}'),
 (7, 'service', NULL, 'explicit', '{}', 'ssh.canary-d2-row.example', NULL, '203.0.113.20', NULL, 22,
    NULL, '{}', '{}', NULL, NULL, NULL, NULL, NULL, NULL,
    NULL, NULL, 'other', 'CANARY-D2-ROW ssh', NULL, NULL, NULL, NULL,
    '{}', '{}', NULL, '{}', '{}');

-- Trigger-aware rows. a2's link gets a detailed source (as the application does after an upsert);
-- a3's link is removed, so the asset lists task 1 while task_asset_links has no row for it
-- (the case db/schema.sql:712-717 re-creates).
UPDATE task_asset_links SET source = 'agent', source_summary = 'CANARY-D2-ROW link note' WHERE asset_id = 2;
DELETE FROM task_asset_links WHERE asset_id = 3;

INSERT INTO company_scope (id, company_id, kind, domain, net, value, raw, reason) VALUES
 (1, 1, 'domain',  'canary-d2-row.example', NULL, NULL, 'CANARY-D2-ROW raw domain', 'CANARY-D2-ROW reason'),
 (2, 1, 'cidr',    NULL, '203.0.113.0/24', NULL, 'CANARY-D2-ROW raw cidr', NULL),
 (3, 1, 'ip',      NULL, '203.0.113.10/32', NULL, 'CANARY-D2-ROW raw ip', NULL),
 (4, 1, 'icp',     NULL, NULL, 'ICP-CANARY-D2-ROW-001', 'CANARY-D2-ROW raw icp', NULL),
 (5, 2, 'keyword', NULL, NULL, 'CANARY-D2-ROW keyword', 'CANARY-D2-ROW raw keyword', NULL);

-- Boundary rows of the task_scope kind CHECK: keyword and icp.
INSERT INTO task_scope (id, task_id, kind, company_id, domain, net, value, source, reason) VALUES
 (1, 1, 'company',     1, NULL, NULL, NULL, 'manual', 'CANARY-D2-ROW'),
 (2, 1, 'root_domain', NULL, 'canary-d2-row.example', NULL, NULL, 'auto', NULL),
 (3, 1, 'ip',          NULL, NULL, '203.0.113.10/32', NULL, 'agent', NULL),
 (4, 1, 'icp',         NULL, NULL, NULL, 'ICP-CANARY-D2-ROW-001', 'manual', NULL),
 (5, 2, 'keyword',     NULL, NULL, NULL, 'CANARY-D2-ROW keyword', 'agent', 'CANARY-D2-ROW'),
 (6, 2, 'subdomain',   NULL, 'www.canary-d2-row.example', NULL, NULL, 'auto', NULL),
 (7, 2, 'cidr',        NULL, NULL, '203.0.113.0/24', NULL, 'manual', NULL);

-- ------------------------------------------------------- exploration graph
-- Kinds and states of every branch of ck_node_kind / ck_node_state, including 'digest',
-- 'superseded', 'paused' and 'deleted'.
INSERT INTO exploration_nodes (id, exploration_id, kind, payload, priority, state, origin, owner,
    blocked_reason, delete_reason, content_version, cold_since_round, completed_at) VALUES
 (1,  1, 'begin',   '{"text":"CANARY-D2-ROW begin"}', 0, 'open', 'system', NULL, NULL, NULL, 0, NULL, NULL),
 (2,  1, 'goal',    '{"text":"CANARY-D2-ROW goal"}', 0, 'open', 'goals', NULL, NULL, NULL, 0, NULL, NULL),
 (3,  1, 'intent',  '{"text":"CANARY-D2-ROW intent"}', 5, 'running', 'planner', 'worker-1', NULL, NULL, 1, NULL, NULL),
 (4,  1, 'fact',    '{"text":"CANARY-D2-ROW fact"}', 0, 'confirmed', 'worker', NULL, NULL, NULL, 2, 1, NULL),
 (5,  1, 'finding', '{"text":"CANARY-D2-ROW finding"}', 0, 'confirmed', 'worker', NULL, NULL, NULL, 0, NULL, NULL),
 (6,  1, 'hint',    '{"text":"CANARY-D2-ROW hint"}', 0, 'active', 'human', NULL, NULL, NULL, 0, NULL, NULL),
 (7,  1, 'digest',  '{"text":"CANARY-D2-ROW digest"}', 0, 'active', 'planner', NULL, NULL, NULL, 1, NULL, NULL),
 (8,  1, 'intent',  '{"text":"CANARY-D2-ROW paused intent"}', 1, 'paused', 'planner', NULL, 'CANARY-D2-ROW blocked', NULL, 0, NULL, NULL),
 (9,  1, 'intent',  '{"text":"CANARY-D2-ROW deleted intent"}', 1, 'deleted', 'human', NULL, NULL, 'CANARY-D2-ROW delete reason', 0, NULL, '2026-01-04 00:00:00+00'),
 (10, 1, 'digest',  '{"text":"CANARY-D2-ROW old digest"}', 0, 'superseded', 'planner', NULL, NULL, NULL, 0, NULL, NULL),
 (11, 2, 'begin',   '{"text":"CANARY-D2-ROW begin two"}', 0, 'open', 'system', NULL, NULL, NULL, 0, NULL, NULL);

INSERT INTO exploration_edges (exploration_id, src_id, dst_id, rel) VALUES
 (1, 1, 2, 'spawns'),
 (1, 2, 3, 'spawns'),
 (1, 3, 4, 'yields'),
 (1, 4, 5, 'proves'),
 (1, 4, 3, 'derived_from'),
 (1, 7, 4, 'covers');

INSERT INTO exploration_anchors (node_id, asset_id) VALUES (4, 1), (4, 2);

INSERT INTO planner_context_checkpoints (exploration_id, checkpoint) VALUES
 (1, '{"note":"CANARY-D2-ROW checkpoint"}');

INSERT INTO task_constraints (id, exploration_id, kind, text, origin) VALUES
 (1, 1, 'allow', 'CANARY-D2-ROW allow constraint', 'goals'),
 (2, 1, 'deny',  'CANARY-D2-ROW deny constraint', 'human');

INSERT INTO activity (id, exploration_id, node_id, worker, kind, tool, tool_use_id, is_error, summary, detail,
    metadata, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, main_seg) VALUES
 (1, 1, 3, 'worker-1', 'tool_use', 'canary_tool', 'toolu_canary_1', false, 'CANARY-D2-ROW summary', 'CANARY-D2-ROW detail',
    '{"note":"CANARY-D2-ROW"}', NULL, NULL, NULL, NULL, NULL),
 (2, 1, 3, 'worker-1', 'tool_result', 'canary_tool', 'toolu_canary_1', true, 'CANARY-D2-ROW result', 'CANARY-D2-ROW detail',
    '{}', NULL, NULL, NULL, NULL, NULL),
 (3, 1, NULL, 'mainagent', 'result', NULL, NULL, false, 'CANARY-D2-ROW main result', NULL,
    '{}', 100, 50, 10, 5, 1);

INSERT INTO main_sessions (exploration_id, seq) VALUES (1, 1);

-- ------------------------------------------------------------------- agents
-- The six built-in agents as db.Open left them. name and description equal what
-- db/db.go:216-232 writes, except agent 4 (worker): an operator edited both, and the
-- upsert at db/db.go:258 overwrites them on every start.
INSERT INTO agents (id, key, name, description, role, builtin, enabled, llm_profile_id, current_prompt_id,
    max_turns, run_seconds, web_search, interactive_shell, wrapup_prompt, wrapup_max_turns,
    task_timeout_wrapup_prompt, task_timeout_wrapup_max_turns, trigger_run_mode, trigger_merge_mode, trigger_max_parallel, updated_at) VALUES
 (1, 'goals', 'Goal Decomposer',
    'Breaks a penetration testing task''s goal into several independent, verifiable sub-goals.',
    'goals', true, true, NULL, NULL, 0, 1200, false, false, '', 0, '', 0, 'serial', 'all', 5, '2026-01-01 00:00:00+00'),
 (2, 'planner', 'Planner',
    'Reads the posture, judges goals, and only adds exploration intents when there is a genuinely uncovered new direction (one planning loop per task).',
    'planner', true, true, 1, NULL, 0, 1200, false, true, 'CANARY-D2-ROW wrapup', 3, 'CANARY-D2-ROW timeout wrapup', 2, 'parallel', 'by_task', 3, '2026-01-01 00:00:00+00'),
 (3, 'mainagent', 'Main',
    'Human-machine interface: observes progress and turns human intent into hints or high-priority intents.',
    'main', true, true, NULL, NULL, 0, 1200, true, true, '', 0, '', 0, 'serial', 'none', 5, '2026-01-01 00:00:00+00'),
 (4, 'worker', 'CANARY-D2-ROW renamed worker',
    'CANARY-D2-ROW operator description',
    'worker', true, true, 2, NULL, 40, 600, false, true, '', 0, '', 0, 'serial', 'all', 5, '2026-01-01 00:00:00+00'),
 (5, 'auto', 'Auto',
    'Platform operations assistant: manages tasks (create/view/pause/hint) and assets with tools, and can create/modify skills, custom tools, and MCP.',
    'assistant', true, true, NULL, NULL, 0, 1200, false, true, '', 0, '', 0, 'serial', 'all', 5, '2026-01-01 00:00:00+00'),
 (6, 'pentest', 'Penetration Testing',
    'Solo pentest agent: single-handedly walks the full chain from recon → attack surface → deep exploitation → verification → wrap-up, with its own planning, execution, and adversarial verification.',
    'assistant', true, true, NULL, NULL, 0, 0, true, true, '', 0, '', 0, 'serial', 'all', 5, '2026-01-01 00:00:00+00'),
 (7, 'canary_custom', 'CANARY-D2-ROW custom agent', 'CANARY-D2-ROW custom description',
    'worker', false, false, 3, NULL, 10, 300, true, false, 'CANARY-D2-ROW', 1, 'CANARY-D2-ROW', 1, 'serial', 'all', 2, '2026-01-01 00:00:00+00');

INSERT INTO agent_prompts (id, agent_id, version, template_text, note, updated_by) VALUES
 (1, 2, 1, 'CANARY-D2-ROW planner prompt v1', 'CANARY-D2-ROW note', 'CANARY-D2-ROW editor'),
 (2, 4, 1, 'CANARY-D2-ROW worker prompt v1', NULL, NULL),
 (3, 4, 2, 'CANARY-D2-ROW worker prompt v2 edited by the operator', 'CANARY-D2-ROW note', 'CANARY-D2-ROW editor'),
 (4, 1, 1, 'CANARY-D2-ROW goals prompt v1', NULL, NULL);
-- trg_agents_upd would set updated_at = now(); it is switched off for these three updates only,
-- so that the rows keep the fixed updated_at the expected changes state, and switched on again.
ALTER TABLE agents DISABLE TRIGGER trg_agents_upd;
UPDATE agents SET current_prompt_id = 1 WHERE id = 2;
UPDATE agents SET current_prompt_id = 3 WHERE id = 4;
UPDATE agents SET current_prompt_id = 4 WHERE id = 1;
ALTER TABLE agents ENABLE TRIGGER trg_agents_upd;

-- The catalog as db/db.go:216-232 writes it, plus the three leftovers that db/db.go:280
-- deletes: EngagementTitle and Now for goals, CoverageGaps for planner.
INSERT INTO agent_prompt_vars (id, agent_id, var_name, description, example, source) VALUES
 (1,  1, 'EngagementDescription', 'Task description (test target/background)', 'Test the example.com site', 'exploration'),
 (2,  2, 'Goal', 'Overall task goal', 'Gain admin access to example.com', 'exploration'),
 (3,  2, 'AssetSummary', 'Asset count/type distribution summary (optional)', 'domain:3 ip:5 site:2', 'distilled'),
 (4,  3, 'Goal', 'Current task goal', 'Gain admin access to example.com', 'exploration'),
 (5,  3, 'AssetSummary', 'Initial posture summary (optional)', 'domain:3 ip:5', 'distilled'),
 (6,  3, 'FindingsSummary', 'Confirmed vulnerability summary (optional)', 'high:1 medium:2', 'distilled'),
 (7,  4, 'ProxyAddr', 'Recorded proxy address (drives the if/else dual wording)', '127.0.0.1:8080', 'runtime'),
 (8,  4, 'WorkerName', 'Worker self-identifier (optional)', 'worker-1', 'runtime'),
 (9,  1, 'EngagementTitle', 'CANARY-D2-ROW renamed variable', 'CANARY-D2-ROW example', 'exploration'),
 (10, 1, 'Now', 'CANARY-D2-ROW promoted variable', 'CANARY-D2-ROW example', 'runtime'),
 (11, 2, 'CoverageGaps', 'CANARY-D2-ROW removed variable', 'CANARY-D2-ROW example', 'distilled');

-- ---------------------------------------------------------------- mcp servers
-- id 1 (ScopeSentry) came with the schema fixture; browser is what seedBuiltins inserted.
INSERT INTO mcp_servers (id, name, transport, command, args, env, url, enabled, insecure, timeout_seconds,
    last_connect_at, last_connect_ok, last_connect_err) VALUES
 (2, 'browser', 'stdio', 'npx', '["@playwright/mcp","--headless"]', '{}', NULL, false, false, 120, NULL, NULL, NULL),
 (3, 'CANARY-D2-ROW http server', 'http', NULL, '[]', '{"X-API-Key":"CANARY-D2-ROW-key"}', 'https://mcp.invalid/http', true, true, 300,
    '2026-03-04 00:00:00+00', true, NULL),
 (4, 'CANARY-D2-ROW sse server', 'sse', NULL, '[]', '{}', 'https://mcp.invalid/sse', false, false, 120,
    '2026-03-04 00:00:00+00', false, 'CANARY-D2-ROW connect error'),
 (5, 'CANARY-D2-ROW stdio server', 'stdio', 'canary-d2-row-mcp', '["--canary"]', '{}', NULL, true, false, 120, NULL, NULL, NULL);

INSERT INTO mcp_tools_cache (id, server_id, tool_name, description, schema) VALUES
 (1, 3, 'canary_tool', 'CANARY-D2-ROW tool description', '{"type":"object","properties":{"q":{"type":"string"}}}');

INSERT INTO agent_visibility (agent_id, resource_kind, resource_id, mcp_tool_name, enabled) VALUES
 (4, 'mcp', 3, '', true),
 (4, 'mcp', 3, 'canary_tool', false);

-- The built-in skill binding (db/db.go:366-381), plus one operator binding.
INSERT INTO agent_skill_visibility (agent_id, skill_name, enabled) VALUES
 (4, 'api-recon', true),
 (5, 'api-recon', true),
 (6, 'api-recon', true),
 (2, 'canary-d2-row-skill', false);

INSERT INTO skill_usage (id, skill, agent_key, task_id, exploration_id, intent_id, session_id, args_len, found) VALUES
 (1, 'api-recon', 'worker', 1, 1, 3, 'CANARY-D2-ROW session', 12, true),
 (2, 'canary-d2-row-missing', 'worker', 1, 1, 3, 'CANARY-D2-ROW session', 0, false);

INSERT INTO tool_usage (id, tool_key, agent_key, task_id, exploration_id, intent_id, session_id) VALUES
 (1, 'canary_tool', 'worker', 1, 1, 3, 'CANARY-D2-ROW session');

-- The record_fact row stands for the stored tool schema of 0.5.12m, which already has the
-- "source" property (server/orchestration.go:557-558).
INSERT INTO tools (key, system, description, schema, agents, enabled, kind, exec, deferred) VALUES
 ('record_fact', true, 'CANARY-D2-ROW operator-edited description',
    '{"type":"object","properties":{"text":{"type":"string"},"source":{"type":"string"}}}', '["worker"]', true, 'builtin', '{}', false),
 ('canary_custom_tool', false, 'CANARY-D2-ROW custom tool',
    '{"type":"object","properties":{}}', '["auto"]', true, 'custom', '{"kind":"shell","command":"echo CANARY-D2-ROW"}', true);

-- -------------------------------------------------------------- conversations
INSERT INTO conversations (id, agent_key, title, llm_profile_id, pinned_at) VALUES
 (1, 'auto', 'CANARY-D2-ROW conversation one', 1, NULL),
 (2, 'pentest', 'CANARY-D2-ROW conversation two', NULL, '2026-03-05 00:00:00+00');

INSERT INTO conversation_activities (id, conversation_id, worker, kind, tool, tool_use_id, is_error, summary, detail,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens) VALUES
 (1, 1, 'auto', 'tool_use', 'canary_tool', 'toolu_canary_2', false, 'CANARY-D2-ROW summary', 'CANARY-D2-ROW detail', 10, 20, 0, 0);

INSERT INTO agent_triggers (id, agent_key, enabled, interval_sec, on_finding, on_goal_met, on_task_timeout,
    on_tool_call, on_task_create, interval_message, finding_message, goal_message, task_timeout_message,
    tool_call_message, task_create_message, tool_names, last_fire) VALUES
 (1, 'worker', true, 60, true, false, true, true, true, 'CANARY-D2-ROW interval', 'CANARY-D2-ROW finding',
    'CANARY-D2-ROW goal', 'CANARY-D2-ROW timeout', 'CANARY-D2-ROW tool', 'CANARY-D2-ROW create', 'canary_tool',
    '2026-03-06 00:00:00+00');

INSERT INTO scheduler_state (key, value) VALUES ('canary-d2-row-key', 'CANARY-D2-ROW value');

-- ---------------------------------------------------------- interception rules
-- The one-time seeds of db/db.go:331-352 and 387-654 are flagged done below, so these rows are
-- the whole rule set: a built-in rule and an operator rule.
INSERT INTO intercept_rules (id, name, enabled, priority, match_target, match_type, pattern, action, message,
    timeout_enabled, timeout_seconds, timeout_action) VALUES
 (1, '[built-in] Shutdown / reboot', true, 100, 'tool_input', 'regex', '\b(?:shutdown|reboot)\b', 'deny',
    'Executing shutdown or reboot commands is forbidden', false, 60, 'deny'),
 (2, 'CANARY-D2-ROW operator rule', true, 5, 'tool_name', 'string', 'canary_tool', 'ask', 'CANARY-D2-ROW message',
    true, 30, 'allow');

INSERT INTO intercept_pending (id, rule_id, conversation_id, task_id, agent_name, tool_name, tool_input, status,
    reason, decided_at, audit, decision_source) VALUES
 (1, 1, 1, '1', 'CANARY-D2-ROW agent', 'canary_tool', '{"cmd":"CANARY-D2-ROW"}', 'denied', 'CANARY-D2-ROW rule message',
    '2026-03-07 00:00:00+00', '{"note":"CANARY-D2-ROW"}', 'rule'),
 (2, NULL, 1, '1', 'CANARY-D2-ROW agent', 'canary_tool', '{}', 'allowed', '[Model] CANARY-D2-ROW decision',
    '2026-03-07 00:00:00+00', NULL, 'model'),
 (3, 2, NULL, '2', 'CANARY-D2-ROW agent', 'canary_tool', '{}', 'pending', '', NULL, NULL, 'rule');

INSERT INTO asset_intercept_rules (id, enabled, kind, pattern, note, builtin) VALUES
 (1, true, 'fuzzy_domain', '.gov', '[built-in] Government websites (.gov)', true),
 (2, false, 'cidr', '198.51.100.0/24', 'CANARY-D2-ROW operator rule', false);

INSERT INTO task_intercept_rules (id, task_id, enabled, action, kind, pattern, note) VALUES
 (1, 1, true, 'block', 'exact_domain', 'blocked.canary-d2-row.example', 'CANARY-D2-ROW'),
 (2, 1, true, 'allow', 'fuzzy_url', 'canary-d2-row', 'CANARY-D2-ROW');

INSERT INTO task_credentials (id, task_id, domain, username, password, source, note) VALUES
 (1, 1, 'canary-d2-row.example', 'CANARY-D2-ROW-user', 'CANARY-D2-ROW-password', 'user', 'CANARY-D2-ROW'),
 (2, 1, 'canary-d2-row.example', 'CANARY-D2-ROW-agent-user', 'CANARY-D2-ROW-password-2', 'agent', '');

-- ------------------------------------------------------------------- findings
INSERT INTO findings (id, task_id, node_id, vulnclass, name, severity, summary, evidence, worker, asset_ids,
    status, report, evidence_version, report_evidence_version) VALUES
 (1, 1, 5, 'sqli', 'CANARY-D2-ROW finding one', 'high', 'CANARY-D2-ROW summary', 'CANARY-D2-ROW evidence', 'worker-1',
    '[1, 2]', 'confirmed', 'CANARY-D2-ROW report', 2, 1),
 (2, 2, NULL, 'xss', 'CANARY-D2-ROW finding two', 'low', 'CANARY-D2-ROW summary', '', 'worker-2',
    '[]', 'pending', '', 0, 0);

-- pending retest on finding 1, running retest on finding 2 (the partial unique index
-- allows one active retest per finding), plus a finished one.
INSERT INTO finding_retests (id, finding_id, conversation_id, status, verdict, notes, snapshot, summary,
    evidence, error, started_at, finished_at) VALUES
 (1, 1, NULL, 'pending', '', 'CANARY-D2-ROW notes', '{"note":"CANARY-D2-ROW"}', '', '', '', NULL, NULL),
 (2, 2, 1, 'running', '', 'CANARY-D2-ROW notes', '{"note":"CANARY-D2-ROW"}', '', '', '', '2026-03-08 00:00:00+00', NULL),
 (3, 1, NULL, 'completed', 'reproduced', 'CANARY-D2-ROW notes', '{"note":"CANARY-D2-ROW"}', 'CANARY-D2-ROW summary',
    'CANARY-D2-ROW evidence', '', '2026-03-08 00:00:00+00', '2026-03-08 01:00:00+00');

INSERT INTO traffic_evidence_snapshots (id, source_traffic_id, captured_at, url, method, status, content_type,
    req_head, resp_head, req_hash, resp_hash, req_len, resp_len, unreferenced_at) VALUES
 ('snap-canary-1', 'traffic-canary-1', 1772928000000, 'https://app.canary-d2-row.example/', 'GET', 200, 'text/html',
    'GET / HTTP/1.1 CANARY-D2-ROW', 'HTTP/1.1 200 OK CANARY-D2-ROW', 'canary-req-hash', 'canary-resp-hash', 100, 200, NULL);

INSERT INTO finding_traffic_bindings (id, finding_id, snapshot_id, role, note, position) VALUES
 (1, 1, 'snap-canary-1', 'supporting', 'CANARY-D2-ROW', 0);

-- Analyst comments (issue #65): the table exists in the 0.5.12m DDL only.
INSERT INTO finding_comments (id, finding_id, body, author_kind, author_subject) VALUES
 (1, 1, 'CANARY-D2-ROW comment one', 'operator', 'ARTEX'),
 (2, 1, 'CANARY-D2-ROW comment two', 'operator', 'ARTEX');

-- ------------------------------------------------------------ logs and /btw
INSERT INTO server_logs (id, level, tag, text) VALUES
 (1, 'info', 'canary', 'CANARY-D2-ROW log line'),
 (2, 'error', 'canary', 'CANARY-D2-ROW error line');

INSERT INTO side_question_sessions (session_key, conversation_id, task_id, exploration_id, intent_id, run_id,
    version, snapshot, generation, memory) VALUES
 ('canary-session-conv', 1, NULL, NULL, NULL, 1, 1, '{"note":"CANARY-D2-ROW"}', 0, '{"note":"CANARY-D2-ROW"}'),
 ('canary-session-task', NULL, 1, 1, 3, 2, 1, '{"note":"CANARY-D2-ROW"}', 1, '{}');

INSERT INTO side_question_requests (id, ordinal, session_key, generation, client_id, question, answer, status,
    error, model, snapshot_at, sequence, usage, context_info) VALUES
 ('canary-request-1', 1, 'canary-session-conv', 0, 'client-1', 'CANARY-D2-ROW question', 'CANARY-D2-ROW answer', 'completed',
    '', '{"name":"canary"}', '2026-03-09 00:00:00+00', 1, '{"in":1}', '{"note":"CANARY-D2-ROW"}'),
 ('canary-request-2', 2, 'canary-session-conv', 0, 'client-2', 'CANARY-D2-ROW question', '', 'running',
    '', '{"name":"canary"}', '2026-03-09 00:00:00+00', 2, '{}', '{}');

-- ------------------------------------------------------------ notifications
INSERT INTO notification_channels (id, name, kind, enabled, config, mode, filter, rate_per_min) VALUES
 (1, 'CANARY-D2-ROW channel', 'webhook', true, '{"url":"https://hook.invalid/CANARY-D2-ROW","headers":{}}', 'realtime',
    '{"min_severity":"high"}', 20);

INSERT INTO notification_events (id, kind, finding_id, snapshot, fanned_out) VALUES
 (1, 'finding_created', 1, '{"name":"CANARY-D2-ROW finding one"}', true);

INSERT INTO notification_deliveries (id, event_id, channel_id, state, attempts, next_attempt_at, last_error,
    batch_id, sent_at) VALUES
 (1, 1, 1, 'sent', 1, '2026-03-10 00:00:00+00', '', NULL, '2026-03-10 00:00:01+00');

-- -------------------------------------------------------------- RSI lessons
INSERT INTO rsi_lessons (id, dedupe_key, kind, cue, action, category_id, company_id, status) VALUES
 (1, 'canary-d2-row-lesson', 'caution', 'CANARY-D2-ROW cue', 'CANARY-D2-ROW action', 1, 1, 'candidate');

INSERT INTO rsi_lesson_evidence (lesson_id, source_kind, source_id, detail) VALUES
 (1, 'finding', 1, 'CANARY-D2-ROW detail');

INSERT INTO rsi_lesson_usage (lesson_id, task_id, mode) VALUES (1, 1, 'shadow');

-- ------------------------------------------- llm_records, llm_usage (Go DDL)
INSERT INTO llm_records (id, model, profile_name, session_id, task_id, worker, latency_ms, input_tokens,
    output_tokens, cache_read, cache_write, status, error, request_body, response_body, raw_request, raw_response) VALUES
 (1, 'canary-model-1', 'CANARY-D2-ROW profile openai', 'CANARY-D2-ROW session', '1', 'worker-1', 1200, 10, 20, 0, 0,
    'ok', '', 'CANARY-D2-ROW request', 'CANARY-D2-ROW response', 'CANARY-D2-ROW raw request', 'CANARY-D2-ROW raw response');

INSERT INTO llm_usage (id, task_id, exploration_id, worker, model, profile_name, latency_ms, input_tokens,
    output_tokens, cache_read, cache_write, status, provider, intent_id, attempt, reason, request_bytes,
    system_bytes, message_bytes, tool_schema_bytes, tool_result_bytes, size_known, call_id, round_no, purpose,
    tool_bytes) VALUES
 (1, '1', 1, 'worker-1', 'canary-model-1', 'CANARY-D2-ROW profile openai', 1200, 10, 20, 0, 0, 'ok',
    'CANARY-D2-ROW provider', 3, 1, 'CANARY-D2-ROW', 100, 10, 80, 5, 5, true, 'CANARY-D2-ROW call', 1, 'CANARY-D2-ROW purpose',
    '{"canary_tool":5}');

-- ---------------------------------------------------------------- settings
-- One-time flags of db.Open: interactive_shell_default_v1 holds 'true' (db/db.go:286,290), the
-- other four 'done' (db/db.go:331,352; 387,571; 582,620; 640,654). The server seeders write
-- 'true' (server/orchestration.go:568-988, server/finding_*.go): the 19 keys of
-- flags-v0.5.12m.txt. Other keys are operator settings.
INSERT INTO settings (key, value) VALUES
 ('interactive_shell_default_v1', 'true'),
 ('asset_intercept_default_rules_v1', 'done'),
 ('intercept_default_rules_v1', 'done'),
 ('intercept_default_rules_v2', 'done'),
 ('intercept_default_rules_v3', 'done'),
 ('auto_default_bindings_v3', 'true'),
 ('auto_report_finding_v1', 'true'),
 ('company_scope_rebind_v1', 'true'),
 ('finding_retester_seed_v1', 'true'),
 ('finding_traffic_tools_v1', 'true'),
 ('finding_workflow_tools_v2_reporter', 'true'),
 ('goal_met_unbind_default_v1', 'true'),
 ('goals_prompt_constraint_step_v1', 'true'),
 ('mainagent_prompt_goalless_intent_v1', 'true'),
 ('planner_list_assets_v1', 'true'),
 ('planner_prompt_compact_realistic_v2', 'true'),
 ('planner_report_finding_v1', 'true'),
 ('reporter_agent_seed_v1', 'true'),
 ('reporter_trigger_evidence_version_v1', 'true'),
 ('task_credential_bindings_v1', 'true'),
 ('tool_schema_refresh_v7_list_facts_paging', 'true'),
 ('worker_prompt_compact_v4', 'true'),
 ('worker_readback_rebind_v2', 'true'),
 ('worker_readtools_unbind_v1', 'true'),
 ('traffic_capture', 'true'),
 ('canary.operator_note', 'CANARY-D2-ROW setting value');

-- ---------------------------------------------------------------- sequences
-- Explicit ids were used everywhere: move every owned sequence to max(id).
SELECT setval(pg_get_serial_sequence('companies', 'id'), (SELECT max(id) FROM companies));
SELECT setval(pg_get_serial_sequence('assets', 'id'), (SELECT max(id) FROM assets));
SELECT setval(pg_get_serial_sequence('company_scope', 'id'), (SELECT max(id) FROM company_scope));
SELECT setval(pg_get_serial_sequence('explorations', 'id'), (SELECT max(id) FROM explorations));
SELECT setval(pg_get_serial_sequence('exploration_nodes', 'id'), (SELECT max(id) FROM exploration_nodes));
SELECT setval(pg_get_serial_sequence('task_constraints', 'id'), (SELECT max(id) FROM task_constraints));
SELECT setval(pg_get_serial_sequence('activity', 'id'), (SELECT max(id) FROM activity));
SELECT setval(pg_get_serial_sequence('llm_profiles', 'id'), (SELECT max(id) FROM llm_profiles));
SELECT setval(pg_get_serial_sequence('task_categories', 'id'), (SELECT max(id) FROM task_categories));
SELECT setval(pg_get_serial_sequence('tasks', 'id'), (SELECT max(id) FROM tasks));
SELECT setval(pg_get_serial_sequence('task_archives', 'id'), (SELECT max(id) FROM task_archives));
SELECT setval(pg_get_serial_sequence('task_templates', 'id'), (SELECT max(id) FROM task_templates));
SELECT setval(pg_get_serial_sequence('task_scope', 'id'), (SELECT max(id) FROM task_scope));
SELECT setval(pg_get_serial_sequence('agents', 'id'), (SELECT max(id) FROM agents));
SELECT setval(pg_get_serial_sequence('agent_prompts', 'id'), (SELECT max(id) FROM agent_prompts));
SELECT setval(pg_get_serial_sequence('agent_prompt_vars', 'id'), (SELECT max(id) FROM agent_prompt_vars));
SELECT setval(pg_get_serial_sequence('mcp_servers', 'id'), (SELECT max(id) FROM mcp_servers));
SELECT setval(pg_get_serial_sequence('mcp_tools_cache', 'id'), (SELECT max(id) FROM mcp_tools_cache));
SELECT setval(pg_get_serial_sequence('skill_usage', 'id'), (SELECT max(id) FROM skill_usage));
SELECT setval(pg_get_serial_sequence('tool_usage', 'id'), (SELECT max(id) FROM tool_usage));
SELECT setval(pg_get_serial_sequence('conversations', 'id'), (SELECT max(id) FROM conversations));
SELECT setval(pg_get_serial_sequence('conversation_activities', 'id'), (SELECT max(id) FROM conversation_activities));
SELECT setval(pg_get_serial_sequence('agent_triggers', 'id'), (SELECT max(id) FROM agent_triggers));
SELECT setval(pg_get_serial_sequence('intercept_rules', 'id'), (SELECT max(id) FROM intercept_rules));
SELECT setval(pg_get_serial_sequence('intercept_pending', 'id'), (SELECT max(id) FROM intercept_pending));
SELECT setval(pg_get_serial_sequence('findings', 'id'), (SELECT max(id) FROM findings));
SELECT setval(pg_get_serial_sequence('finding_retests', 'id'), (SELECT max(id) FROM finding_retests));
SELECT setval(pg_get_serial_sequence('finding_traffic_bindings', 'id'), (SELECT max(id) FROM finding_traffic_bindings));
SELECT setval(pg_get_serial_sequence('finding_comments', 'id'), (SELECT max(id) FROM finding_comments));
SELECT setval(pg_get_serial_sequence('server_logs', 'id'), (SELECT max(id) FROM server_logs));
SELECT setval(pg_get_serial_sequence('side_question_requests', 'ordinal'), (SELECT max(ordinal) FROM side_question_requests));
SELECT setval(pg_get_serial_sequence('asset_intercept_rules', 'id'), (SELECT max(id) FROM asset_intercept_rules));
SELECT setval(pg_get_serial_sequence('task_intercept_rules', 'id'), (SELECT max(id) FROM task_intercept_rules));
SELECT setval(pg_get_serial_sequence('task_credentials', 'id'), (SELECT max(id) FROM task_credentials));
SELECT setval(pg_get_serial_sequence('notification_channels', 'id'), (SELECT max(id) FROM notification_channels));
SELECT setval(pg_get_serial_sequence('notification_events', 'id'), (SELECT max(id) FROM notification_events));
SELECT setval(pg_get_serial_sequence('notification_deliveries', 'id'), (SELECT max(id) FROM notification_deliveries));
SELECT setval(pg_get_serial_sequence('rsi_lessons', 'id'), (SELECT max(id) FROM rsi_lessons));
SELECT setval(pg_get_serial_sequence('llm_records', 'id'), (SELECT max(id) FROM llm_records));
SELECT setval(pg_get_serial_sequence('llm_usage', 'id'), (SELECT max(id) FROM llm_usage));

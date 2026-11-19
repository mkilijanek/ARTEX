-- Operator state of a 0.5.10m instance, applied on top of the state the startup
-- writers of that release leave behind (docs/phase-d2-plan.md section 2.2, AC9).
-- Every text value carries the row canary: it must never appear in test output.
-- The startup writers at v0.5.10m and at this commit are identical apart from the
-- module path (git diff v0.5.10m HEAD over agent/, server/, traffic/, db/db.go has no
-- other non-test line), so the rows they wrote at this commit are the rows 0.5.10m wrote.

-- 0.5.10m has no finding_comments table (the r0509 shape lacks it).
DROP TABLE finding_comments;

-- The operator renamed a built-in agent and rewrote its description.
UPDATE agents SET name = 'CANARY-D2-ROW renamed planner', description = 'CANARY-D2-ROW operator description'
 WHERE key = 'planner';

-- Prompt-variable catalog rows from older releases (db/db.go:280 deletes these names).
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
SELECT id, 'EngagementTitle', 'CANARY-D2-ROW', 'CANARY-D2-ROW', 'exploration' FROM agents WHERE key = 'goals';
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
SELECT id, 'Now', 'CANARY-D2-ROW', 'CANARY-D2-ROW', 'runtime' FROM agents WHERE key = 'goals';
INSERT INTO agent_prompt_vars(agent_id, var_name, description, example, source)
SELECT id, 'CoverageGaps', 'CANARY-D2-ROW', 'CANARY-D2-ROW', 'distilled' FROM agents WHERE key = 'planner';

-- record_fact as 0.5.10m stored it: no source property, description edited by the operator.
UPDATE tools SET description = 'CANARY-D2-ROW operator description',
                 schema = schema #- '{properties,source}'
 WHERE key = 'record_fact';

-- Operator-edited prompt bodies of the current versions.
UPDATE agent_prompts SET template_text = 'CANARY-D2-ROW operator prompt ' || a.key
  FROM agents a WHERE a.id = agent_prompts.agent_id AND a.current_prompt_id = agent_prompts.id
   AND a.key IN ('worker', 'planner', 'goals');

-- An operator-set interpreter path, so the python seeder has nothing to write.
INSERT INTO settings(key, value) VALUES ('python_interpreter', '/usr/bin/python3')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

-- Interrupted retests: one pending and one running (a unique index allows one active
-- retest per finding).
INSERT INTO findings(id, name, summary) VALUES (9001, 'CANARY-D2-ROW', 'CANARY-D2-ROW'), (9002, 'CANARY-D2-ROW', 'CANARY-D2-ROW');
INSERT INTO finding_retests(id, finding_id, status, snapshot, notes)
VALUES (9101, 9001, 'pending', '{"note":"CANARY-D2-ROW"}', 'CANARY-D2-ROW');
INSERT INTO finding_retests(id, finding_id, status, snapshot, notes, started_at)
VALUES (9102, 9002, 'running', '{"note":"CANARY-D2-ROW"}', 'CANARY-D2-ROW', now());

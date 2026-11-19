-- Rows for the atomic-apply tests (docs/phase-d2-plan.md AC6), loaded after the r0509
-- schema. Synthetic; text columns carry the row canary.
--
-- The state is chosen so that the statements of schema.sql that run BEFORE the failing
-- one already have work to do, which a rolled-back apply must undo:
--   * a task listed in assets.task_ids with the task_asset_links row missing
--     (the backfill at db/schema.sql:712-717 inserts it);
--   * a task that pins an LLM profile and has no chain
--     (the backfill at db/schema.sql:745-755 inserts the chain entry and sets
--     active_llm_profile_id);
--   * no finding_comments table (new since the r0509 shape).
INSERT INTO companies(name, nkey) VALUES ('CANARY-D2-ROW company', 'canary-d2-row-company');
INSERT INTO llm_profiles(name, format, model) VALUES ('CANARY-D2-ROW profile', 'openai', 'canary-d2-row-model');
WITH e AS (
  INSERT INTO explorations(description, goal) VALUES ('CANARY-D2-ROW exploration', 'CANARY-D2-ROW goal') RETURNING id
)
INSERT INTO tasks(name, description, goal, exploration_id, llm_profile_id)
SELECT 'CANARY-D2-ROW task', 'CANARY-D2-ROW description', 'CANARY-D2-ROW goal', e.id,
       (SELECT id FROM llm_profiles WHERE name = 'CANARY-D2-ROW profile')
  FROM e;
INSERT INTO assets(type, domain, root_domain, task_ids)
SELECT 'root_domain', 'canary-d2-row.example', 'canary-d2-row.example', ARRAY[t.id]
  FROM tasks t WHERE t.name = 'CANARY-D2-ROW task';
-- trg_assets_task_links created the link on the insert; remove it by hand
-- (session_replication_role is not used), so that the link is missing.
DELETE FROM task_asset_links;

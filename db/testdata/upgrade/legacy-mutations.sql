-- SYNTHETIC shape "legacy": NOT a recorded release (docs/phase-d2-plan.md section 5.1, K2).
-- Applied by db/upgrade_matrix_test.go after schema-b4b9600f5c8f.sql (the r0509 DDL) and
-- goddl-4bfc9dbe09c3.sql, before data-legacy.sql. It puts back the older forms that the
-- upgrade branches of db/schema.sql look for, so that startup has to enter them. Releases
-- older than 0.5.9m are not tested as recorded shapes; this is an approximation of them,
-- reverse-engineered from the branch conditions. TestUpgradeLegacyBranchesWereEntered asserts
-- that every condition below is true before startup and false after it.

-- db/schema.sql:270-282: the 'covers' relation postdates shipped installs, whose rel CHECK is
-- an inline auto-named constraint (exploration_edges_rel_check) without 'covers'; ck_edge_rel
-- does not exist yet.
ALTER TABLE exploration_edges DROP CONSTRAINT ck_edge_rel;
ALTER TABLE exploration_edges ADD CHECK (rel IN ('spawns','derived_from','yields','proves'));

-- db/schema.sql:212-224: ck_node_kind of installs that predate the 'digest' kind.
ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_kind;
ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_kind
    CHECK (kind IN ('begin','goal','intent','fact','finding','hint'));

-- db/schema.sql:227-247: ck_node_state without 'paused', 'superseded' and 'deleted' (and
-- without the digest branch).
ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_state;
ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_state CHECK (
    (kind='begin'   AND state IN ('open')) OR
    (kind='intent'  AND state IN ('open','running','done','blocked','exhausted','stopped')) OR
    (kind='goal'    AND state IN ('open','met','abandoned')) OR
    (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
    (kind='finding' AND state IN ('confirmed','dismissed')) OR
    (kind='hint'    AND state IN ('active','consumed'))
);

-- db/schema.sql:478-490: llm_profiles from before the thinking_type split. The column does
-- not exist; the data fixture holds reasoning_effort values 'off', 'high' and ''.
ALTER TABLE llm_profiles DROP COLUMN thinking_type;

-- db/schema.sql:781-783: the original task_scope dedup index (commit 0b16166), without the
-- value column; uq_task_scope_v2 does not exist yet.
DROP INDEX uq_task_scope_v2;
CREATE UNIQUE INDEX uq_task_scope ON task_scope(
    task_id, kind, COALESCE(domain,''), COALESCE(net::text,''), COALESCE(company_id,0));

-- db/schema.sql:105-111: assets.company_source as ALTER ... ADD COLUMN left it: nullable, no
-- default, no CHECK. The data fixture holds a row with NULL.
ALTER TABLE assets DROP CONSTRAINT assets_company_source_check;
ALTER TABLE assets ALTER COLUMN company_source DROP NOT NULL;
ALTER TABLE assets ALTER COLUMN company_source DROP DEFAULT;

-- db/schema.sql:845-848: agents.current_prompt_id has no foreign key yet. The block adds it
-- and swallows duplicate_object.
ALTER TABLE agents DROP CONSTRAINT fk_agents_curprompt;

-- db/schema.sql:1111-1116 (intercept_pending.decision_source empty) is a data condition: the
-- data fixture holds a row with decision_source = ''.

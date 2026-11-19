-- One row that violates a CHECK which schema.sql drops and re-adds on every start
-- (task_scope_kind_check, db/schema.sql:777-779). Loaded after violation-base.sql.
-- The CHECK is dropped first so that the row can be inserted; the re-added CHECK then
-- fails the whole apply with SQLSTATE 23514.
ALTER TABLE task_scope DROP CONSTRAINT task_scope_kind_check;
INSERT INTO task_scope(task_id, kind, value, reason)
SELECT t.id, 'CANARY-D2-ROW-kind', 'CANARY-D2-ROW value', 'CANARY-D2-ROW reason'
  FROM tasks t WHERE t.name = 'CANARY-D2-ROW task';

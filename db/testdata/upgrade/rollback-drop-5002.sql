-- Guarded drop of migration 5002 (tool_rag_observations) after a rollback to a binary that does not ship it
-- (docs/phase-d3-plan.md section 10). Optional: the table is ignored by older binaries, which start with
-- ARTEX_SCHEMA_ALLOW_NEWER=5002. Run it with no ARTEX instance running, on the database that was rolled back:
--   psql -v ON_ERROR_STOP=1 -f rollback-drop-5002.sql
-- It acts only when 5002 is the newest fork migration in the ledger and its row has the name of the shipped file, and
-- raises, changing nothing, otherwise: restore the pre-upgrade dump then. Loss is limited to the measurements in the table.
-- Order of the recipes on a database this release started: this file first, then rollback-0004-index.sql, then
-- rollback-cleanup-ledger.sql (each of those two refuses while 5002 is recorded).
-- This file is run by db/tool_rag_migration_test.go and db/upgrade_rollback_test.go.
BEGIN;
SELECT pg_advisory_xact_lock(7337741001);
DO $$
BEGIN
  IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5002 THEN
    RAISE EXCEPTION '5002 is not the newest fork migration: restore the pre-upgrade dump instead';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 5002 AND name = '5002_tool_rag_observations.sql') THEN
    RAISE EXCEPTION '5002 is recorded under another name: restore the pre-upgrade dump instead';
  END IF;
END $$;
DELETE FROM schema_migrations WHERE version = 5002 AND name = '5002_tool_rag_observations.sql';
DROP TABLE IF EXISTS tool_rag_observations;
COMMIT;

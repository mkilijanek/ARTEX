-- Guarded drop of migration 5003 (finding_detections, detection_rules, detection_steps, detection_query_results, the
-- function stop_deleted_conversation_detection and the trigger trg_conversation_detection_delete) after a rollback to a
-- binary that does not ship it (docs/detector-plan.md sections 5 and 16). Optional: the tables are ignored by older
-- binaries, which start with ARTEX_SCHEMA_ALLOW_NEWER=5003. Run it with no ARTEX instance running, on the database
-- that was rolled back, after a dump:
--   psql -v ON_ERROR_STOP=1 -f rollback-drop-5003.sql
-- It acts only when 5003 is the newest fork migration in the ledger and raises, changing nothing, otherwise:
-- restore the pre-upgrade dump then. IT DESTROYS EVERY DETECTION RUN, RULE, STEP AND QUERY RESULT: it is the one
-- irreversible step of the detection work.
-- Order of the recipes on a database this release started: this file first, then rollback-drop-5002.sql, then
-- rollback-0004-index.sql, then rollback-cleanup-ledger.sql (each of those three refuses while 5003 is recorded).
-- This file is run by db/detection_migration_test.go and db/upgrade_rollback_test.go.
BEGIN;
SELECT pg_advisory_xact_lock(7337741001);
DO $$
BEGIN
  IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5003 THEN
    RAISE EXCEPTION '5003 is not the newest fork migration: restore the pre-upgrade dump instead';
  END IF;
END $$;
DELETE FROM schema_migrations WHERE version = 5003 AND name = '5003_finding_detections.sql';
DROP TRIGGER IF EXISTS trg_conversation_detection_delete ON conversations;
DROP FUNCTION IF EXISTS stop_deleted_conversation_detection();
DROP TABLE IF EXISTS detection_query_results;
DROP TABLE IF EXISTS detection_steps;
DROP TABLE IF EXISTS detection_rules;
DROP TABLE IF EXISTS finding_detections;
COMMIT;

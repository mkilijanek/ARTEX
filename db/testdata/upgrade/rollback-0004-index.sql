-- Guarded rollback of migration 0004 (docs/phase-d2-stage2-plan.md 3.8).
-- Optional: the release before 0004 refuses a database that has it ("unknown migration applied") unless
-- ARTEX_SCHEMA_ALLOW_NEWER=0004 is set; the index is harmless to it. To go back without the override, stop every
-- ARTEX instance and run this file on the database, with:
--   psql -v ON_ERROR_STOP=1 -f rollback-0004-index.sql
-- It drops the index and deletes the ledger row of 0004 only when the ledger holds exactly the baseline set and
-- 0004 (0001, 0002, 0003, 0004, 5001) and the row of 0004 has the name of the shipped file, and raises, changing nothing, when it holds anything else: a later
-- migration has run, or the ledger was edited. The next start of this release applies 0004 again. 0.5.13m starts
-- without either step.
-- This file is quoted by docs/schema-upgrade.md and run by db/upgrade_migration0004_test.go.
BEGIN;
SELECT pg_advisory_xact_lock(7337741001);
DO $$
BEGIN
  IF (SELECT array_agg(version ORDER BY version) FROM schema_migrations) IS DISTINCT FROM ARRAY[1,2,3,4,5001]
     OR NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 4 AND name = '0004_llm_usage_ts_index.sql') THEN
    RAISE EXCEPTION 'schema_migrations is not baseline + 0004: restore the pre-upgrade dump';
  END IF;
END $$;
DELETE FROM schema_migrations WHERE version = 4 AND name = '0004_llm_usage_ts_index.sql';
DROP INDEX IF EXISTS idx_llm_usage_ts;
COMMIT;

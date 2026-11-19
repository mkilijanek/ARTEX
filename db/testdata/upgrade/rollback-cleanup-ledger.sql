-- Guarded cleanup of the migration ledger after a rollback to 0.5.13m (docs/phase-d2-stage2-plan.md 3.8).
-- Optional: it only restores the exact catalog of a database that never had the ledger. 0.5.13m starts
-- with or without it. Run it with no ARTEX instance running, on the database that was rolled back:
--   psql -v ON_ERROR_STOP=1 -f rollback-cleanup-ledger.sql
-- It drops the ledger only when the ledger holds exactly the baseline set (0001, 0002, 0003, 5001) and
-- raises, changing nothing, when it holds anything else: a later migration has run, or the ledger was
-- edited. The next start of the migration runner adopts the baseline set again.
-- This file is quoted by docs/schema-upgrade.md and run by db/upgrade_rollback_test.go.
BEGIN;
SELECT pg_advisory_xact_lock(7337741001);
DO $$
BEGIN
  IF (SELECT array_agg(version ORDER BY version) FROM schema_migrations) IS DISTINCT FROM ARRAY[1,2,3,5001] THEN
    RAISE EXCEPTION 'schema_migrations is not the baseline set: do not drop it; restore the pre-upgrade dump';
  END IF;
END $$;
DROP TABLE schema_migrations;
COMMIT;

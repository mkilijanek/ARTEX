-- Guarded drop of migration 5004 (signal_sidecar_config, signal_sidecar_status, the view signal_sidecar_config_v, the
-- functions signal_sidecar_heartbeat, signal_sidecar_set_link, signal_sidecar_set_groups, signal_sidecar_grant, and the
-- database role of the Signal sidecar) after a rollback to a binary that does not ship it. Optional: the objects are
-- ignored by older binaries, which start with ARTEX_SCHEMA_ALLOW_NEWER=5004. Run it with no ARTEX instance running
-- and the sidecar stopped, on the database that was rolled back, after a dump:
--   psql -v ON_ERROR_STOP=1 -f rollback-drop-5004.sql
-- Change the role name below when ARTEX_SIGNAL_DB_ROLE named another one.
-- It acts only when 5004 is the newest fork migration in the ledger and its row has the name of the shipped file, and
-- raises, changing nothing, otherwise: restore the pre-upgrade dump then. It destroys the stored Signal state of the
-- database (the sidecar's own data volume is not touched). It reads public_temp_revoked first: TEMPORARY is granted to
-- PUBLIC again only when provisioning had revoked it.
-- Order of the recipes on a database this release started: this file first, then rollback-drop-5003.sql, then
-- rollback-drop-5002.sql, then rollback-0004-index.sql, then rollback-cleanup-ledger.sql (each of those refuses while
-- 5004 is recorded).
-- Locks wait at most 5 seconds. A lock timeout (SQLSTATE 55P03) means that a session is still connected as the sidecar
-- role or another statement holds a lock: stop it and run the file again.
-- This file is run by db/signal_sidecar_migration_test.go and db/upgrade_rollback_test.go.
BEGIN;
SET LOCAL lock_timeout = '5s';
SELECT pg_advisory_xact_lock(7337741001);
DO $$
DECLARE
  v_role    text := 'artex_signal';
  v_revoked boolean;
BEGIN
  IF (SELECT max(version) FROM schema_migrations WHERE version >= 5000) IS DISTINCT FROM 5004 THEN
    RAISE EXCEPTION '5004 is not the newest fork migration: restore the pre-upgrade dump instead';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 5004 AND name = '5004_signal_sidecar.sql') THEN
    RAISE EXCEPTION '5004 is recorded under another name: restore the pre-upgrade dump instead';
  END IF;
  SELECT c.public_temp_revoked INTO v_revoked FROM public.signal_sidecar_config c WHERE c.id;
  IF NOT FOUND THEN
    RAISE WARNING 'signal_sidecar_config has no row: whether PUBLIC held TEMPORARY is unknown, so it is not granted back';
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = v_role) THEN
    IF shobj_description((SELECT r.oid FROM pg_roles r WHERE r.rolname = v_role), 'pg_authid') IS DISTINCT FROM 'artex signal sidecar' THEN
      RAISE EXCEPTION 'the role % was not made for the sidecar: it is left alone; drop the objects by hand', v_role;
    END IF;
    EXECUTE format('DROP OWNED BY %I', v_role);
    EXECUTE format('DROP ROLE %I', v_role);
  END IF;
  DROP VIEW IF EXISTS public.signal_sidecar_config_v;
  DROP FUNCTION IF EXISTS public.signal_sidecar_grant(regrole);
  DROP FUNCTION IF EXISTS public.signal_sidecar_set_groups(bigint, jsonb);
  DROP FUNCTION IF EXISTS public.signal_sidecar_set_link(bigint, text);
  DROP FUNCTION IF EXISTS public.signal_sidecar_heartbeat(text, bigint, text, text, text, text, text, text);
  DROP TABLE IF EXISTS public.signal_sidecar_status;
  DROP TABLE IF EXISTS public.signal_sidecar_config;
  IF v_revoked THEN
    EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO PUBLIC', current_database());
  END IF;
  DELETE FROM schema_migrations WHERE version = 5004 AND name = '5004_signal_sidecar.sql';
END $$;
COMMIT;

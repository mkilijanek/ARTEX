-- downgrade-safe: yes: additive (two tables, one view, four functions); earlier releases never reference them
CREATE TABLE IF NOT EXISTS signal_sidecar_config (
    id                  BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    desired             TEXT    NOT NULL DEFAULT 'idle' CHECK (desired IN ('idle','link','unlink')),
    gen                 BIGINT  NOT NULL DEFAULT 0 CHECK (gen >= 0),
    device_name         TEXT    NOT NULL DEFAULT 'ARTEX' CHECK (device_name ~ '^[A-Za-z0-9 ._-]{1,50}$'),
    link_ttl_seconds    INTEGER NOT NULL DEFAULT 120 CHECK (link_ttl_seconds BETWEEN 30 AND 120),
    confirmed_account   TEXT    NOT NULL DEFAULT '' CHECK (confirmed_account = '' OR confirmed_account ~ '^[+][1-9][0-9]{6,14}$'),
    groups_refresh_gen  BIGINT  NOT NULL DEFAULT 0 CHECK (groups_refresh_gen >= 0),
    db_auth_state       TEXT    NOT NULL DEFAULT 'unchecked' CHECK (db_auth_state IN ('unchecked','ok','trust','asserted')),
    -- set when provisioning revoked TEMPORARY on the database from PUBLIC (a rollback re-grants only then)
    public_temp_revoked BOOLEAN NOT NULL DEFAULT FALSE,
    requested_by        TEXT    NOT NULL DEFAULT '',
    requested_at        TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS signal_sidecar_status (
    id                 BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    state              TEXT    NOT NULL DEFAULT 'unknown' CHECK (state IN ('unknown','unlinked','linking','linked','error')),
    handled_gen        BIGINT  NOT NULL DEFAULT 0 CHECK (handled_gen >= 0),
    account_number     TEXT    NOT NULL DEFAULT '',
    account_aci        TEXT    NOT NULL DEFAULT '',
    link_uri           TEXT,
    link_expires_at    TIMESTAMPTZ,
    groups             JSONB   NOT NULL DEFAULT '[]',
    groups_at          TIMESTAMPTZ,
    groups_gen         BIGINT  NOT NULL DEFAULT 0,
    remote_unlink      TEXT    NOT NULL DEFAULT '' CHECK (remote_unlink IN ('','removed','local_only')),
    last_error         TEXT    NOT NULL DEFAULT '' CHECK (char_length(last_error) <= 500),
    last_error_at      TIMESTAMPTZ,
    heartbeat_at       TIMESTAMPTZ,
    sidecar_version    TEXT    NOT NULL DEFAULT '',
    signal_cli_version TEXT    NOT NULL DEFAULT '',
    CHECK (link_uri IS NULL OR state = 'linking'),
    CHECK (state = 'linked' OR (account_number = '' AND account_aci = '')),
    CHECK (jsonb_typeof(groups) = 'array' AND octet_length(groups::text) <= 131072)
);
INSERT INTO signal_sidecar_config DEFAULT VALUES ON CONFLICT DO NOTHING;
INSERT INTO signal_sidecar_status DEFAULT VALUES ON CONFLICT DO NOTHING;

-- The only relation the sidecar role reads. It runs with the rights of its owner on the tables (the default for
-- views) and exposes nothing else of the status row: no link, number, groups or error text.
CREATE OR REPLACE VIEW signal_sidecar_config_v AS
    SELECT c.desired, c.gen, c.device_name, c.link_ttl_seconds, c.confirmed_account, c.groups_refresh_gen,
           (c.db_auth_state IN ('ok','asserted')) AS db_auth_ok,
           s.handled_gen, s.groups_gen
    FROM public.signal_sidecar_config c CROSS JOIN public.signal_sidecar_status s;

-- Rules for the three functions below: tables are written as public.<name>; the configuration row is read
-- without a row lock and only the status row is locked (the server locks the configuration row first and the
-- status row second, never the reverse); no error text carries an argument value, and a table CHECK that fires
-- is replaced by a fixed text, because PostgreSQL would quote the whole row.
CREATE OR REPLACE FUNCTION public.signal_sidecar_heartbeat(
    p_state text, p_handled_gen bigint, p_number text, p_aci text, p_error text,
    p_remote_unlink text, p_sidecar_version text, p_cli_version text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_cfg_gen bigint;
    v_rows    integer;
    v_cur_gen bigint;
    v_cur_state text;
    v_number  text := coalesce(p_number, '');
    v_aci     text := coalesce(p_aci, '');
    v_remote  text := coalesce(p_remote_unlink, '');
    v_error   text := left(regexp_replace(coalesce(p_error, ''), 'sgnl://[^[:space:]]*', '[link removed]', 'gi'), 500);
BEGIN
    IF p_state IS NULL OR p_state NOT IN ('unlinked','linking','linked','error') THEN
        RAISE EXCEPTION 'signal sidecar: unknown state' USING ERRCODE = '22023';
    END IF;
    IF p_handled_gen IS NULL OR p_handled_gen < 0 THEN
        RAISE EXCEPTION 'signal sidecar: generation out of range' USING ERRCODE = '22023';
    END IF;
    IF v_number <> '' AND v_number !~ '^[+][1-9][0-9]{6,14}$' THEN
        RAISE EXCEPTION 'signal sidecar: number is not E.164' USING ERRCODE = '22023';
    END IF;
    IF v_aci !~ '^[0-9A-Fa-f-]{0,36}$' THEN
        RAISE EXCEPTION 'signal sidecar: account id is not valid' USING ERRCODE = '22023';
    END IF;
    IF v_remote NOT IN ('', 'removed', 'local_only') THEN
        RAISE EXCEPTION 'signal sidecar: unknown remote unlink value' USING ERRCODE = '22023';
    END IF;
    IF p_state = 'linked' AND v_number = '' THEN
        RAISE EXCEPTION 'signal sidecar: linked state needs a number' USING ERRCODE = '22023';
    END IF;

    SELECT c.gen INTO v_cfg_gen FROM public.signal_sidecar_config c WHERE c.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'signal sidecar row missing' USING ERRCODE = 'P0002';
    END IF;
    IF p_handled_gen > v_cfg_gen THEN
        RAISE EXCEPTION 'signal sidecar: generation is ahead of the command' USING ERRCODE = '55000';
    END IF;

    BEGIN
        UPDATE public.signal_sidecar_status s SET
            state              = p_state,
            handled_gen        = p_handled_gen,
            account_number     = CASE WHEN p_state = 'linked' THEN v_number ELSE '' END,
            account_aci        = CASE WHEN p_state = 'linked' THEN v_aci ELSE '' END,
            link_uri           = CASE WHEN p_state = 'linking' THEN s.link_uri ELSE NULL END,
            link_expires_at    = CASE WHEN p_state = 'linking' THEN s.link_expires_at ELSE NULL END,
            groups             = CASE WHEN p_state = 'unlinked' THEN '[]'::jsonb ELSE s.groups END,
            groups_at          = CASE WHEN p_state = 'unlinked' THEN NULL ELSE s.groups_at END,
            remote_unlink      = CASE WHEN p_state <> 'unlinked' THEN ''
                                      WHEN v_remote <> '' THEN v_remote
                                      WHEN p_handled_gen > s.handled_gen THEN ''
                                      ELSE s.remote_unlink END,
            last_error         = CASE WHEN v_error <> '' THEN v_error
                                      WHEN p_state IN ('linked','linking') OR p_handled_gen > s.handled_gen THEN ''
                                      ELSE s.last_error END,
            last_error_at      = CASE WHEN v_error <> '' THEN now()
                                      WHEN p_state IN ('linked','linking') OR p_handled_gen > s.handled_gen THEN NULL
                                      ELSE s.last_error_at END,
            heartbeat_at       = now(),
            sidecar_version    = left(coalesce(p_sidecar_version, ''), 64),
            signal_cli_version = left(coalesce(p_cli_version, ''), 64)
        WHERE s.id
          AND s.handled_gen <= p_handled_gen
          AND (p_state <> 'linking' OR s.state = 'linking');
        GET DIAGNOSTICS v_rows = ROW_COUNT;
    EXCEPTION WHEN check_violation THEN
        RAISE EXCEPTION 'signal sidecar: the status row was rejected' USING ERRCODE = '22023';
    END;

    IF v_rows = 0 THEN
        SELECT s.handled_gen, s.state INTO v_cur_gen, v_cur_state FROM public.signal_sidecar_status s WHERE s.id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'signal sidecar row missing' USING ERRCODE = 'P0002';
        ELSIF v_cur_gen > p_handled_gen THEN
            RAISE EXCEPTION 'signal sidecar: generation is stale' USING ERRCODE = '55000';
        ELSE
            RAISE EXCEPTION 'signal sidecar: linking is not allowed here' USING ERRCODE = '55000';
        END IF;
    END IF;
END;
$fn$;
REVOKE ALL ON FUNCTION public.signal_sidecar_heartbeat(text, bigint, text, text, text, text, text, text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION public.signal_sidecar_set_link(p_gen bigint, p_uri text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_gen     bigint;
    v_desired text;
    v_ttl     integer;
    v_rows    integer;
BEGIN
    IF p_gen IS NULL OR p_uri IS NULL THEN
        RAISE EXCEPTION 'signal sidecar: a required argument is missing' USING ERRCODE = '22023';
    END IF;
    -- 18 characters of "sgnl://linkdevice?" and 1 to 400 of parameters (a regular expression repeats at most 255 times).
    IF p_uri !~ '^sgnl://linkdevice[?][A-Za-z0-9%&=_.~+/,-]+$' OR char_length(p_uri) > 418 THEN
        RAISE EXCEPTION 'signal sidecar: link address is not valid' USING ERRCODE = '22023';
    END IF;

    SELECT c.gen, c.desired, c.link_ttl_seconds INTO v_gen, v_desired, v_ttl
      FROM public.signal_sidecar_config c WHERE c.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'signal sidecar row missing' USING ERRCODE = 'P0002';
    END IF;
    IF v_gen <> p_gen OR v_desired <> 'link' THEN
        RETURN false;
    END IF;

    BEGIN
        UPDATE public.signal_sidecar_status s SET
            state           = 'linking',
            link_uri        = p_uri,
            link_expires_at = now() + make_interval(secs => v_ttl),
            last_error      = '',
            last_error_at   = NULL,
            remote_unlink   = ''
        WHERE s.id AND s.handled_gen < p_gen AND s.state IN ('unlinked','linking');
        GET DIAGNOSTICS v_rows = ROW_COUNT;
    EXCEPTION WHEN check_violation THEN
        RAISE EXCEPTION 'signal sidecar: the status row was rejected' USING ERRCODE = '22023';
    END;

    IF v_rows = 0 THEN
        IF NOT EXISTS (SELECT 1 FROM public.signal_sidecar_status s WHERE s.id) THEN
            RAISE EXCEPTION 'signal sidecar row missing' USING ERRCODE = 'P0002';
        END IF;
        RETURN false;
    END IF;
    RETURN true;
END;
$fn$;
REVOKE ALL ON FUNCTION public.signal_sidecar_set_link(bigint, text) FROM PUBLIC;

CREATE OR REPLACE FUNCTION public.signal_sidecar_set_groups(p_refresh_gen bigint, p_groups jsonb)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_cfg_refresh bigint;
    v_el          jsonb;
    v_rows        integer;
BEGIN
    IF p_refresh_gen IS NULL OR p_groups IS NULL THEN
        RAISE EXCEPTION 'signal sidecar: a required argument is missing' USING ERRCODE = '22023';
    END IF;
    IF getdatabaseencoding() <> 'UTF8' THEN
        RAISE EXCEPTION 'signal sidecar: the database encoding is not UTF8' USING ERRCODE = '22023';
    END IF;
    IF jsonb_typeof(p_groups) IS DISTINCT FROM 'array' THEN
        RAISE EXCEPTION 'signal sidecar: groups must be an array' USING ERRCODE = '22023';
    END IF;
    IF jsonb_array_length(p_groups) > 200 THEN
        RAISE EXCEPTION 'signal sidecar: too many groups' USING ERRCODE = '22023';
    END IF;
    IF octet_length(p_groups::text) > 131072 THEN
        RAISE EXCEPTION 'signal sidecar: group list is too large' USING ERRCODE = '22023';
    END IF;
    FOR v_el IN SELECT e FROM jsonb_array_elements(p_groups) AS t(e) LOOP
        IF jsonb_typeof(v_el) IS DISTINCT FROM 'object'
           OR jsonb_typeof(v_el -> 'id') IS DISTINCT FROM 'string'
           OR jsonb_typeof(v_el -> 'name') IS DISTINCT FROM 'string'
           OR EXISTS (SELECT 1 FROM jsonb_object_keys(v_el) AS k WHERE k NOT IN ('id','name')) THEN
            RAISE EXCEPTION 'signal sidecar: a group entry has the wrong shape' USING ERRCODE = '22023';
        END IF;
        IF (v_el ->> 'id') !~ '^group[.][A-Za-z0-9+/]{59}=$' THEN
            RAISE EXCEPTION 'signal sidecar: a group id is not valid' USING ERRCODE = '22023';
        END IF;
        IF char_length(v_el ->> 'name') > 128 THEN
            RAISE EXCEPTION 'signal sidecar: a group name is too long' USING ERRCODE = '22023';
        END IF;
        -- The set stripped by the sidecar; this function refuses a name that still holds one.
        IF (v_el ->> 'name') ~ E'[\\u0001-\\u001F\\u007F-\\u009F\\u00AD\\u061C\\u180E\\u200B-\\u200F\\u2028-\\u202E\\u2060-\\u206F\\uFEFF\\uFFF9-\\uFFFB\\U000E0000-\\U000E007F]' THEN
            RAISE EXCEPTION 'signal sidecar: a group name holds a control or format character' USING ERRCODE = '22023';
        END IF;
    END LOOP;

    SELECT c.groups_refresh_gen INTO v_cfg_refresh FROM public.signal_sidecar_config c WHERE c.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'signal sidecar row missing' USING ERRCODE = 'P0002';
    END IF;
    IF p_refresh_gen > v_cfg_refresh THEN
        RETURN false;
    END IF;

    BEGIN
        UPDATE public.signal_sidecar_status s SET
            groups     = p_groups,
            groups_at  = now(),
            groups_gen = p_refresh_gen
        WHERE s.id AND s.state = 'linked' AND s.groups_gen <= p_refresh_gen;
        GET DIAGNOSTICS v_rows = ROW_COUNT;
    EXCEPTION WHEN check_violation THEN
        RAISE EXCEPTION 'signal sidecar: the status row was rejected' USING ERRCODE = '22023';
    END;

    IF v_rows = 0 THEN
        IF NOT EXISTS (SELECT 1 FROM public.signal_sidecar_status s WHERE s.id) THEN
            RAISE EXCEPTION 'signal sidecar row missing' USING ERRCODE = 'P0002';
        END IF;
        RETURN false;
    END IF;
    RETURN true;
END;
$fn$;
REVOKE ALL ON FUNCTION public.signal_sidecar_set_groups(bigint, jsonb) FROM PUBLIC;

-- Grants the sidecar role what it needs and nothing else; runs with the rights of the caller. The role is a
-- regrole, which prints as a correctly quoted identifier.
CREATE OR REPLACE FUNCTION public.signal_sidecar_grant(p_role regrole)
RETURNS void
LANGUAGE plpgsql
SECURITY INVOKER
SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF p_role IS NULL OR p_role::oid = 0 THEN
        RAISE EXCEPTION 'signal sidecar: a role is required' USING ERRCODE = '22023';
    END IF;
    EXECUTE format('GRANT SELECT ON public.signal_sidecar_config_v TO %s', p_role);
    EXECUTE format('GRANT EXECUTE ON FUNCTION public.signal_sidecar_heartbeat(text, bigint, text, text, text, text, text, text) TO %s', p_role);
    EXECUTE format('GRANT EXECUTE ON FUNCTION public.signal_sidecar_set_link(bigint, text) TO %s', p_role);
    EXECUTE format('GRANT EXECUTE ON FUNCTION public.signal_sidecar_set_groups(bigint, jsonb) TO %s', p_role);
END;
$fn$;
REVOKE ALL ON FUNCTION public.signal_sidecar_grant(regrole) FROM PUBLIC;

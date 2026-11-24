package db

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// The least-privilege contract of the sidecar role, checked as that role, from the catalogs and by trying.

// publicExecutable are the functions of schema public that PostgreSQL makes executable by PUBLIC when they are
// created, by name: try_inet parses text, the others are trigger functions, which PostgreSQL refuses to call
// outside a trigger. A later migration that creates a function either revokes PUBLIC or is added here on purpose.
var publicExecutable = []string{
	"set_updated_at", "stop_deleted_conversation_detection", "stop_deleted_conversation_retest",
	"sync_task_asset_links", "try_inet",
}

var sidecarFunctions = []string{"signal_sidecar_heartbeat", "signal_sidecar_set_groups", "signal_sidecar_set_link"}

func requireLeastPrivilege(t *testing.T, tmp *dbtest.Temp, role *dbtest.SidecarRole) {
	t.Helper()
	rc := signalRoleConn(t, tmp, role)
	ctx := context.Background()
	col := func(query string, args ...any) []string {
		rows, err := rc.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatalf("%.60s: %s", query, dbtest.AtomicErrorSummary(err))
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	one := func(query string, args ...any) bool {
		var b bool
		if err := rc.QueryRowContext(ctx, query, args...).Scan(&b); err != nil {
			t.Fatalf("%.60s: %s", query, dbtest.AtomicErrorSummary(err))
		}
		return b
	}

	// Every relation of every user schema: no privilege of any kind, no column privilege, no sequence privilege,
	// except SELECT on the one view.
	rels := col(`SELECT n.nspname || '.' || c.relname || ':' || c.relkind::text || ':' ||
	       CASE WHEN c.relkind = 'S' THEN has_sequence_privilege(c.oid, 'USAGE,SELECT,UPDATE')::text
	            ELSE (has_table_privilege(c.oid, 'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
	               OR has_any_column_privilege(c.oid, 'INSERT,UPDATE,REFERENCES')
	               OR (c.relname <> 'signal_sidecar_config_v' AND (has_table_privilege(c.oid, 'SELECT') OR has_any_column_privilege(c.oid, 'SELECT'))))::text END
	  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' AND c.relkind IN ('r','p','v','m','f','S')`)
	if len(rels) < 60 {
		t.Fatalf("only %d relations seen: the enumeration is broken", len(rels))
	}
	for _, r := range rels {
		if !strings.HasSuffix(r, ":false") {
			t.Errorf("the role holds a privilege on %s", r)
		}
	}
	if !one(`SELECT has_table_privilege('public.signal_sidecar_config_v', 'SELECT')`) {
		t.Error("the role cannot read signal_sidecar_config_v")
	}
	// Schema, database.
	if one(`SELECT has_schema_privilege('public', 'CREATE')`) || one(`SELECT has_database_privilege(current_database(), 'CREATE')`) ||
		one(`SELECT has_database_privilege(current_database(), 'TEMPORARY')`) {
		t.Error("the role may create objects, schemas or temporary tables")
	}
	// Functions of schema public: the three of the sidecar and the pinned list, nothing else.
	got := col(`SELECT p.proname FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND has_function_privilege(p.oid, 'EXECUTE')`)
	want := append(append([]string{}, sidecarFunctions...), publicExecutable...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("executable functions of schema public:\n  %s\nwant\n  %s", strings.Join(got, ","), strings.Join(want, ","))
	}
	// The accepted residual: the advisory locks stay executable; dangerous system functions do not.
	if !one(`SELECT has_function_privilege('pg_try_advisory_lock(bigint)', 'EXECUTE')`) {
		t.Error("the accepted residual changed: pg_advisory_lock is no longer executable by the role")
	}
	for _, fn := range []string{"pg_read_file", "pg_ls_dir", "pg_reload_conf", "lo_import"} {
		if one(`SELECT coalesce(bool_or(has_function_privilege(p.oid, 'EXECUTE')), false) FROM pg_proc p WHERE p.proname = $1`, fn) {
			t.Errorf("the role may execute %s", fn)
		}
	}
	// Reads the role must not make succeed, by trying.
	for _, table := range []string{"findings", "settings", "notification_channels", "signal_sidecar_status", "signal_sidecar_config"} {
		_, err := rc.ExecContext(ctx, `SELECT * FROM public.`+table)
		requireSQLState(t, "select from "+table, err, "42501")
	}
	for _, stmt := range []string{`CREATE TEMP TABLE sidecar_probe (a int)`, `CREATE TABLE public.sidecar_probe (a int)`, `CREATE SCHEMA sidecar_probe`} {
		_, err := rc.ExecContext(ctx, stmt)
		requireSQLState(t, stmt, err, "42501")
	}
	// The helper is not executable by the role.
	_, err := rc.ExecContext(ctx, `SELECT public.signal_sidecar_grant(current_user::regrole)`)
	requireSQLState(t, "signal_sidecar_grant", err, "42501")
	// Attributes of the role.
	var super, createDB, createRole, repl, bypass bool
	var members, owned, connLimit int
	var comment sql.NullString
	if err := rc.QueryRowContext(ctx, `SELECT r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	       (SELECT count(*) FROM pg_auth_members m WHERE m.member = r.oid),
	       (SELECT count(*) FROM pg_shdepend d WHERE d.refclassid = 'pg_authid'::regclass AND d.refobjid = r.oid AND d.deptype = 'o'),
	       shobj_description(r.oid, 'pg_authid')
	  FROM pg_roles r WHERE r.rolname = current_user`).Scan(&super, &createDB, &createRole, &repl, &bypass, &connLimit, &members, &owned, &comment); err != nil {
		t.Fatal(err)
	}
	if super || createDB || createRole || repl || bypass || members != 0 || owned != 0 || connLimit != 5 || comment.String != "artex signal sidecar" {
		t.Errorf("role attributes: super %v createdb %v createrole %v replication %v bypassrls %v members %d owned %d limit %d marker %q",
			super, createDB, createRole, repl, bypass, members, owned, connLimit, comment.String)
	}
}

// A function that PostgreSQL makes executable by PUBLIC is found by the catalog, not by a list of the role's
// grants: whatever a migration adds to schema public that PUBLIC may execute must be named in publicExecutable.
func TestSignalRoleCatalogPublicExecutableFunctionsArePinned(t *testing.T) {
	d, _, _ := signalFresh(t, "sigpublicfn")
	rows, err := d.Query(`SELECT p.proname FROM pg_proc p
	  WHERE p.pronamespace = 'public'::regnamespace
	    AND EXISTS (SELECT 1 FROM aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	sort.Strings(got)
	want := append([]string{}, publicExecutable...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("functions that PUBLIC may execute: %v, pinned %v", got, want)
	}
	// The pinned list of the provisioning check is the same set plus the three functions of the sidecar.
	pinned := append(append([]string{}, publicExecutable...), sidecarFunctions...)
	sort.Strings(pinned)
	have := append([]string{}, signalRoleFunctions...)
	sort.Strings(have)
	if strings.Join(pinned, ",") != strings.Join(have, ",") {
		t.Errorf("signalRoleFunctions = %v, want %v", have, pinned)
	}
}

// The contract holds on a fresh database and on one upgraded from the migration before.
func TestSignalRoleLeastPrivilegeOnFreshAndUpgradedDatabase(t *testing.T) {
	for _, mode := range []string{"fresh", "upgraded"} {
		t.Run(mode, func(t *testing.T) {
			var d *DB
			var tmp *dbtest.Temp
			var srv *dbtest.Server
			if mode == "fresh" {
				d, tmp, srv = signalFresh(t, "sigpriv")
			} else {
				d, tmp, srv = signalUpgraded(t, "sigprivup")
			}
			role := srv.NewSidecarRole(t, tmp)
			if st := d.ProvisionSignalRole(context.Background(), signalCfg(tmp, role)); st.State != SignalRoleOK {
				t.Fatalf("provisioning: %+v", st)
			}
			requireLeastPrivilege(t, tmp, role)
		})
	}
}

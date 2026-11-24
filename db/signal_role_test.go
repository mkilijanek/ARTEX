package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// Provisioning of the database role of the sidecar, for the three kinds of database user: a superuser, a user
// with CREATEROLE only and a user with neither. The role names come from dbtest.NewSidecarRole, so that the
// guard and the leftover check cover them.

// recorder collects the statements that provisioning sends to change something.
type recorder struct {
	mu    sync.Mutex
	stmts []string
}

func (r *recorder) hook(stmt string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, stmt)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}

// userKinds are the database users the server may run as.
type serverUser struct {
	d    *DB
	dsn  string
	tmp  *dbtest.Temp
	srv  *dbtest.Server
	role *dbtest.SidecarRole // the sidecar role to provision
}

// newServerUser opens a fresh database as a superuser, or as the owner of the database with or without CREATEROLE.
func newServerUser(t *testing.T, kind, label string) *serverUser {
	t.Helper()
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, label)
	u := &serverUser{tmp: tmp, srv: srv, dsn: tmp.DSN}
	switch kind {
	case "superuser":
	case "createrole", "neither":
		owner := srv.CreateOwnerRole(t, tmp, kind == "createrole")
		dsn, err := tmp.DSNFor(owner.Name, owner.Password)
		if err != nil {
			t.Fatal(err)
		}
		u.dsn = dsn
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	u.d = startup(t, u.dsn)
	u.role = srv.NewSidecarRole(t, tmp)
	return u
}

func (u *serverUser) cfg(rec *recorder) SignalRoleConfig {
	c := SignalRoleConfig{Role: u.role.Name, Password: u.role.Password, DSN: u.dsn, hba: noTrustHBA}
	if rec != nil {
		c.execHook = rec.hook
	}
	return c
}

func (u *serverUser) provision(t *testing.T, rec *recorder) SignalRoleStatus {
	t.Helper()
	return u.d.ProvisionSignalRole(context.Background(), u.cfg(rec))
}

func superConn(t *testing.T, u *serverUser) *DB {
	t.Helper()
	return startup(t, u.tmp.DSN)
}

func roleAttr(t *testing.T, d *DB, role, expr string) string {
	t.Helper()
	var v sql.NullString
	if err := d.QueryRow(`SELECT (`+expr+`)::text FROM pg_roles WHERE rolname = $1`, role).Scan(&v); err != nil {
		t.Fatalf("%s: %s", expr, dbtest.AtomicErrorSummary(err))
	}
	return v.String
}

func roleExists(t *testing.T, d *DB, role string) bool {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM pg_roles WHERE rolname = $1`, role).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func requireLogin(t *testing.T, u *serverUser, password string, want bool) {
	t.Helper()
	dsn, err := u.tmp.DSNFor(u.role.Name, password)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Ping(); (err == nil) != want {
		t.Errorf("login with that password: err = %v, want success %v", dbtest.AtomicErrorSummary(err), want)
	}
}

// 1: role absent, the server may create it: attributes, marker, grants, a revoked PUBLIC TEMPORARY, state ok.
func TestSignalRoleProvisionCreatesTheRoleForEveryDatabaseUser(t *testing.T) {
	for _, kind := range []string{"superuser", "createrole"} {
		t.Run(kind, func(t *testing.T) {
			logs := dbtest.CaptureLog(t)
			u := newServerUser(t, kind, "sigrole"+kind)
			st := u.provision(t, nil)
			if st.State != SignalRoleOK || st.Auth != "ok" {
				t.Fatalf("state = %+v, want ok / ok", st)
			}
			if !strings.Contains(logs(), "[signal] database role ready") {
				t.Errorf("no ready line:\n%s", logs())
			}
			for _, c := range []struct{ expr, want string }{
				{`rolcanlogin`, "true"}, {`rolsuper`, "false"}, {`rolcreatedb`, "false"}, {`rolcreaterole`, "false"},
				{`rolreplication`, "false"}, {`rolbypassrls`, "false"}, {`rolinherit`, "false"}, {`rolconnlimit`, "5"},
				{`shobj_description(oid, 'pg_authid')`, "artex signal sidecar"},
				{`array_to_string(rolconfig, ',')`, "idle_in_transaction_session_timeout=10s,search_path=public"},
			} {
				if got := roleAttr(t, u.d, u.role.Name, c.expr); got != c.want {
					t.Errorf("%s = %q, want %q", c.expr, got, c.want)
				}
			}
			// PUBLIC held TEMPORARY, so it was revoked and recorded; the authentication state is recorded too.
			s, err := u.d.ReadSignalSidecar(context.Background())
			if err != nil || !s.PublicTempRevoked || s.DBAuthState != "ok" {
				t.Errorf("config: revoked %v, auth %q, err %v", s.PublicTempRevoked, s.DBAuthState, err)
			}
			requireLogin(t, u, u.role.Password, true)
			requireLeastPrivilege(t, u.tmp, u.role)
			if got := u.d.SignalRoleState(context.Background(), u.role.Name); got.State != SignalRoleOK {
				t.Errorf("SignalRoleState = %+v", got)
			}
			if strings.Contains(logs(), u.role.Password) || strings.Contains(logs(), "SCRAM-SHA-256") {
				t.Error("the log carries the password or its verifier")
			}
		})
	}
}

// A user that may not create roles: the role is reported missing, with the manual statements in the log and no
// password in them; once an operator created the role, the same user only verifies.
func TestSignalRoleProvisionWithoutRightToCreateRoles(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	u := newServerUser(t, "neither", "sigroleneither")
	rec := &recorder{}
	st := u.provision(t, rec)
	if st.State != SignalRoleMissing {
		t.Fatalf("state = %+v, want missing", st)
	}
	if !strings.Contains(logs(), "CREATE ROLE "+quoteSQLIdent(u.role.Name)) || !strings.Contains(logs(), "signal_sidecar_grant") ||
		strings.Contains(logs(), u.role.Password) {
		t.Errorf("the manual statements are not shown, or the password is:\n%s", logs())
	}
	// The operator (here a superuser through the same database) creates the role by the documented path.
	super := superConn(t, u)
	if st := super.ProvisionSignalRole(context.Background(), SignalRoleConfig{Role: u.role.Name, Password: u.role.Password, DSN: u.tmp.DSN, hba: noTrustHBA}); st.State != SignalRoleOK {
		t.Fatalf("superuser provisioning: %+v", st)
	}
	rec = &recorder{}
	if st := u.provision(t, rec); st.State != SignalRoleOK || len(rec.all()) != 0 {
		t.Errorf("verification by a user without CREATEROLE: %+v, statements %v", st, rec.all())
	}
	// Something differs and the user may not alter roles: state error, a short text, never a panic or a start failure.
	if _, err := super.Exec(`ALTER ROLE ` + quoteSQLIdent(u.role.Name) + ` CONNECTION LIMIT 9`); err != nil {
		t.Fatal(err)
	}
	st = u.provision(t, nil)
	if st.State != SignalRoleError || !strings.Contains(st.Detail, "42501") {
		t.Errorf("alter without the right: %+v, want error naming SQLSTATE 42501", st)
	}
}

// 2: everything correct: no statement that changes anything is sent.
func TestSignalRoleProvisionSecondRunSendsNothing(t *testing.T) {
	u := newServerUser(t, "superuser", "sigrolesecond")
	if st := u.provision(t, nil); st.State != SignalRoleOK {
		t.Fatalf("first run: %+v", st)
	}
	rec := &recorder{}
	if st := u.provision(t, rec); st.State != SignalRoleOK || len(rec.all()) != 0 {
		t.Errorf("second run: %+v, statements %q; want ok and none", st, rec.all())
	}
}

// 3: only what differs is altered.
func TestSignalRoleProvisionAltersOnlyWhatDiffers(t *testing.T) {
	u := newServerUser(t, "superuser", "sigrolediffers")
	if st := u.provision(t, nil); st.State != SignalRoleOK {
		t.Fatalf("first run: %+v", st)
	}
	r := quoteSQLIdent(u.role.Name)
	for _, stmt := range []string{
		`ALTER ROLE ` + r + ` CONNECTION LIMIT 9`,
		`ALTER ROLE ` + r + ` RESET search_path`,
		`REVOKE EXECUTE ON FUNCTION signal_sidecar_set_link(bigint, text) FROM ` + r,
	} {
		signalExec(t, u.d, stmt)
	}
	rec := &recorder{}
	if st := u.provision(t, rec); st.State != SignalRoleOK {
		t.Fatalf("repair run: %+v", st)
	}
	got := rec.all()
	want := []string{"ALTER ROLE " + r + " CONNECTION LIMIT 5", "ALTER ROLE " + r + " SET search_path = 'public'", "SELECT public.signal_sidecar_grant($1::text::regrole)"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("statements:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if v := roleAttr(t, u.d, u.role.Name, `rolconnlimit`); v != "5" {
		t.Errorf("connection limit %s", v)
	}
}

// 4: a changed password in the environment is one ALTER ROLE with a verifier, and the statement is never logged.
func TestSignalRoleProvisionChangesAChangedPassword(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	u := newServerUser(t, "superuser", "sigrolepw")
	if st := u.provision(t, nil); st.State != SignalRoleOK {
		t.Fatalf("first run: %+v", st)
	}
	oldPassword := u.role.Password
	cfg := u.cfg(nil)
	cfg.Password = "a-changed-password-1234"
	rec := &recorder{}
	cfg.execHook = rec.hook
	if st := u.d.ProvisionSignalRole(context.Background(), cfg); st.State != SignalRoleOK {
		t.Fatalf("password run: %+v", st)
	}
	got := rec.all()
	if len(got) != 1 || !strings.HasPrefix(got[0], "ALTER ROLE "+quoteSQLIdent(u.role.Name)+" PASSWORD 'SCRAM-SHA-256$4096:") {
		t.Fatalf("statements = %q, want one ALTER ROLE ... PASSWORD with a verifier", got)
	}
	requireLogin(t, u, cfg.Password, true)
	requireLogin(t, u, oldPassword, false)
	for _, secret := range []string{cfg.Password, "SCRAM-SHA-256", "ALTER ROLE"} {
		if strings.Contains(logs(), secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
}

// 5: the verifier is the one PostgreSQL computes: the same salt gives the same text.
func TestSignalRoleScramVerifierMatchesTheServer(t *testing.T) {
	u := newServerUser(t, "superuser", "sigrolescram")
	r := quoteSQLIdent(u.role.Name)
	// A role made by the server's own hashing, read back through pg_authid.
	conn, err := u.d.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `SET password_encryption = 'scram-sha-256'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `CREATE ROLE `+r+` LOGIN PASSWORD 'pencil-and-paper'`); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := conn.QueryRowContext(context.Background(), `SELECT rolpassword FROM pg_authid WHERE rolname = $1`, u.role.Name).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 3 {
		t.Fatalf("stored verifier has %d parts", len(parts))
	}
	var iter int
	var saltB64 string
	if n, err := fmt.Sscanf(parts[1], "%d:%s", &iter, &saltB64); err != nil || n != 2 {
		t.Fatalf("cannot read the iteration count and salt: %v", err)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatal(err)
	}
	got, err := scramVerifier("pencil-and-paper", salt, iter)
	if err != nil {
		t.Fatal(err)
	}
	if got != stored {
		t.Error("the verifier differs from the one the server computed for the same salt")
	}
}

// 6: refusals; each one changes nothing.
func TestSignalRoleProvisionRefuses(t *testing.T) {
	type refusal struct {
		name   string
		prep   func(t *testing.T, u *serverUser, super *DB)
		cfg    func(u *serverUser, c *SignalRoleConfig)
		detail string
		// statements is how many changing statements the refusal may send: recording the authentication state.
		statements int
		check      func(t *testing.T, u *serverUser, super *DB)
	}
	r := func(u *serverUser) string { return quoteSQLIdent(u.role.Name) }
	marked := func(t *testing.T, u *serverUser, super *DB, attrs string) {
		signalExec(t, super, `CREATE ROLE `+r(u)+` LOGIN `+attrs)
		signalExec(t, super, `COMMENT ON ROLE `+r(u)+` IS 'artex signal sidecar'`)
	}
	cases := []refusal{
		{name: "the role is the connected user",
			cfg:    func(u *serverUser, c *SignalRoleConfig) { c.Role = "postgres" },
			detail: "connected as"},
		{name: "the role exists without the marker",
			prep:   func(t *testing.T, u *serverUser, super *DB) { signalExec(t, super, `CREATE ROLE `+r(u)+` LOGIN`) },
			detail: "did not create",
			check: func(t *testing.T, u *serverUser, super *DB) {
				if v := roleAttr(t, super, u.role.Name, `rolconnlimit`); v != "-1" {
					t.Errorf("a role without the marker was altered: connection limit %s", v)
				}
			}},
		{name: "the role can create databases",
			prep:   func(t *testing.T, u *serverUser, super *DB) { marked(t, u, super, "CREATEDB") },
			detail: "CREATEDB"},
		{name: "the role can create roles",
			prep:   func(t *testing.T, u *serverUser, super *DB) { marked(t, u, super, "CREATEROLE") },
			detail: "CREATEROLE"},
		{name: "the role is a superuser",
			prep:   func(t *testing.T, u *serverUser, super *DB) { marked(t, u, super, "SUPERUSER") },
			detail: "SUPERUSER"},
		{name: "the role is a member of another role",
			prep: func(t *testing.T, u *serverUser, super *DB) {
				marked(t, u, super, "")
				signalExec(t, super, `GRANT pg_monitor TO `+r(u))
			},
			detail: "membership"},
		{name: "the role owns an object",
			prep: func(t *testing.T, u *serverUser, super *DB) {
				marked(t, u, super, "")
				signalExec(t, super, `CREATE TABLE sidecar_probe (a int)`)
				signalExec(t, super, `ALTER TABLE sidecar_probe OWNER TO `+r(u))
			},
			detail: "owned objects"},
		{name: "PUBLIC may create objects in schema public",
			prep: func(t *testing.T, u *serverUser, super *DB) {
				signalExec(t, super, `GRANT CREATE ON SCHEMA public TO PUBLIC`)
			},
			detail: "PUBLIC may create",
			check: func(t *testing.T, u *serverUser, super *DB) {
				if roleExists(t, super, u.role.Name) {
					t.Error("the role was created although PUBLIC may create objects")
				}
			}},
		{name: "pg_hba.conf uses trust for the role",
			cfg: func(u *serverUser, c *SignalRoleConfig) {
				c.hba = func(context.Context, signalQuerier) ([]hbaRule, bool, error) {
					return []hbaRule{{Type: "local", Databases: []string{"all"}, Users: []string{"all"}, Method: "trust"}}, true, nil
				}
			},
			detail:     "trust",
			statements: 1,
			check: func(t *testing.T, u *serverUser, super *DB) {
				if roleExists(t, super, u.role.Name) {
					t.Error("the role was created although the rules use trust")
				}
				if s, err := super.ReadSignalSidecar(context.Background()); err != nil || s.DBAuthState != "trust" {
					t.Errorf("recorded authentication state %q, err %v, want trust", s.DBAuthState, err)
				}
			}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newServerUser(t, "superuser", "sigrefuse"+string(rune('a'+i)))
			if c.prep != nil {
				c.prep(t, u, u.d)
			}
			before := snapshotRoles(t, u.d, u.role.Name)
			cfg := u.cfg(nil)
			rec := &recorder{}
			cfg.execHook = rec.hook
			if c.cfg != nil {
				c.cfg(u, &cfg)
			}
			st := u.d.ProvisionSignalRole(context.Background(), cfg)
			if st.State != SignalRoleError || !strings.Contains(st.Detail, c.detail) {
				t.Fatalf("state = %+v, want error mentioning %q", st, c.detail)
			}
			if got := rec.all(); len(got) != c.statements {
				t.Errorf("a refusal sent statements: %q, want %d", got, c.statements)
			}
			if after := snapshotRoles(t, u.d, u.role.Name); after != before {
				t.Errorf("a refusal changed the role: %q -> %q", before, after)
			}
			if c.check != nil {
				c.check(t, u, u.d)
			}
		})
	}
}

// snapshotRoles is the content-free state of the role, and of the grants the refusals must not change.
func snapshotRoles(t *testing.T, d *DB, role string) string {
	t.Helper()
	var s sql.NullString
	if err := d.QueryRow(`SELECT (SELECT coalesce(string_agg(r::text, ','), '') FROM pg_roles r WHERE rolname = $1)
	  || '|' || (SELECT coalesce(string_agg(has_function_privilege(p.oid, 'EXECUTE')::text, ','), '') FROM pg_proc p WHERE proname LIKE 'signal\_sidecar\_%')`, role).Scan(&s); err != nil {
		t.Fatalf("snapshot: %s", dbtest.AtomicErrorSummary(err))
	}
	return s.String
}

// 5 and 6 of the authentication rules: unreadable rules stay unchecked unless the operator asserts them; the
// sidecar's view reports db_auth_ok only for ok and asserted.
func TestSignalRoleProvisionAuthenticationRules(t *testing.T) {
	cases := []struct {
		name     string
		hba      func(context.Context, signalQuerier) ([]hbaRule, bool, error)
		asserted bool
		want     string
		authOK   bool
	}{
		{"rules readable, none trust", noTrustHBA, false, "ok", true},
		{"rules unreadable", func(context.Context, signalQuerier) ([]hbaRule, bool, error) { return nil, false, nil }, false, "unchecked", false},
		{"rules unreadable, asserted", func(context.Context, signalQuerier) ([]hbaRule, bool, error) { return nil, false, nil }, true, "asserted", true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newServerUser(t, "superuser", "sigauth"+string(rune('a'+i)))
			cfg := u.cfg(nil)
			cfg.hba, cfg.AuthAsserted = c.hba, c.asserted
			if st := u.d.ProvisionSignalRole(context.Background(), cfg); st.State != SignalRoleOK || st.Auth != c.want {
				t.Fatalf("state = %+v, want ok with auth %s", st, c.want)
			}
			var authOK bool
			if err := u.d.QueryRow(`SELECT db_auth_ok FROM signal_sidecar_config_v`).Scan(&authOK); err != nil || authOK != c.authOK {
				t.Errorf("db_auth_ok = %v err %v, want %v", authOK, err, c.authOK)
			}
		})
	}
}

// The rules are read from pg_hba_file_rules as they are: a superuser can read them, a plain user cannot.
func TestSignalRoleReadsAuthenticationRulesOnlyWhenAllowed(t *testing.T) {
	super := newServerUser(t, "superuser", "sighbasuper")
	rules, readable, err := readHBA(context.Background(), super.d.DB)
	if err != nil || !readable || len(rules) == 0 {
		t.Fatalf("superuser: %d rules, readable %v, err %v", len(rules), readable, err)
	}
	plain := newServerUser(t, "neither", "sighbaplain")
	if _, readable, err := readHBA(context.Background(), plain.d.DB); err != nil || readable {
		t.Errorf("plain user: readable %v err %v, want unreadable and no error", readable, err)
	}
}

func TestSignalRoleHBATrustMatching(t *testing.T) {
	rule := func(typ, db, user, method string) []hbaRule {
		return []hbaRule{{Type: typ, Databases: strings.Split(db, ","), Users: strings.Split(user, ","), Method: method}}
	}
	cases := []struct {
		name  string
		rules []hbaRule
		want  bool
	}{
		{"all and all", rule("local", "all", "all", "trust"), true},
		{"the database by name", rule("host", "app", "all", "trust"), true},
		{"the role by name", rule("host", "all", "artex_signal", "trust"), true},
		{"another database", rule("host", "other", "all", "trust"), false},
		{"another role", rule("host", "all", "someone", "trust"), false},
		{"scram", rule("host", "all", "all", "scram-sha-256"), false},
		{"a file reference", rule("host", "@names", "all", "trust"), true},
		{"sameuser with another name", rule("host", "sameuser", "all", "trust"), false},
		{"replication type only", rule("local", "replication", "all", "trust"), false},
		{"hostssl", rule("hostssl", "all", "all", "trust"), true},
		{"no rules", nil, false},
	}
	for _, c := range cases {
		if got := hbaTrustMatches(c.rules, "app", "artex_signal"); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// With no password configured nothing is changed: an absent role is "unchecked", a correct one is ok.
func TestSignalRoleVerifyOnlyWithoutPassword(t *testing.T) {
	u := newServerUser(t, "superuser", "sigroleverify")
	rec := &recorder{}
	cfg := u.cfg(rec)
	cfg.Password = ""
	if st := u.d.ProvisionSignalRole(context.Background(), cfg); st.State != SignalRoleUnchecked {
		t.Errorf("absent role without a password: %+v, want unchecked", st)
	}
	if st := u.provision(t, nil); st.State != SignalRoleOK {
		t.Fatalf("provision: %+v", st)
	}
	if st := u.d.ProvisionSignalRole(context.Background(), cfg); st.State != SignalRoleOK || len(rec.all()) != 0 {
		t.Errorf("present role without a password: %+v, statements %q", st, rec.all())
	}
	signalExec(t, u.d, `ALTER ROLE `+quoteSQLIdent(u.role.Name)+` CONNECTION LIMIT 9`)
	if st := u.d.ProvisionSignalRole(context.Background(), cfg); st.State != SignalRoleError || len(rec.all()) != 0 {
		t.Errorf("a differing role without a password: %+v, statements %q; want error and no change", st, rec.all())
	}
}

func TestSignalRoleNameRule(t *testing.T) {
	for _, ok := range []string{"artex_signal", "_x", "a1", strings.Repeat("a", 63)} {
		if !ValidSignalRoleName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "Artex", "1a", "a-b", "a b", "a\"b", "a;b", strings.Repeat("a", 64), "pg_x\n"} {
		if ValidSignalRoleName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	d := &DB{}
	if st := d.provisionSignalRole(context.Background(), SignalRoleConfig{Role: "bad name", Password: "x"}); st.State != SignalRoleError {
		t.Errorf("an invalid role name: %+v", st)
	}
}

// Only a wrong password (28P01) means "the password differs"; a role that may not log in is an error.
func TestSignalRoleLoginCheckTreatsOnlyAWrongPasswordAsWrong(t *testing.T) {
	u := newServerUser(t, "superuser", "sigrolelogin")
	if st := u.provision(t, nil); st.State != SignalRoleOK {
		t.Fatalf("provisioning: %+v", st)
	}
	ctx := context.Background()
	cfg := u.cfg(nil)
	if ok, err := u.d.signalLoginWorks(ctx, cfg, u.role.Name); err != nil || !ok {
		t.Fatalf("login with the right password = %v %v", ok, err)
	}
	cfg.Password = "not-the-password"
	if ok, err := u.d.signalLoginWorks(ctx, cfg, u.role.Name); err != nil || ok {
		t.Fatalf("login with a wrong password = %v %v, want false and no error", ok, err)
	}
	cfg.Password = u.role.Password
	signalExec(t, u.d, `ALTER ROLE `+quoteSQLIdent(u.role.Name)+` NOLOGIN`)
	if ok, err := u.d.signalLoginWorks(ctx, cfg, u.role.Name); err == nil {
		t.Fatalf("login of a role without LOGIN = %v, want an error", ok)
	}
}

// Provisioning takes the schema advisory lock, so two starting servers do not interleave.
func TestSignalRoleProvisionWaitsForTheAdvisoryLock(t *testing.T) {
	u := newServerUser(t, "superuser", "sigrolelock")
	ctx := context.Background()
	holder, err := superConn(t, u).BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback() //nolint:errcheck
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		t.Fatal(err)
	}
	st := u.provision(t, nil)
	if st.State != SignalRoleError || !strings.Contains(st.Detail, "55P03") {
		t.Errorf("provisioning while the lock is held: %+v, want error naming SQLSTATE 55P03", st)
	}
	if roleExists(t, u.d, u.role.Name) {
		t.Error("the role was created without the lock")
	}
}

// A failure without a SQLSTATE says what kind it was, and nothing of its text.
func TestSignalRoleStateTextNamesTheErrorClass(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "connection or driver error (timeout)"},
		{fmt.Errorf("dial tcp 10.1.2.3:5432: %w", context.Canceled), "connection or driver error (canceled)"},
		{io.ErrUnexpectedEOF, "connection or driver error (connection closed)"},
		{errors.New("password=hunter2"), "connection or driver error (other)"},
	} {
		if got := stateText(c.err); got != c.want {
			t.Errorf("stateText(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

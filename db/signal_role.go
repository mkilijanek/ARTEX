package db

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The database role of the Signal sidecar. It can read one view and execute three functions of ARTEX's schema
// and nothing else (migration 5004). The server creates and checks it at start, when the operator gave it a
// password; where the server may not create roles, the operator runs the statements that the log line shows
// and the server only verifies. Provisioning never fails the server start: every branch ends in a state.

// States of the role as the server reports them.
const (
	SignalRoleOK        = "ok"
	SignalRoleMissing   = "missing"
	SignalRoleError     = "error"
	SignalRoleUnchecked = "unchecked"
)

// DefaultSignalRole is the role name when none is configured.
const DefaultSignalRole = "artex_signal"

// signalRoleMarker is the comment that marks a role as created for the sidecar; a role without it is never
// altered.
const signalRoleMarker = "artex signal sidecar"

const (
	signalRoleConnLimit = 5
	scramIterations     = 4096
)

var signalRoleNameRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidSignalRoleName reports whether name may be the sidecar role.
func ValidSignalRoleName(name string) bool { return signalRoleNameRE.MatchString(name) }

// signalRoleFunctions are the functions of the public schema that the role may execute: the three of the
// sidecar, and the functions that PostgreSQL makes executable by PUBLIC when they are created and that do
// nothing outside their purpose (try_inet parses text; the others are trigger functions, which PostgreSQL
// refuses to call outside a trigger). A function that a later migration adds appears here, or it revokes PUBLIC.
var signalRoleFunctions = []string{
	"signal_sidecar_heartbeat", "signal_sidecar_set_groups", "signal_sidecar_set_link",
	"set_updated_at", "try_inet", "sync_task_asset_links",
	"stop_deleted_conversation_retest", "stop_deleted_conversation_detection",
}

// SignalRoleConfig is what provisioning needs from the server's environment.
type SignalRoleConfig struct {
	// Role is the role name (ARTEX_SIGNAL_DB_ROLE); empty means DefaultSignalRole.
	Role string
	// Password is ARTEX_SIGNAL_DB_PASSWORD. Empty: verify only, change nothing.
	Password string
	// DSN is the server's own connection string; the test login of the role reuses its endpoint and database.
	DSN string
	// AuthAsserted is ARTEX_SIGNAL_DB_AUTH_ASSERTED=1: the operator states that the database checks passwords,
	// for a server whose authentication rules ARTEX cannot read.
	AuthAsserted bool

	// execHook, set by tests, sees every statement that is sent to change something.
	execHook func(stmt string)
	// hba, set by tests, replaces the read of pg_hba_file_rules.
	hba func(ctx context.Context, q signalQuerier) (rules []hbaRule, readable bool, err error)
}

// SignalRoleStatus is the outcome of provisioning or of a check. Detail is a short text without secrets.
type SignalRoleStatus struct {
	State  string // ok | missing | error | unchecked
	Detail string
	// Auth is what is known about password authentication: unchecked | ok | trust | asserted.
	Auth string
}

type signalQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type signalExecQuerier interface {
	signalQuerier
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// hookedTx reports every statement that changes something to a test.
type hookedTx struct {
	*sql.Tx
	hook func(string)
}

func (h hookedTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if h.hook != nil {
		h.hook(query)
	}
	return h.Tx.ExecContext(ctx, query, args...)
}

// scramVerifier is the SCRAM-SHA-256 verifier of a password, in the form PostgreSQL stores and accepts in
// CREATE ROLE ... PASSWORD. The password is used as given (PostgreSQL applies SASLprep, which changes only
// non-ASCII text).
func scramVerifier(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("derive key: %w", err)
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac(salted, "Client Key"))
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, enc(salt), enc(stored[:]), enc(mac(salted, "Server Key"))), nil
}

func newScramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("random salt: %w", err)
	}
	return scramVerifier(password, salt, scramIterations)
}

func quoteSQLIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteSQLLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// pgCode returns the SQLSTATE of an error, or "".
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// stateText reduces a database error to a text that carries no statement, name or value.
func stateText(err error) string {
	if c := pgCode(err); c == "42501" {
		return "SQLSTATE 42501 (insufficient privilege)"
	} else if c != "" {
		return "SQLSTATE " + c
	}
	return "connection or driver error (" + errClass(err) + ")"
}

// errClass names the kind of a failure that has no SQLSTATE, without any text of the error itself.
func errClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &ne):
		return "network"
	}
	return "other"
}

// hbaRule is one usable line of pg_hba.conf.
type hbaRule struct {
	Type      string
	Databases []string
	Users     []string
	Method    string
}

// hbaTrustMatches reports whether a rule with method trust can apply to a connection of role to database. It is
// deliberately conservative: a name that cannot be resolved here (a file reference) counts as a match, and the
// order of the rules is ignored.
func hbaTrustMatches(rules []hbaRule, database, role string) bool {
	has := func(list []string, name string, extra func(string) bool) bool {
		for _, e := range list {
			if e == "all" || e == name || strings.HasPrefix(e, "@") || (extra != nil && extra(e)) {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		switch r.Type {
		case "local", "host", "hostssl", "hostnossl", "hostgssenc", "hostnogssenc":
		default:
			continue
		}
		if r.Method != "trust" {
			continue
		}
		dbOK := has(r.Databases, database, func(e string) bool { return e == "sameuser" && database == role })
		userOK := has(r.Users, role, nil)
		if dbOK && userOK {
			return true
		}
	}
	return false
}

// readHBA reads pg_hba_file_rules. readable is false when the connected user may not read it.
func readHBA(ctx context.Context, q signalQuerier) (rules []hbaRule, readable bool, err error) {
	rows, err := q.QueryContext(ctx, `SELECT type, to_json(database)::text, to_json(user_name)::text, auth_method
	                                    FROM pg_hba_file_rules WHERE error IS NULL`)
	if err != nil {
		if pgCode(err) == "42501" {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var r hbaRule
		var dbs, users sql.NullString
		var typ, method sql.NullString
		if err := rows.Scan(&typ, &dbs, &users, &method); err != nil {
			return nil, false, err
		}
		r.Type, r.Method = typ.String, method.String
		if dbs.Valid {
			if err := json.Unmarshal([]byte(dbs.String), &r.Databases); err != nil {
				return nil, false, fmt.Errorf("pg_hba_file_rules: database column: %w", err)
			}
		}
		if users.Valid {
			if err := json.Unmarshal([]byte(users.String), &r.Users); err != nil {
				return nil, false, fmt.Errorf("pg_hba_file_rules: user column: %w", err)
			}
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		if pgCode(err) == "42501" {
			return nil, false, nil
		}
		return nil, false, err
	}
	return rules, true, nil
}

// signalRoleFacts is what the catalogs say about a role.
type signalRoleFacts struct {
	exists                                                      bool
	oid                                                         uint32
	super, createDB, createRole, replication, bypassRLS, canLog bool
	inherit                                                     bool
	connLimit                                                   int
	comment                                                     string
	memberships                                                 int
	owned                                                       int
	config                                                      []string
}

func readSignalRoleFacts(ctx context.Context, q signalQuerier, role string) (signalRoleFacts, error) {
	var f signalRoleFacts
	var comment sql.NullString
	var cfg sql.NullString
	err := q.QueryRowContext(ctx, `
SELECT r.oid::bigint, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication, r.rolbypassrls, r.rolcanlogin,
       r.rolinherit, r.rolconnlimit, shobj_description(r.oid, 'pg_authid'),
       (SELECT count(*) FROM pg_auth_members m WHERE m.member = r.oid),
       (SELECT count(*) FROM pg_shdepend d WHERE d.refclassid = 'pg_authid'::regclass AND d.refobjid = r.oid AND d.deptype = 'o'),
       to_json(coalesce(r.rolconfig, ARRAY[]::text[]))::text
FROM pg_roles r WHERE r.rolname = $1`, role).Scan(&f.oid, &f.super, &f.createDB, &f.createRole, &f.replication,
		&f.bypassRLS, &f.canLog, &f.inherit, &f.connLimit, &comment, &f.memberships, &f.owned, &cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return signalRoleFacts{}, nil
	}
	if err != nil {
		return signalRoleFacts{}, err
	}
	f.exists = true
	f.comment = comment.String
	if cfg.Valid {
		if err := json.Unmarshal([]byte(cfg.String), &f.config); err != nil {
			return signalRoleFacts{}, fmt.Errorf("role settings: %w", err)
		}
	}
	return f, nil
}

// forbidden lists what makes an existing role one that an operator must look at.
func (f signalRoleFacts) forbidden() []string {
	var out []string
	for _, a := range []struct {
		on   bool
		name string
	}{{f.super, "SUPERUSER"}, {f.createDB, "CREATEDB"}, {f.createRole, "CREATEROLE"},
		{f.replication, "REPLICATION"}, {f.bypassRLS, "BYPASSRLS"}} {
		if a.on {
			out = append(out, a.name)
		}
	}
	if f.memberships > 0 {
		out = append(out, "membership in another role")
	}
	if f.owned > 0 {
		out = append(out, "owned objects")
	}
	return out
}

func (f signalRoleFacts) hasSetting(name, value string) bool {
	want := name + "=" + value
	for _, c := range f.config {
		if c == want {
			return true
		}
	}
	return false
}

// signalRoleSettings are the per-role settings. A per-role statement_timeout is not relied on: a session can
// override it.
var signalRoleSettings = [][2]string{
	{"idle_in_transaction_session_timeout", "10s"},
	{"search_path", "public"},
}

// signalRoleProblems checks every attribute and privilege that the least-privilege contract names, as the
// server sees them (effective privileges, PUBLIC included). It returns one short text per problem.
func signalRoleProblems(ctx context.Context, q signalQuerier, role string, f signalRoleFacts) ([]string, error) {
	var problems []string
	if f.forbidden() != nil {
		problems = append(problems, "role has "+strings.Join(f.forbidden(), ", "))
	}
	if !f.canLog {
		problems = append(problems, "role cannot log in")
	}
	if f.inherit {
		problems = append(problems, "role has INHERIT")
	}
	if f.connLimit != signalRoleConnLimit {
		problems = append(problems, fmt.Sprintf("connection limit is %d, want %d", f.connLimit, signalRoleConnLimit))
	}
	if f.comment != signalRoleMarker {
		problems = append(problems, "ownership marker is missing")
	}
	for _, s := range signalRoleSettings {
		if !f.hasSetting(s[0], s[1]) {
			problems = append(problems, "setting "+s[0]+" is not set")
		}
	}

	// Relations of every user schema: no privilege at all, except SELECT on the one view.
	rows, err := q.QueryContext(ctx, `
SELECT n.nspname || '.' || c.relname
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
  AND c.relkind IN ('r','p','v','m','f','S')
  AND CASE
        WHEN c.relkind = 'S' THEN has_sequence_privilege($1::name, c.oid, 'USAGE,SELECT,UPDATE')
        WHEN n.nspname = 'public' AND c.relname = 'signal_sidecar_config_v' THEN
             has_table_privilege($1::name, c.oid, 'INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
             OR has_any_column_privilege($1::name, c.oid, 'INSERT,UPDATE,REFERENCES')
             OR NOT has_table_privilege($1::name, c.oid, 'SELECT')
        ELSE has_table_privilege($1::name, c.oid, 'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
             OR has_any_column_privilege($1::name, c.oid, 'SELECT,INSERT,UPDATE,REFERENCES')
      END
ORDER BY 1 LIMIT 10`, role)
	if err != nil {
		return nil, err
	}
	var rels []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		rels = append(rels, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rels) > 0 {
		problems = append(problems, "unexpected relation privileges on "+strings.Join(rels, ", "))
	}

	var schemaCreate, dbCreate, dbTemp bool
	if err := q.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_namespace n WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
                AND has_schema_privilege($1::name, n.oid, 'CREATE')),
       has_database_privilege($1::name, current_database(), 'CREATE'),
       has_database_privilege($1::name, current_database(), 'TEMPORARY')`, role).Scan(&schemaCreate, &dbCreate, &dbTemp); err != nil {
		return nil, err
	}
	if schemaCreate {
		problems = append(problems, "role can create objects in a schema")
	}
	if dbCreate {
		problems = append(problems, "role can create schemas")
	}
	if dbTemp {
		problems = append(problems, "role can create temporary tables")
	}

	frows, err := q.QueryContext(ctx, `
SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
  AND has_function_privilege($1::name, p.oid, 'EXECUTE')
ORDER BY 1`, role)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, n := range signalRoleFunctions {
		allowed[n] = true
	}
	var funcs []string
	for frows.Next() {
		var n string
		if err := frows.Scan(&n); err != nil {
			frows.Close()
			return nil, err
		}
		if !allowed[n] {
			funcs = append(funcs, n)
		}
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return nil, err
	}
	if len(funcs) > 0 {
		sort.Strings(funcs)
		if len(funcs) > 10 {
			funcs = funcs[:10]
		}
		problems = append(problems, "unexpected EXECUTE on "+strings.Join(funcs, ", "))
	}
	var missing int
	if err := q.QueryRowContext(ctx, `
SELECT count(*) FROM unnest(ARRAY['signal_sidecar_heartbeat','signal_sidecar_set_link','signal_sidecar_set_groups']) AS f(name)
WHERE NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                   WHERE n.nspname = 'public' AND p.proname = f.name AND has_function_privilege($1::name, p.oid, 'EXECUTE'))`, role).Scan(&missing); err != nil {
		return nil, err
	}
	if missing > 0 {
		problems = append(problems, "a sidecar function is not executable")
	}
	return problems, nil
}

// signalGrantsMissing reports whether the one view or one of the three functions lacks its grant.
func signalGrantsMissing(ctx context.Context, q signalQuerier, role string) (bool, error) {
	var missing bool
	err := q.QueryRowContext(ctx, `
SELECT NOT has_table_privilege($1::name, 'public.signal_sidecar_config_v', 'SELECT')
    OR EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                WHERE n.nspname = 'public'
                  AND p.proname IN ('signal_sidecar_heartbeat','signal_sidecar_set_link','signal_sidecar_set_groups')
                  AND NOT has_function_privilege($1::name, p.oid, 'EXECUTE'))`, role).Scan(&missing)
	return missing, err
}

// signalManualSQL is the statement list that the log line shows where the server may not create the role.
func signalManualSQL(role string) string {
	r := quoteSQLIdent(role)
	return "CREATE ROLE " + r + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 5 PASSWORD '<password>'; " +
		"COMMENT ON ROLE " + r + " IS '" + signalRoleMarker + "'; " +
		"ALTER ROLE " + r + " SET idle_in_transaction_session_timeout = '10s'; ALTER ROLE " + r + " SET search_path = public; " +
		"SELECT signal_sidecar_grant(" + quoteSQLLiteral(role) + "); " +
		"REVOKE TEMPORARY ON DATABASE <database> FROM PUBLIC (and, in the same transaction, UPDATE signal_sidecar_config SET public_temp_revoked = true, only when PUBLIC held the privilege)"
}

// SignalRoleState checks the role as it is, changing nothing: absent is 'missing', a role that differs from the
// contract is 'error' with the first differences named, a conforming role is 'ok'. Auth is read from the
// configuration row.
func (d *DB) SignalRoleState(ctx context.Context, role string) SignalRoleStatus {
	if role == "" {
		role = DefaultSignalRole
	}
	st := SignalRoleStatus{State: SignalRoleError, Auth: "unchecked"}
	if !ValidSignalRoleName(role) {
		st.Detail = "the role name is not valid"
		return st
	}
	var auth string
	if err := d.QueryRowContext(ctx, `SELECT db_auth_state FROM signal_sidecar_config WHERE id`).Scan(&auth); err == nil {
		st.Auth = auth
	}
	return d.signalRoleCheck(ctx, d.DB, role, st)
}

func (d *DB) signalRoleCheck(ctx context.Context, q signalQuerier, role string, st SignalRoleStatus) SignalRoleStatus {
	f, err := readSignalRoleFacts(ctx, q, role)
	if err != nil {
		st.State, st.Detail = SignalRoleError, "cannot read the role: "+stateText(err)
		return st
	}
	if !f.exists {
		st.State, st.Detail = SignalRoleMissing, "the role does not exist"
		return st
	}
	problems, err := signalRoleProblems(ctx, q, role, f)
	if err != nil {
		st.State, st.Detail = SignalRoleError, "cannot check the role: "+stateText(err)
		return st
	}
	if len(problems) > 0 {
		st.State, st.Detail = SignalRoleError, strings.Join(problems, "; ")
		return st
	}
	st.State, st.Detail = SignalRoleOK, ""
	return st
}

// ProvisionSignalRole creates or checks the role of the sidecar and reports a state; it never returns an error
// and never fails the start. Steps, each ending in a state: refuse a role that is the connected user; refuse a
// role that exists without the ownership marker, or with a forbidden attribute, a membership or an owned object;
// refuse when PUBLIC may create objects in schema public or when pg_hba.conf lets the role in without a password;
// create the role, or alter only what differs; check the password by a test login and change it only when that
// fails; grant through signal_sidecar_grant when something is missing; revoke TEMPORARY from PUBLIC once; check
// the result. With an empty password nothing is changed.
func (d *DB) ProvisionSignalRole(ctx context.Context, cfg SignalRoleConfig) SignalRoleStatus {
	st := d.provisionSignalRole(ctx, cfg)
	switch st.State {
	case SignalRoleOK:
		log.Printf("[signal] database role ready")
	case SignalRoleMissing:
		log.Printf("[signal] database role missing and cannot be created by this database user; run once as a user who may: %s", signalManualSQL(roleOrDefault(cfg.Role)))
	case SignalRoleUnchecked:
		log.Printf("[signal] database role not checked: %s", st.Detail)
	default:
		log.Printf("[signal] database role: %s", st.Detail)
	}
	return st
}

func roleOrDefault(role string) string {
	if role == "" {
		return DefaultSignalRole
	}
	return role
}

func (d *DB) provisionSignalRole(ctx context.Context, cfg SignalRoleConfig) SignalRoleStatus {
	role := roleOrDefault(cfg.Role)
	st := SignalRoleStatus{State: SignalRoleError, Auth: "unchecked"}
	fail := func(detail string) SignalRoleStatus {
		st.State, st.Detail = SignalRoleError, detail
		return st
	}
	if !ValidSignalRoleName(role) {
		return fail("the role name is not valid (lower-case letters, digits and underscore, at most 63)")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fail("cannot start a transaction: " + stateText(err))
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	q := hookedTx{Tx: tx, hook: cfg.execHook}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`, signalLockTimeout); err != nil {
		return fail("cannot set the lock timeout: " + stateText(err))
	}
	// Two servers that start together provision one after the other, so neither sees the other's half-done work.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		return fail("cannot take the provisioning lock: " + stateText(err))
	}

	// 2: the role is never the user that is connected.
	var cur, sess, dbName string
	if err := tx.QueryRowContext(ctx, `SELECT current_user, session_user, current_database()`).Scan(&cur, &sess, &dbName); err != nil {
		return fail("cannot read the connected user: " + stateText(err))
	}
	if role == cur || role == sess {
		return fail("the sidecar role is the user ARTEX is connected as; choose another role name")
	}

	// 3: the role as it is.
	f, err := readSignalRoleFacts(ctx, tx, role)
	if err != nil {
		return fail("cannot read the role: " + stateText(err))
	}
	if f.exists {
		if f.comment != signalRoleMarker {
			return fail("a role of that name exists that this server did not create; it is left unchanged")
		}
		if bad := f.forbidden(); bad != nil {
			return fail("the role has " + strings.Join(bad, ", ") + "; an operator must look at it, nothing was changed")
		}
	}

	verifyOnly := cfg.Password == ""
	if verifyOnly {
		st = d.signalRoleCheck(ctx, tx, role, st)
		if st.State == SignalRoleMissing {
			st.State, st.Detail = SignalRoleUnchecked, "no password is configured and the role does not exist"
		}
		return st
	}

	// 4: PUBLIC must not create objects in schema public.
	var publicCreate bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace n, aclexplode(n.nspacl) a
	                                                  WHERE n.nspname = 'public' AND a.grantee = 0 AND a.privilege_type = 'CREATE')`).Scan(&publicCreate); err != nil {
		return fail("cannot read the schema privileges: " + stateText(err))
	}
	if publicCreate {
		return fail("PUBLIC may create objects in schema public; run REVOKE CREATE ON SCHEMA public FROM PUBLIC, then restart")
	}

	// 5: authentication rules.
	hba := cfg.hba
	if hba == nil {
		hba = readHBA
	}
	rules, readable, err := hba(ctx, tx)
	if err != nil {
		return fail("cannot read the authentication rules: " + stateText(err))
	}
	switch {
	case readable && hbaTrustMatches(rules, dbName, role):
		st.Auth = "trust"
		if err := recordSignalAuth(ctx, q, st.Auth); err != nil {
			return fail(err.Error())
		}
		if err := tx.Commit(); err != nil {
			return fail("cannot commit: " + stateText(err))
		}
		st.Detail = "pg_hba.conf lets this role in without a password (method trust); switch the rules to scram-sha-256, then restart"
		return st
	case readable:
		st.Auth = "ok"
	case cfg.AuthAsserted:
		st.Auth = "asserted"
	default:
		st.Auth = "unchecked"
	}

	// 6 and 7: create the role, or alter only what differs.
	passwordChanged := false
	if !f.exists {
		verifier, err := newScramVerifier(cfg.Password)
		if err != nil {
			return fail("cannot hash the password: " + stateText(err))
		}
		r := quoteSQLIdent(role)
		stmts := []string{
			"CREATE ROLE " + r + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 5 PASSWORD " + quoteSQLLiteral(verifier),
			"COMMENT ON ROLE " + r + " IS " + quoteSQLLiteral(signalRoleMarker),
		}
		for _, s := range signalRoleSettings {
			stmts = append(stmts, "ALTER ROLE "+r+" SET "+s[0]+" = "+quoteSQLLiteral(s[1]))
		}
		for i, s := range stmts {
			if _, err := q.ExecContext(ctx, s); err != nil {
				if i == 0 && pgCode(err) == "42501" {
					st.State, st.Detail = SignalRoleMissing, "this database user may not create roles"
					return st
				}
				return fail("cannot create the role: " + stateText(err))
			}
		}
	} else {
		var clauses []string
		if !f.canLog {
			clauses = append(clauses, "LOGIN")
		}
		if f.inherit {
			clauses = append(clauses, "NOINHERIT")
		}
		if f.connLimit != signalRoleConnLimit {
			clauses = append(clauses, fmt.Sprintf("CONNECTION LIMIT %d", signalRoleConnLimit))
		}
		r := quoteSQLIdent(role)
		if len(clauses) > 0 {
			if _, err := q.ExecContext(ctx, "ALTER ROLE "+r+" "+strings.Join(clauses, " ")); err != nil {
				return fail("cannot alter the role: " + stateText(err))
			}
		}
		for _, s := range signalRoleSettings {
			if f.hasSetting(s[0], s[1]) {
				continue
			}
			if _, err := q.ExecContext(ctx, "ALTER ROLE "+r+" SET "+s[0]+" = "+quoteSQLLiteral(s[1])); err != nil {
				return fail("cannot alter the role settings: " + stateText(err))
			}
		}
		// The password is checked by a login, not by a statement.
		ok, err := d.signalLoginWorks(ctx, cfg, role)
		if err != nil {
			return fail("cannot test the role's login: " + stateText(err))
		}
		if !ok {
			verifier, err := newScramVerifier(cfg.Password)
			if err != nil {
				return fail("cannot hash the password: " + stateText(err))
			}
			if _, err := q.ExecContext(ctx, "ALTER ROLE "+r+" PASSWORD "+quoteSQLLiteral(verifier)); err != nil {
				return fail("cannot set the role's password: " + stateText(err))
			}
			passwordChanged = true
		}
	}

	// 8: grants, then the one database-wide revoke.
	missing, err := signalGrantsMissing(ctx, tx, role)
	if err != nil {
		return fail("cannot read the grants: " + stateText(err))
	}
	if missing {
		if _, err := q.ExecContext(ctx, `SELECT public.signal_sidecar_grant($1::text::regrole)`, role); err != nil {
			return fail("cannot grant to the role: " + stateText(err))
		}
	}
	var tempOnPublic bool
	if err := tx.QueryRowContext(ctx, `SELECT d.datacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a WHERE a.grantee = 0 AND a.privilege_type = 'TEMPORARY')
	                                     FROM pg_database d WHERE d.datname = current_database()`).Scan(&tempOnPublic); err != nil {
		return fail("cannot read the database privileges: " + stateText(err))
	}
	if tempOnPublic {
		if _, err := q.ExecContext(ctx, "REVOKE TEMPORARY ON DATABASE "+quoteSQLIdent(dbName)+" FROM PUBLIC"); err != nil {
			return fail("cannot revoke TEMPORARY from PUBLIC: " + stateText(err))
		}
		if res, err := q.ExecContext(ctx, `UPDATE signal_sidecar_config SET public_temp_revoked = true, updated_at = now() WHERE id`); err != nil {
			return fail("cannot record the revoke: " + stateText(err))
		} else if n, _ := res.RowsAffected(); n == 0 {
			return fail("cannot record the revoke: configuration row missing")
		}
	}
	if err := recordSignalAuth(ctx, q, st.Auth); err != nil {
		return fail(err.Error())
	}

	// 9: the final check, before the commit, so that a failure leaves nothing half done.
	st = d.signalRoleCheck(ctx, tx, role, st)
	if st.State != SignalRoleOK {
		return st
	}
	if err := tx.Commit(); err != nil {
		return fail("cannot commit: " + stateText(err))
	}
	if passwordChanged {
		ok, err := d.signalLoginWorks(ctx, cfg, role)
		if err != nil {
			return fail("cannot test the role's login after setting the password: " + stateText(err))
		}
		if !ok {
			return fail("the role's login still fails after the password was set")
		}
	}
	return st
}

// recordSignalAuth stores the authentication state in the configuration row when it differs; when it does not,
// no statement is sent.
func recordSignalAuth(ctx context.Context, q signalExecQuerier, auth string) error {
	var cur string
	if err := q.QueryRowContext(ctx, `SELECT db_auth_state FROM signal_sidecar_config WHERE id`).Scan(&cur); err != nil {
		return fmt.Errorf("cannot read the authentication state: %s", stateText(err))
	}
	if cur == auth {
		return nil
	}
	res, err := q.ExecContext(ctx, `UPDATE signal_sidecar_config SET db_auth_state = $1, updated_at = now() WHERE id`, auth)
	if err != nil {
		return fmt.Errorf("cannot record the authentication state: %s", stateText(err))
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.New("cannot record the authentication state: configuration row missing")
	}
	return nil
}

// signalLoginWorks logs in as the role with the configured password over the endpoint of the server's own DSN.
// A failed password (28P01) is (false, nil); any other failure, 28000 included, is an error.
func (d *DB) signalLoginWorks(ctx context.Context, cfg SignalRoleConfig, role string) (bool, error) {
	cc, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return false, errors.New("the DSN cannot be parsed")
	}
	cc.User, cc.Password = role, cfg.Password
	conn := stdlib.OpenDB(*cc)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if err := conn.PingContext(ctx); err != nil {
		if c := pgCode(err); c == "28P01" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

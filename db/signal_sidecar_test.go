package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Migration 5004: two single-row tables, one view, three functions for the sidecar and one grant helper. The
// tests run the functions as the sidecar's own database role, so that the grants are part of what is tested.

const sidecarMigration = "5004_signal_sidecar.sql"

const (
	testNumber = "+15555550101"
	testACI    = "00000000-0000-4000-8000-000000000001"
	// A link address shaped like the one signal-cli prints: 130 characters, with a comma in the capabilities.
	testLinkURI = "sgnl://linkdevice?uuid=AbCdEfGhIjKlMnOpQrStUv&pub_key=BQabcdefghijklmnopqrstuvwxyz0123456789%2Bxyz&capabilities=backup,backup2,xyz"
)

func noTrustHBA(context.Context, signalQuerier) ([]hbaRule, bool, error) { return nil, true, nil }

func signalCfg(tmp *dbtest.Temp, r *dbtest.SidecarRole) SignalRoleConfig {
	return SignalRoleConfig{Role: r.Name, Password: r.Password, DSN: tmp.DSN, hba: noTrustHBA}
}

// signalFresh is a database that started with the embedded migrations only.
func signalFresh(t *testing.T, label string) (*DB, *dbtest.Temp, *dbtest.Server) {
	t.Helper()
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, label)
	return startup(t, tmp.DSN), tmp, srv
}

// signalUpgraded is a database that ran every migration but 5004 first, and then started with the full set.
func signalUpgraded(t *testing.T, label string) (*DB, *dbtest.Temp, *dbtest.Server) {
	t.Helper()
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, label)
	startMigrated(t, tmp, migrationSet(t, fstest.MapFS{sidecarMigration: nil}))
	return startup(t, tmp.DSN), tmp, srv
}

// signalProvisioned is a fresh database with the role created and the connection of the role.
func signalProvisioned(t *testing.T, label string) (*DB, *dbtest.Temp, *sql.DB) {
	t.Helper()
	d, tmp, srv := signalFresh(t, label)
	role := srv.NewSidecarRole(t, tmp)
	if st := d.ProvisionSignalRole(context.Background(), signalCfg(tmp, role)); st.State != SignalRoleOK {
		t.Fatalf("provisioning: %+v", st)
	}
	return d, tmp, signalRoleConn(t, tmp, role)
}

func signalRoleConn(t *testing.T, tmp *dbtest.Temp, role *dbtest.SidecarRole) *sql.DB {
	t.Helper()
	dsn, err := tmp.DSNFor(role.Name, role.Password)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func signalExec(t *testing.T, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, stmt string, args ...any) {
	t.Helper()
	if _, err := q.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("%.80s: %s", stmt, dbtest.AtomicErrorSummary(err))
	}
}

func requireSQLState(t *testing.T, what string, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != want {
		t.Errorf("%s: err = %s, want SQLSTATE %s", what, dbtest.AtomicErrorSummary(err), want)
	}
}

// resetSignal puts both rows back to their defaults.
func resetSignal(t *testing.T, d *DB) {
	t.Helper()
	signalExec(t, d, `DELETE FROM signal_sidecar_config`)
	signalExec(t, d, `DELETE FROM signal_sidecar_status`)
	signalExec(t, d, `INSERT INTO signal_sidecar_config DEFAULT VALUES`)
	signalExec(t, d, `INSERT INTO signal_sidecar_status DEFAULT VALUES`)
}

func heartbeat(rc *sql.DB, state string, gen int64, number, errText, remote string) error {
	_, err := rc.ExecContext(context.Background(), `SELECT public.signal_sidecar_heartbeat($1, $2, $3, $4, $5, $6, $7, $8)`,
		state, gen, number, testACI, errText, remote, "sidecar-test", "cli-test")
	return err
}

func setLink(rc *sql.DB, gen int64, uri string) (bool, error) {
	var ok bool
	err := rc.QueryRowContext(context.Background(), `SELECT public.signal_sidecar_set_link($1, $2)`, gen, uri).Scan(&ok)
	return ok, err
}

func setGroups(rc *sql.DB, refreshGen int64, groups string) (bool, error) {
	var ok bool
	err := rc.QueryRowContext(context.Background(), `SELECT public.signal_sidecar_set_groups($1, $2::jsonb)`, refreshGen, groups).Scan(&ok)
	return ok, err
}

// groupID is a well-formed id: base64 of the base64 of 32 bytes.
func groupID(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	inner := base64.StdEncoding.EncodeToString(raw)
	return "group." + base64.StdEncoding.EncodeToString([]byte(inner))
}

func groupsJSON(names ...string) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"name":%q}`, groupID(byte(i+1)), n))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

type statusRow struct {
	state, number, aci, remote, lastErr, uri string
	handled, groupsGen                       int64
	uriNull, hbNull                          bool
	groups                                   string
}

func readStatus(t *testing.T, d *DB) statusRow {
	t.Helper()
	var r statusRow
	var uri sql.NullString
	var hb sql.NullTime
	if err := d.QueryRow(`SELECT state, account_number, account_aci, remote_unlink, last_error, link_uri, handled_gen,
	                             groups_gen, heartbeat_at, groups::text FROM signal_sidecar_status`).
		Scan(&r.state, &r.number, &r.aci, &r.remote, &r.lastErr, &uri, &r.handled, &r.groupsGen, &hb, &r.groups); err != nil {
		t.Fatalf("read status: %s", dbtest.AtomicErrorSummary(err))
	}
	r.uri, r.uriNull, r.hbNull = uri.String, !uri.Valid, !hb.Valid
	return r
}

// ---- schema ----

// Both a fresh database and one upgraded from the migration before end with the same objects and the ledger row.
func TestSignalSidecarSchemaOnFreshAndUpgradedDatabase(t *testing.T) {
	for _, mode := range []string{"fresh", "upgraded"} {
		t.Run(mode, func(t *testing.T) {
			var d *DB
			if mode == "fresh" {
				d, _, _ = signalFresh(t, "sigschema")
			} else {
				d, _, _ = signalUpgraded(t, "sigschemaup")
			}
			var tables, views, funcs, ledger int
			if err := d.QueryRow(`SELECT
  (SELECT count(*) FROM pg_class WHERE relname IN ('signal_sidecar_config','signal_sidecar_status') AND relkind = 'r'),
  (SELECT count(*) FROM pg_class WHERE relkind = 'v' AND relnamespace = 'public'::regnamespace),
  (SELECT count(*) FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'signal\_sidecar\_%'),
  (SELECT count(*) FROM schema_migrations WHERE version = 5004 AND name = $1)`, sidecarMigration).Scan(&tables, &views, &funcs, &ledger); err != nil {
				t.Fatal(err)
			}
			if tables != 2 || views != 1 || funcs != 4 || ledger != 1 {
				t.Errorf("tables %d, views %d, functions %d, ledger rows %d; want 2, 1, 4, 1", tables, views, funcs, ledger)
			}
			var cfgRows, stRows int
			if err := d.QueryRow(`SELECT (SELECT count(*) FROM signal_sidecar_config), (SELECT count(*) FROM signal_sidecar_status)`).Scan(&cfgRows, &stRows); err != nil || cfgRows != 1 || stRows != 1 {
				t.Errorf("rows: config %d, status %d, err %v; want one each", cfgRows, stRows, err)
			}
		})
	}
	content := mustReadEmbedded(t, sidecarMigration)
	if flag, ok := migrationDowngradeFlag(content); !ok || flag != "yes" {
		t.Errorf("the migration's first line does not state downgrade-safe: yes")
	}
}

// The table CHECK constraints refuse each state that the protocol never produces.
func TestSignalSidecarTableChecksRefuseInvalidRows(t *testing.T) {
	d, _, _ := signalFresh(t, "sigchecks")
	bad := []struct{ name, stmt string }{
		{"link address outside the linking state", `UPDATE signal_sidecar_status SET link_uri = 'x'`},
		{"number outside the linked state", `UPDATE signal_sidecar_status SET account_number = '` + testNumber + `'`},
		{"account id outside the linked state", `UPDATE signal_sidecar_status SET account_aci = '` + testACI + `'`},
		{"groups that are not an array", `UPDATE signal_sidecar_status SET groups = '{}'::jsonb`},
		{"groups over 128 KiB", `UPDATE signal_sidecar_status SET groups = (SELECT jsonb_agg(repeat('a', 100)) FROM generate_series(1, 2000))`},
		{"error over 500 characters", `UPDATE signal_sidecar_status SET last_error = repeat('x', 501)`},
		{"negative generation", `UPDATE signal_sidecar_config SET gen = -1`},
		{"negative handled generation", `UPDATE signal_sidecar_status SET handled_gen = -1`},
		{"negative refresh generation", `UPDATE signal_sidecar_config SET groups_refresh_gen = -1`},
		{"link lifetime below 30", `UPDATE signal_sidecar_config SET link_ttl_seconds = 29`},
		{"link lifetime above 120", `UPDATE signal_sidecar_config SET link_ttl_seconds = 121`},
		{"unknown desired value", `UPDATE signal_sidecar_config SET desired = 'bogus'`},
		{"unknown state", `UPDATE signal_sidecar_status SET state = 'bogus'`},
		{"unknown authentication state", `UPDATE signal_sidecar_config SET db_auth_state = 'bogus'`},
		{"device name with a semicolon", `UPDATE signal_sidecar_config SET device_name = 'a;b'`},
		{"confirmed account that is not E.164", `UPDATE signal_sidecar_config SET confirmed_account = '12345'`},
		{"unknown remote unlink value", `UPDATE signal_sidecar_status SET remote_unlink = 'bogus'`},
		{"second configuration row", `INSERT INTO signal_sidecar_config (id) VALUES (false)`},
	}
	for _, c := range bad {
		_, err := d.Exec(c.stmt)
		requireSQLState(t, c.name, err, "23514")
	}
	_, err := d.Exec(`INSERT INTO signal_sidecar_status DEFAULT VALUES`)
	requireSQLState(t, "second status row", err, "23505")
	good := []string{
		`UPDATE signal_sidecar_config SET link_ttl_seconds = 30`,
		`UPDATE signal_sidecar_config SET link_ttl_seconds = 120`,
		`UPDATE signal_sidecar_status SET last_error = repeat('x', 500)`,
		`UPDATE signal_sidecar_status SET remote_unlink = 'local_only'`,
		`UPDATE signal_sidecar_status SET state = 'linking', link_uri = 'x'`,
		`UPDATE signal_sidecar_status SET state = 'linked', link_uri = NULL, account_number = '` + testNumber + `', account_aci = '` + testACI + `'`,
	}
	for _, stmt := range good {
		signalExec(t, d, stmt)
	}
}

// ---- function contracts ----

// Every function carries the catalog facts the contract names.
func TestSignalSidecarFunctionCatalogFacts(t *testing.T) {
	d, _, _ := signalFresh(t, "sigcatalog")
	rows, err := d.Query(`SELECT proname, prosecdef, coalesce(array_to_string(proconfig, ','), ''), proacl IS NULL,
	       EXISTS (SELECT 1 FROM aclexplode(coalesce(proacl, acldefault('f', proowner))) a WHERE a.grantee = 0)
	  FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND proname LIKE 'signal\_sidecar\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, cfg string
		var secdef, aclNull, public bool
		if err := rows.Scan(&name, &secdef, &cfg, &aclNull, &public); err != nil {
			t.Fatal(err)
		}
		n++
		wantDefiner := name != "signal_sidecar_grant"
		if secdef != wantDefiner {
			t.Errorf("%s: security definer = %v, want %v", name, secdef, wantDefiner)
		}
		if cfg != "search_path=pg_catalog, pg_temp" {
			t.Errorf("%s: proconfig = %q, want the fixed search_path", name, cfg)
		}
		if aclNull || public {
			t.Errorf("%s: executable by PUBLIC (acl null %v, public entry %v)", name, aclNull, public)
		}
	}
	if err := rows.Err(); err != nil || n != 4 {
		t.Errorf("functions seen %d, err %v; want 4", n, err)
	}
}

// signal_sidecar_set_link: the command table's preconditions, one refused case each.
func TestSignalSidecarSetLinkPreconditions(t *testing.T) {
	d, _, rc := signalProvisioned(t, "siglink")
	ctx := context.Background()
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}

	// desired is 'idle': refused, nothing stored.
	if ok, err := setLink(rc, 0, testLinkURI); err != nil || ok {
		t.Fatalf("set_link without a command: ok=%v err=%v, want false", ok, err)
	}
	if r := readStatus(t, d); r.state != "unlinked" || !r.uriNull {
		t.Fatalf("a refused set_link changed the row: %+v", r)
	}

	gen, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester")
	if err != nil || !applied || gen != 1 {
		t.Fatalf("RequestSignalLink = %d, %v, %v", gen, applied, err)
	}
	// Another generation: refused.
	if ok, err := setLink(rc, gen+1, testLinkURI); err != nil || ok {
		t.Fatalf("set_link for a newer generation: ok=%v err=%v, want false", ok, err)
	}
	if ok, err := setLink(rc, gen-1, testLinkURI); err != nil || ok {
		t.Fatalf("set_link for an older generation: ok=%v err=%v, want false", ok, err)
	}
	// The current generation: stored, and the state is 'linking' in the same statement.
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || !ok {
		t.Fatalf("set_link: ok=%v err=%v, want true", ok, err)
	}
	r := readStatus(t, d)
	if r.state != "linking" || r.uri != testLinkURI {
		t.Fatalf("after set_link: state %q, address stored %v", r.state, r.uri == testLinkURI)
	}
	var ttl, left float64
	if err := d.QueryRow(`SELECT c.link_ttl_seconds, extract(epoch FROM s.link_expires_at - now())
	                        FROM signal_sidecar_config c, signal_sidecar_status s`).Scan(&ttl, &left); err != nil || left > ttl || left < ttl-10 {
		t.Errorf("link expiry is %.1f s ahead, lifetime %.0f s, err %v", left, ttl, err)
	}
	// A cancelled command (desired back to idle): a late set_link is refused.
	if cancelled, err := d.CancelSignalLink(ctx); err != nil || !cancelled {
		t.Fatalf("CancelSignalLink = %v, %v", cancelled, err)
	}
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || ok {
		t.Fatalf("set_link after the cancel: ok=%v err=%v, want false", ok, err)
	}
	// A generation the sidecar has already finished: refused.
	signalExec(t, d, `UPDATE signal_sidecar_config SET desired = 'link'`)
	if err := heartbeat(rc, "unlinked", gen, "", "link cancelled", ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || ok {
		t.Fatalf("set_link for a finished generation: ok=%v err=%v, want false", ok, err)
	}
}

// The address rule accepts a comma (the live address has one), and refuses everything else outside the set.
func TestSignalSidecarSetLinkAddressPattern(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigurirule")
	ctx := context.Background()
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	gen, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester")
	if err != nil || !applied {
		t.Fatalf("RequestSignalLink = %v, %v", applied, err)
	}
	prefix := "sgnl://linkdevice?"
	ok := map[string]string{
		"the live shape (130 characters, with a comma)": testLinkURI,
		"one character":             prefix + "a",
		"400 characters":            prefix + strings.Repeat("a", 400),
		"every allowed punctuation": prefix + "a%&=_.~+/,-",
	}
	if len(testLinkURI) != 130 {
		t.Fatalf("the test address has %d characters, want 130", len(testLinkURI))
	}
	for name, uri := range ok {
		if stored, err := setLink(rc, gen, uri); err != nil || !stored {
			t.Errorf("%s: stored=%v err=%v, want accepted", name, stored, err)
		}
	}
	refused := map[string]string{
		"another scheme":     "https://linkdevice?a",
		"another host":       "sgnl://other?a",
		"no parameters":      prefix,
		"401 characters":     prefix + strings.Repeat("a", 401),
		"a space":            prefix + "a b",
		"a newline":          prefix + "a\nb",
		"an angle bracket":   prefix + "a<b",
		"a quote":            prefix + "a'b",
		"a semicolon":        prefix + "a;b",
		"a trailing newline": prefix + "a\n",
		"empty":              "",
	}
	for name, uri := range refused {
		_, err := setLink(rc, gen, uri)
		requireSQLState(t, name, err, "22023")
	}
}

// signal_sidecar_heartbeat: refusals and effects.
func TestSignalSidecarHeartbeatContract(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sighb")
	ctx := context.Background()

	requireSQLState(t, "unknown state", heartbeat(rc, "bogus", 0, "", "", ""), "22023")
	requireSQLState(t, "state unknown is not reportable", heartbeat(rc, "unknown", 0, "", "", ""), "22023")
	requireSQLState(t, "number that is not E.164", heartbeat(rc, "unlinked", 0, "12345", "", ""), "22023")
	requireSQLState(t, "linked without a number", heartbeat(rc, "linked", 0, "", "", ""), "22023")
	requireSQLState(t, "unknown remote unlink value", heartbeat(rc, "unlinked", 0, "", "", "bogus"), "22023")
	requireSQLState(t, "generation ahead of the command", heartbeat(rc, "unlinked", 5, "", "", ""), "55000")
	requireSQLState(t, "linking when not linking", heartbeat(rc, "linking", 0, "", "", ""), "55000")

	// An error is cut to 500 characters and a link address in it is replaced; the heartbeat time is the database's.
	long := "failure " + testLinkURI + " " + strings.Repeat("x", 700)
	if err := heartbeat(rc, "error", 0, "", long, ""); err != nil {
		t.Fatal(err)
	}
	r := readStatus(t, d)
	if len(r.lastErr) != 500 || strings.Contains(r.lastErr, "sgnl://") || strings.Contains(r.lastErr, "linkdevice") || r.hbNull {
		t.Errorf("stored error: %d characters, address left: %v, heartbeat unset: %v", len(r.lastErr), strings.Contains(r.lastErr, "sgnl://"), r.hbNull)
	}
	var skew float64
	if err := d.QueryRow(`SELECT abs(extract(epoch FROM now() - heartbeat_at)) FROM signal_sidecar_status`).Scan(&skew); err != nil || skew > 5 {
		t.Errorf("heartbeat_at is %.1f s from the database clock (err %v)", skew, err)
	}

	// A link, then the account: the address is cleared; a keep-alive during the link keeps it.
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	gen, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester")
	if err != nil || !applied {
		t.Fatalf("RequestSignalLink = %v, %v", applied, err)
	}
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || !ok {
		t.Fatalf("set_link: %v %v", ok, err)
	}
	if err := heartbeat(rc, "linking", 0, "", "", ""); err != nil {
		t.Fatalf("keep-alive: %v", err)
	}
	if r := readStatus(t, d); r.state != "linking" || r.uri != testLinkURI {
		t.Errorf("the keep-alive changed the link: %+v", r.state)
	}
	if err := heartbeat(rc, "linked", gen, testNumber, "", ""); err != nil {
		t.Fatal(err)
	}
	r = readStatus(t, d)
	if r.state != "linked" || !r.uriNull || r.number != testNumber || r.aci != testACI || r.handled != gen {
		t.Errorf("after linked: %+v", r)
	}
	// A stale generation is refused.
	requireSQLState(t, "stale generation", heartbeat(rc, "linked", gen-1, testNumber, "", ""), "55000")

	// Groups are emptied by 'unlinked' only; the remote unlink value is kept only with 'unlinked'.
	signalExec(t, d, `UPDATE signal_sidecar_config SET groups_refresh_gen = 1`)
	if ok, err := setGroups(rc, 1, groupsJSON("one", "two")); err != nil || !ok {
		t.Fatalf("set_groups: %v %v", ok, err)
	}
	if err := heartbeat(rc, "linked", gen, testNumber, "", "local_only"); err != nil {
		t.Fatal(err)
	}
	if r := readStatus(t, d); r.remote != "" || !strings.Contains(r.groups, "one") {
		t.Errorf("linked heartbeat: remote %q, groups kept %v", r.remote, strings.Contains(r.groups, "one"))
	}
	if err := heartbeat(rc, "unlinked", gen, "", "", "local_only"); err != nil {
		t.Fatal(err)
	}
	r = readStatus(t, d)
	if r.state != "unlinked" || r.number != "" || r.aci != "" || r.groups != "[]" || r.remote != "local_only" {
		t.Errorf("after unlinked: %+v", r)
	}
	// A later keep-alive of the same generation does not forget the remote unlink result.
	if err := heartbeat(rc, "unlinked", gen, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if r := readStatus(t, d); r.remote != "local_only" {
		t.Errorf("the remote unlink result was lost: %q", r.remote)
	}
}

// signal_sidecar_set_groups: limits, shapes, the stripped set and the preconditions.
func TestSignalSidecarSetGroupsContract(t *testing.T) {
	d, _, rc := signalProvisioned(t, "siggroups")
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	// Not linked: refused (false), nothing stored.
	signalExec(t, d, `UPDATE signal_sidecar_config SET groups_refresh_gen = 1`)
	if ok, err := setGroups(rc, 1, groupsJSON("a")); err != nil || ok {
		t.Fatalf("set_groups while unlinked: ok=%v err=%v, want false", ok, err)
	}
	if err := heartbeat(rc, "linked", 0, testNumber, "", ""); err != nil {
		t.Fatal(err)
	}
	// A refresh generation the server never asked for: refused.
	if ok, err := setGroups(rc, 2, groupsJSON("a")); err != nil || ok {
		t.Fatalf("set_groups for a refresh generation that was not asked: ok=%v err=%v, want false", ok, err)
	}
	if ok, err := setGroups(rc, 1, groupsJSON("a", "b")); err != nil || !ok {
		t.Fatalf("set_groups: ok=%v err=%v, want true", ok, err)
	}
	r := readStatus(t, d)
	if r.groupsGen != 1 || !strings.Contains(r.groups, `"name": "b"`) {
		t.Errorf("stored: generation %d, groups %s", r.groupsGen, r.groups)
	}
	// An older refresh generation than the stored one: refused.
	signalExec(t, d, `UPDATE signal_sidecar_config SET groups_refresh_gen = 3`)
	if ok, err := setGroups(rc, 2, groupsJSON("c")); err != nil || !ok {
		t.Fatalf("set_groups 2: %v %v", ok, err)
	}
	if ok, err := setGroups(rc, 1, groupsJSON("old")); err != nil || ok {
		t.Fatalf("set_groups with an older generation: ok=%v err=%v, want false", ok, err)
	}

	many := make([]string, 201)
	for i := range many {
		many[i] = "g"
	}
	id := groupID(1)
	refused := map[string]string{
		"not an array":             `{"id":"x"}`,
		"201 groups":               groupsJSON(many...),
		"over 128 KiB":             `[{"id":"` + id + `","name":"` + strings.Repeat("a", 131100) + `"}]`,
		"no id":                    `[{"name":"a"}]`,
		"no name":                  `[{"id":"` + id + `"}]`,
		"an extra key":             `[{"id":"` + id + `","name":"a","extra":1}]`,
		"an element that is text":  `["a"]`,
		"an id that is a number":   `[{"id":1,"name":"a"}]`,
		"an id without the prefix": `[{"id":"` + strings.TrimPrefix(id, "group.") + `","name":"a"}]`,
		"an id that is too short":  `[{"id":"group.abc","name":"a"}]`,
		"an id with a quote":       `[{"id":"group.` + strings.Repeat("a", 58) + `'=","name":"a"}]`,
		"a name over 128":          `[{"id":"` + id + `","name":"` + strings.Repeat("a", 129) + `"}]`,
		"a name with a control":    `[{"id":"` + id + `","name":"a\u0007b"}]`,
		"a name with a bidi mark":  `[{"id":"` + id + `","name":"a\u202eb"}]`,
	}
	for name, doc := range refused {
		_, err := setGroups(rc, 3, doc)
		requireSQLState(t, name, err, "22023")
	}
	accepted := map[string]string{
		"200 groups":                    groupsJSON(many[:200]...),
		"a name of 128":                 `[{"id":"` + id + `","name":"` + strings.Repeat("a", 128) + `"}]`,
		"an empty list":                 `[]`,
		"an empty name":                 `[{"id":"` + id + `","name":""}]`,
		"a name with accents and emoji": `[{"id":"` + id + `","name":"Zażółć 🙂"}]`,
	}
	for name, doc := range accepted {
		if ok, err := setGroups(rc, 3, doc); err != nil || !ok {
			t.Errorf("%s: ok=%v err=%v, want accepted", name, ok, err)
		}
	}
}

// The stripped set, as ranges. The function must refuse a name that holds a code point of the set and accept one
// that does not; every range is probed at both ends and one step outside, and a sweep of every code point shows
// that the pattern inside the function matches exactly the set.
var strippedRanges = [][2]rune{
	{0x0001, 0x001F}, {0x007F, 0x009F}, {0x00AD, 0x00AD}, {0x061C, 0x061C}, {0x180E, 0x180E},
	{0x200B, 0x200F}, {0x2028, 0x202E}, {0x2060, 0x206F}, {0xFEFF, 0xFEFF}, {0xFFF9, 0xFFFB}, {0xE0000, 0xE007F},
}

func isStripped(r rune) bool {
	for _, rg := range strippedRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

func TestSignalSidecarGroupNameCodePoints(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigcodepoints")
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := heartbeat(rc, "linked", 0, testNumber, "", ""); err != nil {
		t.Fatal(err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_config SET groups_refresh_gen = 1`)
	id := groupID(1)
	probe := func(r rune) error {
		// JSON text with the code point as an escape: no surrogate pair needed for the escape of a BMP point.
		var esc string
		if r > 0xFFFF {
			r -= 0x10000
			esc = fmt.Sprintf(`\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		} else {
			esc = fmt.Sprintf(`\u%04x`, r)
		}
		_, err := setGroups(rc, 1, `[{"id":"`+id+`","name":"a`+esc+`b"}]`)
		return err
	}
	for _, rg := range strippedRanges {
		for _, r := range []rune{rg[0], rg[1]} {
			requireSQLState(t, fmt.Sprintf("U+%04X inside the set", r), probe(r), "22023")
		}
		for _, r := range []rune{rg[0] - 1, rg[1] + 1} {
			if r == 0 || isStripped(r) || (r >= 0xD800 && r <= 0xDFFF) {
				continue
			}
			if err := probe(r); err != nil {
				t.Errorf("U+%04X outside the set was refused: %s", r, dbtest.AtomicErrorSummary(err))
			}
		}
	}
	// The sweep: take the pattern out of the function and match every code point of Unicode with it.
	var src string
	if err := d.QueryRow(`SELECT prosrc FROM pg_proc WHERE proname = 'signal_sidecar_set_groups'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`~ E'(\[[^']*U000E0000-\\\\U000E007F\])'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("the pattern of the stripped set was not found in the function body")
	}
	pattern := strings.ReplaceAll(m[1], `\\`, `\`)
	rows, err := d.Query(`SELECT cp FROM generate_series(1, 1114111) AS cp
	                       WHERE cp NOT BETWEEN 55296 AND 57343 AND chr(cp) ~ $1 ORDER BY cp`, pattern)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	matched := map[rune]bool{}
	for rows.Next() {
		var cp int
		if err := rows.Scan(&cp); err != nil {
			t.Fatal(err)
		}
		matched[rune(cp)] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wrong := 0
	for r := rune(1); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if matched[r] != isStripped(r) {
			wrong++
			if wrong <= 5 {
				t.Errorf("U+%04X: pattern matches %v, the set says %v", r, matched[r], isStripped(r))
			}
		}
	}
}

// No error of the functions carries an argument value.
func TestSignalSidecarErrorsNeverCarryArguments(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigsentinel")
	const sentinel = "SENTINEL-ARGUMENT-VALUE-7788"
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_config SET desired = 'link', gen = 1`)
	calls := map[string]func() error{
		"heartbeat state":  func() error { return heartbeat(rc, sentinel, 0, "", "", "") },
		"heartbeat number": func() error { return heartbeat(rc, "unlinked", 0, sentinel, "", "") },
		"heartbeat remote": func() error { return heartbeat(rc, "unlinked", 0, "", "", sentinel) },
		"heartbeat aci": func() error {
			_, err := rc.Exec(`SELECT public.signal_sidecar_heartbeat('unlinked', 0, '', $1, '', '', '', '')`, sentinel)
			return err
		},
		"set_link address": func() error { _, err := setLink(rc, 1, "sgnl://linkdevice?"+sentinel+"<"); return err },
		"set_link scheme":  func() error { _, err := setLink(rc, 1, sentinel); return err },
		"set_groups name": func() error {
			_, err := setGroups(rc, 1, `[{"id":"`+groupID(1)+`","name":"`+sentinel+`\u0007"}]`)
			return err
		},
		"set_groups id":   func() error { _, err := setGroups(rc, 1, `[{"id":"`+sentinel+`","name":"a"}]`); return err },
		"set_groups key":  func() error { _, err := setGroups(rc, 1, `[{"`+sentinel+`":1}]`); return err },
		"set_groups type": func() error { _, err := setGroups(rc, 1, `"`+sentinel+`"`); return err },
		"grant":           func() error { _, err := d.Exec(`SELECT public.signal_sidecar_grant(0::oid::regrole)`); return err },
	}
	for name, call := range calls {
		err := call()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Errorf("%s: err = %v, want a database error", name, err)
			continue
		}
		all := strings.Join([]string{pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where, pgErr.InternalQuery, pgErr.ConstraintName, pgErr.ColumnName}, "|")
		if strings.Contains(all, sentinel) {
			t.Errorf("%s: the error carries the argument: %s", name, all)
		}
	}
}

// A function that finds no row to update raises, never reports success.
func TestSignalSidecarFunctionsRaiseWhenARowIsMissing(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigmissing")
	signalExec(t, d, `DELETE FROM signal_sidecar_status`)
	requireSQLState(t, "heartbeat without a status row", heartbeat(rc, "unlinked", 0, "", "", ""), "P0002")
	signalExec(t, d, `UPDATE signal_sidecar_config SET desired = 'link', gen = 1`)
	_, err := setLink(rc, 1, testLinkURI)
	requireSQLState(t, "set_link without a status row", err, "P0002")
	_, err = setGroups(rc, 0, `[]`)
	requireSQLState(t, "set_groups without a status row", err, "P0002")
	signalExec(t, d, `INSERT INTO signal_sidecar_status DEFAULT VALUES`)
	signalExec(t, d, `DELETE FROM signal_sidecar_config`)
	requireSQLState(t, "heartbeat without a configuration row", heartbeat(rc, "unlinked", 0, "", "", ""), "P0002")
	_, err = setLink(rc, 1, testLinkURI)
	requireSQLState(t, "set_link without a configuration row", err, "P0002")
	_, err = setGroups(rc, 0, `[]`)
	requireSQLState(t, "set_groups without a configuration row", err, "P0002")
	if _, _, err := d.RequestSignalLink(context.Background(), "ARTEX", "x"); !errors.Is(err, ErrSignalRowMissing) {
		t.Errorf("RequestSignalLink without rows: err = %v, want ErrSignalRowMissing", err)
	}
	if _, err := d.ReadSignalSidecar(context.Background()); !errors.Is(err, ErrSignalRowMissing) {
		t.Errorf("ReadSignalSidecar without a row: err = %v, want ErrSignalRowMissing", err)
	}
}

// ---- the commands of the server ----

func TestSignalSidecarCommandProtocol(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigcommands")
	ctx := context.Background()

	// Without a heartbeat nothing that needs the sidecar is accepted.
	if _, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil || applied {
		t.Fatalf("link without a heartbeat: applied=%v err=%v", applied, err)
	}
	if _, applied, err := d.RequestSignalLink(ctx, "bad;name", "tester"); !errors.Is(err, ErrSignalDeviceName) || applied {
		t.Fatalf("link with a bad device name: applied=%v err=%v", applied, err)
	}
	// State 'unknown' (a heartbeat is not needed for the read).
	st, err := d.ReadSignalSidecar(ctx)
	if err != nil || st.State != "unknown" || st.HeartbeatFresh || !st.HeartbeatAt.IsZero() {
		t.Fatalf("initial read: %+v err %v", st, err)
	}
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_config SET confirmed_account = '`+testNumber+`'`)
	gen, applied, err := d.RequestSignalLink(ctx, "Office", "tester")
	if err != nil || !applied || gen != 1 {
		t.Fatalf("link: %d %v %v", gen, applied, err)
	}
	st, _ = d.ReadSignalSidecar(ctx)
	if st.Desired != "link" || st.Gen != 1 || st.ConfirmedAccount != "" || st.DeviceName != "Office" || st.RequestedBy != "tester" || !st.HeartbeatFresh || st.RequestedAt.IsZero() {
		t.Errorf("after the link command: %+v", st)
	}
	// A second link while one is pending, and an unlink meanwhile, are refused.
	if _, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil || applied {
		t.Errorf("second link: applied=%v err=%v", applied, err)
	}
	if _, applied, err := d.RequestSignalUnlink(ctx, "tester"); err != nil || applied {
		t.Errorf("unlink while a link is pending: applied=%v err=%v", applied, err)
	}
	if applied, err := d.RequestSignalGroupsRefresh(ctx); err != nil || applied {
		t.Errorf("groups refresh while unlinked: applied=%v err=%v", applied, err)
	}
	// The address is served only while pending and not expired.
	if _, ok, err := d.ReadSignalLink(ctx); err != nil || ok {
		t.Errorf("link served before the sidecar stored it: %v %v", ok, err)
	}
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || !ok {
		t.Fatalf("set_link: %v %v", ok, err)
	}
	info, ok, err := d.ReadSignalLink(ctx)
	if err != nil || !ok || info.Gen != gen || info.URI != testLinkURI || !info.ExpiresAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("ReadSignalLink = %+v %v %v", info, ok, err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_status SET link_expires_at = now() - interval '1 second'`)
	if _, ok, _ := d.ReadSignalLink(ctx); ok {
		t.Error("an expired link is still served")
	}
	signalExec(t, d, `UPDATE signal_sidecar_status SET link_expires_at = now() + interval '60 seconds'`)

	// Cancel: desired goes back to idle at once, the generation stays; the address is no longer served.
	if cancelled, err := d.CancelSignalLink(ctx); err != nil || !cancelled {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	if _, ok, _ := d.ReadSignalLink(ctx); ok {
		t.Error("a cancelled link is still served")
	}
	if cancelled, err := d.CancelSignalLink(ctx); err != nil || cancelled {
		t.Errorf("second cancel: %v %v", cancelled, err)
	}
	st, _ = d.ReadSignalSidecar(ctx)
	if st.Desired != "idle" || st.Gen != 1 {
		t.Errorf("after the cancel: desired %q gen %d", st.Desired, st.Gen)
	}
	// The sidecar finishes the generation; a new link is accepted.
	if err := heartbeat(rc, "unlinked", gen, "", "link cancelled", ""); err != nil {
		t.Fatal(err)
	}
	gen, applied, err = d.RequestSignalLink(ctx, "ARTEX", "tester")
	if err != nil || !applied || gen != 2 {
		t.Fatalf("link after the cancel: %d %v %v", gen, applied, err)
	}
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || !ok {
		t.Fatalf("set_link: %v %v", ok, err)
	}
	if err := heartbeat(rc, "linked", gen, testNumber, "", ""); err != nil {
		t.Fatal(err)
	}
	// The sidecar finished the generation; the next status read puts desired back to idle.
	if changed, err := d.ReconcileSignalSidecar(ctx); err != nil || !changed {
		t.Fatalf("reconcile: %v %v", changed, err)
	}
	if changed, err := d.ReconcileSignalSidecar(ctx); err != nil || changed {
		t.Fatalf("second reconcile: %v %v", changed, err)
	}
	if _, ok, _ := d.ReadSignalLink(ctx); ok {
		t.Error("a link is served although the account is linked")
	}
	// Link while linked: refused.
	if _, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil || applied {
		t.Errorf("link while linked: applied=%v err=%v", applied, err)
	}

	// Confirmation: stale generation, wrong digits, right digits.
	if res, err := d.ConfirmSignalAccount(ctx, gen-1, "550101"); err != nil || res != SignalConfirmStale {
		t.Errorf("confirm with an old generation = %v %v", res, err)
	}
	for _, digits := range []string{"550100", "", "5501011", "abcdef"} {
		if res, err := d.ConfirmSignalAccount(ctx, gen, digits); err != nil || res != SignalConfirmMismatch {
			t.Errorf("confirm with %q = %v %v, want a mismatch", digits, res, err)
		}
	}
	if st, _ := d.ReadSignalSidecar(ctx); st.ConfirmedAccount != "" {
		t.Fatal("a refused confirmation stored the account")
	}
	if res, err := d.ConfirmSignalAccount(ctx, gen, "550101"); err != nil || res != SignalConfirmed {
		t.Fatalf("confirm = %v %v", res, err)
	}
	if st, _ := d.ReadSignalSidecar(ctx); st.ConfirmedAccount != testNumber {
		t.Errorf("confirmed account %q", st.ConfirmedAccount)
	}

	// Groups refresh, and the list it brings.
	if applied, err := d.RequestSignalGroupsRefresh(ctx); err != nil || !applied {
		t.Fatalf("groups refresh: %v %v", applied, err)
	}
	if ok, err := setGroups(rc, 1, groupsJSON("Alerts")); err != nil || !ok {
		t.Fatalf("set_groups: %v %v", ok, err)
	}
	groups, at, err := d.ReadSignalGroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Name != "Alerts" || groups[0].ID != groupID(1) || at.IsZero() {
		t.Errorf("ReadSignalGroups = %v %v %v", groups, at, err)
	}
	if st, _ := d.ReadSignalSidecar(ctx); st.GroupsCount != 1 || st.GroupsGen != 1 || st.GroupsAt.IsZero() {
		t.Errorf("groups in the state: %+v", st)
	}

	// Unlink clears the confirmation in the same statement; the sidecar's answer empties the rest.
	ugen, applied, err := d.RequestSignalUnlink(ctx, "tester")
	if err != nil || !applied || ugen != 3 {
		t.Fatalf("unlink: %d %v %v", ugen, applied, err)
	}
	if st, _ := d.ReadSignalSidecar(ctx); st.Desired != "unlink" || st.ConfirmedAccount != "" {
		t.Errorf("after the unlink command: desired %q, confirmed %q", st.Desired, st.ConfirmedAccount)
	}
	if err := heartbeat(rc, "unlinked", ugen, "", "", "local_only"); err != nil {
		t.Fatal(err)
	}
	if changed, err := d.ReconcileSignalSidecar(ctx); err != nil || !changed {
		t.Fatalf("reconcile after the unlink: %v %v", changed, err)
	}
	st, _ = d.ReadSignalSidecar(ctx)
	if st.State != "unlinked" || st.AccountNumber != "" || st.GroupsCount != 0 || st.RemoteUnlink != "local_only" || st.Desired != "idle" {
		t.Errorf("after the unlink: %+v", st)
	}
	// Unlink from 'unlinked' and from 'unknown' is refused; from 'error' it is accepted.
	if _, applied, err := d.RequestSignalUnlink(ctx, "tester"); err != nil || applied {
		t.Errorf("unlink from unlinked: %v %v", applied, err)
	}
	if err := heartbeat(rc, "error", ugen, "", "two accounts", ""); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil || applied {
		t.Errorf("link from error: %v %v", applied, err)
	}
	if _, applied, err := d.RequestSignalUnlink(ctx, "tester"); err != nil || !applied {
		t.Errorf("unlink from error: %v %v", applied, err)
	}
	resetSignal(t, d)
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_status SET state = 'unknown'`)
	if _, applied, err := d.RequestSignalUnlink(ctx, "tester"); err != nil || applied {
		t.Errorf("unlink from unknown: %v %v", applied, err)
	}
	// A stale heartbeat refuses both commands.
	signalExec(t, d, `UPDATE signal_sidecar_status SET state = 'unlinked', heartbeat_at = now() - interval '31 seconds'`)
	if _, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil || applied {
		t.Errorf("link with a stale heartbeat: %v %v", applied, err)
	}
	st, _ = d.ReadSignalSidecar(ctx)
	if st.HeartbeatFresh || st.HeartbeatAge < 30*time.Second {
		t.Errorf("heartbeat age %v, fresh %v", st.HeartbeatAge, st.HeartbeatFresh)
	}
}

// A link request that a dead sidecar never finished is given up after its lifetime plus a grace.
func TestSignalSidecarReconcileGivesUpAnUnfinishedLink(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigbackstop")
	ctx := context.Background()
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil || !applied {
		t.Fatalf("link: %v %v", applied, err)
	}
	if changed, err := d.ReconcileSignalSidecar(ctx); err != nil || changed {
		t.Fatalf("a young request was given up: %v %v", changed, err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_config SET requested_at = now() - interval '181 seconds'`)
	if st, _ := d.ReadSignalSidecar(ctx); !st.LinkRequestStale {
		t.Error("the old request is not reported as stale")
	}
	if changed, err := d.ReconcileSignalSidecar(ctx); err != nil || !changed {
		t.Fatalf("reconcile: %v %v", changed, err)
	}
	st, _ := d.ReadSignalSidecar(ctx)
	if st.Desired != "idle" || st.Gen != 1 || st.HandledGen != 0 {
		t.Errorf("after giving up: desired %q gen %d handled %d", st.Desired, st.Gen, st.HandledGen)
	}
	// A restarted sidecar acknowledges the generation without acting on it.
	if err := heartbeat(rc, "unlinked", 1, "", "link interrupted", ""); err != nil {
		t.Fatal(err)
	}
}

// The server holds the configuration row first and the status row second; the sidecar's functions lock the
// status row only. A lock held on one row never blocks the other side, and a held configuration row ends a
// command with ErrSignalBusy instead of a wait.
func TestSignalSidecarLockOrder(t *testing.T) {
	d, _, rc := signalProvisioned(t, "siglocks")
	ctx := context.Background()
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_config SET desired = 'link', gen = 1`)

	hold, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback() //nolint:errcheck
	if _, err := hold.ExecContext(ctx, `SELECT 1 FROM signal_sidecar_config WHERE id FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	// With the configuration row held, the three functions still run: they read it without a lock.
	done := make(chan error, 1)
	go func() {
		if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
			done <- err
			return
		}
		if ok, err := setLink(rc, 1, testLinkURI); err != nil || !ok {
			done <- fmt.Errorf("set_link: %v %v", ok, err)
			return
		}
		_, err := setGroups(rc, 0, `[]`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a function failed while the configuration row was held: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a function waited for the configuration row")
	}
	hold.Rollback() //nolint:errcheck

	// A command that needs the configuration row gives up with the named error, after about the lock timeout.
	signalExec(t, d, `UPDATE signal_sidecar_config SET desired = 'idle'`)
	signalExec(t, d, `UPDATE signal_sidecar_status SET state = 'linked', link_uri = NULL, link_expires_at = NULL,
	                  account_number = '`+testNumber+`', heartbeat_at = now()`)
	hold3, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold3.Rollback() //nolint:errcheck
	if _, err := hold3.ExecContext(ctx, `SELECT 1 FROM signal_sidecar_config WHERE id FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err = d.RequestSignalUnlink(ctx, "tester")
	if !errors.Is(err, ErrSignalBusy) {
		t.Fatalf("command against a held row: err = %v, want ErrSignalBusy", err)
	}
	if el := time.Since(start); el < 2*time.Second || el > 10*time.Second {
		t.Errorf("the command waited %v, want about 3 s", el)
	}
	hold3.Rollback() //nolint:errcheck

	// With the status row held, a command still goes through (it reads the status row without a lock).
	hold2, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer hold2.Rollback() //nolint:errcheck
	if _, err := hold2.ExecContext(ctx, `SELECT 1 FROM signal_sidecar_status WHERE id FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	signalExec(t, d, `UPDATE signal_sidecar_config SET desired = 'idle'`)
	if _, _, err := d.RequestSignalLink(ctx, "ARTEX", "tester"); err != nil {
		t.Errorf("a command was blocked by the status row: %v", err)
	}
}

// 200 heartbeats interleaved with link, cancel and unlink commands: no deadlock, no error, consistent rows.
func TestSignalSidecarConcurrentHeartbeatsAndCommands(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigconcurrent")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	report := func(err error) {
		if err != nil {
			select {
			case errs <- err:
			default:
			}
		}
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				// A heartbeat that is older than the generation the other goroutine just finished is refused.
				if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
						report(err)
					}
				}
			}
		}()
	}
	applied := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			gen, ok, err := d.RequestSignalLink(ctx, "ARTEX", "tester")
			report(err)
			if ok {
				applied++
				if stored, err := setLink(rc, gen, testLinkURI); err != nil {
					report(err)
				} else if stored {
					_, err := d.CancelSignalLink(ctx)
					report(err)
				}
				// The sidecar finishes the generation.
				report(heartbeat(rc, "unlinked", gen, "", "link cancelled", ""))
			}
			_, err = d.ReconcileSignalSidecar(ctx)
			report(err)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call failed: %s", dbtest.AtomicErrorSummary(err))
	}
	st, err := d.ReadSignalSidecar(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if applied == 0 || st.Gen != int64(applied) || st.HandledGen > st.Gen {
		t.Errorf("rows are inconsistent: applied %d, gen %d, handled %d", applied, st.Gen, st.HandledGen)
	}
}

// A confirmation that arrives while an unlink is requested is refused: the account is on its way out.
func TestSignalSidecarConfirmRefusedWhileUnlinkRequested(t *testing.T) {
	d, _, rc := signalProvisioned(t, "sigconfunlink")
	ctx := context.Background()
	if err := heartbeat(rc, "unlinked", 0, "", "", ""); err != nil {
		t.Fatal(err)
	}
	gen, applied, err := d.RequestSignalLink(ctx, "ARTEX", "tester")
	if err != nil || !applied {
		t.Fatalf("link: %v %v", applied, err)
	}
	if ok, err := setLink(rc, gen, testLinkURI); err != nil || !ok {
		t.Fatalf("set_link: %v %v", ok, err)
	}
	if err := heartbeat(rc, "linked", gen, testNumber, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReconcileSignalSidecar(ctx); err != nil {
		t.Fatal(err)
	}
	ugen, applied, err := d.RequestSignalUnlink(ctx, "tester")
	if err != nil || !applied {
		t.Fatalf("unlink: %v %v", applied, err)
	}
	if res, err := d.ConfirmSignalAccount(ctx, ugen, "550101"); err != nil || res != SignalConfirmStale {
		t.Errorf("confirm while an unlink is requested = %v %v, want stale", res, err)
	}
	if st, _ := d.ReadSignalSidecar(ctx); st.ConfirmedAccount != "" {
		t.Errorf("confirmed_account = %q, want empty", st.ConfirmedAccount)
	}
}

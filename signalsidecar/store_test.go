package signalsidecar

import (
	"context"
	"encoding/base64"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// dbRig is a migrated temporary database, a provisioned sidecar role and the
// connection settings of that role. The owner connection plays the server: it
// writes the configuration row directly.
type dbRig struct {
	t     *testing.T
	owner *db.DB
	tmp   *dbtest.Temp
	role  *dbtest.SidecarRole
	cc    *pgx.ConnConfig
}

func newDBRig(t *testing.T, label string) *dbRig {
	t.Helper()
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, label)
	owner, err := db.Open(tmp.DSN)
	if err != nil {
		t.Fatalf("open the temporary database: %s", dbtest.AtomicErrorSummary(err))
	}
	t.Cleanup(func() { owner.Close() })
	role := srv.NewSidecarRole(t, tmp)
	r := &dbRig{t: t, owner: owner, tmp: tmp, role: role}
	r.exec(`CREATE ROLE ` + pgx.Identifier{role.Name}.Sanitize() + ` LOGIN PASSWORD '` + role.Password + `'`)
	r.exec(`SELECT public.signal_sidecar_grant($1::text::regrole)`, role.Name)
	r.exec(`UPDATE public.signal_sidecar_config SET db_auth_state = 'ok'`)
	dsn, err := tmp.DSNFor(role.Name, role.Password)
	if err != nil {
		t.Fatal(err)
	}
	r.cc, err = pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse the sidecar DSN")
	}
	return r
}

func (r *dbRig) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.owner.Exec(q, args...); err != nil {
		r.t.Fatalf("%s: %s", q, dbtest.AtomicErrorSummary(err))
	}
}

func (r *dbRig) newStore() *Store {
	s := NewStore(r.cc, func(string, ...any) {})
	r.t.Cleanup(s.Close)
	return s
}

// requestLink writes what the server writes for a link request.
func (r *dbRig) requestLink() int64 {
	r.t.Helper()
	var gen int64
	if err := r.owner.QueryRow(`UPDATE public.signal_sidecar_config
	    SET desired = 'link', gen = gen + 1, confirmed_account = '' RETURNING gen`).Scan(&gen); err != nil {
		r.t.Fatal(dbtest.AtomicErrorSummary(err))
	}
	return gen
}

func (r *dbRig) requestCancel() {
	r.t.Helper()
	r.exec(`UPDATE public.signal_sidecar_config SET desired = 'idle'`)
}

func (r *dbRig) requestUnlink() int64 {
	r.t.Helper()
	var gen int64
	if err := r.owner.QueryRow(`UPDATE public.signal_sidecar_config
	    SET desired = 'unlink', gen = gen + 1, confirmed_account = '' RETURNING gen`).Scan(&gen); err != nil {
		r.t.Fatal(dbtest.AtomicErrorSummary(err))
	}
	return gen
}

func (r *dbRig) requestGroups() {
	r.t.Helper()
	r.exec(`UPDATE public.signal_sidecar_config SET groups_refresh_gen = groups_refresh_gen + 1`)
}

// reconcile is what the server does on a status read: a finished command is
// reset to idle.
func (r *dbRig) reconcile() {
	r.t.Helper()
	r.exec(`UPDATE public.signal_sidecar_config c SET desired = 'idle'
	         FROM public.signal_sidecar_status s WHERE s.handled_gen = c.gen AND c.desired <> 'idle'`)
}

func (r *dbRig) confirm(number string) {
	r.t.Helper()
	r.exec(`UPDATE public.signal_sidecar_config SET confirmed_account = $1`, number)
}

type statusRow struct {
	State          string
	Handled        int64
	Number, ACI    string
	URI            sql.NullString
	ExpiresAt      sql.NullTime
	Err            string
	Remote         string
	Groups         string
	GroupsAt       sql.NullTime
	GroupsGen      int64
	HeartbeatAt    sql.NullTime
	SidecarVersion string
	CLIVersion     string
	Now            time.Time
}

// String leaves the link address out, so a failed test never prints it.
func (s statusRow) String() string {
	return fmt.Sprintf("state=%s handled=%d number=%s uri_set=%v err=%q remote=%s groups=%s groups_gen=%d",
		s.State, s.Handled, s.Number, s.URI.Valid, s.Err, s.Remote, s.Groups, s.GroupsGen)
}

func (r *dbRig) status() statusRow {
	r.t.Helper()
	var s statusRow
	err := r.owner.QueryRow(`SELECT state, handled_gen, account_number, account_aci, link_uri, link_expires_at,
	    last_error, remote_unlink, groups::text, groups_at, groups_gen, heartbeat_at, sidecar_version,
	    signal_cli_version, now() FROM public.signal_sidecar_status`).
		Scan(&s.State, &s.Handled, &s.Number, &s.ACI, &s.URI, &s.ExpiresAt, &s.Err, &s.Remote, &s.Groups,
			&s.GroupsAt, &s.GroupsGen, &s.HeartbeatAt, &s.SidecarVersion, &s.CLIVersion, &s.Now)
	if err != nil {
		r.t.Fatal(dbtest.AtomicErrorSummary(err))
	}
	return s
}

// waitStatus polls the status row until cond holds.
func (r *dbRig) waitStatus(what string, d time.Duration, cond func(statusRow) bool) statusRow {
	r.t.Helper()
	deadline := time.Now().Add(d)
	for {
		s := r.status()
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("after %v: %s did not happen; status: %v", d, what, s)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// testOuterGroup is the id of testGroup in the form ARTEX stores.
var testOuterGroup = "group." + base64.StdEncoding.EncodeToString([]byte(testGroup))

const testLinkURI = "sgnl://linkdevice?uuid=ZmFrZQ&pub_key=BQabc%2B&capabilities=a,b"

func TestStoreReadsTheView(t *testing.T) {
	r := newDBRig(t, "storeview")
	s := r.newStore()
	v, err := s.Config(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	want := ConfigView{Desired: "idle", DeviceName: "ARTEX", LinkTTLSeconds: 120, DBAuthOK: true}
	if v != want {
		t.Fatalf("view %+v, want %+v", v, want)
	}
	r.exec(`UPDATE public.signal_sidecar_config SET desired = 'link', gen = 4, device_name = 'My Box',
	    link_ttl_seconds = 45, confirmed_account = '+48600100101', groups_refresh_gen = 3, db_auth_state = 'unchecked'`)
	r.exec(`UPDATE public.signal_sidecar_status SET handled_gen = 2, groups_gen = 1`)
	v, err = s.Config(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	want = ConfigView{Desired: "link", Gen: 4, DeviceName: "My Box", LinkTTLSeconds: 45, ConfirmedAccount: "+48600100101",
		GroupsRefreshGen: 3, DBAuthOK: false, HandledGen: 2, GroupsGen: 1}
	if v != want {
		t.Fatalf("view %+v, want %+v", v, want)
	}
}

func TestStoreCallsTheThreeFunctions(t *testing.T) {
	r := newDBRig(t, "storefn")
	s := r.newStore()
	ctx := ctx5(t)

	if err := s.Heartbeat(ctx, Heartbeat{State: "unlinked", SidecarVersion: "sv", CLIVersion: "cv"}); err != nil {
		t.Fatal(err)
	}
	if st := r.status(); st.State != "unlinked" || !st.HeartbeatAt.Valid || st.SidecarVersion != "sv" || st.CLIVersion != "cv" {
		t.Fatalf("after the heartbeat: %v", st)
	}

	// Nothing is stored for a link nobody asked for.
	if ok, err := s.SetLink(ctx, 0, testLinkURI); err != nil || ok {
		t.Fatalf("set_link without a request: %v %v", ok, err)
	}
	gen := r.requestLink()
	ok, err := s.SetLink(ctx, gen, testLinkURI)
	if err != nil || !ok {
		t.Fatalf("set_link: %v %v", ok, err)
	}
	st := r.status()
	if st.State != "linking" || st.URI.String != testLinkURI || !st.ExpiresAt.Valid {
		t.Fatalf("after set_link: %v", st)
	}

	if err := s.Heartbeat(ctx, Heartbeat{State: "linked", HandledGen: gen, Number: "+48600100101", ACI: "11111111-2222-4333-8444-555555555555"}); err != nil {
		t.Fatal(err)
	}
	st = r.status()
	if st.State != "linked" || st.Number != "+48600100101" || st.URI.Valid || st.Handled != gen {
		t.Fatalf("after linked: %v", st)
	}

	group := GroupEntry{ID: testOuterGroup, Name: "Ops"}
	if ok, err := s.SetGroups(ctx, 0, []GroupEntry{group}); err != nil || !ok {
		t.Fatalf("set_groups: %v %v", ok, err)
	}
	if st := r.status(); !st.GroupsAt.Valid || !strings.Contains(st.Groups, `"Ops"`) {
		t.Fatalf("after set_groups: %v", st)
	}
	// A refresh generation the server never asked for is not taken.
	if ok, err := s.SetGroups(ctx, 5, nil); err != nil || ok {
		t.Fatalf("set_groups for a future generation: %v %v", ok, err)
	}
}

func TestStoreBindsEveryValueAsAParameter(t *testing.T) {
	r := newDBRig(t, "storebind")
	s := r.newStore()
	nasty := `x'); DROP TABLE public.signal_sidecar_status; -- $1 \`
	if err := s.Heartbeat(ctx5(t), Heartbeat{State: "error", Error: nasty}); err != nil {
		t.Fatal(err)
	}
	if st := r.status(); st.Err != nasty || st.State != "error" {
		t.Fatalf("stored %q (%v)", st.Err, st)
	}
}

func TestStoreErrorsCarryNoArgumentValue(t *testing.T) {
	r := newDBRig(t, "storeerr")
	s := r.newStore()
	ctx := ctx5(t)
	gen := r.requestLink()
	cases := map[string]func() error{
		"link address": func() error { _, err := s.SetLink(ctx, gen, "http://SENTINEL-URI-VALUE"); return err },
		"number":       func() error { return s.Heartbeat(ctx, Heartbeat{State: "unlinked", Number: "SENTINEL-NUMBER-VALUE"}) },
		"group name": func() error {
			_, err := s.SetGroups(ctx, 0, []GroupEntry{{ID: testOuterGroup, Name: "SENTINEL-NAME-VALUE\u202e"}})
			return err
		},
	}
	for name, fn := range cases {
		err := fn()
		var de *DBError
		if !errors.As(err, &de) || de.SQLState != "22023" {
			t.Errorf("%s: error %v, want a DBError with SQLSTATE 22023", name, err)
			continue
		}
		if strings.Contains(err.Error(), "SENTINEL") {
			t.Errorf("%s: the error carries the argument: %v", name, err)
		}
	}
}

func TestStoreReconnectsAfterTheServerEndsTheSession(t *testing.T) {
	r := newDBRig(t, "storereconn")
	s := r.newStore()
	if _, err := s.Config(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	r.exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1`, r.role.Name)
	deadline := time.Now().Add(10 * time.Second)
	var sawFailure bool
	for {
		_, err := s.Config(ctx5(t))
		if err == nil {
			break
		}
		var de *DBError
		if !errors.As(err, &de) {
			t.Fatalf("error of another kind: %v", err)
		}
		sawFailure = true
		if time.Now().After(deadline) {
			t.Fatalf("no reconnect within 10 s: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawFailure {
		t.Log("the terminated session was replaced without a visible failure")
	}
}

func TestStoreFailsFastWhileTheDatabaseIsDown(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cc, err := pgx.ParseConfig("host=" + dir + " user=u dbname=d sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(cc, func(string, ...any) {})
	defer s.Close()
	if _, err := s.Config(ctx5(t)); err == nil {
		t.Fatal("no error without a database")
	}
	start := time.Now()
	_, err = s.Config(ctx5(t))
	if !errors.Is(err, ErrDBUnavailable) || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("second call: %v after %v, want an immediate ErrDBUnavailable", err, time.Since(start))
	}
}

func TestCheckConnSecurity(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	cases := []struct {
		dsn string
		ok  bool
	}{
		{"postgres://u:p@db.example/x", false},
		{"postgres://u:p@db.example/x?sslmode=disable", false},
		{"postgres://u:p@db.example/x?sslmode=allow", false},
		{"postgres://u:p@db.example/x?sslmode=prefer", false},
		{"postgres://u:p@db.example/x?sslmode=require", false},
		{"postgres://u:p@db.example/x?sslmode=verify-ca", false},
		{"postgres://u:p@db.example/x?sslmode=verify-full", true},
		{"host=db.example user=u dbname=x sslmode=verify-full", true},
		{"host=db.example user=u dbname=x", false},
		{"host=/run/postgresql user=u dbname=x", true},
		{"postgres://u@/x?host=/run/postgresql", true},
		{"host=/run/postgresql user=u dbname=x sslmode=disable", true},
		{"host=/run/postgresql,db.example user=u dbname=x sslmode=verify-full", true},
		{"host=/run/postgresql,db.example user=u dbname=x sslmode=disable", false},
	}
	for _, c := range cases {
		cc, err := pgx.ParseConfig(c.dsn)
		if err != nil {
			t.Errorf("%q: %v", c.dsn, err)
			continue
		}
		err = CheckConnSecurity(cc)
		if (err == nil) != c.ok {
			t.Errorf("%q: error %v, want ok=%v", c.dsn, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInsecureDSN) {
			t.Errorf("%q: error %v is not ErrInsecureDSN", c.dsn, err)
		}
	}
}

func TestParseStoreConfigNeverEchoesTheAddress(t *testing.T) {
	_, err := ParseStoreConfig("postgres://u:SECRET-PW-VALUE@db.example/x?sslmode=bogus")
	if err == nil || strings.Contains(err.Error(), "SECRET-PW-VALUE") {
		t.Fatalf("error %v", err)
	}
	_, err = ParseStoreConfig("postgres://u:SECRET-PW-VALUE@db.example/x")
	if !errors.Is(err, ErrInsecureDSN) || strings.Contains(err.Error(), "SECRET-PW-VALUE") {
		t.Fatalf("error %v", err)
	}
	if _, err := ParseStoreConfig("host=/run/postgresql user=u dbname=x"); err != nil {
		t.Fatalf("socket address: %v", err)
	}
}

// fakePostgres answers every login with success, the way a server does that
// does not check passwords. It returns the connection settings for it.
func fakePostgres(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dir, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, ".s.PGSQL.5432"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				b := pgproto3.NewBackend(conn, conn)
				for {
					msg, err := b.ReceiveStartupMessage()
					if err != nil {
						return
					}
					switch msg.(type) {
					case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
						if _, err := conn.Write([]byte{'N'}); err != nil {
							return
						}
						continue
					case *pgproto3.StartupMessage:
						b.Send(&pgproto3.AuthenticationOk{})
						b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
						if b.Flush() != nil {
							return
						}
						_, _ = b.Receive() // the terminate message
					}
					return
				}
			}()
		}
	}()
	cc, err := pgx.ParseConfig("host=" + dir + " user=sidecar dbname=x sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return cc
}

func TestProbeFlagsADatabaseThatAcceptsAWrongPassword(t *testing.T) {
	cc := fakePostgres(t)
	if err := ProbeWrongPassword(ctx5(t), cc); !errors.Is(err, ErrPasswordNotChecked) {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbePassesWhenTheDatabaseChecksThePassword(t *testing.T) {
	r := newDBRig(t, "probeok")
	if err := ProbeWrongPassword(ctx5(t), r.cc); err != nil {
		t.Fatalf("probe against a database that checks passwords: %v", err)
	}
	// The probe changed nothing: the real password still works.
	s := r.newStore()
	if _, err := s.Config(ctx5(t)); err != nil {
		t.Fatal(err)
	}
}

func TestProbeIsInconclusiveWithoutADatabase(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cc, err := pgx.ParseConfig("host=" + dir + " user=u dbname=d sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := ProbeWrongPassword(ctx5(t), cc); !errors.Is(err, ErrProbeInconclusive) {
		t.Fatalf("probe: %v", err)
	}
}

func TestProbeDoesNotChangeTheCallersConfig(t *testing.T) {
	cc := fakePostgres(t)
	before := cc.Password
	_ = ProbeWrongPassword(ctx5(t), cc)
	if cc.Password != before {
		t.Fatal("the probe changed the password of the given settings")
	}
}

func TestDBErrorTextHoldsOnlyFixedTexts(t *testing.T) {
	e := dbError("x", context.DeadlineExceeded)
	if !strings.Contains(e.Error(), "timed out") {
		t.Fatal(e)
	}
	e = dbError("x", errors.New("failed to connect to `host=+48600100101`"))
	if strings.Contains(e.Error(), "48600100101") {
		t.Fatalf("number not masked: %v", e)
	}
}

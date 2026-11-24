package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
	"github.com/mkilijanek/artex/signalsidecar"
	"golang.org/x/sys/unix"
)

// fakeBin is the stand-in for signal-cli, built once.
var fakeBin string

func TestMain(m *testing.M) {
	dbtest.MustIsolateFromConfigFile()
	// A database name of its own, so that the leftover check of the sidecar
	// package never sees the temporary databases of this one.
	db.SuffixTestDatabase("cmd")
	db.SuffixTestDatabase("signalsidecar")
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "fakecli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "artex-signal tests:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	fakeBin = filepath.Join(dir, "fakecli")
	if out, err := exec.Command("go", "build", "-o", fakeBin, "../../signalsidecar/testdata/fakecli").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "artex-signal tests: building the fake CLI: %v\n%s", err, out)
		return 1
	}
	code := m.Run()
	if err := dbtest.CheckNoLeftovers(); err != nil {
		fmt.Fprintln(os.Stderr, "artex-signal tests:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func TestReadSettings(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if _, err := readSettings(get); err == nil {
		t.Fatal("no error without a database address")
	} else {
		var ce *configError
		if !errors.As(err, &ce) {
			t.Fatalf("error %T", err)
		}
	}
	env[envDSN] = "postgres://x"
	s, err := readSettings(get)
	if err != nil || s.dsn != "postgres://x" || s.socket != "/sock/api.sock" || s.data != "/data" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	env[envSocket], env[envData] = "/run/x.sock", "/var/data"
	s, _ = readSettings(get)
	if s.socket != "/run/x.sock" || s.data != "/var/data" {
		t.Fatalf("given: %+v", s)
	}
}

// The initial environment stays readable in /proc/<pid>/environ after
// os.Unsetenv, and the child runs as the same user. A non-dumpable process
// keeps its /proc files away from that user.
func TestRealMainMakesTheProcessNonDumpable(t *testing.T) {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0) })
	none := func(string) string { return "" }
	if code := realMain(nil, none, func(string) error { return nil }); code != exitConfig {
		t.Fatalf("exit %d, want %d", code, exitConfig)
	}
	got, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("PR_GET_DUMPABLE = %d after the sidecar started, want 0", got)
	}
}

func TestRealMainExitCodes(t *testing.T) {
	var removed []string
	unset := func(k string) error { removed = append(removed, k); return nil }
	none := func(string) string { return "" }
	if code := realMain(nil, none, unset); code != exitConfig {
		t.Fatalf("no address: exit %d, want %d", code, exitConfig)
	}
	if len(removed) != 0 {
		t.Fatalf("removed %v although nothing was read", removed)
	}
	if code := realMain([]string{"bogus"}, none, unset); code != 2 {
		t.Fatalf("unknown command: exit %d", code)
	}
	if code := realMain([]string{"version"}, none, unset); code != 0 {
		t.Fatalf("version: exit %d", code)
	}
	// A TCP address without verified TLS is refused after the settings were
	// read and removed from the environment.
	get := func(k string) string {
		if k == envDSN {
			return "postgres://u:p@db.example/x?sslmode=disable"
		}
		return ""
	}
	if code := realMain(nil, get, unset); code != exitConfig {
		t.Fatalf("insecure address: exit %d, want %d", code, exitConfig)
	}
	if strings.Join(removed, ",") != envDSN+","+envSocket+","+envData {
		t.Fatalf("removed %v", removed)
	}
}

func serveSocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path
}

func TestHealthcheckFailsWhenTheChildHasBeenDownBeyondTheGrace(t *testing.T) {
	about := func(down int64) string {
		return serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(signalsidecar.About{Service: "artex-signal", State: "linked", ChildDownSeconds: down})
		}))
	}
	if err := healthcheck(about(30)); err != nil {
		t.Fatalf("a short outage must not fail the check: %v", err)
	}
	if err := healthcheck(about(121)); err == nil {
		t.Fatal("a child down for 121 s passed the check")
	}
}

func TestHealthcheckUsesTheSocketOnly(t *testing.T) {
	good := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/about" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(signalsidecar.About{Service: "artex-signal", Version: "1", SignalCLI: "2", State: "linked"})
	}))
	if err := healthcheck(good); err != nil {
		t.Fatalf("healthy sidecar: %v", err)
	}
	// With no database settings at all the subcommand still works.
	if code := realMain([]string{"healthcheck"}, func(k string) string {
		if k == envSocket {
			return good
		}
		return ""
	}, func(string) error { return nil }); code != 0 {
		t.Fatalf("exit %d", code)
	}

	other := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"service":"something-else"}`))
	}))
	broken := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	garbage := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	for name, path := range map[string]string{
		"another service": other, "server error": broken, "not json": garbage,
		"nobody listening": filepath.Join(t.TempDir(), "none.sock"),
	} {
		if err := healthcheck(path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// neverCalled is the child hook of a test that must not start signal-cli.
func neverCalled(t *testing.T) func(string) (string, []string) {
	return func(string) (string, []string) {
		t.Error("signal-cli was started")
		return "/nonexistent", nil
	}
}

func TestServeRefusesATCPAddressWithoutVerifiedTLS(t *testing.T) {
	logs := &logSink{}
	h := defaultHooks()
	h.child, h.logf = neverCalled(t), logs.logf
	start := time.Now()
	err := serve(context.Background(), settings{dsn: "postgres://u:SECRET-PW-VALUE@db.example/x?sslmode=require", socket: "/nonexistent/x", data: "/nonexistent"}, h)
	var ce *configError
	if !errors.As(err, &ce) || time.Since(start) > 10*time.Second {
		t.Fatalf("serve: %v after %v", err, time.Since(start))
	}
	if strings.Contains(err.Error(), "SECRET-PW-VALUE") {
		t.Fatalf("the error holds the password: %v", err)
	}
}

// fakePostgres accepts every login, like a server that does not check
// passwords; it returns a connection string for the sidecar role.
func fakePostgres(t *testing.T) string {
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
						_, _ = b.Receive()
					}
					return
				}
			}()
		}
	}()
	return "host=" + dir + " user=sidecar dbname=x sslmode=disable"
}

func TestServeRefusesADatabaseThatAcceptsAWrongPassword(t *testing.T) {
	logs := &logSink{}
	h := defaultHooks()
	h.child, h.logf = neverCalled(t), logs.logf
	start := time.Now()
	err := serve(context.Background(), settings{dsn: fakePostgres(t), socket: "/nonexistent/x", data: "/nonexistent"}, h)
	var ce *configError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "wrong password") || time.Since(start) > 10*time.Second {
		t.Fatalf("serve: %v after %v", err, time.Since(start))
	}
}

// fixture is a migrated temporary database with a provisioned sidecar role.
type fixture struct {
	t     *testing.T
	owner *db.DB
	dsn   string // of the sidecar role
}

func newFixture(t *testing.T, label string) *fixture {
	t.Helper()
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, label)
	owner, err := db.Open(tmp.DSN)
	if err != nil {
		t.Fatalf("open the temporary database: %s", dbtest.AtomicErrorSummary(err))
	}
	t.Cleanup(func() { owner.Close() })
	role := srv.NewSidecarRole(t, tmp)
	f := &fixture{t: t, owner: owner}
	f.exec(`CREATE ROLE ` + pgx.Identifier{role.Name}.Sanitize() + ` LOGIN PASSWORD '` + role.Password + `'`)
	f.exec(`SELECT public.signal_sidecar_grant($1::text::regrole)`, role.Name)
	if f.dsn, err = tmp.DSNFor(role.Name, role.Password); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.owner.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %s", q, dbtest.AtomicErrorSummary(err))
	}
}

func (f *fixture) status() (state string, handled int64, errText string, number string) {
	f.t.Helper()
	if err := f.owner.QueryRow(`SELECT state, handled_gen, last_error, account_number FROM public.signal_sidecar_status`).
		Scan(&state, &handled, &errText, &number); err != nil && !errors.Is(err, sql.ErrNoRows) {
		f.t.Fatal(dbtest.AtomicErrorSummary(err))
	}
	return
}

func (f *fixture) waitStatus(what string, d time.Duration, cond func(state string, handled int64, errText, number string) bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for {
		state, handled, errText, number := f.status()
		if cond(state, handled, errText, number) {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("after %v: %s did not happen (state %s, handled %d, error %q)", d, what, state, handled, errText)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The whole sidecar against a database and the fake CLI: it waits for the
// database to confirm password authentication before it starts signal-cli,
// links, sends over the socket and stops cleanly on a signal.
func TestServeRunsTheSidecarEndToEnd(t *testing.T) {
	const number = "+48600100101"
	t.Setenv(envDSN, "postgres://leak:SECRET-DSN-VALUE@db/x")
	f := newFixture(t, "mainsrv")
	f.exec(`UPDATE public.signal_sidecar_config SET db_auth_state = 'unchecked'`)

	dir, err := os.MkdirTemp("", "sc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data := filepath.Join(dir, "data")
	sockDir := filepath.Join(dir, "sock")
	for _, d := range []string{data, sockDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(sockDir, "api.sock")
	group := "dGhpcy1pcy1hLXRlc3QtZ3JvdXAtaWQtMzItYnl0ZXM="
	calls, starts, dump := filepath.Join(dir, "calls"), filepath.Join(dir, "starts"), filepath.Join(dir, "dump")
	rawScenario, _ := json.Marshal(map[string]any{
		"calls_file": calls, "starts_file": starts, "dump_file": dump,
		"finish": map[string]any{"mode": "after", "after_ms": 100, "number": number},
	})
	scenario := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(scenario, rawScenario, 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &logSink{}
	h := defaultHooks()
	h.parseDSN = pgx.ParseConfig // the test database is on TCP without TLS
	h.child = func(dataDir string) (string, []string) {
		return fakeBin, []string{"-c", dataDir, "--scenario", scenario}
	}
	h.logf = logs.logf

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, settings{dsn: f.dsn, socket: socket, data: data}, h) }()

	// The database has not confirmed password authentication: this is a
	// transient condition, so no error is reported (the state stays unknown),
	// and signal-cli is not started.
	time.Sleep(2 * time.Second)
	f.waitStatus("no error", time.Second, func(state string, _ int64, errText, _ string) bool {
		return state == "unknown" && errText == ""
	})
	if exists(starts) || exists(socket) {
		t.Fatal("signal-cli or the socket was started before the database confirmed the authentication")
	}

	f.exec(`UPDATE public.signal_sidecar_config SET db_auth_state = 'ok'`)
	f.waitStatus("unlinked", 15*time.Second, func(state string, _ int64, _, _ string) bool { return state == "unlinked" })
	if !exists(starts) || !exists(socket) {
		t.Fatal("signal-cli or the socket did not come up")
	}
	if err := healthcheck(socket); err != nil {
		t.Fatalf("health check: %v", err)
	}
	raw, _ := os.ReadFile(dump)
	if strings.Contains(string(raw), "SECRET-DSN-VALUE") || strings.Contains(string(raw), envDSN) {
		t.Fatalf("the child saw the database settings: %s", raw)
	}

	// Link and confirm through the database, then send over the socket.
	f.exec(`UPDATE public.signal_sidecar_config SET desired = 'link', gen = gen + 1, confirmed_account = ''`)
	f.waitStatus("linked", 15*time.Second, func(state string, handled int64, _, n string) bool {
		return state == "linked" && handled == 1 && n == number
	})
	f.exec(`UPDATE public.signal_sidecar_config SET desired = 'idle', confirmed_account = $1`, number)

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}}}
	body, _ := json.Marshal(map[string]any{"number": number, "recipients": []string{"group." + base64.StdEncoding.EncodeToString([]byte(group))},
		"message": "hello", "text_mode": "normal"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Post("http://sidecar/v2/send", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		status := resp.StatusCode
		resp.Body.Close()
		if status == http.StatusCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("send answered %d", status)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rawCalls, _ := os.ReadFile(calls); !strings.Contains(string(rawCalls), `"method":"send"`) {
		t.Fatalf("signal-cli got no send:\n%s", rawCalls)
	}

	// A signal ends it cleanly: socket removed, child gone.
	pidLine, _ := os.ReadFile(starts)
	var pid int
	_, _ = fmt.Sscanf(string(pidLine), "%d", &pid)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not return")
	}
	if exists(socket) {
		t.Error("the socket file is still there")
	}
	if pid > 0 {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("the child is still running: %v", err)
		}
	}
	if got := logs.all(); strings.Contains(got, "SECRET-DSN-VALUE") || strings.Contains(got, number) {
		t.Errorf("the log holds a secret:\n%s", got)
	}
}

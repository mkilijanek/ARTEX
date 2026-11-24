package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/notify"
	"golang.org/x/crypto/bcrypt"
)

const (
	sigTestPassword = "correct horse battery"
	sigTestNumber   = "+15555550101"
	sigTestURI      = "sgnl://linkdevice?uuid=SENTINELURI&pub_key=abc"
	sigTestGroup    = "group.QUFFQ0F3UUZCZ2NJQ1FvTERBME9EeEFSRWhNVUZSWVhHQmthR3h3ZEhoOD0="
)

// syncBuf is a log sink that tests can read while the server writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// signalRig is a Server with a real database, an admin password and the Signal routes behind the same
// authentication wrapper as in production. The test plays the sidecar by writing the status row as the owner.
type signalRig struct {
	t     *testing.T
	s     *Server
	pg    *db.DB
	h     http.Handler
	token string
	logs  *syncBuf
}

func newSignalRig(t *testing.T) *signalRig {
	t.Helper()
	useThrottle(t)
	signalConfirmBackoff.clearAll()
	t.Cleanup(signalConfirmBackoff.clearAll)
	m, err := NewManager(t.TempDir(), "", testDSN())
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.pg

	prev, had, err := pg.GetSetting(authPassKey)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(sigTestPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting(authPassKey, string(hash)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = pg.SetSetting(authPassKey, prev)
		} else {
			_, _ = pg.Exec(`DELETE FROM settings WHERE key=$1`, authPassKey)
		}
	})

	resetSignalRows(t, pg)
	t.Cleanup(func() { resetSignalRows(t, pg) })

	// The service socket is configured (its path is never dialled by these routes); the role is ready.
	notify.LoadSignalEnv(func(k string) string {
		if k == notify.SignalAPIEnv {
			return "unix:/nonexistent/signal.sock"
		}
		return ""
	})
	oldRole := signalRole.Load()
	setSignalRole(db.SignalRoleStatus{State: db.SignalRoleOK, Auth: "ok"})
	t.Cleanup(func() {
		signalRole.Store(oldRole)
		notify.LoadSignalEnv(os.Getenv)
	})

	logs := &syncBuf{}
	oldOut := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(oldOut) })

	key := []byte("0123456789abcdef0123456789abcdef")
	s := &Server{m: m, jwtKey: key}
	mux := http.NewServeMux()
	s.registerSignalRoutes(mux)
	return &signalRig{t: t, s: s, pg: pg, h: s.requireAuth(mux), token: subjectTestToken(t, key, "operator-7", time.Hour), logs: logs}
}

func resetSignalRows(t *testing.T, pg *db.DB) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM signal_sidecar_config`, `DELETE FROM signal_sidecar_status`,
		`INSERT INTO signal_sidecar_config DEFAULT VALUES`, `INSERT INTO signal_sidecar_status DEFAULT VALUES`,
	} {
		if _, err := pg.Exec(q); err != nil {
			t.Fatalf("reset signal rows: %v", err)
		}
	}
}

// req sends a request through requireAuth and the route wrapper. contentType "" sends none.
func (g *signalRig) req(method, path, contentType, body string, hdr map[string]string) *httptest.ResponseRecorder {
	g.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	return w
}

func (g *signalRig) auth() map[string]string {
	return map[string]string{"Authorization": "Bearer " + g.token}
}

func (g *signalRig) get(path string) *httptest.ResponseRecorder {
	g.t.Helper()
	return g.req("GET", path, "", "", g.auth())
}

func (g *signalRig) del(path string) *httptest.ResponseRecorder {
	g.t.Helper()
	return g.req("DELETE", path, "", "", g.auth())
}

func (g *signalRig) post(path string, body any) *httptest.ResponseRecorder {
	g.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		g.t.Fatal(err)
	}
	return g.req("POST", path, "application/json", string(raw), g.auth())
}

func (g *signalRig) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.pg.Exec(q, args...); err != nil {
		g.t.Fatalf("%s: %v", q, err)
	}
}

// sidecar plays one heartbeat: the state and the generation handled; ageS is how old the heartbeat is.
func (g *signalRig) sidecar(state string, handled int64, number string, ageS float64) {
	g.t.Helper()
	g.exec(`UPDATE signal_sidecar_status SET state=$1::text, handled_gen=$2, account_number=$3::text,
	        account_aci=CASE WHEN $1::text='linked' THEN '00000000-0000-0000-0000-000000000001' ELSE '' END,
	        link_uri=NULL, link_expires_at=NULL, heartbeat_at=now() - make_interval(secs => $4::float8),
	        sidecar_version='sc-1', signal_cli_version='cli-1' WHERE id`, state, handled, number, ageS)
}

// pendingLink plays the sidecar storing the link address for the current generation.
func (g *signalRig) pendingLink(secsLeft float64) {
	g.t.Helper()
	g.exec(`UPDATE signal_sidecar_status SET state='linking', link_uri=$1::text,
	        link_expires_at=now() + make_interval(secs => $2::float8), heartbeat_at=now() WHERE id`, sigTestURI, secsLeft)
}

type signalCfgRow struct {
	desired   string
	gen       int64
	confirmed string
	refresh   int64
	by        string
}

func (g *signalRig) config() signalCfgRow {
	g.t.Helper()
	var c signalCfgRow
	if err := g.pg.QueryRow(`SELECT desired, gen, confirmed_account, groups_refresh_gen, requested_by FROM signal_sidecar_config WHERE id`).
		Scan(&c.desired, &c.gen, &c.confirmed, &c.refresh, &c.by); err != nil {
		g.t.Fatal(err)
	}
	return c
}

func decodeSignalMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not a JSON object (%d): %q", w.Code, w.Body.String())
	}
	return m
}

func wantSignalCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body)
	}
	if code != "" && decodeSignalMap(t, w)["code"] != code {
		t.Fatalf("code = %v, want %q: %s", decodeSignalMap(t, w)["code"], code, w.Body)
	}
}

func wantSignalNoStore(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (status %d)", got, w.Code)
	}
}

func signalLinkBody() map[string]any {
	return map[string]any{"password": sigTestPassword, "device_name": "ARTEX test", "acknowledge": true}
}

// ---- wrapper ----

func TestSignalRoutesTakeTheTokenFromTheAuthorizationHeaderOnly(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	for _, path := range []string{"/api/notify/signal/account", "/api/notify/signal/groups", "/api/notify/signal/link"} {
		cookie := g.req("GET", path, "", "", map[string]string{"Cookie": "artex_token=" + g.token})
		wantSignalCode(t, cookie, 401, "")
		wantSignalNoStore(t, cookie)
		query := g.req("GET", path+"?token="+g.token, "", "", nil)
		wantSignalCode(t, query, 401, "")
		wantSignalNoStore(t, query)
		if w := g.req("GET", path, "", "", nil); w.Code != 401 {
			t.Fatalf("%s without a token: %d", path, w.Code)
		}
		wrong := subjectTestToken(t, []byte("another-key-another-key-another-key"), "operator-7", time.Hour)
		if w := g.req("GET", path, "", "", map[string]string{"Authorization": "Bearer " + wrong}); w.Code != 401 {
			t.Fatalf("%s with a token of another key: %d", path, w.Code)
		}
		if w := g.get(path); w.Code == 401 {
			t.Fatalf("%s refused the header token", path)
		}
	}
	// A cookie token must not start a link either: nothing is written.
	raw, _ := json.Marshal(signalLinkBody())
	w := g.req("POST", "/api/notify/signal/link", "application/json", string(raw), map[string]string{"Cookie": "artex_token=" + g.token})
	wantSignalCode(t, w, 401, "")
	if c := g.config(); c.gen != 0 || c.desired != "idle" {
		t.Fatalf("a cookie-authenticated request changed the row: %+v", c)
	}
}

func TestSignalRoutesRefuseATokenWithoutASubject(t *testing.T) {
	g := newSignalRig(t)
	empty := subjectTestToken(t, g.s.jwtKey, "", time.Hour)
	w := g.req("GET", "/api/notify/signal/account", "", "", map[string]string{"Authorization": "Bearer " + empty})
	wantSignalCode(t, w, 401, "")
	wantSignalNoStore(t, w)
	raw, _ := json.Marshal(signalLinkBody())
	w = g.req("POST", "/api/notify/signal/link", "application/json", string(raw), map[string]string{"Authorization": "Bearer " + empty})
	wantSignalCode(t, w, 401, "")
	if g.config().gen != 0 {
		t.Fatal("a token without a subject started a link")
	}
}

func TestSignalRoutesWantJSONForARequestBody(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	raw, _ := json.Marshal(signalLinkBody())
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		w := g.req("POST", "/api/notify/signal/link", ct, string(raw), g.auth())
		wantSignalCode(t, w, 415, "")
		wantSignalNoStore(t, w)
	}
	if g.config().gen != 0 {
		t.Fatal("a request with the wrong content type started a link")
	}
	for _, p := range []string{"/api/notify/signal/unlink", "/api/notify/signal/account/confirm", "/api/notify/signal/groups/refresh"} {
		if w := g.req("POST", p, "text/plain", "{}", g.auth()); w.Code != 415 {
			t.Fatalf("%s with text/plain: %d", p, w.Code)
		}
	}
	// charset parameters are fine.
	w := g.req("POST", "/api/notify/signal/link", "application/json; charset=utf-8", string(raw), g.auth())
	wantSignalCode(t, w, 202, "")
}

func TestSignalRoutesReportAServiceThatIsNotConfigured(t *testing.T) {
	g := newSignalRig(t)
	notify.LoadSignalEnv(func(string) string { return "" })
	w := g.get("/api/notify/signal/account")
	wantSignalCode(t, w, 200, "")
	if m := decodeSignalMap(t, w); m["mode"] != "off" {
		t.Fatalf("mode = %v, want off", m["mode"])
	}
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 503, "not_configured")
	if g.config().gen != 0 {
		t.Fatal("a link was requested without a configured service")
	}
}

// ---- status ----

func TestSignalAccountStatusShape(t *testing.T) {
	g := newSignalRig(t)

	// Never reported: unknown, not alive.
	w := g.get("/api/notify/signal/account")
	wantSignalCode(t, w, 200, "")
	wantSignalNoStore(t, w)
	m := decodeSignalMap(t, w)
	sc := m["sidecar"].(map[string]any)
	if m["mode"] != "on" || m["state"] != "unknown" || sc["alive"] != false || m["linking"] != nil || m["groups_at"] != nil || m["last_error_at"] != nil {
		t.Fatalf("unexpected status before any heartbeat: %s", w.Body)
	}
	for _, k := range []string{"mode", "sidecar", "state", "gen", "account", "linking", "groups_count", "groups_at", "remote_unlink",
		"last_error", "last_error_at", "db_role", "db_auth", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status has no %q", k)
		}
	}
	if warn, ok := m["warnings"].([]any); !ok || len(warn) != 0 {
		t.Fatalf("warnings = %v, want an empty list", m["warnings"])
	}
	if role := m["db_role"].(map[string]any); role["state"] != "ok" {
		t.Fatalf("db_role = %v", role)
	}

	// Linked, not confirmed: a hint, never the number.
	g.exec(`UPDATE signal_sidecar_config SET gen = 1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 4)
	g.exec(`UPDATE signal_sidecar_status SET last_error='cannot reach ` + sigTestNumber + ` or ` + sigTestURI + `', last_error_at=now() WHERE id`)
	w = g.get("/api/notify/signal/account")
	m = decodeSignalMap(t, w)
	acct := m["account"].(map[string]any)
	sc = m["sidecar"].(map[string]any)
	if m["state"] != "linked" || acct["confirmed"] != false || acct["number_hint"] != "+15… (11 digits)" {
		t.Fatalf("linked status: %s", w.Body)
	}
	if sc["alive"] != true || sc["version"] != "sc-1" || sc["signal_cli_version"] != "cli-1" || sc["heartbeat_age_s"].(float64) < 3 {
		t.Fatalf("sidecar block: %v", sc)
	}
	for _, leak := range []string{sigTestNumber, "5555550101", "SENTINELURI", "sgnl://"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Fatalf("status leaks %q: %s", leak, w.Body)
		}
	}
	if m["last_error_at"] == nil {
		t.Fatal("last_error_at missing")
	}

	// Confirmed, with the expiry of a pending link on another row version.
	g.exec(`UPDATE signal_sidecar_config SET confirmed_account=$1 WHERE id`, sigTestNumber)
	m = decodeSignalMap(t, g.get("/api/notify/signal/account"))
	if m["account"].(map[string]any)["confirmed"] != true {
		t.Fatalf("confirmed = %v", m["account"])
	}

	// A stale heartbeat is not alive.
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 45)
	m = decodeSignalMap(t, g.get("/api/notify/signal/account"))
	if m["sidecar"].(map[string]any)["alive"] != false {
		t.Fatalf("a 45 s old heartbeat is alive: %v", m["sidecar"])
	}
}

func TestSignalAccountStatusCarriesTheLinkExpiry(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=1, requested_at=now() WHERE id`)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	g.pendingLink(90)
	w := g.get("/api/notify/signal/account")
	m := decodeSignalMap(t, w)
	lk, ok := m["linking"].(map[string]any)
	if m["state"] != "linking" || !ok {
		t.Fatalf("linking status: %s", w.Body)
	}
	exp, err := time.Parse(time.RFC3339, lk["expires_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(exp); left < 80*time.Second || left > 95*time.Second {
		t.Fatalf("expiry in %v, want about 90 s", left)
	}
	if strings.Contains(w.Body.String(), "SENTINELURI") {
		t.Fatal("status carries the link address")
	}
}

func TestSignalAccountStatusReconcilesAFinishedCommand(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=1, requested_at=now() WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	if g.config().desired != "link" {
		t.Fatal("setup")
	}
	wantSignalCode(t, g.get("/api/notify/signal/account"), 200, "")
	if d := g.config().desired; d != "idle" {
		t.Fatalf("desired = %q after the sidecar finished the generation, want idle", d)
	}

	// A dead sidecar never finishes: the link is given up after its lifetime plus a grace.
	g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=2, requested_at=now() - interval '10 minutes' WHERE id`)
	g.sidecar(db.SignalStateUnlinked, 1, "", 120)
	wantSignalCode(t, g.get("/api/notify/signal/account"), 200, "")
	if d := g.config().desired; d != "idle" {
		t.Fatalf("desired = %q for a link that outlived its lifetime, want idle", d)
	}
}

// ---- link ----

func TestSignalLinkHappyPath(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 1)

	w := g.post("/api/notify/signal/link", signalLinkBody())
	wantSignalCode(t, w, 202, "")
	wantSignalNoStore(t, w)
	if m := decodeSignalMap(t, w); m["gen"] != float64(1) {
		t.Fatalf("link response = %s", w.Body)
	}
	if c := g.config(); c.desired != "link" || c.gen != 1 || c.by != "operator-7" {
		t.Fatalf("config after link = %+v", c)
	}
	// Nothing is pending until the sidecar stores the address.
	wantSignalCode(t, g.get("/api/notify/signal/link"), 404, "")

	g.pendingLink(100)
	w = g.get("/api/notify/signal/link")
	wantSignalCode(t, w, 200, "")
	wantSignalNoStore(t, w)
	m := decodeSignalMap(t, w)
	if m["uri"] != sigTestURI || m["gen"] != float64(1) || m["expires_at"] == "" {
		t.Fatalf("link = %s", w.Body)
	}

	// The phone scanned: the sidecar reports linked, the address is gone, the command is reconciled.
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	wantSignalCode(t, g.get("/api/notify/signal/link"), 404, "")
	m = decodeSignalMap(t, g.get("/api/notify/signal/account"))
	if m["state"] != "linked" || m["account"].(map[string]any)["confirmed"] != false {
		t.Fatalf("status after link: %v", m)
	}
	if g.config().desired != "idle" {
		t.Fatal("desired is not back to idle")
	}
	var uri *string
	if err := g.pg.QueryRow(`SELECT link_uri FROM signal_sidecar_status WHERE id`).Scan(&uri); err != nil || uri != nil {
		t.Fatalf("link_uri = %v err=%v", uri, err)
	}

	logs := g.logs.String()
	if !strings.Contains(logs, "[signal] link requested by operator-7 (generation 1)") {
		t.Fatalf("no audit line with the operator: %q", logs)
	}
	for _, leak := range []string{"SENTINELURI", "sgnl://", sigTestPassword, sigTestNumber, "5555550101"} {
		if strings.Contains(logs, leak) {
			t.Fatalf("log leaks %q: %s", leak, logs)
		}
	}
}

func TestSignalLinkDefaultsTheDeviceName(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.post("/api/notify/signal/link", map[string]any{"password": sigTestPassword, "acknowledge": true}), 202, "")
	var name string
	if err := g.pg.QueryRow(`SELECT device_name FROM signal_sidecar_config WHERE id`).Scan(&name); err != nil || name != "ARTEX" {
		t.Fatalf("device name = %q err=%v", name, err)
	}
}

func TestSignalLinkRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(g *signalRig)
		body   map[string]any
		status int
		code   string
	}{
		{"acknowledge missing", nil, map[string]any{"password": sigTestPassword}, 400, ""},
		{"acknowledge false", nil, map[string]any{"password": sigTestPassword, "acknowledge": false}, 400, ""},
		{"no password", nil, map[string]any{"acknowledge": true}, 400, ""},
		{"bad device name", nil, map[string]any{"password": sigTestPassword, "acknowledge": true, "device_name": "bad/name"}, 400, ""},
		{"wrong password", nil, map[string]any{"password": "nope", "acknowledge": true}, 403, "password_incorrect"},
		{"link pending", func(g *signalRig) {
			g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=1, requested_at=now() WHERE id`)
		}, nil, 409, "link_pending"},
		{"link pending and shown", func(g *signalRig) {
			g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=1, requested_at=now() WHERE id`)
			g.pendingLink(60)
		}, nil, 409, "link_pending"},
		{"already linked", func(g *signalRig) {
			g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
			g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
		}, nil, 409, "already_linked"},
		{"error state", func(g *signalRig) { g.sidecar(db.SignalStateError, 0, "", 0) }, nil, 409, "unlink_first"},
		{"state unknown", func(g *signalRig) {
			g.sidecar(db.SignalStateUnlinked, 0, "", 0)
			g.exec(`UPDATE signal_sidecar_status SET state='unknown' WHERE id`)
		}, nil, 409, "not_ready"},
		{"no heartbeat", func(g *signalRig) { g.exec(`UPDATE signal_sidecar_status SET heartbeat_at=NULL WHERE id`) }, nil, 503, "sidecar_unavailable"},
		{"stale heartbeat", func(g *signalRig) { g.sidecar(db.SignalStateUnlinked, 0, "", 31) }, nil, 503, "sidecar_unavailable"},
		{"role missing", func(g *signalRig) { setSignalRole(db.SignalRoleStatus{State: db.SignalRoleMissing}) }, nil, 503, "db_role_not_ready"},
		{"role error", func(g *signalRig) { setSignalRole(db.SignalRoleStatus{State: db.SignalRoleError, Detail: "x"}) }, nil, 503, "db_role_not_ready"},
		{"role unchecked", func(g *signalRig) { setSignalRole(db.SignalRoleStatus{State: db.SignalRoleUnchecked}) }, nil, 503, "db_role_not_ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newSignalRig(t)
			g.sidecar(db.SignalStateUnlinked, 0, "", 0)
			if tc.setup != nil {
				tc.setup(g)
			}
			before := g.config()
			body := tc.body
			if body == nil {
				body = signalLinkBody()
			}
			w := g.post("/api/notify/signal/link", body)
			wantSignalCode(t, w, tc.status, tc.code)
			wantSignalNoStore(t, w)
			if tc.status != 202 && g.config() != before {
				t.Fatalf("a refused request changed the row: %+v -> %+v", before, g.config())
			}
		})
	}
}

// A wrong password is 403 with a code, never 401 (the client treats 401 as a lost session), and it counts
// against the login throttle.
func TestSignalWrongPasswordIsThrottled(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	bad := map[string]any{"password": "nope", "acknowledge": true}
	for i := 0; i < 5; i++ {
		wantSignalCode(t, g.post("/api/notify/signal/link", bad), 403, "password_incorrect")
	}
	w := g.post("/api/notify/signal/link", signalLinkBody()) // a correct password is refused while locked out
	wantSignalCode(t, w, 429, "")
	wantSignalNoStore(t, w)
	if secs, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || secs <= 0 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if g.config().gen != 0 {
		t.Fatal("a locked-out request started a link")
	}
	// The unlink route shares the counter.
	wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{"password": sigTestPassword}), 429, "")
}

func TestSignalCorrectPasswordResetsTheFailureCount(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	for i := 0; i < 4; i++ {
		wantSignalCode(t, g.post("/api/notify/signal/link", map[string]any{"password": "nope", "acknowledge": true}), 403, "password_incorrect")
	}
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 202, "") // success resets the counter
	g.exec(`UPDATE signal_sidecar_config SET desired='idle' WHERE id`)
	g.sidecar(db.SignalStateUnlinked, 1, "", 0)
	for i := 0; i < 4; i++ {
		wantSignalCode(t, g.post("/api/notify/signal/link", map[string]any{"password": "nope", "acknowledge": true}), 403, "password_incorrect")
	}
}

func TestSignalSecondLinkWhileOneIsPendingIsAConflict(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 202, "")
	// A second request is a conflict until the first finishes or is cancelled.
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 409, "link_pending")
}

func TestSignalCancelLink(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.del("/api/notify/signal/link"), 409, "")
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 202, "")
	g.pendingLink(100)
	wantSignalCode(t, g.get("/api/notify/signal/link"), 200, "")

	w := g.del("/api/notify/signal/link")
	wantSignalCode(t, w, 202, "")
	wantSignalNoStore(t, w)
	if c := g.config(); c.desired != "idle" || c.gen != 1 {
		t.Fatalf("config after cancel = %+v", c)
	}
	// The address is withheld at once, before the sidecar has cleared it.
	wantSignalCode(t, g.get("/api/notify/signal/link"), 404, "")
	wantSignalCode(t, g.del("/api/notify/signal/link"), 409, "")
	if !strings.Contains(g.logs.String(), "[signal] link cancelled by operator-7") {
		t.Fatalf("no audit line: %q", g.logs.String())
	}
}

func TestSignalLinkAddressIsWithheldAfterExpiry(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=1, requested_at=now() WHERE id`)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	g.pendingLink(-1)
	w := g.get("/api/notify/signal/link")
	wantSignalCode(t, w, 404, "")
	if strings.Contains(w.Body.String(), "SENTINELURI") {
		t.Fatal("expired address served")
	}
}

// ---- confirm ----

func TestSignalConfirmAccount(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=3 WHERE id`)
	g.sidecar(db.SignalStateLinked, 3, sigTestNumber, 0)

	// Stale generation and unparsable digits are not guesses.
	wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 2, "last_digits": "550101"}), 409, "stale")
	for _, digits := range []string{"", "12345", "1234567", "55010a", "+55010"} {
		wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 3, "last_digits": digits}), 400, "")
	}
	w := g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 3, "last_digits": "000000"})
	wantSignalCode(t, w, 403, "number_mismatch")
	wantSignalNoStore(t, w)
	if g.config().confirmed != "" {
		t.Fatal("wrong digits confirmed the account")
	}

	w = g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 3, "last_digits": "550101"})
	wantSignalCode(t, w, 200, "")
	wantSignalNoStore(t, w)
	if g.config().confirmed != sigTestNumber {
		t.Fatal("the account was not confirmed")
	}
	m := decodeSignalMap(t, g.get("/api/notify/signal/account"))
	if m["account"].(map[string]any)["confirmed"] != true {
		t.Fatalf("status = %v", m["account"])
	}
	logs := g.logs.String()
	if !strings.Contains(logs, "[signal] account confirmed by operator-7") || !strings.Contains(logs, "confirmation refused for operator-7") {
		t.Fatalf("audit lines missing: %q", logs)
	}
	for _, leak := range []string{"550101", "000000", sigTestNumber} {
		if strings.Contains(logs, leak) {
			t.Fatalf("log leaks %q: %s", leak, logs)
		}
	}
}

func TestSignalConfirmRefusesWhenNothingIsLinked(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 0, "last_digits": "550101"}), 409, "stale")
}

func TestSignalConfirmGuessesAreThrottled(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	for i := 0; i < 3; i++ {
		wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": fmt.Sprintf("00000%d", i)}), 403, "number_mismatch")
	}
	w := g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": "550101"})
	wantSignalCode(t, w, 429, "")
	if secs, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || secs <= 0 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	if g.config().confirmed != "" {
		t.Fatal("the account was confirmed while locked out")
	}
}

// Wrong digits cost more with every further wrong attempt: from the 3rd the wait is 10 s and doubles up
// to 15 minutes, whatever the shared password throttle would allow. A right answer or a new link clears it.
func TestSignalConfirmWrongDigitsBackOffProgressively(t *testing.T) {
	g := newSignalRig(t)
	clk := useThrottle(t)
	old := signalConfirmBackoff
	signalConfirmBackoff = &signalConfirmBackoffState{now: clk.now, m: map[string]*signalConfirmEntry{}}
	t.Cleanup(func() { signalConfirmBackoff = old })
	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	wrong := func() *httptest.ResponseRecorder {
		return g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": "000000"})
	}
	retryAfter := func(w *httptest.ResponseRecorder) int {
		t.Helper()
		secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
		if err != nil {
			t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
		}
		return secs
	}
	for i := 0; i < 3; i++ {
		wantSignalCode(t, wrong(), 403, "number_mismatch")
	}
	for _, want := range []int{10, 20, 40, 80, 160, 320, 640, 900, 900} {
		w := wrong()
		wantSignalCode(t, w, 429, "")
		if got := retryAfter(w); got != want {
			t.Fatalf("Retry-After = %d, want %d", got, want)
		}
		// A correct answer is refused while the wait runs, and one second short of it still is.
		clk.advance(time.Duration(want)*time.Second - time.Second)
		wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": "550101"}), 429, "")
		clk.advance(time.Second)
		wantSignalCode(t, wrong(), 403, "number_mismatch")
	}
	// A new link request clears the back-off.
	signalConfirmBackoff.clearAll()
	clk.advance(time.Hour)
	wantSignalCode(t, wrong(), 403, "number_mismatch")
	// A right answer clears it too.
	wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": "550101"}), 200, "")
	if n := len(signalConfirmBackoff.m); n != 0 {
		t.Fatalf("%d back-off entries left after a right answer", n)
	}
}

func TestSignalLinkAndUnlinkClearTheConfirmation(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=1, confirmed_account=$1 WHERE id`, sigTestNumber)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{"password": sigTestPassword}), 202, "")
	if g.config().confirmed != "" {
		t.Fatal("unlink kept the confirmation")
	}
}

// A session that holds the rows locked makes a command fail with a named, retryable answer; the answer and the
// log carry nothing of the request.
func TestSignalCommandsAnswerBusyWhenTheRowsAreLocked(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	tx, err := g.pg.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // releases the lock
	if _, err := tx.Exec(`SELECT 1 FROM signal_sidecar_config WHERE id FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/notify/signal/groups/refresh", map[string]any{}},
		{"POST", "/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": "550101"}},
		{"POST", "/api/notify/signal/unlink", map[string]any{"password": sigTestPassword}},
	} {
		w := g.post(c.path, c.body)
		wantSignalCode(t, w, 503, "busy")
		wantSignalNoStore(t, w)
	}
	for _, leak := range []string{sigTestPassword, "550101", sigTestNumber} {
		if strings.Contains(g.logs.String(), leak) {
			t.Fatalf("log leaks %q: %s", leak, g.logs.String())
		}
	}
	if !strings.Contains(g.logs.String(), "[signal] groups refresh:") {
		t.Fatalf("the failure is not logged: %q", g.logs.String())
	}
}

// ---- unlink ----

func TestSignalUnlink(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)

	wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{"password": "nope"}), 403, "password_incorrect")
	wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{}), 400, "")
	if g.config().desired != "idle" {
		t.Fatal("a refused unlink changed the row")
	}
	w := g.post("/api/notify/signal/unlink", map[string]any{"password": sigTestPassword})
	wantSignalCode(t, w, 202, "")
	wantSignalNoStore(t, w)
	if c := g.config(); c.desired != "unlink" || c.gen != 2 {
		t.Fatalf("config after unlink = %+v", c)
	}
	wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{"password": sigTestPassword}), 409, "unlink_pending")
	if !strings.Contains(g.logs.String(), "[signal] unlink requested by operator-7 (generation 2)") {
		t.Fatalf("no audit line: %q", g.logs.String())
	}

	// The sidecar finishes: the status carries what it could do on Signal's side.
	g.sidecar(db.SignalStateUnlinked, 2, "", 0)
	g.exec(`UPDATE signal_sidecar_status SET remote_unlink='local_only' WHERE id`)
	m := decodeSignalMap(t, g.get("/api/notify/signal/account"))
	if m["state"] != "unlinked" || m["remote_unlink"] != "local_only" || m["account"].(map[string]any)["number_hint"] != "" {
		t.Fatalf("status after unlink: %v", m)
	}
	if g.config().desired != "idle" {
		t.Fatal("desired is not back to idle")
	}
}

func TestSignalUnlinkFromErrorStateAndRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(g *signalRig)
		status int
		code   string
	}{
		{"error state is accepted", func(g *signalRig) { g.sidecar(db.SignalStateError, 0, "", 0) }, 202, ""},
		{"unlinked", func(g *signalRig) { g.sidecar(db.SignalStateUnlinked, 0, "", 0) }, 409, "nothing_to_unlink"},
		{"unknown", func(g *signalRig) {
			g.sidecar(db.SignalStateUnlinked, 0, "", 0)
			g.exec(`UPDATE signal_sidecar_status SET state='unknown' WHERE id`)
		}, 409, "nothing_to_unlink"},
		{"link pending", func(g *signalRig) {
			g.exec(`UPDATE signal_sidecar_config SET desired='link', gen=1, requested_at=now() WHERE id`)
			g.pendingLink(60)
		}, 409, "link_pending"},
		{"no heartbeat", func(g *signalRig) {
			g.sidecar(db.SignalStateLinked, 0, sigTestNumber, 0)
			g.exec(`UPDATE signal_sidecar_status SET heartbeat_at=NULL WHERE id`)
		}, 503, "sidecar_unavailable"},
		{"stale heartbeat", func(g *signalRig) { g.sidecar(db.SignalStateLinked, 0, sigTestNumber, 40) }, 503, "sidecar_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newSignalRig(t)
			tc.setup(g)
			before := g.config()
			wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{"password": sigTestPassword}), tc.status, tc.code)
			if tc.status != 202 && g.config() != before {
				t.Fatalf("a refused unlink changed the row: %+v -> %+v", before, g.config())
			}
		})
	}
}

// ---- groups ----

func TestSignalGroupsAndRefresh(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.post("/api/notify/signal/groups/refresh", map[string]any{}), 409, "not_linked")

	w := g.get("/api/notify/signal/groups")
	wantSignalCode(t, w, 200, "")
	if m := decodeSignalMap(t, w); m["fetched_at"] != nil || len(m["groups"].([]any)) != 0 {
		t.Fatalf("empty list = %s", w.Body)
	}

	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	g.exec(`UPDATE signal_sidecar_status SET groups=$1::jsonb, groups_at=now() WHERE id`,
		fmt.Sprintf(`[{"id":%q,"name":"Ops room"}]`, sigTestGroup))

	w = g.get("/api/notify/signal/groups")
	wantSignalCode(t, w, 200, "")
	wantSignalNoStore(t, w)
	m := decodeSignalMap(t, w)
	list := m["groups"].([]any)
	if len(list) != 1 || m["fetched_at"] == nil {
		t.Fatalf("groups = %s", w.Body)
	}
	e := list[0].(map[string]any)
	if e["id"] != sigTestGroup || e["name"] != "Ops room" || e["masked"] != "group.…"+sigTestGroup[len(sigTestGroup)-4:] {
		t.Fatalf("group = %v", e)
	}
	if strings.Contains(e["masked"].(string), sigTestGroup[len("group."):len(sigTestGroup)-4]) {
		t.Fatal("masked id shows more than the end of the id")
	}
	if n := decodeSignalMap(t, g.get("/api/notify/signal/account"))["groups_count"]; n != float64(1) {
		t.Fatalf("groups_count = %v", n)
	}

	w = g.post("/api/notify/signal/groups/refresh", map[string]any{})
	wantSignalCode(t, w, 202, "")
	wantSignalNoStore(t, w)
	if g.config().refresh != 1 {
		t.Fatal("the refresh was not requested")
	}
}

// ---- the sender hook ----

func TestSignalSenderFollowsTheRows(t *testing.T) {
	g := newSignalRig(t)
	clock := time.Unix(1_700_000_000, 0)
	c := newSignalSender(g.pg)
	c.now = func() time.Time { return clock }
	next := func() (string, string) {
		clock = clock.Add(signalSenderTTL + time.Second)
		return c.sender(context.Background())
	}

	if n, p := next(); n != "" || p != "Signal service is not connected" {
		t.Fatalf("no heartbeat: %q %q", n, p)
	}
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	if n, p := next(); n != "" || p != "Signal account is not linked: link one under System, Notifications" {
		t.Fatalf("unlinked: %q %q", n, p)
	}
	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	if n, p := next(); n != sigTestNumber || p != "confirm the linked Signal account first" {
		t.Fatalf("linked, not confirmed: %q %q", n, p)
	}
	g.exec(`UPDATE signal_sidecar_config SET confirmed_account=$1 WHERE id`, sigTestNumber)
	if n, p := next(); n != sigTestNumber || p != "" {
		t.Fatalf("confirmed: %q %q", n, p)
	}
	// A confirmation of another number than the linked one does not count.
	g.exec(`UPDATE signal_sidecar_config SET confirmed_account='+15555550106' WHERE id`)
	if n, p := next(); p != "confirm the linked Signal account first" {
		t.Fatalf("confirmed another number: %q %q", n, p)
	}
	g.exec(`UPDATE signal_sidecar_config SET confirmed_account=$1 WHERE id`, sigTestNumber)
	if _, p := next(); p != "" {
		t.Fatalf("confirmed again: %q", p)
	}
	// A dead sidecar: the account cannot send.
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 40)
	if n, p := next(); n != "" || p != "Signal service is not connected" {
		t.Fatalf("stale heartbeat: %q %q", n, p)
	}
}

func TestSignalSenderCachesForThreeSeconds(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=1, confirmed_account=$1 WHERE id`, sigTestNumber)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	clock := time.Unix(1_700_000_000, 0)
	c := newSignalSender(g.pg)
	c.now = func() time.Time { return clock }
	if _, p := c.sender(context.Background()); p != "" {
		t.Fatalf("first answer: %q", p)
	}
	g.sidecar(db.SignalStateUnlinked, 1, "", 0)
	clock = clock.Add(2 * time.Second)
	if _, p := c.sender(context.Background()); p != "" {
		t.Fatalf("within the cache time the answer must not change, got %q", p)
	}
	clock = clock.Add(2 * time.Second)
	if _, p := c.sender(context.Background()); p == "" {
		t.Fatal("after the cache time the answer must follow the rows")
	}
}

func TestSignalSenderDoesNotCacheAnAbandonedRead(t *testing.T) {
	g := newSignalRig(t)
	g.exec(`UPDATE signal_sidecar_config SET gen=1, confirmed_account=$1 WHERE id`, sigTestNumber)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	c := newSignalSender(g.pg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, p := c.sender(ctx); n != "" || p != "Signal state cannot be read: database unavailable" {
		t.Fatalf("cancelled read: %q %q", n, p)
	}
	if !strings.Contains(g.logs.String(), "[signal] sender check failed") {
		t.Fatalf("a failed read must be logged: %q", g.logs.String())
	}
	if n, p := c.sender(context.Background()); n != sigTestNumber || p != "" {
		t.Fatalf("the abandoned read was cached: %q %q", n, p)
	}
}

// ---- start ----

// Start-up installs the sender hook and provisions the role; a role that cannot be provisioned is a state
// on the status route, never a failed start.
func TestStartSignalNeverFailsAndShowsTheRoleState(t *testing.T) {
	g := newSignalRig(t)
	t.Cleanup(func() { notify.SetSignalSender(nil) })
	st := StartSignal(context.Background(), g.pg, db.SignalRoleConfig{Role: "Not A Valid Role", DSN: testDSN()})
	if st.State != db.SignalRoleError {
		t.Fatalf("state = %q, want error", st.State)
	}
	m := decodeSignalMap(t, g.get("/api/notify/signal/account"))
	role := m["db_role"].(map[string]any)
	if role["state"] != "error" || role["detail"] == "" {
		t.Fatalf("db_role = %v", role)
	}
	// The hook is installed: the notifier is told why it cannot send.
	ch, ok := notify.Get(notify.KindSignal)
	if !ok {
		t.Fatal("no signal channel kind")
	}
	cfg := map[string]any{"recipient_type": "group", "recipients": []any{sigTestGroup}}
	if err := ch.Validate(cfg); err == nil || !strings.Contains(err.Error(), "Signal service is not connected") {
		t.Fatalf("channel validation = %v, want the not connected text", err)
	}

	// A link request is refused for the role, before any command is written.
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 503, "db_role_not_ready")
	if g.config().gen != 0 {
		t.Fatal("a link was requested while the role was not ready")
	}

	if st := StartSignal(context.Background(), nil, db.SignalRoleConfig{}); st.State != db.SignalRoleUnchecked {
		t.Fatalf("no database: %q", st.State)
	}
	if !strings.Contains(g.logs.String(), "[signal] no database") {
		t.Fatalf("the missing database was not logged: %q", g.logs.String())
	}
}

// A service that is not connected and a state that cannot be read may clear by themselves, so a delivery
// that meets them is retried; the other problems need the operator.
func TestSignalSenderSaysWhichProblemsMayClear(t *testing.T) {
	g := newSignalRig(t)
	clock := time.Unix(1_700_000_000, 0)
	c := newSignalSender(g.pg)
	c.now = func() time.Time { return clock }
	next := func() (string, string, bool) {
		clock = clock.Add(signalSenderTTL + time.Second)
		return c.senderKind(context.Background())
	}
	want := func(what, problem string, retry bool) {
		t.Helper()
		if _, p, r := next(); p != problem || r != retry {
			t.Fatalf("%s: problem %q retry %v, want %q %v", what, p, r, problem, retry)
		}
	}
	want("no heartbeat", "Signal service is not connected", true)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	want("unlinked", "Signal account is not linked: link one under System, Notifications", false)
	g.sidecar(db.SignalStateError, 0, "", 0)
	want("error state", "Signal service is in an error state: unlink and link the account again", false)
	g.exec(`UPDATE signal_sidecar_config SET gen=1 WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	want("not confirmed", "confirm the linked Signal account first", false)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 40)
	want("stale heartbeat", "Signal service is not connected", true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, p, r := newSignalSender(g.pg).senderKind(ctx); p != "Signal state cannot be read: database unavailable" || !r {
		t.Fatalf("unreadable state: %q %v", p, r)
	}
}

// The notifier is not served an answer from before the account changed.
func TestSignalRoutesDropTheSenderAnswer(t *testing.T) {
	g := newSignalRig(t)
	c := newSignalSender(g.pg)
	signalSenderNow.Store(c)
	t.Cleanup(func() { signalSenderNow.Store(nil) })
	prime := func() {
		t.Helper()
		c.sender(context.Background())
		c.mu.Lock()
		ok := c.valid
		c.mu.Unlock()
		if !ok {
			t.Fatal("the answer was not cached")
		}
	}
	cached := func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.valid
	}

	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	prime()
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 202, "")
	if cached() {
		t.Error("a link request left the cached answer")
	}

	g.exec(`UPDATE signal_sidecar_config SET desired='idle' WHERE id`)
	g.sidecar(db.SignalStateLinked, 1, sigTestNumber, 0)
	prime()
	wantSignalCode(t, g.post("/api/notify/signal/account/confirm", map[string]any{"gen": 1, "last_digits": "550101"}), 200, "")
	if cached() {
		t.Error("a confirmation left the cached answer")
	}

	prime()
	wantSignalCode(t, g.post("/api/notify/signal/unlink", map[string]any{"password": sigTestPassword}), 202, "")
	if cached() {
		t.Error("an unlink request left the cached answer")
	}
}

// A password check that cannot read the stored hash says so in the log.
func TestSignalPasswordCheckLogsAFailedRead(t *testing.T) {
	g := newSignalRig(t)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	closed, err := db.Open(testDSN())
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	closed.Close()
	orig := g.s.m.pg
	g.s.m.pg = closed
	defer func() { g.s.m.pg = orig }()
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 503, "")
	if !strings.Contains(g.logs.String(), "[signal] password check") {
		t.Fatalf("the failed read was not logged: %q", g.logs.String())
	}
}

// A role state that came from a failed database call is read again after a minute, so a start-up accident does
// not stay on the status route; a refusal of another kind is not.
func TestSignalStatusReadsARoleFailureAgain(t *testing.T) {
	g := newSignalRig(t)
	clock := time.Unix(1_700_000_000, 0)
	oldNow := signalNow
	signalNow = func() time.Time { return clock }
	t.Cleanup(func() { signalNow = oldNow })
	roleDetail := func() string {
		t.Helper()
		return decodeSignalMap(t, g.get("/api/notify/signal/account"))["db_role"].(map[string]any)["detail"].(string)
	}

	const accident = "cannot read the role: connection or driver error (timeout)"
	setSignalRole(db.SignalRoleStatus{State: db.SignalRoleError, Detail: accident, Auth: "unchecked"})
	clock = clock.Add(signalRoleRecheck - time.Second)
	if got := roleDetail(); got != accident {
		t.Fatalf("before a minute the state is kept, got %q", got)
	}
	clock = clock.Add(2 * time.Second)
	if got := roleDetail(); got == accident {
		t.Fatalf("after a minute the role was not read again: %v", currentSignalRole())
	}

	const refusal = "the role has SUPERUSER"
	setSignalRole(db.SignalRoleStatus{State: db.SignalRoleError, Detail: refusal, Auth: "unchecked"})
	clock = clock.Add(10 * signalRoleRecheck)
	if got := roleDetail(); got != refusal {
		t.Fatalf("a refusal of another kind was read again, got %q", got)
	}
}

// The address of the link is a credential: fetching it leaves one audit line for each generation, and the line
// does not carry it.
func TestSignalLinkFetchIsAuditedOncePerGeneration(t *testing.T) {
	g := newSignalRig(t)
	signalLinkLogged.Store(0)
	g.sidecar(db.SignalStateUnlinked, 0, "", 0)
	wantSignalCode(t, g.post("/api/notify/signal/link", signalLinkBody()), 202, "")
	g.pendingLink(60)
	for i := 0; i < 3; i++ {
		wantSignalCode(t, g.get("/api/notify/signal/link"), 200, "")
	}
	logs := g.logs.String()
	if n := strings.Count(logs, "link address fetched by operator-7 (generation 1)"); n != 1 {
		t.Fatalf("audit lines = %d, want 1:\n%s", n, logs)
	}
	if strings.Contains(logs, "SENTINELURI") {
		t.Fatal("the log carries the link address")
	}
}

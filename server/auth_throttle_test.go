package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// useThrottle swaps the shared throttle for a fresh one on a fake clock.
func useThrottle(t *testing.T) *fakeClock {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	old, oldDev := authThrottle, deviceThrottle
	authThrottle = newLoginThrottle(clk.now)
	deviceThrottle = newLoginThrottle(clk.now)
	t.Cleanup(func() { authThrottle, deviceThrottle = old, oldDev })
	return clk
}

func TestThrottleLocksAfterFiveFailuresAndDoublesToTheCap(t *testing.T) {
	clk := useThrottle(t)
	th := authThrottle
	for i := 1; i <= 4; i++ {
		if d := th.fail("a"); d != 0 {
			t.Fatalf("failure %d started a lockout of %s", i, d)
		}
		if _, locked := th.check("a"); locked {
			t.Fatalf("locked after %d failures", i)
		}
	}
	if d := th.fail("a"); d != time.Second {
		t.Fatalf("5th failure lockout=%s want 1s", d)
	}
	if left, locked := th.check("a"); !locked || left != time.Second {
		t.Fatalf("check after 5th failure: left=%s locked=%v", left, locked)
	}
	if _, locked := th.check("b"); locked {
		t.Fatal("another source is locked")
	}
	// Doubling: each failure made after the lock lapsed doubles the delay.
	delay := time.Second
	for i := 0; i < 20; i++ {
		clk.advance(delay + time.Millisecond)
		if _, locked := th.check("a"); locked {
			t.Fatal("still locked after the delay")
		}
		got := th.fail("a")
		if delay < throttleMaxDelay {
			delay *= 2
		}
		if delay > throttleMaxDelay {
			delay = throttleMaxDelay
		}
		if got != delay {
			t.Fatalf("lockout after failure %d = %s want %s", 6+i, got, delay)
		}
	}
	if delay != 15*time.Minute {
		t.Fatalf("cap = %s want 15m", delay)
	}
}

func TestThrottleSuccessResetsAndIdleCountsAreForgotten(t *testing.T) {
	clk := useThrottle(t)
	th := authThrottle
	for i := 0; i < 5; i++ {
		th.fail("a")
	}
	th.reset("a")
	if _, locked := th.check("a"); locked {
		t.Fatal("still locked after a reset")
	}
	if d := th.fail("a"); d != 0 {
		t.Fatalf("first failure after a reset locked for %s", d)
	}
	// Four failures, then a long quiet period: the count starts again.
	for i := 0; i < 3; i++ {
		th.fail("a")
	}
	clk.advance(throttleForgetAfter + time.Second)
	if d := th.fail("a"); d != 0 {
		t.Fatalf("a stale count survived the idle window: lockout %s", d)
	}
}

func TestThrottleMemoryIsBounded(t *testing.T) {
	useThrottle(t)
	th := authThrottle
	for i := 0; i < throttleMaxSources+500; i++ {
		th.fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	th.mu.Lock()
	n, l := len(th.m), th.ll.Len()
	th.mu.Unlock()
	if n != throttleMaxSources || l != throttleMaxSources {
		t.Fatalf("sources kept: map=%d list=%d want %d", n, l, throttleMaxSources)
	}
}

func TestThrottleKeyIsTheRemoteIPOnly(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.7:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("X-Real-IP", "203.0.113.10")
	if got := throttleKey(r); got != "192.0.2.7" {
		t.Fatalf("key=%q", got)
	}
	r.RemoteAddr = "[2001:db8::1]:443"
	if got := throttleKey(r); got != "2001:db8::/64" {
		t.Fatalf("key=%q", got)
	}
}

// loginRig is a Server with a real database, an admin password, and a counting bcrypt check.
type loginRig struct {
	s     *Server
	calls *int
}

const rigPassword = "correct horse battery"

func newLoginRig(t *testing.T) *loginRig {
	t.Helper()
	m, err := NewManager(t.TempDir(), "", testDSN())
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte(rigPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(authPassKey, string(hash)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	old := loginCompare
	loginCompare = func(h, p []byte) error { calls++; return old(h, p) }
	t.Cleanup(func() { loginCompare = old })
	return &loginRig{s: &Server{m: m, jwtKey: []byte("0123456789abcdef0123456789abcdef")}, calls: &calls}
}

func (g *loginRig) login(src, user, pw string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
	r.RemoteAddr = src + ":40000"
	w := httptest.NewRecorder()
	g.s.authLogin(w, r)
	return w
}

func TestLoginThrottleSixFailuresGive429WithRetryAfter(t *testing.T) {
	clk := useThrottle(t)
	g := newLoginRig(t)
	for i := 1; i <= 5; i++ {
		if w := g.login("192.0.2.1", "ARTEX", "wrong-password-1"); w.Code != 401 {
			t.Fatalf("failure %d: status=%d want 401", i, w.Code)
		}
	}
	w := g.login("192.0.2.1", "ARTEX", "wrong-password-1")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("6th: status=%d retry-after=%q want 429/1; body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if w := g.login("192.0.2.2", "ARTEX", rigPassword); w.Code != 200 {
		t.Fatalf("another source: status=%d want 200", w.Code)
	}
	clk.advance(1100 * time.Millisecond)
	if w := g.login("192.0.2.1", "ARTEX", rigPassword); w.Code != 200 {
		t.Fatalf("after the window: status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	// A success resets the count: the next failure does not lock.
	if w := g.login("192.0.2.1", "ARTEX", "wrong-password-1"); w.Code != 401 {
		t.Fatalf("failure after a success: status=%d want 401", w.Code)
	}
}

func TestLoginThrottleRefusesTheCorrectPasswordDuringTheLockoutWithoutRevealingTheUser(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	for i := 0; i < 5; i++ {
		g.login("192.0.2.3", "ARTEX", "wrong-password-1")
	}
	good := g.login("192.0.2.3", "ARTEX", rigPassword)
	other := g.login("192.0.2.3", "nobody", "whatever-it-is")
	wrongPw := g.login("192.0.2.3", "ARTEX", "wrong-password-1")
	for name, w := range map[string]*httptest.ResponseRecorder{"correct": good, "unknown user": other, "wrong password": wrongPw} {
		if w.Code != 429 {
			t.Fatalf("%s during lockout: status=%d want 429", name, w.Code)
		}
		if strings.Contains(w.Body.String(), "token") {
			t.Fatalf("%s during lockout got a token", name)
		}
	}
	if good.Body.String() != other.Body.String() || good.Header().Get("Retry-After") != other.Header().Get("Retry-After") {
		t.Fatalf("the lockout reply depends on the request: %q/%q vs %q/%q",
			good.Body.String(), good.Header().Get("Retry-After"), other.Body.String(), other.Header().Get("Retry-After"))
	}
}

func TestLoginFailuresAlwaysTakeTheBcryptPath(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	g.login("192.0.2.4", "nobody", "wrong-password-1")
	g.login("192.0.2.4", "ARTEX", "wrong-password-1")
	g.login("192.0.2.4", "", "")
	if *g.calls != 3 {
		t.Fatalf("bcrypt ran %d times for 3 failed logins, want 3 (a wrong user name must cost the same)", *g.calls)
	}
	// Failures with a wrong user name count towards the lockout like any other.
	for i := 0; i < 2; i++ {
		g.login("192.0.2.4", "nobody", "x")
	}
	if w := g.login("192.0.2.4", "ARTEX", rigPassword); w.Code != 429 {
		t.Fatalf("5 mixed failures did not lock: status=%d", w.Code)
	}
}

func TestLoginThrottleLogsNothingFromTheRequest(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())
	for i := 0; i < 7; i++ {
		g.login("192.0.2.5", "secret-user-name", "secret-password-value")
	}
	out := buf.String()
	if strings.Contains(out, "secret-user-name") || strings.Contains(out, "secret-password-value") {
		t.Fatalf("the log carries request content: %s", out)
	}
	if n := strings.Count(out, "locked out"); n != 1 {
		t.Fatalf("lockout lines=%d want exactly 1 (one per lockout start, none per refused request): %s", n, out)
	}
}

func TestChangePasswordThrottlesWrongCurrentPassword(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	tok, err := signJWT(g.s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	change := func(old string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"old_password": old, "new_password": "another long password"})
		r := httptest.NewRequest("POST", "/api/auth/change-password", bytes.NewReader(b))
		r.RemoteAddr = "192.0.2.6:40000"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		g.s.authChangePassword(w, r)
		return w
	}
	for i := 0; i < 5; i++ {
		if w := change("wrong-password-1"); w.Code != 401 {
			t.Fatalf("failure %d: status=%d want 401", i+1, w.Code)
		}
	}
	if w := change(rigPassword); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("during lockout: status=%d retry-after=%q want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}
	// Without a valid token nothing is counted: a tool cannot lock the operator out this way.
	r := httptest.NewRequest("POST", "/api/auth/change-password", strings.NewReader(`{}`))
	r.RemoteAddr = "192.0.2.99:40000"
	w := httptest.NewRecorder()
	g.s.authChangePassword(w, r)
	if w.Code != 401 {
		t.Fatalf("no token: status=%d want 401", w.Code)
	}
	if _, locked := authThrottle.check("192.0.2.99"); locked {
		t.Fatal("requests without a token were counted")
	}
}

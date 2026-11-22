package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// slowCompare replaces the password check with one that always says "wrong", takes d, and counts
// how many calls reached it. Install it after newLoginRig (it restores the rig's check on cleanup).
func slowCompare(t *testing.T, d time.Duration) *int32 {
	t.Helper()
	var n int32
	old := loginCompare
	loginCompare = func(h, p []byte) error {
		atomic.AddInt32(&n, 1)
		time.Sleep(d)
		return bcrypt.ErrMismatchedHashAndPassword
	}
	t.Cleanup(func() { loginCompare = old })
	return &n
}

// burst runs every fn at once (released together) and returns the status codes and the
// Retry-After values, in order.
func burst(fns []func() *httptest.ResponseRecorder) (codes []int, retry []string) {
	codes, retry = make([]int, len(fns)), make([]string, len(fns))
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i, fn := range fns {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			w := fn()
			codes[i], retry[i] = w.Code, w.Header().Get("Retry-After")
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return codes, retry
}

func expectGate(t *testing.T, what string, codes []int, retry []string, reached int32) {
	t.Helper()
	n401, n429 := 0, 0
	for i, c := range codes {
		switch c {
		case 401:
			n401++
		case 429:
			n429++
			if retry[i] == "" {
				t.Fatalf("%s: a 429 without Retry-After", what)
			}
		default:
			t.Fatalf("%s: unexpected status %d", what, c)
		}
	}
	if reached > throttleFreeFailures || n401 > throttleFreeFailures || n401+n429 != len(codes) {
		t.Fatalf("%s: %d of %d parallel wrong passwords reached the check (%d got 401, %d got 429); at most %d may",
			what, reached, len(codes), n401, n429, throttleFreeFailures)
	}
	if reached != int32(n401) || n401 != throttleFreeFailures {
		t.Fatalf("%s: reached=%d 401=%d want %d each", what, reached, n401, throttleFreeFailures)
	}
}

func TestParallelWrongLoginsFromOneSourceReachTheCheckAtMostFiveTimes(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	reached := slowCompare(t, 300*time.Millisecond)
	var fns []func() *httptest.ResponseRecorder
	for i := 0; i < 50; i++ {
		fns = append(fns, func() *httptest.ResponseRecorder { return g.login("192.0.2.50", "ARTEX", "wrong-password-1") })
	}
	codes, retry := burst(fns)
	expectGate(t, "one source", codes, retry, atomic.LoadInt32(reached))
	if _, locked := authThrottle.check("192.0.2.50"); !locked {
		t.Fatal("the source is not locked after the burst")
	}
}

func TestParallelWrongLoginsFromLocalSourcesShareTheBudget(t *testing.T) {
	useThrottle(t)
	useLocalAddrs(t, time.Now, fixedAddrs())
	g := newLoginRig(t)
	reached := slowCompare(t, 300*time.Millisecond)
	var fns []func() *httptest.ResponseRecorder
	for i := 0; i < 50; i++ {
		src := fmt.Sprintf("127.0.0.%d", 2+i%40)
		fns = append(fns, func() *httptest.ResponseRecorder { return g.login(src, "ARTEX", "wrong-password-1") })
	}
	codes, retry := burst(fns)
	expectGate(t, "local sources", codes, retry, atomic.LoadInt32(reached))
}

func TestParallelWrongLoginsWithOneDeviceCookieReachTheCheckAtMostFiveTimes(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	cookie := deviceCookieOf(t, g.loginWith("192.0.2.60", "ARTEX", rigPassword, false))
	reached := slowCompare(t, 300*time.Millisecond)
	var fns []func() *httptest.ResponseRecorder
	for i := 0; i < 50; i++ {
		fns = append(fns, func() *httptest.ResponseRecorder {
			return g.loginWith("192.0.2.61", "ARTEX", "wrong-password-1", false, cookie)
		})
	}
	codes, retry := burst(fns)
	expectGate(t, "one device", codes, retry, atomic.LoadInt32(reached))
	authThrottle.mu.Lock()
	_, counted := authThrottle.m["192.0.2.61"]
	authThrottle.mu.Unlock()
	if counted {
		t.Fatal("a request with a valid device cookie touched the source counter")
	}
	// Without the cookie the same source is still free.
	if w := g.loginWith("192.0.2.61", "ARTEX", "wrong-password-1", false); w.Code != 401 {
		t.Fatalf("cookie-less request from the same source: status=%d want 401", w.Code)
	}
}

func TestParallelLoginAndChangePasswordShareOneBudget(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	tok, err := signJWT(g.s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	reached := slowCompare(t, 300*time.Millisecond)
	change := func() *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"old_password": "wrong-password-1", "new_password": "another long password"})
		r := httptest.NewRequest("POST", "/api/auth/change-password", bytes.NewReader(b))
		r.RemoteAddr = "192.0.2.70:40000"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		g.s.authChangePassword(w, r)
		return w
	}
	var fns []func() *httptest.ResponseRecorder
	for i := 0; i < 20; i++ {
		fns = append(fns, change, func() *httptest.ResponseRecorder { return g.login("192.0.2.70", "ARTEX", "wrong-password-1") })
	}
	codes, retry := burst(fns)
	expectGate(t, "login and change-password", codes, retry, atomic.LoadInt32(reached))
}

func TestRequestsWithoutAPasswordVerdictDoNotConsumeAFailure(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	for i := 0; i < 3*throttleFreeFailures; i++ {
		r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader([]byte("{not json")))
		r.RemoteAddr = "192.0.2.80:40000"
		w := httptest.NewRecorder()
		g.s.authLogin(w, r)
		if w.Code != 400 {
			t.Fatalf("bad JSON %d: status=%d want 400", i+1, w.Code)
		}
	}
	authThrottle.mu.Lock()
	n := len(authThrottle.m)
	authThrottle.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries left after requests that got no verdict", n)
	}
	for i := 1; i <= throttleFreeFailures; i++ {
		if w := g.login("192.0.2.80", "ARTEX", "wrong-password-1"); w.Code != 401 {
			t.Fatalf("wrong password %d after the malformed ones: status=%d want 401", i, w.Code)
		}
	}
	if w := g.login("192.0.2.80", "ARTEX", rigPassword); w.Code != 429 {
		t.Fatalf("after %d failures: status=%d want 429", throttleFreeFailures, w.Code)
	}
}

func TestLoginStatusAndRetryAfterOfALockedKeyAreUnchangedByTheReservation(t *testing.T) {
	clk := useThrottle(t)
	g := newLoginRig(t)
	for i := 0; i < throttleFreeFailures; i++ {
		g.login("192.0.2.90", "ARTEX", "wrong-password-1")
	}
	if w := g.login("192.0.2.90", "ARTEX", rigPassword); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("status=%d retry-after=%q want 429/1", w.Code, w.Header().Get("Retry-After"))
	}
	clk.advance(1100 * time.Millisecond)
	// One attempt is allowed after the lockout; it fails and doubles the delay.
	if w := g.login("192.0.2.90", "ARTEX", "wrong-password-1"); w.Code != 401 {
		t.Fatalf("first attempt after the lockout: status=%d want 401", w.Code)
	}
	if w := g.login("192.0.2.90", "ARTEX", rigPassword); w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("status=%d retry-after=%q want 429/2", w.Code, w.Header().Get("Retry-After"))
	}
}

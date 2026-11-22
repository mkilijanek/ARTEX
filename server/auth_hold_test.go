package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stalledLogins starts n login requests whose body never completes (an io.Pipe that is closed
// only by the returned function, which also runs on cleanup) and returns once they have had time
// to block in the body read. The function ends the requests and returns the codes they got.
func stalledLogins(t *testing.T, g *loginRig, n int, src string, cookies ...*http.Cookie) func() []int {
	t.Helper()
	codes := make([]int, n)
	var wg sync.WaitGroup
	var pipes []*io.PipeWriter
	for i := 0; i < n; i++ {
		pr, pw := io.Pipe()
		pipes = append(pipes, pw)
		r := httptest.NewRequest("POST", "/api/auth/login", pr)
		r.RemoteAddr = src + ":40000"
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.s.authLogin(w, r)
			codes[i] = w.Code
		}()
		go pw.Write([]byte("{")) // the start of a body, then nothing
	}
	var once sync.Once
	finish := func() []int {
		once.Do(func() {
			for _, pw := range pipes {
				pw.CloseWithError(io.ErrUnexpectedEOF)
			}
			wg.Wait()
		})
		return codes
	}
	t.Cleanup(func() { finish() })
	time.Sleep(300 * time.Millisecond) // the handlers are now blocked reading the body
	return finish
}

func inflightOf(th *loginThrottle, key string) int {
	th.mu.Lock()
	defer th.mu.Unlock()
	if el, ok := th.m[key]; ok {
		return el.Value.(*throttleEntry).inflight
	}
	return 0
}

func TestRequestsStalledInTheBodyHoldNoSlotOfTheSource(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	stalledLogins(t, g, throttleFreeFailures, "192.0.2.100")
	if n := inflightOf(authThrottle, "192.0.2.100"); n != 0 {
		t.Fatalf("%d slots held by requests that have not sent their body", n)
	}
	if w := g.login("192.0.2.100", "ARTEX", rigPassword); w.Code != 200 {
		t.Fatalf("a complete correct login while %d bodies stall: status=%d want 200", throttleFreeFailures, w.Code)
	}
}

func TestRequestsStalledInTheBodyHoldNoSlotOfTheDevice(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	cookie := deviceCookieOf(t, g.loginWith("192.0.2.101", "ARTEX", rigPassword, false))
	stalledLogins(t, g, throttleFreeFailures, "192.0.2.101", cookie)
	if w := g.loginWith("192.0.2.101", "ARTEX", rigPassword, false, cookie); w.Code != 200 {
		t.Fatalf("a complete correct login of the device while %d bodies stall: status=%d want 200", throttleFreeFailures, w.Code)
	}
}

func TestStalledRequestsEndWithoutAVerdictAndWithoutAFailure(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	finish := stalledLogins(t, g, throttleFreeFailures, "192.0.2.102")
	for i, c := range finish() {
		if c != 400 {
			t.Fatalf("stalled request %d ended with status %d want 400", i, c)
		}
	}
	if n := inflightOf(authThrottle, "192.0.2.102"); n != 0 {
		t.Fatalf("inflight=%d after the requests ended", n)
	}
	for i := 1; i <= throttleFreeFailures; i++ {
		if w := g.login("192.0.2.102", "ARTEX", "wrong-password-1"); w.Code != 401 {
			t.Fatalf("wrong password %d: status=%d want 401", i, w.Code)
		}
	}
}

func TestOversizedBodiesAreRefusedWithoutConsumingAFailure(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	reached := slowCompare(t, time.Millisecond)
	big := `{"username":"ARTEX","password":"` + strings.Repeat("a", 1<<20) + `"}`
	for i := 0; i < 2*throttleFreeFailures; i++ {
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(big))
		r.RemoteAddr = "192.0.2.103:40000"
		w := httptest.NewRecorder()
		g.s.authLogin(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized login %d: status=%d want 413", i+1, w.Code)
		}
	}
	tok, err := signJWT(g.s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"old_password": strings.Repeat("a", 1<<20), "new_password": "another long password"})
	r := httptest.NewRequest("POST", "/api/auth/change-password", bytes.NewReader(b))
	r.RemoteAddr = "192.0.2.103:40000"
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	g.s.authChangePassword(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized change-password: status=%d want 413", w.Code)
	}
	if n := *reached; n != 0 {
		t.Fatalf("the password check ran %d times for refused bodies", n)
	}
	for i := 1; i <= throttleFreeFailures; i++ {
		if w := g.login("192.0.2.103", "ARTEX", "wrong-password-1"); w.Code != 401 {
			t.Fatalf("wrong password %d after the oversized bodies: status=%d want 401", i, w.Code)
		}
	}
	if w := g.login("192.0.2.103", "ARTEX", "wrong-password-1"); w.Code != 429 {
		t.Fatalf("after %d failures: status=%d want 429", throttleFreeFailures, w.Code)
	}
}

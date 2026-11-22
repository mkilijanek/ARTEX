package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// loginWith is a login from src that presents the given cookies; tls marks the request as TLS.
var tlsState = tls.ConnectionState{}

func (g *loginRig) loginWith(src, user, pw string, tls bool, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
	r.RemoteAddr = src + ":40000"
	if tls {
		r.TLS = &tlsState
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	g.s.authLogin(w, r)
	return w
}

func deviceCookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == deviceCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in the reply; headers=%v", deviceCookieName, w.Header())
	return nil
}

func TestLoginSetsTheDeviceCookie(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	w := g.loginWith("192.0.2.10", "ARTEX", rigPassword, false)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	c := deviceCookieOf(t, w)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure {
		t.Fatalf("attributes: httponly=%v samesite=%v secure=%v", c.HttpOnly, c.SameSite, c.Secure)
	}
	if c.MaxAge != int(deviceCookieTTL/time.Second) || deviceCookieTTL != 90*24*time.Hour {
		t.Fatalf("max-age=%d ttl=%s want 90 days", c.MaxAge, deviceCookieTTL)
	}
	if c.Path != "/" {
		t.Fatalf("path=%q", c.Path)
	}
	// A second login gets a different id.
	if c2 := deviceCookieOf(t, g.loginWith("192.0.2.10", "ARTEX", rigPassword, false)); c2.Value == c.Value {
		t.Fatal("two logins got the same device cookie")
	}
	// Over TLS the cookie is Secure.
	if c3 := deviceCookieOf(t, g.loginWith("192.0.2.10", "ARTEX", rigPassword, true)); !c3.Secure {
		t.Fatal("cookie is not Secure on a TLS request")
	}
	// A failed login sets none.
	for _, c := range g.loginWith("192.0.2.10", "ARTEX", "wrong-password-1", false).Result().Cookies() {
		if c.Name == deviceCookieName {
			t.Fatal("a failed login set a device cookie")
		}
	}
}

func lockSource(g *loginRig, src string) {
	for i := 0; i < throttleFreeFailures; i++ {
		g.login(src, "ARTEX", "wrong-password-1")
	}
}

func TestLockedSourceRefusesCookielessButAcceptsAValidDeviceCookie(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	dev := deviceCookieOf(t, g.loginWith("192.0.2.11", "ARTEX", rigPassword, false))
	// A tool behind the same gateway address locks the source.
	lockSource(g, "192.0.2.11")
	if w := g.loginWith("192.0.2.11", "ARTEX", rigPassword, false); w.Code != 429 {
		t.Fatalf("cookie-less correct login on a locked source: status=%d want 429", w.Code)
	}
	w := g.loginWith("192.0.2.11", "ARTEX", rigPassword, false, dev)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "token") {
		t.Fatalf("correct login with the device cookie: status=%d body=%s", w.Code, w.Body.String())
	}
	// Wrong passwords with the cookie neither need nor extend the source lockout.
	if w := g.loginWith("192.0.2.11", "ARTEX", "wrong-password-1", false, deviceCookieOf(t, w)); w.Code != 401 {
		t.Fatalf("wrong password with the cookie on a locked source: status=%d want 401", w.Code)
	}
}

func TestForgedDeviceCookiesAreIgnored(t *testing.T) {
	clk := useThrottle(t)
	g := newLoginRig(t)
	good := deviceCookieOf(t, g.loginWith("192.0.2.12", "ARTEX", rigPassword, false))
	parts := strings.Split(good.Value, ".")
	if len(parts) != 3 {
		t.Fatalf("cookie value shape: %q", good.Value)
	}
	forged := map[string]string{
		"random":       "00112233445566778899aabbccddeeff.9999999999.0000000000000000000000000000000000000000000000000000000000000000",
		"tampered id":  "ffffffffffffffffffffffffffffffff." + parts[1] + "." + parts[2],
		"extended exp": parts[0] + ".9999999999." + parts[2],
		"garbage":      "x",
		"empty parts":  "..",
		"missing mac":  parts[0] + "." + parts[1],
		"non-hex mac":  parts[0] + "." + parts[1] + ".zz",
	}
	lockSource(g, "192.0.2.12")
	for name, v := range forged {
		w := g.loginWith("192.0.2.12", "ARTEX", rigPassword, false, &http.Cookie{Name: deviceCookieName, Value: v})
		if w.Code != 429 {
			t.Fatalf("%s: status=%d want 429 (a forged cookie must not lift the source lockout)", name, w.Code)
		}
	}
	// A cookie signed with another key is forged too.
	other := &Server{jwtKey: []byte("another-key-another-key-another-key")}
	w := httptest.NewRecorder()
	other.setDeviceCookie(w, httptest.NewRequest("POST", "/", nil))
	if w := g.loginWith("192.0.2.12", "ARTEX", rigPassword, false, deviceCookieOf(t, w)); w.Code != 429 {
		t.Fatalf("cookie from another key: status=%d want 429", w.Code)
	}
	// An expired genuine cookie is refused as well.
	clk.advance(deviceCookieTTL + time.Hour)
	// The source lockout has lapsed by now, so lock it again.
	lockSource(g, "192.0.2.12")
	if w := g.loginWith("192.0.2.12", "ARTEX", rigPassword, false, good); w.Code != 429 {
		t.Fatalf("expired cookie: status=%d want 429", w.Code)
	}
}

func TestDeviceCounterLocksAfterItsOwnFailures(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	dev := deviceCookieOf(t, g.loginWith("192.0.2.13", "ARTEX", rigPassword, false))
	for i := 1; i <= throttleFreeFailures; i++ {
		if w := g.loginWith("192.0.2.13", "ARTEX", "wrong-password-1", false, dev); w.Code != 401 {
			t.Fatalf("failure %d: status=%d want 401", i, w.Code)
		}
	}
	w := g.loginWith("192.0.2.13", "ARTEX", rigPassword, false, dev)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("device after its failures: status=%d retry-after=%q want 429/1", w.Code, w.Header().Get("Retry-After"))
	}
	// The failures of the device did not lock the source: the same address without a cookie
	// still logs in, and so does another device cookie.
	if w := g.loginWith("192.0.2.13", "ARTEX", rigPassword, false); w.Code != 200 {
		t.Fatalf("source after device failures: status=%d want 200 (device failures must not count on the source)", w.Code)
	}
	if _, locked := authThrottle.check("192.0.2.13"); locked {
		t.Fatal("device failures locked the source")
	}
}

func TestLockoutLogNamesTheSourceCappedAndSanitised(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())
	lockSource(g, "192.0.2.14")
	if out := buf.String(); !strings.Contains(out, "192.0.2.14") || strings.Count(out, "locked out") != 1 {
		t.Fatalf("source lockout log: %q", out)
	}
	if got := logSafe(throttleKey(&http.Request{RemoteAddr: "evil\nforged line" + strings.Repeat("x", 200)})); strings.ContainsAny(got, "\n\r") || len(got) > 80 {
		t.Fatalf("source not sanitised/capped: %q", got)
	}
	buf.Reset()
	dev := deviceCookieOf(t, g.loginWith("192.0.2.15", "ARTEX", rigPassword, false))
	for i := 0; i < throttleFreeFailures; i++ {
		g.loginWith("192.0.2.15", "ARTEX", "wrong-password-1", false, dev)
	}
	out := buf.String()
	if !strings.Contains(out, "192.0.2.15") || !strings.Contains(out, "device") || strings.Contains(out, dev.Value) || strings.Contains(out, strings.Split(dev.Value, ".")[0]) {
		t.Fatalf("device lockout log: %q", out)
	}
}

func TestForwardedHeadersDoNotPickTheDeviceOrSource(t *testing.T) {
	useThrottle(t)
	g := newLoginRig(t)
	lockSource(g, "192.0.2.16")
	b, _ := json.Marshal(map[string]string{"username": "ARTEX", "password": rigPassword})
	r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
	r.RemoteAddr = "192.0.2.16:40000"
	r.Header.Set("X-Forwarded-For", "203.0.113.50")
	r.Header.Set("X-Device-Id", "anything")
	w := httptest.NewRecorder()
	g.s.authLogin(w, r)
	if w.Code != 429 {
		t.Fatalf("status=%d want 429", w.Code)
	}
}

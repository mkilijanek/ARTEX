package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func subjectTestToken(t *testing.T, key []byte, sub string, exp time.Duration) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   sub,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(exp)),
	}).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRequireAuthExposesVerifiedSubject(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	s := &Server{jwtKey: key}
	var got string
	called := 0
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		got = operatorSubject(r)
	}))

	good, err := signJWT(key)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Authorization", "Bearer "+good)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || called != 1 || got != "ARTEX" {
		t.Fatalf("code=%d called=%d subject=%q want 200/1/ARTEX", w.Code, called, got)
	}

	called = 0
	bad := subjectTestToken(t, []byte("another-key-another-key-another-key"), "ARTEX", time.Hour)
	r = httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Authorization", "Bearer "+bad)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || called != 0 {
		t.Fatalf("code=%d called=%d want 401/0", w.Code, called)
	}

	if operatorSubject(httptest.NewRequest("GET", "/", nil)) != "" {
		t.Fatal("a request that did not pass requireAuth must have no subject")
	}

	// A valid token with an empty subject yields no identity.
	called = 0
	got = "unset"
	r = httptest.NewRequest("GET", "/api/x", nil)
	r.Header.Set("Authorization", "Bearer "+subjectTestToken(t, key, "", time.Hour))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "" {
		t.Fatalf("empty-subject token produced identity %q", got)
	}
}

func TestVerifyJWTSubjectRejects(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{Subject: "ARTEX"}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"expired": subjectTestToken(t, key, "ARTEX", -time.Hour),
		"none":    none,
		"garbage": "not.a.jwt",
		"empty":   "",
	}
	for name, tok := range cases {
		sub, ok := verifyJWTSubject(tok, key)
		if ok || sub != "" {
			t.Errorf("%s: sub=%q ok=%v want empty/false", name, sub, ok)
		}
		if verifyJWT(tok, key) {
			t.Errorf("%s: verifyJWT accepted", name)
		}
	}
	good := subjectTestToken(t, key, "ARTEX", time.Hour)
	if sub, ok := verifyJWTSubject(good, key); !ok || sub != "ARTEX" {
		t.Errorf("good: sub=%q ok=%v", sub, ok)
	}
	if !verifyJWT(good, key) {
		t.Error("verifyJWT rejected a good token")
	}
}

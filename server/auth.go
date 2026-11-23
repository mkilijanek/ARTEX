package server

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	jwtKeyFilename = "jwt.key"
	authPassKey    = "auth.password_hash"
	jwtTTL         = 7 * 24 * time.Hour
	keyChars       = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	// The lower bound matches the setup page's frontend validation — validation that
	// lives only in the frontend is as good as none: hitting the API directly bypasses
	// it. The upper bound is bcrypt's hard limit: past 72 bytes GenerateFromPassword returns ErrPasswordTooLong; rejecting early beats a cryptic "password encryption failed".
	minPasswordRunes = 8
	maxPasswordBytes = 72
)

// errDataSourceUnavailable is the uniform reply when a password-related read fails.
// These handlers must never mistake a failed read for "not set": authInit once let an
// unauthenticated request through on a database error, overwriting the admin password.
const errDataSourceUnavailable = "Data source temporarily unavailable, please retry later"

// validatePassword returns an empty string when passing, otherwise a reason ready to show the user.
func validatePassword(pw string) string {
	if utf8.RuneCountInString(pw) < minPasswordRunes {
		return fmt.Sprintf("Password must be at least %d characters", minPasswordRunes)
	}
	if len(pw) > maxPasswordBytes {
		return fmt.Sprintf("Password must not exceed %d bytes", maxPasswordBytes)
	}
	return ""
}

// loadOrCreateJWTKey reads the 32-byte signing key from keyDir/jwt.key. keyDir is
// the project base dir (next to the executable), NOT the browsable workspace root
// (dataDir) — the signing key must never be listable/downloadable via the file
// manager. On first run a random key is generated and persisted.
//
// A file named jwt.key in dataDir is never adopted: older versions kept the key there, and the
// data directory is shared with agent tools, so the key is kept only next to the executable.
// It is renamed to jwt.key.ignored-<unix> (mode 0600), without following a link, and a neutral
// warning is logged.
func loadOrCreateJWTKey(keyDir, dataDir string) ([]byte, error) {
	path := filepath.Join(keyDir, jwtKeyFilename)
	setAsideLegacyKey(dataDir, path)
	if data, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return []byte(strings.TrimSpace(string(data))), nil
	}
	buf := make([]byte, 32)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(keyChars))))
		if err != nil {
			return nil, fmt.Errorf("generate jwt key: %w", err)
		}
		buf[i] = keyChars[n.Int64()]
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		return nil, fmt.Errorf("write jwt key: %w", err)
	}
	log.Printf("[auth] new JWT key written to %s", path)
	return buf, nil
}

// Login throttling (plan 4.9). The key is the IP of RemoteAddr; forwarded headers are never
// used, because a tool process behind the same proxy could write them. Every source that is local
// to the server (loopback, and any address of a local interface) is ONE key, and an IPv6 source
// is its /64: a tool process needs no capability to bind() to another address of 127.0.0.0/8
// before it connects, so a per-address counter alone would never see it twice. Without
// CAP_NET_RAW a tool cannot spoof a source, but it can choose among the local ones. There is no global
// lockout: one source never locks out another. The state is in memory and bounded.
//
// A password check is reserved before it runs (acquire) and settled after (fail, reset, release): a
// key with k free failures left admits at most k unsettled checks, so a burst of parallel requests
// gets no more guesses than the same requests made one after another.
//
// Behind the Docker gateway or a reverse proxy every client shares one source address, so one
// client's failures would also delay the operator. A browser that logged in before carries a
// signed device cookie; a request with a valid one is throttled only by its own per-device counter (same
// doubling rule) and never by the source lockout. A missing or forged cookie falls back to the
// source throttle.
const (
	throttleFreeFailures = 5                    // failures a source may make before it is locked out
	throttleBaseDelay    = time.Second          // lockout after the 5th failure; doubles with each further failure
	throttleMaxDelay     = 15 * time.Minute     // upper bound of the lockout
	throttleForgetAfter  = 2 * throttleMaxDelay // idle time after which a count is forgotten (longer than any lockout)
	throttleMaxSources   = 10000                // LRU bound on the sources remembered
)

type throttleEntry struct {
	key         string
	fails       int
	lastFail    time.Time
	lockedUntil time.Time
	inflight    int // password checks admitted by acquire and not yet settled
}

type loginThrottle struct {
	mu  sync.Mutex
	now func() time.Time
	ll  *list.List // front = most recently failed
	m   map[string]*list.Element
}

func newLoginThrottle(now func() time.Time) *loginThrottle {
	return &loginThrottle{now: now, ll: list.New(), m: map[string]*list.Element{}}
}

// authThrottle is shared by login and change-password: both verify the same password.
var authThrottle = newLoginThrottle(time.Now)

// deviceThrottle counts the failures of requests that present a valid device cookie, per device.
var deviceThrottle = newLoginThrottle(time.Now)

// acquire admits one password check for key, or refuses it with the time to wait. The decision
// and the reservation are one step under the mutex, so N parallel requests cannot all see the
// same count: a key with k free failures left (k = throttleFreeFailures - fails, at least 1,
// because after a lockout the next attempt is allowed) admits at most k checks that are not yet
// settled. Every admitted check must end in fail, reset or release.
func (t *loginThrottle) acquire(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	el, ok := t.m[key]
	if !ok {
		e := &throttleEntry{key: key, inflight: 1}
		t.m[key] = t.ll.PushFront(e)
		t.trim()
		return 0, true
	}
	e := el.Value.(*throttleEntry)
	if left := e.lockedUntil.Sub(now); left > 0 {
		return left, false
	}
	if e.fails > 0 && now.Sub(e.lastFail) > throttleForgetAfter {
		e.fails = 0 // same forgetting rule as fail
	}
	free := throttleFreeFailures - e.fails
	if free < 1 {
		free = 1
	}
	if e.inflight >= free {
		return throttleBaseDelay, false // the checks in flight may start a lockout; try again shortly
	}
	e.inflight++
	return 0, true
}

// release gives back a reservation that ended without a password verdict (a malformed request,
// a database error): it counts as neither a failure nor a success.
func (t *loginThrottle) release(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.m[key]; ok {
		e := el.Value.(*throttleEntry)
		if e.inflight > 0 {
			e.inflight--
		}
		if e.inflight == 0 && e.fails == 0 {
			t.ll.Remove(el)
			delete(t.m, key)
		}
	}
}

// trim drops the least recently used entries beyond the bound, except those with a check in
// flight: their settlement must find the entry it reserved.
func (t *loginThrottle) trim() {
	for el := t.ll.Back(); el != nil && t.ll.Len() > throttleMaxSources; {
		prev := el.Prev()
		if e := el.Value.(*throttleEntry); e.inflight == 0 {
			t.ll.Remove(el)
			delete(t.m, e.key)
		}
		el = prev
	}
}

// check reports whether key is locked out and for how long. It reserves nothing.
func (t *loginThrottle) check(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.m[key]
	if !ok {
		return 0, false
	}
	if left := el.Value.(*throttleEntry).lockedUntil.Sub(t.now()); left > 0 {
		return left, true
	}
	return 0, false
}

// fail records one failed attempt (settling the reservation made by acquire, if any) and returns the lockout it started (0 while the source is
// still within its free failures). A count older than throttleForgetAfter with no active
// lockout is forgotten first.
func (t *loginThrottle) fail(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var e *throttleEntry
	if el, ok := t.m[key]; ok {
		e = el.Value.(*throttleEntry)
		t.ll.MoveToFront(el)
		if now.After(e.lockedUntil) && now.Sub(e.lastFail) > throttleForgetAfter {
			e.fails = 0
		}
	} else {
		e = &throttleEntry{key: key}
		t.m[key] = t.ll.PushFront(e)
		t.trim()
	}
	if e.inflight > 0 {
		e.inflight--
	}
	e.fails++
	e.lastFail = now
	if e.fails < throttleFreeFailures {
		return 0
	}
	shift := e.fails - throttleFreeFailures
	if shift > 20 {
		shift = 20
	}
	d := throttleBaseDelay << shift
	if d > throttleMaxDelay {
		d = throttleMaxDelay
	}
	e.lockedUntil = now.Add(d)
	return d
}

// reset forgets key after a successful login (and settles the reservation made by acquire). An
// entry that still has other checks in flight is kept, with a clean count, so they settle on it.
func (t *loginThrottle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.m[key]; ok {
		e := el.Value.(*throttleEntry)
		if e.inflight > 0 {
			e.inflight--
		}
		if e.inflight > 0 {
			e.fails, e.lockedUntil = 0, time.Time{}
			return
		}
		t.ll.Remove(el)
		delete(t.m, key)
	}
}

// throttleKey is the source of a request: the IP of RemoteAddr, nothing else, normalised by
// sourceKey (local sources are one key, IPv6 sources their /64).
func throttleKey(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return sourceKey(host)
	}
	return sourceKey(r.RemoteAddr)
}

const (
	deviceCookieName = "artex_device"
	deviceCookieTTL  = 90 * 24 * time.Hour
)

// deviceMAC signs "<id>.<expiry>" with a key derived from the session key, so a device cookie
// cannot be replayed as a session token or the other way round.
func deviceMAC(jwtKey []byte, payload string) string {
	dk := hmac.New(sha256.New, jwtKey)
	dk.Write([]byte("artex-device-cookie-v1"))
	m := hmac.New(sha256.New, dk.Sum(nil))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// setDeviceCookie issues a fresh device cookie (random 128-bit id, signed, 90 days).
func (s *Server) setDeviceCookie(w http.ResponseWriter, r *http.Request) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		log.Printf("[auth] could not generate a device id: %v", err)
		return
	}
	exp := deviceThrottle.now().Add(deviceCookieTTL)
	payload := hex.EncodeToString(id) + "." + strconv.FormatInt(exp.Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name:     deviceCookieName,
		Value:    payload + "." + deviceMAC(s.jwtKey, payload),
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(deviceCookieTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
}

// deviceID returns the id of a valid, unexpired device cookie of r.
func (s *Server) deviceID(r *http.Request) (string, bool) {
	c, err := r.Cookie(deviceCookieName)
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 || len(parts[0]) != 32 {
		return "", false
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(deviceMAC(s.jwtKey, payload))) {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !deviceThrottle.now().Before(time.Unix(exp, 0)) {
		return "", false
	}
	return parts[0], true
}

// throttleSubject says which counter applies to a request: its device's when it presents a valid
// device cookie, otherwise its source's.
type throttleSubject struct {
	th     *loginThrottle
	key    string
	device bool
	source string // the capped, sanitised source address, for log lines
	held   bool   // a reservation made by admit is still to be settled
}

func (s *Server) throttleSubjectOf(r *http.Request) throttleSubject {
	src := logSafe(throttleKey(r))
	if id, ok := s.deviceID(r); ok {
		return throttleSubject{th: deviceThrottle, key: id, device: true, source: src}
	}
	return throttleSubject{th: authThrottle, key: throttleKey(r), source: src}
}

// admit is the cheap, non-reserving gate that runs first: when the subject of r is locked out it
// answers 429 with Retry-After and ok is false, before the body is read or any password work
// is done. It reserves nothing, so a request that stalls while sending its body cannot hold a slot.
// A locked subject gets the same reply whatever it sends: a correct password is refused too, and
// the reply does not depend on the user name.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) (sub *throttleSubject, ok bool) {
	sub = new(throttleSubject)
	*sub = s.throttleSubjectOf(r)
	if left, locked := sub.th.check(sub.key); locked {
		refuseThrottled(w, left)
		return nil, false
	}
	return sub, true
}

func refuseThrottled(w http.ResponseWriter, left time.Duration) {
	secs := int((left + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErr(w, http.StatusTooManyRequests, "Too many failed attempts, retry later")
}

// reserve takes one password check for the subject, atomically with the lockout test. It is
// called immediately before the password comparison, when everything that can fail without a
// verdict (body, validation, database) is behind the request. A key with k free failures left
// admits at most k such checks that are not yet settled; the others get the 429 of a locked key
// and ok is false. A reservation ends with throttleFailure, throttleSuccess or release (defer
// release is safe: it does nothing once the check is settled).
func (sub *throttleSubject) reserve(w http.ResponseWriter) (ok bool) {
	left, admitted := sub.th.acquire(sub.key)
	if !admitted {
		refuseThrottled(w, left)
		return false
	}
	sub.held = true
	return true
}

const authBodyLimit = 64 << 10 // login and change-password bodies are a few hundred bytes

// authBodyDeadline bounds the time a client may take to send the body of a login request.
var authBodyDeadline = 10 * time.Second

// decodeAuthBody reads the small JSON body of a login request: at most authBodyLimit bytes and
// at most authBodyDeadline. A reply has been written when it returns false. (The deadline is
// per request; the server re-arms the connection's own deadlines for the next one.)
func decodeAuthBody(w http.ResponseWriter, r *http.Request, v any) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(authBodyDeadline)) // not supported by every ResponseWriter
	r.Body = http.MaxBytesReader(w, r.Body, authBodyLimit)
	if err := decode(r, v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "Request body too large")
		} else {
			writeErr(w, 400, "Invalid request format")
		}
		return false
	}
	return true
}

// release ends a reservation that got no password verdict; it counts as neither a failure nor a
// success. It is a no-op once the check is settled.
func (sub *throttleSubject) release() {
	if sub != nil && sub.held {
		sub.held = false
		sub.th.release(sub.key)
	}
}

// throttleFailure counts one failed password check and settles its reservation. The log line
// carries the source address (capped, sanitised) but no user name, no password, no device id and
// no other request content; it is written once, when a lockout starts.
func (sub *throttleSubject) throttleFailure() {
	sub.held = false
	d := sub.th.fail(sub.key)
	if d == 0 {
		return
	}
	if sub.device {
		log.Printf("[auth] a device at source %s is locked out of password checks for %s after repeated failures", sub.source, d)
		return
	}
	log.Printf("[auth] source %s is locked out of password checks for %s after repeated failures", sub.source, d)
}

// throttleSuccess forgets the counter of the subject after a successful password check.
func (sub *throttleSubject) throttleSuccess() {
	sub.held = false
	sub.th.reset(sub.key)
}

// loginCompare is bcrypt's check; a variable so tests can count the calls.
var loginCompare = bcrypt.CompareHashAndPassword

// signJWT issues a 7-day HS256 token for user ARTEX.
func signJWT(key []byte) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   "ARTEX",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(jwtTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(key)
}

// verifyJWTSubject is the single validation path: it returns the subject of a
// valid, non-expired HS256 token. The subject may be empty for a token that
// carries none; callers that need an identity must check it.
func verifyJWTSubject(tokenStr string, key []byte) (string, bool) {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return key, nil
	})
	if err != nil || !t.Valid {
		return "", false
	}
	sub, err := t.Claims.GetSubject()
	if err != nil {
		return "", false
	}
	return sub, true
}

// verifyJWT returns true when tokenStr is a valid, non-expired HS256 token.
func verifyJWT(tokenStr string, key []byte) bool {
	_, ok := verifyJWTSubject(tokenStr, key)
	return ok
}

type ctxKey int

const operatorSubjectKey ctxKey = iota

// operatorSubject returns the verified JWT subject stored by requireAuth, or ""
// when the request did not pass through it or the token carried no subject.
func operatorSubject(r *http.Request) string {
	sub, _ := r.Context().Value(operatorSubjectKey).(string)
	return sub
}

// extractToken reads the JWT from Authorization: Bearer header,
// artex_token cookie, or ?token= query param (for SSE connections).
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("artex_token"); err == nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// requireAuth wraps h with JWT validation.
// /api/auth/* and /api/health are exempt.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/auth/") || p == "/api/health" {
			h.ServeHTTP(w, r)
			return
		}
		tok := extractToken(r)
		if tok == "" {
			writeErr(w, 401, "Unauthorized")
			return
		}
		sub, ok := verifyJWTSubject(tok, s.jwtKey)
		if !ok {
			writeErr(w, 401, "token is invalid or expired")
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operatorSubjectKey, sub)))
	})
}

// GET /api/auth/status — reports whether the admin password has been initialised.
// A read failure must answer 503, not initialized:false: on initialized:false the
// frontend ships the user to /setup to set a password (login/page.tsx); wrapping a
// database outage in a 200 pushes the user toward overwriting the existing password.
func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	hash, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	writeJSON(w, 200, map[string]any{"initialized": hash != ""})
}

// initSetupToken generates the one-time setup token when no admin password is set
// and writes it to the server log. Without it, anyone who can reach the port
// before the operator could claim the instance through POST /api/auth/init.
func (s *Server) initSetupToken() {
	if s.m == nil || s.m.pg == nil {
		return
	}
	if hash, _, err := s.m.pg.GetSetting(authPassKey); err == nil && hash != "" {
		return
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("[auth] generate setup token: %v", err)
	}
	s.setupToken = hex.EncodeToString(buf)
	log.Printf("[auth] no admin password is set. Setup token for the /setup page: %s", s.setupToken)
}

// checkSetupToken compares got with the active setup token in constant time.
// It fails when no token is active.
func (s *Server) checkSetupToken(got string) bool {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	return s.setupToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.setupToken)) == 1
}

// POST /api/auth/init — sets the password for the first time; rejected if already set.
// Requires the setup token from the server log.
func (s *Server) authInit(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	existing, _, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if existing != "" {
		writeErr(w, 403, "Password is already set")
		return
	}
	var req struct {
		Password   string `json:"password"`
		SetupToken string `json:"setup_token"`
	}
	if err := decode(r, &req); err != nil || req.Password == "" {
		writeErr(w, 400, "Password cannot be empty")
		return
	}
	// 403, not 401: the frontend treats any 401 as an expired session and redirects to /login.
	if !s.checkSetupToken(req.SetupToken) {
		writeErr(w, 403, "Invalid setup token. The server log prints it at startup while no password is set")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "Password encryption failed")
		return
	}
	// INSERT ... ON CONFLICT DO NOTHING instead of an upsert: the GetSetting above is
	// only a fast-fail path; the real "first-set-only" guarantee rests on the primary
	// key. bcrypt runs tens of milliseconds, another request may set the password meanwhile, and the read check itself can fail under an outage.
	inserted, err := pg.InsertSettingIfAbsent(authPassKey, string(hash))
	if err != nil {
		writeErr(w, 500, "Save failed: "+err.Error())
		return
	}
	if !inserted {
		writeErr(w, 403, "Password is already set")
		return
	}
	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "token generation failed")
		return
	}
	writeJSON(w, 200, map[string]any{"token": tok})
}

// POST /api/auth/change-password — changes the admin password. Requires a valid
// token (this route is under /api/auth/* which requireAuth exempts, so the token
// is validated here) AND the current password.
func (s *Server) authChangePassword(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	if !verifyJWT(extractToken(r), s.jwtKey) {
		writeErr(w, 401, "Unauthorized")
		return
	}
	sub, ok := s.admit(w, r)
	if !ok {
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if req.NewPassword == "" {
		writeErr(w, 400, "New password cannot be empty")
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "Password not initialized; set a password first")
		return
	}
	if !sub.reserve(w) {
		return
	}
	defer sub.release()
	if err := loginCompare([]byte(hash), []byte(req.OldPassword)); err != nil {
		sub.throttleFailure()
		writeErr(w, 401, "Current password is wrong")
		return
	}
	sub.throttleSuccess()
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, 500, "Password encryption failed")
		return
	}
	if err := pg.SetSetting(authPassKey, string(newHash)); err != nil {
		writeErr(w, 500, "Save failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/auth/login — validates username/password and returns a JWT.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	sub, ok := s.admit(w, r)
	if !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeAuthBody(w, r, &req) {
		return
	}
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		writeErr(w, 503, errDataSourceUnavailable)
		return
	}
	if !ok || hash == "" {
		writeErr(w, 403, "Password not initialized; set a password first")
		return
	}
	// The hash is checked for a wrong user name too, so every failure takes the same time and
	// the reply does not tell whether the user exists.
	if !sub.reserve(w) {
		return
	}
	defer sub.release()
	pwErr := loginCompare([]byte(hash), []byte(req.Password))
	if req.Username != "ARTEX" || pwErr != nil {
		sub.throttleFailure()
		writeErr(w, 401, "Incorrect username or password")
		return
	}
	sub.throttleSuccess()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		writeErr(w, 500, "token generation failed")
		return
	}
	s.setDeviceCookie(w, r)
	writeJSON(w, 200, map[string]any{"token": tok})
}

// setAsideLegacyKey renames dataDir/jwt.key out of the way. The rename moves the directory entry
// (a link is moved as a link); a regular file is then made owner-only. A link is left as it is.
func setAsideLegacyKey(dataDir, keyPath string) {
	legacy := filepath.Join(dataDir, jwtKeyFilename)
	if legacy == keyPath {
		return
	}
	if _, err := os.Lstat(legacy); err != nil {
		return
	}
	aside := fmt.Sprintf("%s.ignored-%d", legacy, time.Now().Unix())
	if err := os.Rename(legacy, aside); err != nil {
		log.Printf("[auth] could not set aside a legacy key file in the data directory: %v", err)
		return
	}
	if fi, err := os.Lstat(aside); err == nil && fi.Mode().IsRegular() {
		if err := os.Chmod(aside, 0o600); err != nil {
			log.Printf("[auth] could not restrict the set-aside legacy key file: %v", err)
		}
	}
	log.Printf("[auth] ignored a legacy key file in the data directory")
}

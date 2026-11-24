package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/notify"
)

// The Signal account routes. The account is linked from the UI to a separate sidecar service; the server and
// the sidecar share two database rows (db/signal_sidecar.go). Every route here
//   - takes the session token from the Authorization header only (a cookie or a query parameter is refused,
//     so a request forged by another site cannot carry it),
//   - needs a token with a subject, because the audit lines name the operator,
//   - wants a JSON content type on every request that has a body,
//   - and answers with Cache-Control: no-store.
//
// The link address is a credential for the account: it is served by one route to the authenticated operator,
// never logged and never part of an error text.

const (
	// signalSenderTTL is how long the sender hook keeps its answer; the notifier asks once per delivery.
	signalSenderTTL = 3 * time.Second
	// signalReadTimeout bounds the database read made by the sender hook.
	signalReadTimeout = 3 * time.Second
	// signalProvisionTimeout bounds role provisioning at start; provisioning never fails the start.
	signalProvisionTimeout = 60 * time.Second
	// signalDefaultDevice is the device name shown on the phone when the request names none.
	signalDefaultDevice = "ARTEX"

	signalNotLinkedText     = "Signal account is not linked: link one under System, Notifications"
	signalNotConfirmedText  = "confirm the linked Signal account first"
	signalNotConnectedText  = "Signal service is not connected"
	signalDBUnavailableText = "Signal state cannot be read: database unavailable"
	signalErrorStateText    = "Signal service is in an error state: unlink and link the account again"

	// signalRoleRecheck is how long the role state of the status route is kept before it is read again.
	signalRoleRecheck = 60 * time.Second
)

// Reasons the Signal routes give in the "code" member of an error body.
const (
	signalCodePasswordIncorrect = "password_incorrect"
	signalCodeNumberMismatch    = "number_mismatch"
	signalCodeLinkPending       = "link_pending"
	signalCodeAlreadyLinked     = "already_linked"
	signalCodeUnlinkFirst       = "unlink_first"
	signalCodeNotReady          = "not_ready"
	signalCodeUnlinkPending     = "unlink_pending"
	signalCodeNothingToUnlink   = "nothing_to_unlink"
	signalCodeNoLinkPending     = "no_link_pending"
	signalCodeNotLinked         = "not_linked"
	signalCodeStale             = "stale"
	signalCodeConflict          = "conflict"
	signalCodeSidecarDown       = "sidecar_unavailable"
	signalCodeRoleNotReady      = "db_role_not_ready"
	signalCodeBusy              = "busy"
	signalCodeNotConfigured     = "not_configured"
)

// ---- process-wide state: the provisioning result ----

// signalRole is the outcome of ProvisionSignalRole at start. The status route reports it and the link route
// refuses to start a link unless it is "ok".
var signalRole atomic.Pointer[db.SignalRoleStatus]

func setSignalRole(st db.SignalRoleStatus) {
	signalRole.Store(&st)
	signalRoleAt.Store(signalNow().UnixNano())
}

// signalRoleAt is when the role state was set or read last (Unix nanoseconds), signalRoleName the role that was
// provisioned, and signalNow the clock of the re-check.
var (
	signalRoleAt   atomic.Int64
	signalRoleName atomic.Pointer[string]
	signalNow      = time.Now
)

// signalRoleCurrent is the role state for the status route. A failure that came from the database (its text
// starts with "cannot") may be a start-up accident, so after signalRoleRecheck the role is read again, changing
// nothing, and the result replaces the state. Other refusals (a foreign role, authentication rules) are not
// re-read: reading the role cannot tell whether they went away.
func signalRoleCurrent(ctx context.Context, pg *db.DB) db.SignalRoleStatus {
	st := currentSignalRole()
	if st.State != db.SignalRoleError || !strings.HasPrefix(st.Detail, "cannot ") {
		return st
	}
	if signalNow().Sub(time.Unix(0, signalRoleAt.Load())) < signalRoleRecheck {
		return st
	}
	name := ""
	if p := signalRoleName.Load(); p != nil {
		name = *p
	}
	rctx, cancel := context.WithTimeout(ctx, signalReadTimeout)
	defer cancel()
	now := pg.SignalRoleState(rctx, name)
	setSignalRole(now)
	return now
}

func currentSignalRole() db.SignalRoleStatus {
	if st := signalRole.Load(); st != nil {
		return *st
	}
	return db.SignalRoleStatus{State: db.SignalRoleUnchecked, Auth: "unchecked"}
}

// StartSignal wires the Signal account into the process: it installs the sender hook that the notifier asks
// for the linked account, and provisions the database role of the sidecar. Provisioning reports a state and
// never fails the start; the state is shown on the status route. A nil database installs nothing.
func StartSignal(ctx context.Context, pg *db.DB, cfg db.SignalRoleConfig) db.SignalRoleStatus {
	if pg == nil {
		log.Printf("[signal] no database: the Signal account is not available")
		return db.SignalRoleStatus{State: db.SignalRoleUnchecked, Detail: "no database", Auth: "unchecked"}
	}
	sc := newSignalSender(pg)
	signalSenderNow.Store(sc)
	notify.SetSignalSenderKind(sc.senderKind)
	signalRoleName.Store(&cfg.Role)
	pctx, cancel := context.WithTimeout(ctx, signalProvisionTimeout)
	defer cancel()
	st := pg.ProvisionSignalRole(pctx, cfg)
	setSignalRole(st)
	return st
}

// ---- the sender hook ----

// signalSenderCache answers the notifier's question "which account may send now" from the database, with a
// short cache so a burst of deliveries costs one read.
type signalSenderCache struct {
	pg  *db.DB
	now func() time.Time

	mu      sync.Mutex
	at      time.Time
	valid   bool
	number  string
	problem string
	retry   bool
}

// signalSenderNow is the cache the notifier asks, so a route that changes the account can drop its answer.
var signalSenderNow atomic.Pointer[signalSenderCache]

// resetSignalSender drops the cached answer: the next delivery reads the rows.
func resetSignalSender() {
	if c := signalSenderNow.Load(); c != nil {
		c.mu.Lock()
		c.valid = false
		c.mu.Unlock()
	}
}

func newSignalSender(pg *db.DB) *signalSenderCache {
	return &signalSenderCache{pg: pg, now: time.Now}
}

// sender is the hook for notify.SetSignalSender. The problem texts are the ones the notifier shows.
func (c *signalSenderCache) sender(ctx context.Context) (number, problem string) {
	number, problem, _ = c.senderKind(ctx)
	return number, problem
}

// senderKind is sender with a third result: the problem may clear by itself, so the delivery is retried.
func (c *signalSenderCache) senderKind(ctx context.Context) (number, problem string, retry bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.now().Sub(c.at) < signalSenderTTL {
		return c.number, c.problem, c.retry
	}
	rctx, cancel := context.WithTimeout(ctx, signalReadTimeout)
	defer cancel()
	st, err := c.pg.ReadSignalSidecar(rctx)
	if err != nil {
		// A failed read is not cached when the caller gave up (its context ended); otherwise it is, so a
		// database outage costs one read and one log line per interval.
		log.Printf("[signal] sender check failed: %s", signalLogText(err.Error()))
		if ctx.Err() == nil {
			c.at, c.valid, c.number, c.problem, c.retry = c.now(), true, "", signalDBUnavailableText, true
		}
		return "", signalDBUnavailableText, true
	}
	number, problem, retry = signalSenderFrom(st)
	c.at, c.valid, c.number, c.problem, c.retry = c.now(), true, number, problem, retry
	return number, problem, retry
}

// signalSenderFrom decides from both rows. The number is returned with the "not confirmed" problem so the
// notifier can mask it in the texts it stores. A service that is not connected may come back; an account that
// is not linked, not confirmed or in an error state needs the operator.
func signalSenderFrom(st db.SignalSidecarState) (number, problem string, retry bool) {
	switch {
	case !st.HeartbeatFresh:
		return "", signalNotConnectedText, true
	case st.State == db.SignalStateError:
		return "", signalErrorStateText, false
	case st.State != db.SignalStateLinked || st.AccountNumber == "":
		return "", signalNotLinkedText, false
	case st.ConfirmedAccount == "" || st.ConfirmedAccount != st.AccountNumber:
		return st.AccountNumber, signalNotConfirmedText, false
	}
	return st.AccountNumber, "", false
}

// ---- text helpers ----

var (
	signalURIRe    = regexp.MustCompile(`(?i)sgnl://[^\s"']*`)
	signalNumberRe = regexp.MustCompile(`\+[1-9][0-9]{6,14}`)
)

// signalLogText removes what must not reach a log or a response from a text that came from a lower layer: a
// link address, and full phone numbers (shortened to their last four digits).
func signalLogText(s string) string {
	s = signalURIRe.ReplaceAllString(s, "[link removed]")
	s = signalNumberRe.ReplaceAllStringFunc(s, func(n string) string { return "…" + n[len(n)-4:] })
	return logSafe(s)
}

// signalNumberHint shows the first three characters of an E.164 number and the count of digits.
func signalNumberHint(n string) string {
	if len(n) < 4 || !signalNumberRe.MatchString(n) {
		return ""
	}
	return fmt.Sprintf("%s… (%d digits)", n[:3], len(n)-1)
}

// signalMaskedGroup shows a group id as group.…abcd.
func signalMaskedGroup(id string) string {
	id = strings.TrimPrefix(id, "group.")
	if len(id) > 4 {
		id = id[len(id)-4:]
	}
	return "group.…" + id
}

func signalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func writeSignalCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

func signalOn() bool { return notify.SignalSocketPath() != "" }

// ---- route wrapper and registration ----

// signalRoute is the wrapper of every Signal route.
func (s *Server) signalRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		sub, ok := verifyJWTSubject(strings.TrimPrefix(auth, "Bearer "), s.jwtKey)
		if !ok || sub == "" {
			writeErr(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if (r.Method == http.MethodPost || r.ContentLength != 0) && !jsonMediaType(r) {
			writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), operatorSubjectKey, sub)))
	}
}

func (s *Server) registerSignalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/notify/signal/account", s.signalRoute(s.signalAccount))
	mux.HandleFunc("POST /api/notify/signal/link", s.signalRoute(s.signalLink))
	mux.HandleFunc("GET /api/notify/signal/link", s.signalRoute(s.signalLinkURI))
	mux.HandleFunc("DELETE /api/notify/signal/link", s.signalRoute(s.signalCancelLink))
	mux.HandleFunc("POST /api/notify/signal/account/confirm", s.signalRoute(s.signalConfirm))
	mux.HandleFunc("POST /api/notify/signal/unlink", s.signalRoute(s.signalUnlink))
	mux.HandleFunc("GET /api/notify/signal/groups", s.signalRoute(s.signalGroups))
	mux.HandleFunc("POST /api/notify/signal/groups/refresh", s.signalRoute(s.signalGroupsRefresh))
}

// signalReady answers 503 and returns nil unless the database is connected and ARTEX_SIGNAL_API is set.
func (s *Server) signalReady(w http.ResponseWriter) *db.DB {
	pg := s.pg(w)
	if pg == nil {
		return nil
	}
	if !signalOn() {
		writeSignalCode(w, http.StatusServiceUnavailable, signalCodeNotConfigured,
			"Signal service is not configured: set "+notify.SignalAPIEnv)
		return nil
	}
	return pg
}

// signalFail logs an error from the database layer and answers for it. The log line and the response never
// carry more than the error text of the database layer, with addresses and numbers removed.
func signalFail(w http.ResponseWriter, what string, err error) {
	log.Printf("[signal] %s: %s", what, signalLogText(err.Error()))
	if errors.Is(err, db.ErrSignalBusy) {
		writeSignalCode(w, http.StatusServiceUnavailable, signalCodeBusy,
			"Signal state is locked by another database session, retry later")
		return
	}
	writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
}

// ---- status ----

type signalSidecarJSON struct {
	Alive            bool   `json:"alive"`
	HeartbeatAgeS    int64  `json:"heartbeat_age_s"`
	Version          string `json:"version"`
	SignalCLIVersion string `json:"signal_cli_version"`
}

type signalAccountJSON struct {
	NumberHint string `json:"number_hint"`
	Confirmed  bool   `json:"confirmed"`
}

type signalLinkingJSON struct {
	ExpiresAt string `json:"expires_at"`
}

type signalDBRoleJSON struct {
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// signalWarningJSON is one entry of the warnings list. The card derives its warnings for the database role
// and the authentication rules from db_role and db_auth; this list is for any further kind.
type signalWarningJSON struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

type signalStatusJSON struct {
	Mode         string              `json:"mode"`
	Sidecar      signalSidecarJSON   `json:"sidecar"`
	State        string              `json:"state"`
	Gen          int64               `json:"gen"`
	Account      signalAccountJSON   `json:"account"`
	Linking      *signalLinkingJSON  `json:"linking"`
	GroupsCount  int                 `json:"groups_count"`
	GroupsAt     *string             `json:"groups_at"`
	RemoteUnlink string              `json:"remote_unlink"`
	LastError    string              `json:"last_error"`
	LastErrorAt  *string             `json:"last_error_at"`
	DBRole       signalDBRoleJSON    `json:"db_role"`
	DBAuth       string              `json:"db_auth"`
	Warnings     []signalWarningJSON `json:"warnings"`
}

func signalAuthValue(v string) string {
	switch v {
	case "ok", "trust", "unchecked", "asserted":
		return v
	}
	return "unchecked"
}

// GET /api/notify/signal/account
func (s *Server) signalAccount(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	role := currentSignalRole()
	if signalOn() {
		role = signalRoleCurrent(r.Context(), pg)
	}
	out := signalStatusJSON{
		Mode: "off", State: db.SignalStateUnknown, DBAuth: signalAuthValue(role.Auth),
		DBRole:   signalDBRoleJSON{State: role.State, Detail: role.Detail},
		Warnings: []signalWarningJSON{},
	}
	if !signalOn() {
		writeJSON(w, http.StatusOK, out)
		return
	}
	// A status read is where the server notices that the sidecar finished a command.
	if _, err := pg.ReconcileSignalSidecar(r.Context()); err != nil {
		signalFail(w, "status: reconcile", err)
		return
	}
	st, err := pg.ReadSignalSidecar(r.Context())
	if err != nil {
		signalFail(w, "status: read", err)
		return
	}
	out.Mode = "on"
	out.Sidecar = signalSidecarJSON{
		Alive: st.HeartbeatFresh, Version: st.SidecarVersion, SignalCLIVersion: st.CLIVersion,
	}
	if !st.HeartbeatAt.IsZero() {
		out.Sidecar.HeartbeatAgeS = int64(st.HeartbeatAge / time.Second)
	}
	out.State, out.Gen = st.State, st.Gen
	if st.State == db.SignalStateLinked {
		out.Account = signalAccountJSON{
			NumberHint: signalNumberHint(st.AccountNumber),
			Confirmed:  st.ConfirmedAccount != "" && st.ConfirmedAccount == st.AccountNumber,
		}
	}
	if st.State == db.SignalStateLinking && !st.LinkExpiresAt.IsZero() {
		out.Linking = &signalLinkingJSON{ExpiresAt: *signalTime(st.LinkExpiresAt)}
	}
	out.GroupsCount, out.GroupsAt = st.GroupsCount, signalTime(st.GroupsAt)
	out.RemoteUnlink = st.RemoteUnlink
	out.LastError, out.LastErrorAt = signalLogText(st.LastError), signalTime(st.LastErrorAt)
	out.DBAuth = signalAuthValue(st.DBAuthState)
	writeJSON(w, http.StatusOK, out)
}

// ---- password re-check ----

// signalCheckPassword settles a password check of the operator against the stored hash, with the login
// throttle. A reply has been written when it returns false. A wrong password is 403 with a code, never 401:
// the web client treats 401 as a lost session.
func (s *Server) signalCheckPassword(w http.ResponseWriter, pg *db.DB, sub *throttleSubject, operator, password string) bool {
	hash, ok, err := pg.GetSetting(authPassKey)
	if err != nil {
		log.Printf("[signal] password check: cannot read the stored password: %s", signalLogText(err.Error()))
		writeErr(w, http.StatusServiceUnavailable, errDataSourceUnavailable)
		return false
	}
	if !ok || hash == "" {
		writeErr(w, http.StatusForbidden, "Password not initialized; set a password first")
		return false
	}
	if !sub.reserve(w) {
		return false
	}
	if err := loginCompare([]byte(hash), []byte(password)); err != nil {
		sub.throttleFailure()
		log.Printf("[signal] password check failed for %s", logSafe(operator))
		writeSignalCode(w, http.StatusForbidden, signalCodePasswordIncorrect, "Current password is wrong")
		return false
	}
	sub.throttleSuccess()
	return true
}

// ---- link ----

type signalLinkRequest struct {
	Password    string `json:"password"`
	DeviceName  string `json:"device_name"`
	Acknowledge bool   `json:"acknowledge"`
}

// signalLinkRefusal says why a link request did not apply, from the rows as they are after it.
func signalLinkRefusal(st db.SignalSidecarState) (status int, code, msg string) {
	switch {
	case !st.HeartbeatFresh:
		return http.StatusServiceUnavailable, signalCodeSidecarDown, signalNotConnectedText
	case st.Desired == db.SignalDesiredLink || st.State == db.SignalStateLinking:
		return http.StatusConflict, signalCodeLinkPending, "A link is already pending"
	case st.Desired == db.SignalDesiredUnlink:
		return http.StatusConflict, signalCodeUnlinkPending, "An unlink is still in progress"
	case st.State == db.SignalStateLinked:
		return http.StatusConflict, signalCodeAlreadyLinked, "An account is already linked"
	case st.State == db.SignalStateError:
		return http.StatusConflict, signalCodeUnlinkFirst, "The Signal service is in an error state: unlink first"
	case st.State == db.SignalStateUnknown:
		return http.StatusConflict, signalCodeNotReady, "The Signal service has not reported its state yet"
	}
	return http.StatusConflict, signalCodeConflict, "The link request conflicts with the current state"
}

// POST /api/notify/signal/link
func (s *Server) signalLink(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	sub, ok := s.admit(w, r)
	if !ok {
		return
	}
	defer sub.release()
	var req signalLinkRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if !req.Acknowledge {
		writeErr(w, http.StatusBadRequest, "acknowledge must be true: the linked device can read the account's messages")
		return
	}
	if req.Password == "" {
		writeErr(w, http.StatusBadRequest, "password is required")
		return
	}
	device := req.DeviceName
	if device == "" {
		device = signalDefaultDevice
	}
	if !signalDeviceRe.MatchString(device) {
		writeErr(w, http.StatusBadRequest, db.ErrSignalDeviceName.Error())
		return
	}
	if !s.signalCheckPassword(w, pg, sub, operatorSubject(r), req.Password) {
		return
	}
	if role := currentSignalRole(); role.State != db.SignalRoleOK {
		writeSignalCode(w, http.StatusServiceUnavailable, signalCodeRoleNotReady,
			"The database role of the Signal service is not ready ("+role.State+")")
		return
	}
	operator := operatorSubject(r)
	gen, applied, err := pg.RequestSignalLink(r.Context(), device, operator)
	if err != nil {
		signalFail(w, "link request", err)
		return
	}
	if !applied {
		st, err := pg.ReadSignalSidecar(r.Context())
		if err != nil {
			signalFail(w, "link request: read", err)
			return
		}
		status, code, msg := signalLinkRefusal(st)
		writeSignalCode(w, status, code, msg)
		return
	}
	resetSignalSender()
	log.Printf("[signal] link requested by %s (generation %d)", logSafe(operator), gen)
	writeJSON(w, http.StatusAccepted, map[string]any{"gen": gen})
}

var signalDeviceRe = regexp.MustCompile(`^[A-Za-z0-9 ._-]{1,50}$`)

// signalLinkLogged is the generation whose address was fetched last; one audit line is written for each.
var signalLinkLogged atomic.Int64

// GET /api/notify/signal/link
func (s *Server) signalLinkURI(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	info, ok, err := pg.ReadSignalLink(r.Context())
	if err != nil {
		signalFail(w, "link read", err)
		return
	}
	if !ok {
		writeSignalCode(w, http.StatusNotFound, signalCodeNoLinkPending, "No link is pending")
		return
	}
	if signalLinkLogged.Swap(info.Gen) != info.Gen {
		log.Printf("[signal] link address fetched by %s (generation %d)", logSafe(operatorSubject(r)), info.Gen)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gen": info.Gen, "uri": info.URI, "expires_at": *signalTime(info.ExpiresAt),
	})
}

// DELETE /api/notify/signal/link
func (s *Server) signalCancelLink(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	applied, err := pg.CancelSignalLink(r.Context())
	if err != nil {
		signalFail(w, "link cancel", err)
		return
	}
	if !applied {
		writeSignalCode(w, http.StatusConflict, signalCodeNoLinkPending, "No link is pending")
		return
	}
	log.Printf("[signal] link cancelled by %s", logSafe(operatorSubject(r)))
	w.WriteHeader(http.StatusAccepted)
}

// ---- confirm ----

type signalConfirmRequest struct {
	Gen        int64  `json:"gen"`
	LastDigits string `json:"last_digits"`
}

var signalDigitsRe = regexp.MustCompile(`^[0-9]{6}$`)

// POST /api/notify/signal/account/confirm
func (s *Server) signalConfirm(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	sub, ok := s.admit(w, r)
	if !ok {
		return
	}
	defer sub.release()
	var req signalConfirmRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if req.Gen < 0 || !signalDigitsRe.MatchString(req.LastDigits) {
		writeErr(w, http.StatusBadRequest, "last_digits must be the last 6 digits of the number")
		return
	}
	// A guess counts like a wrong password: it is the only thing standing between a stolen session and a
	// hijacked link.
	if !sub.reserve(w) {
		return
	}
	operator := operatorSubject(r)
	res, err := pg.ConfirmSignalAccount(r.Context(), req.Gen, req.LastDigits)
	if err != nil {
		signalFail(w, "confirm", err)
		return
	}
	switch res {
	case db.SignalConfirmed:
		sub.throttleSuccess()
		resetSignalSender()
		log.Printf("[signal] account confirmed by %s", logSafe(operator))
		w.WriteHeader(http.StatusOK)
	case db.SignalConfirmMismatch:
		sub.throttleFailure()
		log.Printf("[signal] confirmation refused for %s: the digits do not match", logSafe(operator))
		writeSignalCode(w, http.StatusForbidden, signalCodeNumberMismatch, "The digits do not match the linked number")
	default:
		writeSignalCode(w, http.StatusConflict, signalCodeStale, "The account changed: reload and try again")
	}
}

// ---- unlink ----

type signalUnlinkRequest struct {
	Password string `json:"password"`
}

// signalUnlinkRefusal says why an unlink request did not apply.
func signalUnlinkRefusal(st db.SignalSidecarState) (status int, code, msg string) {
	switch {
	case !st.HeartbeatFresh:
		return http.StatusServiceUnavailable, signalCodeSidecarDown, signalNotConnectedText
	case st.Desired == db.SignalDesiredLink || st.State == db.SignalStateLinking:
		return http.StatusConflict, signalCodeLinkPending, "A link is pending: cancel it first"
	case st.Desired == db.SignalDesiredUnlink:
		return http.StatusConflict, signalCodeUnlinkPending, "An unlink is already in progress"
	case st.State == db.SignalStateUnlinked || st.State == db.SignalStateUnknown:
		return http.StatusConflict, signalCodeNothingToUnlink, "There is no linked account to unlink"
	}
	return http.StatusConflict, signalCodeConflict, "The unlink request conflicts with the current state"
}

// POST /api/notify/signal/unlink
func (s *Server) signalUnlink(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	sub, ok := s.admit(w, r)
	if !ok {
		return
	}
	defer sub.release()
	var req signalUnlinkRequest
	if !decodeAuthBody(w, r, &req) {
		return
	}
	if req.Password == "" {
		writeErr(w, http.StatusBadRequest, "password is required")
		return
	}
	if !s.signalCheckPassword(w, pg, sub, operatorSubject(r), req.Password) {
		return
	}
	operator := operatorSubject(r)
	gen, applied, err := pg.RequestSignalUnlink(r.Context(), operator)
	if err != nil {
		signalFail(w, "unlink request", err)
		return
	}
	if !applied {
		st, err := pg.ReadSignalSidecar(r.Context())
		if err != nil {
			signalFail(w, "unlink request: read", err)
			return
		}
		status, code, msg := signalUnlinkRefusal(st)
		writeSignalCode(w, status, code, msg)
		return
	}
	resetSignalSender()
	log.Printf("[signal] unlink requested by %s (generation %d)", logSafe(operator), gen)
	w.WriteHeader(http.StatusAccepted)
}

// ---- groups ----

type signalGroupJSON struct {
	ID     string `json:"id"`
	Masked string `json:"masked"`
	Name   string `json:"name"`
}

// GET /api/notify/signal/groups
func (s *Server) signalGroups(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	groups, at, err := pg.ReadSignalGroups(r.Context())
	if err != nil {
		signalFail(w, "groups read", err)
		return
	}
	out := make([]signalGroupJSON, 0, len(groups))
	for _, g := range groups {
		out = append(out, signalGroupJSON{ID: g.ID, Masked: signalMaskedGroup(g.ID), Name: g.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "fetched_at": signalTime(at)})
}

// POST /api/notify/signal/groups/refresh
func (s *Server) signalGroupsRefresh(w http.ResponseWriter, r *http.Request) {
	pg := s.signalReady(w)
	if pg == nil {
		return
	}
	applied, err := pg.RequestSignalGroupsRefresh(r.Context())
	if err != nil {
		signalFail(w, "groups refresh", err)
		return
	}
	if !applied {
		writeSignalCode(w, http.StatusConflict, signalCodeNotLinked, "No account is linked")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

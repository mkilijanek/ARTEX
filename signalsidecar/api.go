package signalsidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// MaxSendBody is the largest request body POST /v2/send accepts.
const MaxSendBody = 64 << 10

// maxErrorRunes cuts an error text before it leaves the sidecar.
const maxErrorRunes = 300

// Refusal is the reason the backend gives for not sending now. The empty value
// means the backend is ready.
type Refusal string

const (
	// RefuseNone means the backend is ready to send.
	RefuseNone Refusal = ""
	// RefuseNotLinked means no Signal account is linked (400).
	RefuseNotLinked Refusal = "not_linked"
	// RefuseNotConfirmed means the operator has not confirmed the linked account (400).
	RefuseNotConfirmed Refusal = "not_confirmed"
	// RefuseConfigStale means the configuration from the database is too old (503).
	RefuseConfigStale Refusal = "config_stale"
	// RefuseChildDown means signal-cli is not running (503).
	RefuseChildDown Refusal = "child_down"
)

// SendTarget is where a message goes. Exactly one of GroupID and Numbers is set.
type SendTarget struct {
	// GroupID is one group in the "group.<id>" form as the client sent it.
	GroupID string
	// Numbers are phone numbers in E.164 form.
	Numbers []string
}

// About is the answer of GET /v1/about. It names the service, never an account.
type About struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	SignalCLI string `json:"signal_cli"`
	State     string `json:"state"`
	// ChildDownSeconds is how long signal-cli has been down or silent; zero
	// (and absent) while it works.
	ChildDownSeconds int64 `json:"child_down_seconds,omitempty"`
}

// Backend is what the API needs from the rest of the sidecar.
type Backend interface {
	// Ready returns the confirmed account the sidecar may send from, or the
	// reason it may not. The account is meaningful only with RefuseNone.
	Ready(ctx context.Context) (account string, refusal Refusal)
	// Send sends one text message from account. A failure of signal-cli is
	// returned as received (an *RPCError, or an error such as ErrChildDown).
	Send(ctx context.Context, account string, to SendTarget, message string) (SendResult, error)
	// About describes the running service.
	About(ctx context.Context) About
}

var (
	e164Re    = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	groupIDRe = regexp.MustCompile(`^group\.[A-Za-z0-9+/=_-]{1,200}$`)
)

type sendRequest struct {
	Number     string   `json:"number"`
	Recipients []string `json:"recipients"`
	Message    string   `json:"message"`
	TextMode   string   `json:"text_mode"`
}

type api struct {
	b    Backend
	logf func(format string, args ...any)
}

// NewAPI returns the handler of the socket API: POST /v2/send, which sends one
// message, and GET /v1/about. Every other path answers 404 with an empty body,
// and a wrong method on those two paths answers 405, in both cases without a
// call to the backend. logf gets one line per refused or failed request, never
// message text; it may be nil.
func NewAPI(b Backend, logf func(format string, args ...any)) http.Handler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &api{b: b, logf: logf}
}

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/send":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		a.send(w, r)
	case "/v1/about":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		writeJSON(w, http.StatusOK, a.b.About(r.Context()))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil { // only plain structs and strings are marshalled here
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(raw)
}

// fail answers with {"error": text}. text is scrubbed here, so a caller cannot
// forget it.
func (a *api) fail(w http.ResponseWriter, status int, text string) {
	text = cutRunes(Scrub(text), maxErrorRunes)
	a.logf("[signal] api: send refused with %d: %s", status, text)
	writeJSON(w, status, map[string]string{"error": text})
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (a *api) send(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > MaxSendBody {
		a.fail(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxSendBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			a.fail(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			a.fail(w, http.StatusBadRequest, "request body could not be read")
		}
		return
	}
	req, target, problem := parseSend(raw)
	if problem != "" {
		a.fail(w, http.StatusBadRequest, problem)
		return
	}
	account, refusal := a.b.Ready(r.Context())
	switch refusal {
	case RefuseNone:
		if account == "" {
			a.fail(w, http.StatusServiceUnavailable, "no sending account available")
			return
		}
	case RefuseNotLinked:
		a.fail(w, http.StatusBadRequest, "no Signal account is linked")
		return
	case RefuseNotConfirmed:
		a.fail(w, http.StatusBadRequest, "the linked Signal account is not confirmed")
		return
	case RefuseConfigStale:
		a.fail(w, http.StatusServiceUnavailable, "the sidecar configuration is stale")
		return
	case RefuseChildDown:
		a.fail(w, http.StatusServiceUnavailable, "signal-cli is not running")
		return
	default:
		a.fail(w, http.StatusServiceUnavailable, "the sidecar is not ready")
		return
	}
	if req.Number != account {
		a.fail(w, http.StatusBadRequest, "number is not the confirmed account")
		return
	}
	res, err := a.b.Send(r.Context(), account, target, req.Message)
	if err != nil {
		status, text := mapSendError(err)
		a.fail(w, status, text)
		return
	}
	out := struct {
		Timestamp string      `json:"timestamp"`
		Errors    *sendErrors `json:"errors,omitempty"`
	}{Timestamp: strconv.FormatInt(res.Timestamp, 10)}
	for _, x := range res.Results {
		if x.Type == "SUCCESS" {
			continue
		}
		if out.Errors == nil {
			out.Errors = &sendErrors{}
		}
		out.Errors.Recipients = append(out.Errors.Recipients, recipientFailure{
			Number: x.Address.Number, UUID: x.Address.UUID, Reason: failureReason(x.Type)})
	}
	if out.Errors != nil {
		a.logf("[signal] api: send reached %d of %d recipients", len(res.Results)-len(out.Errors.Recipients), len(res.Results))
	}
	writeJSON(w, http.StatusCreated, out)
}

type sendErrors struct {
	Recipients []recipientFailure `json:"recipients"`
}

// recipientFailure is one entry of errors.recipients. The address is returned
// as received; the caller masks it.
type recipientFailure struct {
	Number string `json:"number,omitempty"`
	UUID   string `json:"uuid,omitempty"`
	Reason string `json:"reason"`
}

func failureReason(t string) string {
	t = strings.ToLower(strings.ReplaceAll(t, "_", " "))
	if t == "" {
		return "no reason given"
	}
	return cutRunes(Scrub(t), 80)
}

// parseSend decodes and checks a request body. problem is a fixed text that
// never repeats a value from the request.
func parseSend(raw []byte) (req sendRequest, to SendTarget, problem string) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, to, "request body is not a valid send request"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, to, "request body has data after the send request"
	}
	switch {
	case req.Number == "":
		return req, to, "number is required"
	case req.TextMode != "normal":
		return req, to, `text_mode must be "normal"`
	case req.Message == "":
		return req, to, "message is required"
	case len(req.Recipients) == 0:
		return req, to, "recipients: at least one recipient is required"
	}
	groups := 0
	for _, r := range req.Recipients {
		switch {
		case e164Re.MatchString(r):
		case groupIDRe.MatchString(r):
			groups++
		default:
			return req, to, "recipients: each must be a phone number like +15555550100 or one group.<id>"
		}
	}
	switch {
	case groups == 1 && len(req.Recipients) == 1:
		to.GroupID = req.Recipients[0]
	case groups == 0:
		to.Numbers = req.Recipients
	default:
		return req, to, "recipients: phone numbers or exactly one group, not a mix"
	}
	return req, to, ""
}

// mapSendError turns a failure of the send call into a status and a text.
// Rate limiting never carries signal-cli's text, which can hold a challenge.
func mapSendError(err error) (int, string) {
	var re *RPCError
	if errors.As(err, &re) {
		switch re.Code {
		case -5:
			return http.StatusTooManyRequests, "rate limited"
		case -32603, -3:
			return http.StatusServiceUnavailable, rpcText(re)
		default: // -4, -1, -32602 and any code not known to be transient
			return http.StatusBadRequest, rpcText(re)
		}
	}
	return http.StatusServiceUnavailable, err.Error()
}

func rpcText(re *RPCError) string {
	if re.Message == "" {
		return fmt.Sprintf("signal-cli error %d", re.Code)
	}
	return re.Message
}

// ListenSocket listens on a Unix socket at path with mode 0600. The parent
// directory must exist, be owned by the current user and have mode 0700. A
// stale socket file is removed; a live socket or any other kind of file is
// left alone and refused.
func ListenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	if !di.IsDir() {
		return nil, fmt.Errorf("socket directory %s is not a directory", dir)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		return nil, fmt.Errorf("socket directory %s has mode %04o, want 0700", dir, perm)
	}
	st, ok := di.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("socket directory %s: owner cannot be determined", dir)
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return nil, fmt.Errorf("socket directory %s is owned by uid %d, want %d", dir, st.Uid, uid)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is in use by another process", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("socket path: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on socket: %w", err)
	}
	// The directory is 0700, so nobody else can connect before this runs.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("socket mode: %w", err)
	}
	return ln, nil
}

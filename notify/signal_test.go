package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mkilijanek/artex/useragent"
)

// Fixtures. The group id is the primary recipient type of the Signal channel;
// it is base64(base64(bytes 0..31)), the bridge's own format.
const (
	sigSender     = "+15555550100"
	sigRecipA     = "+15555550101"
	sigRecipB     = "+15555550102"
	sigGroupOuter = "QUFFQ0F3UUZCZ2NJQ1FvTERBME9EeEFSRWhNVUZSWVhHQmthR3h3ZEhoOD0="
	sigGroupInner = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	sigGroup      = "group." + sigGroupOuter
)

func groupCfg(rec ...string) map[string]any {
	if len(rec) == 0 {
		rec = []string{sigGroup}
	}
	return map[string]any{"recipient_type": "group", "recipients": toAny(rec)}
}

func numbersCfg(rec ...string) map[string]any {
	return map[string]any{"recipient_type": "numbers", "recipients": toAny(rec)}
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

var sigMsg = Message{Items: []Item{{FindingID: 1, Severity: "high", Name: "SQL injection"}}}

// useSignal points the Signal channel at sock with the given sender for one
// test; Signal state is process-wide, so these tests never run in parallel.
// The sender comes from an installed hook, as the server does it.
func useSignal(t *testing.T, sock, number string) {
	t.Helper()
	env := map[string]string{SignalAPIEnv: "unix:" + sock}
	LoadSignalEnv(func(k string) string { return env[k] })
	SetSignalSender(func(context.Context) (string, string) { return number, "" })
	t.Cleanup(func() {
		SetSignalSender(nil)
		LoadSignalEnv(os.Getenv)
	})
}

type bridgeReq struct {
	method, path, query, ua, ctype string
	body                           []byte
}

// fakeBridge serves the bridge API on a Unix socket and records every request.
type fakeBridge struct {
	sock string
	mu   sync.Mutex
	reqs []bridgeReq
}

func (b *fakeBridge) requests() []bridgeReq {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bridgeReq(nil), b.reqs...)
}

// shortSockDir returns a directory whose path is short enough for a unix socket
// (108 bytes), which t.TempDir() with a long test name may not be.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sig")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newFakeBridgeFunc(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fakeBridge {
	t.Helper()
	b := &fakeBridge{sock: filepath.Join(shortSockDir(t), "api.sock")}
	l, err := net.Listen("unix", b.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.reqs = append(b.reqs, bridgeReq{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			ua: r.Header.Get("User-Agent"), ctype: r.Header.Get("Content-Type"), body: raw,
		})
		b.mu.Unlock()
		h(w, r)
	}))
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return b
}

// newFakeBridge answers every request with one fixed response.
func newFakeBridge(t *testing.T, status int, ctype, body string) *fakeBridge {
	t.Helper()
	return newFakeBridgeFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func sendGroup(t *testing.T, cfg map[string]any, m Message) (int, error) {
	t.Helper()
	return signalChannel{}.Send(context.Background(), cfg, m)
}

// ---- registry (AC1 side in notify) ----

func TestSignalRegistered(t *testing.T) {
	ch, ok := Get(KindSignal)
	if !ok || !ValidKind("signal") {
		t.Fatal("signal must be registered")
	}
	if ch.Kind() != "signal" || ch.DefaultRatePerMin() != 20 {
		t.Fatalf("kind/rate = %q/%d", ch.Kind(), ch.DefaultRatePerMin())
	}
	if ch.SecretKeys() == nil || len(ch.SecretKeys()) != 0 || ch.DestinationKeys() == nil || len(ch.DestinationKeys()) != 0 {
		t.Fatalf("secret/destination keys must be empty and non-nil: %#v %#v", ch.SecretKeys(), ch.DestinationKeys())
	}
	raw, _ := json.Marshal(ch.SecretKeys())
	if string(raw) != "[]" {
		t.Fatalf("secret_keys must render as [], got %s", raw)
	}
}

// ---- start-up configuration (AC3) ----

func TestSignalLoadEnv(t *testing.T) {
	cases := []struct {
		name       string
		api        string
		wantSocket string
		wantLog    []string // each must appear in the log
		wantErrSub string   // Validate error with a valid recipient list
	}{
		{"unset", "", "", nil, SignalAPIEnv},
		{"valid", "unix:/run/artex-private/signal/api.sock", "/run/artex-private/signal/api.sock", nil, ""},
		{"api is http", "http://signal-cli:8080", "", []string{"[notify] signal: " + SignalAPIEnv}, SignalAPIEnv},
		{"api relative", "unix:run/api.sock", "", []string{"[notify] signal: " + SignalAPIEnv}, SignalAPIEnv},
		{"api without scheme", "/run/api.sock", "", []string{"[notify] signal: " + SignalAPIEnv}, SignalAPIEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			old := log.Writer()
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(old) })
			t.Cleanup(func() { LoadSignalEnv(os.Getenv) })
			SetSignalSender(func(context.Context) (string, string) { return sigSender, "" })
			t.Cleanup(func() { SetSignalSender(nil) })

			env := map[string]string{SignalAPIEnv: tc.api}
			LoadSignalEnv(func(k string) string { return env[k] })
			// The parse runs in init, before the log sink exists: it only stores the lines, and
			// LogSignalConfig (called by main once the sink is up) writes them.
			if buf.Len() != 0 {
				t.Errorf("LoadSignalEnv must not log, got %q", buf.String())
			}
			LogSignalConfig()

			if got := SignalSocketPath(); got != tc.wantSocket {
				t.Errorf("SignalSocketPath() = %q, want %q", got, tc.wantSocket)
			}
			logged := buf.String()
			for _, s := range tc.wantLog {
				if !strings.Contains(logged, s) {
					t.Errorf("log %q should contain %q", logged, s)
				}
			}
			if tc.wantLog == nil && logged != "" {
				t.Errorf("unexpected log output %q", logged)
			}
			err := signalChannel{}.Validate(groupCfg())
			switch {
			case tc.wantErrSub == "" && err != nil:
				t.Errorf("Validate with valid env: %v", err)
			case tc.wantErrSub != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSub)):
				t.Errorf("Validate error = %v, want it to name %q", err, tc.wantErrSub)
			}
		})
	}
}

// The former sender variable is ignored: one log line says so, the value is
// never logged, and it does not become the sender.
func TestSignalLegacyNumberVariableIsIgnored(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	t.Cleanup(func() { LoadSignalEnv(os.Getenv) })

	env := map[string]string{SignalAPIEnv: "unix:/s/api.sock", "ARTEX_SIGNAL_NUMBER": "+15555550107"}
	LoadSignalEnv(func(k string) string { return env[k] })
	LogSignalConfig()

	const want = "[notify] signal: ARTEX_SIGNAL_NUMBER is no longer used; link the account under System, Notifications"
	logged := buf.String()
	if n := strings.Count(logged, want); n != 1 || strings.Count(logged, "[notify] signal:") != 1 {
		t.Fatalf("want exactly the one ignore line, got %q", logged)
	}
	if strings.Contains(logged, "5555550199") {
		t.Fatalf("the value must not be logged: %q", logged)
	}
	err := signalChannel{}.Validate(groupCfg())
	if err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Fatalf("the variable must not provide a sender, got %v", err)
	}

	// A blank variable logs nothing.
	buf.Reset()
	env["ARTEX_SIGNAL_NUMBER"] = "  "
	LoadSignalEnv(func(k string) string { return env[k] })
	LogSignalConfig()
	if buf.Len() != 0 {
		t.Fatalf("a blank variable must not log, got %q", buf.String())
	}
}

func TestSignalSenderHook(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	env := map[string]string{SignalAPIEnv: "unix:" + br.sock}
	LoadSignalEnv(func(k string) string { return env[k] })
	t.Cleanup(func() {
		SetSignalSender(nil)
		LoadSignalEnv(os.Getenv)
	})

	cases := []struct {
		name     string
		hook     func(context.Context) (string, string)
		wantErr  string // "" = the send goes out
		wantSent string // number in the request body
	}{
		{"no hook installed", nil, "Signal account is not linked: link one under System, Notifications", ""},
		{"not linked", func(context.Context) (string, string) {
			return "", "Signal account is not linked: link one under System, Notifications"
		}, "Signal account is not linked: link one under System, Notifications", ""},
		{"not confirmed", func(context.Context) (string, string) {
			return sigSender, "confirm the linked Signal account first"
		}, "confirm the linked Signal account first", ""},
		{"error state", func(context.Context) (string, string) {
			return "", "Signal service is in an error state: unlink and link the account again"
		}, "unlink and link the account again", ""},
		{"empty number and no problem", func(context.Context) (string, string) { return "", "" }, "not linked", ""},
		{"malformed number", func(context.Context) (string, string) { return "12345", "" }, "invalid", ""},
		{"ready", func(context.Context) (string, string) { return sigRecipB, "" }, "", sigRecipB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SetSignalSender(tc.hook)
			before := len(br.requests())
			vErr := signalChannel{}.Validate(groupCfg())
			_, sErr := sendGroup(t, groupCfg(), sigMsg)
			reqs := br.requests()[before:]
			if tc.wantErr == "" {
				if vErr != nil || sErr != nil {
					t.Fatalf("want ok, got validate=%v send=%v", vErr, sErr)
				}
				if len(reqs) != 1 {
					t.Fatalf("want one request, got %d", len(reqs))
				}
				var p signalPayload
				if err := json.Unmarshal(reqs[0].body, &p); err != nil || p.Number != tc.wantSent {
					t.Fatalf("number in the body = %q (%v), want %q", p.Number, err, tc.wantSent)
				}
				return
			}
			for _, err := range []error{vErr, sErr} {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
			}
			if !IsPermanent(sErr) {
				t.Fatalf("a refused send must be permanent: %v", sErr)
			}
			if len(reqs) != 0 {
				t.Fatalf("nothing may be sent, got %d requests", len(reqs))
			}
		})
	}
}

// A problem that may clear by itself (the service is not connected, the state cannot be read) is retried; a
// hook that does not say so, or says it is not retryable, is permanent.
func TestSignalSenderHookRetryableProblems(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	env := map[string]string{SignalAPIEnv: "unix:" + br.sock}
	LoadSignalEnv(func(k string) string { return env[k] })
	t.Cleanup(func() {
		SetSignalSender(nil)
		LoadSignalEnv(os.Getenv)
	})
	cases := []struct {
		name    string
		problem string
		retry   bool
	}{
		{"service down", "Signal service is not connected", true},
		{"state unreadable", "Signal state cannot be read: database unavailable", true},
		{"not linked", "Signal account is not linked: link one under System, Notifications", false},
		{"not confirmed", "confirm the linked Signal account first", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			SetSignalSenderKind(func(context.Context) (string, string, bool) { return sigSender, tc.problem, tc.retry })
			before := len(br.requests())
			vErr := signalChannel{}.Validate(groupCfg())
			_, sErr := sendGroup(t, groupCfg(), sigMsg)
			for _, err := range []error{vErr, sErr} {
				if err == nil || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.problem)
				}
			}
			if IsPermanent(sErr) == tc.retry {
				t.Fatalf("permanent = %v for retryable = %v: %v", IsPermanent(sErr), tc.retry, sErr)
			}
			if strings.Contains(sErr.Error(), sigSender) {
				t.Fatalf("the sender number is not masked: %v", sErr)
			}
			if n := len(br.requests()) - before; n != 0 {
				t.Fatalf("nothing may be sent, got %d requests", n)
			}
		})
	}
}

// The sender is asked on every Validate and Send, so a change of the account
// shows up in the next call without a restart.
func TestSignalSenderHookIsAskedEachTime(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)
	var calls atomic.Int32
	SetSignalSender(func(context.Context) (string, string) {
		calls.Add(1)
		return sigSender, ""
	})
	if err := (signalChannel{}).Validate(groupCfg()); err != nil {
		t.Fatal(err)
	}
	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	if calls.Load() < 2 {
		t.Fatalf("hook calls = %d, want at least 2", calls.Load())
	}
}

// The dialer refuses a socket path that is a symlink or not a socket, before it connects.
func TestSignalDialRefusesSymlinkAndNonSocket(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	dir := shortSockDir(t)

	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(br.sock, link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.sock")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, want string
	}{
		{"symlink to a live socket", link, "symlink"},
		{"regular file", plain, "not a socket"},
		{"directory", dir, "not a socket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useSignal(t, tc.path, sigSender)
			before := len(br.requests())
			_, err := sendGroup(t, groupCfg(), sigMsg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to say %q", err, tc.want)
			}
			if strings.Contains(err.Error(), dir) {
				t.Fatalf("error leaks the path: %v", err)
			}
			if n := len(br.requests()) - before; n != 0 {
				t.Fatalf("nothing may reach the socket, got %d requests", n)
			}
		})
	}
}

// ---- Validate (AC2) ----

func TestSignalValidateRecipients(t *testing.T) {
	useSignal(t, "/s/api.sock", sigSender)
	// A second, distinct valid group id: base64(base64(bytes 1..32)).
	group2 := "group.QVFJREJBVUdCd2dKQ2dzTURRNFBFQkVTRXhRVkZoY1lHUm9iSEIwZUh5QT0="
	cases := []struct {
		name    string
		cfg     map[string]any
		wantErr string // "" = valid; else substring naming the field
	}{
		// group first: the primary type
		{"group primary", groupCfg(), ""},
		{"group, type absent (auto)", map[string]any{"recipients": []any{sigGroup}}, ""},
		{"group listed twice is one group", groupCfg(sigGroup, sigGroup), ""},
		{"group id not base64", groupCfg("group.!!!notbase64"), "recipients[0]"},
		{"group id decodes once to wrong length", groupCfg("group.bm90LWEtZ3JvdXA="), "recipients[0]"},
		{"group id single base64 of 32 bytes", groupCfg("group." + sigGroupInner), "recipients[0]"},
		{"group id empty", groupCfg("group."), "recipients[0]"},
		{"two groups", groupCfg(sigGroup, group2), "recipients"},
		{"group with numbers", map[string]any{"recipients": []any{sigGroup, sigRecipA}}, "recipients"},
		{"group type with a number", groupCfg(sigRecipA), "recipient_type"},
		{"group type with two entries", groupCfg(sigGroup, group2), "recipients"},
		{"group type, mixed", map[string]any{"recipient_type": "group", "recipients": []any{sigGroup, sigRecipA}}, "recipients"},
		{"numbers type with a group", numbersCfg(sigGroup), "recipient_type"},
		// numbers
		{"numbers", numbersCfg(sigRecipA, sigRecipB), ""},
		{"numbers, type absent", map[string]any{"recipients": []any{sigRecipA}}, ""},
		{"numbers, whitespace trimmed", numbersCfg("  " + sigRecipA + " "), ""},
		{"number without plus", numbersCfg("15555550101"), "recipients[0]"},
		{"number too short", numbersCfg("+123456"), "recipients[0]"},
		{"number too long", numbersCfg("+1234567890123456"), "recipients[0]"},
		{"number leading zero", numbersCfg("+0555555010"), "recipients[0]"},
		{"number with letters", numbersCfg("+1555abc0101"), "recipients[0]"},
		{"username", numbersCfg("alice.01"), "not supported"},
		{"uuid", numbersCfg("a1b2c3d4-0000-4000-8000-000000000000"), "not supported"},
		{"u: username prefix", map[string]any{"recipients": []any{"u:alice.01"}}, "not supported"},
		// shape
		{"empty list", map[string]any{"recipients": []any{}}, "recipients"},
		{"missing", map[string]any{}, "recipients"},
		{"nil config", nil, "recipients"},
		{"blank entries only", map[string]any{"recipients": []any{"", "  "}}, "recipients"},
		{"non-string entry", map[string]any{"recipients": []any{sigRecipA, 15555550101.0}}, "recipients[1]"},
		{"recipient_type invalid", map[string]any{"recipient_type": "usernames", "recipients": []any{sigRecipA}}, "recipient_type"},
		{"single string accepted", map[string]any{"recipients": sigRecipA}, ""},
		// config cannot carry the sender or an endpoint
		{"number key ignored", map[string]any{"recipients": []any{sigRecipA}, "number": "+19999999999"}, ""},
		{"url key ignored", map[string]any{"recipients": []any{sigRecipA}, "url": "http://evil.example"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := signalChannel{}.Validate(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error naming %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should name %q", err, tc.wantErr)
			}
			// A validation message never echoes a recipient value.
			for _, v := range []string{sigRecipA, sigGroupOuter, sigGroupInner, "alice.01"} {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("error echoes recipient value %q: %v", v, err)
				}
			}
		})
	}
}

// ---- Send: request shape (AC4, AC5, AC8) ----

func TestSignalSendGroupRequestShape(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1700000000000"}]`)
	useSignal(t, br.sock, sigSender)

	// "number" and url keys in the config are ignored and never sent.
	cfg := groupCfg()
	cfg["number"] = "+19999999999"
	cfg["url"] = "http://127.0.0.1:1/evil"
	cfg["base_url"] = "http://127.0.0.1:1/evil"
	cfg["other"] = "x"
	kept, err := sendGroup(t, cfg, sigMsg)
	if err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("kept = %d, want 1", kept)
	}
	reqs := br.requests()
	if len(reqs) != 1 {
		t.Fatalf("want exactly one request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.method != "POST" || r.path != "/v2/send" || r.query != "" {
		t.Fatalf("request = %s %s?%s", r.method, r.path, r.query)
	}
	if !strings.HasPrefix(r.ctype, "application/json") {
		t.Fatalf("content type = %q", r.ctype)
	}
	var body map[string]any
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"message", "number", "recipients", "text_mode"}) {
		t.Fatalf("body keys = %v", keys)
	}
	if body["number"] != sigSender || body["text_mode"] != "normal" {
		t.Fatalf("number/text_mode = %v/%v", body["number"], body["text_mode"])
	}
	// The group id is sent verbatim.
	if !reflect.DeepEqual(body["recipients"], []any{sigGroup}) {
		t.Fatalf("recipients = %v", body["recipients"])
	}
	if msg, _ := body["message"].(string); !strings.HasPrefix(msg, "[🟠 High] SQL injection") {
		t.Fatalf("message = %q", msg)
	}
}

func TestSignalSendNumbersDeduplicated(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)
	if _, err := sendGroup(t, numbersCfg(sigRecipA, sigRecipB, sigRecipA), sigMsg); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Recipients []string `json:"recipients"`
	}
	if err := json.Unmarshal(br.requests()[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Recipients, []string{sigRecipA, sigRecipB}) {
		t.Fatalf("recipients = %v", body.Recipients)
	}
}

func TestSignalSendUserAgentFollowsNotifyComponent(t *testing.T) {
	t.Cleanup(func() { useragent.SetHidden(true); useragent.SetVersion("dev") })
	useragent.SetVersion("9.9.9")
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)

	useragent.SetHidden(false)
	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	useragent.SetHidden(true)
	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	reqs := br.requests()
	if !strings.Contains(reqs[0].ua, "ARTEX/9.9.9 (+Notify)") {
		t.Fatalf("shown: User-Agent = %q", reqs[0].ua)
	}
	if !strings.HasPrefix(reqs[1].ua, "Go-http-client/") {
		t.Fatalf("hidden: User-Agent = %q", reqs[1].ua)
	}
}

func TestSignalIgnoresProxyEnvironment(t *testing.T) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(k, "http://127.0.0.1:1")
	}
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)
	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatalf("send must reach the socket, not the proxy: %v", err)
	}
	if len(br.requests()) != 1 {
		t.Fatal("request did not reach the socket")
	}
	// http.ProxyFromEnvironment caches the environment once per process, so also
	// assert the configuration itself.
	st, ok := signalState.Load().client.Transport.(*signalTransport)
	if !ok || st.base.Proxy != nil {
		t.Fatal("the Signal transport must not have a Proxy function")
	}
}

func TestSignalClientConfiguration(t *testing.T) {
	useSignal(t, "/s/api.sock", sigSender)
	c := signalState.Load().client
	if c.Timeout != 25*time.Second {
		t.Fatalf("timeout = %v", c.Timeout)
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect = %v", err)
	}
}

// ---- Send: classification (AC6) ----

func TestSignalSendClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		ctype     string
		body      string
		permanent bool
		wantSub   []string
		wantNot   []string
	}{
		{"400 json", 400, "application/json", `{"error":"Specified account does not exist"}`, true, []string{"HTTP 400", "Specified account does not exist"}, nil},
		{"400 empty body is text/plain with json", 400, "text/plain; charset=utf-8", `{"error":"Couldn't process request - invalid request"}`, true, []string{"HTTP 400", "invalid request"}, nil},
		{"429 with challenge tokens", 429, "application/json",
			`{"error":"Rate Limit Reached","challenge_tokens":["CHALLENGE-TOKEN-XYZ"],"account":"+15555550100"}`, false,
			[]string{"Signal rate limit"}, []string{"CHALLENGE-TOKEN-XYZ", "challenge_tokens", "5555550100"}},
		{"500", 500, "text/plain", "boom", false, []string{"HTTP 500"}, nil},
		{"502 from nginx", 502, "text/html", "<html>Bad Gateway</html>", false, []string{"HTTP 502"}, nil},
		{"404", 404, "text/plain", "404 page not found", true, []string{"HTTP 404"}, nil},
		{"403 from allowlist", 403, "text/html", "<html>403</html>", true, []string{"HTTP 403"}, nil},
		{"413 from allowlist", 413, "text/html", "<html>413</html>", true, []string{"HTTP 413"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			br := newFakeBridge(t, tc.status, tc.ctype, tc.body)
			useSignal(t, br.sock, sigSender)
			_, err := sendGroup(t, groupCfg(), sigMsg)
			if err == nil {
				t.Fatal("want an error")
			}
			if IsPermanent(err) != tc.permanent {
				t.Fatalf("IsPermanent = %v, want %v (%v)", IsPermanent(err), tc.permanent, err)
			}
			for _, s := range tc.wantSub {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q should contain %q", err, s)
				}
			}
			for _, s := range tc.wantNot {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q must not contain %q", err, s)
				}
			}
			if n := len(br.requests()); n != 1 {
				t.Errorf("requests = %d, want 1", n)
			}
		})
	}
}

func TestSignalSendRedirectsAreNotFollowed(t *testing.T) {
	for _, code := range []int{301, 302, 307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			br := newFakeBridgeFunc(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "/v2/elsewhere")
				w.WriteHeader(code)
			})
			useSignal(t, br.sock, sigSender)
			_, err := sendGroup(t, groupCfg(), sigMsg)
			if err == nil || !IsPermanent(err) {
				t.Fatalf("a redirect must be a permanent error, got %v", err)
			}
			if n := len(br.requests()); n != 1 {
				t.Fatalf("redirect was followed: %d requests recorded", n)
			}
		})
	}
}

func TestSignalSendSocketMissingIsRetryable(t *testing.T) {
	useSignal(t, filepath.Join(shortSockDir(t), "absent.sock"), sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	assertSocketUnreachable(t, err)
}

func TestSignalSendConnectionRefusedIsRetryable(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	// Leave the socket file behind with nobody listening: ECONNREFUSED.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if _, statErr := os.Lstat(sock); statErr != nil {
		t.Fatalf("stale socket file expected: %v", statErr)
	}
	useSignal(t, sock, sigSender)
	_, err = sendGroup(t, groupCfg(), sigMsg)
	assertSocketUnreachable(t, err)
}

func assertSocketUnreachable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	if IsPermanent(err) {
		t.Fatalf("an unreachable socket must be retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "Signal service socket unreachable (is docker-compose.signal.yml up?)") {
		t.Fatalf("error = %q", err)
	}
	for _, leak := range []string{"connect:", "no such file", "connection refused", "api.sock", "absent.sock"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
}

func TestSignalSendTimeoutIsRetryable(t *testing.T) {
	br := newFakeBridgeFunc(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	useSignal(t, br.sock, sigSender)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := signalChannel{}.Send(ctx, groupCfg(), sigMsg)
	if err == nil || IsPermanent(err) {
		t.Fatalf("a timeout must be a retryable error, got %v", err)
	}
}

func TestSignalSendPartialDeliveryIsPermanent(t *testing.T) {
	body := `[{"timestamp":"1700000000000","errors":{"recipients":[` +
		`{"number":"+15555550102","uuid":"","username":"","reason":"Unregistered user +15555550102"}]}}]`
	br := newFakeBridge(t, 201, "application/json", body)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, numbersCfg(sigRecipA, sigRecipB), sigMsg)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("partial delivery must be a permanent error, got %v", err)
	}
	want := "delivered to 1 of 2; failed: …0102 (Unregistered user …0102)"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if strings.Contains(err.Error(), "5555550102") {
		t.Fatalf("full number in error: %v", err)
	}
}

func TestSignalSendPartialDeliveryGroup(t *testing.T) {
	body := `[{"timestamp":"1","errors":{"recipients":[{"number":"","uuid":"123e4567-e89b-12d3-a456-426614174000","username":"","reason":"Untrusted identity"}]}}]`
	br := newFakeBridge(t, 201, "application/json", body)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed for 1 member(s)") || strings.Contains(err.Error(), "426614174000") && strings.Contains(err.Error(), "123e4567") {
		t.Fatalf("error = %q", err)
	}
}

func TestSignalSend201Shapes(t *testing.T) {
	cases := []struct {
		name, body string
		wantErr    bool
	}{
		{"array no errors", `[{"timestamp":"1"}]`, false},
		{"array empty errors", `[{"timestamp":"1","errors":{"recipients":[]}}]`, false},
		{"object form", `{"timestamp":"1"}`, false},
		{"object numeric timestamp", `{"timestamp":1732370000000}`, false},
		{"array of two", `[{"timestamp":"1"},{"timestamp":2}]`, false},
		{"empty body", ``, true},
		{"whitespace body", " \n", true},
		{"empty object", `{}`, true},
		{"empty array", `[]`, true},
		{"null", `null`, true},
		{"object with error", `{"error":"boom"}`, true},
		{"error and timestamp", `{"timestamp":"1","error":"boom"}`, true},
		{"missing timestamp", `{"foo":"bar"}`, true},
		{"zero numeric timestamp", `{"timestamp":0}`, true},
		{"empty string timestamp", `{"timestamp":""}`, true},
		{"boolean timestamp", `{"timestamp":true}`, true},
		{"array with one bad element", `[{"timestamp":"1"},{}]`, true},
		{"array with null element", `[{"timestamp":"1"},null]`, true},
		{"array of scalars", `[1,2]`, true},
		{"scalar", `42`, true},
		{"not json", `OK`, true},
		{"truncated json array", `[{"timestamp":`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			br := newFakeBridge(t, 201, "application/json", tc.body)
			useSignal(t, br.sock, sigSender)
			kept, err := sendGroup(t, groupCfg(), sigMsg)
			if tc.wantErr {
				if err == nil || !IsPermanent(err) {
					t.Fatalf("want a permanent error, got kept=%d err=%v", kept, err)
				}
				if !strings.HasPrefix(err.Error(), "Signal service returned an unexpected success response (HTTP 201)") &&
					!strings.Contains(err.Error(), "unreadable success response") {
					t.Fatalf("error = %q", err)
				}
				return
			}
			if err != nil || kept != 1 {
				t.Fatalf("kept=%d err=%v", kept, err)
			}
		})
	}
}

// The error for an unexpected success never quotes the body: only the text of an error member, with
// numbers masked and cut short, may follow the status.
func TestSignalSendUnexpectedSuccessHintIsScrubbed(t *testing.T) {
	long := strings.Repeat("x", 300)
	body := `{"error":"account ` + sigSender + ` ` + long + `","challenge_tokens":["CHALLENGE-TOKEN-XYZ"],"timestamp":"1"}`
	br := newFakeBridge(t, 200, "application/json", body)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	if err == nil || !IsPermanent(err) {
		t.Fatalf("want a permanent error, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "Signal service returned an unexpected success response (HTTP 200): account …0100") {
		t.Fatalf("error = %q", err)
	}
	for _, leak := range []string{"5555550100", "CHALLENGE-TOKEN-XYZ", "challenge_tokens", long} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
	if len(err.Error()) > 200 {
		t.Fatalf("hint is not short: %d bytes", len(err.Error()))
	}
}

func TestSignalSendWithoutApiIsPermanentAndSendsNothing(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	SetSignalSender(func(context.Context) (string, string) { return sigSender, "" })
	t.Cleanup(func() {
		SetSignalSender(nil)
		LoadSignalEnv(os.Getenv)
	})
	LoadSignalEnv(func(string) string { return "" })
	_, err := sendGroup(t, groupCfg(), sigMsg)
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "ARTEX_SIGNAL_API") {
		t.Fatalf("want a permanent error naming ARTEX_SIGNAL_API, got %v", err)
	}
	if n := len(br.requests()); n != 0 {
		t.Fatalf("nothing may be sent without a socket, got %d requests", n)
	}
}

func TestSignalSendInvalidConfigIsPermanent(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, map[string]any{"recipients": []any{}}, sigMsg)
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "recipients") {
		t.Fatalf("got %v", err)
	}
	if len(br.requests()) != 0 {
		t.Fatal("an invalid config must not reach the bridge")
	}
}

// ---- redaction (AC7) ----

// assertSignalClean fails when err text carries any full number or group id.
func assertSignalClean(t *testing.T, text string, probes ...string) {
	t.Helper()
	for _, p := range probes {
		if strings.Contains(text, p) {
			t.Fatalf("text leaks %q:\n    %s", p, text)
		}
	}
	for _, frag := range []string{"+1555", "15555550", sigGroupOuter, sigGroupInner} {
		if strings.Contains(text, frag) {
			t.Fatalf("text leaks fragment %q:\n    %s", frag, text)
		}
	}
}

func TestSignalErrorsRedactNumbersAndGroups(t *testing.T) {
	// (a) fixed probe numbers and the group id in the bridge's error body, as the
	// real bridge echoes them ("Invalid identifier group.…").
	body, _ := json.Marshal(map[string]any{"error": "Invalid identifier " + sigGroup +
		" for +15555550100, +15555550101 and +15555550107"})
	br := newFakeBridge(t, 400, "application/json", string(body))
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	if err == nil {
		t.Fatal("want an error")
	}
	assertSignalClean(t, err.Error(), sigSender, sigRecipA, "5555550199")
	for _, want := range []string{"group.…", "…0100", "…0101", "…0107"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "group."+sigGroupOuter[:10]) {
		t.Errorf("group id head survived: %v", err)
	}
}

func TestSignalErrorTruncationProbe(t *testing.T) {
	// (b) the 200-byte cut point falls inside a number: masking must run before
	// the cut, so no "+1555" fragment can survive.
	for pad := 190; pad <= 200; pad++ {
		msg := strings.Repeat("x", pad) + sigRecipA + " tail"
		raw, _ := json.Marshal(map[string]any{"error": msg})
		br := newFakeBridge(t, 400, "application/json", string(raw))
		useSignal(t, br.sock, sigSender)
		_, err := sendGroup(t, numbersCfg(sigRecipA), sigMsg)
		if err == nil {
			t.Fatal("want an error")
		}
		assertSignalClean(t, err.Error())
		if pad == 195 && !strings.Contains(err.Error(), "…0101") {
			// the cut must keep the masked form, not drop the value before masking
			t.Logf("pad=195 error: %s", err)
		}
	}
	// The same with a number that the configuration does not know about (regex
	// path, not the literal path).
	msg := strings.Repeat("x", 195) + "+15555550177 tail"
	raw, _ := json.Marshal(map[string]any{"error": msg})
	br := newFakeBridge(t, 400, "application/json", string(raw))
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	if err == nil {
		t.Fatal("want an error")
	}
	assertSignalClean(t, err.Error(), "+155555", "15555550177")
}

func TestSignalErrorLiteralProbe(t *testing.T) {
	// (c) values the regexes alone do not catch: the inner (once-decoded) group
	// id bare or the outer id without the prefix, in a body that is not JSON
	// shaped as {"error"} either.
	bodies := map[string]string{
		"inner id bare":         `{"error":"unknown group ` + sigGroupInner + ` here"}`,
		"outer id without pfx":  `{"error":"unknown group ` + sigGroupOuter + ` here"}`,
		"sender and recip bare": `{"error":"account 15555550100 cannot send to 15555550101 or 15555550102"}`,
		"plain text body":       "no such account " + sigSender + " / " + sigGroupInner,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			br := newFakeBridge(t, 400, "text/plain", body)
			useSignal(t, br.sock, sigSender)
			_, err := sendGroup(t, groupCfg(sigGroup), sigMsg)
			if err == nil {
				t.Fatal("want an error")
			}
			assertSignalClean(t, err.Error(), sigSender, sigRecipA, sigRecipB)
			if strings.Contains(err.Error(), "AAECAwQF") || strings.Contains(err.Error(), "QUFFQ0F3") {
				t.Fatalf("group id head survived: %v", err)
			}
		})
	}
	// Numbers channel: the configured recipients are replaced as literals.
	br := newFakeBridge(t, 400, "application/json", `{"error":"cannot reach 15555550101 (or +15555550102) from 15555550100"}`)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, numbersCfg(sigRecipA, sigRecipB), sigMsg)
	if err == nil {
		t.Fatal("want an error")
	}
	assertSignalClean(t, err.Error(), sigSender, sigRecipA, sigRecipB)
}

func TestSignalErrorDropsEverythingButTheErrorField(t *testing.T) {
	br := newFakeBridge(t, 400, "application/json",
		`{"error":"nope","challenge_tokens":["TOKEN-ABC"],"account":"+15555550100","extra":"EXTRA-FIELD"}`)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
	for _, s := range []string{"TOKEN-ABC", "EXTRA-FIELD", "challenge_tokens", "account"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error quotes the raw body (%q): %v", s, err)
		}
	}
}

func TestSignalTransportAndConfigErrorsAreRedacted(t *testing.T) {
	useSignal(t, filepath.Join(shortSockDir(t), "absent.sock"), sigSender)
	_, err := sendGroup(t, numbersCfg(sigRecipA), sigMsg)
	if err == nil {
		t.Fatal("want an error")
	}
	assertSignalClean(t, err.Error(), sigSender, sigRecipA)
	// A message carrying the sender (for example a wrapped error) is masked too.
	scrub := newSignalScrubber(sigSender, []string{sigRecipA, sigGroup})
	got := scrub.err(Permanent(errors.New("boom " + sigSender + " " + sigRecipA + " " + sigGroup)))
	if !IsPermanent(got) {
		t.Fatal("scrub.err must keep the permanent marker")
	}
	assertSignalClean(t, got.Error(), sigSender, sigRecipA)
	if IsPermanent(scrub.err(errors.New("x"))) {
		t.Fatal("scrub.err must not add the permanent marker")
	}
	if scrub.err(nil) != nil {
		t.Fatal("scrub.err(nil) must be nil")
	}
}

func TestSignalScrubIsIdempotent(t *testing.T) {
	scrub := newSignalScrubber(sigSender, []string{sigGroup})
	once := scrub.text("a " + sigSender + " " + sigGroup + " +15555550123")
	if twice := scrub.text(once); twice != once {
		t.Fatalf("scrub is not idempotent: %q -> %q", once, twice)
	}
}

func TestSignalScrubBodyUsesOneLineLast(t *testing.T) {
	scrub := newSignalScrubber(sigSender, nil)
	got := scrub.body([]byte(`{"error":"` + strings.Repeat("y", 400) + `"}`))
	if n := len([]rune(got)); n != 200 {
		t.Fatalf("fragment should be cut to 200 runes, got %d", n)
	}
}

// ---- redaction invariant, with configured values and the real channel ----

func TestSignalChannelErrorsNeverLeakRecipients(t *testing.T) {
	useSignal(t, filepath.Join(shortSockDir(t), "absent.sock"), sigSender)
	for _, cfg := range []map[string]any{groupCfg(), numbersCfg(sigRecipA, sigRecipB)} {
		_, err := signalChannel{}.Send(context.Background(), cfg, sigMsg)
		if err == nil {
			t.Fatal("an unreachable socket should error")
		}
		assertSignalClean(t, err.Error(), sigSender, sigRecipA, sigRecipB)
	}
}

// ---- rendering (AC9) ----

func TestSignalTextSingleGolden(t *testing.T) {
	it := Item{
		Name: "SQL injection", VulnClass: "sqli", Severity: "high",
		Summary: "id parameter\nis injectable", Assets: []string{"a.example.com", "b.example.com"},
		DetailURL: "https://artex.example.com/findings/7",
	}
	got, kept := signalText(Message{Items: []Item{it}})
	want := "[🟠 High] SQL injection\n" +
		"Type: sqli\n" +
		"Assets: a.example.com, b.example.com\n" +
		"Summary: id parameter is injectable\n" +
		"View details: https://artex.example.com/findings/7"
	if got != want || kept != 1 {
		t.Fatalf("got (%d)\n%s\nwant\n%s", kept, got, want)
	}

	it.FromStatus, it.ToStatus = "pending", "fixed"
	it.VulnClass, it.DetailURL, it.Assets, it.Summary = "", "", nil, ""
	got, _ = signalText(Message{Items: []Item{it}})
	want = "[🟠 High] SQL injection\nStatus change: Pending → Fixed"
	if got != want {
		t.Fatalf("status change: got\n%s\nwant\n%s", got, want)
	}
}

func TestSignalTextDigestGolden(t *testing.T) {
	m := Message{
		Batch: true, WindowMinutes: 15, HomeURL: "https://artex.example.com",
		Items: []Item{
			{Name: "SQL injection", Severity: "critical", Assets: []string{"a.example.com"}},
			{Name: "XSS", Severity: "low"},
		},
	}
	got, kept := signalText(m)
	want := "Last 15 min · Vulnerability digest · 2 total\n" +
		"1. 🔴 Critical · SQL injection — a.example.com\n" +
		"2. 🔵 Low · XSS\n\n" +
		"View all on the platform: https://artex.example.com"
	if got != want || kept != 2 {
		t.Fatalf("got (%d)\n%s\nwant\n%s", kept, got, want)
	}
}

func TestSignalTextDigestOverflowKeepsWholeItems(t *testing.T) {
	var items []Item
	for i := 0; i < 60; i++ {
		items = append(items, Item{Name: strings.Repeat("v", 80) + itoa(i), Severity: "high", Assets: []string{"host.example.com"}})
	}
	m := Message{Batch: true, Items: items, HomeURL: "https://artex.example.com"}
	got, kept := signalText(m)
	if kept <= 0 || kept >= len(items) {
		t.Fatalf("kept = %d of %d, want a partial batch", kept, len(items))
	}
	if len(got) > signalTextLimit || !utf8.ValidString(got) {
		t.Fatalf("size %d, valid=%v", len(got), utf8.ValidString(got))
	}
	if !strings.Contains(got, "showing the first") || !strings.Contains(got, "more in the next message") {
		t.Fatalf("title should announce the continuation: %q", strings.SplitN(got, "\n", 2)[0])
	}
	// every kept item is present whole, the first dropped one is absent
	if !strings.Contains(got, itoa(kept-1)+" ") && !strings.Contains(got, strings.Repeat("v", 80)+itoa(kept-1)) {
		t.Fatalf("last kept item missing")
	}
	if strings.Contains(got, strings.Repeat("v", 80)+itoa(kept)+" ") || strings.Contains(got, strings.Repeat("v", 80)+itoa(kept)+"\n") {
		t.Fatalf("an item beyond kept was rendered")
	}
}

func TestSignalTextSingleOverlongItemTruncatedAsUTF8(t *testing.T) {
	// multi-byte runes: a byte cut would split one
	it := Item{Name: strings.Repeat("漏洞", 1500), Severity: "high", DetailURL: "https://artex.example.com/f/1"}
	got, kept := signalText(Message{Items: []Item{it}})
	if kept != 1 || len(got) > signalTextLimit || !utf8.ValidString(got) {
		t.Fatalf("kept=%d len=%d valid=%v", kept, len(got), utf8.ValidString(got))
	}
	// a single huge digest item still goes out (kept 1), truncated
	got, kept = signalText(Message{Batch: true, Items: []Item{it, it}})
	if kept != 1 || len(got) > signalTextLimit || !utf8.ValidString(got) {
		t.Fatalf("digest: kept=%d len=%d valid=%v", kept, len(got), utf8.ValidString(got))
	}
}

func TestSignalTextNewlineInTitleCannotForgeALine(t *testing.T) {
	m := Message{Batch: true, Items: []Item{{Name: "bad\n2. 🔴 Critical · forged", Severity: "low"}}}
	got, _ := signalText(m)
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("digest item must stay on one line: %q", got)
	}
}

func TestSignalSendReturnsKeptOnOverflow(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)
	var items []Item
	for i := 0; i < 60; i++ {
		items = append(items, Item{Name: strings.Repeat("v", 80) + itoa(i), Severity: "high"})
	}
	kept, err := sendGroup(t, groupCfg(), Message{Batch: true, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if kept <= 0 || kept >= len(items) {
		t.Fatalf("kept = %d of %d", kept, len(items))
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(br.requests()[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Message) > signalTextLimit {
		t.Fatalf("message is %d bytes", len(body.Message))
	}
}

func TestSignalSendEmptyMessageIsPermanent(t *testing.T) {
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), Message{})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("got %v", err)
	}
	if len(br.requests()) != 0 {
		t.Fatal("an empty message must not reach the bridge")
	}
}

// ---- masking helpers with an empty key set ----

func TestSignalConfigIsNotMaskedAndUpdatesMerge(t *testing.T) {
	stored := map[string]any{"recipient_type": "group", "recipients": []any{sigGroup}}
	masked := MaskConfig(KindSignal, stored)
	if !reflect.DeepEqual(masked, stored) {
		t.Fatalf("nothing in a Signal config is secret, got %v", masked)
	}
	merged, err := PrepareConfigUpdate(KindSignal, stored, map[string]any{"recipients": []any{sigRecipA}, "recipient_type": "numbers"})
	if err != nil {
		t.Fatalf("a recipient change must not demand re-entering anything: %v", err)
	}
	if !reflect.DeepEqual(merged["recipients"], []any{sigRecipA}) || merged["recipient_type"] != "numbers" {
		t.Fatalf("merged = %v", merged)
	}
}

// ---- isolation re-check hook (C12) ----

// sigLogBuf is a log sink that the send goroutine and the test may touch at the same time.
type sigLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *sigLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *sigLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// resetSignalRecheck gives a test a clean hook and once-flag, and restores them after it.
func resetSignalRecheck(t *testing.T) {
	t.Helper()
	signalSentOnce.Store(false)
	SetSignalRecheck(nil)
	t.Cleanup(func() { SetSignalRecheck(nil); signalSentOnce.Store(false) })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The hook runs once, after the first successful send, with a bounded context that outlives the
// caller's (the send must not wait for it, and a cancelled request must not cancel it).
func TestSignalRecheckHookRunsOnceAfterFirstSuccess(t *testing.T) {
	resetSignalRecheck(t)
	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)

	var calls atomic.Int32
	type seen struct {
		remaining time.Duration
		hasDL     bool
		err       error
	}
	got := make(chan seen, 4)
	SetSignalRecheck(func(ctx context.Context) error {
		calls.Add(1)
		dl, ok := ctx.Deadline()
		got <- seen{time.Until(dl), ok, ctx.Err()}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := (signalChannel{}).Send(ctx, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	cancel() // the request is over; the re-check goes on
	select {
	case s := <-got:
		if !s.hasDL || s.remaining <= time.Minute || s.remaining > 2*time.Minute {
			t.Errorf("hook deadline = %v (set %v), want within 2 minutes", s.remaining, s.hasDL)
		}
		if s.err != nil {
			t.Errorf("hook context already done: %v", s.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook was not called")
	}
	for range 2 {
		if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond) // a wrong second call would have started by now
	if n := calls.Load(); n != 1 {
		t.Fatalf("hook called %d times, want 1", n)
	}
}

// A hook that fails (here: the check was skipped) is logged and tried again after the next
// successful send; a failed send does not call it at all.
func TestSignalRecheckHookFailureIsLoggedAndRetried(t *testing.T) {
	resetSignalRecheck(t)
	sink := &sigLogBuf{}
	old := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(old) })

	br := newFakeBridge(t, 201, "application/json", `[{"timestamp":"1"}]`)
	useSignal(t, br.sock, sigSender)

	var calls atomic.Int32
	SetSignalRecheck(func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("lock not obtained")
		}
		return nil
	})
	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the skipped line", func() bool {
		return strings.Contains(sink.String(), "[notify] signal: isolation recheck skipped: lock not obtained")
	})
	waitFor(t, "the once-flag to reset", func() bool { return !signalSentOnce.Load() })

	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the second call", func() bool { return calls.Load() == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("hook called %d times, want 2", n)
	}
	// it succeeded the second time: no further call
	if _, err := sendGroup(t, groupCfg(), sigMsg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("hook called %d times after success, want 2", n)
	}
}

func TestSignalRecheckHookNotCalledOnFailedSend(t *testing.T) {
	resetSignalRecheck(t)
	br := newFakeBridge(t, 400, "application/json", `{"error":"no"}`)
	useSignal(t, br.sock, sigSender)
	var calls atomic.Int32
	SetSignalRecheck(func(context.Context) error { calls.Add(1); return nil })
	if _, err := sendGroup(t, groupCfg(), sigMsg); err == nil {
		t.Fatal("want an error")
	}
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 || signalSentOnce.Load() {
		t.Fatalf("calls=%d once=%v after a failed send", calls.Load(), signalSentOnce.Load())
	}
}

func TestRecheckSignalWithoutHookIsNil(t *testing.T) {
	resetSignalRecheck(t)
	if err := RecheckSignal(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := errors.New("boom")
	SetSignalRecheck(func(context.Context) error { return want })
	if err := RecheckSignal(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the hook's error", err)
	}
}

func TestSignalRateLimitTextNamesNoBridge(t *testing.T) {
	br := newFakeBridge(t, 429, "application/json", `{"error":"Rate Limit Reached","challenge_tokens":["CHALLENGE-TOKEN-XYZ"]}`)
	useSignal(t, br.sock, sigSender)
	_, err := sendGroup(t, groupCfg(), sigMsg)
	const want = "Signal rate limit: the account is rate limited by Signal (see the deployment README); delivery is retried"
	if err == nil || err.Error() != want || IsPermanent(err) {
		t.Fatalf("error = %v (permanent=%v), want retryable %q", err, err != nil && IsPermanent(err), want)
	}
}

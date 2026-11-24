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
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	apiNumber = "+15555550101"
	apiPeer   = "+15555550102"
	apiGroup  = "group.QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5YWJjZGVmZ2hpamtsbW5vcA=="
)

// fakeBackend is a scripted Backend that records what the handler asked of it.
type fakeBackend struct {
	mu       sync.Mutex
	account  string
	refusal  Refusal
	result   SendResult
	sendErr  error
	about    About
	readies  int
	sends    int
	abouts   int
	lastAcc  string
	lastTo   SendTarget
	lastText string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		account: apiNumber,
		result:  SendResult{Timestamp: 1700000000000, Results: []SendRecipientResult{{Type: "SUCCESS"}}},
		about:   About{Service: "artex-signal", Version: "1.2.3", SignalCLI: "0.14.10", State: "linked"},
	}
}

func (f *fakeBackend) Ready(context.Context) (string, Refusal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readies++
	return f.account, f.refusal
}

func (f *fakeBackend) Send(_ context.Context, account string, to SendTarget, message string) (SendResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends++
	f.lastAcc, f.lastTo, f.lastText = account, to, message
	return f.result, f.sendErr
}

func (f *fakeBackend) About(context.Context) About {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abouts++
	return f.about
}

func (f *fakeBackend) calls() (readies, sends, abouts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readies, f.sends, f.abouts
}

type apiLogSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *apiLogSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *apiLogSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// privateDir makes a 0700 directory for a socket.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "s")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// apiRig serves NewAPI over a real Unix socket.
type apiRig struct {
	b    *fakeBackend
	log  *apiLogSink
	http *http.Client
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	b := newFakeBackend()
	sink := &apiLogSink{}
	sock := filepath.Join(privateDir(t), "api.sock")
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewAPI(b, sink.logf), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	t.Cleanup(func() { tr.CloseIdleConnections(); srv.Close() })
	return &apiRig{b: b, log: sink, http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

func (r *apiRig) do(t *testing.T, method, path string, body io.Reader) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://sidecar"+path, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, raw
}

func (r *apiRig) send(t *testing.T, payload any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := r.do(t, "POST", "/v2/send", bytes.NewReader(raw))
	return code, body
}

func sendBody(recipients ...string) map[string]any {
	return map[string]any{"number": apiNumber, "recipients": recipients, "message": "hello", "text_mode": "normal"}
}

func errorText(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error == "" {
		t.Fatalf("not an error body: %q (%v)", body, err)
	}
	return e.Error
}

func TestSendToGroupSucceeds(t *testing.T) {
	r := newAPIRig(t)
	code, body := r.send(t, sendBody(apiGroup))
	if code != 201 || string(body) != `{"timestamp":"1700000000000"}` {
		t.Fatalf("%d %s", code, body)
	}
	if r.b.lastAcc != apiNumber || r.b.lastTo.GroupID != apiGroup || r.b.lastTo.Numbers != nil || r.b.lastText != "hello" {
		t.Fatalf("backend got %q %+v %q", r.b.lastAcc, r.b.lastTo, r.b.lastText)
	}
}

func TestSendToNumbersSucceeds(t *testing.T) {
	r := newAPIRig(t)
	code, body := r.send(t, sendBody(apiPeer, "+15555550100"))
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	if r.b.lastTo.GroupID != "" || strings.Join(r.b.lastTo.Numbers, ",") != apiPeer+",+15555550100" {
		t.Fatalf("target %+v", r.b.lastTo)
	}
}

func TestPartialDeliveryIsListedAndStillCreated(t *testing.T) {
	r := newAPIRig(t)
	r.b.result = SendResult{Timestamp: 5, Results: []SendRecipientResult{
		{Type: "SUCCESS", Address: RecipientAddress{Number: apiPeer}},
		{Type: "UNREGISTERED_FAILURE", Address: RecipientAddress{Number: "+15555550100"}},
		{Type: "NETWORK_FAILURE", Address: RecipientAddress{UUID: "11111111-2222-4333-8444-555555555555"}},
	}}
	code, body := r.send(t, sendBody(apiPeer, "+15555550100", "+15555550101"))
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	// The shape the ARTEX notifier parses.
	var got struct {
		Timestamp string `json:"timestamp"`
		Errors    struct {
			Recipients []struct {
				Number string `json:"number"`
				UUID   string `json:"uuid"`
				Reason string `json:"reason"`
			} `json:"recipients"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Timestamp != "5" || len(got.Errors.Recipients) != 2 {
		t.Fatalf("%s", body)
	}
	a, b := got.Errors.Recipients[0], got.Errors.Recipients[1]
	if a.Reason != "unregistered failure" || a.Number != "+15555550100" || a.UUID != "" ||
		b.Reason != "network failure" || b.UUID != "11111111-2222-4333-8444-555555555555" || b.Number != "" {
		t.Fatalf("%s", body)
	}
	if strings.Contains(r.log.all(), "5555550100") || strings.Contains(r.log.all(), "11111111") {
		t.Fatalf("address in log: %s", r.log.all())
	}
}

func TestFullDeliveryHasNoErrorsMember(t *testing.T) {
	r := newAPIRig(t)
	_, body := r.send(t, sendBody(apiGroup))
	if strings.Contains(string(body), "errors") {
		t.Fatalf("%s", body)
	}
}

func TestBackendErrorsMapToStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"rate", &RPCError{Code: -5, Message: "limited"}, 429},
		{"unexpected", &RPCError{Code: -32603, Message: "reset"}, 503},
		{"io", &RPCError{Code: -3, Message: "io"}, 503},
		{"untrusted", &RPCError{Code: -4, Message: "untrusted identity"}, 400},
		{"user", &RPCError{Code: -1, Message: "Unknown group"}, 400},
		{"account", &RPCError{Code: -32602, Message: "no account"}, 400},
		{"othercode", &RPCError{Code: -32600, Message: "bad params"}, 400},
		{"childdown", ErrChildDown, 503},
		{"closed", fmt.Errorf("x: %w", ErrClosed), 503},
		{"linking", ErrLinkPending, 503},
		{"deadline", context.DeadlineExceeded, 503},
		{"plain", errors.New("boom"), 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newAPIRig(t)
			r.b.sendErr = tc.err
			code, body := r.send(t, sendBody(apiGroup))
			if code != tc.code {
				t.Fatalf("%d %s, want %d", code, body, tc.code)
			}
			errorText(t, body)
		})
	}
}

func TestRateLimitBodyHasNoChallengeData(t *testing.T) {
	r := newAPIRig(t)
	r.b.sendErr = &RPCError{Code: -5, Message: "limited, token CHALLENGE-SENTINEL",
		Data: json.RawMessage(`{"response":{"results":[{"type":"RATE_LIMIT_FAILURE","token":"CHALLENGE-SENTINEL"}]}}`)}
	code, body := r.send(t, sendBody(apiGroup))
	if code != 429 || string(body) != `{"error":"rate limited"}` {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(r.log.all(), "CHALLENGE-SENTINEL") {
		t.Fatalf("log: %s", r.log.all())
	}
}

func TestErrorTextsAreScrubbedInBodiesAndLogs(t *testing.T) {
	leaks := "failed for +15555550107 in group.QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5YWJjZGVmZ2hpamtsbW5vcA== " +
		"id 11111111-2222-4333-8444-555555555555 via sgnl://linkdevice?uuid=SECRETPARAM&pub_key=abc"
	secrets := []string{"+15555550107", "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5YWJjZGVmZ2hpamtsbW5vcA", "11111111-2222-4333-8444-555555555555", "SECRETPARAM", "linkdevice"}
	for _, code := range []int{-1, -3, -4, -32602, -32603} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := newAPIRig(t)
			r.b.sendErr = &RPCError{Code: code, Message: leaks}
			status, body := r.send(t, sendBody(apiGroup))
			for _, s := range secrets {
				if strings.Contains(string(body), s) || strings.Contains(r.log.all(), s) {
					t.Fatalf("%q leaked (status %d): %s | %s", s, status, body, r.log.all())
				}
			}
			if !strings.Contains(errorText(t, body), "…") {
				t.Fatalf("expected masked text: %s", body)
			}
		})
	}
	t.Run("plain", func(t *testing.T) {
		r := newAPIRig(t)
		r.b.sendErr = errors.New(leaks)
		_, body := r.send(t, sendBody(apiGroup))
		for _, s := range secrets {
			if strings.Contains(string(body), s) || strings.Contains(r.log.all(), s) {
				t.Fatalf("%q leaked: %s | %s", s, body, r.log.all())
			}
		}
	})
}

func TestMessageTextIsNeverLogged(t *testing.T) {
	r := newAPIRig(t)
	r.b.sendErr = &RPCError{Code: -1, Message: "bad"}
	p := sendBody(apiGroup)
	p["message"] = "MESSAGE-SENTINEL"
	r.send(t, p)
	p["text_mode"] = "markdown"
	r.send(t, p)
	if strings.Contains(r.log.all(), "MESSAGE-SENTINEL") || strings.Contains(r.log.all(), apiNumber) {
		t.Fatalf("log: %s", r.log.all())
	}
}

func TestRefusalsMapToStatusesAndNothingIsSent(t *testing.T) {
	cases := []struct {
		refusal Refusal
		code    int
	}{
		{RefuseNotLinked, 400},
		{RefuseNotConfirmed, 400},
		{RefuseConfigStale, 503},
		{RefuseChildDown, 503},
		{Refusal("something_new"), 503},
	}
	for _, tc := range cases {
		t.Run(string(tc.refusal), func(t *testing.T) {
			r := newAPIRig(t)
			r.b.refusal = tc.refusal
			if tc.refusal == RefuseNotLinked || tc.refusal == RefuseChildDown {
				r.b.account = ""
			}
			code, body := r.send(t, sendBody(apiGroup))
			if code != tc.code {
				t.Fatalf("%d %s", code, body)
			}
			errorText(t, body)
			if _, sends, _ := r.b.calls(); sends != 0 {
				t.Fatal("a refused send reached the backend")
			}
		})
	}
}

func TestReadyWithoutAccountAndWithoutRefusalIsUnavailable(t *testing.T) {
	r := newAPIRig(t)
	r.b.account = ""
	code, _ := r.send(t, sendBody(apiGroup))
	if code != 503 {
		t.Fatalf("%d", code)
	}
}

func TestBadRequestsAreRefusedWithoutASend(t *testing.T) {
	mod := func(f func(m map[string]any)) map[string]any {
		m := sendBody(apiGroup)
		f(m)
		return m
	}
	cases := []struct {
		name string
		body any
	}{
		{"wrongnumber", mod(func(m map[string]any) { m["number"] = "+15555550109" })},
		{"nonumber", mod(func(m map[string]any) { delete(m, "number") })},
		{"markdown", mod(func(m map[string]any) { m["text_mode"] = "styled" })},
		{"notextmode", mod(func(m map[string]any) { delete(m, "text_mode") })},
		{"nomessage", mod(func(m map[string]any) { m["message"] = "" })},
		{"norecipients", mod(func(m map[string]any) { m["recipients"] = []string{} })},
		{"recipientsnull", mod(func(m map[string]any) { m["recipients"] = nil })},
		{"mixed", sendBody(apiGroup, apiPeer)},
		{"twogroups", sendBody(apiGroup, apiGroup)},
		{"badnumber", sendBody("+0123456789")},
		{"nonumberplus", sendBody("15555550102")},
		{"uuid", sendBody("11111111-2222-4333-8444-555555555555")},
		{"username", sendBody("someone.01")},
		{"emptygroup", sendBody("group.")},
		{"badgroupchars", sendBody("group.abc def")},
		{"extrafield", mod(func(m map[string]any) { m["base64_attachments"] = []string{"AAAA"} })},
		{"recipientsstring", mod(func(m map[string]any) { m["recipients"] = apiPeer })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newAPIRig(t)
			code, body := r.send(t, tc.body)
			if code != 400 {
				t.Fatalf("%d %s", code, body)
			}
			text := errorText(t, body)
			for _, s := range []string{apiNumber, apiPeer, "15555550109", "someone"} {
				if strings.Contains(text, s) {
					t.Fatalf("error echoes %q: %s", s, text)
				}
			}
			if _, sends, _ := r.b.calls(); sends != 0 {
				t.Fatal("a bad request reached the backend send")
			}
		})
	}
}

func TestMalformedBodiesAreRefused(t *testing.T) {
	for name, body := range map[string]string{
		"empty":    "",
		"notjson":  "not json",
		"array":    "[]",
		"trailing": `{"number":"` + apiNumber + `","recipients":["` + apiGroup + `"],"message":"m","text_mode":"normal"} {}`,
		"badtype":  `{"number":5}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newAPIRig(t)
			code, _, raw := r.do(t, "POST", "/v2/send", strings.NewReader(body))
			if code != 400 {
				t.Fatalf("%d %s", code, raw)
			}
			errorText(t, raw)
			if readies, sends, _ := r.b.calls(); sends != 0 || readies != 0 {
				t.Fatalf("backend called: %d %d", readies, sends)
			}
		})
	}
}

// bodyOfSize builds a valid send body of exactly n bytes.
func bodyOfSize(t *testing.T, n int) []byte {
	t.Helper()
	p := sendBody(apiGroup)
	p["message"] = ""
	base, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p["message"] = strings.Repeat("a", n-len(base))
	out, err := json.Marshal(p)
	if err != nil || len(out) != n {
		t.Fatalf("size %d, want %d (%v)", len(out), n, err)
	}
	return out
}

func TestBodyLimitIs64KiB(t *testing.T) {
	r := newAPIRig(t)
	code, _, raw := r.do(t, "POST", "/v2/send", bytes.NewReader(bodyOfSize(t, 64<<10)))
	if code != 201 {
		t.Fatalf("at the limit: %d %s", code, raw)
	}
	_, sendsBefore, _ := r.b.calls()
	code, _, raw = r.do(t, "POST", "/v2/send", bytes.NewReader(bodyOfSize(t, 64<<10+1)))
	if code != 413 {
		t.Fatalf("one byte over: %d %s", code, raw)
	}
	errorText(t, raw)
	// A body of unknown length, far over the limit.
	code, _, raw = r.do(t, "POST", "/v2/send", io.MultiReader(strings.NewReader(`{"message":"`), bytes.NewReader(bytes.Repeat([]byte("a"), 1<<20))))
	if code != 413 {
		t.Fatalf("streamed: %d %s", code, raw)
	}
	if _, sends, _ := r.b.calls(); sends != sendsBefore {
		t.Fatal("an oversize body reached the backend")
	}
}

func TestOtherPathsAre404WithEmptyBodyAndNoBackendCall(t *testing.T) {
	r := newAPIRig(t)
	paths := []string{"/", "/v1/devices/" + apiNumber, "/v1/qrcodelink", "/v1/register/" + apiNumber, "/v1/groups/" + apiNumber,
		"/v2/send/", "/v1/about/", "/V2/send", "/v2/sendx", "/v1/accounts", "//v2/send", "/v2/send/../v1/about"}
	for _, p := range paths {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
			code, _, body := r.do(t, m, p, strings.NewReader("x"))
			if code != 404 || len(body) != 0 {
				t.Fatalf("%s %s: %d %q", m, p, code, body)
			}
		}
	}
	if a, b, c := r.b.calls(); a+b+c != 0 {
		t.Fatalf("backend called %d %d %d times", a, b, c)
	}
}

func TestWrongMethodsAre405WithoutBackendCall(t *testing.T) {
	r := newAPIRig(t)
	for path, allowed := range map[string]string{"/v2/send": "POST", "/v1/about": "GET"} {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
			if m == allowed {
				continue
			}
			code, h, body := r.do(t, m, path, strings.NewReader("{}"))
			if code != 405 || len(body) != 0 || h.Get("Allow") != allowed {
				t.Fatalf("%s %s: %d allow=%q %q", m, path, code, h.Get("Allow"), body)
			}
		}
	}
	if a, b, c := r.b.calls(); a+b+c != 0 {
		t.Fatalf("backend called %d %d %d times", a, b, c)
	}
}

func TestAboutReportsTheServiceOnly(t *testing.T) {
	r := newAPIRig(t)
	r.b.refusal = RefuseNotLinked // About does not depend on readiness
	code, h, body := r.do(t, "GET", "/v1/about", nil)
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %v", code, h)
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"service": "artex-signal", "version": "1.2.3", "signal_cli": "0.14.10", "state": "linked"}
	if len(m) != len(want) {
		t.Fatalf("%s", body)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("%s: %s", k, body)
		}
	}
	if strings.Contains(string(body), "155555") {
		t.Fatalf("account data in %s", body)
	}
	if readies, _, abouts := r.b.calls(); readies != 0 || abouts != 1 {
		t.Fatalf("readies %d abouts %d", readies, abouts)
	}
}

func TestNilLogfIsAccepted(t *testing.T) {
	b := newFakeBackend()
	b.sendErr = errors.New("boom")
	h := NewAPI(b, nil)
	rec := &respRecorder{h: http.Header{}}
	req, _ := http.NewRequest("POST", "/v2/send", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)
	if rec.code != 400 {
		t.Fatalf("%d", rec.code)
	}
}

type respRecorder struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (r *respRecorder) Header() http.Header         { return r.h }
func (r *respRecorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *respRecorder) WriteHeader(c int)           { r.code = c }

func TestListenSocketHasMode0600(t *testing.T) {
	sock := filepath.Join(privateDir(t), "api.sock")
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestListenSocketRemovesAStaleSocketFile(t *testing.T) {
	sock := filepath.Join(privateDir(t), "api.sock")
	first, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Close() // the file stays behind, as after a crash
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("no stale file: %v", err)
	}
	ln, err := ListenSocket(sock)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	defer ln.Close()
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestListenSocketRefusesALiveSocket(t *testing.T) {
	sock := filepath.Join(privateDir(t), "api.sock")
	live, err := ListenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := ListenSocket(sock); err == nil {
		t.Fatal("a second listener took over a live socket")
	}
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("the live socket was removed: %v", err)
	}
	c.Close()
}

func TestListenSocketLeavesOtherFilesAlone(t *testing.T) {
	dir := privateDir(t)
	sock := filepath.Join(dir, "api.sock")
	if err := os.WriteFile(sock, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenSocket(sock); err == nil {
		t.Fatal("a regular file was replaced")
	}
	if b, err := os.ReadFile(sock); err != nil || string(b) != "keep" {
		t.Fatalf("file changed: %q %v", b, err)
	}
}

func TestListenSocketRefusesAnUnsafeParentDirectory(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o750, 0o770, 0o701, 0o777, 0o500, 0o600} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "d")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			ln, err := ListenSocket(filepath.Join(dir, "api.sock"))
			if err == nil {
				ln.Close()
				t.Fatal("accepted")
			}
		})
	}
}

func TestListenSocketRefusesAMissingOrNonDirectoryParent(t *testing.T) {
	base := t.TempDir()
	if _, err := ListenSocket(filepath.Join(base, "nodir", "api.sock")); err == nil {
		t.Fatal("missing parent accepted")
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenSocket(filepath.Join(file, "api.sock")); err == nil {
		t.Fatal("file as parent accepted")
	}
}

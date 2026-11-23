package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
)

// Phase D.1 / PR 1 network red tests for the custom http tool (D2, AC12). Everything stays
// on loopback: lab targets are httptest servers, names are resolved by an injected
// resolver, and "outside" destinations are reached only through a stand-in proxy that
// records and never forwards.

const (
	credCodeOriginMismatch  = "ORIGIN_MISMATCH"
	credCodeTemplatedOrigin = "TEMPLATED_ORIGIN_NOT_ALLOWED"
	credCodeBlocked         = "TARGET_BLOCKED"
	credCodeControlPlane    = "TARGET_CONTROL_PLANE"
)

var operatorHeaders = map[string]string{"X-Api-Key": "op-key", "Authorization": "Bearer op-token", "Cookie": "op=1"}

type credHit struct {
	Method string
	URI    string
	Host   string
	Header http.Header
	Body   string
}

type credLab struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  []credHit
	conns atomic.Int64
	port  string
}

type credCountingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l credCountingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

func newCredLab(t *testing.T, addr string, useTLS bool) *credLab {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("lab listen %s: %v", addr, err)
	}
	lab := &credLab{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(lab.serve))
	srv.Listener = credCountingListener{ln, &lab.conns}
	if useTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	lab.srv = srv
	_, lab.port, _ = net.SplitHostPort(srv.Listener.Addr().String())
	return lab
}

func (l *credLab) URL() string { return l.srv.URL }

func (l *credLab) Host() string {
	return strings.TrimPrefix(strings.TrimPrefix(l.srv.URL, "http://"), "https://")
}

func (l *credLab) Hits() []credHit {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]credHit(nil), l.hits...)
}

func (l *credLab) Reset() {
	l.mu.Lock()
	l.hits = nil
	l.mu.Unlock()
}

func (l *credLab) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	l.mu.Lock()
	l.hits = append(l.hits, credHit{r.Method, r.URL.RequestURI(), r.Host, r.Header.Clone(), string(body)})
	l.mu.Unlock()
	switch {
	case r.URL.Path == "/redir":
		code := http.StatusFound
		if c, err := strconv.Atoi(r.URL.Query().Get("code")); err == nil {
			code = c
		}
		http.Redirect(w, r, r.URL.Query().Get("to"), code)
	case strings.HasPrefix(r.URL.Path, "/chain/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/chain/"))
		if n > 0 {
			http.Redirect(w, r, "/chain/"+strconv.Itoa(n-1), http.StatusFound)
			return
		}
		fmt.Fprint(w, "chain-end")
	default: // /echo and everything else
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"method": r.Method, "header": r.Header, "body": string(body)})
	}
}

// credProxy is a stand-in http proxy: it records every request and answers "PROXIED";
// it never opens an outbound connection.
type credProxy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []credHit
}

func newCredProxy(t *testing.T) *credProxy {
	t.Helper()
	p := &credProxy{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, credHit{r.Method, r.URL.String(), r.URL.Host, r.Header.Clone(), ""})
		p.mu.Unlock()
		if r.Method == http.MethodConnect {
			http.Error(w, "stand-in proxy does not tunnel", http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "PROXIED")
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *credProxy) URL() string { return p.srv.URL }

func (p *credProxy) Seen() []credHit {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]credHit(nil), p.seen...)
}

// SawHostname reports whether any recorded request was addressed to hostname.
func (p *credProxy) SawHostname(hostname string) bool {
	for _, s := range p.Seen() {
		if u, err := url.Parse(s.URI); err == nil && u.Hostname() == hostname {
			return true
		}
	}
	return false
}

// ---------- policy fixtures ----------

type credResolver struct {
	mu    sync.Mutex
	table map[string][]string
	seq   map[string][][]string
	calls map[string]int
}

func newCredResolver(table map[string][]string) *credResolver {
	return &credResolver{table: table, seq: map[string][][]string{}, calls: map[string]int{}}
}

func (r *credResolver) Resolve(_ context.Context, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.calls[host]
	r.calls[host]++
	answers, ok := r.table[host]
	if s := r.seq[host]; len(s) > 0 {
		answers, ok = s[min(n, len(s)-1)], true
	}
	if !ok {
		return nil, fmt.Errorf("fake resolver: no such host %q", host)
	}
	out := make([]netip.Addr, 0, len(answers))
	for _, a := range answers {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

func (r *credResolver) Calls(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[host]
}

func credRule(kind, pattern string) db.AssetInterceptRule {
	return db.AssetInterceptRule{Enabled: true, Kind: kind, Pattern: pattern}
}

func credStaticRules(block, allow []db.AssetInterceptRule) agent.TargetRuleLoader {
	return func(context.Context, int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
		return block, allow, nil
	}
}

func setCredPolicy(t *testing.T, loader agent.TargetRuleLoader, ctlPort string, res *credResolver) {
	t.Helper()
	if res == nil {
		res = newCredResolver(map[string][]string{"localhost": {"127.0.0.1"}, "api.vendor.test": {"203.0.113.10"}})
	}
	agent.SetTargetPolicy(loader, agent.ControlPlane{DBCaptured: true,
		ListenAddr:     ":" + ctlPort,
		Resolver:       res.Resolve,
		InterfaceAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.77.0.5")}, nil },
	}, false)
	t.Cleanup(func() { agent.SetTargetPolicy(nil, agent.ControlPlane{}, false) })
}

var credRI = agent.RunInfo{TaskID: 7, ExplorationID: 1}

type toolOut struct {
	Text    string
	IsError bool
}

func callHTTPTool(t *testing.T, ri agent.RunInfo, spec map[string]any, params map[string]any) toolOut {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&Server{}).runHTTPTool(agent.WithRunInfo(t.Context(), ri), raw, params, nil)
	if err != nil {
		t.Fatalf("runHTTPTool returned a Go error: %v", err)
	}
	return toolOut{Text: res.Flatten(), IsError: res.IsError}
}

func credTrunc(s string) string {
	if len(s) > 240 {
		return s[:240] + "..."
	}
	return s
}

func requireRefusal(t *testing.T, got toolOut, code string) {
	t.Helper()
	if !got.IsError || !strings.Contains(got.Text, code) {
		t.Fatalf("want a refusal carrying %s, got is_error=%v text=%q", code, got.IsError, credTrunc(got.Text))
	}
}

func requireRan(t *testing.T, got toolOut) {
	t.Helper()
	if got.IsError {
		t.Fatalf("want the request to run, got error %q", credTrunc(got.Text))
	}
}

func requireNoHits(t *testing.T, what string, lab *credLab) {
	t.Helper()
	if hits := lab.Hits(); len(hits) != 0 {
		t.Fatalf("%s received %d request(s), want none; first: %s %s headers=%v", what, len(hits), hits[0].Method, hits[0].URI, hits[0].Header)
	}
}

func operatorHeaderNames(h http.Header) []string {
	var found []string
	for k := range operatorHeaders {
		if h.Get(k) != "" {
			found = append(found, k)
		}
	}
	return found
}

func httpSpec(urlTemplate string, headers map[string]string) map[string]any {
	spec := map[string]any{"method": "GET", "url": urlTemplate}
	if headers != nil {
		spec["headers"] = headers
	}
	return spec
}

// T-CRED-1 (AC12, C10): a template whose authority has no placeholder is pinned to that
// origin; a rendered value that moves the request elsewhere is refused on the FIRST
// request, before any operator header leaves.
func TestCustomHTTPOriginPinned(t *testing.T) {
	t.Run("userinfo smuggles another host, direct", func(t *testing.T) {
		a := newCredLab(t, "127.0.0.1:0", false)
		b := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
		got := callHTTPTool(t, credRI, httpSpec("http://127.0.0.1:"+a.port+"{path}", operatorHeaders), map[string]any{"path": "@127.0.0.2:" + b.port + "/echo"})
		requireRefusal(t, got, credCodeOriginMismatch)
		requireNoHits(t, "the attacker lab", b)
		requireNoHits(t, "the template's own lab", a)
	})
	t.Run("host, userinfo and port variations, via proxy", func(t *testing.T) {
		px := newCredProxy(t)
		setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
		spec := httpSpec("http://api.vendor.test{path}", operatorHeaders)
		spec["proxy"] = px.URL()
		for _, tc := range []struct{ name, path, leakedHost string }{
			{"userinfo", "@evil.test/x", "evil.test"},
			{"host suffix", ".evil.test/x", "api.vendor.test.evil.test"},
			{"port", ":8080/x", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := len(px.Seen())
				requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{"path": tc.path}), credCodeOriginMismatch)
				if after := px.Seen(); len(after) != before {
					t.Fatalf("a request left for the proxy despite the refusal: %+v", after[before:])
				}
			})
		}
		// The unmodified origin keeps working and carries the operator headers.
		requireRan(t, callHTTPTool(t, credRI, spec, map[string]any{"path": "/ok"}))
		seen := px.Seen()
		if len(seen) == 0 || seen[len(seen)-1].Header.Get("X-Api-Key") != "op-key" {
			t.Fatalf("in-origin request lost its operator headers: %+v", seen)
		}
	})
}

// T-CRED-2 (AC12, C10): cross-origin hops (exact scheme+host+port) drop every operator
// header plus Authorization and Cookie; same-origin hops keep them.
func TestCustomHTTPCrossOriginStripsHeaders(t *testing.T) {
	a := newCredLab(t, "127.0.0.1:0", false)
	a2 := newCredLab(t, "127.0.0.1:0", false) // same host, other port
	b := newCredLab(t, "127.0.0.2:0", false)  // other host
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	run := func(to string) toolOut {
		return callHTTPTool(t, credRI, httpSpec(a.URL()+"/redir?to={to}", operatorHeaders), map[string]any{"to": to})
	}
	t.Run("same origin keeps operator headers", func(t *testing.T) {
		a.Reset()
		requireRan(t, run(a.URL()+"/echo"))
		hits := a.Hits()
		if len(hits) != 2 || len(operatorHeaderNames(hits[1].Header)) != len(operatorHeaders) {
			t.Fatalf("same-origin hop lost operator headers: %+v", hits)
		}
	})
	for _, tc := range []struct {
		name   string
		target *credLab
	}{{"other host", b}, {"other port", a2}} {
		t.Run(tc.name+" strips operator headers", func(t *testing.T) {
			tc.target.Reset()
			requireRan(t, run(tc.target.URL()+"/echo"))
			hits := tc.target.Hits()
			if len(hits) != 1 {
				t.Fatalf("redirect target received %d requests, want 1", len(hits))
			}
			if leaked := operatorHeaderNames(hits[0].Header); len(leaked) != 0 {
				t.Fatalf("operator headers followed a cross-origin redirect: %v", leaked)
			}
		})
	}
	t.Run("https to http strips operator headers", func(t *testing.T) {
		// A trusted https lab needs the system trust store, which Go reads once per
		// process: run the scenario in a fresh copy of the test binary.
		tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw})
		tlsSrv.Close()
		certFile := t.TempDir() + "/lab-ca.pem"
		if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCustomHTTPSchemeChild$", "-test.count=1", "-test.v")
		cmd.Env = append(credFilterEnv(os.Environ(), "ARTEX_PG_DSN", "ARTEX_CONFIG", "SSL_CERT_FILE", "SSL_CERT_DIR"),
			"ARTEX_LAB_CHILD=scheme", "ARTEX_CONFIG=/nonexistent/artex.json", "SSL_CERT_FILE="+certFile)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child (https lab -> http lab) failed: %v\n%s", err, credTail(string(out), 700))
		}
	})
}

// TestCustomHTTPSchemeChild is the body of the https->http case; it only runs when the
// parent re-executes the test binary with the lab certificate trusted.
func TestCustomHTTPSchemeChild(t *testing.T) {
	if os.Getenv("ARTEX_LAB_CHILD") != "scheme" {
		t.Skip("helper process for TestCustomHTTPCrossOriginStripsHeaders/https_to_http")
	}
	secure := newCredLab(t, "127.0.0.1:0", true)
	plain := newCredLab(t, "127.0.0.1:0", false) // same host as the https lab, different scheme and port
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	got := callHTTPTool(t, credRI, httpSpec(secure.URL()+"/redir?to={to}", operatorHeaders), map[string]any{"to": plain.URL() + "/echo"})
	requireRan(t, got)
	first := secure.Hits()
	if len(first) != 1 || len(operatorHeaderNames(first[0].Header)) != len(operatorHeaders) {
		t.Fatalf("fixture: the https lab did not receive the operator headers on the first request: %+v", first)
	}
	hits := plain.Hits()
	if len(hits) != 1 {
		t.Fatalf("the http lab received %d requests, want 1", len(hits))
	}
	if leaked := operatorHeaderNames(hits[0].Header); len(leaked) != 0 {
		t.Fatalf("operator headers followed an https->http redirect: %v", leaked)
	}
}

func credFilterEnv(env []string, drop ...string) []string {
	var out []string
next:
	for _, kv := range env {
		for _, d := range drop {
			if strings.HasPrefix(kv, d+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

func credTail(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// T-CRED-3 (AC12, C10): a cross-origin 307/308 would replay the request body to the new
// origin and is refused; a same-origin one still replays.
func TestCustomHTTP307BodyRefused(t *testing.T) {
	a := newCredLab(t, "127.0.0.1:0", false)
	b := newCredLab(t, "127.0.0.2:0", false)
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	spec := map[string]any{"method": "POST", "url": a.URL() + "/redir?code={code}&to={to}", "body": `{"secret":"{value}"}`}
	for _, code := range []string{"307", "308"} {
		t.Run(code+" cross-origin", func(t *testing.T) {
			b.Reset()
			got := callHTTPTool(t, credRI, spec, map[string]any{"code": code, "to": b.URL() + "/echo", "value": "s3cret"})
			if !got.IsError {
				t.Fatalf("a cross-origin %s replay with a body was followed: %q", code, credTrunc(got.Text))
			}
			if !strings.Contains(got.Text, "REDIRECT_BODY_REFUSED") {
				t.Fatalf("want the refusal code REDIRECT_BODY_REFUSED, got %q", credTrunc(got.Text))
			}
			requireNoHits(t, "the cross-origin target", b)
		})
	}
	t.Run("307 same-origin replays the body", func(t *testing.T) {
		a.Reset()
		requireRan(t, callHTTPTool(t, credRI, spec, map[string]any{"code": "307", "to": a.URL() + "/echo", "value": "s3cret"}))
		hits := a.Hits()
		if len(hits) != 2 || hits[1].Method != "POST" || hits[1].Body != `{"secret":"s3cret"}` {
			t.Fatalf("same-origin 307 did not replay the POST body: %+v", hits)
		}
	})
}

// T-CRED-4 (AC12, C8/C10): the initial URL and every hop pass D1, and the connection is
// pinned to the checked IP.
func TestCustomHTTPInitialURLPolicy(t *testing.T) {
	t.Run("initial url on a blocked host", func(t *testing.T) {
		b := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules([]db.AssetInterceptRule{credRule("cidr", "127.0.0.2/32")}, nil), "1", nil)
		requireRefusal(t, callHTTPTool(t, credRI, httpSpec(b.URL()+"/echo", nil), map[string]any{}), credCodeBlocked)
		requireNoHits(t, "the blocked lab", b)
	})
	t.Run("redirect hop to a blocked host", func(t *testing.T) {
		a := newCredLab(t, "127.0.0.1:0", false)
		b := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules([]db.AssetInterceptRule{credRule("cidr", "127.0.0.2/32")}, nil), "1", nil)
		requireRefusal(t, callHTTPTool(t, credRI, httpSpec(a.URL()+"/redir?to={to}", nil), map[string]any{"to": b.URL() + "/echo"}), credCodeBlocked)
		requireNoHits(t, "the blocked second hop", b)
	})
	t.Run("artex listener", func(t *testing.T) {
		ctl := newCredLab(t, "127.0.0.1:0", false)
		setCredPolicy(t, credStaticRules(nil, nil), ctl.port, nil)
		requireRefusal(t, callHTTPTool(t, credRI, httpSpec(ctl.URL()+"/echo", nil), map[string]any{}), credCodeControlPlane)
		requireNoHits(t, "the artex stand-in", ctl)
	})
	t.Run("cloud metadata, via proxy", func(t *testing.T) {
		px := newCredProxy(t)
		setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
		spec := httpSpec("http://169.254.169.254/latest/meta-data/", nil)
		spec["proxy"] = px.URL()
		requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{}), credCodeControlPlane)
		if px.SawHostname("169.254.169.254") {
			t.Fatalf("the metadata request was handed to the proxy: %+v", px.Seen())
		}
	})
	t.Run("dial uses the checked ip", func(t *testing.T) {
		// Checked answer 127.0.0.2, later answer (or system resolver) 127.0.0.1, same port:
		// whoever is dialled by name is the trap.
		// The database host is resolved through the same injected resolver. Pin the DSN to an
		// address literal so an ambient DSN on "localhost" (hosted CI) cannot use up an answer.
		t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@10.77.0.9:5432/artex?sslmode=disable")
		pinned := newCredLab(t, "127.0.0.2:0", false)
		trap := newCredLab(t, "127.0.0.1:"+pinned.port, false)
		res := newCredResolver(map[string][]string{})
		res.seq["localhost"] = [][]string{{"127.0.0.2"}, {"127.0.0.1"}}
		setCredPolicy(t, credStaticRules(nil, nil), "1", res)
		got := callHTTPTool(t, credRI, httpSpec("http://localhost:"+pinned.port+"/echo", nil), map[string]any{})
		if n := res.Calls("localhost"); n != 1 {
			t.Fatalf("the policy resolved the name %d times, want exactly once (injected resolver)", n)
		}
		requireRan(t, got)
		if hits := pinned.Hits(); len(hits) != 1 {
			t.Fatalf("checked ip 127.0.0.2 received %d requests, want 1", len(hits))
		}
		if n := trap.conns.Load(); n != 0 {
			t.Fatalf("the dial went to the re-resolved address: %d connections on the trap, want 0", n)
		}
	})
	t.Run("hop cap holds", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.1:0", false)
		chain := lab.URL() + "/chain/20"
		_, baseErr := http.Get(chain)
		if baseErr == nil || !strings.Contains(baseErr.Error(), "stopped after 10 redirects") {
			t.Fatalf("baseline net/http client did not stop after 10 redirects: %v", baseErr)
		}
		baseHits := len(lab.Hits())
		lab.Reset()
		var mu sync.Mutex
		loaderCalls := 0
		loader := func(context.Context, int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
			mu.Lock()
			loaderCalls++
			mu.Unlock()
			return nil, nil, nil
		}
		setCredPolicy(t, loader, "1", nil)
		got := callHTTPTool(t, credRI, httpSpec(chain, nil), map[string]any{})
		if !got.IsError || !strings.Contains(got.Text, "stopped after 10 redirects") {
			t.Fatalf("the tool did not fail like net/http after 10 redirects: is_error=%v %q", got.IsError, credTrunc(got.Text))
		}
		if n := len(lab.Hits()); n != baseHits {
			t.Fatalf("the tool followed %d hops, net/http follows %d", n, baseHits)
		}
		mu.Lock()
		defer mu.Unlock()
		if loaderCalls == 0 {
			t.Fatal("the egress policy never consulted the rule loader: the cap was not exercised with the policy installed")
		}
	})
}

// T-CRED-5 (AC12, C10): a template whose authority holds a placeholder may carry
// operator headers only to a host matched by an enabled TASK allow rule; chat sessions
// (task id 0) have no task allow rules; a header-less template runs under D1 only.
func TestCustomHTTPTemplatedAuthorityHeaders(t *testing.T) {
	b := newCredLab(t, "127.0.0.2:0", false)
	loader := func(_ context.Context, taskID int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
		if taskID == 7 {
			return nil, []db.AssetInterceptRule{credRule("exact_ip", "127.0.0.2")}, nil
		}
		return nil, nil, nil
	}
	setCredPolicy(t, loader, "1", nil)
	params := map[string]any{"host": "127.0.0.2:" + b.port}
	withHeaders := httpSpec("http://{host}/echo", operatorHeaders)
	noHeaders := httpSpec("http://{host}/echo", nil)

	t.Run("no task allow rule: refused", func(t *testing.T) {
		b.Reset()
		requireRefusal(t, callHTTPTool(t, agent.RunInfo{TaskID: 8, ExplorationID: 1}, withHeaders, params), credCodeTemplatedOrigin)
		requireNoHits(t, "the templated host", b)
	})
	t.Run("matching task allow rule: headers sent", func(t *testing.T) {
		b.Reset()
		requireRan(t, callHTTPTool(t, credRI, withHeaders, params))
		hits := b.Hits()
		if len(hits) != 1 || len(operatorHeaderNames(hits[0].Header)) != len(operatorHeaders) {
			t.Fatalf("allowed templated host did not receive the operator headers: %+v", hits)
		}
	})
	t.Run("chat session: always refused", func(t *testing.T) {
		b.Reset()
		requireRefusal(t, callHTTPTool(t, agent.RunInfo{SessionID: "chat-1"}, withHeaders, params), credCodeTemplatedOrigin)
		requireNoHits(t, "the templated host", b)
	})
	t.Run("header-less template runs under D1 only", func(t *testing.T) {
		b.Reset()
		requireRan(t, callHTTPTool(t, agent.RunInfo{SessionID: "chat-1"}, noHeaders, params))
		hits := b.Hits()
		if len(hits) != 1 || len(operatorHeaderNames(hits[0].Header)) != 0 {
			t.Fatalf("header-less templated request did not run cleanly: %+v", hits)
		}
	})
}

// Review round: a cross-origin chain must never get operator headers back. net/http copies
// the original request's headers onto every hop, so the strip has to run on every hop
// that is not the original origin, and for good once any hop has left it.
func TestCustomHTTPChainStaysStripped(t *testing.T) {
	a := newCredLab(t, "127.0.0.1:0", false)
	b := newCredLab(t, "127.0.0.2:0", false)
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	hdrs := map[string]string{"X-Api-Key": "op-key", "Authorization": "Bearer op-token", "Cookie": "op=1"}
	run := func(to string) toolOut {
		return callHTTPTool(t, credRI, httpSpec(a.URL()+"/redir?to={to}", hdrs), map[string]any{"to": to})
	}
	t.Run("other origin then same-origin-as-previous hop", func(t *testing.T) {
		a.Reset()
		b.Reset()
		requireRan(t, run(b.URL()+"/redir?to="+b.URL()+"/echo"))
		hits := b.Hits()
		if len(hits) != 2 {
			t.Fatalf("want 2 hops on the other origin, got %d", len(hits))
		}
		for i, h := range hits {
			if leaked := operatorHeaderNames(h.Header); len(leaked) != 0 {
				t.Fatalf("hop %d on the other origin received %v", i, leaked)
			}
		}
	})
	t.Run("leaving and coming back stays stripped", func(t *testing.T) {
		a.Reset()
		b.Reset()
		requireRan(t, run(b.URL()+"/redir?to="+a.URL()+"/echo"))
		hits := a.Hits()
		if len(hits) != 2 {
			t.Fatalf("want 2 requests on the original origin, got %d", len(hits))
		}
		if leaked := operatorHeaderNames(hits[1].Header); len(leaked) != 0 {
			t.Fatalf("returning to the original origin brought back %v", leaked)
		}
	})
}

// A placeholder name with / ? # { } must not slip past the authority classification.
func TestCustomHTTPOddPlaceholderNames(t *testing.T) {
	b := newCredLab(t, "127.0.0.2:0", false)
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	spec := httpSpec("http://{ten/ant}:"+b.port+"/echo", operatorHeaders)
	// The odd name is no longer substituted, so the literal braces leave an invalid URL:
	// the request fails before anything is sent (it used to run with operator headers).
	if got := callHTTPTool(t, credRI, spec, map[string]any{"ten/ant": "127.0.0.2"}); !got.IsError {
		t.Fatalf("a placeholder with / in its name was substituted: %q", credTrunc(got.Text))
	}
	requireNoHits(t, "the templated host", b)
	for _, tmpl := range []string{"http://{a/b}.example.test/x", "http://{a}{b/x.example.test/", "{a/b}"} {
		if pin, templated := templateOrigin(tmpl); pin != nil || !templated {
			t.Fatalf("templateOrigin(%q) = %v, %v; want templated authority", tmpl, pin, templated)
		}
	}
}

// Error text for the model never carries the URL (query keys), host or header values.
func TestCustomHTTPErrorTextIsContentFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close() // nothing listens here any more
	setCredPolicy(t, credStaticRules(nil, nil), "9", nil)
	for name, tmpl := range map[string]string{
		"connection refused": "http://127.0.0.1:" + port + "/x?key=SECRET",
		"unparseable url":    "http://127.0.0.1:99999x/?key=SECRET",
	} {
		t.Run(name, func(t *testing.T) {
			got := callHTTPTool(t, credRI, httpSpec(tmpl, map[string]string{"X-Api-Key": "HDRSECRET"}), map[string]any{})
			if !got.IsError || !strings.HasPrefix(got.Text, "request failed: ") {
				t.Fatalf("want a request failed error, got %v %q", got.IsError, got.Text)
			}
			for _, bad := range []string{"SECRET", "127.0.0.1", port} {
				if strings.Contains(got.Text, bad) {
					t.Fatalf("error text leaks %q: %q", bad, got.Text)
				}
			}
		})
	}
}

// The recording-proxy path: the guard refuses the initial URL before the proxy is touched.
func TestCustomHTTPRecordingProxyPath(t *testing.T) {
	px := newCredProxy(t)
	setCredPolicy(t, credStaticRules(nil, nil), "9", nil)
	s := &Server{m: &Manager{globalProxy: strings.TrimPrefix(px.URL(), "http://")}}
	run := func(u string) toolOut {
		raw, _ := json.Marshal(map[string]any{"method": "GET", "url": u, "use_recording_proxy": true, "headers": map[string]string{"X-Api-Key": "k"}})
		res, err := s.runHTTPTool(agent.WithRunInfo(t.Context(), credRI), raw, map[string]any{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return toolOut{Text: res.Flatten(), IsError: res.IsError}
	}
	requireRefusal(t, run("http://169.254.169.254/latest/meta-data/"), credCodeControlPlane)
	if seen := px.Seen(); len(seen) != 0 {
		t.Fatalf("a refused request reached the recording proxy: %+v", seen)
	}
	requireRan(t, run("http://api.vendor.test/ok"))
	if seen := px.Seen(); len(seen) != 1 || seen[0].Header.Get("X-Api-Key") != "k" {
		t.Fatalf("allowed request did not go through the recording proxy: %+v", seen)
	}
}

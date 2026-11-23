package traffic

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	canaryUser = "canaryuser"
	canaryPass = "canarypass-7f3a"
)

// upstreamProxy is a stand-in for the operator's http proxy. With want set it answers 407 to a
// request that does not carry exactly that Proxy-Authorization.
type upstreamProxy struct {
	srv  *httptest.Server
	want string // required Proxy-Authorization value, "" = none required

	mu    sync.Mutex
	auths []string // the Proxy-Authorization of each request, in order
	reqs  []string // "METHOD target"
}

func newUpstreamProxy(t *testing.T, user, pass string) *upstreamProxy {
	t.Helper()
	p := &upstreamProxy{}
	if user != "" {
		p.want = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *upstreamProxy) serve(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("Proxy-Authorization")
	target := r.Host
	if r.Method != http.MethodConnect {
		target = r.URL.String()
	}
	p.mu.Lock()
	p.auths = append(p.auths, got)
	p.reqs = append(p.reqs, r.Method+" "+target)
	p.mu.Unlock()
	if p.want != "" && got != p.want {
		w.Header().Set("Proxy-Authenticate", `Basic realm="x"`)
		http.Error(w, "proxy auth required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			return
		}
		fmt.Fprint(c, "HTTP/1.1 200 OK\r\n\r\n")
		go func() { _, _ = io.Copy(dst, c); dst.Close() }()
		go func() { _, _ = io.Copy(c, dst); c.Close() }()
		return
	}
	out, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	resp, err := (&http.Transport{Proxy: nil}).RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *upstreamProxy) url(user, pass string) string {
	u, _ := url.Parse(p.srv.URL)
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

func (p *upstreamProxy) seen() ([]string, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.auths...), append([]string(nil), p.reqs...)
}

// socks5Server is a minimal SOCKS5 server (CONNECT, user/password or no authentication).
type socks5Server struct {
	ln         net.Listener
	user, pass string
	mu         sync.Mutex
	targets    []string
	gotUser    []string
}

func newSocks5(t *testing.T, user, pass string) *socks5Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socks5Server{ln: ln, user: user, pass: pass}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *socks5Server) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if s.user == "" {
		c.Write([]byte{5, 0})
	} else {
		c.Write([]byte{5, 2})
		ver, _ := br.ReadByte()
		ul, _ := br.ReadByte()
		ub := make([]byte, ul)
		io.ReadFull(br, ub)
		pl, _ := br.ReadByte()
		pb := make([]byte, pl)
		io.ReadFull(br, pb)
		s.mu.Lock()
		s.gotUser = append(s.gotUser, string(ub)+":"+string(pb))
		s.mu.Unlock()
		if ver != 1 || string(ub) != s.user || string(pb) != s.pass {
			c.Write([]byte{1, 1})
			return
		}
		c.Write([]byte{1, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	case 3:
		l, _ := br.ReadByte()
		b := make([]byte, l)
		io.ReadFull(br, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	}
	pb := make([]byte, 2)
	io.ReadFull(br, pb)
	target := net.JoinHostPort(host, fmt.Sprint(int(pb[0])<<8|int(pb[1])))
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	dst, err := net.Dial("tcp", target)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer dst.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go func() { _, _ = io.Copy(dst, br); dst.Close() }()
	_, _ = io.Copy(c, dst)
}

func (s *socks5Server) url(user, pass string) string {
	u := "socks5://"
	if user != "" {
		u += url.UserPassword(user, pass).String() + "@"
	}
	return u + s.ln.Addr().String()
}

// startForwarder starts a forwarder on a free loopback port that dials through *up.
func startForwarder(t *testing.T, up *string) (*Forwarder, *syncLog) {
	t.Helper()
	var mu sync.Mutex
	f, err := NewForwarder("127.0.0.1:0", func() string { mu.Lock(); defer mu.Unlock(); return *up })
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	f.logf = logs.printf
	if err := f.Listen(); err != nil {
		t.Fatal(err)
	}
	go f.Serve()
	t.Cleanup(func() { f.Close() })
	return f, logs
}

type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *syncLog) printf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", a...)
}
func (l *syncLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// toolClient is what a tool does: it knows the forwarder's address and nothing else.
func toolClient(f *Forwarder, roots *x509.CertPool) *http.Client {
	pu, _ := url.Parse("http://" + f.Addr())
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(pu),
			TLSClientConfig: &tls.Config{RootCAs: roots},
		},
	}
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func plainTarget(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "target:%s:auth=%q", r.URL.Path, r.Header.Get("Proxy-Authorization"))
	}))
	t.Cleanup(s.Close)
	return s
}

func tlsTarget(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "tls-target") }))
	t.Cleanup(s.Close)
	roots := x509.NewCertPool()
	roots.AddCert(s.Certificate())
	return s, roots
}

func TestParseEgressAddrAllowsLoopbackOnly(t *testing.T) {
	ok := map[string]string{
		"127.0.0.1:8789": "127.0.0.1:8789",
		":8789":          "127.0.0.1:8789",
		"localhost:8789": "127.0.0.1:8789",
		"[::1]:8789":     "[::1]:8789",
		"127.0.0.2:1":    "127.0.0.2:1",
	}
	for in, want := range ok {
		got, err := ParseEgressAddr(in)
		if err != nil || got != want {
			t.Errorf("ParseEgressAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"0.0.0.0:8789", "[::]:8789", "10.1.2.3:8789", "example.com:8789", "8789", "127.0.0.1:x", "127.0.0.1:70000", ""} {
		if got, err := ParseEgressAddr(in); err == nil {
			t.Errorf("ParseEgressAddr(%q) = %q, want an error", in, got)
		}
	}
	if _, err := NewForwarder("0.0.0.0:8789", func() string { return "" }); err == nil {
		t.Error("NewForwarder accepted a wildcard address")
	}
}

// The forwarder must not be reachable from another container: it listens on loopback, so a
// connection to the same port on any other address of this host is refused.
func TestForwarderIsNotReachableOnOtherAddresses(t *testing.T) {
	up := ""
	f, _ := startForwarder(t, &up)
	host, port, _ := net.SplitHostPort(f.Addr())
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("bound to %s, not a loopback address", f.Addr())
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	tried := 0
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		tried++
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), port), 2*time.Second)
		if err == nil {
			c.Close()
			t.Errorf("the forwarder answered on %s", ipn.IP)
		}
	}
	if tried == 0 {
		t.Log("no non-loopback IPv4 address on this host; the bind check above is the only evidence")
	}
}

func TestForwarderAddsCredentialsToPlainHTTP(t *testing.T) {
	target := plainTarget(t)
	up := newUpstreamProxy(t, canaryUser, canaryPass)
	cfg := up.url(canaryUser, canaryPass)
	f, logs := startForwarder(t, &cfg)

	code, body := get(t, toolClient(f, nil), target.URL+"/x")
	if code != 200 || !strings.HasPrefix(body, "target:/x") {
		t.Fatalf("got %d %q", code, body)
	}
	auths, reqs := up.seen()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(canaryUser+":"+canaryPass))
	if len(auths) != 1 || auths[0] != want {
		t.Fatalf("the upstream saw Proxy-Authorization %q, want exactly %q (requests %v)", auths, want, reqs)
	}
	if strings.Contains(logs.String(), canaryPass) {
		t.Errorf("the log holds the password: %s", logs.String())
	}
}

func TestForwarderConnectThroughAuthenticatedHTTPProxy(t *testing.T) {
	target, roots := tlsTarget(t)
	up := newUpstreamProxy(t, canaryUser, canaryPass)
	cfg := up.url(canaryUser, canaryPass)
	f, _ := startForwarder(t, &cfg)

	code, body := get(t, toolClient(f, roots), target.URL)
	if code != 200 || body != "tls-target" {
		t.Fatalf("got %d %q", code, body)
	}
	auths, reqs := up.seen()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(canaryUser+":"+canaryPass))
	if len(auths) != 1 || auths[0] != want || !strings.HasPrefix(reqs[0], "CONNECT ") {
		t.Fatalf("upstream saw %v %v, want one CONNECT with %q", reqs, auths, want)
	}
}

func TestForwarderUnauthenticatedUpstreamGetsNoCredentials(t *testing.T) {
	target, roots := tlsTarget(t)
	plain := plainTarget(t)
	up := newUpstreamProxy(t, "", "")
	cfg := up.url("", "")
	f, _ := startForwarder(t, &cfg)
	c := toolClient(f, roots)

	if code, _ := get(t, c, plain.URL); code != 200 {
		t.Fatalf("plain: %d", code)
	}
	if code, _ := get(t, c, target.URL); code != 200 {
		t.Fatalf("tls: %d", code)
	}
	auths, reqs := up.seen()
	if len(reqs) != 2 {
		t.Fatalf("the upstream saw %v, want the plain request and a CONNECT", reqs)
	}
	for _, a := range auths {
		if a != "" {
			t.Errorf("a credential was sent to an unauthenticated upstream: %q", a)
		}
	}
}

func TestForwarderThroughSocks5WithCredentials(t *testing.T) {
	target, roots := tlsTarget(t)
	plain := plainTarget(t)
	sk := newSocks5(t, canaryUser, canaryPass)
	cfg := sk.url(canaryUser, canaryPass)
	f, _ := startForwarder(t, &cfg)
	c := toolClient(f, roots)

	if code, body := get(t, c, plain.URL+"/s"); code != 200 || !strings.HasPrefix(body, "target:/s") {
		t.Fatalf("plain: %d %q", code, body)
	}
	if code, body := get(t, c, target.URL); code != 200 || body != "tls-target" {
		t.Fatalf("tls: %d %q", code, body)
	}
	sk.mu.Lock()
	defer sk.mu.Unlock()
	if len(sk.targets) != 2 {
		t.Fatalf("the socks5 server saw %v, want two connections", sk.targets)
	}
	for _, g := range sk.gotUser {
		if g != canaryUser+":"+canaryPass {
			t.Errorf("socks5 credentials = %q", g)
		}
	}
}

func TestForwarderSocks5WithoutCredentials(t *testing.T) {
	_, roots := tlsTarget(t)
	plain := plainTarget(t)
	sk := newSocks5(t, "", "")
	cfg := sk.url("", "")
	f, _ := startForwarder(t, &cfg)
	if code, _ := get(t, toolClient(f, roots), plain.URL); code != 200 {
		t.Fatalf("got %d", code)
	}
}

// With no upstream proxy configured the forwarder refuses: a tool must never get a direct route
// out through it. The refusal is a 502 with a short body, for plain HTTP and for CONNECT, and the
// target is never contacted.
func TestForwarderWithoutUpstreamRefusesAndNeverDials(t *testing.T) {
	var hits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer target.Close()
	var dials int
	up := ""
	f, _ := startForwarder(t, &up)
	f.dialer.Control = func(network, address string, _ syscall.RawConn) error {
		mu.Lock()
		defer mu.Unlock()
		dials++
		return nil
	}
	resp, err := toolClient(f, nil).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || len(body) > 200 {
		t.Fatalf("plain: status %d body %q, want 502 with a short body", resp.StatusCode, body)
	}
	host := strings.TrimPrefix(target.URL, "http://")
	c, err := net.Dial("tcp", f.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	r, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("CONNECT: status %d, want 502", r.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 || dials != 0 {
		t.Fatalf("the target was reached directly: hits=%d dials=%d", hits, dials)
	}
}

// The refusal is logged at most once a minute, so a tool retrying in a loop cannot flood the log.
func TestForwarderLogsTheMissingUpstreamOncePerMinute(t *testing.T) {
	up := ""
	f, logs := startForwarder(t, &up)
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	f.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := toolClient(f, nil)
	count := func() int { return strings.Count(logs.String(), "no egress proxy is configured") }
	for i := 0; i < 5; i++ {
		resp, err := c.Get("http://192.0.2.1/x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if n := count(); n != 1 {
		t.Fatalf("log lines after 5 refusals: %d want 1\n%s", n, logs.String())
	}
	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	resp, err := c.Get("http://192.0.2.1/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := count(); n != 2 {
		t.Fatalf("log lines after a minute: %d want 2\n%s", n, logs.String())
	}
}

// The host of a request is chosen by the client: log lines carry it capped and without control
// characters.
func TestForwarderCapsAndSanitisesTheClientHostInLogs(t *testing.T) {
	cfg := "http://127.0.0.1:1" // a closed port: every request fails after the upstream dial
	f, logs := startForwarder(t, &cfg)
	long := strings.Repeat("a", 500) + ".example"
	c, err := net.Dial("tcp", f.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: x\r\n\r\n", long)
	if _, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil {
		t.Fatal(err)
	}
	resp, err := toolClient(f, nil).Get("http://" + long + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	out := logs.String()
	if out == "" {
		t.Fatal("nothing was logged")
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) > 300 {
			t.Fatalf("a log line of %d bytes carries the client host uncapped: %.120s...", len(line), line)
		}
	}
	if got := logSafe("evil\nforged [egress] line\r\x00"); strings.ContainsAny(got, "\n\r\x00") {
		t.Fatalf("control characters survived: %q", got)
	}
}

// The upstream is read on every connection: a change of the global proxy applies without a
// restart of the forwarder.
func TestForwarderReadsTheUpstreamLive(t *testing.T) {
	plain := plainTarget(t)
	up := newUpstreamProxy(t, canaryUser, canaryPass)
	cfg := ""
	var mu sync.Mutex
	f, err := NewForwarder("127.0.0.1:0", func() string { mu.Lock(); defer mu.Unlock(); return cfg })
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Listen(); err != nil {
		t.Fatal(err)
	}
	go f.Serve()
	defer f.Close()
	c := toolClient(f, nil)

	if resp, err := c.Get(plain.URL); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("without an upstream: %v %v, want 502", resp, err)
	}
	if _, reqs := up.seen(); len(reqs) != 0 {
		t.Fatalf("direct request reached the proxy: %v", reqs)
	}
	mu.Lock()
	cfg = up.url(canaryUser, canaryPass)
	mu.Unlock()
	get(t, c, plain.URL)
	if _, reqs := up.seen(); len(reqs) != 1 {
		t.Fatalf("after the change the proxy saw %v, want one request", reqs)
	}
	mu.Lock()
	cfg = ""
	mu.Unlock()
	if resp, err := c.Get(plain.URL); err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("after clearing: %v %v, want 502", resp, err)
	}
	if _, reqs := up.seen(); len(reqs) != 1 {
		t.Fatalf("after clearing the proxy saw %v, want still one request", reqs)
	}
}

// A tool cannot supply its own proxy credentials: Proxy-Authorization from the client never
// reaches the upstream.
func TestForwarderDropsTheClientsProxyAuthorization(t *testing.T) {
	plain := plainTarget(t)
	up := newUpstreamProxy(t, "", "")
	cfg := up.url("", "")
	f, _ := startForwarder(t, &cfg)
	req, _ := http.NewRequest("GET", plain.URL, nil)
	req.Header.Set("Proxy-Authorization", "Basic dG9vbDpzZWNyZXQ=")
	resp, err := toolClient(f, nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if auths, _ := up.seen(); len(auths) != 1 || auths[0] != "" {
		t.Fatalf("upstream saw %q, want no Proxy-Authorization", auths)
	}
	if strings.Contains(string(b), "dG9vbDpzZWNyZXQ") {
		t.Fatalf("the target saw the client's Proxy-Authorization: %s", b)
	}
}

// Errors name neither the credentials nor the upstream, to the client and in the log.
func TestForwarderErrorsHideTheUpstream(t *testing.T) {
	// an upstream that refuses connections: a closed port
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	cfg := "http://" + url.UserPassword(canaryUser, canaryPass).String() + "@" + dead
	f, logs := startForwarder(t, &cfg)

	for _, mode := range []string{"plain", "connect"} {
		var resp *http.Response
		var err error
		if mode == "plain" {
			resp, err = toolClient(f, nil).Get("http://target.invalid/")
		} else {
			// CONNECT by hand so the status line is visible
			c, derr := net.Dial("tcp", f.Addr())
			if derr != nil {
				t.Fatal(derr)
			}
			fmt.Fprint(c, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\n")
			resp, err = http.ReadResponse(bufio.NewReader(c), nil)
			defer c.Close()
		}
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: status %d, want 502", mode, resp.StatusCode)
		}
		seen := fmt.Sprint(resp.Header) + string(b)
		for _, s := range []string{canaryUser, canaryPass, dead} {
			if strings.Contains(seen, s) {
				t.Errorf("%s: the answer to the tool holds %q: %s", mode, s, seen)
			}
		}
	}
	lg := logs.String()
	if lg == "" {
		t.Fatal("nothing was logged")
	}
	for _, s := range []string{canaryUser, canaryPass, dead} {
		if strings.Contains(lg, s) {
			t.Errorf("the log holds %q:\n%s", s, lg)
		}
	}
}

// An upstream that wants other credentials produces a plain 502 for the tool, not a relayed 407.
func TestForwarderDoesNotRelayAProxyAuthenticationChallenge(t *testing.T) {
	plain := plainTarget(t)
	up := newUpstreamProxy(t, "right", "pw")
	cfg := up.url("wrong", "pw")
	f, _ := startForwarder(t, &cfg)
	resp, err := toolClient(f, nil).Get(plain.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("Proxy-Authenticate") != "" {
		t.Fatal("the challenge reached the tool")
	}
}

// A stored proxy that does not parse must not turn into a direct connection.
func TestForwarderFailsClosedOnAnInvalidUpstream(t *testing.T) {
	var hits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer target.Close()
	cfg := "nope://x"
	f, _ := startForwarder(t, &cfg)
	resp, err := toolClient(f, nil).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the target was contacted directly %d times", hits)
	}
}

func TestForwarderRejectsRequestsThatAreNotProxyRequests(t *testing.T) {
	up := ""
	f, _ := startForwarder(t, &up)
	resp, err := http.Get("http://" + f.Addr() + "/x") // origin-form: not a proxy request
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("origin-form request: status %d, want 400", resp.StatusCode)
	}
	for _, target := range []string{"nohost", "h:0", "h:99999", "h:x"} {
		c, err := net.Dial("tcp", f.Addr())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		r, err := http.ReadResponse(bufio.NewReader(c), nil)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("CONNECT %s: status %d, want 400", target, r.StatusCode)
		}
	}
}

func TestForwarderListenFailureIsReported(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	f, err := NewForwarder(busy.Addr().String(), func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Listen(); err == nil {
		t.Fatal("Listen on a busy port succeeded")
	}
	if err := f.Serve(); err == nil {
		t.Fatal("Serve without a listener returned nil")
	}
}

func TestForwarderCloseEndsOpenTunnels(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	upstream := newUpstreamProxy(t, canaryUser, canaryPass)
	up := upstream.url(canaryUser, canaryPass)
	f, _ := startForwarder(t, &up)
	c, err := net.Dial("tcp", f.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	host := strings.TrimPrefix(target.URL, "http://")
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	br := bufio.NewReader(c)
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	f.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("read succeeded on a closed tunnel")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("the tunnel was still open after Close")
	}
}

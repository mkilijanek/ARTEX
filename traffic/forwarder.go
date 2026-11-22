package traffic

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/net/proxy"
)

// DefaultEgressAddr is where the credential-free egress forwarder listens: loopback only, so it
// is not reachable from other containers, and not published by Compose.
const DefaultEgressAddr = "127.0.0.1:8789"

// ParseEgressAddr checks the forwarder's listen address and returns it in host:port form. The
// host must be a loopback address (an empty host and "localhost" mean 127.0.0.1): the forwarder
// adds the operator's proxy credentials itself, so anything that can reach it can use them.
func ParseEgressAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("egress address %q: %w", raw, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("egress address %q: %q is not a port", raw, port)
	}
	switch {
	case host == "" || strings.EqualFold(host, "localhost"):
		host = "127.0.0.1"
	default:
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return "", fmt.Errorf("egress address %q: the host must be a loopback address", raw)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// Forwarder is a plain forward proxy for tools: HTTP requests with an absolute URI and CONNECT,
// no TLS interception and no recording. Every connection leaves through the operator's global
// egress proxy, which is read on each connection, so a change applies without a restart. The
// credentials of that proxy exist only in this process; a client of the forwarder sends none
// and learns neither the credentials nor the upstream address (errors name neither).
type Forwarder struct {
	addr     string
	upstream func() string
	logf     func(string, ...any)
	dialer   net.Dialer
	tr       *http.Transport
	now      func() time.Time

	mu           sync.Mutex
	noUpstreamAt time.Time // when the missing upstream was last logged
	ln           net.Listener
	srv          *http.Server
	tunnels      map[net.Conn]struct{}
	closed       bool
}

// NewForwarder builds a forwarder that listens on addr (see ParseEgressAddr) and dials through
// the proxy URL upstream returns. An empty URL means no egress proxy is configured: every request
// is then refused, never dialled directly.
func NewForwarder(addr string, upstream func() string) (*Forwarder, error) {
	addr, err := ParseEgressAddr(addr)
	if err != nil {
		return nil, err
	}
	f := &Forwarder{
		addr: addr, upstream: upstream, logf: log.Printf, now: time.Now,
		dialer:  net.Dialer{Timeout: dialTimeout},
		tunnels: map[net.Conn]struct{}{},
	}
	f.tr = &http.Transport{
		// An explicit function: the process environment is never consulted. An invalid stored
		// proxy is an error, not a direct connection.
		Proxy:               func(*http.Request) (*url.URL, error) { return f.requireUpstream() },
		DialContext:         f.dialer.DialContext,
		TLSHandshakeTimeout: dialTimeout,
		MaxIdleConns:        32,
		IdleConnTimeout:     30 * time.Second,
	}
	return f, nil
}

const dialTimeout = 30 * time.Second

// Listen binds the address. It is separate from Serve so a failure is reported to the caller.
func (f *Forwarder) Listen() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("egress forwarder is closed")
	}
	if f.ln != nil {
		return errors.New("egress forwarder is already listening")
	}
	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		return fmt.Errorf("egress forwarder cannot listen on %s: %w", f.addr, err)
	}
	f.ln = ln
	f.srv = &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	return nil
}

// Addr is the bound address (host:port), or the configured one before Listen.
func (f *Forwarder) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		return f.ln.Addr().String()
	}
	return f.addr
}

// Serve accepts connections until Close. Run it in a goroutine after Listen.
func (f *Forwarder) Serve() error {
	f.mu.Lock()
	srv, ln := f.srv, f.ln
	f.mu.Unlock()
	if srv == nil {
		return errors.New("egress forwarder is not listening")
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Close stops listening and ends open tunnels.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	f.closed = true
	srv := f.srv
	open := make([]net.Conn, 0, len(f.tunnels))
	for c := range f.tunnels {
		open = append(open, c)
	}
	f.mu.Unlock()
	var err error
	if srv != nil {
		err = srv.Close()
	}
	for _, c := range open {
		_ = c.Close()
	}
	f.tr.CloseIdleConnections()
	return err
}

// errNoUpstream is why a request is refused while no egress proxy is configured.
var errNoUpstream = errors.New("no egress proxy is configured")

// noUpstreamLogEvery spaces the log lines about a missing upstream.
const noUpstreamLogEvery = time.Minute

// requireUpstream is the current global proxy, or errNoUpstream: the forwarder has no direct route.
func (f *Forwarder) requireUpstream() (*url.URL, error) {
	u, err := f.upstreamURL()
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errNoUpstream
	}
	return u, nil
}

// logClip keeps a client-chosen string from forging log lines: control characters become '?' and
// the length is capped at max bytes.
func logClip(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func logSafe(s string) string { return logClip(s, 80) }

// upstreamURL parses the current global proxy; nil means none is set.
func (f *Forwarder) upstreamURL() (*url.URL, error) {
	raw := strings.TrimSpace(f.upstream())
	if raw == "" {
		return nil, nil
	}
	u, err := ValidateProxyURL(raw)
	if err != nil {
		return nil, errors.New("the configured egress proxy is invalid")
	}
	return u, nil
}

// scrub removes the upstream's address and credentials from err before it is logged: the log is
// readable by operators who must not need the proxy's details, and the text never goes to a client.
func (f *Forwarder) scrub(err error) string {
	msg := err.Error()
	u, perr := f.upstreamURL()
	if perr != nil || u == nil {
		return msg
	}
	if u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			msg = strings.ReplaceAll(msg, p, "***")
		}
		if n := u.User.Username(); n != "" {
			msg = strings.ReplaceAll(msg, n, "***")
		}
	}
	for _, s := range []string{u.Host, u.Hostname()} {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "<upstream>")
		}
	}
	return msg
}

// fail logs err and answers 502. what may carry a client-chosen host: it is capped and sanitised
// here. A missing upstream is logged at most once a minute, whatever the request.
func (f *Forwarder) fail(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, errNoUpstream) {
		f.mu.Lock()
		due := f.now().Sub(f.noUpstreamAt) >= noUpstreamLogEvery
		if due {
			f.noUpstreamAt = f.now()
		}
		f.mu.Unlock()
		if due {
			f.logf("[egress] refused a request: %s; tools have no route out until a global egress proxy is set", errNoUpstream)
		}
		http.Error(w, "egress forwarder: no egress proxy is configured", http.StatusBadGateway)
		return
	}
	// An error text can carry the target too (a SOCKS5 failure names it), so it is clipped as well.
	f.logf("[egress] %s: %s", logSafe(what), logClip(f.scrub(err), 200))
	http.Error(w, "egress forwarder: the connection could not be made", http.StatusBadGateway)
}

// ServeHTTP handles CONNECT and absolute-URI requests.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		f.connect(w, r)
		return
	}
	f.forward(w, r)
}

// hopHeaders are removed in both directions; Proxy-Authorization in particular is never passed
// on: the credentials the upstream sees are the operator's, not anything a tool sent.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func (f *Forwarder) forward(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "egress forwarder: this is a proxy; send an absolute URI or CONNECT", http.StatusBadRequest)
		return
	}
	if r.URL.Scheme != "http" {
		http.Error(w, "egress forwarder: use CONNECT for https", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Close = false
	out.Header = r.Header.Clone()
	stripHop(out.Header)
	resp, err := f.tr.RoundTrip(out)
	if err != nil {
		f.fail(w, r.Method+" "+r.URL.Host, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		// Only the upstream proxy answers this way; relaying it would make a tool think the
		// forwarder wants credentials.
		f.fail(w, r.Method+" "+r.URL.Host, errors.New("the egress proxy asked for authentication"))
		return
	}
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if rerr != nil {
			return
		}
	}
}

func (f *Forwarder) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" {
		http.Error(w, "egress forwarder: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		http.Error(w, "egress forwarder: CONNECT needs a port", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	up, err := f.dial(ctx, r.Host)
	if err != nil {
		f.fail(w, "CONNECT "+r.Host, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "egress forwarder: cannot take over the connection", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		up.Close()
		f.logf("[egress] CONNECT %s: %v", logSafe(r.Host), err)
		return
	}
	// Bytes the client sent before it saw the answer belong to the tunnel.
	var pending []byte
	if n := brw.Reader.Buffered(); n > 0 {
		pending, _ = brw.Reader.Peek(n)
		pending = append([]byte(nil), pending...)
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		up.Close()
		return
	}
	f.tunnel(client, up, pending)
}

type halfCloser interface{ CloseWrite() error }

func (f *Forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.tunnels[c] = struct{}{}
	return true
}

func (f *Forwarder) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.tunnels, c)
	f.mu.Unlock()
}

func (f *Forwarder) tunnel(client, up net.Conn, pending []byte) {
	if !f.track(client) {
		client.Close()
		up.Close()
		return
	}
	f.track(up)
	defer func() {
		f.untrack(client)
		f.untrack(up)
		client.Close()
		up.Close()
	}()
	if len(pending) > 0 {
		if _, err := up.Write(pending); err != nil {
			return
		}
	}
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if hc, ok := dst.(halfCloser); ok {
			_ = hc.CloseWrite()
		} else {
			// no half-close: end both directions, as the other copy would block forever
			client.Close()
			up.Close()
		}
		done <- struct{}{}
	}
	go pipe(up, client)
	go pipe(client, up)
	<-done
	<-done
}

// dial opens a connection to target (host:port) through the current global proxy.
func (f *Forwarder) dial(ctx context.Context, target string) (net.Conn, error) {
	u, err := f.requireUpstream()
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "socks5":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", withDefaultPort(u.Host, "1080"), auth, &f.dialer)
		if err != nil {
			return nil, err
		}
		cd, ok := d.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("the socks5 dialer cannot take a context")
		}
		return cd.DialContext(ctx, "tcp", target)
	default: // http, https (ValidateProxyURL allows nothing else)
		return f.dialHTTPProxy(ctx, u, target)
	}
}

func withDefaultPort(hostport, port string) string {
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport
	}
	return net.JoinHostPort(strings.Trim(hostport, "[]"), port)
}

// dialHTTPProxy opens a tunnel to target through an http or https proxy with CONNECT, adding
// Proxy-Authorization from the proxy URL's user information.
func (f *Forwarder) dialHTTPProxy(ctx context.Context, u *url.URL, target string) (net.Conn, error) {
	defPort := "80"
	if u.Scheme == "https" {
		defPort = "443"
	}
	conn, err := f.dialer.DialContext(ctx, "tcp", withDefaultPort(u.Host, defPort))
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname()})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tc
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if u.User != nil {
		pw, _ := u.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw)))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("the egress proxy refused CONNECT: %s", resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn reads through the bufio.Reader that parsed the proxy's answer, so bytes that
// arrived with it are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if hc, ok := c.Conn.(halfCloser); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}

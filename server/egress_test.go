//go:build unix

package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/sandbox"
	"github.com/mkilijanek/artex/traffic"
)

const canaryProxy = "http://canaryuser:canarypw@proxy.canary.example:3128"

// egressManager is a manager whose global proxy and traffic switch are put back afterwards: the
// settings live in the shared test database.
func egressManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.SetTrafficEnabled(false)
		_ = m.SetGlobalProxy("")
		m.Close()
	})
	return m
}

// A forwarder whose Serve stops after the start-up raises the same status warning (E1) as one that
// could not start; a clean Close does not.
func TestEgressServeStopRaisesTheStatusWarning(t *testing.T) {
	iso, _ := activateEnforcedTree(t)
	m := egressManager(t)
	f, err := traffic.NewForwarder("127.0.0.1:0", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.serveEgress(f); close(done) }()
	f.Close()
	<-done
	if v := iso.View(); strings.Contains(v.Reason, "E1") {
		t.Fatalf("a normal Close raised the warning: %+v", v)
	}
	// Serve on a forwarder that is not listening returns an error at once.
	g, err := traffic.NewForwarder("127.0.0.1:0", func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	m.serveEgress(g)
	v := iso.View()
	if v.Status != sandbox.Degraded || !strings.Contains(v.Reason, "E1: the egress forwarder stopped") {
		t.Fatalf("view after an unexpected stop: %+v", v)
	}
}

func TestDefaultForwarderAddressesAgree(t *testing.T) {
	if agent.DefaultEgressForwarder != "http://"+traffic.DefaultEgressAddr {
		t.Fatalf("agent default %q and traffic default %q differ", agent.DefaultEgressForwarder, traffic.DefaultEgressAddr)
	}
}

// AC2, AC43: with a global proxy that carries credentials and capture off, the address tools get is
// the forwarder's, whatever the proxy is; the server itself keeps using the proxy.
func TestToolProxyAddrNeverReturnsTheGlobalProxy(t *testing.T) {
	m := egressManager(t)
	if got := m.ToolProxyAddr(); got != "" {
		t.Fatalf("no global proxy: ToolProxyAddr = %q, want none", got)
	}
	for _, raw := range []string{canaryProxy, "socks5://canaryuser:canarypw@10.9.8.7:1080", "http://proxy.canary.example:3128"} {
		if err := m.SetGlobalProxy(raw); err != nil {
			t.Fatal(err)
		}
		if got := m.ToolProxyAddr(); got != "http://127.0.0.1:8789" {
			t.Errorf("%s: ToolProxyAddr = %q, want the forwarder", raw, got)
		}
		if got := m.ProxyAddr(); got != raw {
			t.Errorf("%s: the server's own ProxyAddr = %q, WebFetch must keep the proxy", raw, got)
		}
	}
	if err := m.SetGlobalProxy(""); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolProxyAddr(); got != "" {
		t.Errorf("after clearing: %q", got)
	}
}

// AC43(d): with capture on, tools get the recording proxy, as before.
func TestToolProxyAddrWithCaptureOnIsTheRecordingProxy(t *testing.T) {
	m := egressManager(t)
	var err error
	m.traffic, err = traffic.Open(filepath.Join(m.dir, "traffic"), "127.0.0.1:18788")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetGlobalProxy(canaryProxy); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTrafficEnabled(true); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolProxyAddr(); got != "http://127.0.0.1:18788" || got != m.ProxyAddr() {
		t.Fatalf("ToolProxyAddr = %q, ProxyAddr = %q, want the recording proxy for both", got, m.ProxyAddr())
	}
}

// AC43(f): the seeded browser MCP gets the forwarder address, and nothing of the operator's proxy
// reaches its row (arguments or environment).
func TestBrowserMCPGetsTheForwarderNotTheGlobalProxy(t *testing.T) {
	m := egressManager(t)
	row := func() (string, string) {
		r := listMCPByName(t, m.pg, browserMCPName)
		return string(r.Args), string(r.Env)
	}
	argv := func() []string {
		var a []string
		if err := json.Unmarshal([]byte(func() string { x, _ := row(); return x }()), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	if err := m.SetGlobalProxy(canaryProxy); err != nil {
		t.Fatal(err)
	}
	args, env := row()
	if a := argv(); len(a) < 2 || a[len(a)-2] != "--proxy-server" || a[len(a)-1] != "http://127.0.0.1:8789" {
		t.Fatalf("browser MCP args = %s, want --proxy-server with the forwarder", args)
	}
	for _, leak := range []string{"canary", "3128"} {
		if strings.Contains(args, leak) || strings.Contains(env, leak) {
			t.Errorf("%q reached the browser MCP row: args %s env %s", leak, args, env)
		}
	}
	if err := m.SetGlobalProxy(""); err != nil {
		t.Fatal(err)
	}
	if args, _ := row(); strings.Contains(args, "--proxy-server") {
		t.Errorf("proxy argument stayed after the proxy was cleared: %s", args)
	}
}

// startEgress starts the forwarder on a free loopback port and points the agents' default at it
// for the test.
func startEgress(t *testing.T, m *Manager) string {
	t.Helper()
	if err := m.StartEgressForwarder("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.SetEgressForwarder("") })
	return m.EgressAddr()
}

// The forwarder reads the global proxy from the manager on every connection: a change through the
// settings applies at once, and the credentials go upstream from the server's memory.
func TestForwarderFollowsTheManagersGlobalProxy(t *testing.T) {
	m := egressManager(t)
	var mu sync.Mutex
	var auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Proxy-Authorization"))
		mu.Unlock()
		io.WriteString(w, "via-upstream")
	}))
	defer up.Close()

	addr := startEgress(t, m)
	if got := m.ToolProxyAddr(); got != "" {
		t.Fatalf("ToolProxyAddr = %q before any global proxy", got)
	}
	upURL, _ := url.Parse(up.URL)
	upURL.User = url.UserPassword("canaryuser", "canarypw")
	if err := m.SetGlobalProxy(upURL.String()); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolProxyAddr(); got != "http://"+addr {
		t.Fatalf("ToolProxyAddr = %q, want the forwarder at %s", got, addr)
	}

	// what a tool does: only the forwarder's address
	pu, _ := url.Parse(m.ToolProxyAddr())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := c.Get("http://target.invalid/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "via-upstream" {
		t.Fatalf("answer %q", b)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("canaryuser:canarypw"))
	mu.Lock()
	defer mu.Unlock()
	if len(auths) != 1 || auths[0] != want {
		t.Fatalf("the upstream saw %q, want the operator's credentials %q", auths, want)
	}
}

// AC43(e), failure: a forwarder that cannot listen is reported, and tools are still pointed at its
// address so their egress fails instead of falling back to the credentials.
func TestForwarderStartFailureStillPointsToolsAtIt(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	m := egressManager(t)
	t.Cleanup(func() { agent.SetEgressForwarder("") })
	if err := m.StartEgressForwarder(busy.Addr().String()); err == nil {
		t.Fatal("starting on a busy port succeeded")
	}
	if err := m.SetGlobalProxy(canaryProxy); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolProxyAddr(); got != "http://"+busy.Addr().String() {
		t.Fatalf("ToolProxyAddr = %q, want the (dead) forwarder address", got)
	}
	// nothing answers there as a proxy: the connection is refused or the busy listener ignores it
	pu, _ := url.Parse(m.ToolProxyAddr())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 2e9}
	if resp, err := c.Get("http://target.invalid/"); err == nil {
		resp.Body.Close()
		t.Fatalf("a tool reached something through the dead forwarder: %s", resp.Status)
	}
}

func TestForwarderRefusesANonLoopbackAddressAndDisabledStaysDefault(t *testing.T) {
	m := egressManager(t)
	if err := m.StartEgressForwarder("0.0.0.0:8789"); err == nil {
		t.Fatal("a wildcard address was accepted")
	}
	if err := m.StartEgressForwarder(""); err != nil {
		t.Fatalf("empty (disabled): %v", err)
	}
	if err := m.SetGlobalProxy(canaryProxy); err != nil {
		t.Fatal(err)
	}
	if got := m.ToolProxyAddr(); got != "http://127.0.0.1:8789" {
		t.Fatalf("ToolProxyAddr = %q: disabled or refused, tools must still be pointed at the default forwarder address", got)
	}
}

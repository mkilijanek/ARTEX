package server

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// useLocalAddrs replaces the interface listing for one test.
func useLocalAddrs(t *testing.T, now func() time.Time, fetch func() ([]net.Addr, error)) {
	t.Helper()
	old := localAddrs
	localAddrs = &localAddrCache{now: now, fetch: fetch}
	t.Cleanup(func() { localAddrs = old })
}

func fixedAddrs(cidrs ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, err
			}
			n.IP = ip
			out = append(out, n)
		}
		out = append(out, &net.IPAddr{IP: net.ParseIP("192.0.2.222")}) // not an *net.IPNet: ignored
		return out, nil
	}
}

func keyOf(remote string) string {
	return throttleKey(&http.Request{RemoteAddr: remote})
}

func TestThrottleKeyLoopbackAndMappedAddressesAreOneKey(t *testing.T) {
	useLocalAddrs(t, time.Now, fixedAddrs())
	for _, a := range []string{"127.0.0.1:1", "127.0.0.2:5", "127.0.0.9:7", "127.255.255.254:9", "[::1]:1", "[::ffff:127.0.0.5]:1", "[::ffff:127.0.0.1]:1"} {
		if got := keyOf(a); got != "local" {
			t.Errorf("%s -> %q want local", a, got)
		}
	}
	if got := keyOf("[::ffff:192.0.2.7]:1"); got != "192.0.2.7" {
		t.Errorf("mapped public address -> %q want 192.0.2.7", got)
	}
	if got := keyOf("192.0.2.7:1"); got != "192.0.2.7" {
		t.Errorf("public address -> %q", got)
	}
	if got := keyOf("128.0.0.1:1"); got == "local" {
		t.Errorf("128.0.0.1 is outside 127.0.0.0/8 but maps to local")
	}
}

func TestThrottleKeyInterfaceAddressesAreLocal(t *testing.T) {
	useLocalAddrs(t, time.Now, fixedAddrs("10.9.8.7/24", "fd00:1:2:3::5/64", "fe80::1/64"))
	for _, a := range []string{"10.9.8.7:1", "[::ffff:10.9.8.7]:1", "[fd00:1:2:3::5]:1", "[fe80::1%eth0]:1"} {
		if got := keyOf(a); got != "local" {
			t.Errorf("%s -> %q want local", a, got)
		}
	}
	// A neighbour in the same subnet is not the host itself.
	if got := keyOf("10.9.8.8:1"); got != "10.9.8.8" {
		t.Errorf("neighbour address -> %q", got)
	}
	if got := keyOf("[fd00:1:2:3::6]:1"); got != "fd00:1:2:3::/64" {
		t.Errorf("neighbour v6 address -> %q", got)
	}
	if got := keyOf("192.0.2.222:1"); got == "local" {
		t.Errorf("an address of a non-IPNet entry must not count")
	}
}

func TestThrottleKeyRealInterfaceAddressesAreLocal(t *testing.T) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot list interfaces: %v", err)
	}
	n := 0
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		n++
		if got := throttleKey(&http.Request{RemoteAddr: net.JoinHostPort(ipn.IP.String(), "40000")}); got != "local" {
			t.Errorf("interface address %s -> %q want local", ipn.IP, got)
		}
	}
	if n == 0 {
		t.Skip("no interface addresses")
	}
}

func TestLocalAddrCacheRefreshesEverySixtySecondsAndLogsFailures(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	calls := 0
	var fail bool
	useLocalAddrs(t, clk.now, func() ([]net.Addr, error) {
		calls++
		if fail {
			return nil, errors.New("boom")
		}
		return fixedAddrs("10.9.8.7/24")()
	})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(log.Writer())
	for i := 0; i < 5; i++ {
		if keyOf("10.9.8.7:1") != "local" {
			t.Fatal("interface address not local")
		}
	}
	clk.advance(59 * time.Second)
	keyOf("10.9.8.7:1")
	if calls != 1 {
		t.Fatalf("interfaces listed %d times within 60 s, want 1", calls)
	}
	fail = true
	clk.advance(2 * time.Second)
	if got := keyOf("10.9.8.7:1"); got != "10.9.8.7" {
		t.Fatalf("after a failed listing the interface address is %q, want not local", got)
	}
	if got := keyOf("[::ffff:127.0.0.3]:1"); got != "local" {
		t.Fatalf("loopback after a failed listing: %q want local", got)
	}
	keyOf("10.9.8.7:1")
	if calls != 2 || !strings.Contains(buf.String(), "could not list the local interface addresses: boom") {
		t.Fatalf("calls=%d log=%q", calls, buf.String())
	}
}

func TestThrottleKeyIPv6SourcesShareTheirSlash64(t *testing.T) {
	useLocalAddrs(t, time.Now, fixedAddrs())
	a := keyOf("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:1")
	b := keyOf("[2001:db8:1:2::1]:2")
	c := keyOf("[2001:db8:1:3::1]:1")
	if a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("same /64: %q vs %q", a, b)
	}
	if c == a {
		t.Fatalf("different /64 share a key: %q", c)
	}
}

func TestLoopbackSourcesShareOneThrottleAndLockAfterFiveFailures(t *testing.T) {
	useThrottle(t)
	useLocalAddrs(t, time.Now, fixedAddrs())
	g := newLoginRig(t)
	for i := 0; i < throttleFreeFailures; i++ {
		src := fmt.Sprintf("127.0.0.%d", 2+i)
		if w := g.login(src, "ARTEX", "wrong-password-1"); w.Code != 401 {
			t.Fatalf("failure from %s: status=%d want 401", src, w.Code)
		}
	}
	for _, src := range []string{"127.0.0.7", "127.0.0.8", "127.0.0.9", "[::1]"} {
		if w := g.login(src, "ARTEX", rigPassword); w.Code != 429 || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s after 5 failures from other loopback sources: status=%d want 429", src, w.Code)
		}
	}
	if w := g.login("192.0.2.1", "ARTEX", rigPassword); w.Code != 200 {
		t.Fatalf("a public source must be unaffected: status=%d", w.Code)
	}
}

func TestManySourcesFailingDoNotBlockAFreshSource(t *testing.T) {
	useThrottle(t)
	useLocalAddrs(t, time.Now, fixedAddrs())
	g := newLoginRig(t)
	for i := 1; i <= 20; i++ {
		src := fmt.Sprintf("198.51.100.%d", i)
		for j := 0; j < throttleFreeFailures; j++ {
			if w := g.login(src, "ARTEX", "wrong-password-1"); w.Code != 401 {
				t.Fatalf("failure %d from %s: status=%d want 401", j+1, src, w.Code)
			}
		}
	}
	if w := g.login("203.0.113.77", "ARTEX", rigPassword); w.Code != 200 {
		t.Fatalf("a fresh source after failures from many others: status=%d want 200 (no global lockout)", w.Code)
	}
}

package server

import (
	"log"
	"net"
	"net/netip"
	"sync"
	"time"
)

// localSourceKey is the one throttle key of every source that is local to the server's own host or
// container: loopback and any address assigned to one of its interfaces. A process on the host
// (a tool process included) may bind() to any address of 127.0.0.0/8, or to an address of its own
// interface, before it connects, so those sources cost it nothing to rotate; they must not each
// get a fresh counter.
const localSourceKey = "local"

// localAddrRefresh is how long the list of interface addresses is reused.
const localAddrRefresh = 60 * time.Second

// localAddrCache lists the addresses assigned to the local interfaces, read again at most every
// localAddrRefresh. A failed read is logged; loopback stays local (it is decided without the
// list), any other address is then not local until a later read succeeds.
type localAddrCache struct {
	mu    sync.Mutex
	now   func() time.Time
	fetch func() ([]net.Addr, error)
	at    time.Time
	set   map[netip.Addr]struct{}
}

var localAddrs = &localAddrCache{now: time.Now, fetch: net.InterfaceAddrs}

func (c *localAddrCache) has(a netip.Addr) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.now(); c.at.IsZero() || now.Sub(c.at) >= localAddrRefresh || now.Before(c.at) {
		c.at = now
		addrs, err := c.fetch()
		if err != nil {
			log.Printf("[auth] could not list the local interface addresses: %v", err)
		}
		c.set = map[netip.Addr]struct{}{}
		for _, addr := range addrs {
			if ipn, ok := addr.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(ipn.IP); ok {
					c.set[ip.Unmap().WithZone("")] = struct{}{}
				}
			}
		}
	}
	_, ok := c.set[a]
	return ok
}

// sourceKey maps the host part of a RemoteAddr to its throttle key: an IPv4-mapped IPv6 address
// is read as IPv4, every local source is "local", any other IPv6 source is its /64 (a client can
// rotate within its prefix), anything else is the address itself. Text that is not an address is
// returned unchanged.
func sourceKey(host string) string {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.Unmap().WithZone("")
	if a.IsLoopback() || localAddrs.has(a) {
		return localSourceKey
	}
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

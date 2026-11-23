package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/db"
)

// Egress policy for WebFetch and the custom http tool (Phase D.1, design D1/D6).
//
// Every destination is checked three times: the tool wrapper (or the custom tool)
// checks the initial URL, CheckRedirect checks every hop, and the dial hook dials
// exactly the addresses the check compared. The name is resolved locally on every
// path and the denylist, block rules and allow rules run on the result. The pin
// applies to DIRECT dials only: when a proxy connects to the destination, the proxy
// resolves the name itself, so a rebinding answer there, or a name only a remote
// proxy can resolve, is not covered (see TargetUnresolved). The decision codes
// below are what the model and the operator see.

// Decision codes. EgressBlocked and EgressNotAllowed come from db.EvaluateEgress.
const (
	TargetControlPlane = "TARGET_CONTROL_PLANE" // built-in denylist: metadata ranges, ARTEX's own ports
	TargetUnavailable  = "TARGET_POLICY_UNAVAILABLE"
	TargetUnresolved   = "TARGET_UNRESOLVED" // the name does not resolve locally and no remote-resolving proxy is configured
	TargetInvalid      = "TARGET_INVALID"    // the host, port or scheme cannot be read: refused, never guessed
)

const maxRedirectHops = 10 // net/http's default cap, re-implemented in CheckRedirect

// TargetRuleLoader returns the egress rules for one run: block = global rules plus
// the task's block rules, allow = the task's allow rules. taskID 0 (chat sessions)
// yields the global block rules only.
type TargetRuleLoader func(ctx context.Context, taskID int64) (block, allow []db.AssetInterceptRule, err error)

// ControlPlane describes the addresses D1 must never let WebFetch or the custom
// http tool reach. Resolver and InterfaceAddrs are injectable for tests; nil means
// the system resolver and net.InterfaceAddrs.
type ControlPlane struct {
	ListenAddr         string // ARTEX listen address (cmd/artex -addr), e.g. ":8787"
	RecordingProxyAddr string // recording proxy address (cmd/artex -proxy); empty when capture is off
	// DBEndpoints lists every (host, port) the database driver may connect to, from the DSN as
	// main read it once at startup (see DatabaseEndpoint); the environment no longer holds it.
	// DBCaptured true means DBEndpoints is final (it may be empty: a socket-only DSN). False
	// means the endpoints were never captured, and every fetch is refused: the policy cannot
	// say what to protect.
	DBEndpoints    []DBEndpoint
	DBCaptured     bool
	Resolver       func(ctx context.Context, host string) ([]netip.Addr, error)
	InterfaceAddrs func() ([]netip.Addr, error)
}

type targetPolicy struct {
	loader      TargetRuleLoader
	cp          ControlPlane
	globalProxy bool
}

var (
	targetPolicyMu sync.RWMutex
	curPolicy      targetPolicy
)

// SetTargetPolicy registers the rule loader, control-plane description and whether a
// global egress proxy is configured (which turns WebFetch's direct retry off). With a
// nil loader every fetch that passes the denylist is refused with TargetUnavailable
// (fail closed). It is safe for concurrent use; the server calls it at startup and again
// when the global proxy changes.
func SetTargetPolicy(loader TargetRuleLoader, cp ControlPlane, globalProxySet bool) {
	targetPolicyMu.Lock()
	defer targetPolicyMu.Unlock()
	curPolicy = targetPolicy{loader: loader, cp: cp, globalProxy: globalProxySet}
}

func currentTargetPolicy() targetPolicy {
	targetPolicyMu.RLock()
	defer targetPolicyMu.RUnlock()
	return curPolicy
}

// TargetError is a policy refusal. Code is one of db.EgressBlocked, db.EgressNotAllowed,
// TargetControlPlane, TargetUnavailable, TargetUnresolved or TargetInvalid. Its text carries the code and a reason, never
// the operator's rule list.
type TargetError struct {
	Code   string
	Reason string
	Hop    int // 0 = the initial URL, n = the n-th redirect
}

func (e *TargetError) Error() string {
	if e.Hop > 0 {
		return fmt.Sprintf("%s: %s (redirect hop %d)", e.Code, e.Reason, e.Hop)
	}
	return e.Code + ": " + e.Reason
}

// ---------- control-plane denylist ----------

var metadataPrefixes = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"), // link-local incl. cloud metadata and ECS 169.254.170.2
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba
	netip.MustParsePrefix("168.63.129.16/32"),   // Azure
}

const denyReason = "destination is an ARTEX control-plane address or a cloud metadata service"

// denylist is the per-fetch snapshot of what must never be reached: metadata ranges,
// (local address, ARTEX port) pairs and the database. Built once per fetch so
// interface and DSN lookups are not repeated per hop.
type denylist struct {
	ports   map[uint16]bool            // ARTEX listen, recording proxy and every database port, denied on local addresses
	local   []netip.Addr               // interface addresses
	pairs   map[netip.AddrPort]bool    // listener literals and each database host's addresses on its own port
	dbNames map[string]map[uint16]bool // normalised database host name -> its port(s)
}

func (d *denylist) isLocal(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || (ip.Is4() && ip.As4()[0] == 0) {
		return true
	}
	for _, l := range d.local {
		if l == ip {
			return true
		}
	}
	return false
}

// check reports whether ip:port is denied, testing every embedded-IPv4 form.
func (d *denylist) check(ip netip.Addr, port uint16) bool {
	for _, f := range db.EgressIPForms(ip) {
		for _, p := range metadataPrefixes {
			if p.Contains(f) {
				return true
			}
		}
		if d.ports[port] && d.isLocal(f) {
			return true
		}
		if d.pairs[netip.AddrPortFrom(f, port)] {
			return true
		}
	}
	return false
}

// checkName covers a destination named like a DSN host on that host's port even when it
// does not resolve.
func (d *denylist) checkName(host string, port uint16) bool {
	return d.dbNames[host][port]
}

func splitHostPortLoose(addr string) (host string, port uint16, ok bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", 0, false
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		h, p = "", addr // a bare port such as "8787"
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return "", 0, false
	}
	return strings.TrimSpace(h), uint16(n), true
}

func systemInterfaceAddrs() ([]netip.Addr, error) {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr.WithZone("").Unmap())
		}
	}
	return out, nil
}

func systemResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// DBEndpoint is one address the database driver may connect to.
type DBEndpoint struct {
	Host string // name or address literal as the DSN spells it, or a unix-socket path
	Port uint16
}

// DatabaseEndpoint returns every (host, port) the application's driver would try for dsn:
// the primary and each fallback host, with the port that belongs to it. A unix-socket host
// (a path) stays in the list: it is no network endpoint, but the database then runs on this
// host and usually listens on TCP loopback as well, so its port is denied on local addresses. It parses the DSN
// with pgconn.ParseConfig, the parser db.Open connects with, so URL and keyword/value
// forms, comma-separated host and port lists and the PG* environment defaults mean the
// same thing here and there. Duplicates (pgx's TLS fallback repeats the host) are merged.
// The error text never carries the DSN.
func DatabaseEndpoint(dsn string) ([]DBEndpoint, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("database dsn is not parseable")
	}
	all := []DBEndpoint{{cfg.Host, cfg.Port}}
	for _, fb := range cfg.Fallbacks {
		all = append(all, DBEndpoint{fb.Host, fb.Port})
	}
	var out []DBEndpoint
	seen := map[DBEndpoint]bool{}
	for _, e := range all {
		if e.Host == "" || e.Port == 0 {
			continue
		}
		k := DBEndpoint{e.Host, e.Port}
		if !isSocketHost(e.Host) {
			k.Host = db.NormalizeEgressHost(e.Host)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	return out, nil
}

func isSocketHost(h string) bool { return strings.HasPrefix(h, "/") || strings.HasPrefix(h, "@") }

// errDBEndpointsNotCaptured is what dsnEndpoints reports when startup never captured the
// endpoints. There is no runtime fallback to the DSN: the environment no longer holds it, and
// a read that finds nothing would silently drop the database from the denylist.
var errDBEndpointsNotCaptured = errors.New("database endpoints were not captured at startup")

// dsnEndpoints returns the database endpoints the denylist protects: the ones captured in the
// control plane at startup. It fails when none were captured.
func dsnEndpoints(cp ControlPlane) ([]DBEndpoint, error) {
	if !cp.DBCaptured {
		return nil, errDBEndpointsNotCaptured
	}
	return cp.DBEndpoints, nil
}

func buildDenylist(ctx context.Context, cp ControlPlane, ri RunInfo) (*denylist, error) {
	d := &denylist{ports: map[uint16]bool{}, pairs: map[netip.AddrPort]bool{}, dbNames: map[string]map[uint16]bool{}}
	ifaces := cp.InterfaceAddrs
	if ifaces == nil {
		ifaces = systemInterfaceAddrs
	}
	local, err := ifaces()
	if err != nil {
		return nil, fmt.Errorf("enumerate local interfaces: %w", err)
	}
	for _, a := range local {
		d.local = append(d.local, a.WithZone("").Unmap())
	}
	for _, addr := range []string{cp.ListenAddr, cp.RecordingProxyAddr} {
		host, port, ok := splitHostPortLoose(addr)
		if !ok {
			continue
		}
		d.ports[port] = true
		if ip, err := netip.ParseAddr(host); err == nil {
			d.pairs[netip.AddrPortFrom(ip.WithZone("").Unmap(), port)] = true
		}
	}
	endpoints, err := dsnEndpoints(cp)
	if err != nil {
		return nil, err
	}
	resolve := cp.Resolver
	if resolve == nil {
		resolve = systemResolver
	}
	resolved := map[string][]netip.Addr{} // one lookup per host name, whatever its ports
	for _, e := range endpoints {
		d.ports[e.Port] = true // every database port is denied on local addresses
		if isSocketHost(e.Host) {
			continue // no network name or address behind a socket path
		}
		name := db.NormalizeEgressHost(e.Host)
		if d.dbNames[name] == nil {
			d.dbNames[name] = map[uint16]bool{}
		}
		d.dbNames[name][e.Port] = true
		ips, seen := resolved[name]
		if !seen {
			if ip, err := netip.ParseAddr(name); err == nil {
				ips = []netip.Addr{ip.WithZone("").Unmap()}
			} else if rs, err := resolve(ctx, name); err == nil {
				for _, ip := range rs {
					ips = append(ips, ip.WithZone("").Unmap())
				}
			} else {
				// Without the address only the name and the local-address pairs remain denied.
				log.Printf("[webfetch-policy] db_host_unresolved task=%d", ri.TaskID)
			}
			resolved[name] = ips
		}
		for _, ip := range ips {
			d.pairs[netip.AddrPortFrom(ip, e.Port)] = true
		}
	}
	return d, nil
}

// ---------- guard ----------

type fetchStateKey struct{}

// fetchState is the policy state of one tool call: what was checked, which addresses
// the dial may use, and the rules. It travels in the request context so redirect and
// dial hooks reach it; it is mutated only between requests of one fetch.
type fetchState struct {
	ri      RunInfo
	deny    *denylist
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	pol     targetPolicy

	rulesLoaded  bool
	block, allow []db.AssetInterceptRule

	mu          sync.Mutex
	checked     map[string][]netip.Addr // normalised host:port -> checked addresses
	envProxyKey string                  // the environment proxy the latest checked request would use
	retryLogged bool
}

// TargetGuard enforces the egress policy for one HTTP client. Create it with the same
// proxy configuration the client uses, install CheckRedirect and DialContext on the
// client, and call Check with the initial URL before the first request. The dial pin
// covers direct dials only; a proxy re-resolves names itself (see DialContext).
type TargetGuard struct {
	hasProxy bool   // a proxy URL is configured
	proxyKey string // canonical host:port of that proxy; "" if unparseable
	envProxy bool   // the client's transport may consult http.ProxyFromEnvironment
	dialer   net.Dialer
}

// NewTargetGuard returns a guard for a client that uses proxyURL ("" = none) and, when
// envProxy is true, http.ProxyFromEnvironment. Only the configured proxy address, or the
// environment proxy of the very request being dialled, may be dialled without a check.
func NewTargetGuard(proxyURL string, envProxy bool) *TargetGuard {
	g := &TargetGuard{hasProxy: proxyURL != "", envProxy: envProxy, dialer: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}
	if pu, err := url.Parse(proxyURL); err == nil && proxyURL != "" {
		g.proxyKey = proxyDialKey(pu)
	}
	return g
}

var proxyDefaultPorts = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}

// proxyDialKey is the host:port net/http dials for a proxy URL.
func proxyDialKey(pu *url.URL) string {
	port := pu.Port()
	if port == "" {
		port = proxyDefaultPorts[strings.ToLower(pu.Scheme)]
	} else if n, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(n, 10) // the dial key carries the number, not its spelling
	}
	return db.NormalizeEgressHost(pu.Hostname()) + ":" + port
}

func stateFrom(ctx context.Context) *fetchState {
	st, _ := ctx.Value(fetchStateKey{}).(*fetchState)
	return st
}

// Check applies the policy to the initial URL and returns a context that carries the
// policy state to the redirect and dial hooks. Use that context for the request. Check
// rewrites u to its canonical authority (ASCII host, numeric port): send u, not the text it
// was parsed from. A refusal is a *TargetError; any other error means the destination could not be
// examined (bad URL, unresolvable name).
func (g *TargetGuard) Check(ctx context.Context, u *url.URL) (context.Context, error) {
	pol := currentTargetPolicy()
	ri := RunInfoFrom(ctx)
	st := &fetchState{ri: ri, pol: pol, resolve: pol.cp.Resolver, checked: map[string][]netip.Addr{}}
	if st.resolve == nil {
		st.resolve = systemResolver
	}
	ctx = context.WithValue(ctx, fetchStateKey{}, st)
	if ri.TaskID == 0 && ri.ExplorationID > 0 {
		return ctx, st.refuse(0, TargetUnavailable, "the run has an exploration but no task id, so its rules cannot be selected")
	}
	deny, err := buildDenylist(ctx, pol.cp, ri)
	if err != nil {
		if errors.Is(err, errDBEndpointsNotCaptured) {
			log.Printf("[webfetch-policy] refusing fetches: %v", err) // a constant text, no DSN part
		}
		// The detail may name hosts or DSN parts; the model gets a fixed reason.
		return ctx, st.refuse(0, TargetUnavailable, "control-plane address list unavailable")
	}
	st.deny = deny
	if err := g.checkURL(ctx, st, u, 0); err != nil {
		return ctx, err
	}
	return ctx, nil
}

func (st *fetchState) refuse(hop int, code, reason string) error {
	log.Printf("[webfetch-policy] decision=%s hop=%d task=%d", code, hop, st.ri.TaskID)
	return &TargetError{Code: code, Reason: reason, Hop: hop}
}

// loadRules reads the rules once per fetch. Chat sessions (task id 0) get the global
// block rules only: there are no task allow rules. No registered loader is an error:
// the policy fails closed. The caller reports a fixed reason; the cause (a database
// error may carry host or credential text) is not passed on.
func (st *fetchState) loadRules(ctx context.Context) error {
	if st.rulesLoaded {
		return nil
	}
	if st.pol.loader == nil {
		return errors.New("no rule loader registered")
	}
	block, allow, err := st.pol.loader(ctx, st.ri.TaskID)
	if err != nil {
		return err
	}
	if st.ri.TaskID == 0 {
		allow = nil
	}
	st.block, st.allow, st.rulesLoaded = block, allow, true
	return nil
}

func targetPort(u *url.URL) (uint16, error) {
	def := uint16(0)
	switch strings.ToLower(u.Scheme) {
	case "http":
		def = 80
	case "https":
		def = 443
	default:
		// With or without an explicit port: only http and https are fetched.
		return 0, errUnsupportedScheme
	}
	p := u.Port()
	if p == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return 0, errInvalidPort
	}
	return uint16(n), nil
}

// Fixed reasons of TargetInvalid: they reach the model, so they carry no destination content.
var (
	errInvalidHost       = errors.New("the destination host is not valid")
	errInvalidPort       = errors.New("the destination port is not valid")
	errUnsupportedScheme = errors.New("the destination url scheme is not supported")
)

// canonicalizeURL rewrites u's authority to the one form the check, the dial key and the
// wire share: ASCII lower-case host without trailing dots (db.CanonicalEgressHost) and a
// port without leading zeros. net/http would otherwise serialise a Unicode or
// compatibility spelling its own way (IDNA applied again, per path), and the policy would
// have judged a different string than the one sent. A host that cannot be canonicalised
// and a port that is not a number are refused. The caller must send the rewritten u.
func canonicalizeURL(u *url.URL) error {
	host, ok := db.CanonicalEgressHost(u.Hostname())
	if !ok || host == "" {
		return errInvalidHost
	}
	port, err := targetPort(u)
	if err != nil {
		return err
	}
	authority := host
	if strings.Contains(host, ":") { // IPv6 literal
		authority = "[" + host + "]"
	}
	if u.Port() != "" {
		authority += ":" + strconv.Itoa(int(port))
	}
	u.Host = authority
	return nil
}

func (g *TargetGuard) envProxyFor(u *url.URL) string {
	if !g.envProxy {
		return ""
	}
	pu, err := http.ProxyFromEnvironment(&http.Request{URL: u})
	if err != nil || pu == nil {
		return ""
	}
	return proxyDialKey(pu)
}

// checkURL runs denylist, block rules and allow rules for one URL (initial request or
// redirect hop) and records the addresses the dial may use for it. It rewrites u to its
// canonical authority first (see canonicalizeURL), so everything below judges the string
// the request will carry.
func (g *TargetGuard) checkURL(ctx context.Context, st *fetchState, u *url.URL, hop int) error {
	if err := canonicalizeURL(u); err != nil {
		return st.refuse(hop, TargetInvalid, err.Error())
	}
	host := u.Hostname()
	port, err := targetPort(u)
	if err != nil {
		return st.refuse(hop, TargetInvalid, err.Error())
	}
	envKey := g.envProxyFor(u)

	// Resolve the name net/http will put on the wire (canonical, see canonicalizeURL)
	// on every path, and check what it resolves to.
	var ips []netip.Addr
	if lit, perr := netip.ParseAddr(host); perr == nil {
		ips = []netip.Addr{lit.WithZone("").Unmap()}
	} else {
		resolved, rerr := st.resolve(ctx, host)
		switch {
		case rerr == nil && len(resolved) > 0:
			for _, ip := range resolved {
				ips = append(ips, ip.WithZone("").Unmap())
			}
		case st.pol.globalProxy || envKey != "":
			// An upstream proxy may resolve names we cannot (remote DNS); only the name checks
			// apply. Documented residual: such a name can resolve to a denied address there.
		default:
			return st.refuse(hop, TargetUnresolved, "the destination name does not resolve, so it was not fetched")
		}
	}

	if st.deny.checkName(host, port) {
		return st.refuse(hop, TargetControlPlane, denyReason)
	}
	for _, ip := range ips {
		if st.deny.check(ip, port) {
			return st.refuse(hop, TargetControlPlane, denyReason)
		}
	}
	if err := st.loadRules(ctx); err != nil {
		return st.refuse(hop, TargetUnavailable, "policy rules unavailable")
	}
	d := db.EvaluateEgress(st.block, st.allow, u, ips)
	if !d.Allowed {
		return st.refuse(hop, d.Code, d.Reason)
	}
	if d.AllowedIPs != nil {
		// Allowed by IP/CIDR rules only: dial just the addresses those rules name.
		var pin []netip.Addr
		for _, ip := range ips {
			for _, a := range d.AllowedIPs {
				if ip == a {
					pin = append(pin, ip)
					break
				}
			}
		}
		ips = pin
	}

	st.mu.Lock()
	st.checked[host+":"+strconv.Itoa(int(port))] = ips
	st.envProxyKey = envKey
	st.mu.Unlock()
	return nil
}

// TaskAllowMatches reports whether u matches an enabled TASK allow rule. ctx must be the
// context Check returned for this call. Global rules are block-only and chat sessions
// (task id 0) have no task allow rules, so both yield false; so does a context without
// policy state. It matches against the addresses Check compared.
func (g *TargetGuard) TaskAllowMatches(ctx context.Context, u *url.URL) bool {
	st := stateFrom(ctx)
	if st == nil || st.ri.TaskID == 0 || u == nil {
		return false
	}
	var enabled []db.AssetInterceptRule
	for _, r := range st.allow {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	if len(enabled) == 0 {
		return false
	}
	port, err := targetPort(u)
	if err != nil {
		return false
	}
	st.mu.Lock()
	ips := st.checked[db.NormalizeEgressHost(u.Hostname())+":"+strconv.Itoa(int(port))]
	st.mu.Unlock()
	return db.EvaluateEgress(nil, enabled, u, ips).Allowed
}

// CheckRedirect is the http.Client CheckRedirect: net/http's 10-hop cap, then the
// policy on the hop's URL, which it rewrites to the canonical authority. Clients that need more (header stripping) call it first.
func (g *TargetGuard) CheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirectHops {
		return errors.New("stopped after 10 redirects")
	}
	st := stateFrom(req.Context())
	if st == nil {
		return &TargetError{Code: TargetUnavailable, Reason: "redirect without policy state", Hop: len(via)}
	}
	if err := g.checkURL(req.Context(), st, req.URL, len(via)); err != nil {
		return err
	}
	if req.Host != "" { // the wire Host header wins over URL.Host: keep it the canonical form
		req.Host = req.URL.Host
	}
	return nil
}

// DialContext dials exactly the addresses Check or CheckRedirect compared for addr, never
// the name (direct dials only). An address that was not checked is refused, except the
// configured proxy and the environment proxy of the request being served, which are
// dialled as given. Through ANY proxy (recording, per-tool, global, environment) only
// the proxy address is dialled: the proxy re-resolves the name, so DNS rebinding and
// IP-based allow rules are not enforced on that leg (residual).
func (g *TargetGuard) DialContext(ctx context.Context, network, addr string) (conn net.Conn, err error) {
	// The transport dials on its own goroutine, where a panic would end the process.
	defer func() {
		if r := recover(); r != nil {
			conn, err = nil, fmt.Errorf("%s: dial hook failed: %v", TargetUnavailable, r)
		}
	}()
	st := stateFrom(ctx)
	if st == nil {
		return nil, fmt.Errorf("%s: dial without policy state", TargetUnavailable)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot parse dial address: %w", TargetUnavailable, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot parse dial port: %w", TargetUnavailable, err)
	}
	key := db.NormalizeEgressHost(host) + ":" + strconv.FormatUint(port, 10)

	st.mu.Lock()
	ips, pinned := st.checked[key]
	envKey := st.envProxyKey
	st.mu.Unlock()

	switch {
	case pinned:
		if g.hasProxy && key != g.proxyKey {
			st.logDirectRetry()
		}
		return g.dialPinned(ctx, network, st, ips, uint16(port))
	case g.proxyKey != "" && key == g.proxyKey:
		return g.dialer.DialContext(ctx, network, addr)
	case envKey != "" && key == envKey:
		if g.hasProxy {
			st.logDirectRetry()
		}
		return g.dialer.DialContext(ctx, network, addr)
	}
	return nil, fmt.Errorf("%s: refusing to dial an address the egress policy did not check", TargetUnavailable)
}

func (st *fetchState) logDirectRetry() {
	st.mu.Lock()
	first := !st.retryLogged
	st.retryLogged = true
	st.mu.Unlock()
	if first {
		log.Printf("[webfetch] direct_retry task=%d", st.ri.TaskID)
	}
}

func (g *TargetGuard) dialPinned(ctx context.Context, network string, st *fetchState, ips []netip.Addr, port uint16) (net.Conn, error) {
	var last error
	for _, ip := range ips {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if st.deny.check(ip, port) { // defence in depth: the list the check used
			last = fmt.Errorf("%s: %s", TargetControlPlane, denyReason)
			continue
		}
		c, err := g.dialer.DialContext(ctx, network, netip.AddrPortFrom(ip, port).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("%s: no checked address to dial", TargetUnavailable)
	}
	return nil, last
}

// ---------- WebFetch wrapper ----------

// policyWebFetch is the SDK's WebFetch with the initial-URL check in front. Name,
// schema and description are the SDK's; the redirect and dial hooks are installed in
// the config the inner tool was built with.
type policyWebFetch struct {
	actool.CoreTool
	guard *TargetGuard
}

func (w *policyWebFetch) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	var in struct {
		URL string `json:"url"`
	}
	// Input the inner tool refuses without touching the network (bad JSON, other scheme)
	// is left to it; a dial without policy state is refused anyway.
	if err := json.Unmarshal(input, &in); err != nil || !(strings.HasPrefix(in.URL, "http://") || strings.HasPrefix(in.URL, "https://")) {
		return w.CoreTool.Call(ctx, input, tc)
	}
	u, err := url.Parse(in.URL)
	if err != nil {
		return actool.Errorf("Error: " + err.Error()), nil
	}
	ctx, err = w.guard.Check(ctx, u)
	if err != nil {
		var te *TargetError
		if errors.As(err, &te) {
			return actool.Errorf(te.Error()), nil
		}
		return actool.Errorf("Error fetching URL: " + err.Error()), nil
	}
	// The tool must fetch the URL the policy judged: u now carries the canonical authority.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return actool.Errorf("Error fetching URL: invalid input"), nil
	}
	if fields["url"], err = json.Marshal(u.String()); err != nil {
		return actool.Errorf("Error fetching URL: invalid input"), nil
	}
	if input, err = json.Marshal(fields); err != nil {
		return actool.Errorf("Error fetching URL: invalid input"), nil
	}
	return w.CoreTool.Call(ctx, input, tc)
}

// attachWebFetchPolicy replaces the SDK's WebFetch with a policy-checked, panic-guarded
// one (design D1): it clears opts.EnableWebFetch and appends guardPanic(policyWebFetch).
// The direct retry is switched off whenever a global egress proxy is configured (D6).
//
// It never invents a tool list: with a nil list it returns errNilToolList, so each caller passes
// its list explicitly. (It used to substitute the SDK's default tools, which put a shell and the
// file tools into a session that never asked for them.)
func attachWebFetchPolicy(opts *agentcore.Options) error {
	if !opts.EnableWebFetch {
		return nil
	}
	if opts.Tools == nil {
		return errNilToolList
	}
	pol := currentTargetPolicy()
	// The primary client consults the environment proxy only without Proxy/CACert/InsecureTLS;
	// the direct-retry client (built without Proxy and CACert) whenever TLS is not skipped.
	envProxy := !opts.WebFetchInsecureTLS && (opts.WebFetchProxy != "" || opts.WebFetchCACert == "")
	guard := NewTargetGuard(opts.WebFetchProxy, envProxy)
	inner := actool.NewWebFetch(actool.WebFetchConfig{
		Proxy:              opts.WebFetchProxy,
		CACert:             opts.WebFetchCACert,
		InsecureTLS:        opts.WebFetchInsecureTLS,
		CheckRedirect:      guard.CheckRedirect,
		DialContext:        guard.DialContext,
		DisableDirectRetry: opts.WebFetchDisableDirectRetry || pol.globalProxy,
	})
	opts.Tools = append(append([]actool.CoreTool(nil), opts.Tools...), guardPanic(&policyWebFetch{CoreTool: inner, guard: guard}))
	opts.EnableWebFetch = false
	return nil
}

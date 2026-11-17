package db

import (
	"net/netip"
	"net/url"
	"path"
	"strings"

	"golang.org/x/net/idna"
)

// Egress rule codes returned by EvaluateEgress; the agent layer surfaces them to
// the model verbatim (design D1).
const (
	EgressBlocked    = "TARGET_BLOCKED"
	EgressNotAllowed = "TARGET_NOT_ALLOWED"
)

// EgressDecision is the result of the destination gate for one outbound URL.
type EgressDecision struct {
	Allowed bool
	Code    string // EgressBlocked / EgressNotAllowed; empty when Allowed
	Reason  string // rejection reason; empty when Allowed
	// AllowedIPs, when non-nil, restricts the addresses a dial may use to these: the
	// allow decision came from IP/CIDR rules only. nil = every resolved address.
	AllowedIPs []netip.Addr
}

// NormalizeEgressHost returns the canonical form egress rules and the dial pin compare
// on: trimmed, trailing dots stripped, lower case, IDNA to ASCII (the same Lookup
// profile net/http applies before dialling). A name IDNA rejects keeps its lower-case
// spelling, so it can only ever equal itself. IP literals are canonicalised
// (v4-mapped unmapped, zone dropped).
func NormalizeEgressHost(host string) string {
	host = strings.TrimSpace(host)
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.WithZone("").Unmap().String()
	}
	host = strings.ToLower(strings.TrimRight(host, "."))
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	}
	return strings.ToLower(host)
}

// EgressIPForms returns the address itself (v4-mapped unmapped, zone dropped) followed
// by every IPv4 address embedded in it: NAT64 64:ff9b::/96, 6to4 2002::/16 and the
// deprecated IPv4-compatible ::a.b.c.d. Callers that must not be bypassed by an
// address spelling (denylist, block rules) test every form.
func EgressIPForms(ip netip.Addr) []netip.Addr {
	if !ip.IsValid() {
		return nil
	}
	ip = ip.WithZone("").Unmap()
	forms := []netip.Addr{ip}
	if !ip.Is6() {
		return forms
	}
	b := ip.As16()
	switch {
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]):
		forms = append(forms, netip.AddrFrom4([4]byte(b[12:16])))
	case b[0] == 0x20 && b[1] == 0x02:
		forms = append(forms, netip.AddrFrom4([4]byte(b[2:6])))
	case allZero(b[0:12]):
		forms = append(forms, netip.AddrFrom4([4]byte(b[12:16])))
	}
	return forms
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// EvaluateEgress is the URL-aware destination matcher for WebFetch and the custom
// http tool (design D1, AC3): domain rules match the normalised parsed hostname,
// URL rules match scheme+host+port+path prefix, IP and CIDR rules match the host
// literal and every resolved IP. Block rules win; allow rules gate only when at
// least one is enabled and are satisfied by any single rule (host, URL or IP). When
// only IP/CIDR allow rules were satisfied, AllowedIPs lists the addresses that
// matched. Block rules additionally test every embedded-IPv4 form of each address.
// A URL rule without scheme and host (path-only, e.g. "/admin") applies to block
// rules only: exact_url is a case-insensitive string prefix of the cleaned path,
// fuzzy_url a case-insensitive substring of the cleaned path plus query. Allow rules
// of that shape never match.
// EvaluateAssetGate keeps its own (substring) semantics; this matcher is separate.
func EvaluateEgress(block, allow []AssetInterceptRule, u *url.URL, ips []netip.Addr) EgressDecision {
	if u == nil {
		return EgressDecision{Code: EgressBlocked, Reason: "no destination url"}
	}
	host := NormalizeEgressHost(u.Hostname())
	var blockIPs, allowIPs []netip.Addr
	add := func(ip netip.Addr) {
		if !ip.IsValid() {
			return
		}
		blockIPs = append(blockIPs, EgressIPForms(ip)...)
		allowIPs = append(allowIPs, ip.WithZone("").Unmap())
	}
	if lit, err := netip.ParseAddr(u.Hostname()); err == nil {
		add(lit)
	}
	for _, ip := range ips {
		add(ip)
	}

	for _, r := range block {
		if r.Enabled && egressRuleMatches(r, host, u, blockIPs, true) {
			return EgressDecision{Code: EgressBlocked, Reason: r.Reason()}
		}
	}
	gated, hostOrURLMatch := false, false
	var ipMatches []netip.Addr
	for _, r := range allow {
		if !r.Enabled {
			continue
		}
		gated = true
		switch r.Kind {
		case "exact_ip", "fuzzy_ip", "cidr":
			// An IP/CIDR rule is satisfied address by address; remember which ones.
			for _, ip := range allowIPs {
				if egressRuleMatches(r, host, u, []netip.Addr{ip}, false) && !containsAddr(ipMatches, ip) {
					ipMatches = append(ipMatches, ip)
				}
			}
		default:
			if egressRuleMatches(r, host, u, allowIPs, false) {
				hostOrURLMatch = true
			}
		}
	}
	switch {
	case !gated, hostOrURLMatch:
		return EgressDecision{Allowed: true}
	case len(ipMatches) > 0:
		// Only the addresses an allow rule names may be dialled, not their siblings.
		return EgressDecision{Allowed: true, AllowedIPs: ipMatches}
	}
	return EgressDecision{Code: EgressNotAllowed, Reason: "destination is outside this task's allow rules"}
}

func containsAddr(list []netip.Addr, a netip.Addr) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

// egressRuleMatches reports whether one enabled rule covers the destination. fold is
// true for block rules: URL path matching is then case-insensitive and path-only URL
// rules apply (block rules err on the side of blocking).
func egressRuleMatches(r AssetInterceptRule, host string, u *url.URL, ips []netip.Addr, fold bool) bool {
	p := strings.TrimSpace(r.Pattern)
	if p == "" {
		return false
	}
	switch r.Kind {
	case "exact_domain":
		return host != "" && NormalizeEgressHost(p) == host
	case "fuzzy_domain":
		rule := NormalizeEgressHost(strings.TrimLeft(p, "."))
		return host != "" && rule != "" && (host == rule || strings.HasSuffix(host, "."+rule))
	case "exact_ip":
		want, err := netip.ParseAddr(p)
		if err != nil {
			return false
		}
		want = want.WithZone("").Unmap()
		for _, ip := range ips {
			if ip == want {
				return true
			}
		}
	case "fuzzy_ip":
		// Octet/group-boundary prefix: "10.1" and "10.1." match 10.1.x.x, not 10.10.x.x.
		prefix := strings.TrimRight(strings.ToLower(p), ".:")
		if prefix == "" {
			return false
		}
		for _, ip := range ips {
			s := ip.String()
			if s == prefix || strings.HasPrefix(s, prefix+".") || strings.HasPrefix(s, prefix+":") {
				return true
			}
		}
	case "cidr":
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			return false
		}
		for _, ip := range ips {
			if pfx.Contains(ip) || pfx.Contains(ip.Unmap()) {
				return true
			}
		}
	case "exact_url", "fuzzy_url":
		if !strings.Contains(p, "://") {
			return egressPathOnlyMatches(r.Kind, p, u, fold)
		}
		return egressURLMatches(p, u, fold)
	}
	return false
}

// egressPathOnlyMatches handles URL rules written without scheme and host. They
// block (block=true) and never allow, so a malformed allow rule cannot widen scope.
func egressPathOnlyMatches(kind, pattern string, u *url.URL, block bool) bool {
	if !block {
		return false
	}
	p := strings.ToLower(pattern)
	up := egressPath(u.Path, true)
	uq := strings.ToLower(u.RawQuery)
	// A pattern that starts with a host ("evil.com/admin") must match that host on a
	// label boundary and the rest against the path (and, fuzzy, the query).
	if i := strings.Index(p, "/"); i > 0 {
		if hostPart, rest := p[:i], p[i:]; egressHostMatches(hostPart, u) {
			if kind == "exact_url" {
				return strings.HasPrefix(up, egressPath(rest, true))
			}
			return strings.Contains(up+"?"+uq, rest)
		}
	}
	// A bare host ("evil.com", "[::1]:8080") typed as a URL rule blocks that host, as the
	// substring matcher did; the path-only reading below still applies as well.
	if !strings.Contains(p, "/") && egressLooksLikeHost(p) && egressHostMatches(p, u) {
		return true
	}
	if kind == "exact_url" {
		return strings.HasPrefix(up, egressPath(p, true))
	}
	return strings.Contains(up+"?"+uq, p)
}

// egressLooksLikeHost reports whether a schemeless pattern without a slash is a host name
// or address rather than a path or query fragment such as "secret" or "token=".
func egressLooksLikeHost(p string) bool {
	if strings.ContainsAny(p, "=?&# ") {
		return false
	}
	return strings.Contains(p, ".") || strings.Contains(p, ":")
}

// egressHostMatches reports whether the host[:port] text equals the URL's normalised host
// or is a parent domain of it (label boundary); a port in the text must match too.
func egressHostMatches(hostPart string, u *url.URL) bool {
	h, port := hostPart, ""
	switch {
	case strings.HasPrefix(hostPart, "["): // bracketed IPv6 literal, optional :port
		end := strings.Index(hostPart, "]")
		if end < 0 {
			return false
		}
		h = hostPart[1:end]
		if rest := hostPart[end+1:]; strings.HasPrefix(rest, ":") {
			port = rest[1:]
		} else if rest != "" {
			return false
		}
	case strings.Count(hostPart, ":") == 1:
		i := strings.LastIndex(hostPart, ":")
		h, port = hostPart[:i], hostPart[i+1:]
	}
	h = NormalizeEgressHost(h)
	if h == "" || strings.ContainsAny(h, "=?&") {
		return false
	}
	if port != "" && port != egressPort(u) {
		return false
	}
	uh := NormalizeEgressHost(u.Hostname())
	if uh == h {
		return true
	}
	// The parent-domain reading is for names only: "0.1.1" must not block 10.0.1.1.
	if _, err := netip.ParseAddr(uh); err == nil {
		return false
	}
	return strings.HasSuffix(uh, "."+h)
}

// egressURLMatches compares a URL rule with the destination on scheme, normalised host,
// port (default ports implied) and path prefix. Userinfo, query and fragment are
// ignored, so text of the rule smuggled into them never matches.
func egressURLMatches(pattern string, u *url.URL, fold bool) bool {
	ru, err := url.Parse(pattern)
	if err != nil || ru.Scheme == "" || ru.Hostname() == "" {
		return false
	}
	if !strings.EqualFold(ru.Scheme, u.Scheme) {
		return false
	}
	if NormalizeEgressHost(ru.Hostname()) != NormalizeEgressHost(u.Hostname()) {
		return false
	}
	if egressPort(ru) != egressPort(u) {
		return false
	}
	rp, up := egressPath(ru.Path, fold), egressPath(u.Path, fold)
	return rp == "/" || up == rp || strings.HasPrefix(up, rp+"/")
}

func egressPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// egressPath resolves dot segments and doubled slashes, so /public/../admin and
// //admin are compared as /admin.
func egressPath(p string, fold bool) string {
	p = path.Clean("/" + p)
	if fold {
		p = strings.ToLower(p)
	}
	return p
}

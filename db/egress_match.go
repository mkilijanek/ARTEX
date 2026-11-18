package db

import (
	"net/netip"
	"net/url"
	"path"
	"strconv"
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

// CanonicalEgressHost returns the form egress rules, the dial pin and the request itself
// use: trimmed, IDNA to ASCII (the Lookup profile net/http applies to a host that is not
// plain ASCII), lower case, trailing dots stripped AFTER the mapping (U+3002 and friends
// map to "."). A name that maps to an IP literal is returned as that address (v4-mapped
// unmapped, zone dropped). ok is false when the name cannot be canonicalised (IDNA
// rejects it, it maps to a delimiter, or it is empty); the returned text is then only
// the lower-case spelling and callers must refuse the destination.
func CanonicalEgressHost(host string) (canonical string, ok bool) {
	host = strings.TrimSpace(host)
	canon := func(h string) (string, bool) {
		if ip, err := netip.ParseAddr(h); err == nil {
			return ip.WithZone("").Unmap().String(), true
		}
		return h, false
	}
	if c, isIP := canon(host); isIP {
		return c, true
	}
	ok = true
	if !isASCII(host) {
		ascii, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return strings.ToLower(strings.TrimRight(host, ".")), false
		}
		host = ascii
	}
	host = strings.ToLower(strings.TrimRight(host, "."))
	if c, isIP := canon(host); isIP {
		return c, true
	}
	if endsInNumber(host) {
		// A host whose last label is a number is an IPv4 address to getaddrinfo (and to the
		// WHATWG URL parser): "2130706433", "0x7f.1", "127.1" and "0177.0.0.1" are 127.0.0.1.
		// A resolver behind a proxy would dial it, so it is judged and dialled as that address.
		if ip, ok := parseLegacyIPv4(host); ok {
			return ip.String(), true
		}
		return host, false
	}
	if host == "" || strings.ContainsAny(host, "/\\?#@[]% \t\r\n") {
		ok = false
	}
	return host, ok
}

// endsInNumber reports whether the last label of host is all decimal digits or 0x followed
// by hex digits (possibly none).
func endsInNumber(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") {
		return strings.Trim(last[2:], "0123456789abcdef") == ""
	}
	return strings.Trim(last, "0123456789") == ""
}

// parseLegacyIPv4 parses 1 to 4 dot-separated numbers (decimal, octal with a leading 0, hex
// with 0x) as inet_aton does: every part but the last is one byte, the last fills the
// remaining bytes. Out-of-range or non-numeric parts fail.
func parseLegacyIPv4(host string) (netip.Addr, bool) {
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	var acc uint64
	for i, p := range parts {
		base := 10
		switch {
		case strings.HasPrefix(p, "0x"):
			base, p = 16, p[2:]
		case len(p) > 1 && p[0] == '0':
			base, p = 8, p[1:]
		}
		n := uint64(0)
		if p != "" || base != 16 {
			var err error
			if n, err = strconv.ParseUint(p, base, 32); err != nil {
				return netip.Addr{}, false
			}
		}
		if i < len(parts)-1 {
			if n > 255 {
				return netip.Addr{}, false
			}
			acc |= n << (8 * (3 - i))
		} else {
			if n >= 1<<(8*(4-i)) {
				return netip.Addr{}, false
			}
			acc |= n
		}
	}
	return netip.AddrFrom4([4]byte{byte(acc >> 24), byte(acc >> 16), byte(acc >> 8), byte(acc)}), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// NormalizeEgressHost is CanonicalEgressHost without the verdict: a name that cannot be
// canonicalised keeps its lower-case spelling, so a rule pattern can only ever equal
// itself. Destinations must go through CanonicalEgressHost and be refused when !ok.
func NormalizeEgressHost(host string) string {
	c, _ := CanonicalEgressHost(host)
	return c
}

// CanonicalEgressPort returns the decimal port of u without leading zeros, the scheme's
// default when none is given ("" for a scheme without one). ok is false when the port is
// not a number in 0..65535: such a destination must be refused and a rule that names such
// a port matches nothing.
func CanonicalEgressPort(u *url.URL) (port string, ok bool) {
	return canonicalPortText(u.Port(), u.Scheme)
}

func canonicalPortText(p, scheme string) (string, bool) {
	if p == "" {
		switch strings.ToLower(scheme) {
		case "http":
			return "80", true
		case "https":
			return "443", true
		}
		return "", true
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(n, 10), true
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
	if _, ok := CanonicalEgressPort(u); !ok {
		return EgressDecision{Code: EgressBlocked, Reason: "destination port is invalid"}
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
	if lit, err := netip.ParseAddr(host); err == nil { // also a name that maps to a literal
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
		if host == "" || rule == "" {
			return false
		}
		if host == rule || strings.HasSuffix(host, "."+rule) {
			return true
		}
		// A pattern ending in a number is normalised to an address ("0.1" is 0.0.0.1). A
		// block rule keeps blocking what its text suffix blocked; an allow rule does not.
		if raw := strings.ToLower(strings.Trim(p, ".")); fold && raw != "" && raw != rule {
			return host == raw || strings.HasSuffix(host, "."+raw)
		}
		return false
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
	if port != "" {
		want, ok := canonicalPortText(port, "")
		if have, hok := CanonicalEgressPort(u); !ok || !hok || want != have {
			return false
		}
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
	rport, rok := CanonicalEgressPort(ru)
	uport, uok := CanonicalEgressPort(u)
	if !rok || !uok || rport != uport {
		return false
	}
	rp, up := egressPath(ru.Path, fold), egressPath(u.Path, fold)
	return rp == "/" || up == rp || strings.HasPrefix(up, rp+"/")
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

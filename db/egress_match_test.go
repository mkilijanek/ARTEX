package db

import (
	"net/netip"
	"net/url"
	"testing"
)

func egressRule(kind, pattern string) AssetInterceptRule {
	return AssetInterceptRule{Enabled: true, Kind: kind, Pattern: pattern}
}

// T-MATCH-1 (AC3): the destination matcher works on the parsed URL, never on raw
// substrings. Every row below is a bypass the asset-gate substring matcher allows.
func TestEgressMatchParsedHostOnly(t *testing.T) {
	disabled := egressRule("exact_domain", "blocked.example")
	disabled.Enabled = false
	for _, tc := range []struct {
		name        string
		block       []AssetInterceptRule
		allow       []AssetInterceptRule
		rawURL      string
		ips         []string
		wantAllowed bool
		wantCode    string
	}{
		// --- allow rules: host must be the parsed hostname ---
		{"allow exact: userinfo smuggles the allowed name", nil, []AssetInterceptRule{egressRule("exact_domain", "allowed.example")}, "http://allowed.example@attacker.net/", nil, false, EgressNotAllowed},
		{"allow exact: allowed name only in query", nil, []AssetInterceptRule{egressRule("exact_domain", "allowed.example")}, "http://attacker.net/?allowed.example", nil, false, EgressNotAllowed},
		{"allow exact: allowed name only in path", nil, []AssetInterceptRule{egressRule("exact_domain", "allowed.example")}, "http://attacker.net/allowed.example", nil, false, EgressNotAllowed},
		{"allow exact: real host", nil, []AssetInterceptRule{egressRule("exact_domain", "allowed.example")}, "http://allowed.example/x", nil, true, ""},
		{"allow fuzzy: allowed name is a prefix label of an attacker domain", nil, []AssetInterceptRule{egressRule("fuzzy_domain", "allowed.example")}, "http://allowed.example.attacker.net/", nil, false, EgressNotAllowed},
		{"allow fuzzy: subdomain on a label boundary", nil, []AssetInterceptRule{egressRule("fuzzy_domain", "allowed.example")}, "http://www.allowed.example/", nil, true, ""},
		{"allow fuzzy: apex itself", nil, []AssetInterceptRule{egressRule("fuzzy_domain", "allowed.example")}, "http://allowed.example/", nil, true, ""},
		{"allow fuzzy: suffix that is not a label boundary", nil, []AssetInterceptRule{egressRule("fuzzy_domain", "allowed.example")}, "http://evilallowed.example/", nil, false, EgressNotAllowed},
		{"allow fuzzy leading dot: subdomain", nil, []AssetInterceptRule{egressRule("fuzzy_domain", ".allowed.example")}, "http://a.allowed.example/", nil, true, ""},
		{"allow fuzzy leading dot: attacker suffix", nil, []AssetInterceptRule{egressRule("fuzzy_domain", ".allowed.example")}, "http://allowed.example.attacker.net/", nil, false, EgressNotAllowed},
		// --- block rules: same parsing ---
		{"block exact: trailing dot and upper case", []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}, nil, "http://BLOCKED.example./x", nil, false, EgressBlocked},
		{"block exact: blocked name is the real host behind userinfo", []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}, nil, "http://attacker.net@blocked.example/", nil, false, EgressBlocked},
		{"block exact: blocked name only as userinfo", []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}, nil, "http://blocked.example@attacker.net/", nil, true, ""},
		{"block exact: blocked name only in path", []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}, nil, "http://attacker.net/blocked.example", nil, true, ""},
		{"block exact: blocked name is a prefix of an attacker domain", []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}, nil, "http://blocked.example.attacker.net/", nil, true, ""},
		{"block exact: port does not matter for a domain rule", []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}, nil, "http://blocked.example:8080/", nil, false, EgressBlocked},
		{"block fuzzy: label boundary only", []AssetInterceptRule{egressRule("fuzzy_domain", "gov.cn")}, nil, "http://evilgov.cn/", nil, true, ""},
		{"block fuzzy: subdomain", []AssetInterceptRule{egressRule("fuzzy_domain", "gov.cn")}, nil, "http://a.b.gov.cn/", nil, false, EgressBlocked},
		{"block fuzzy: rule as a prefix of an attacker domain", []AssetInterceptRule{egressRule("fuzzy_domain", "gov.cn")}, nil, "http://gov.cn.attacker.net/", nil, true, ""},
		{"block: disabled rule is ignored", []AssetInterceptRule{disabled}, nil, "http://blocked.example/", nil, true, ""},
		// --- IDNA: Unicode rule vs punycode URL and back ---
		{"idna: unicode rule, punycode url", []AssetInterceptRule{egressRule("exact_domain", "bücher.example")}, nil, "http://xn--bcher-kva.example/", nil, false, EgressBlocked},
		{"idna: unicode rule, unicode url", []AssetInterceptRule{egressRule("exact_domain", "bücher.example")}, nil, "http://bücher.example/", nil, false, EgressBlocked},
		{"idna: punycode rule, unicode url", []AssetInterceptRule{egressRule("exact_domain", "xn--bcher-kva.example")}, nil, "http://BÜCHER.example/", nil, false, EgressBlocked},
		// --- URL rules: scheme + host + port + path prefix ---
		{"url exact: same url", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080/admin", nil, false, EgressBlocked},
		{"url exact: path prefix", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080/admin/users", nil, false, EgressBlocked},
		{"url exact: host case and trailing dot", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://LAB.example.:8080/admin/x", nil, false, EgressBlocked},
		{"url exact: other scheme", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "https://lab.example:8080/admin", nil, true, ""},
		{"url exact: other port", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:9090/admin", nil, true, ""},
		{"url exact: other path", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080/public", nil, true, ""},
		{"url exact: rule text only in query", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080/?next=/admin", nil, true, ""},
		{"url exact: rule text embedded in an attacker query", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://attacker.net/?u=http://lab.example:8080/admin", nil, true, ""},
		{"url exact: userinfo smuggles the rule host", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080@attacker.net/admin", nil, true, ""},
		{"url exact: default port is implied", []AssetInterceptRule{egressRule("exact_url", "http://lab.example/admin")}, nil, "http://lab.example:80/admin", nil, false, EgressBlocked},
		{"url fuzzy: path prefix", []AssetInterceptRule{egressRule("fuzzy_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080/admin/users", nil, false, EgressBlocked},
		{"url fuzzy: userinfo smuggles the rule host", []AssetInterceptRule{egressRule("fuzzy_url", "http://lab.example:8080/admin")}, nil, "http://lab.example:8080@attacker.net/admin", nil, true, ""},
		{"url fuzzy: rule text embedded in an attacker query", []AssetInterceptRule{egressRule("fuzzy_url", "http://lab.example:8080/admin")}, nil, "http://attacker.net/?u=http://lab.example:8080/admin", nil, true, ""},
		{"url allow: satisfied by the url alone", nil, []AssetInterceptRule{egressRule("exact_url", "http://allowed.example/api")}, "http://allowed.example/api/v1", []string{"203.0.113.9"}, true, ""},
		// --- IPs: host literal and every resolved IP ---
		{"ip block: host literal without resolved ips", []AssetInterceptRule{egressRule("exact_ip", "203.0.113.9")}, nil, "http://203.0.113.9/", nil, false, EgressBlocked},
		{"ip block: resolved ip of a name", []AssetInterceptRule{egressRule("exact_ip", "203.0.113.9")}, nil, "http://name.example/", []string{"203.0.113.9"}, false, EgressBlocked},
		{"cidr block: any resolved ip", []AssetInterceptRule{egressRule("cidr", "10.0.0.0/8")}, nil, "http://name.example/", []string{"203.0.113.9", "10.1.2.3"}, false, EgressBlocked},
		{"cidr block: no resolved ip inside", []AssetInterceptRule{egressRule("cidr", "10.0.0.0/8")}, nil, "http://name.example/", []string{"203.0.113.9"}, true, ""},
		// --- path-only URL rules (finding 2): block rules match, allow rules never do ---
		{"path-only exact_url blocks a path prefix", []AssetInterceptRule{egressRule("exact_url", "/admin")}, nil, "http://x.example/admin/users", nil, false, EgressBlocked},
		{"path-only exact_url is case-insensitive", []AssetInterceptRule{egressRule("exact_url", "/ADMIN")}, nil, "http://x.example/admin", nil, false, EgressBlocked},
		{"path-only exact_url resolves dot segments", []AssetInterceptRule{egressRule("exact_url", "/admin")}, nil, "http://x.example/public/../admin", nil, false, EgressBlocked},
		{"path-only exact_url errs toward blocking on a string prefix", []AssetInterceptRule{egressRule("exact_url", "/admin")}, nil, "http://x.example/administrator", nil, false, EgressBlocked},
		{"path-only exact_url ignores other paths", []AssetInterceptRule{egressRule("exact_url", "/admin")}, nil, "http://x.example/public", nil, true, ""},
		{"path-only exact_url ignores the query", []AssetInterceptRule{egressRule("exact_url", "/admin")}, nil, "http://x.example/?next=/admin", nil, true, ""},
		{"path-only fuzzy_url matches a path substring", []AssetInterceptRule{egressRule("fuzzy_url", "secret")}, nil, "http://x.example/my-Secret-area", nil, false, EgressBlocked},
		{"path-only fuzzy_url matches a query substring", []AssetInterceptRule{egressRule("fuzzy_url", "token=")}, nil, "http://x.example/a?TOKEN=1", nil, false, EgressBlocked},
		{"path-only fuzzy_url ignores unrelated urls", []AssetInterceptRule{egressRule("fuzzy_url", "secret")}, nil, "http://x.example/other?a=b", nil, true, ""},
		// --- host-first schemeless block rules (review): label boundary, never allow ---
		{"host-first fuzzy_url blocks host and path", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com/admin")}, nil, "http://evil.com/admin/x", nil, false, EgressBlocked},
		{"host-first fuzzy_url blocks a subdomain", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com/admin")}, nil, "http://sub.evil.com/admin", nil, false, EgressBlocked},
		{"host-first fuzzy_url is case-insensitive", []AssetInterceptRule{egressRule("fuzzy_url", "EVIL.com/Admin")}, nil, "http://Evil.COM/ADMIN", nil, false, EgressBlocked},
		{"host-first fuzzy_url respects the label boundary", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com/admin")}, nil, "http://notevil.com/admin", nil, true, ""},
		{"host-first fuzzy_url needs the path too", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com/admin")}, nil, "http://evil.com/public", nil, true, ""},
		{"host-first exact_url blocks a path prefix", []AssetInterceptRule{egressRule("exact_url", "evil.com/admin")}, nil, "http://evil.com/admin/x", nil, false, EgressBlocked},
		{"host-first exact_url respects the label boundary", []AssetInterceptRule{egressRule("exact_url", "evil.com/admin")}, nil, "http://notevil.com/admin", nil, true, ""},
		{"bare host exact_url blocks that host", []AssetInterceptRule{egressRule("exact_url", "evil.com")}, nil, "http://evil.com/", nil, false, EgressBlocked},
		{"bare host fuzzy_url blocks a subdomain", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com")}, nil, "https://a.evil.com/x", nil, false, EgressBlocked},
		{"bare host with port needs that port", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com:8443")}, nil, "http://evil.com/", nil, true, ""},
		{"bare host with port blocks that port", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com:8443")}, nil, "https://evil.com:8443/", nil, false, EgressBlocked},
		{"bare host respects the label boundary", []AssetInterceptRule{egressRule("fuzzy_url", "evil.com")}, nil, "http://notevil.com/", nil, true, ""},
		{"bare host only in the query does not match the host", []AssetInterceptRule{egressRule("exact_url", "evil.com")}, nil, "http://x.example/?u=evil.com", nil, true, ""},
		{"dotted fragment is not a parent of an ip literal", []AssetInterceptRule{egressRule("fuzzy_url", "0.1.1")}, nil, "http://10.0.1.1/", nil, true, ""},
		{"ip literal typed as a url rule blocks that address", []AssetInterceptRule{egressRule("exact_url", "10.0.1.1")}, nil, "http://10.0.1.1/x", nil, false, EgressBlocked},
		{"bare host allow never matches", nil, []AssetInterceptRule{egressRule("fuzzy_url", "evil.com")}, "http://evil.com/", nil, false, EgressNotAllowed},
		{"bracketed ipv6 host-first blocks", []AssetInterceptRule{egressRule("fuzzy_url", "[::1]:8080/x")}, nil, "http://[::1]:8080/x/y", nil, false, EgressBlocked},
		{"bracketed ipv6 bare host blocks", []AssetInterceptRule{egressRule("exact_url", "[2001:db8::1]")}, nil, "http://[2001:db8::1]/", nil, false, EgressBlocked},
		{"bracketed ipv6 other port is not blocked", []AssetInterceptRule{egressRule("fuzzy_url", "[::1]:8080/x")}, nil, "http://[::1]:9090/x", nil, true, ""},
		{"host-first schemeless allow never matches", nil, []AssetInterceptRule{egressRule("exact_url", "evil.com/admin")}, "http://evil.com/admin", nil, false, EgressNotAllowed},
		{"path-only exact_url never satisfies an allow gate", nil, []AssetInterceptRule{egressRule("exact_url", "/api")}, "http://x.example/api", nil, false, EgressNotAllowed},
		{"path-only fuzzy_url never satisfies an allow gate", nil, []AssetInterceptRule{egressRule("fuzzy_url", "api")}, "http://x.example/api", nil, false, EgressNotAllowed},
		// --- allow gate semantics ---
		{"allow domain: resolved ip needs no ip rule", nil, []AssetInterceptRule{egressRule("exact_domain", "allowed.example")}, "http://allowed.example/", []string{"203.0.113.9"}, true, ""},
		{"allow cidr: satisfied by a resolved ip", nil, []AssetInterceptRule{egressRule("cidr", "203.0.113.0/24")}, "http://other.example/", []string{"203.0.113.9"}, true, ""},
		{"allow cidr: outside", nil, []AssetInterceptRule{egressRule("cidr", "203.0.113.0/24")}, "http://other.example/", []string{"198.51.100.1"}, false, EgressNotAllowed},
		{"allow: block beats allow", []AssetInterceptRule{egressRule("exact_domain", "a.example")}, []AssetInterceptRule{egressRule("exact_domain", "a.example")}, "http://a.example/", nil, false, EgressBlocked},
		{"allow: block on a resolved ip beats a domain allow", []AssetInterceptRule{egressRule("cidr", "127.0.0.0/8")}, []AssetInterceptRule{egressRule("exact_domain", "allowed.example")}, "http://allowed.example/", []string{"127.0.0.1"}, false, EgressBlocked},
		{"allow: no rules means no gate", nil, nil, "http://anything.example/", nil, true, ""},
		{"allow: only disabled rules means no gate", nil, []AssetInterceptRule{func() AssetInterceptRule { r := egressRule("exact_domain", "a.example"); r.Enabled = false; return r }()}, "http://anything.example/", nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.rawURL)
			if err != nil {
				t.Fatalf("fixture url %q: %v", tc.rawURL, err)
			}
			var ips []netip.Addr
			for _, s := range tc.ips {
				ips = append(ips, netip.MustParseAddr(s))
			}
			got := EvaluateEgress(tc.block, tc.allow, u, ips)
			if got.Allowed != tc.wantAllowed || got.Code != tc.wantCode {
				t.Fatalf("EvaluateEgress(%s) = allowed=%v code=%q reason=%q, want allowed=%v code=%q", tc.rawURL, got.Allowed, got.Code, got.Reason, tc.wantAllowed, tc.wantCode)
			}
			if !got.Allowed && got.Reason == "" {
				t.Fatalf("rejection of %s carries no reason", tc.rawURL)
			}
		})
	}
}

// Finding 3: an allow decision that came from an IP or CIDR rule only covers the
// addresses that match such a rule; the caller dials just those.
func TestEgressAllowedIPsFollowTheMatchingRule(t *testing.T) {
	addrs := func(ss ...string) []netip.Addr {
		var out []netip.Addr
		for _, s := range ss {
			out = append(out, netip.MustParseAddr(s))
		}
		return out
	}
	u, _ := url.Parse("http://name.example/")
	for _, tc := range []struct {
		name  string
		allow []AssetInterceptRule
		ips   []netip.Addr
		want  []netip.Addr // nil = every resolved address stays eligible
	}{
		{"cidr allow narrows to the matching address", []AssetInterceptRule{egressRule("cidr", "10.0.0.5/32")}, addrs("10.0.0.99", "10.0.0.5"), addrs("10.0.0.5")},
		{"exact_ip allow narrows too", []AssetInterceptRule{egressRule("exact_ip", "10.0.0.5")}, addrs("10.0.0.99", "10.0.0.5"), addrs("10.0.0.5")},
		{"domain allow keeps every address", []AssetInterceptRule{egressRule("exact_domain", "name.example")}, addrs("10.0.0.99", "10.0.0.5"), nil},
		{"url allow keeps every address", []AssetInterceptRule{egressRule("exact_url", "http://name.example/")}, addrs("10.0.0.99", "10.0.0.5"), nil},
		{"a host rule beside an ip rule keeps every address", []AssetInterceptRule{egressRule("cidr", "10.0.0.5/32"), egressRule("exact_domain", "name.example")}, addrs("10.0.0.99", "10.0.0.5"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateEgress(nil, tc.allow, u, tc.ips)
			if !got.Allowed {
				t.Fatalf("not allowed: %+v", got)
			}
			if len(got.AllowedIPs) != len(tc.want) {
				t.Fatalf("AllowedIPs = %v, want %v", got.AllowedIPs, tc.want)
			}
			for i := range tc.want {
				if got.AllowedIPs[i] != tc.want[i] {
					t.Fatalf("AllowedIPs = %v, want %v", got.AllowedIPs, tc.want)
				}
			}
		})
	}
}

// Review finding 1: the policy compares the ASCII host the transport puts on the wire. A
// compatibility spelling (ideographic full stop, full-width dot or letters, half-width
// ideographic full stop) maps to the same name, trailing dot included.
func TestEgressMatchCompatibilityHostSpellings(t *testing.T) {
	block := []AssetInterceptRule{egressRule("exact_domain", "blocked.example")}
	for _, host := range []string{
		"blocked.example\u3002",       // ideographic full stop as the trailing dot
		"blocked\u3002example",        // ... and as a label separator
		"blocked\uff0eexample",        // full-width full stop
		"blocked\uff61example\uff61",  // half-width ideographic full stop, trailing
		"\uff42locked.example",        // full-width b
		"BLOCKED.EXAMPLE\u3002\u3002", // several mapped trailing dots
		"blocked.example.",
	} {
		t.Run(host, func(t *testing.T) {
			u, err := url.Parse("http://" + host + "/x")
			if err != nil {
				t.Fatal(err)
			}
			if got := EvaluateEgress(block, nil, u, nil); got.Allowed || got.Code != EgressBlocked {
				t.Fatalf("host %q passed the block rule: %+v", host, got)
			}
			if got, ok := CanonicalEgressHost(u.Hostname()); !ok || got != "blocked.example" {
				t.Fatalf("CanonicalEgressHost(%q) = %q, %v; want blocked.example", host, got, ok)
			}
		})
	}
	t.Run("mapped ip literal is an ip", func(t *testing.T) {
		u, _ := url.Parse("http://\uff11\uff10.0.0.1/")
		if got := EvaluateEgress([]AssetInterceptRule{egressRule("exact_ip", "10.0.0.1")}, nil, u, nil); got.Allowed {
			t.Fatalf("full-width digits slipped past an ip rule: %+v", got)
		}
	})
	t.Run("a host that cannot be canonicalised is reported", func(t *testing.T) {
		for _, host := range []string{"a\uff1ab", "bad host\u3002example", "\u3002"} {
			if got, ok := CanonicalEgressHost(host); ok && got != "" {
				t.Errorf("CanonicalEgressHost(%q) = %q, true; want refusal", host, got)
			}
		}
	})
	t.Run("ascii names net/http dials as written are kept", func(t *testing.T) {
		if got, ok := CanonicalEgressHost("My_Host.Example."); !ok || got != "my_host.example" {
			t.Fatalf("got %q, %v", got, ok)
		}
	})
}

// Review finding 2: ports compare numerically; a leading zero is not a different port, and
// a port that is not a port matches nothing.
func TestEgressMatchPortsAreNumeric(t *testing.T) {
	for _, tc := range []struct {
		name        string
		block       []AssetInterceptRule
		rawURL      string
		wantAllowed bool
	}{
		{"exact_url :8443 vs :08443", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8443/private")}, "http://lab.example:08443/private", false},
		{"exact_url :08443 vs :8443", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:08443/private")}, "http://lab.example:8443/private", false},
		{"exact_url default vs :080", []AssetInterceptRule{egressRule("exact_url", "http://lab.example/private")}, "http://lab.example:080/private", false},
		{"exact_url :80 vs :0080", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:80/private")}, "http://lab.example:0080/private", false},
		{"exact_url https default vs :00443", []AssetInterceptRule{egressRule("exact_url", "https://lab.example/private")}, "https://lab.example:00443/private", false},
		{"exact_url different number stays allowed", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:8443/private")}, "http://lab.example:08444/private", true},
		{"exact_url rule with a non-numeric port matches nothing", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:80a/private")}, "http://lab.example/private", true},
		{"exact_url rule with an out-of-range port matches nothing", []AssetInterceptRule{egressRule("exact_url", "http://lab.example:99999/private")}, "http://lab.example:80/private", true},
		{"host:port rule :8443 vs :08443", []AssetInterceptRule{egressRule("fuzzy_url", "lab.example:8443")}, "http://lab.example:08443/", false},
		{"host:port rule :08443 vs :8443", []AssetInterceptRule{egressRule("fuzzy_url", "lab.example:08443")}, "http://lab.example:8443/", false},
		{"host:port rule :80 vs implied default", []AssetInterceptRule{egressRule("fuzzy_url", "lab.example:80")}, "http://lab.example/", false},
		{"host:port rule :080 vs implied default", []AssetInterceptRule{egressRule("fuzzy_url", "lab.example:080")}, "http://lab.example/", false},
		{"host:port rule with a non-numeric port matches nothing", []AssetInterceptRule{egressRule("fuzzy_url", "lab.example:http")}, "http://lab.example/", true},
		{"host:port rule with an out-of-range port matches nothing", []AssetInterceptRule{egressRule("fuzzy_url", "lab.example:65536/x")}, "http://lab.example/x", true},
		{"bracketed ipv6 :8080 vs :08080", []AssetInterceptRule{egressRule("fuzzy_url", "[::1]:8080")}, "http://[::1]:08080/", false},
		{"domain rule ignores the port", []AssetInterceptRule{egressRule("exact_domain", "lab.example")}, "http://lab.example:08443/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.rawURL)
			if err != nil {
				t.Fatal(err)
			}
			if got := EvaluateEgress(tc.block, nil, u, nil); got.Allowed != tc.wantAllowed {
				t.Fatalf("EvaluateEgress(%s) allowed=%v (%+v), want %v", tc.rawURL, got.Allowed, got, tc.wantAllowed)
			}
		})
	}
	t.Run("a destination with an unparsable port is refused", func(t *testing.T) {
		for _, raw := range []string{"http://lab.example:99999/", "http://lab.example:65536/"} {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := EvaluateEgress(nil, nil, u, nil); got.Allowed {
				t.Fatalf("%s was allowed: %+v", raw, got)
			}
		}
	})
}

// Security re-check item 1: a host whose last label is a number is an IPv4 address in the
// inet_aton / WHATWG sense (1 to 4 parts, decimal, octal or hex, the last part filling the
// remaining bytes). The resolver behind a proxy turns these into addresses, so the
// canonical host is the dotted form; a numeric-looking name that does not parse is refused.
func TestEgressMatchNumericIPv4Spellings(t *testing.T) {
	for host, want := range map[string]string{
		"2130706433":                      "127.0.0.1",
		"0x7f.1":                          "127.0.0.1",
		"127.1":                           "127.0.0.1",
		"0177.0.0.1":                      "127.0.0.1",
		"\uff10\uff58\uff17\uff46.\uff11": "127.0.0.1", // full-width 0x7f.1
		"127.0.0.01":                      "127.0.0.1",
		"0xa9fea9fe":                      "169.254.169.254",
		"0x7f.0.0.1.":                     "127.0.0.1",
		"10.1.513":                        "10.1.2.1",
		"0x0":                             "0.0.0.0",
		"1.2.3.0x":                        "1.2.3.0",
		"127.0.0.1":                       "127.0.0.1",
	} {
		t.Run(host, func(t *testing.T) {
			got, ok := CanonicalEgressHost(host)
			if !ok || got != want {
				t.Fatalf("CanonicalEgressHost(%q) = %q, %v; want %q", host, got, ok, want)
			}
		})
	}
	for _, host := range []string{"foo.123", "1.2.3.4.5", "256.1.1.1", "4294967296", "0x100000000", "example.0x", "09.1", "1..2"} {
		t.Run("invalid "+host, func(t *testing.T) {
			if got, ok := CanonicalEgressHost(host); ok {
				t.Fatalf("CanonicalEgressHost(%q) = %q, true; want refusal", host, got)
			}
		})
	}
	for _, host := range []string{"example.123abc", "1.example.com", "0x7f.example", "a1", "1a", "123abc"} {
		t.Run("name "+host, func(t *testing.T) {
			if got, ok := CanonicalEgressHost(host); !ok || got != host {
				t.Fatalf("CanonicalEgressHost(%q) = %q, %v; want the name unchanged", host, got, ok)
			}
		})
	}
	t.Run("rules see the address", func(t *testing.T) {
		for _, h := range []string{"2130706433", "0x7f.1", "127.1", "0177.0.0.1", "\uff10\uff58\uff17\uff46.\uff11"} {
			u, err := url.Parse("http://" + h + ":8080/x")
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []AssetInterceptRule{egressRule("exact_ip", "127.0.0.1"), egressRule("cidr", "127.0.0.0/8"), egressRule("fuzzy_ip", "127.0"), egressRule("exact_domain", "127.0.0.1")} {
				if got := EvaluateEgress([]AssetInterceptRule{r}, nil, u, nil); got.Allowed {
					t.Errorf("%s passed block rule %s %s", h, r.Kind, r.Pattern)
				}
			}
		}
	})
}

// A fuzzy_domain block rule that ends in a number is read as an address by the numeric
// IPv4 parsing ("0.1" is 0.0.0.1). It must still block what its text suffix blocked
// before; an allow rule of that shape compares the address only and never widens.
func TestEgressMatchFuzzyDomainNumericSuffixStillBlocks(t *testing.T) {
	u, err := url.Parse("http://10.0.0.1/")
	if err != nil {
		t.Fatal(err)
	}
	for _, pat := range []string{"0.1", ".0.1", "1", "foo.1"} {
		block := []AssetInterceptRule{egressRule("fuzzy_domain", pat)}
		want := pat != "foo.1" // "foo.1" was never a suffix of 10.0.0.1
		if got := !EvaluateEgress(block, nil, u, nil).Allowed; got != want {
			t.Errorf("fuzzy_domain block %q vs 10.0.0.1: blocked=%v, want %v", pat, got, want)
		}
		allow := []AssetInterceptRule{egressRule("fuzzy_domain", pat)}
		if EvaluateEgress(nil, allow, u, nil).Allowed {
			t.Errorf("fuzzy_domain allow %q admitted 10.0.0.1 by text suffix", pat)
		}
	}
}

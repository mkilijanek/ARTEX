package server

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Review findings 1 and 2 (PR 1) for the custom http tool: the policy judges the authority
// the transport really uses. A compatibility spelling of a host (U+3002 for the dot, ...)
// or a zero-padded port must neither pass a block rule nor reach the wire in another form.
// Loopback labs, an injected resolver and a stand-in proxy that never forwards.

const credCodeInvalid = "TARGET_INVALID"

const credTrailingDot = "\u3002" // ideographic full stop: maps to "." once the trailing dot is trimmed

func credPinDSN(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGPASSFILE", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGTARGETSESSIONATTRS"} {
		t.Setenv(k, "") // an ambient PGSSLROOTCERT or PGSERVICE can make the DSN parse fail
	}
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@10.77.0.9:5432/artex?sslmode=disable")
}

func credProxySawName(p *credProxy, name string) bool {
	for _, s := range p.Seen() {
		h := s.Host
		if u, err := url.Parse(s.URI); err == nil && u.Host != "" {
			h = u.Host
		}
		if strings.HasPrefix(strings.TrimRight(strings.ToLower(h), "."), name) {
			return true
		}
	}
	return false
}

func TestCustomHTTPCompatibilityHostSpellingIsBlocked(t *testing.T) {
	credPinDSN(t)
	block := []db.AssetInterceptRule{credRule("exact_domain", "blocked.example")}
	t.Run("through a proxy", func(t *testing.T) {
		px := newCredProxy(t)
		res := newCredResolver(map[string][]string{"blocked.example": {"203.0.113.9"}})
		setCredPolicy(t, credStaticRules(block, nil), "1", res)
		for _, raw := range []string{"http://blocked.example" + credTrailingDot + "/x", "http://\uff42locked.example/x"} {
			spec := httpSpec(raw, map[string]string{"X-Api-Key": "op-key"})
			spec["proxy"] = px.URL()
			requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{}), credCodeBlocked)
		}
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the proxy received %d request(s): %+v", n, px.Seen())
		}
	})
	t.Run("through a proxy that resolves remotely", func(t *testing.T) {
		// Nothing resolves locally: only the name checks apply, so the name must be judged
		// in the form the proxy is addressed with.
		px := newCredProxy(t)
		res := newCredResolver(map[string][]string{})
		setCredPolicy(t, credStaticRules(block, nil), "1", res)
		agent.SetTargetPolicy(credStaticRules(block, nil), agent.ControlPlane{ // a global proxy is configured
			ListenAddr:     ":1",
			Resolver:       res.Resolve,
			InterfaceAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.77.0.5")}, nil },
		}, true)
		spec := httpSpec("http://blocked.example"+credTrailingDot+"/x", nil)
		spec["proxy"] = px.URL()
		requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{}), credCodeBlocked)
		if credProxySawName(px, "blocked.example") {
			t.Fatalf("the proxy was asked for the blocked host: %+v", px.Seen())
		}
	})
	t.Run("redirect hop", func(t *testing.T) {
		a := newCredLab(t, "127.0.0.1:0", false)
		b := newCredLab(t, "127.0.0.2:0", false)
		res := newCredResolver(map[string][]string{"blocked.example": {"127.0.0.2"}})
		setCredPolicy(t, credStaticRules(block, nil), "1", res)
		requireRefusal(t, callHTTPTool(t, credRI, httpSpec(a.URL()+"/redir?to={to}", nil),
			map[string]any{"to": "http://blocked.example" + credTrailingDot + ":" + b.port + "/echo"}), credCodeBlocked)
		requireNoHits(t, "the blocked second hop", b)
	})
}

func TestCustomHTTPCompatibilityHostSpellingGoesOutCanonical(t *testing.T) {
	credPinDSN(t)
	t.Run("direct", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.2:0", false)
		res := newCredResolver(map[string][]string{"ok.example": {"127.0.0.2"}})
		setCredPolicy(t, credStaticRules(nil, nil), "1", res)
		requireRan(t, callHTTPTool(t, credRI, httpSpec("http://OK.example"+credTrailingDot+":0"+lab.port+"/echo", nil), map[string]any{}))
		if hits := lab.Hits(); len(hits) != 1 || hits[0].Host != "ok.example:"+lab.port {
			t.Fatalf("lab saw %+v, want one request with Host ok.example:%s", hits, lab.port)
		}
	})
	t.Run("through a proxy", func(t *testing.T) {
		px := newCredProxy(t)
		res := newCredResolver(map[string][]string{"ok.example": {"203.0.113.7"}})
		setCredPolicy(t, credStaticRules(nil, nil), "1", res)
		spec := httpSpec("http://ok.example"+credTrailingDot+"/ok", nil)
		spec["proxy"] = px.URL()
		requireRan(t, callHTTPTool(t, credRI, spec, map[string]any{}))
		seen := px.Seen()
		if len(seen) != 1 {
			t.Fatalf("proxy saw %+v, want exactly one request", seen)
		}
		if u, err := url.Parse(seen[0].URI); err != nil || u.Host != "ok.example" {
			t.Fatalf("proxy was addressed with %q (%v), want host ok.example", seen[0].URI, err)
		}
	})
	t.Run("redirect hop is sent canonical", func(t *testing.T) {
		a := newCredLab(t, "127.0.0.1:0", false)
		b := newCredLab(t, "127.0.0.2:0", false)
		res := newCredResolver(map[string][]string{"ok.example": {"127.0.0.2"}})
		setCredPolicy(t, credStaticRules(nil, nil), "1", res)
		requireRan(t, callHTTPTool(t, credRI, httpSpec(a.URL()+"/redir?to={to}", nil),
			map[string]any{"to": "http://ok.example" + credTrailingDot + ":0" + b.port + "/echo"}))
		if hits := b.Hits(); len(hits) != 1 || hits[0].Host != "ok.example:"+b.port {
			t.Fatalf("second hop saw %+v, want one request with Host ok.example:%s", hits, b.port)
		}
	})
	t.Run("a host that cannot be canonicalised is refused", func(t *testing.T) {
		px := newCredProxy(t)
		setCredPolicy(t, credStaticRules(nil, nil), "1", newCredResolver(map[string][]string{}))
		spec := httpSpec("http://a\uff1ab.example/x", nil)
		spec["proxy"] = px.URL()
		requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{}), credCodeInvalid)
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the proxy received %d request(s): %+v", n, px.Seen())
		}
	})
}

// Review finding 2: a zero-padded port is the same port to the dialler, so it is the same
// port to every rule.
func TestCustomHTTPZeroPaddedPortKeepsPolicy(t *testing.T) {
	credPinDSN(t)
	t.Run("exact_url block rule", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules([]db.AssetInterceptRule{credRule("exact_url", lab.URL()+"/private")}, nil), "1", nil)
		for _, raw := range []string{"http://127.0.0.2:0" + lab.port + "/private", "http://127.0.0.2:00" + lab.port + "/private/x", lab.URL() + "/private"} {
			requireRefusal(t, callHTTPTool(t, credRI, httpSpec(raw, nil), map[string]any{}), credCodeBlocked)
		}
		requireNoHits(t, "the blocked lab", lab)
	})
	t.Run("redirect hop", func(t *testing.T) {
		a := newCredLab(t, "127.0.0.1:0", false)
		b := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules([]db.AssetInterceptRule{credRule("exact_url", b.URL()+"/private")}, nil), "1", nil)
		requireRefusal(t, callHTTPTool(t, credRI, httpSpec(a.URL()+"/redir?to={to}", nil),
			map[string]any{"to": "http://127.0.0.2:0" + b.port + "/private"}), credCodeBlocked)
		requireNoHits(t, "the blocked second hop", b)
	})
	t.Run("an unparsable port is refused", func(t *testing.T) {
		lab := newCredLab(t, "127.0.0.2:0", false)
		setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
		for _, raw := range []string{"http://127.0.0.2:99999/x", "http://127.0.0.2:65536/x"} {
			got := callHTTPTool(t, credRI, httpSpec(raw, nil), map[string]any{})
			requireRefusal(t, got, credCodeInvalid)
			if strings.Contains(got.Text, "99999") || strings.Contains(got.Text, "65536") {
				t.Fatalf("refusal carries the port: %q", credTrunc(got.Text))
			}
		}
		requireNoHits(t, "the lab", lab)
	})
}

// Review (security re-check) item 1: numeric IPv4 spellings are addresses; with a proxy that
// resolves remotely only name checks would apply, so the guard turns them into the dotted
// address first.
func TestCustomHTTPNumericIPv4SpellingsAreCheckedAsAddresses(t *testing.T) {
	credPinDSN(t)
	ctl := newCredLab(t, "127.0.0.1:0", false)
	px := newCredProxy(t)
	res := newCredResolver(map[string][]string{})
	setCredPolicy(t, credStaticRules(nil, nil), ctl.port, res)
	agent.SetTargetPolicy(credStaticRules(nil, nil), agent.ControlPlane{ // a global proxy is configured
		ListenAddr:     ":" + ctl.port,
		Resolver:       res.Resolve,
		InterfaceAddrs: func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("10.77.0.5")}, nil },
	}, true)
	for _, h := range []string{"2130706433", "0x7f.1", "127.1", "0177.0.0.1", "\uff10\uff58\uff17\uff46.\uff11"} {
		spec := httpSpec("http://"+h+":"+ctl.port+"/x", nil)
		spec["proxy"] = px.URL()
		requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{}), credCodeControlPlane)
	}
	spec := httpSpec("http://foo.123/x", nil)
	spec["proxy"] = px.URL()
	requireRefusal(t, callHTTPTool(t, credRI, spec, map[string]any{}), credCodeInvalid)
	if n := len(px.Seen()); n != 0 {
		t.Fatalf("the proxy received %d request(s): %+v", n, px.Seen())
	}
	requireNoHits(t, "the artex stand-in", ctl)
}

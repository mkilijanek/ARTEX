package agent

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// Review findings 1 and 2 (PR 1): the policy is enforced against the authority the
// transport really uses. A compatibility spelling of a host (U+3002 for the dot, ...) or
// a zero-padded port must neither pass a block rule nor reach the wire in another form.
// Everything stays on loopback; "outside" names are only ever sent to a stand-in proxy
// that records and never forwards, and name resolution is injected.

const idnTrailingDot = "\u3002" // ideographic full stop: maps to "." after the trailing-dot trim

// pinDSN keeps the ambient database host (CI uses localhost) out of the resolver the test
// injects.
func pinDSN(t *testing.T) {
	t.Helper()
	clearPGEnv(t) // an ambient PGSSLROOTCERT or PGSERVICE can make the DSN parse fail
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@10.77.0.9:5432/artex?sslmode=disable")
}

func proxySawName(p *standInProxy, name string) bool {
	for _, s := range p.Seen() {
		h := s.Host
		if u, err := url.Parse(s.URL); err == nil && u.Host != "" {
			h = u.Host
		}
		if strings.HasPrefix(strings.TrimRight(strings.ToLower(h), "."), name) {
			return true
		}
	}
	return false
}

func requireErrored(t *testing.T, got toolOutcome) {
	t.Helper()
	if !got.IsError {
		t.Fatalf("want a refusal, got a result: %q", truncateForLog(got.Text))
	}
}

const codeInvalid = "TARGET_INVALID" // invalid host, port or scheme

// requireInvalid wants the TARGET_INVALID refusal with its decision log line.
func requireInvalid(t *testing.T, got toolOutcome, logs *lockedBuffer) {
	t.Helper()
	requireDecision(t, got, codeInvalid)
	if !strings.Contains(logs.String(), logPolicyDecision+codeInvalid) {
		t.Fatalf("no %s decision log line: %q", codeInvalid, logs.String())
	}
}

func TestWebFetchCompatibilityHostSpellingIsBlocked(t *testing.T) {
	pinDSN(t)
	block := []db.AssetInterceptRule{rule("exact_domain", "blocked.example")}
	t.Run("through a proxy, name resolves locally", func(t *testing.T) {
		px := newStandInProxy(t)
		res := newFakeResolver(map[string][]string{"blocked.example": {"203.0.113.9"}})
		setTestTargetPolicy(t, staticRules(block, nil), testControlPlane("1", res), false)
		requireDecision(t, fetchURLs(t, policyRI, px.URL(), "http://blocked.example"+idnTrailingDot+"/x")[0], codeBlocked)
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the proxy received %d request(s): %+v", n, px.Seen())
		}
	})
	t.Run("through a proxy that resolves remotely", func(t *testing.T) {
		px := newStandInProxy(t)
		setTestTargetPolicy(t, staticRules(block, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), true)
		for _, o := range fetchURLs(t, policyRI, px.URL(), "http://blocked.example"+idnTrailingDot+"/x", "http://\uff42locked.example/x", "https://blocked.example"+idnTrailingDot+"/x") {
			requireDecision(t, o, codeBlocked)
		}
		if proxySawName(px, "blocked.example") {
			t.Fatalf("the proxy was asked for the blocked host: %+v", px.Seen())
		}
	})
	t.Run("direct", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.2:0", false)
		res := newFakeResolver(map[string][]string{"blocked.example": {"127.0.0.2"}})
		setTestTargetPolicy(t, staticRules(block, nil), testControlPlane("1", res), false)
		requireDecision(t, fetchURLs(t, policyRI, "", "http://blocked.example"+idnTrailingDot+":"+lab.port+"/x")[0], codeBlocked)
		requireHits(t, "blocked lab", lab, 0)
	})
}

func TestWebFetchCompatibilityHostSpellingGoesOutCanonical(t *testing.T) {
	pinDSN(t)
	t.Run("direct: dial key, resolver name and Host header are the ASCII name", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.2:0", false)
		res := newFakeResolver(map[string][]string{"ok.example": {"127.0.0.2"}})
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", res), false)
		requireFetched(t, fetchURLs(t, policyRI, "", "http://OK.example"+idnTrailingDot+":0"+lab.port+"/ok")[0], "lab-ok")
		hits := lab.Hits()
		if len(hits) != 1 || hits[0].Host != "ok.example:"+lab.port {
			t.Fatalf("lab saw %+v, want one request with Host ok.example:%s", hits, lab.port)
		}
	})
	t.Run("through a proxy: the proxy is addressed with the ASCII name", func(t *testing.T) {
		px := newStandInProxy(t)
		res := newFakeResolver(map[string][]string{"ok.example": {"203.0.113.7"}})
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", res), false)
		requireFetched(t, fetchURLs(t, policyRI, px.URL(), "http://ok.example"+idnTrailingDot+"/ok")[0], "PROXIED")
		seen := px.Seen()
		if len(seen) != 1 {
			t.Fatalf("proxy saw %+v, want exactly one request", seen)
		}
		if u, err := url.Parse(seen[0].URL); err != nil || u.Host != "ok.example" {
			t.Fatalf("proxy was addressed with host %q (%v), want ok.example", seen[0].URL, err)
		}
	})
	t.Run("https through a proxy: CONNECT names the ASCII host", func(t *testing.T) {
		// net/http builds the CONNECT target with a different IDNA profile than the Host header
		// of a plain request; the rewrite makes both the same string.
		px := newStandInProxy(t)
		res := newFakeResolver(map[string][]string{"ok.example": {"203.0.113.7"}})
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", res), false)
		requireErrored(t, fetchURLs(t, policyRI, px.URL(), "https://ok.example"+idnTrailingDot+"/ok")[0]) // the stand-in does not tunnel
		seen := px.Seen()
		if len(seen) == 0 || seen[0].Method != "CONNECT" || seen[0].Host != "ok.example:443" {
			t.Fatalf("proxy saw %+v, want a CONNECT to ok.example:443", seen)
		}
	})
	t.Run("a host that cannot be canonicalised is refused", func(t *testing.T) {
		px := newStandInProxy(t)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), true)
		logs := captureLog(t)
		requireInvalid(t, fetchURLs(t, policyRI, px.URL(), "http://a\uff1ab.example/x")[0], logs)
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the proxy received %d request(s): %+v", n, px.Seen())
		}
	})
}

func TestWebFetchCompatibilityHostSpellingOnRedirectHop(t *testing.T) {
	pinDSN(t)
	block := []db.AssetInterceptRule{rule("exact_domain", "blocked.example")}
	t.Run("direct hop to a blocked name", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		b := newLabTarget(t, "127.0.0.2:0", false)
		res := newFakeResolver(map[string][]string{"blocked.example": {"127.0.0.2"}})
		setTestTargetPolicy(t, staticRules(block, nil), testControlPlane("1", res), false)
		requireDecision(t, fetchURLs(t, policyRI, "", redirectTo(a, "http://blocked.example"+idnTrailingDot+":"+b.port+"/secret"))[0], codeBlocked)
		requireHits(t, "first hop", a, 1)
		requireHits(t, "blocked second hop", b, 0)
	})
	t.Run("hop to a blocked name through a proxy", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		px := newStandInProxy(t, a)
		res := newFakeResolver(map[string][]string{"blocked.example": {"203.0.113.9"}})
		setTestTargetPolicy(t, staticRules(block, nil), testControlPlane("1", res), false)
		requireDecision(t, fetchURLs(t, policyRI, px.URL(), redirectTo(a, "http://blocked.example"+idnTrailingDot+"/secret"))[0], codeBlocked)
		if proxySawName(px, "blocked.example") {
			t.Fatalf("the proxy was asked for the blocked host: %+v", px.Seen())
		}
	})
	t.Run("an allowed hop is dialled and sent canonical", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		b := newLabTarget(t, "127.0.0.2:0", false)
		res := newFakeResolver(map[string][]string{"ok.example": {"127.0.0.2"}})
		setTestTargetPolicy(t, staticRules(block, nil), testControlPlane("1", res), false)
		requireFetched(t, fetchURLs(t, policyRI, "", redirectTo(a, "http://ok.example"+idnTrailingDot+":0"+b.port+"/ok"))[0], "lab-ok")
		if hits := b.Hits(); len(hits) != 1 || hits[0].Host != "ok.example:"+b.port {
			t.Fatalf("second hop saw %+v, want one request with Host ok.example:%s", hits, b.port)
		}
	})
	t.Run("a hop that cannot be canonicalised is refused", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), false)
		logs := captureLog(t)
		requireInvalid(t, fetchURLs(t, policyRI, "", redirectTo(a, "http://a\uff1ab.example/x"))[0], logs)
		if !strings.Contains(logs.String(), logPolicyDecision+codeInvalid+" hop=1") {
			t.Fatalf("the hop is not in the log line: %q", logs.String())
		}
		requireHits(t, "first hop", a, 1)
	})
}

// Review finding 2: a zero-padded port is the same port to the dialler, so it is the same
// port to every rule and to the control-plane denylist.
func TestWebFetchZeroPaddedPortKeepsPolicy(t *testing.T) {
	pinDSN(t)
	t.Run("exact_url block rule", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("exact_url", lab.URL()+"/private")}, nil), testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, "", "http://127.0.0.2:0"+lab.port+"/private", "http://127.0.0.2:00"+lab.port+"/private/x", lab.URL()+"/private")
		for _, o := range out {
			requireDecision(t, o, codeBlocked)
		}
		requireHits(t, "blocked lab", lab, 0)
	})
	t.Run("redirect hop", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		b := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("exact_url", b.URL()+"/private")}, nil), testControlPlane("1", nil), false)
		requireDecision(t, fetchURLs(t, policyRI, "", redirectTo(a, "http://127.0.0.2:0"+b.port+"/private"))[0], codeBlocked)
		requireHits(t, "blocked second hop", b, 0)
	})
	t.Run("control-plane port", func(t *testing.T) {
		ctl := newLabTarget(t, "127.0.0.1:0", false)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane(ctl.port, nil), false)
		requireDecision(t, fetchURLs(t, policyRI, "", "http://127.0.0.1:0"+ctl.port+"/x")[0], codeControlPlane)
		requireHits(t, "control plane stand-in", ctl, 0)
	})
	t.Run("zero-padded port is sent as the number", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
		requireFetched(t, fetchURLs(t, policyRI, "", "http://127.0.0.2:0"+lab.port+"/ok")[0], "lab-ok")
		if hits := lab.Hits(); len(hits) != 1 || hits[0].Host != "127.0.0.2:"+lab.port {
			t.Fatalf("lab saw %+v, want Host 127.0.0.2:%s", hits, lab.port)
		}
	})
	t.Run("an unparsable port is refused", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
		logs := captureLog(t)
		for _, o := range fetchURLs(t, policyRI, "", "http://127.0.0.2:99999/x", "http://127.0.0.2:65536/x") {
			requireInvalid(t, o, logs)
			if strings.Contains(o.Text, "99999") || strings.Contains(o.Text, "65536") {
				t.Fatalf("refusal text carries the port: %q", o.Text)
			}
		}
		requireHits(t, "lab", lab, 0)
	})
}

// Review (security re-check) item 3: a destination the guard cannot read is a policy
// refusal with a code and a content-free log line, for an invalid host, port or scheme.
func TestWebFetchPolicyInvalidDestinationIsRefusedAndLogged(t *testing.T) {
	pinDSN(t)
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
	for _, dest := range []string{"ftp://203.0.113.5/x", "ftp://203.0.113.5:8080/x", "http://203.0.113.5:99999/", "http://a\uff1ab.example/", "http://foo.123/"} {
		logs := captureLog(t)
		err := checkDest(t, dest)
		var te *TargetError
		if !asTargetError(err, &te) || te.Code != codeInvalid {
			t.Fatalf("%s: want %s, got %v", dest, codeInvalid, err)
		}
		if !strings.Contains(logs.String(), logPolicyDecision+codeInvalid+" hop=0 task=7") {
			t.Fatalf("%s: no decision log line: %q", dest, logs.String())
		}
		for _, bad := range []string{"ftp", "99999", "foo", "203.0.113.5"} {
			if strings.Contains(te.Error(), bad) || strings.Contains(logs.String(), bad) {
				t.Fatalf("%s: refusal or log carries %q: %v / %q", dest, bad, te, logs.String())
			}
		}
	}
}

// Review (security re-check) item 1: numeric IPv4 spellings are addresses, whatever the
// proxy's resolver would make of them. With a proxy that resolves remotely only name checks
// run, so the guard itself must turn them into the dotted address before judging them.
func TestWebFetchNumericIPv4SpellingsAreCheckedAsAddresses(t *testing.T) {
	pinDSN(t)
	ctl := newLabTarget(t, "127.0.0.1:0", false)
	px := newStandInProxy(t)
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane(ctl.port, newFakeResolver(map[string][]string{})), true)
	var urls []string
	for _, h := range []string{"2130706433", "0x7f.1", "127.1", "0177.0.0.1", "\uff10\uff58\uff17\uff46.\uff11"} {
		urls = append(urls, "http://"+h+":"+ctl.port+"/x")
	}
	urls = append(urls, "http://0xa9fea9fe/latest/meta-data/") // metadata, no port
	for i, o := range fetchURLs(t, policyRI, px.URL(), urls...) {
		t.Run(urls[i], func(t *testing.T) { requireDecision(t, o, codeControlPlane) })
	}
	if n := len(px.Seen()); n != 0 {
		t.Fatalf("the proxy received %d request(s): %+v", n, px.Seen())
	}
	requireHits(t, "control plane stand-in", ctl, 0)
	t.Run("a numeric-looking name that is not an address is refused", func(t *testing.T) {
		logs := captureLog(t)
		requireInvalid(t, fetchURLs(t, policyRI, px.URL(), "http://foo.123/x")[0], logs)
	})
}

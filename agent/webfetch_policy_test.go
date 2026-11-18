package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
)

// Phase D.1 / PR 1 network red tests for the WebFetch egress policy (D1, D6).
// Decision codes are the strings the plan names (§4, §7).
const (
	codeBlocked       = "TARGET_BLOCKED"
	codeNotAllowed    = "TARGET_NOT_ALLOWED"
	codeControlPlane  = "TARGET_CONTROL_PLANE" // denylist: control plane and metadata ranges
	codeUnavailable   = "TARGET_POLICY_UNAVAILABLE"
	codeUnresolved    = "TARGET_UNRESOLVED"
	logRetryDirect    = "[webfetch] direct_retry"
	logPolicyDecision = "[webfetch-policy] decision="
)

var policyRI = RunInfo{TaskID: 7, ExplorationID: 1}

func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

func redirectTo(lab *labTarget, to string) string {
	return lab.URL() + "/redir?to=" + url.QueryEscape(to)
}

func requireFetched(t *testing.T, got toolOutcome, want string) {
	t.Helper()
	if got.IsError || !strings.Contains(got.Text, want) {
		t.Fatalf("want a successful fetch containing %q, got is_error=%v text=%q", want, got.IsError, truncateForLog(got.Text))
	}
}

func requireHits(t *testing.T, what string, lab *labTarget, want int) {
	t.Helper()
	if got := len(lab.Hits()); got != want {
		t.Fatalf("%s received %d requests, want %d", what, got, want)
	}
}

// T-WF-1 (AC4, C8): a host matched by an enabled block rule is refused and the lab
// server receives nothing.
func TestWebFetchPolicyBlockRule(t *testing.T) {
	t.Run("global ip rule", func(t *testing.T) {
		open := newLabTarget(t, "127.0.0.1:0", false)
		blocked := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("exact_ip", "127.0.0.2")}, nil), testControlPlane("1", nil), false)
		logs := captureLog(t)
		out := fetchURLs(t, policyRI, "", blocked.URL()+"/page", open.URL()+"/ok")
		requireDecision(t, out[0], codeBlocked)
		requireHits(t, "blocked lab", blocked, 0)
		requireFetched(t, out[1], "lab-ok")
		// AC22: the decision log line is content-free.
		line := logs.String()
		if !strings.Contains(line, logPolicyDecision+codeBlocked) || !strings.Contains(line, "task=7") {
			t.Fatalf("no decision log line with decision and task id: %q", line)
		}
		if strings.Contains(line, "127.0.0.2") || strings.Contains(line, blocked.port) || strings.Contains(line, "/page") {
			t.Fatalf("decision log line carries URL, host or port: %q", line)
		}
	})
	t.Run("url rule", func(t *testing.T) {
		blocked := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("exact_url", blocked.URL()+"/secret")}, nil), testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, "", blocked.URL()+"/secret", blocked.URL()+"/open")
		requireDecision(t, out[0], codeBlocked)
		requireFetched(t, out[1], "lab-ok")
		if hits := blocked.Hits(); len(hits) != 1 || hits[0].URI != "/open" {
			t.Fatalf("blocked lab saw %+v, want only /open", hits)
		}
	})
	t.Run("cidr rule covers the resolved ip of a name", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.1:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("cidr", "127.0.0.0/8")}, nil), testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, "", "http://localhost:"+lab.port+"/page")
		requireDecision(t, out[0], codeBlocked)
		requireHits(t, "lab", lab, 0)
	})
	t.Run("disabled rule is ignored", func(t *testing.T) {
		blocked := newLabTarget(t, "127.0.0.2:0", false)
		off := rule("exact_ip", "127.0.0.2")
		off.Enabled = false
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{off}, nil), testControlPlane("1", nil), false)
		requireFetched(t, fetchURLs(t, policyRI, "", blocked.URL()+"/ok")[0], "lab-ok")
	})
}

// T-WF-2 (AC6, C9): every redirect hop is checked and the next hop is never dialled.
func TestWebFetchPolicyRedirectHopBlocked(t *testing.T) {
	t.Run("hop to a blocked host", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		b := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("cidr", "127.0.0.2/32")}, nil), testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, "", redirectTo(a, b.URL()+"/secret"))
		requireDecision(t, out[0], codeBlocked)
		requireHits(t, "first hop", a, 1)
		requireHits(t, "blocked second hop", b, 0)
	})
	t.Run("hop to cloud metadata", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		px := newStandInProxy(t, a) // keeps the test on loopback: it records, never forwards
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, px.URL(), redirectTo(a, "http://169.254.169.254/latest/meta-data/"))
		requireDecision(t, out[0], codeControlPlane)
		requireHits(t, "first hop", a, 1)
		if px.SawURL("http://169.254.169.254/") {
			t.Fatalf("the metadata hop was handed to the proxy: %+v", px.Seen())
		}
	})
	t.Run("hop outside the allow list", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		b := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules(nil, []db.AssetInterceptRule{rule("cidr", "127.0.0.1/32")}), testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, "", redirectTo(a, b.URL()+"/page"))
		requireDecision(t, out[0], codeNotAllowed)
		requireHits(t, "first hop", a, 1)
		requireHits(t, "not-allowed second hop", b, 0)
	})
	t.Run("in-policy redirects still work", func(t *testing.T) {
		a := newLabTarget(t, "127.0.0.1:0", false)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("cidr", "127.0.0.2/32")}, nil), testControlPlane("1", nil), false)
		requireFetched(t, fetchURLs(t, policyRI, "", a.URL()+"/chain/3")[0], "chain-end")
		requireHits(t, "chain lab", a, 4)
	})
}

// T-WF-3 (AC5, C8): with task allow rules, a host outside them is refused; a domain
// allow rule is satisfied by the host alone (its resolved IP is not re-matched); block
// rules still apply to the resolved IP.
func TestWebFetchPolicyAllowList(t *testing.T) {
	cp := func() ControlPlane { return testControlPlane("1", nil) }
	t.Run("host outside the allow rules", func(t *testing.T) {
		in := newLabTarget(t, "127.0.0.1:0", false)
		out := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules(nil, []db.AssetInterceptRule{rule("exact_domain", "localhost")}), cp(), false)
		res := fetchURLs(t, policyRI, "", "http://localhost:"+in.port+"/ok", out.URL()+"/page")
		requireFetched(t, res[0], "lab-ok") // domain rule satisfied by the host; 127.0.0.1 is in no ip rule
		requireDecision(t, res[1], codeNotAllowed)
		requireHits(t, "lab outside the allow list", out, 0)
	})
	t.Run("block on the resolved ip beats a domain allow", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.1:0", false)
		setTestTargetPolicy(t, staticRules(
			[]db.AssetInterceptRule{rule("cidr", "127.0.0.1/32")},
			[]db.AssetInterceptRule{rule("exact_domain", "localhost")}), cp(), false)
		requireDecision(t, fetchURLs(t, policyRI, "", "http://localhost:"+lab.port+"/ok")[0], codeBlocked)
		requireHits(t, "lab", lab, 0)
	})
	t.Run("allow rule smuggled through userinfo", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.2:0", false)
		setTestTargetPolicy(t, staticRules(nil, []db.AssetInterceptRule{rule("exact_domain", "localhost")}), cp(), false)
		requireDecision(t, fetchURLs(t, policyRI, "", "http://localhost:80@127.0.0.2:"+lab.port+"/ok")[0], codeNotAllowed)
		requireHits(t, "lab", lab, 0)
	})
}

// T-WF-4 (AC7, C8 control plane): resolver and interface enumeration are injected;
// control-plane and metadata destinations are refused in every spelling; a loopback lab
// on another port and the configured proxy stay reachable.
func TestWebFetchPolicyControlPlaneAndMetadata(t *testing.T) {
	t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@postgres:5432/artex?sslmode=disable")
	ctl := newLabTarget(t, "127.0.0.1:0", false) // stands in for the ARTEX listener
	lab := newLabTarget(t, "127.0.0.1:0", false)
	res := newFakeResolver(map[string][]string{"localhost": {"127.0.0.1"}, "postgres": {"10.9.8.7"}})
	setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane(ctl.port, res), false)

	t.Run("loopback to the artex port, direct", func(t *testing.T) {
		urls := []string{
			"http://127.0.0.1:" + ctl.port + "/",
			"http://localhost:" + ctl.port + "/",
			"http://[::]:" + ctl.port + "/",
			"http://0.0.0.0:" + ctl.port + "/",
			"http://[::1]:" + ctl.port + "/",
		}
		urls = append(urls, lab.URL()+"/ok")
		out := fetchURLs(t, policyRI, "", urls...)
		for i, u := range urls[:len(urls)-1] {
			t.Run(u, func(t *testing.T) { requireDecision(t, out[i], codeControlPlane) })
		}
		requireHits(t, "artex stand-in", ctl, 0)
		requireFetched(t, out[len(out)-1], "lab-ok") // loopback on another port is a legitimate lab
	})

	t.Run("interface, db and metadata destinations, via proxy", func(t *testing.T) {
		px := newStandInProxy(t) // records, never forwards
		refused := []string{
			"http://10.77.0.5:5432/",                   // fake interface address : db port
			"http://127.0.0.1:5432/",                   // local address : db port
			"http://10.9.8.7:5432/",                    // resolved DSN host : db port
			"http://postgres:5432/",                    // DSN host by name
			"http://10.77.0.5:" + ctl.port + "/",       // interface address : artex port
			"http://169.254.169.254/latest/meta-data/", // metadata
			"http://169.254.170.2/v2/credentials",      // ECS
			"http://[::ffff:169.254.169.254]/",         // v4-mapped
			"http://[64:ff9b::a9fe:a9fe]/",             // NAT64
			"http://[2002:a9fe:a9fe::1]/",              // 6to4
			"http://[fd00:ec2::254]/",                  // EC2 IPv6
			"http://[fe80::1]/",                        // link-local
			"http://100.100.100.200/",                  // Alibaba
			"http://168.63.129.16/",                    // Azure
		}
		allowed := []string{"http://203.0.113.5/ok", "http://postgres:5433/", "http://10.77.0.5:8080/"}
		out := fetchURLs(t, policyRI, px.URL(), append(append([]string{}, refused...), allowed...)...)
		for i, u := range refused {
			t.Run(u, func(t *testing.T) {
				requireDecision(t, out[i], codeControlPlane)
				if px.SawURL(u) {
					t.Fatalf("the proxy was asked to reach %s", u)
				}
			})
		}
		for i, u := range allowed {
			t.Run(u, func(t *testing.T) { requireFetched(t, out[len(refused)+i], "PROXIED") })
		}
	})
}

// T-WF-5 (AC9, C8 fail closed).
func TestWebFetchPolicyFailsClosed(t *testing.T) {
	t.Run("rule read error", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.1:0", false)
		boom := func(context.Context, int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
			return nil, nil, errors.New("rules unreadable")
		}
		setTestTargetPolicy(t, boom, testControlPlane("1", nil), false)
		logs := captureLog(t)
		requireDecision(t, fetchURLs(t, policyRI, "", lab.URL()+"/ok")[0], codeUnavailable)
		requireHits(t, "lab", lab, 0)
		if !strings.Contains(logs.String(), logPolicyDecision+codeUnavailable) {
			t.Fatalf("no %s log line: %q", codeUnavailable, logs.String())
		}
	})
	t.Run("exploration id without task id", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.1:0", false)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", nil), false)
		requireDecision(t, fetchURLs(t, RunInfo{ExplorationID: 5}, "", lab.URL()+"/ok")[0], codeUnavailable)
		requireHits(t, "lab", lab, 0)
	})
	t.Run("no loader registered fails closed", func(t *testing.T) {
		lab := newLabTarget(t, "127.0.0.1:0", false)
		px := newStandInProxy(t)
		setTestTargetPolicy(t, nil, testControlPlane("1", nil), false)
		out := fetchURLs(t, policyRI, px.URL(), "http://169.254.169.254/latest/", "http://203.0.113.5/ok")
		requireDecision(t, out[0], codeControlPlane) // the denylist still answers first
		requireDecision(t, out[1], codeUnavailable)
		requireDecision(t, fetchURLs(t, policyRI, "", lab.URL()+"/ok")[0], codeUnavailable)
		requireHits(t, "lab", lab, 0)
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the proxy saw %d requests without a rule loader: %+v", n, px.Seen())
		}
	})
	t.Run("chat session applies global rules and the denylist", func(t *testing.T) {
		open := newLabTarget(t, "127.0.0.1:0", false)
		blocked := newLabTarget(t, "127.0.0.2:0", false)
		var mu sync.Mutex
		var taskIDs []int64
		loader := func(_ context.Context, taskID int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
			mu.Lock()
			taskIDs = append(taskIDs, taskID)
			mu.Unlock()
			if taskID != 0 {
				return nil, []db.AssetInterceptRule{rule("exact_ip", "203.0.113.1")}, nil // task allow rules must not apply to chat
			}
			return []db.AssetInterceptRule{rule("exact_ip", "127.0.0.2")}, nil, nil
		}
		setTestTargetPolicy(t, loader, testControlPlane("1", nil), false)
		out := fetchURLs(t, RunInfo{SessionID: "chat-1"}, "", blocked.URL()+"/page", open.URL()+"/ok")
		requireDecision(t, out[0], codeBlocked)
		requireFetched(t, out[1], "lab-ok")
		mu.Lock()
		defer mu.Unlock()
		if len(taskIDs) == 0 || taskIDs[0] != 0 {
			t.Fatalf("chat session consulted the loader with task ids %v, want global rules (task id 0)", taskIDs)
		}
	})
}

// failingProxyFetch fetches an https lab target through a proxy that breaks the tunnel
// with a TLS-class error. The lab's accepted-connection counter shows whether a direct
// retry reached it.
func failingProxyFetch(t *testing.T, recordingProxyAddrKnown, globalProxySet bool) (lab *labTarget, out toolOutcome, logs *lockedBuffer) {
	t.Helper()
	lab = newLabTarget(t, "127.0.0.1:0", true)
	fp := newFailingProxy(t, "tls-garbage")
	cp := testControlPlane("1", nil)
	if recordingProxyAddrKnown {
		cp.RecordingProxyAddr = fp.Addr()
	}
	setTestTargetPolicy(t, staticRules(nil, nil), cp, globalProxySet)
	logs = captureLog(t)
	out = fetchURLs(t, policyRI, fp.URL(), lab.URL()+"/ok")[0]
	return lab, out, logs
}

// T-WF-6 (AC11, C11): with a global proxy configured, a tls:-class proxy failure makes
// no direct request, with capture off (the proxy is the global egress proxy) and with
// capture on (the proxy is the recording proxy, known to the control plane).
func TestWebFetchNoDirectRetryWithGlobalProxy(t *testing.T) {
	for _, capture := range []bool{false, true} {
		name := map[bool]string{false: "capture off", true: "capture on"}[capture]
		t.Run(name, func(t *testing.T) {
			lab, out, _ := failingProxyFetch(t, capture, true)
			if !out.IsError {
				t.Fatalf("a broken proxy tunnel must fail the fetch, got %q", truncateForLog(out.Text))
			}
			if strings.Contains(out.Text, codeControlPlane) {
				t.Fatalf("the configured proxy must stay dialable, got %q", truncateForLog(out.Text))
			}
			if n := lab.Conns(); n != 0 {
				t.Fatalf("direct retry reached the target: %d connections, want 0", n)
			}
		})
	}
}

// T-WF-7 (AC11, D6): capture on and no global proxy keeps the direct retry, and logs it
// without URL or host.
func TestWebFetchDirectRetryLogged(t *testing.T) {
	lab, out, logs := failingProxyFetch(t, true, false)
	if !out.IsError {
		t.Fatalf("the https lab has an untrusted certificate, the retried fetch must still fail, got %q", truncateForLog(out.Text))
	}
	if n := lab.Conns(); n < 1 {
		t.Fatalf("direct retry was not made: %d connections, want >= 1", n)
	}
	line := logs.String()
	if !strings.Contains(line, logRetryDirect+" task=7") {
		t.Fatalf("direct retry not logged as %q task=7: %q", logRetryDirect, line)
	}
	// The lab's own httptest server logs "TLS handshake error from 127.0.0.1:<port>" through
	// the same log package; only the policy's direct_retry line is under test.
	for _, l := range strings.Split(line, "\n") {
		if strings.Contains(l, logRetryDirect) && (strings.Contains(l, "127.0.0.1") || strings.Contains(l, lab.port)) {
			t.Fatalf("direct_retry log line carries host or port: %q", l)
		}
	}
}

// T-WF-8 (AC6, C9): the 10-hop cap holds once a policy CheckRedirect replaces net/http's
// default. Compared against a plain http.Client on the same chain, and the policy must
// actually be in the path (the rule loader is consulted).
func TestWebFetchRedirectHopCap(t *testing.T) {
	lab := newLabTarget(t, "127.0.0.1:0", false)
	chain := lab.URL() + "/chain/20"
	_, baseErr := http.Get(chain)
	if baseErr == nil || !strings.Contains(baseErr.Error(), "stopped after 10 redirects") {
		t.Fatalf("baseline net/http client did not stop after 10 redirects: %v", baseErr)
	}
	baseHits := len(lab.Hits())
	lab.Reset()

	var mu sync.Mutex
	loaderCalls := 0
	loader := func(context.Context, int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
		mu.Lock()
		loaderCalls++
		mu.Unlock()
		return nil, nil, nil
	}
	setTestTargetPolicy(t, loader, testControlPlane("1", nil), false)
	out := fetchURLs(t, policyRI, "", chain)[0]
	if !out.IsError || !strings.Contains(out.Text, "stopped after 10 redirects") {
		t.Fatalf("WebFetch did not fail like net/http after 10 redirects: is_error=%v %q", out.IsError, truncateForLog(out.Text))
	}
	if got := len(lab.Hits()); got != baseHits {
		t.Fatalf("WebFetch followed %d hops, net/http follows %d", got, baseHits)
	}
	mu.Lock()
	defer mu.Unlock()
	if loaderCalls == 0 {
		t.Fatalf("the egress policy never consulted the rule loader: the cap was not exercised with a policy CheckRedirect installed")
	}
}

// T-WF-9 (AC8, C8 dial): the direct path dials exactly the checked IP, so a rebinding
// answer on a second lookup cannot change it; with an environment proxy the wrapper and
// hop checks still refuse before any request.
func TestWebFetchDialPinnedAgainstRebinding(t *testing.T) {
	t.Run("direct dial uses the checked ip", func(t *testing.T) {
		// The checked answer is 127.0.0.2; a second lookup (or the system resolver) says
		// 127.0.0.1. Same port on both: whoever is dialled by name is the trap.
		// The database host is resolved through the same injected resolver. Pin the DSN to an
		// address literal so an ambient DSN on "localhost" (hosted CI) cannot use up an answer.
		t.Setenv("ARTEX_PG_DSN", "postgres://artex:pw@10.77.0.9:5432/artex?sslmode=disable")
		pinned := newLabTarget(t, "127.0.0.2:0", false)
		trap := newLabTarget(t, "127.0.0.1:"+pinned.port, false)
		res := newFakeResolver(map[string][]string{})
		res.seq["localhost"] = [][]string{{"127.0.0.2"}, {"127.0.0.1"}}
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", res), false)
		out := fetchURLs(t, policyRI, "", "http://localhost:"+pinned.port+"/ok")[0]
		if n := res.Calls("localhost"); n != 1 {
			t.Fatalf("the policy resolved the name %d times, want exactly once (injected resolver)", n)
		}
		requireFetched(t, out, "lab-ok")
		requireHits(t, "checked ip 127.0.0.2", pinned, 1)
		if n := trap.Conns(); n != 0 {
			t.Fatalf("the dial went to the re-resolved address: %d connections on the trap, want 0", n)
		}
	})
	t.Run("env proxy", func(t *testing.T) {
		px := newStandInProxy(t)
		// http.ProxyFromEnvironment reads its variables once per process, so this runs in a
		// fresh copy of the test binary. The child has no database: TestMain then skips its
		// advisory lock instead of waiting for ours.
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWebFetchEnvProxyChild$", "-test.count=1", "-test.v")
		cmd.Env = append(filterEnv(os.Environ(), "ARTEX_PG_DSN", "ARTEX_CONFIG", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"),
			"ARTEX_LAB_CHILD=envproxy", "ARTEX_CONFIG=/nonexistent/artex.json",
			"HTTP_PROXY="+px.URL(), "http_proxy="+px.URL(), "NO_PROXY=", "no_proxy=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child with HTTP_PROXY set failed: %v\n%s", err, tailString(string(out), 700))
		}
		if px.SawURL("http://198.51.100.7/x") {
			t.Fatalf("the blocked host reached the environment proxy: %+v", px.Seen())
		}
		if !px.SawURL("http://203.0.113.9/ok") {
			t.Fatalf("the allowed host never reached the environment proxy: %+v", px.Seen())
		}
	})
}

// TestWebFetchEnvProxyChild is the body of the env_proxy case above; it only runs when
// the parent re-executes the test binary.
func TestWebFetchEnvProxyChild(t *testing.T) {
	if os.Getenv("ARTEX_LAB_CHILD") != "envproxy" {
		t.Skip("helper process for TestWebFetchDialPinnedAgainstRebinding/env_proxy")
	}
	setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("cidr", "198.51.100.0/24")}, nil), testControlPlane("1", nil), false)
	out := fetchURLs(t, policyRI, "", "http://198.51.100.7/x", "http://203.0.113.9/ok")
	requireDecision(t, out[0], codeBlocked)
	requireFetched(t, out[1], "PROXIED")
}

func filterEnv(env []string, drop ...string) []string {
	var out []string
next:
	for _, kv := range env {
		for _, d := range drop {
			if strings.HasPrefix(kv, d+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

func tailString(s string, n int) string {
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// roleHarness drives one real role entry point (planner, worker, main agent, chat)
// with the given provider.
type roleHarness struct {
	role string
	run  func(ctx context.Context, p llm.Provider) error
}

func roleHarnesses(t *testing.T) []roleHarness {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { d.Close() })
	exp, err := d.CreateExploration("only local lab fixtures", "D1 lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM explorations WHERE id=$1`, exp) })
	ts := d.Exploration(exp)
	dir := t.TempDir()
	node := &db.Node{ID: 101, Payload: json.RawMessage(`{"summary":"local lab fixture"}`)}
	return []roleHarness{
		{"planner", func(ctx context.Context, p llm.Provider) error {
			_, _, err := NewPlanner(p, "fixture", dir, nil, 100000, 4).Plan(ctx, exp, nil, ts, "local lab fixture", nil, nil)
			return err
		}},
		{"worker", func(ctx context.Context, p llm.Provider) error {
			_, _, err := NewWorker(p, "fixture", dir, nil, 100000, 4).Execute(ctx, "fixture", exp, nil, ts, node, nil, nil, nil, nil)
			return err
		}},
		{"main", func(ctx context.Context, p llm.Provider) error {
			_, err := NewMainAgent(p, "fixture", dir, nil, 100000, 4).Chat(ctx, exp, 0, nil, ts, "local lab fixture", "message", nil, nil, nil, nil, nil)
			return err
		}},
		{"chat", func(ctx context.Context, p llm.Provider) error {
			_, err := NewChatAgent(p, "fixture", dir, nil, 100000).Chat(ctx, "assistant", "sess-1", "message", 4, 0, false, nil)
			return err
		}},
	}
}

// TestWebFetchToolSetPerRole (AC10): for every role the tool-name set offers WebFetch
// exactly once and has no duplicates, and the WebFetch the model reaches is wrapped by
// guardPanic: a panic inside the policy wrapper (here the rule loader) becomes a tool
// error instead of crashing the run.
func TestWebFetchToolSetPerRole(t *testing.T) {
	lab := newLabTarget(t, "127.0.0.1:0", false)
	for _, h := range roleHarnesses(t) {
		t.Run(h.role, func(t *testing.T) {
			panicking := func(context.Context, int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
				panic("loader exploded")
			}
			setTestTargetPolicy(t, panicking, testControlPlane("1", nil), false)
			p := &scriptedToolProvider{calls: []scriptCall{webFetchCall(lab.URL()+"/ok", false)}}
			if err := h.run(t.Context(), p); err != nil {
				t.Fatalf("%s run failed instead of reporting a tool error: %v", h.role, err)
			}
			seen := map[string]int{}
			for _, n := range p.ToolNames() {
				seen[n]++
			}
			if seen["WebFetch"] != 1 {
				t.Fatalf("%s offers WebFetch %d times, want exactly once (tools: %v)", h.role, seen["WebFetch"], p.ToolNames())
			}
			for n, c := range seen {
				if c != 1 {
					t.Errorf("%s offers tool %q %d times", h.role, n, c)
				}
			}
			for _, must := range []string{"Bash", "Read"} {
				if seen[must] != 1 {
					t.Errorf("%s lost default tool %s (tools: %v)", h.role, must, p.ToolNames())
				}
			}
			out := p.Outcomes()
			if len(out) != 1 {
				t.Fatalf("%s: got %d tool results, want 1", h.role, len(out))
			}
			if !out[0].IsError || !strings.Contains(out[0].Text, "internal error in tool WebFetch") {
				t.Fatalf("%s: a panic in the WebFetch wrapper must become a tool error, got is_error=%v text=%q", h.role, out[0].IsError, truncateForLog(out[0].Text))
			}
		})
	}
}

// TestRunInfoReachesToolContext (AC9): the RunInfo each role attaches is readable from
// the context a tool call receives, which is what the egress policy keys its rules on.
func TestRunInfoReachesToolContext(t *testing.T) {
	var mu sync.Mutex
	var got []RunInfo
	probe := actool.Build(actool.Spec{
		Name:   "RunInfoProbe",
		Schema: map[string]any{"type": "object"},
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			mu.Lock()
			got = append(got, RunInfoFrom(ctx))
			mu.Unlock()
			return actool.Text("probed"), nil
		},
	})
	prev := ToolAugment()
	SetToolAugment(func(context.Context, string) ([]actool.CoreTool, DeferredInfo, func()) {
		return []actool.CoreTool{probe}, DeferredInfo{}, nil
	})
	t.Cleanup(func() { SetToolAugment(prev) })

	for _, h := range roleHarnesses(t) {
		t.Run(h.role, func(t *testing.T) {
			mu.Lock()
			got = nil
			mu.Unlock()
			p := &scriptedToolProvider{calls: []scriptCall{{Name: "RunInfoProbe", Input: `{}`}}}
			if err := h.run(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(got) != 1 {
				t.Fatalf("%s: probe tool ran %d times, want 1 (tools: %v)", h.role, len(got), p.ToolNames())
			}
			ri := got[0]
			switch h.role {
			case "chat":
				if ri.SessionID != "sess-1" || ri.TaskID != 0 || ri.ExplorationID != 0 {
					t.Fatalf("chat RunInfo in tool context = %+v, want SessionID sess-1 and no task", ri)
				}
			case "worker":
				if ri.TaskID == 0 || ri.ExplorationID == 0 || ri.IntentID != 101 {
					t.Fatalf("worker RunInfo in tool context = %+v, want task, exploration and intent 101", ri)
				}
			default:
				if ri.TaskID == 0 || ri.ExplorationID == 0 {
					t.Fatalf("%s RunInfo in tool context = %+v, want task and exploration ids", h.role, ri)
				}
			}
		})
	}
}

// ---- review findings 1, 3, 6 ----

// Finding 1: the name net/http puts on the wire (IDNA, no trailing dot) is what gets
// resolved, on every path. The fake resolver only knows normalised names, so a lookup of
// the raw spelling cannot be answered.
func TestWebFetchResolvesNormalisedNameOnEveryPath(t *testing.T) {
	idn := "http://\u00fc.169.254.169.254.nip.io/"
	norm := db.NormalizeEgressHost("\u00fc.169.254.169.254.nip.io")
	if norm == "\u00fc.169.254.169.254.nip.io" {
		t.Fatalf("fixture name did not normalise: %q", norm)
	}
	t.Run("idn name through the recording proxy resolving to metadata", func(t *testing.T) {
		px := newStandInProxy(t)
		res := newFakeResolver(map[string][]string{norm: {"169.254.169.254"}})
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", res), false)
		requireDecision(t, fetchURLs(t, policyRI, px.URL(), idn)[0], codeControlPlane)
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the recording proxy saw %d requests: %+v", n, px.Seen())
		}
	})
	t.Run("unresolvable name through the recording proxy is refused", func(t *testing.T) {
		px := newStandInProxy(t)
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), false)
		requireDecision(t, fetchURLs(t, policyRI, px.URL(), "http://nope.example/x")[0], codeUnresolved)
		if n := len(px.Seen()); n != 0 {
			t.Fatalf("the recording proxy saw %d requests: %+v", n, px.Seen())
		}
	})
	t.Run("unresolvable name on the direct path is refused", func(t *testing.T) {
		setTestTargetPolicy(t, staticRules(nil, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), false)
		requireDecision(t, fetchURLs(t, policyRI, "", "http://nope.example/x")[0], codeUnresolved)
	})
	t.Run("global proxy: unresolvable name falls back to name checks (documented residual)", func(t *testing.T) {
		px := newStandInProxy(t)
		setTestTargetPolicy(t, staticRules([]db.AssetInterceptRule{rule("exact_domain", "blocked.example")}, nil), testControlPlane("1", newFakeResolver(map[string][]string{})), true)
		out := fetchURLs(t, policyRI, px.URL(), "http://nope.example/x", "http://blocked.example/x")
		requireFetched(t, out[0], "PROXIED")
		requireDecision(t, out[1], codeBlocked)
	})
}

// Finding 3: an allow decision made by an IP/CIDR rule pins only the addresses that match
// it. localhost resolves to a trap first and the allowed address second.
func TestWebFetchIPAllowRuleDialsOnlyMatchingAddress(t *testing.T) {
	allowed := newLabTarget(t, "127.0.0.2:0", false)
	trap := newLabTarget(t, "127.0.0.1:"+allowed.port, false)
	res := newFakeResolver(map[string][]string{"localhost": {"127.0.0.1", "127.0.0.2"}})
	setTestTargetPolicy(t, staticRules(nil, []db.AssetInterceptRule{rule("cidr", "127.0.0.2/32")}), testControlPlane("1", res), false)
	requireFetched(t, fetchURLs(t, policyRI, "", "http://localhost:"+allowed.port+"/ok")[0], "lab-ok")
	requireHits(t, "the allowed address", allowed, 1)
	if n := trap.Conns(); n != 0 {
		t.Fatalf("the dial used an address no allow rule matches: %d connections on the trap", n)
	}
}

// Finding 6: a rule-store failure reaches the model as a fixed reason, not the driver text.
func TestWebFetchPolicyUnavailableHidesStoreError(t *testing.T) {
	lab := newLabTarget(t, "127.0.0.1:0", false)
	boom := func(context.Context, int64) ([]db.AssetInterceptRule, []db.AssetInterceptRule, error) {
		return nil, nil, errors.New(`pq: password authentication failed for user "artex" host=db.internal`)
	}
	setTestTargetPolicy(t, boom, testControlPlane("1", nil), false)
	out := fetchURLs(t, policyRI, "", lab.URL()+"/ok")[0]
	requireDecision(t, out, codeUnavailable)
	if strings.Contains(out.Text, "password") || strings.Contains(out.Text, "db.internal") {
		t.Fatalf("the store error reached the model: %q", truncateForLog(out.Text))
	}
	if !strings.Contains(out.Text, "policy rules unavailable") {
		t.Fatalf("no fixed reason in %q", truncateForLog(out.Text))
	}
}

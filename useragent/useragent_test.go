package useragent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func reset(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		SetVersion("dev")
		SetHidden(true)
		SetCustom("")
		SetComponents(Configurable())
	})
	SetVersion("1.2.3")
	SetHidden(false)
	SetCustom("")
	SetComponents(Configurable())
}

func TestForShownFormat(t *testing.T) {
	reset(t)
	got := For("LLM")
	want := "Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/1.2.3 (+LLM)"
	if got != want {
		t.Fatalf("For(LLM) = %q, want %q", got, want)
	}
	if got := For("API Recon"); got != "Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/1.2.3 (+API Recon)" {
		t.Fatalf("For(API Recon) = %q", got)
	}
}

func TestForHiddenIsEmptyAndIgnoresCustom(t *testing.T) {
	reset(t)
	SetCustom("Custom/1")
	SetHidden(true)
	if got := For("LLM"); got != "" {
		t.Fatalf("hidden For = %q, want empty", got)
	}
}

func TestForCustomForCheckedComponents(t *testing.T) {
	reset(t)
	SetCustom("Custom/1")
	if got := For("MCP"); got != "Custom/1" {
		t.Fatalf("For(MCP) = %q, want custom", got)
	}
}

func TestForUncheckedComponentIsHidden(t *testing.T) {
	reset(t)
	SetCustom("Custom/1")
	SetComponents([]string{"LLM"})
	if got := For("Notify"); got != "" {
		t.Fatalf("unchecked For = %q, want empty", got)
	}
	if got := For("LLM"); got != "Custom/1" {
		t.Fatalf("checked For = %q", got)
	}
}

func TestSetComponentsDropsUnknownAndLocked(t *testing.T) {
	reset(t)
	SetComponents([]string{"WebFetch", "WebSearch", "bogus", "Tools"})
	got := Components()
	if len(got) != 1 || got[0] != "Tools" {
		t.Fatalf("Components() = %v, want [Tools]", got)
	}
	if For("WebFetch") != "" {
		t.Fatal("locked component must never resolve")
	}
}

func TestConcurrentToggle(t *testing.T) {
	reset(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				SetHidden(j%2 == 0)
				SetCustom("x")
				SetComponents([]string{"LLM"})
				_ = For("LLM")
				_ = Components()
				SetVersion("v")
			}
		}(i)
	}
	wg.Wait()
}

func TestEffective(t *testing.T) {
	reset(t)
	e := Effective()
	if e["LLM"] != For("LLM") || e["LLM"] == "" {
		t.Fatalf("LLM = %q", e["LLM"])
	}
	SetComponents([]string{"LLM"})
	e = Effective()
	if e["Notify"] != "Go default (Go-http-client/1.1 or /2.0)" {
		t.Fatalf("Notify = %q", e["Notify"])
	}
	if !strings.Contains(e["API Recon"], "spa-api-recon") {
		t.Fatalf("API Recon = %q", e["API Recon"])
	}
	SetHidden(true)
	if Effective()["LLM"] != "Go default (Go-http-client/1.1 or /2.0)" {
		t.Fatal("hidden must show the Go default")
	}
}

// Fixture shared with web/src/lib/user-agent.test.mjs: the same inputs give the same string.
func TestShownFormatFixture(t *testing.T) {
	reset(t)
	SetVersion("2.2.0")
	const want = "Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/2.2.0 (+API Recon)"
	if got := For("API Recon"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

var (
	// any Set of a user-agent header, whatever the value expression is
	normaUASet = regexp.MustCompile(`(?i)\.Set\(\s*"user-agent"\s*,`)
	// the one form the constants mirror: a string literal value
	normaUALit = regexp.MustCompile(`(?i)\.Header\.Set\(\s*"user-agent"\s*,\s*"([^"]*)"\s*\)`)
)

// checkNormaUA requires exactly n user-agent Set lines in src, each a Header.Set with a string
// literal equal to want.
func checkNormaUA(src, want string, n int) error {
	if got := len(normaUASet.FindAllString(src, -1)); got != n {
		return fmt.Errorf("found %d User-Agent Set lines, want exactly %d", got, n)
	}
	lits := normaUALit.FindAllStringSubmatch(src, -1)
	if len(lits) != n {
		return fmt.Errorf("%d of the %d User-Agent Set lines are no longer Header.Set with a string literal", n-len(lits), n)
	}
	for _, m := range lits {
		if m[1] != want {
			return fmt.Errorf("User-Agent is %q, the constant is %q", m[1], want)
		}
	}
	return nil
}

func TestCheckNormaUA(t *testing.T) {
	ok := "req.Header.Set(\"user-agent\", \"norma/0.4\")\n"
	if err := checkNormaUA(ok, "norma/0.4", 1); err != nil {
		t.Fatal(err)
	}
	if err := checkNormaUA(strings.Replace(ok, "0.4", "0.5", 1), "norma/0.4", 1); err == nil {
		t.Fatal("a changed value must fail")
	}
	if err := checkNormaUA(ok+"req.Header.Set(\"User-Agent\", \"other\")\n", "norma/0.4", 1); err == nil {
		t.Fatal("a second User-Agent Set must fail")
	}
	if err := checkNormaUA("req.Header.Set(\"User-Agent\", ua)\n", "norma/0.4", 1); err == nil {
		t.Fatal("a non-literal value must fail")
	}
	if err := checkNormaUA("// no header here\n", "norma/0.4", 1); err == nil {
		t.Fatal("a missing Set must fail")
	}
	two := ok + "r2.Header.Set(\"user-agent\", \"norma/0.4\")\n"
	if err := checkNormaUA(two, "norma/0.4", 2); err != nil {
		t.Fatal(err)
	}
	if err := checkNormaUA(two, "norma/0.4", 1); err == nil {
		t.Fatal("an extra Set line must fail")
	}
}

// The values shown for the Norma components are copies; fail when the library changes them
// (third_party/norma/tool/webfetch.go:236, websearch.go:226).
func TestNormaConstantsMatchSources(t *testing.T) {
	// WebFetch sets it twice: on the request and on the direct retry after a proxy failure.
	for _, c := range []struct {
		file, want string
		n          int
	}{
		{"../third_party/norma/tool/webfetch.go", WebFetchUA, 2},
		{"../third_party/norma/tool/websearch.go", WebSearchUA, 1},
	} {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkNormaUA(string(b), c.want, c.n); err != nil {
			t.Fatalf("%s: %v; update useragent.WebFetchUA/WebSearchUA", c.file, err)
		}
	}
}

// Regression: readers must only ever see one of two complete states while Apply alternates between
// them. Publishing the parts with separate setters shows a mix of both (hidden=false with the old
// component set, and so on).
func TestApplyIsAtomic(t *testing.T) {
	reset(t)
	a := State{Hidden: true, Custom: "", Components: Configurable()}
	b := State{Hidden: false, Custom: "B/1", Components: []string{"LLM"}}
	same := func(x, y State) bool {
		return x.Hidden == y.Hidden && x.Custom == y.Custom && slices.Equal(x.Components, y.Components)
	}
	Apply(a.Hidden, a.Custom, a.Components) // readers start from a complete state
	var stop atomic.Bool
	var torn atomic.Int64
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if s := Snapshot(); !same(s, a) && !same(s, b) {
					torn.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 100000; i++ {
		s := a
		if i%2 == 1 {
			s = b
		}
		Apply(s.Hidden, s.Custom, s.Components)
	}
	stop.Store(true)
	wg.Wait()
	if n := torn.Load(); n != 0 {
		t.Fatalf("readers observed %d torn states", n)
	}
	// A final Apply is fully visible, with non-configurable names dropped.
	Apply(false, "C/1", []string{"WebFetch", "Notify", "bogus"})
	if s := Snapshot(); s.Hidden || s.Custom != "C/1" || !slices.Equal(s.Components, []string{"Notify"}) {
		t.Fatalf("Snapshot = %+v", s)
	}
}

// The hint texts are mirrored in web/src/lib/user-agent.ts (UA_GO_DEFAULT, UA_API_RECON_DEFAULT)
// and asserted there against the same literals.
func TestHintFixtures(t *testing.T) {
	reset(t)
	SetHidden(true)
	e := Effective()
	if e["LLM"] != "Go default (Go-http-client/1.1 or /2.0)" {
		t.Fatalf("Go default hint = %q", e["LLM"])
	}
	if e["API Recon"] != "script default (Mozilla/5.0 (spa-api-recon) / Mozilla/5.0 (spa-api-recon spider))" {
		t.Fatalf("API Recon hint = %q", e["API Recon"])
	}
}

func TestHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"plain/1": "plain/1",
		"café/1":  "caf\xe9/1",
		"Üÿ":      "\xdc\xff",
		"a¡b":     "a\xa1b",
	} {
		if got := HeaderValue(in); got != want {
			t.Errorf("HeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
	// A value outside the set (a profile saved before the 8-bit rule) is sent as it always was.
	if got := HeaderValue("Skaner-ż/1"); got != "Skaner-ż/1" {
		t.Errorf("out-of-set value changed: %q", got)
	}
}

// net/http and HTTP/2 both carry the obs-text byte 0xE9 in a header value.
func TestLatin1ByteOverHTTP1AndHTTP2(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		got := make(chan string, 1)
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got <- r.Header.Get("User-Agent")
		}))
		srv.EnableHTTP2 = h2
		srv.StartTLS()
		req, err := http.NewRequest("GET", srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", HeaderValue("café/1"))
		resp, err := srv.Client().Do(req)
		if err != nil {
			srv.Close()
			t.Fatalf("h2=%v: %v", h2, err)
		}
		resp.Body.Close()
		wantMajor := 1
		if h2 {
			wantMajor = 2
		}
		if resp.ProtoMajor != wantMajor {
			t.Fatalf("h2=%v: negotiated HTTP/%d", h2, resp.ProtoMajor)
		}
		if ua := <-got; ua != "caf\xe9/1" {
			t.Fatalf("h2=%v: server saw %q", h2, ua)
		}
		srv.Close()
	}
}

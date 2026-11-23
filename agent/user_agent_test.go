package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"

	"github.com/mkilijanek/artex/useragent"
)

// serveUA spins up an httptest server that records the User-Agent of the request
// it receives, drives one request through a quotaAwareHTTPClient built with the
// given userAgent, and returns what actually reached the wire.
func serveUA(t *testing.T, userAgent string) string {
	t.Helper()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := quotaAwareHTTPClient("", "", userAgent)
	if err != nil {
		t.Fatalf("quotaAwareHTTPClient: %v", err)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	return <-got
}

func TestUserAgentHeaderSetWhenConfigured(t *testing.T) {
	const want = "artex-test-agent/1.2.3"
	if got := serveUA(t, want); got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

func TestUserAgentHeaderDefaultWhenEmpty(t *testing.T) {
	// Empty = no override: the Go client's default UA must reach the server unchanged.
	got := serveUA(t, "")
	if !strings.HasPrefix(got, "Go-http-client/") {
		t.Fatalf("User-Agent = %q, want the default Go-http-client UA when unset", got)
	}
}

func TestValidateUserAgentRejectsNewline(t *testing.T) {
	if _, err := ValidateUserAgent("good/1.0\r\nX-Injected: 1"); err == nil {
		t.Fatalf("expected a newline-containing user agent to be rejected")
	}
}

// The rule is "no control character at all" (r < 0x20 or r == 0x7f), so the
// non-newline controls are rejected too, TAB included: a header value is one
// printable token, not something to lay out with tabs.
func TestValidateUserAgentRejectsControlCharacters(t *testing.T) {
	for name, in := range map[string]string{
		"NUL": "good/1.0\x00",
		"US":  "good/1.0\x1f",
		"DEL": "good/1.0\x7f",
		"TAB": "good/1.0\tX",
	} {
		if _, err := ValidateUserAgent(in); err == nil {
			t.Errorf("%s: expected a user agent containing %q to be rejected", name, in)
		}
	}
}

func TestValidateUserAgentAcceptsPrintableToken(t *testing.T) {
	const want = "MyTool/1.0 (contact-free)"
	if ua, err := ValidateUserAgent("  " + want + "  "); err != nil || ua != want {
		t.Fatalf("printable value: got (%q, %v), want (%q, nil)", ua, err, want)
	}
}

func TestValidateUserAgentRejectsTooLong(t *testing.T) {
	if _, err := ValidateUserAgent(strings.Repeat("a", MaxUserAgentLen+1)); err == nil {
		t.Fatalf("expected a user agent longer than %d to be rejected", MaxUserAgentLen)
	}
}

func TestValidateUserAgentTrimsAndAllowsEmpty(t *testing.T) {
	if ua, err := ValidateUserAgent("   "); err != nil || ua != "" {
		t.Fatalf("blank user agent: got (%q, %v), want (\"\", nil)", ua, err)
	}
	if ua, err := ValidateUserAgent("  my-app/1.0  "); err != nil || ua != "my-app/1.0" {
		t.Fatalf("trim: got (%q, %v), want (\"my-app/1.0\", nil)", ua, err)
	}
}

// shownUA turns the ARTEX User-Agent on for one test and restores the default after.
func shownUA(t *testing.T) {
	t.Helper()
	useragent.SetVersion("9.9.9")
	useragent.SetHidden(false)
	t.Cleanup(func() {
		useragent.SetHidden(true)
		useragent.SetVersion("dev")
	})
}

func TestUserAgentProfileWinsOverARTEX(t *testing.T) {
	shownUA(t)
	if got := serveUA(t, "profile/1"); got != "profile/1" {
		t.Fatalf("User-Agent = %q, want the profile value", got)
	}
}

func TestUserAgentShownUsesARTEXLLM(t *testing.T) {
	shownUA(t)
	got := serveUA(t, "")
	if !strings.Contains(got, "ARTEX/9.9.9 (+LLM)") || strings.Contains(got, "Firefox/") {
		t.Fatalf("User-Agent = %q, want the ARTEX LLM string", got)
	}
}

// The value is resolved per request, so a toggle reaches an already built client.
func TestUserAgentResolvedAtRequestTime(t *testing.T) {
	got := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Get("User-Agent") }))
	defer srv.Close()
	client, err := quotaAwareHTTPClient("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	shownUA(t)
	useragent.SetHidden(true)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	useragent.SetHidden(false)
	resp, err = client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if first := <-got; !strings.HasPrefix(first, "Go-http-client/") {
		t.Fatalf("hidden request User-Agent = %q", first)
	}
	if second := <-got; !strings.Contains(second, "ARTEX/9.9.9 (+LLM)") {
		t.Fatalf("shown request User-Agent = %q", second)
	}
}

// The rule: printable characters of the 8-bit set (Latin-1, U+0000-U+00FF), at most 256 characters.
func TestValidateUserAgentAcceptsPrintableLatin1(t *testing.T) {
	for name, in := range map[string]string{
		"accent":     "caf\u00e9/1",
		"umlauts":    "\u00dcn\u00efc\u00f6d\u00e9-Agent",
		"256 ascii":  strings.Repeat("x", 256),
		"256 accent": strings.Repeat("\u00e9", 256), // 512 UTF-8 bytes, 256 characters
		"U+00A1":     "a\u00a1b",
		"U+00FF":     "a\u00ffb",
	} {
		got, err := ValidateUserAgent(in)
		if err != nil || got != in {
			t.Errorf("%s: got (%q, %v), want it accepted unchanged", name, got, err)
		}
	}
}

func TestValidateUserAgentRejectsOutsidePrintableLatin1(t *testing.T) {
	for name, in := range map[string]string{
		"z with dot above": "Skaner-\u017c/1",
		"emoji":            "ua-\U0001F600",
		"C1 control":       "a\u0085b",
		"NBSP":             "a\u00a0b",
		"soft hyphen":      "a\u00adb",
		"257 ascii":        strings.Repeat("x", 257),
		"257 accent":       strings.Repeat("\u00e9", 257),
		"CR LF":            "a\r\nX-Injected: 1",
		"raw invalid byte": "good/1.0\x80",
		"DEL":              "a\x7fb",
	} {
		if _, err := ValidateUserAgent(in); err == nil {
			t.Errorf("%s: expected %q to be rejected", name, in)
		}
	}
}

// ValidateUserAgent accepts exactly the printable characters of U+0000-U+00FF.
func TestValidateUserAgentSetMatchesIsPrint(t *testing.T) {
	// The rule leans on unicode.IsPrint: NBSP and the soft hyphen are not printable for it.
	if unicode.IsPrint(0xA0) || unicode.IsPrint(0xAD) || !unicode.IsPrint(0xA1) || !unicode.IsPrint(0xFF) {
		t.Fatal("unicode.IsPrint no longer matches the documented set")
	}
	for r := rune(0); r <= 0xFF; r++ {
		_, err := ValidateUserAgent("a" + string(r) + "b")
		want := r >= 0x20 && r <= 0x7e || r >= 0xa1 && r != 0xad
		if (err == nil) != want {
			t.Errorf("U+%04X: accepted=%v, want %v", r, err == nil, want)
		}
	}
}

// What reaches the wire for a profile value with a Latin-1 character is the single byte.
func TestUserAgentLatin1OnTheWire(t *testing.T) {
	if got := serveUA(t, "caf\u00e9/1"); got != "caf\xe9/1" {
		t.Fatalf("User-Agent bytes = %q, want caf\\xe9/1", got)
	}
	shownUA(t)
	useragent.SetCustom("Ünï/1")
	t.Cleanup(func() { useragent.SetCustom("") })
	if got := serveUA(t, ""); got != "\xdcn\xef/1" {
		t.Fatalf("custom User-Agent bytes = %q", got)
	}
}

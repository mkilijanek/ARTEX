package useragent

import (
	"os"
	"strings"
	"sync"
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
	if e["Notify"] != "Go default (Go-http-client/1.1)" {
		t.Fatalf("Notify = %q", e["Notify"])
	}
	if !strings.Contains(e["API Recon"], "spa-api-recon") {
		t.Fatalf("API Recon = %q", e["API Recon"])
	}
	SetHidden(true)
	if Effective()["LLM"] != "Go default (Go-http-client/1.1)" {
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

// The values shown for the Norma components are copies; fail when the library changes them.
func TestNormaConstantsMatchSources(t *testing.T) {
	for file, want := range map[string]string{
		"../third_party/norma/tool/webfetch.go":  WebFetchUA,
		"../third_party/norma/tool/websearch.go": WebSearchUA,
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"`+want+`"`) {
			t.Fatalf("%s no longer contains the User-Agent %q; update useragent.WebFetchUA/WebSearchUA", file, want)
		}
	}
}

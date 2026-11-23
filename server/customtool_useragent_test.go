package server

import (
	"strings"
	"testing"

	"github.com/mkilijanek/artex/useragent"
)

// A custom HTTP tool sends the ARTEX Tools User-Agent when shown, nothing of ours when hidden,
// and a User-Agent among its configured headers (any case) always wins.
func TestCustomHTTPUserAgent(t *testing.T) {
	t.Cleanup(func() { useragent.SetHidden(true); useragent.SetVersion("dev") })
	useragent.SetVersion("9.9.9")
	lab := newCredLab(t, "127.0.0.1:0", false)
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	ua := func(headers map[string]string) string {
		lab.Reset()
		requireRan(t, callHTTPTool(t, credRI, httpSpec(lab.URL()+"/echo", headers), nil))
		hits := lab.Hits()
		if len(hits) != 1 {
			t.Fatalf("lab got %d requests", len(hits))
		}
		return hits[0].Header.Get("User-Agent")
	}

	useragent.SetHidden(true)
	if got := ua(nil); !strings.HasPrefix(got, "Go-http-client/") {
		t.Fatalf("hidden: User-Agent = %q", got)
	}
	useragent.SetHidden(false)
	if got := ua(nil); !strings.Contains(got, "ARTEX/9.9.9 (+Tools)") {
		t.Fatalf("shown: User-Agent = %q", got)
	}
	if got := ua(map[string]string{"user-agent": "tool-cfg/1"}); got != "tool-cfg/1" {
		t.Fatalf("a configured header must win, got %q", got)
	}
}

func TestCustomHTTPUserAgentLatin1Byte(t *testing.T) {
	t.Cleanup(func() { useragent.Apply(true, "", useragent.Configurable()) })
	useragent.Apply(false, "caf\u00e9/1", useragent.Configurable())
	lab := newCredLab(t, "127.0.0.1:0", false)
	setCredPolicy(t, credStaticRules(nil, nil), "1", nil)
	requireRan(t, callHTTPTool(t, credRI, httpSpec(lab.URL()+"/echo", nil), nil))
	hits := lab.Hits()
	if len(hits) != 1 || hits[0].Header.Get("User-Agent") != "caf\xe9/1" {
		t.Fatalf("hits = %+v", hits)
	}
}

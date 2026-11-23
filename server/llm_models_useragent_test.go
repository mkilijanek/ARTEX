package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/useragent"
)

// listModelsUA drives one model-listing call and returns the User-Agent the endpoint saw.
func listModelsUA(t *testing.T, s *Server, extra map[string]any) string {
	t.Helper()
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("User-Agent")
		w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()
	body := map[string]any{"provider": "openai", "base_url": srv.URL, "api_key": "k"}
	for k, v := range extra {
		body[k] = v
	}
	resp := postJSON(t, s.pgListModels, "/api/llm/models", body)
	if resp["ok"] != true {
		t.Fatalf("listing failed: %+v", resp)
	}
	return <-got
}

func TestModelListingUserAgent(t *testing.T) {
	t.Cleanup(func() { useragent.Apply(true, "", useragent.Configurable()); useragent.SetVersion("dev") })
	useragent.SetVersion("9.9.9")

	useragent.Apply(true, "", useragent.Configurable())
	if got := listModelsUA(t, &Server{}, nil); !strings.HasPrefix(got, "Go-http-client/") {
		t.Fatalf("hidden: User-Agent = %q", got)
	}
	useragent.Apply(false, "", useragent.Configurable())
	if got := listModelsUA(t, &Server{}, nil); !strings.Contains(got, "ARTEX/9.9.9 (+LLM)") {
		t.Fatalf("shown: User-Agent = %q", got)
	}
}

// A profile's own User-Agent wins, as on the provider transport.
func TestModelListingProfileUserAgentWins(t *testing.T) {
	m, err := NewManager(t.TempDir(), "", testDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if m.pg == nil {
		t.Skip("requires isolated PostgreSQL")
	}
	t.Cleanup(func() { useragent.Apply(true, "", useragent.Configurable()) })
	useragent.Apply(false, "", useragent.Configurable())
	id, err := m.pg.SaveProfile(&db.LLMProfile{Name: "ua-list", Format: "openai", Model: "m", BaseURL: "http://x", UserAgent: "profile/7"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.DeleteProfile(id) })
	s := &Server{m: m}
	if got := listModelsUA(t, s, map[string]any{"profile_id": id}); got != "profile/7" {
		t.Fatalf("User-Agent = %q, want the profile value", got)
	}
}

func TestModelListingUserAgentLatin1Byte(t *testing.T) {
	t.Cleanup(func() { useragent.Apply(true, "", useragent.Configurable()) })
	useragent.Apply(false, "caf\u00e9/1", useragent.Configurable())
	if got := listModelsUA(t, &Server{}, nil); got != "caf\xe9/1" {
		t.Fatalf("User-Agent bytes = %q", got)
	}
}

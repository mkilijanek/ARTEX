package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
)

func TestFixedEndpointConnectionTestKeyBinding(t *testing.T) {
	srv, hits, key := keyRecorder(t)
	s := &Server{llmCfg: agent.ConfigFromEndpoint("anthropic", "m", srv.URL+"/relay/v2/messages?route=a", "fixture-key", "", agent.EndpointFixed)}
	for _, tc := range []struct {
		mode, path string
		want       bool
	}{{"fixed", "/relay/v2/messages?route=a", true}, {"fixed", "/relay/v2/messages?route=b", false}, {"base", "/relay/v2/messages?route=a", false}} {
		before := hits.Load()
		resp := postJSON(t, s.testLLM, "/api/llm/test", map[string]any{"provider": "anthropic", "model": "m", "base_url": srv.URL + tc.path, "endpoint_mode": tc.mode, "streaming": false})
		if (hits.Load() > before) != tc.want || resp["ok"] != tc.want {
			t.Fatalf("mode=%s path=%s response=%v hits=%d", tc.mode, tc.path, resp, hits.Load()-before)
		}
		if tc.want {
			if got, _ := key.Load().(string); got != "fixture-key" {
				t.Fatal("saved key not used for exact endpoint")
			}
		}
	}
}

func TestProfileEndpointChangeRequiresExplicitKey(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("requires isolated PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	id, err := pg.SaveProfile(&db.LLMProfile{Name: t.Name(), Format: "anthropic", Model: "fixture", BaseURL: "https://example.invalid/one", EndpointMode: "fixed", APIKey: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id)
	s := &Server{m: &Manager{pg: pg}}
	body, _ := json.Marshal(map[string]any{"id": id, "name": t.Name(), "format": "anthropic", "model": "fixture", "base_url": "https://example.invalid/two", "endpoint_mode": "fixed"})
	r := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.pgSaveProfile(w, r)
	if w.Code != 400 {
		t.Fatalf("changed endpoint retained saved key: %d %s", w.Code, w.Body.String())
	}
	got, err := pg.ProfileByID(id)
	if err != nil || got.BaseURL != "https://example.invalid/one" {
		t.Fatal("rejected save changed profile", err)
	}
}

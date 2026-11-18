package server

import (
	"bytes"
	"encoding/json"
	"github.com/mkilijanek/artex/db"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelListingRefusesRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{"data":[{"id":"fixture"}]}`)) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	s := &Server{}
	result := postJSON(t, s.pgListModels, "/api/llm/models", map[string]any{"provider": "anthropic", "base_url": origin.URL, "api_key": "fixture"})
	if calls != 0 {
		t.Fatalf("model listing followed credential redirect: %d %+v", calls, result)
	}
}
func TestSaveProfileReadFailures(t *testing.T) {
	for _, closed := range []bool{false, true} {
		m, err := NewManager(t.TempDir(), "")
		if err != nil {
			t.Fatal(err)
		}
		s := &Server{m: m}
		if m.pg == nil {
			m.Close()
			t.Skip("requires isolated PostgreSQL")
		}
		if closed {
			m.pg.Close()
		}
		req := httptest.NewRequest("POST", "/api/llm/profiles", bytes.NewBufferString(`{"id":9223372036854775807,"name":"missing","format":"anthropic","model":"fixture"}`))
		rec := httptest.NewRecorder()
		s.pgSaveProfile(rec, req)
		want := 404
		if closed {
			want = 500
		}
		if rec.Code != want {
			t.Fatalf("closed=%v status=%d body=%s", closed, rec.Code, rec.Body.String())
		}
		m.Close()
	}
}
func TestDimensionsDoNotInventUnselectedMeasurements(t *testing.T) {
	// The DTO must not grow unselected provider/model/latency/size fields.
	for _, field := range []string{`"model"`, `"provider"`, `"latency_ms"`, `"request_bytes"`} {
		if strings.Contains(string(mustDimensionJSON(t)), field) {
			t.Fatal("unselected field reported", field)
		}
	}
}

func mustDimensionJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(db.UsageDimension{})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

package tool

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchClientRefusesCredentialRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer origin.Close()
	p, err := newSearchProvider(WebSearchConfig{Backend: "brave-free", BraveAPIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	client := p.(*braveProvider).client
	req, _ := http.NewRequest("GET", origin.URL, nil)
	req.Header.Set("X-Subscription-Token", "fixture")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 0 || resp.StatusCode != 307 {
		t.Fatalf("redirect leaked: calls=%d status=%d", calls, resp.StatusCode)
	}
}

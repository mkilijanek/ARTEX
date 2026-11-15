package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderRedirectRefusesCredentialForwarding(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
			defer origin.Close()
			client, err := quotaAwareHTTPClient("", "X-Session", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest("GET", origin.URL, nil)
			req.Header.Set("x-api-key", "fixture-key")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if calls != 0 || resp.StatusCode != status {
				t.Fatalf("redirect followed: calls=%d status=%d", calls, resp.StatusCode)
			}
		})
	}
}

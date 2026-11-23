package enrich

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/useragent"
)

func probeUA(t *testing.T) string {
	t.Helper()
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Get("User-Agent") }))
	defer srv.Close()
	req, err := probeRequest(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := buildClient(nil).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return <-got
}

func TestProbeUserAgent(t *testing.T) {
	t.Cleanup(func() { useragent.SetHidden(true); useragent.SetVersion("dev") })
	useragent.SetVersion("9.9.9")

	useragent.SetHidden(true)
	if got := probeUA(t); !strings.HasPrefix(got, "Go-http-client/") {
		t.Fatalf("hidden: User-Agent = %q, want the Go default (the old artex-enrich/1.0 is gone)", got)
	}

	useragent.SetHidden(false)
	if got := probeUA(t); !strings.Contains(got, "ARTEX/9.9.9 (+Enrichment)") {
		t.Fatalf("shown: User-Agent = %q", got)
	}
}

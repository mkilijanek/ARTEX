package agent

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const EndpointFixed = "fixed"

// Legacy mode keeps SDK suffix normalization. Fixed mode preserves the exact
// endpoint, including escaped path, trailing slash and query string.
func ConfigFromEndpoint(provider, model, baseURL, apiKey, proxy, mode string) Config {
	c := ConfigFrom(provider, model, baseURL, apiKey, proxy)
	c.EndpointMode = strings.TrimSpace(mode)
	if c.EndpointMode == EndpointFixed {
		c.BaseURL = strings.TrimSpace(baseURL)
	}
	return c
}

func ValidateEndpoint(mode, raw string) error {
	if mode == "" || mode == "base" {
		return nil
	}
	if mode != EndpointFixed {
		return fmt.Errorf("unknown LLM endpoint mode %q", mode)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("fixed LLM endpoint must be an absolute http(s) URL without user info or fragment")
	}
	return nil
}

// SameEndpoint includes mode and the exact fixed path/query in key binding.
func SameEndpoint(a, b Config) bool {
	mode := func(s string) string {
		if s == "base" {
			return ""
		}
		return s
	}
	return strings.TrimSpace(a.Proxy) == strings.TrimSpace(b.Proxy) && a.Format == b.Format && mode(a.EndpointMode) == mode(b.EndpointMode) && a.BaseURL == b.BaseURL
}

type fixedEndpointTransport struct {
	base     http.RoundTripper
	endpoint *url.URL
}

func (t fixedEndpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.endpoint.Scheme || req.URL.Host != t.endpoint.Host {
		return nil, fmt.Errorf("LLM fixed endpoint refused request to a different origin")
	}
	r := req.Clone(req.Context())
	u := *t.endpoint
	r.URL = &u
	r.Host = u.Host
	return t.base.RoundTrip(r)
}

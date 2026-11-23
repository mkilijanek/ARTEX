package mcphttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mkilijanek/artex/useragent"
)

// A Streamable HTTP server whose slow_call tool takes 3s, so the client's
// per-request timeout can be observed: 1s cuts the call off, 10s lets it finish.
func TestStreamableClientRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"slow_call","description":"slow","inputSchema":{"type":"object"}}]}}`, req.ID)
		case "tools/call":
			time.Sleep(3 * time.Second)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":"done"}]}}`, req.ID)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// timeout_seconds=1: the slow call is cut off well before the server answers.
	c, err := New(ctx, "t", srv.URL, nil, false, 1)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.Tools(ctx); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	start := time.Now()
	if _, err := c.Call(ctx, "slow_call", nil); err == nil {
		t.Fatal("expected the 1s-timeout call to fail")
	} else if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("call not cut off by the timeout: %v", elapsed)
	}
	_ = c.Close()

	// timeout_seconds=10: the same slow call completes.
	c2, err := New(ctx, "t", srv.URL, nil, false, 10)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	got, err := c2.Call(ctx, "slow_call", nil)
	if err != nil || got != "done" {
		t.Fatalf("Call() = %q, %v", got, err)
	}
	_ = c2.Close()

	// timeout_seconds=0: falls back to the 120s default (call completes).
	c3, err := New(ctx, "t", srv.URL, nil, false, 0)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got, err := c3.Call(ctx, "slow_call", nil); err != nil || got != "done" {
		t.Fatalf("default-timeout Call() = %q, %v", got, err)
	}
	_ = c3.Close()
}

// mcpUA connects to a Streamable HTTP server and returns the User-Agent of every request seen.
func mcpUA(t *testing.T, headers map[string]string) []string {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("User-Agent"))
		mu.Unlock()
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"t","version":"0"}}}`, req.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := New(ctx, "t", srv.URL, headers, false, 5)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), seen...)
}

func TestMCPUserAgent(t *testing.T) {
	t.Cleanup(func() { useragent.SetHidden(true); useragent.SetVersion("dev") })
	useragent.SetVersion("9.9.9")

	useragent.SetHidden(true)
	for _, ua := range mcpUA(t, nil) {
		if !strings.HasPrefix(ua, "Go-http-client/") {
			t.Fatalf("hidden: User-Agent = %q", ua)
		}
	}
	useragent.SetHidden(false)
	seen := mcpUA(t, nil)
	if len(seen) == 0 {
		t.Fatal("no request reached the server")
	}
	for _, ua := range seen {
		if !strings.Contains(ua, "ARTEX/9.9.9 (+MCP)") {
			t.Fatalf("shown: User-Agent = %q", ua)
		}
	}
	for _, ua := range mcpUA(t, map[string]string{"user-agent": "mcp-cfg/1"}) {
		if ua != "mcp-cfg/1" {
			t.Fatalf("a configured header must win, got %q", ua)
		}
	}
}

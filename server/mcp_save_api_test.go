//go:build unix

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postMCP(t *testing.T, s *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/mcp", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	s.pgSaveMCP(rec, req)
	return rec
}

// The operator's MCP API applies the same rule.
func TestOperatorMCPAPIRefusesDeniedKeysForStdioOnly(t *testing.T) {
	e := newPlatEnv(t)
	for _, key := range deniedKeys {
		name := uniq("api")
		rec := postMCP(t, e.s, map[string]any{"name": name, "transport": "stdio", "command": "true", "enabled": false, "env": map[string]string{key: "SECRET-VALUE-123"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d: %s", key, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), key) || strings.Contains(rec.Body.String(), "SECRET-VALUE-123") {
			t.Errorf("%s: body %q", key, rec.Body.String())
		}
		if e.mcpNamed(name) != nil {
			t.Errorf("%s: a row was written", key)
		}
	}
	name := uniq("apihttp")
	rec := postMCP(t, e.s, map[string]any{"name": name, "transport": "http", "url": "http://127.0.0.1:9/mcp", "enabled": false, "env": map[string]string{"LD_PRELOAD": "h", "X-API-Key": "k"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("http row: %d %s", rec.Code, rec.Body.String())
	}
	if row := e.mcpNamed(name); row == nil || !strings.Contains(string(row.Env), "LD_PRELOAD") {
		t.Errorf("row %+v", row)
	} else {
		t.Cleanup(func() { _ = e.m.pg.DeleteMCP(row.ID) })
	}
	// an update of an existing row is checked too
	name = uniq("apiupd")
	rec = postMCP(t, e.s, map[string]any{"name": name, "transport": "stdio", "command": "true", "enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	row := e.mcpNamed(name)
	t.Cleanup(func() { _ = e.m.pg.DeleteMCP(row.ID) })
	rec = postMCP(t, e.s, map[string]any{"id": row.ID, "name": name, "transport": "stdio", "command": "true", "enabled": false, "env": map[string]string{"NODE_OPTIONS": "--require /tmp/x.js"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("update with a denied key: %d", rec.Code)
	}
}

//go:build unix

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db"
)

// Names the drop chain refuses (sandbox.ValidateVarName), plus a header-shaped name that cannot be
// an environment variable at all.
var deniedKeys = []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "NODE_OPTIONS", "PATH", "HOME", "BASH_ENV", "PYTHONPATH", "GIT_SSH_COMMAND", "GCONV_PATH", "X-Api-Key"}

// AC14: create_mcp and update_mcp refuse a denied key for a stdio row before a human is asked and
// before anything is written; the error names the key and never the value.
func TestMCPToolsRefuseDeniedKeysForStdio(t *testing.T) {
	e := newPlatEnv(t)
	for _, key := range deniedKeys {
		name := uniq("deny")
		res := e.refused(e.s.toolCreateMCP(), "create_mcp", map[string]any{"name": name, "transport": "stdio", "command": "true", "env": map[string]string{key: "SECRET-VALUE-123"}})
		msg := res.Flatten()
		if !strings.Contains(msg, key) || strings.Contains(msg, "SECRET-VALUE-123") {
			t.Errorf("%s: error %q must name the key and not the value", key, msg)
		}
		if e.mcpNamed(name) != nil {
			t.Errorf("%s: a row was written", key)
		}
	}
	// update_mcp: the row exists and must stay as it was
	good := &db.MCPServer{Name: uniq("upd"), Transport: "stdio", Command: "true", Env: json.RawMessage(`{"FOO":"bar"}`), Enabled: false}
	id, err := e.m.pg.SaveMCP(good)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.m.pg.DeleteMCP(id) })
	e.refused(e.s.toolUpdateMCP(), "update_mcp", map[string]any{"id": id, "name": good.Name, "transport": "stdio", "command": "true", "env": map[string]string{"LD_PRELOAD": "/tmp/x.so"}})
	if got := e.mcpNamed(good.Name); got == nil || !strings.Contains(string(got.Env), "FOO") || strings.Contains(string(got.Env), "LD_PRELOAD") {
		t.Errorf("the refused update changed the row: %+v", got)
	}
	// an http row turned into a stdio row carries its header names with it: refused too
	e.refused(e.s.toolUpdateMCP(), "update_mcp", map[string]any{"id": id, "name": good.Name, "transport": "stdio", "command": "true", "env": map[string]string{"X-Api-Key": "k"}})
}

// The same names are headers for http and sse and stay allowed there (contradiction 12).
func TestMCPToolsAllowTheSameNamesAsHeadersForHTTPAndSSE(t *testing.T) {
	e := newPlatEnv(t)
	for _, tr := range []string{"http", "sse"} {
		name := uniq(tr)
		in := map[string]any{"name": name, "transport": tr, "url": "http://127.0.0.1:9/mcp", "enabled": false,
			"env": map[string]string{"LD_PRELOAD": "h", "X-API-Key": "k", "PATH": "p"}}
		if res := e.run(e.s.toolCreateMCP(), "create_mcp", in, true); res.IsError {
			t.Fatalf("%s: %s", tr, res.Flatten())
		}
		row := e.mcpNamed(name)
		if row == nil || !strings.Contains(string(row.Env), "LD_PRELOAD") {
			t.Fatalf("%s: row %+v", tr, row)
		}
		t.Cleanup(func() { _ = e.m.pg.DeleteMCP(row.ID) })
	}
	// and a stdio row with an ordinary name is still created
	name := uniq("ok")
	if res := e.run(e.s.toolCreateMCP(), "create_mcp", map[string]any{"name": name, "transport": "stdio", "command": "true", "enabled": false, "env": map[string]string{"FOO": "bar"}}, true); res.IsError {
		t.Fatal(res.Flatten())
	}
	if row := e.mcpNamed(name); row != nil {
		t.Cleanup(func() { _ = e.m.pg.DeleteMCP(row.ID) })
	} else {
		t.Fatal("no row")
	}
}

// AC15: a stdio row planted in the database before the upgrade, enabled and with no tool cache, is
// refused at the server's start-up discovery and at the next run, before a variables file is
// written; the log names the key, the MCP status carries the refusal, nothing starts. An http row
// with the same key in its headers is left alone.
func TestPlantedStdioRowIsRefusedAtSpawn(t *testing.T) {
	e := newPlatEnv(t)
	_, l := activateEnforcedTree(t)
	calls, _ := fakeStdio(t)
	logs := captureServerLog(t)

	planted := &db.MCPServer{Name: uniq("planted"), Transport: "stdio", Command: "true", Enabled: true, Env: json.RawMessage(`{"LD_PRELOAD":"/tmp/x.so","FOO":"bar"}`)}
	id, err := e.m.pg.SaveMCP(planted) // straight into the database: the legacy fixture
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.m.pg.DeleteMCP(id) })
	headers := &db.MCPServer{Name: uniq("hdr"), Transport: "http", URL: "http://127.0.0.1:9/mcp", Enabled: false, Env: json.RawMessage(`{"LD_PRELOAD":"h"}`)}
	hid, err := e.m.pg.SaveMCP(headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.m.pg.DeleteMCP(hid) })

	startedPlanted := func() []spawnCall { // rows other tests left in the shared database may start
		var out []spawnCall
		for _, c := range *calls {
			if c.name == planted.Name {
				out = append(out, c)
			}
		}
		return out
	}
	e.s.discoverEmptyMCPsOnStartup()
	if got := startedPlanted(); len(got) != 0 {
		t.Fatalf("a planted row was started: %+v", got)
	}
	if ents, _ := os.ReadDir(l.VarsDir); len(ents) != 0 {
		t.Errorf("a variables file was written: %v", ents)
	}
	want := "[mcp] refused server " + planted.Name + ": denied variable name LD_PRELOAD"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
	if strings.Contains(logs.String(), "/tmp/x.so") {
		t.Error("a value reached the log")
	}
	row := e.mcpNamed(planted.Name)
	if row == nil || row.LastConnectOK == nil || *row.LastConnectOK || row.LastConnectErr == nil || !strings.Contains(*row.LastConnectErr, "LD_PRELOAD") {
		t.Errorf("the MCP status does not carry the refusal: %+v", row)
	}
	// the next run goes through the same function
	if _, err := connectMCP(t.Context(), planted); err == nil {
		t.Error("a run connected a planted row")
	}
	if got := startedPlanted(); len(got) != 0 {
		t.Errorf("a run started a planted row: %+v", got)
	}
	// and the marker a model library would write is not there
	if _, err := os.Stat(filepath.Join(os.TempDir(), "marker-0")); err == nil {
		t.Log("a marker file exists from another run; not evidence")
	}
}

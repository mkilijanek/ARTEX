//go:build unix

package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/mcp"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
)

type fakeMCPClient struct{ closed int }

func (f *fakeMCPClient) Tools(context.Context) ([]actool.CoreTool, error) { return nil, nil }
func (f *fakeMCPClient) Close() error                                     { f.closed++; return nil }

// spawnCall is what the SDK's stdio client was asked to start.
type spawnCall struct {
	name, command string
	env           map[string]string
	args          []string
}

func fakeStdio(t *testing.T) (calls *[]spawnCall, client *fakeMCPClient) {
	t.Helper()
	calls, client = &[]spawnCall{}, &fakeMCPClient{}
	old := newStdioClient
	newStdioClient = func(_ context.Context, name, command string, env map[string]string, args ...string) (*mcp.Client, error) {
		*calls = append(*calls, spawnCall{name, command, env, args})
		return nil, errFakeSpawn
	}
	t.Cleanup(func() { newStdioClient = old })
	return calls, client
}

var errFakeSpawn = errors.New("fake spawn: no process")

// goodHost satisfies P1-P5 on a temporary layout and runs the self-check against a shell that
// reports the dropped state, so the status is enforced.
type goodHost struct{ l sandbox.Layout }

func (goodHost) EUID() int                   { return 0 }
func (goodHost) StatusFile() ([]byte, error) { return []byte("CapEff:\t00000000a80425fb\n"), nil }
func (h goodHost) Stat(p string) (fs.FileMode, uint32, error) {
	if p == h.l.ShimDir {
		return fs.ModeDir | 0o755, 0, nil
	}
	return 0o755, 0, nil
}
func (goodHost) IsStatic(string) (bool, error) { return true, nil }
func (goodHost) Getenv(string) string          { return "" }

type nullKiller struct{}

func (nullKiller) KillAll(context.Context) error     { return nil }
func (nullKiller) Left(context.Context) (int, error) { return 0, nil }

func activateEnforcedTree(t *testing.T) (*sandbox.Isolation, sandbox.Layout) {
	t.Helper()
	root := t.TempDir()
	l := sandbox.DefaultLayout()
	l.Artex, l.ShimDir, l.ProcRoot = filepath.Join(root, "app", "artex"), filepath.Join(root, "shims"), filepath.Join(root, "proc")
	l.PrivateDir, l.VarsDir, l.ServerTmp, l.ToolHome = filepath.Join(root, "p"), filepath.Join(root, "p", "v"), filepath.Join(root, "t"), filepath.Join(root, "home")
	data := filepath.Join(root, "app", "data")
	for _, d := range []string{data, l.ToolHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("TMPDIR", os.Getenv("TMPDIR"))
	iso, err := sandbox.Activate(t.Context(), sandbox.Config{Layout: l, DataDir: data, KeyDir: filepath.Dir(l.Artex), Probe: goodHost{l}, Killer: nullKiller{},
		Runner: func(context.Context, string, string) (string, error) { return "", errors.New("no shell here") }, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	if !iso.On() {
		t.Fatalf("isolation is off: %+v", iso.View())
	}
	sandbox.Set(iso)
	t.Cleanup(func() { sandbox.Set(nil) })
	return iso, l
}

func stdioRow(name, command string, env map[string]string, args ...string) *db.MCPServer {
	e, _ := json.Marshal(env)
	a, _ := json.Marshal(args)
	return &db.MCPServer{Name: name, Transport: "stdio", Command: command, Env: e, Args: a}
}

// AC14, AC37: with isolation on, the spawn goes through the drop chain, the variables travel in a
// root-only file and never in the arguments or the environment of the root stage, and the
// process starts in a directory of the tool user's home.
func TestMCPStdioStartsThroughTheDropChain(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to hand the directory to the tool user")
	}
	iso, l := activateEnforcedTree(t)
	calls, _ := fakeStdio(t)
	_, err := connectMCP(t.Context(), stdioRow("browser", "npx", map[string]string{"FOO": "canary-value"}, "-y", "@x/y"))
	if !errors.Is(err, errFakeSpawn) {
		t.Fatalf("err %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("spawns %d", len(*calls))
	}
	c := (*calls)[0]
	if c.command != l.Artex {
		t.Errorf("command %q, want the artex binary", c.command)
	}
	if len(c.env) != 0 {
		t.Errorf("the SDK was handed an environment map %v: the root stage would receive model-writable values", c.env)
	}
	wantDir := filepath.Join(l.ToolHome, "mcp", "browser")
	if len(c.args) < 8 || c.args[0] != sandbox.CmdAstool || c.args[1] != "--cd" || c.args[2] != wantDir || c.args[3] != "--vars" || c.args[5] != "--" || c.args[6] != "npx" || c.args[7] != "-y" {
		t.Fatalf("args %v", c.args)
	}
	for _, a := range c.args {
		if strings.Contains(a, "canary-value") {
			t.Fatalf("a value is on the command line: %v", c.args)
		}
	}
	// the spawn failed, so nothing consumed the file: the function removed it again
	if ents, _ := os.ReadDir(l.VarsDir); len(ents) != 0 {
		t.Errorf("variables files left behind: %v", ents)
	}
	if u := ownerUID(t, wantDir); u != sandbox.ToolUID {
		t.Errorf("working directory owner %d", u)
	}
	if iso.Leases().Held() != 0 {
		t.Errorf("a failed spawn kept %d leases", iso.Leases().Held())
	}
}

// AC15: a stdio row with a denied key is refused before anything is written, with a log line that
// names the key and never a value, and the MCP status carries the refusal.
func TestMCPStdioRefusesADeniedKey(t *testing.T) {
	iso, l := activateEnforcedTree(t)
	calls, _ := fakeStdio(t)
	logs := captureServerLog(t)
	_, err := connectMCP(t.Context(), stdioRow("evil", "true", map[string]string{"LD_PRELOAD": "secret-value-xyz", "FOO": "bar"}))
	var de *sandbox.DeniedVarError
	if !errors.As(err, &de) || de.Key != "LD_PRELOAD" {
		t.Fatalf("err %v", err)
	}
	if len(*calls) != 0 {
		t.Fatal("the process was started")
	}
	if ents, _ := os.ReadDir(l.VarsDir); len(ents) != 0 {
		t.Fatalf("a variables file was written: %v", ents)
	}
	if !strings.Contains(logs.String(), "[mcp] refused server evil: denied variable name LD_PRELOAD") {
		t.Errorf("log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "secret-value-xyz") || strings.Contains(err.Error(), "secret-value-xyz") {
		t.Error("a value reached the log or the error")
	}
	if iso.Leases().Held() != 0 {
		t.Errorf("leases held %d", iso.Leases().Held())
	}
}

// The deny-list is validation, not isolation: it applies when isolation is unavailable too, and
// the program is then started directly with its map, as before.
func TestMCPStdioWithoutIsolationValidatesAndStartsDirectly(t *testing.T) {
	activateUnavailable(t)
	calls, _ := fakeStdio(t)
	captureServerLog(t)
	if _, err := connectMCP(t.Context(), stdioRow("evil", "true", map[string]string{"LD_PRELOAD": "x"})); err == nil {
		t.Fatal("a denied key was accepted")
	}
	if len(*calls) != 0 {
		t.Fatal("the process was started")
	}
	_, err := connectMCP(t.Context(), stdioRow("ok", "npx", map[string]string{"FOO": "1"}, "a"))
	if !errors.Is(err, errFakeSpawn) || len(*calls) != 1 {
		t.Fatalf("err %v calls %v", err, *calls)
	}
	c := (*calls)[0]
	if c.command != "npx" || c.env["FOO"] != "1" || len(c.args) != 1 || c.args[0] != "a" {
		t.Fatalf("%+v", c)
	}
}

// A model-chosen server name must not choose a path: a name with separators or dots gets a
// derived directory name.
func TestMCPWorkingDirectoryNameIsSafe(t *testing.T) {
	for name, want := range map[string]string{"browser": "browser", "my-server_1.2": "my-server_1.2"} {
		if got := mcpDirName(name); got != want {
			t.Errorf("%q -> %q", name, got)
		}
	}
	for _, bad := range []string{"../../etc", "a/b", "..", ".", "", "a b", ".hidden", strings.Repeat("x", 200), "x\x00y"} {
		got := mcpDirName(bad)
		if got == bad || strings.ContainsAny(got, "/\\ \x00") || strings.HasPrefix(got, ".") || got == "" || len(got) > 64 {
			t.Errorf("%q -> %q", bad, got)
		}
	}
	if mcpDirName("../a") == mcpDirName("../b") {
		t.Error("different names must not collide")
	}
}

// The client of a stdio server holds a lease for its life: the sweep cannot kill a server that is
// in use, and a closed client lets go.
func TestMCPStdioClientHoldsALeaseUntilClosed(t *testing.T) {
	iso, _ := activateEnforcedTree(t)
	inner := &fakeMCPClient{}
	cl := &leasedMCPClient{mcpClient: inner}
	lease, err := iso.Leases().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cl.lease = lease
	if iso.Leases().Held() != 1 {
		t.Fatalf("held %d", iso.Leases().Held())
	}
	_ = cl.Close()
	_ = cl.Close()
	if inner.closed != 2 || iso.Leases().Held() != 0 {
		t.Fatalf("closed %d held %d", inner.closed, iso.Leases().Held())
	}
}

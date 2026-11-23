package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The architecture test (AC27, AC39) keeps the session guard and the spawn rules honest. It reads
// every non-test .go file under agent/, server/ and cmd/ (test files are excluded: they may build
// sessions directly, and they do not ship in the binary) and fails on:
//
//   - a call to agentcore.NewSession or agentcore.Run outside agent/session_guard.go;
//   - an agentcore.Options{} literal without a Tools field;
//   - DisableBackgroundTasks set to true outside the reviewed places;
//   - a DefaultTools() or ShellSessionTools() call outside the reviewed list;
//   - exec.Command*, mcp.NewStdioClient, os.StartProcess or a literal "__astool"-style stage name
//     outside the sandbox package and the MCP spawn function (the lease rule);
//   - db.DSN() or config.PostgresDSN() after startup, agent.FromEnv() outside main (AC39).

// site names a function in a file. fn "" means anywhere in the file.
type site struct{ file, fn string }

// Reviewed places. Each entry was read by hand; a new one needs the same.
var (
	// DefaultTools(): the one constructor of the agent kinds' base list (the file tools in it run
	// out of process), and the tool catalog (names only).
	reviewedDefaultTools = []site{
		{"agent/sandbox_tools.go", "defaultTools"},
		{"agent/toolrag.go", "toolRAGKeep"},
	}
	// ShellSessionTools(): the per-agent injection at assembly time and the catalog.
	reviewedShellTools = []site{
		{"server/assembly.go", "wireTools"},
		{"agent/toolrag.go", "toolRAGKeep"},
	}
	// DisableBackgroundTasks: true. The connection test sets it (it runs no tools). The goal
	// decomposer sets it too: it has three domain tools and no shell, and without the flag the SDK
	// would add Monitor to it.
	reviewedNoBackground = []site{
		{"agent/provider.go", "connectionTestOptions"},
		{"agent/goals.go", "DecomposeGoalsWithProvider"},
	}
	// The SDK's stdio MCP client: the MCP spawn function file only (the function value is
	// referenced at file level and called by connectStdioMCP).
	reviewedMCPSpawn = []site{{"server/mcpdiscover.go", ""}}
	// Spawns of tool processes outside the sandbox package.
	reviewedSpawn = []site{
		// the stage glue replaces the process image; it is the drop chain itself
		{"cmd/artex/astool.go", ""},
		{"cmd/artex/toolenv.go", ""},
	}
	// Files allowed to read the database DSN and the environment-held LLM config: startup only.
	reviewedStartupReads = []site{
		{"cmd/artex/main.go", ""},
	}
)

func (s site) matches(file, fn string) bool {
	return s.file == file && (s.fn == "" || s.fn == fn)
}

func inList(list []site, file, fn string) bool {
	for _, s := range list {
		if s.matches(file, fn) {
			return true
		}
	}
	return false
}

// archFiles reads the non-test sources under the given roots, keyed by slash path relative to
// the repository root.
func archFiles(t *testing.T, root string, dirs ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dirs {
		err := filepath.WalkDir(filepath.Join(root, d), func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// importName returns the local name of an import path in a file ("" when not imported).
func importName(f *ast.File, path string) string {
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		if p != path {
			continue
		}
		if im.Name != nil {
			return im.Name.Name
		}
		return filepath.Base(p)
	}
	return ""
}

const (
	agentcorePath = "github.com/Autumn-27/norma/agentcore"
	normaToolPath = "github.com/Autumn-27/norma/tool"
	normaMCPPath  = "github.com/Autumn-27/norma/mcp"
)

// archViolations applies every rule to the files and returns one line per violation.
func archViolations(files map[string]string) []string {
	var out []string
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, files[name], 0)
		if err != nil {
			out = append(out, name+": "+err.Error())
			continue
		}
		core := importName(f, agentcorePath)
		tl := importName(f, normaToolPath)
		mcp := importName(f, normaMCPPath)
		pos := func(n ast.Node) string { return fmt.Sprintf("%s:%d", name, fset.Position(n.Pos()).Line) }

		var walk func(n ast.Node, fn string)
		walk = func(n ast.Node, fn string) {
			ast.Inspect(n, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					if x.Body != nil {
						walk(x.Body, x.Name.Name)
					}
					return false
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					id, _ := sel.X.(*ast.Ident)
					recv := ""
					if id != nil {
						recv = id.Name
					}
					switch {
					case core != "" && recv == core && (sel.Sel.Name == "NewSession" || sel.Sel.Name == "Run"):
						if name != "agent/session_guard.go" {
							out = append(out, pos(x)+": agentcore."+sel.Sel.Name+" outside the session guard")
						}
					case sel.Sel.Name == "DefaultTools" && tl != "" && recv == tl:
						if !inList(reviewedDefaultTools, name, fn) {
							out = append(out, pos(x)+": DefaultTools() outside the reviewed callers (in "+fn+")")
						}
					case sel.Sel.Name == "ShellSessionTools" && tl != "" && recv == tl:
						if !inList(reviewedShellTools, name, fn) {
							out = append(out, pos(x)+": ShellSessionTools() outside the reviewed callers (in "+fn+")")
						}
					case recv == "exec" && strings.HasPrefix(sel.Sel.Name, "Command"),
						recv == "os" && sel.Sel.Name == "StartProcess",
						recv == "syscall" && (sel.Sel.Name == "Exec" || sel.Sel.Name == "ForkExec"):
						if !inList(reviewedSpawn, name, fn) {
							out = append(out, pos(x)+": a process spawn ("+recv+"."+sel.Sel.Name+") outside the sandbox package and the MCP spawn function (in "+fn+")")
						}
					case recv == "db" && sel.Sel.Name == "DSN", recv == "config" && sel.Sel.Name == "PostgresDSN", recv == "pgdb" && sel.Sel.Name == "DSN":
						if !inList(reviewedStartupReads, name, fn) {
							out = append(out, pos(x)+": the database DSN is read outside startup ("+recv+"."+sel.Sel.Name+")")
						}
					case recv == "agent" && sel.Sel.Name == "FromEnv":
						if !inList(reviewedStartupReads, name, fn) {
							out = append(out, pos(x)+": the LLM config is read from the environment outside startup")
						}
					}
				case *ast.SelectorExpr:
					if id, _ := x.X.(*ast.Ident); id != nil && mcp != "" && id.Name == mcp && x.Sel.Name == "NewStdioClient" {
						if !inList(reviewedMCPSpawn, name, fn) {
							out = append(out, pos(x)+": mcp.NewStdioClient outside the MCP spawn function (in "+fn+")")
						}
					}
				case *ast.CompositeLit:
					if sel, ok := x.Type.(*ast.SelectorExpr); ok && core != "" {
						if id, _ := sel.X.(*ast.Ident); id != nil && id.Name == core && sel.Sel.Name == "Options" {
							hasTools := false
							for _, el := range x.Elts {
								kv, ok := el.(*ast.KeyValueExpr)
								if !ok {
									continue
								}
								k, _ := kv.Key.(*ast.Ident)
								if k == nil {
									continue
								}
								switch k.Name {
								case "Tools":
									hasTools = true
								case "DisableBackgroundTasks":
									if v, _ := kv.Value.(*ast.Ident); v != nil && v.Name == "true" && !inList(reviewedNoBackground, name, fn) {
										out = append(out, pos(kv)+": DisableBackgroundTasks: true outside the reviewed places (in "+fn+")")
									}
								}
							}
							if !hasTools {
								out = append(out, pos(x)+": agentcore.Options{} literal without a Tools field")
							}
						}
					}
				case *ast.AssignStmt:
					// opts.DisableBackgroundTasks = true
					for i, l := range x.Lhs {
						if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "DisableBackgroundTasks" && i < len(x.Rhs) {
							if v, _ := x.Rhs[i].(*ast.Ident); v != nil && v.Name == "true" && !inList(reviewedNoBackground, name, fn) {
								out = append(out, pos(x)+": DisableBackgroundTasks = true outside the reviewed places (in "+fn+")")
							}
						}
					}
				case *ast.BasicLit:
					if x.Kind == token.STRING {
						if v, err := strconv.Unquote(x.Value); err == nil {
							switch v {
							case "__astool", "__toolenv", "__killall", "__toolfs", "__migrate-workspaces":
								out = append(out, pos(x)+": the stage name "+v+" spelled out; use the sandbox constant")
							}
						}
					}
				}
				return true
			})
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fd.Body != nil {
					walk(fd.Body, fd.Name.Name)
				}
			} else {
				walk(d, "")
			}
		}
	}
	return out
}

func TestArchitectureOfSessionsAndSpawns(t *testing.T) {
	files := archFiles(t, "..", "agent", "server", "cmd")
	if len(files) < 50 {
		t.Fatalf("only %d files read: the scan is looking in the wrong place", len(files))
	}
	for _, v := range archViolations(files) {
		t.Error(v)
	}
}

// The scan must also see what it claims to forbid: each synthetic file below breaks one rule and
// has to be reported. A rule that silently stopped matching would let all of them through.
func TestArchitectureRulesCatchViolations(t *testing.T) {
	hdr := `package x
import (
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/mcp"
	"os/exec"
	"syscall"
	"os"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/agent"
)
var _ agentcore.Options
`
	for name, tc := range map[string]struct{ src, want string }{
		"NewSession":       {`func f(o agentcore.Options) { _ = agentcore.NewSession(o) }`, "outside the session guard"},
		"Run":              {`func f(o agentcore.Options) { agentcore.Run(nil, o, "") }`, "outside the session guard"},
		"Options no tools": {`func f() { _ = agentcore.Options{MaxTurns: 1} }`, "without a Tools field"},
		"background":       {`func f() { _ = agentcore.Options{Tools: nil, DisableBackgroundTasks: true} }`, "DisableBackgroundTasks"},
		"background set":   {`func f(o *agentcore.Options) { o.DisableBackgroundTasks = true }`, "DisableBackgroundTasks"},
		"default tools":    {`func f() { _ = actool.DefaultTools() }`, "DefaultTools()"},
		"shell tools":      {`func f() { _ = actool.ShellSessionTools() }`, "ShellSessionTools()"},
		"exec":             {`func f() { exec.Command("sh") }`, "process spawn"},
		"exec context":     {`func f() { exec.CommandContext(nil, "sh") }`, "process spawn"},
		"mcp stdio":        {`func f() { mcp.NewStdioClient(nil, "n", "c", nil) }`, "mcp.NewStdioClient"},
		"start process":    {`func f() { os.StartProcess("a", nil, nil) }`, "process spawn"},
		"syscall exec":     {`func f() { syscall.Exec("a", nil, nil) }`, "process spawn"},
		"stage name":       {`func f() { _ = []string{"__astool", "--"} }`, "stage name"},
		"file tool stage":  {`func f() { _ = []string{"__toolfs"} }`, "stage name"},
		"dsn":              {`func f() { db.DSN() }`, "DSN"},
		"from env":         {`func f() { agent.FromEnv() }`, "environment"},
	} {
		got := archViolations(map[string]string{"server/rogue.go": hdr + tc.src})
		found := false
		for _, g := range got {
			if strings.Contains(g, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: not reported (want %q), got %v", name, tc.want, got)
		}
	}
	// the reviewed places are accepted, and only exactly there
	ok := map[string]string{
		"agent/session_guard.go": hdr + `func f(o agentcore.Options) { agentcore.NewSession(o) }`,
		"agent/sandbox_tools.go": hdr + `func defaultTools() { _ = actool.DefaultTools() }`,
		"server/mcpdiscover.go": hdr + `var newStdioClient = mcp.NewStdioClient
func connectStdioMCP() { newStdioClient(nil, "n", "c", nil) }`,
		"agent/provider.go": hdr + `func connectionTestOptions() { _ = agentcore.Options{Tools: nil, DisableBackgroundTasks: true} }`,
	}
	if got := archViolations(ok); len(got) != 0 {
		t.Errorf("the reviewed places were reported: %v", got)
	}
	wrong := map[string]string{
		"agent/sandbox_tools.go": hdr + `func somethingElse() { _ = actool.DefaultTools() }`,
		"server/other.go":        hdr + `func other() { mcp.NewStdioClient(nil, "n", "c", nil) }`,
		"agent/provider.go":      hdr + `func notTheTest() { _ = agentcore.Options{Tools: nil, DisableBackgroundTasks: true} }`,
	}
	if got := archViolations(wrong); len(got) != 3 {
		t.Errorf("a reviewed file with another function must be reported: %v", got)
	}
}

// Test files are not scanned, and the shipped binary pulls in no test-only package.
func TestBinaryHasNoTestPackage(t *testing.T) {
	files := archFiles(t, "..", "agent")
	for n := range files {
		if strings.HasSuffix(n, "_test.go") {
			t.Fatalf("%s was scanned", n)
		}
	}
	// db/dbtest is the test-support package: no non-test file may import it
	for n, src := range archFiles(t, "..", "agent", "server", "cmd", "db", "sandbox", "traffic", "config") {
		if strings.Contains(src, "artex/db/dbtest\"") {
			t.Errorf("%s imports the test-support package db/dbtest", n)
		}
	}
}

// AC27: the binary's dependency closure contains no test-support package. Test files are excluded
// from the scan above because they do not ship; this confirms nothing test-only is compiled in.
func TestShippedBinaryDependsOnNoTestPackage(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool")
	}
	cmd := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/artex")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list failed: %v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		if p == "testing" || strings.HasPrefix(p, "testing/") || p == "net/http/httptest" || strings.HasSuffix(p, "/dbtest") {
			t.Errorf("the binary depends on the test-support package %s", p)
		}
	}
}

// AC30: the isolation override is read from the environment only. No agent, server, database or
// configuration code names the variable, and no setting about isolation exists, so neither the
// UI nor a model-writable row can switch it.
func TestOverrideHasNoSettingsKey(t *testing.T) {
	for n, src := range archFiles(t, "..", "agent", "server", "cmd", "db", "config", "intercept") {
		if strings.Contains(src, "ARTEX_TOOL_ISOLATION") {
			t.Errorf("%s names the override variable", n)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, n, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(x ast.Node) bool {
			c, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "GetSetting" && sel.Sel.Name != "SetSetting" && sel.Sel.Name != "UpsertSetting") || len(c.Args) == 0 {
				return true
			}
			if lit, ok := c.Args[0].(*ast.BasicLit); ok && strings.Contains(strings.ToLower(lit.Value), "isolation") {
				t.Errorf("%s: a setting named %s", n, lit.Value)
			}
			return true
		})
	}
}

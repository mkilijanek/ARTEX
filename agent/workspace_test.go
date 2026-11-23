//go:build unix

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
)

func workspaceLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old, flags := log.Writer(), log.Flags()
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })
	return &b
}

func ownerOf(t *testing.T, p string) (uint32, uint32) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid
}

func rootOnly(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: the workspace belongs to another user")
	}
}

func put(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), mode); err != nil {
		t.Fatal(err)
	}
}

// AC28: a session's workspace is made through EnsureWorkspace: tool user, 0700, under a root-owned
// 0711 category directory.
func TestPrepareRunDirMakesToolOwnedWorkspaces(t *testing.T) {
	rootOnly(t)
	activateForTest(t, &fakePlatform{euid: 0})
	data := t.TempDir()
	dir, err := prepareRunDir(t.Context(), data, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(data, "tasks", "5", "i3") {
		t.Fatalf("dir %q", dir)
	}
	for _, d := range []string{filepath.Join(data, "tasks", "5"), dir} {
		if u, g := ownerOf(t, d); u != sandbox.ToolUID || g != sandbox.ToolGID {
			t.Errorf("%s owner %d:%d", d, u, g)
		}
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v", d, fi.Mode().Perm())
		}
	}
	tasks := filepath.Join(data, "tasks")
	if u, _ := ownerOf(t, tasks); u != 0 {
		t.Errorf("the category directory belongs to %d", u)
	}
	if fi, _ := os.Stat(tasks); fi.Mode().Perm() != 0o711 {
		t.Errorf("category mode %v", fi.Mode().Perm())
	}
	if _, err := prepareSessionDir(t.Context(), data, "conv-9"); err != nil {
		t.Fatal(err)
	}
	if u, _ := ownerOf(t, filepath.Join(data, "sessions", "conv-9")); u != sandbox.ToolUID {
		t.Errorf("session directory owner %d", u)
	}
}

// AC13, AC28: a link or a file planted where a workspace belongs fails the start with a logged
// reason and nothing outside is touched.
func TestPrepareRunDirRefusesPlantedLinks(t *testing.T) {
	rootOnly(t)
	activateForTest(t, &fakePlatform{euid: 0})
	data := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	put(t, victim, 0o644)
	if err := os.Lchown(victim, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(outside, 0, 0); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(data, "tasks", "5", "keep"), 0o644)
	if err := os.Symlink(outside, filepath.Join(data, "tasks", "5", "i4")); err != nil { // i<next> -> outside
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "tasks", "6")); err != nil { // the task directory itself
		t.Fatal(err)
	}
	put(t, filepath.Join(data, "tasks", "7"), 0o644) // a file where the task directory belongs
	if err := os.Symlink(outside, filepath.Join(data, "sessions")); err != nil {
		t.Fatal(err)
	}
	logs := workspaceLog(t)
	for name, f := range map[string]func() error{
		"intent":   func() error { _, err := prepareRunDir(t.Context(), data, 5, 4); return err },
		"task":     func() error { _, err := prepareRunDir(t.Context(), data, 6, 0); return err },
		"file":     func() error { _, err := prepareRunDir(t.Context(), data, 7, 0); return err },
		"category": func() error { _, err := prepareSessionDir(t.Context(), data, "conv-1"); return err },
	} {
		if err := f(); err == nil {
			t.Errorf("%s: the start was not refused", name)
		}
	}
	if u, _ := ownerOf(t, victim); u != 0 {
		t.Errorf("the link target's file was changed: owner %d", u)
	}
	if u, _ := ownerOf(t, outside); u != 0 {
		t.Errorf("the link target was changed: owner %d", u)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Errorf("something was created through a link: %v", ents)
	}
	if n := strings.Count(logs.String(), "[workspace]"); n < 4 {
		t.Errorf("%d logged reasons, want one per refusal:\n%s", n, logs.String())
	}
}

// AC23: with isolation on, what an older server left in the workspace is handed over when a
// session starts; with it off nothing is changed (the tools are root's then).
func TestPrepareRunDirHandsOverWhatIsInTheWorkspace(t *testing.T) {
	rootOnly(t)
	data := t.TempDir()
	f := filepath.Join(data, "tasks", "5", "i3", "out", "result.txt")
	put(t, f, 0o644)
	for _, p := range []string{f, filepath.Dir(f)} {
		if err := os.Lchown(p, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prepareRunDir(t.Context(), data, 5, 3); err != nil { // isolation not active
		t.Fatal(err)
	}
	if u, _ := ownerOf(t, f); u != 0 {
		t.Fatalf("without isolation the walk ran: owner %d", u)
	}
	activateForTest(t, &fakePlatform{euid: 0})
	if _, err := prepareRunDir(t.Context(), data, 5, 3); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f, filepath.Dir(f)} {
		if u, _ := ownerOf(t, p); u != sandbox.ToolUID {
			t.Errorf("%s still owned by %d", p, u)
		}
	}
	// the next start finds a file the old version wrote in the meantime
	put(t, filepath.Join(data, "tasks", "5", "i3", "late.txt"), 0o644)
	if err := os.Lchown(filepath.Join(data, "tasks", "5", "i3", "late.txt"), 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRunDir(t.Context(), data, 5, 3); err != nil {
		t.Fatal(err)
	}
	if u, _ := ownerOf(t, filepath.Join(data, "tasks", "5", "i3", "late.txt")); u != sandbox.ToolUID {
		t.Errorf("a file written after the migration was not handed over: owner %d", u)
	}
}

// A start never reaches into another task's workspace, not through a link either.
func TestPrepareRunDirDoesNotRunOverAnotherTasksWorkspace(t *testing.T) {
	rootOnly(t)
	activateForTest(t, &fakePlatform{euid: 0})
	data := t.TempDir()
	theirs := filepath.Join(data, "tasks", "6", "sub", "theirs.txt")
	put(t, theirs, 0o644)
	for _, p := range []string{theirs, filepath.Dir(theirs), filepath.Join(data, "tasks", "6")} {
		if err := os.Lchown(p, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	put(t, filepath.Join(data, "tasks", "5", "mine.txt"), 0o644)
	if err := os.Symlink("../6", filepath.Join(data, "tasks", "5", "to-theirs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(data, "tasks", "6"), filepath.Join(data, "tasks", "5", "abs-to-theirs")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRunDir(t.Context(), data, 5, 0); err != nil {
		t.Fatal(err)
	}
	if u, _ := ownerOf(t, filepath.Join(data, "tasks", "5", "mine.txt")); u != sandbox.ToolUID {
		t.Errorf("own file owner %d", u)
	}
	for _, p := range []string{theirs, filepath.Dir(theirs), filepath.Join(data, "tasks", "6")} {
		if u, _ := ownerOf(t, p); u != 0 {
			t.Errorf("%s belongs to %d after another task's start", p, u)
		}
	}
}

// AC13: a worker started on an intent whose directory is a planted link fails with a logged
// reason before any session is built, and writes nothing through the link.
func TestWorkerWithAPlantedIntentDirectoryFailsToStart(t *testing.T) {
	rootOnly(t)
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("no test database configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	expID, err := d.CreateExploration("g", "c")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })
	ts := d.Exploration(expID)
	payload := map[string]any{"summary": "work"}
	intentID, err := ts.AddNode("intent", payload, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	activateForTest(t, &fakePlatform{euid: 0})
	data := t.TempDir()
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(data, "tasks", fmt.Sprint(expID), "keep"), 0o644)
	if err := os.Symlink(outside, filepath.Join(data, "tasks", fmt.Sprint(expID), fmt.Sprintf("i%d", intentID))); err != nil {
		t.Fatal(err)
	}
	called := false
	provider := captureUsageProvider{stream: func(context.Context, func(llm.StreamEvent, error) bool) { called = true }}
	logs := workspaceLog(t)
	w := NewWorker(provider, "test-model", data, nil, 0, 3)
	reason, _, err := w.Execute(t.Context(), "agent", expID, nil, ts, &db.Node{ID: intentID, Payload: raw}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("the worker started on a planted directory")
	}
	if reason != ReasonSessionStart {
		t.Errorf("reason %q", reason)
	}
	if called {
		t.Error("the model was called")
	}
	if !strings.Contains(logs.String(), "[workspace]") {
		t.Errorf("no logged reason:\n%s", logs.String())
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("something was written through the link: %v", ents)
	}
}

// The workspace paths of every agent kind are made in one place: no agent file makes a directory
// of its own.
func TestNoAgentMakesItsWorkspaceWithMkdirAll(t *testing.T) {
	fset := token.NewFileSet()
	for _, f := range []string{"worker.go", "planner.go", "mainagent.go", "chat.go"} {
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && (sel.Sel.Name == "MkdirAll" || sel.Sel.Name == "Mkdir") {
					t.Errorf("%s: os.%s call: a workspace is made by prepareWorkspace", fset.Position(call.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// The data directory itself is the server's and may not exist yet.
func TestPrepareRunDirMakesAMissingDataDirectory(t *testing.T) {
	data := filepath.Join(t.TempDir(), "not", "yet", "there")
	dir, err := prepareRunDir(t.Context(), data, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("%s: %v", dir, err)
	}
	if _, err := prepareSessionDir(t.Context(), data, "conv-1"); err != nil {
		t.Fatal(err)
	}
}

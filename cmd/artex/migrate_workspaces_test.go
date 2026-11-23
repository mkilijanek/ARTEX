//go:build !windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mkilijanek/artex/sandbox"
)

func ownerUIDOf(t *testing.T, p string) uint32 {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Uid
}

func plant(t *testing.T, data string) (outside string) {
	t.Helper()
	outside = filepath.Join(filepath.Dir(data), "outside")
	for _, p := range []string{filepath.Join(data, "tasks", "5", "f.txt"), filepath.Join(outside, "secret")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Lchown(p, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(data, "tasks", "5", "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "tasks", "6")); err != nil {
		t.Fatal(err)
	}
	return outside
}

func TestMigrateWorkspacesCommandOnALinkPlantedTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	data := filepath.Join(t.TempDir(), "data")
	outside := plant(t, data)
	var out bytes.Buffer
	code := runMigrateWorkspaces([]string{"--data", data}, &out)
	if code != 1 { // task 6 is a link: refused, so the run does not report success
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if u := ownerUIDOf(t, filepath.Join(data, "tasks", "5", "f.txt")); u != sandbox.ToolUID {
		t.Errorf("owner %d", u)
	}
	if u := ownerUIDOf(t, filepath.Join(outside, "secret")); u != 0 {
		t.Errorf("the file behind a link was changed: owner %d", u)
	}
	if u := ownerUIDOf(t, outside); u != 0 {
		t.Errorf("the directory behind a link was changed: owner %d", u)
	}
	for _, want := range []string{`"tasks/6"`, "refused", "1 workspaces done", "1 refused"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// only the named task: success
	out.Reset()
	if code := runMigrateWorkspaces([]string{"--data", data, "--task", "5"}, &out); code != 0 {
		t.Fatalf("--task 5: exit %d\n%s", code, out.String())
	}
}

func TestMigrateWorkspacesCommandRefusesWhileTheServerIsRunning(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	data := filepath.Join(t.TempDir(), "data")
	plant(t, data)
	// a server holds the lock (shared, for the life of the process)
	if err := sandbox.HoldServerLock(data); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := runMigrateWorkspaces([]string{"--data", data}, &out)
	if code != 3 || !strings.Contains(out.String(), "stop it first") {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if u := ownerUIDOf(t, filepath.Join(data, "tasks", "5", "f.txt")); u != 0 {
		t.Errorf("the run changed ownership while the server was up: owner %d", u)
	}
}

func TestMigrateWorkspacesCommandUsage(t *testing.T) {
	var out bytes.Buffer
	if code := runMigrateWorkspaces([]string{"--nope"}, &out); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
	if code := runMigrateWorkspaces([]string{"extra"}, &out); code != 2 {
		t.Errorf("extra argument: exit %d", code)
	}
	if os.Geteuid() != 0 {
		if code := runMigrateWorkspaces([]string{"--data", t.TempDir()}, &out); code != 1 {
			t.Errorf("not root: exit %d", code)
		}
	}
}

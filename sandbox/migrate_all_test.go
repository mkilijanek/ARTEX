//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkTree builds a data directory with a planted-link workspace set: task 5 (root-owned files, a
// link to a sibling task, a link out, a hard link), task 6 (a sibling whose files must not be
// touched through task 5), task 7 that is itself a link, a file where a workspace belongs, and a
// chat session.
func linkTree(t *testing.T) (data, outside string) {
	t.Helper()
	root := t.TempDir()
	data, outside = filepath.Join(root, "data"), filepath.Join(root, "outside")
	mustWrite(t, filepath.Join(outside, "secret"), "S", 0o600)
	mustWrite(t, filepath.Join(data, "tasks", "5", "a", "b", "c.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(data, "tasks", "5", "top.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(data, "tasks", "6", "theirs.txt"), "t", 0o644)
	mustWrite(t, filepath.Join(data, "sessions", "conv-1", "s.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(data, "datafile"), "d", 0o600)
	for _, p := range []string{
		"tasks/5", "tasks/5/a", "tasks/5/a/b", "tasks/5/a/b/c.txt", "tasks/5/top.txt",
		"tasks/6", "tasks/6/theirs.txt", "sessions/conv-1", "sessions/conv-1/s.txt", "datafile",
	} {
		mustChownRoot(t, filepath.Join(data, p))
	}
	for name, target := range map[string]string{
		"tasks/5/to-sibling": "../6", "tasks/5/to-outside": outside, "tasks/5/to-secret": filepath.Join(outside, "secret"),
	} {
		if err := os.Symlink(target, filepath.Join(data, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(data, "datafile"), filepath.Join(data, "tasks", "5", "hl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "tasks", "7")); err != nil { // a workspace that is a link
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(data, "tasks", "8"), "a file", 0o644) // a file where a workspace belongs
	return data, outside
}

func TestMigrateAllFinishesEveryWorkspaceWithoutFollowingLinks(t *testing.T) {
	needRoot(t)
	data, outside := linkTree(t)
	lb := &logBuf{}
	rep, err := MigrateAllWorkspaces(t.Context(), data, "", lb.logf)
	if err != nil {
		t.Fatal(err)
	}
	// task 5, task 6 and the session are done; the linked task 7 and the file 8 are refused
	if rep.Workspaces != 3 || rep.Failed != 2 {
		t.Fatalf("report %+v\n%s", rep, lb.text())
	}
	for _, p := range []string{"tasks/5", "tasks/5/a", "tasks/5/a/b/c.txt", "tasks/5/top.txt", "tasks/6", "tasks/6/theirs.txt", "sessions/conv-1/s.txt"} {
		if u := uidOf(t, filepath.Join(data, p)); u != ToolUID {
			t.Errorf("%s owner %d", p, u)
		}
	}
	if m := mode(t, filepath.Join(data, "tasks", "5")); m != 0o700 {
		t.Errorf("workspace mode %v", m)
	}
	// nothing was reached through a link or by the hard link
	for _, p := range []string{outside, filepath.Join(outside, "secret")} {
		if u := uidOf(t, p); u != uint32(os.Geteuid()) {
			t.Errorf("%s was changed through a link: owner %d", p, u)
		}
	}
	for _, p := range []string{"datafile", "tasks/5/hl"} {
		if u := uidOf(t, filepath.Join(data, p)); u != 0 {
			t.Errorf("%s: a file with two names was changed (owner %d)", p, u)
		}
	}
	if rep.HardLinks != 1 {
		t.Errorf("hard links %d", rep.HardLinks)
	}
	// the category directories are the server's and were not handed over
	for _, p := range []string{"tasks", "sessions"} {
		if u := uidOf(t, filepath.Join(data, p)); u != 0 {
			t.Errorf("%s belongs to %d", p, u)
		}
	}
	if fi, err := os.Lstat(filepath.Join(data, "tasks", "7")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the linked workspace was replaced: %v %v", fi, err)
	}
	txt := lb.text()
	for _, want := range []string{`"tasks/7"`, `"tasks/8"`, "refused"} {
		if !strings.Contains(txt, want) {
			t.Errorf("log lacks %s:\n%s", want, txt)
		}
	}
	// a second run hands over nothing
	rep2, err := MigrateAllWorkspaces(t.Context(), data, "", lb.logf)
	if err != nil || rep2.Chowned != 0 {
		t.Fatalf("second run %+v %v", rep2, err)
	}
}

func TestMigrateAllOnlyTheNamedTask(t *testing.T) {
	needRoot(t)
	data, _ := linkTree(t)
	rep, err := MigrateAllWorkspaces(t.Context(), data, "5", (&logBuf{}).logf)
	if err != nil || rep.Workspaces != 1 || rep.Failed != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	if u := uidOf(t, filepath.Join(data, "tasks", "6", "theirs.txt")); u != 0 {
		t.Errorf("another task was walked: owner %d", u)
	}
	if u := uidOf(t, filepath.Join(data, "sessions", "conv-1", "s.txt")); u != 0 {
		t.Errorf("a session was walked: owner %d", u)
	}
	for _, bad := range []string{"../6", "5/a", "x", "-1", ""} {
		if bad == "" {
			continue
		}
		if _, err := MigrateAllWorkspaces(t.Context(), data, bad, (&logBuf{}).logf); err == nil {
			t.Errorf("task %q accepted", bad)
		}
	}
	// the task given is a link: refused, nothing followed
	rep, err = MigrateAllWorkspaces(t.Context(), data, "7", (&logBuf{}).logf)
	if err != nil || rep.Failed != 1 || rep.Workspaces != 0 {
		t.Fatalf("linked task: %+v %v", rep, err)
	}
}

// The offline run has no entry bound: a tree larger than the session-start bound is finished in
// one go.
func TestMigrateAllHasNoBounds(t *testing.T) {
	needRoot(t)
	data := filepath.Join(t.TempDir(), "data")
	for i := 0; i < 30; i++ {
		mustWrite(t, filepath.Join(data, "tasks", "1", fmt.Sprintf("f%d", i)), "x", 0o644)
	}
	rep, err := MigrateAllWorkspaces(t.Context(), data, "", (&logBuf{}).logf)
	if err != nil || rep.Workspaces != 1 || rep.Chowned != 30 {
		t.Fatalf("%+v %v", rep, err)
	}
	// the options it builds are unbounded (the shipped bound is 200,000 handed over, 10 s)
	o := DefaultMigrateOptions()
	o.MaxEntries, o.MaxTime = 0, 0
	if o.MaxEntries != 0 || o.MaxTime != 0 {
		t.Fatal("bounds set")
	}
}

func TestServerLockExcludesTheOfflineRun(t *testing.T) {
	data := t.TempDir()
	rel, err := LockDataDirExclusive(data)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if err := HoldServerLock(data); err != nil {
		t.Fatal(err)
	}
	if _, err := LockDataDirExclusive(data); err != ErrServerRunning {
		t.Fatalf("a held server lock did not stop the offline run: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(data, ".isolation", "server.lock")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
	// two servers (or a test and a server) hold it together
	if err := HoldServerLock(data); err != nil {
		t.Fatalf("a second shared lock: %v", err)
	}
}

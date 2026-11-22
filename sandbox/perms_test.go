//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func owner(t *testing.T, path string) (uint32, uint32) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return st.Uid, st.Gid
}

type permTree struct {
	l                    Layout
	app, data, key       string
	in                   PermsInput
	outside, outsideFile string
}

func newPermTree(t *testing.T) permTree {
	t.Helper()
	root := t.TempDir()
	p := permTree{app: filepath.Join(root, "app"), data: filepath.Join(root, "app", "data")}
	p.l = DefaultLayout()
	p.l.Artex = filepath.Join(p.app, "artex")
	p.l.PrivateDir = filepath.Join(root, "run", "artex-private")
	p.l.VarsDir = filepath.Join(p.l.PrivateDir, "vars")
	p.l.ServerTmp = filepath.Join(root, "run", "artex-tmp")
	for _, d := range []string{p.app, p.data, p.l.PrivateDir, p.l.ServerTmp,
		filepath.Join(p.data, "tasks", "7"), filepath.Join(p.data, "sessions", "conv-1"),
		filepath.Join(p.data, "traffic", "_ca"), filepath.Join(p.data, "transcripts"), filepath.Join(p.data, "evidence"),
		filepath.Join(p.data, "archives", "tasks", ".restore"), filepath.Join(p.data, ".isolation")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(p.app, "artex"), filepath.Join(p.app, "start.sh"), filepath.Join(p.app, "jwt.key"),
		filepath.Join(p.data, "traffic", "_ca", "ca.pem"), filepath.Join(p.data, "stray.txt")} {
		if err := os.WriteFile(f, []byte("x"), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	p.outside = filepath.Join(root, "outside")
	p.outsideFile = filepath.Join(p.outside, "secret")
	if err := os.MkdirAll(p.outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.outsideFile, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.in = PermsInput{Layout: p.l, AppDir: p.app, KeyDir: p.app, DataDir: p.data, Drop: true}
	return p
}

func TestApplyPermissionsSetsTheTable(t *testing.T) {
	p := newPermTree(t)
	rep, err := ApplyPermissions(p.in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 0 {
		t.Fatalf("problems: %v", rep.Problems)
	}
	for path, want := range map[string]os.FileMode{
		p.app: 0o755, filepath.Join(p.app, "artex"): 0o755, filepath.Join(p.app, "start.sh"): 0o755,
		filepath.Join(p.app, "jwt.key"):      0o600,
		p.l.PrivateDir:                       0o700,
		p.l.VarsDir:                          0o700,
		p.l.ServerTmp:                        0o711,
		p.data:                               0o711,
		filepath.Join(p.data, "tasks"):       0o711,
		filepath.Join(p.data, "sessions"):    0o711,
		filepath.Join(p.data, "spill"):       0o711,
		filepath.Join(p.data, "drafts"):      0o711,
		filepath.Join(p.data, "noa"):         0o711,
		filepath.Join(p.data, "public"):      0o755,
		filepath.Join(p.data, "traffic"):     0o700,
		filepath.Join(p.data, "transcripts"): 0o700,
		filepath.Join(p.data, "evidence"):    0o700,
		filepath.Join(p.data, "archives"):    0o700,
		filepath.Join(p.data, ".isolation"):  0o700,
		filepath.Join(p.data, "stray.txt"):   0o600,
	} {
		if got := mode(t, path); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}

// The closed directories exist after the first pass, so the server creating them later (with
// its default mode) never leaves them open on a fresh volume.
func TestApplyPermissionsCreatesTheClosedDirectories(t *testing.T) {
	p := newPermTree(t)
	for _, d := range []string{"traffic", "transcripts", "evidence", "archives"} {
		os.RemoveAll(filepath.Join(p.data, d))
	}
	if _, err := ApplyPermissions(p.in); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"traffic", "transcripts", "evidence", "archives"} {
		if got := mode(t, filepath.Join(p.data, d)); got != 0o700 {
			t.Errorf("%s: mode %o, want 0700", d, got)
		}
	}
}

// Only the named directories are readable by the tool user; nothing the table does not list
// is left open, whatever it is called.
func TestApplyPermissionsClosesUnknownTopLevelEntries(t *testing.T) {
	p := newPermTree(t)
	if err := os.MkdirAll(filepath.Join(p.data, "future-dir"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPermissions(p.in); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, filepath.Join(p.data, "future-dir")); got != 0o700 {
		t.Errorf("an unlisted directory is mode %o, want 0700", got)
	}
}

// Top-level children of tasks and sessions go to the tool user at 0700.
func TestApplyPermissionsHandsWorkspacesToTheToolUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	p := newPermTree(t)
	if _, err := ApplyPermissions(p.in); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(p.data, "tasks", "7"), filepath.Join(p.data, "sessions", "conv-1")} {
		if u, g := owner(t, d); u != ToolUID || g != ToolGID {
			t.Errorf("%s owned by %d:%d", d, u, g)
		}
		if got := mode(t, d); got != 0o700 {
			t.Errorf("%s mode %o", d, got)
		}
	}
	for _, d := range []string{p.data, filepath.Join(p.data, "tasks"), filepath.Join(p.data, "traffic")} {
		if u, _ := owner(t, d); u != 0 {
			t.Errorf("%s is owned by %d, want root", d, u)
		}
	}
}

// A link in the way is never followed: the target keeps its mode and owner, the link is
// reported, and the step goes on.
func TestApplyPermissionsNeverFollowsLinks(t *testing.T) {
	p := newPermTree(t)
	links := map[string]string{
		filepath.Join(p.data, "linked-top"):      p.outside,     // top-level entry
		filepath.Join(p.data, "tasks", "linked"): p.outside,     // workspace child
		filepath.Join(p.app, "start.sh.link"):    p.outsideFile, // not in the table, ignored
	}
	for l, target := range links {
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	// replace jwt.key and start.sh by links to a file outside
	for _, name := range []string{"jwt.key", "start.sh"} {
		if err := os.Remove(filepath.Join(p.app, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p.outsideFile, filepath.Join(p.app, name)); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := ApplyPermissions(p.in)
	if err != nil {
		t.Fatal(err)
	}
	if got := mode(t, p.outside); got != 0o755 {
		t.Errorf("the link target directory changed to %o", got)
	}
	if got := mode(t, p.outsideFile); got != 0o644 {
		t.Errorf("the link target file changed to %o", got)
	}
	found := false
	for _, s := range rep.Symlinks {
		if s == filepath.Join(p.data, "linked-top") {
			found = true
		}
	}
	if !found {
		t.Errorf("the top-level link is not reported: %v", rep.Symlinks)
	}
	if len(rep.Problems) == 0 {
		t.Error("a refused link must be reported as a problem")
	}
}

// A missing optional file (no start.sh, no key yet) is not a problem.
func TestApplyPermissionsToleratesMissingOptionalFiles(t *testing.T) {
	p := newPermTree(t)
	os.Remove(filepath.Join(p.app, "jwt.key"))
	os.Remove(filepath.Join(p.app, "start.sh"))
	rep, err := ApplyPermissions(p.in)
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("err %v problems %v", err, rep.Problems)
	}
}

// Without the drop the run directories are left alone: a binary install has no /run/artex-private
// and must not get an error line for it at every start.
func TestApplyPermissionsLeavesTheRunDirectoriesWithoutTheDrop(t *testing.T) {
	p := newPermTree(t)
	os.Remove(p.l.VarsDir)
	os.RemoveAll(p.l.ServerTmp)
	p.in.Drop = false
	before := mode(t, p.l.PrivateDir)
	rep, err := ApplyPermissions(p.in)
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("err %v problems %v", err, rep.Problems)
	}
	if _, err := os.Lstat(p.l.ServerTmp); !os.IsNotExist(err) {
		t.Error("the server temp directory was created")
	}
	if got := mode(t, p.l.PrivateDir); got != before {
		t.Errorf("the private directory changed from %o to %o", before, got)
	}
}

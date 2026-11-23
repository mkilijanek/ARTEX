package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
)

func spillTestSession(t *testing.T, data, kind, id string) (dir string, closeFn func()) {
	t.Helper()
	dir, err := sessionSpillDir(data, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	_, closeFn, err = newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: noTools, ToolOutputDir: dir, SessionID: id}, SessionInfo{Kind: kind, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return dir, closeFn
}

func writeSpill(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "output-1.txt")
	if err := os.WriteFile(p, []byte("spilled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// A session leaves its spill directory when it ends: a resumed session still reads the files an
// earlier one spilled.
func TestSpillFilesOutliveTheirSessionAndAResumedSessionReadsThem(t *testing.T) {
	data := t.TempDir()
	dir, closeFn := spillTestSession(t, data, "chat", "c1")
	p := writeSpill(t, dir)
	closeFn()
	if !exists(p) {
		t.Fatal("the spill file went with its session")
	}
	dir2, closeFn2 := spillTestSession(t, data, "chat", "c1") // the next turn of the same chat
	defer closeFn2()
	if dir2 != dir {
		t.Fatalf("the resumed session got %s, not %s", dir2, dir)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "spilled\n" {
		t.Errorf("the resumed session cannot read the earlier spill file: %q, %v", b, err)
	}
}

// The sweep removes old idle session directories and leaves new ones, directories in use, links and
// anything outside the spill root.
func TestSweepSpillRemovesOnlyOldDirectories(t *testing.T) {
	data := t.TempDir()
	root := filepath.Join(data, "spill")
	now := time.Now()
	mk := func(kind, id string, age time.Duration) string {
		d, err := sessionSpillDir(data, kind, id)
		if err != nil {
			t.Fatal(err)
		}
		writeSpill(t, d)
		old := now.Add(-age)
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
		return d
	}
	oldDir := mk("worker", "old", 10*24*time.Hour)
	freshDir := mk("worker", "fresh", time.Hour)
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	os.WriteFile(keep, []byte("x"), 0o600)
	old := now.Add(-10 * 24 * time.Hour)
	link := filepath.Join(root, "worker", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(link, old, old); err != nil && !os.IsPermission(err) {
		t.Logf("chtimes on link: %v", err)
	}
	strayFile := filepath.Join(root, "worker", "stray")
	os.WriteFile(strayFile, []byte("x"), 0o600)
	os.Chtimes(strayFile, old, old)

	n, err := sweepSpill(root, spillMaxAge, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || exists(oldDir) {
		t.Errorf("removed %d; the old directory exists: %v", n, exists(oldDir))
	}
	for name, p := range map[string]string{"fresh": freshDir, "link": link, "stray file": strayFile, "outside": keep} {
		if !exists(p) {
			t.Errorf("the sweep removed the %s entry %s", name, p)
		}
	}
	// a missing spill root is nothing to do
	if n, err := sweepSpill(filepath.Join(data, "none"), spillMaxAge, now); n != 0 || err != nil {
		t.Errorf("missing root: %d, %v", n, err)
	}
}

// SweepSpill works on <data>/spill and takes only directories past the age limit.
func TestSweepSpillOfADataDirectory(t *testing.T) {
	data := t.TempDir()
	stale, err := sessionSpillDir(data, "worker", "stale")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := sessionSpillDir(data, "worker", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	writeSpill(t, stale)
	writeSpill(t, fresh)
	old := time.Now().Add(-spillMaxAge - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	SweepSpill(data)
	if exists(stale) || !exists(fresh) {
		t.Errorf("stale exists %v, fresh exists %v", exists(stale), exists(fresh))
	}
	SweepSpill(t.TempDir()) // no spill directory: nothing to do
}

//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: the walk changes ownership")
	}
}

func mustWrite(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func mustChownRoot(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Lchown(p, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func uidOf(t *testing.T, path string) uint32 {
	t.Helper()
	u, _ := owner(t, path)
	return u
}

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}
func (l *logBuf) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// AC28: the directories are the tool user's and 0700, existing ones are repaired, and nothing
// other than a plain directory is accepted in the path.
func TestEnsureWorkspaceCreatesToolOwnedPrivateDirectories(t *testing.T) {
	base := t.TempDir()
	got, err := EnsureWorkspace(base, "5", "i3")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(base, "5", "i3") {
		t.Fatalf("path %q", got)
	}
	for _, d := range []string{filepath.Join(base, "5"), got} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("%s mode %v", d, m)
		}
		if os.Geteuid() == 0 {
			if u, g := owner(t, d); u != ToolUID || g != ToolGID {
				t.Errorf("%s owner %d:%d", d, u, g)
			}
		}
	}
	// repaired when it exists with another mode
	if err := os.Chmod(got, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWorkspace(base, "5", "i3"); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, got); m != 0o700 {
		t.Errorf("existing directory left at %v", m)
	}
}

func TestEnsureWorkspaceRefusesLinksFilesAndOddNames(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"), "x", 0o600)
	before, _ := owner(t, outside)

	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(base, "inside")); err != nil { // stays inside the base
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(base, "file"), "x", 0o644)
	if err := os.MkdirAll(filepath.Join(base, "8"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "8", "i9")); err != nil { // a planted i<next>
		t.Fatal(err)
	}

	cases := map[string][]string{
		"link to the outside":      {"link", "x"},
		"link inside the base":     {"inside"},
		"file":                     {"file"},
		"planted intent directory": {"8", "i9"},
		"dotdot":                   {".."},
		"empty":                    {""},
		"separator":                {"a/b"},
		"no components":            nil,
	}
	for name, rel := range cases {
		if _, err := EnsureWorkspace(base, rel...); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("a directory was created through a link")
	}
	if m := mode(t, outside); m == 0o700 && os.Geteuid() == 0 {
		if u, _ := owner(t, outside); u != before {
			t.Error("the link target was handed to the tool user")
		}
	}
	if m := mode(t, filepath.Join(base, "real")); m != 0o755 {
		t.Errorf("the target of the inner link was changed to %v", m)
	}
}

func wsOpts(lb *logBuf) MigrateOptions {
	return MigrateOptions{Logf: lb.logf}
}

func TestMigrateHandsEverythingToTheToolUserPostOrder(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	files := []string{"a/b/c.txt", "a/b/d/e.txt", "top.txt", "a/x.bin"}
	for _, f := range files {
		mustWrite(t, filepath.Join(ws, f), "x", 0o644)
	}
	mustChownRoot(t, ws, filepath.Join(ws, "a"), filepath.Join(ws, "a/b"), filepath.Join(ws, "a/b/d"))
	for _, f := range files {
		mustChownRoot(t, filepath.Join(ws, f))
	}
	var order []string
	lb := &logBuf{}
	o := wsOpts(lb)
	o.onChown = func(rel string) { order = append(order, rel) }
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Complete || rep.Chowned != 7 {
		t.Fatalf("report %+v log:\n%s", rep, lb.text())
	}
	for _, f := range append(files, "a", "a/b", "a/b/d") {
		if u := uidOf(t, filepath.Join(ws, f)); u != ToolUID {
			t.Errorf("%s still owned by %d", f, u)
		}
	}
	// post-order: a directory is handed over after everything in it
	pos := map[string]int{}
	for i, r := range order {
		pos[r] = i
	}
	for _, d := range []string{"a", "a/b", "a/b/d"} {
		for p, i := range pos {
			if strings.HasPrefix(p, d+"/") && i > pos[d] {
				t.Errorf("%s was handed over before %s", d, p)
			}
		}
	}
	// idempotent
	rep2, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil || !rep2.Complete || rep2.Chowned != 0 {
		t.Fatalf("second run %+v %v", rep2, err)
	}
}

// AC23: a link is changed as a link and never followed, whatever it points to.
func TestMigrateNeverFollowsLinks(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	ws := filepath.Join(root, "tasks", "5")
	sibling := filepath.Join(root, "tasks", "6")
	outside := filepath.Join(root, "secret")
	mustWrite(t, filepath.Join(ws, "f"), "x", 0o644)
	mustWrite(t, filepath.Join(sibling, "theirs.txt"), "t", 0o644)
	mustWrite(t, filepath.Join(sibling, "sub", "deep.txt"), "t", 0o644)
	mustWrite(t, outside, "s", 0o600)
	mustChownRoot(t, filepath.Join(ws, "f"), outside, sibling, filepath.Join(sibling, "theirs.txt"), filepath.Join(sibling, "sub"), filepath.Join(sibling, "sub", "deep.txt"))
	mustChownRoot(t, ws)
	for name, target := range map[string]string{
		"to-file":    outside,
		"to-sibling": sibling,
		"to-up":      "../6",
		"to-dir":     root,
		"to-self":    ".",
		"dangling":   "/nonexistent-target",
	} {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := MigrateWorkspace(t.Context(), ws, wsOpts(&logBuf{})); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, sibling, filepath.Join(sibling, "theirs.txt"), filepath.Join(sibling, "sub"), filepath.Join(sibling, "sub", "deep.txt")} {
		if u := uidOf(t, p); u != 0 {
			t.Errorf("%s was reached through a link: owner %d", p, u)
		}
	}
	if u := uidOf(t, filepath.Join(ws, "f")); u != ToolUID {
		t.Errorf("the workspace's own file was not handed over: %d", u)
	}
	for _, l := range []string{"to-file", "to-sibling", "to-up", "to-dir", "to-self", "dangling"} {
		fi, err := os.Lstat(filepath.Join(ws, l))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("link %s was replaced: %v %v", l, fi, err)
		}
	}
}

// A directory swapped for a link between the walk's classification and its open is refused.
func TestMigrateRefusesADirectorySwappedForALink(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	ws := filepath.Join(root, "w")
	outside := filepath.Join(root, "outside")
	mustWrite(t, filepath.Join(ws, "d", "inner.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(outside, "victim.txt"), "v", 0o644)
	mustChownRoot(t, ws, filepath.Join(ws, "d"), filepath.Join(ws, "d", "inner.txt"), outside, filepath.Join(outside, "victim.txt"))
	lb := &logBuf{}
	o := wsOpts(lb)
	swapped := false
	o.beforeOpen = func(rel string) {
		if rel == "d" && !swapped {
			swapped = true
			if err := os.Rename(filepath.Join(ws, "d"), filepath.Join(ws, "d.moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(ws, "d")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := MigrateWorkspace(t.Context(), ws, o); err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("hook not called")
	}
	for _, p := range []string{outside, filepath.Join(outside, "victim.txt")} {
		if u := uidOf(t, p); u != 0 {
			t.Errorf("%s followed the swapped link: owner %d", p, u)
		}
	}
}

// The workspace is the tool user's: between the walk's look at an entry and the change of its
// owner, a tool can put a hard link to a server file at that name. The change must land on the
// entry that was looked at or nowhere, for files and for directories (changed after their
// contents) alike.
func TestMigrateChangesOnlyTheEntryItLookedAt(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	ws := filepath.Join(root, "w")
	secret := filepath.Join(root, "secret")
	mustWrite(t, secret, "s", 0o600)
	mustWrite(t, filepath.Join(ws, "f"), "x", 0o644)
	mustWrite(t, filepath.Join(ws, "d", "inner.txt"), "x", 0o644)
	mustChownRoot(t, secret, ws, filepath.Join(ws, "f"), filepath.Join(ws, "d"), filepath.Join(ws, "d", "inner.txt"))
	lb := &logBuf{}
	o := wsOpts(lb)
	swapped := map[string]bool{}
	o.onChown = func(rel string) {
		if rel != "f" && rel != "d" {
			return
		}
		p := filepath.Join(ws, rel)
		if err := os.Rename(p, p+".moved"); err != nil {
			t.Error(err)
		}
		if err := os.Link(secret, p); err != nil {
			t.Error(err)
		}
		swapped[rel] = true
	}
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil {
		t.Fatal(err)
	}
	if !swapped["f"] || !swapped["d"] {
		t.Fatalf("hook calls %v", swapped)
	}
	if u := uidOf(t, secret); u != 0 {
		t.Errorf("the linked server file was handed to the tool user: owner %d", u)
	}
	if rep.Changed != 2 || rep.Complete {
		t.Errorf("report %+v, want 2 changed entries and an incomplete walk; log:\n%s", rep, lb.text())
	}
	if u := uidOf(t, filepath.Join(ws, "d.moved", "inner.txt")); u != ToolUID {
		t.Errorf("the directory's own file was not handed over: %d", u)
	}
}

func TestMigrateSkipsHardLinksAndClearsSetIDBits(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	ws := filepath.Join(root, "w")
	data := filepath.Join(root, "datafile")
	mustWrite(t, data, "secret", 0o600)
	mustWrite(t, filepath.Join(ws, "plain.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(ws, "suid"), "x", 0o755)
	if err := os.Chmod(filepath.Join(ws, "suid"), os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(data, filepath.Join(ws, "hl")); err != nil {
		t.Fatal(err)
	}
	mustChownRoot(t, data, filepath.Join(ws, "plain.txt"), filepath.Join(ws, "suid"), ws)
	lb := &logBuf{}
	rep, err := MigrateWorkspace(t.Context(), ws, wsOpts(lb))
	if err != nil {
		t.Fatal(err)
	}
	if rep.HardLinks != 1 {
		t.Errorf("hard links %d", rep.HardLinks)
	}
	if u := uidOf(t, filepath.Join(ws, "hl")); u != 0 {
		t.Errorf("a hard-linked file was changed: owner %d", u)
	}
	if u := uidOf(t, data); u != 0 {
		t.Errorf("the other name of the file was changed: owner %d", u)
	}
	if !strings.Contains(lb.text(), "hl") {
		t.Errorf("the skipped hard link was not logged:\n%s", lb.text())
	}
	fi, err := os.Lstat(filepath.Join(ws, "suid"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("set-uid bit survived: %v", fi.Mode())
	}
	if u := uidOf(t, filepath.Join(ws, "suid")); u != ToolUID {
		t.Errorf("owner %d", u)
	}
}

// The walk is bounded by the entries it hands over and by time, logs progress, and an incomplete
// walk is not an error: the session still starts.
func TestMigrateStopsAtTheEntryLimitWithProgress(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	for i := 0; i < 100; i++ {
		mustWrite(t, filepath.Join(ws, fmt.Sprintf("d%d", i%10), fmt.Sprintf("f%d", i)), "x", 0o644)
	}
	lb := &logBuf{}
	o := wsOpts(lb)
	o.MaxEntries = 50
	o.ProgressEvery = 10
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil {
		t.Fatalf("hitting the limit is not an error: %v", err)
	}
	if rep.Complete || rep.Chowned != 50 || !strings.Contains(rep.Reason, "entr") {
		t.Fatalf("report %+v", rep)
	}
	txt := lb.text()
	if n := strings.Count(txt, "entries walked"); n < 4 {
		t.Errorf("%d progress lines:\n%s", n, txt)
	}
	if !strings.Contains(txt, "stopped") {
		t.Errorf("the stop was not logged:\n%s", txt)
	}
}

// Entries that are already right are visited but do not count towards the bound, so repeated starts
// converge: a tree twice the bound is finished by the second pass, and a finished tree costs one
// visiting pass that hands over nothing and is complete whatever the bound.
func TestMigrateConvergesInTwoPassesAndAFinishedTreeCostsOneVisit(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	const dirs, perDir = 20, 99 // 20 directories + 1,980 files = 2,000 entries
	for d := 0; d < dirs; d++ {
		for f := 0; f < perDir; f++ {
			mustWrite(t, filepath.Join(ws, fmt.Sprintf("d%02d", d), fmt.Sprintf("f%03d", f)), "", 0o644)
		}
	}
	total := dirs * (perDir + 1)
	var passes []MigrateReport
	for pass := 0; pass < 4; pass++ {
		o := wsOpts(&logBuf{})
		o.MaxEntries = total / 2
		rep, err := MigrateWorkspace(t.Context(), ws, o)
		if err != nil {
			t.Fatal(err)
		}
		passes = append(passes, rep)
		if rep.Complete {
			break
		}
	}
	if len(passes) != 2 || !passes[1].Complete || passes[0].Complete || passes[0].Chowned != total/2 || passes[1].Chowned != total-total/2 {
		t.Fatalf("passes: %+v", passes)
	}
	// the tree is finished: one pass visits everything, hands over nothing, completes
	o := wsOpts(&logBuf{})
	o.MaxEntries = 1
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil || !rep.Complete || rep.Chowned != 0 || rep.Entries != total {
		t.Fatalf("a finished tree: %+v %v", rep, err)
	}
	for d := 0; d < dirs; d++ {
		if u := uidOf(t, filepath.Join(ws, fmt.Sprintf("d%02d", d), "f000")); u != ToolUID {
			t.Fatalf("d%02d/f000 owner %d", d, u)
		}
	}
}

func TestMigrateStopsAtTheTimeLimit(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	for i := 0; i < 40; i++ {
		mustWrite(t, filepath.Join(ws, fmt.Sprintf("f%d", i)), "x", 0o644)
	}
	var tick time.Time = time.Unix(1_700_000_000, 0)
	lb := &logBuf{}
	o := wsOpts(lb)
	o.MaxTime = 10 * time.Second
	o.Now = func() time.Time { tick = tick.Add(time.Second); return tick }
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Complete || !strings.Contains(rep.Reason, "time") {
		t.Fatalf("report %+v", rep)
	}
}

func TestMigrateStopsOnACancelledContext(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	mustWrite(t, filepath.Join(ws, "f"), "x", 0o644)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rep, err := MigrateWorkspace(ctx, ws, wsOpts(&logBuf{}))
	if err == nil || rep.Complete {
		t.Fatalf("a cancelled walk reported success: %+v %v", rep, err)
	}
}

// A tree deeper than the bound is not walked below it, with a logged reason, instead of holding a
// descriptor per level.
func TestMigrateBoundsTheDepth(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	deep := ws
	for i := 0; i < maxWalkDepth+20; i++ {
		deep = filepath.Join(deep, "d")
	}
	mustWrite(t, filepath.Join(deep, "f"), "x", 0o644)
	mustChownRoot(t, filepath.Join(deep, "f"))
	lb := &logBuf{}
	rep, err := MigrateWorkspace(t.Context(), ws, wsOpts(lb))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Complete || !strings.Contains(lb.text(), "deeper") {
		t.Fatalf("report %+v\n%s", rep, lb.text())
	}
}

// The stamp lets a later walk leave the files of an unchanged directory alone; a directory that
// changed since the last complete walk is looked at again, wherever it is in the tree; an
// incomplete walk leaves no stamp.
func TestMigrateStampSkipsOnlyWhatIsOlderThanTheLastCompleteWalk(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	ws := filepath.Join(root, "tasks", "5")
	stamp := filepath.Join(root, ".isolation", "workspaces")
	mustWrite(t, filepath.Join(ws, "a", "b", "old.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(ws, "a", "c", "other.txt"), "x", 0o644)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		t.Fatal(err)
	}
	lb := &logBuf{}
	opts := func(now time.Time) MigrateOptions {
		o := wsOpts(lb)
		o.StampPath, o.StampKey = stamp, "tasks/5"
		o.Now = func() time.Time { return now }
		return o
	}
	// first walk: complete, stamped at a time after every change so far
	future := time.Now().Add(time.Hour)
	rep, err := MigrateWorkspace(t.Context(), ws, opts(future))
	if err != nil || !rep.Complete {
		t.Fatalf("%+v %v", rep, err)
	}
	if fi, err := os.Stat(stamp); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stamp: %v %v", fi, err)
	}
	// a root-owned file appears in a deep directory; the directories above it do not change
	root1 := filepath.Join(ws, "a", "b", "new.txt")
	mustWrite(t, root1, "x", 0o644)
	mustChownRoot(t, root1)
	back := future.Add(2 * time.Hour) // pretend the stamp is newer than the change: the directory is "unchanged"
	if _, err := MigrateWorkspace(t.Context(), ws, opts(back)); err != nil {
		t.Fatal(err)
	}
	if u := uidOf(t, root1); u != 0 {
		t.Fatalf("a file in a directory older than the stamp was visited (owner %d): the stamp is not used", u)
	}
	// make the stamp older than the change: now the directory is changed and the file is fixed
	earlier := time.Now().Add(-time.Hour)
	if _, err := MigrateWorkspace(t.Context(), ws, withStamp(opts(earlier), stamp, "tasks/5", earlier)); err != nil {
		t.Fatal(err)
	}
	if u := uidOf(t, root1); u != ToolUID {
		t.Fatalf("a file in a changed directory was not handed over (owner %d)", u)
	}
}

// withStamp rewrites the stored stamp of key to t, as a walk at that time would have left it.
func withStamp(o MigrateOptions, path, key string, at time.Time) MigrateOptions {
	st := newStampStore(path)
	_ = st.set(key, at)
	return o
}

func TestMigrateIncompleteWalkLeavesNoStamp(t *testing.T) {
	needRoot(t)
	root := t.TempDir()
	ws := filepath.Join(root, "tasks", "5")
	stamp := filepath.Join(root, ".isolation", "workspaces")
	for i := 0; i < 30; i++ {
		mustWrite(t, filepath.Join(ws, fmt.Sprintf("f%d", i)), "x", 0o644)
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o700); err != nil {
		t.Fatal(err)
	}
	o := wsOpts(&logBuf{})
	o.StampPath, o.StampKey, o.MaxEntries = stamp, "tasks/5", 10
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil || rep.Complete {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := newStampStore(stamp).get("tasks/5"); err == nil {
		t.Error("an incomplete walk left a stamp")
	}
}

// Two sessions of one task start together: the walks do not run in parallel over the same tree
// and both finish.
func TestMigrateSerializesWalksOfOneWorkspace(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	for i := 0; i < 50; i++ {
		mustWrite(t, filepath.Join(ws, fmt.Sprintf("f%d", i)), "x", 0o644)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	active, maxActive := 0, 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := wsOpts(&logBuf{})
			o.onChown = func(string) {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
			}
			if _, err := MigrateWorkspace(t.Context(), ws, o); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxActive > 1 {
		t.Errorf("%d walks changed ownership at the same time", maxActive)
	}
}

func TestMigrateRefusesALinkOrAFileAsTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "f"), "x", 0o644)
	for _, p := range []string{filepath.Join(root, "l"), filepath.Join(root, "f"), filepath.Join(root, "missing")} {
		if _, err := MigrateWorkspace(t.Context(), p, wsOpts(&logBuf{})); err == nil {
			t.Errorf("%s accepted as a workspace", p)
		}
	}
}

var _ = syscall.EPERM

// The same swap, for a link whose target stays inside the workspace: os.Root would follow it, so
// only the comparison with the classified directory stops the walk from going in.
func TestMigrateRefusesADirectorySwappedForALinkInsideTheWorkspace(t *testing.T) {
	needRoot(t)
	ws := filepath.Join(t.TempDir(), "w")
	mustWrite(t, filepath.Join(ws, "d", "inner.txt"), "x", 0o644)
	mustWrite(t, filepath.Join(ws, "other", "o.txt"), "x", 0o644)
	lb := &logBuf{}
	o := wsOpts(lb)
	swapped := false
	o.beforeOpen = func(rel string) {
		if rel == "d" && !swapped {
			swapped = true
			if err := os.Rename(filepath.Join(ws, "d"), filepath.Join(ws, "d.moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("other", filepath.Join(ws, "d")); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep, err := MigrateWorkspace(t.Context(), ws, o)
	if err != nil {
		t.Fatal(err)
	}
	if !swapped || !strings.Contains(lb.text(), "changed while it was being walked") || rep.Complete {
		t.Fatalf("the swapped directory was entered: swapped=%v report %+v\n%s", swapped, rep, lb.text())
	}
}

// Without isolation the directories are made and checked the same way but not handed to the tool
// user.
func TestEnsureWorkspaceKeepOwnerDoesNotChangeTheOwner(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	got, err := EnsureWorkspaceKeepOwner(base, "5", "i3")
	if err != nil {
		t.Fatal(err)
	}
	if u := uidOf(t, got); u != uint32(os.Geteuid()) {
		t.Errorf("owner %d", u)
	}
	if m := mode(t, got); m != 0o700 {
		t.Errorf("mode %v", m)
	}
	if err := os.Symlink(outside, filepath.Join(base, "l")); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWorkspaceKeepOwner(base, "l"); err == nil {
		t.Error("a link was accepted")
	}
}

// OpenWorkspace gives the directory that was checked: a file made through it lands there even if
// the path is replaced by a link afterwards.
func TestOpenWorkspaceHoldsTheCheckedDirectory(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	r, err := OpenWorkspace(base, "5", "uploads")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.Rename(filepath.Join(base, "5", "uploads"), filepath.Join(base, "5", "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "5", "uploads")); err != nil {
		t.Fatal(err)
	}
	f, err := r.OpenFile("f.png", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(filepath.Join(base, "5", "moved", "f.png")); err != nil {
		t.Errorf("the file is not in the directory that was checked: %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("a file was created through the replacing link: %v", ents)
	}
}

// The isolation chooses whether the tool user gets the directory.
func TestIsolationEnsureWorkspaceFollowsTheMode(t *testing.T) {
	needRoot(t)
	off := &Isolation{}
	on := &Isolation{on: true}
	b1, b2 := t.TempDir(), t.TempDir()
	d1, err := off.EnsureWorkspace(b1, "a")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := on.EnsureWorkspace(b2, "a")
	if err != nil {
		t.Fatal(err)
	}
	if u := uidOf(t, d1); u != 0 {
		t.Errorf("without the drop chain the directory belongs to %d", u)
	}
	if u := uidOf(t, d2); u != ToolUID {
		t.Errorf("with the drop chain the directory belongs to %d", u)
	}
}

// Server directories: the server's, with the mode asked for, never through a link.
func TestOpenServerDirMakesRootOwnedDirectoriesAndRefusesLinks(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	r, err := OpenServerDir(base, 0o755, "spill", "scripts")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	for _, d := range []string{filepath.Join(base, "spill"), filepath.Join(base, "spill", "scripts")} {
		if m := mode(t, d); m != 0o755 {
			t.Errorf("%s mode %v", d, m)
		}
		if os.Geteuid() == 0 {
			if u, g := owner(t, d); u != 0 || g != 0 {
				t.Errorf("%s owner %d:%d", d, u, g)
			}
		}
	}
	// a directory the tool user owns becomes the server's again
	if os.Geteuid() == 0 {
		if err := os.Lchown(filepath.Join(base, "spill", "scripts"), ToolUID, ToolGID); err != nil {
			t.Fatal(err)
		}
		r, err := OpenServerDir(base, 0o711, "spill", "scripts")
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		if u, _ := owner(t, filepath.Join(base, "spill", "scripts")); u != 0 {
			t.Errorf("owner %d", u)
		}
	}
	if err := os.Symlink(outside, filepath.Join(base, "l")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(base, "f"), "x", 0o644)
	for _, rel := range [][]string{{"l"}, {"l", "x"}, {"f"}, {".."}, {"a/b"}, nil} {
		if r, err := OpenServerDir(base, 0o755, rel...); err == nil {
			r.Close()
			t.Errorf("%v accepted", rel)
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("created through a link: %v", ents)
	}
}

// OpenDirNoFollow changes nothing and refuses a link or a file in any component.
func TestOpenDirNoFollowOpensOnlyPlainDirectories(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(base, "a", "b", "f"), "x", 0o644)
	if err := os.Chmod(filepath.Join(base, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDirNoFollow(base, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if m := mode(t, filepath.Join(base, "a")); m != 0o755 {
		t.Errorf("the mode was changed to %v", m)
	}
	if err := os.Symlink(outside, filepath.Join(base, "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(base, "inside")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range [][]string{{"l"}, {"inside"}, {"a", "b", "f"}, {"missing"}, {".."}, {"a/b"}} {
		if r, err := OpenDirNoFollow(base, rel...); err == nil {
			r.Close()
			t.Errorf("%v accepted", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "missing")); err == nil {
		t.Error("a directory was made")
	}
}

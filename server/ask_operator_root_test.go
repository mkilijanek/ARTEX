//go:build unix

package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mkilijanek/artex/sandbox"
)

type askTree struct {
	data, task, uploads, outside string
}

func newAskTree(t *testing.T) askTree {
	t.Helper()
	a := askTree{data: t.TempDir(), outside: t.TempDir()}
	a.task = filepath.Join(a.data, "tasks", "7")
	a.uploads = filepath.Join(a.task, "uploads")
	for _, d := range []string{filepath.Join(a.task, "i3"), filepath.Join(a.data, "tasks", "8")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (a askTree) write(t *testing.T, p, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (a askTree) uploadNames(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(a.uploads)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// AC29(a): the source is opened relative to the task's working directory without following a
// link; a link, a path outside, and a link component that leaves the directory are refused and
// nothing is copied.
func TestAskOperatorRefusesASourceOutsideTheWorkspaceOrALink(t *testing.T) {
	a := newAskTree(t)
	secret := a.write(t, filepath.Join(a.outside, "secret.txt"), "TOP-SECRET")
	a.write(t, filepath.Join(a.data, "tasks", "8", "theirs.png"), "OTHER-TASK-IMAGE")
	a.write(t, filepath.Join(a.task, "ok.png"), "fine")
	if err := os.Symlink(secret, filepath.Join(a.task, "leak.png")); err != nil { // a link named like an image
		t.Fatal(err)
	}
	if err := os.Symlink("ok.png", filepath.Join(a.task, "alias.png")); err != nil { // a link to a file inside
		t.Fatal(err)
	}
	if err := os.Symlink(a.outside, filepath.Join(a.task, "dirlink")); err != nil {
		t.Fatal(err)
	}
	a.write(t, filepath.Join(a.outside, "pic.png"), "OUTSIDE-IMAGE")
	if err := os.Symlink(filepath.Join(a.data, "tasks", "8"), filepath.Join(a.task, "to8")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"link to a secret":                filepath.Join(a.task, "leak.png"),
		"link to a file inside":           filepath.Join(a.task, "alias.png"),
		"absolute path outside":           filepath.Join(a.outside, "pic.png"),
		"another task's file":             filepath.Join(a.data, "tasks", "8", "theirs.png"),
		"dotdot out of the task":          filepath.Join(a.task, "..", "8", "theirs.png"),
		"relative dotdot":                 "../../8/theirs.png",
		"through a directory link":        filepath.Join(a.task, "dirlink", "pic.png"),
		"through a link to another task":  filepath.Join(a.task, "to8", "theirs.png"),
		"relative through a link":         "dirlink/pic.png",
		"the task directory":              a.task,
		"a directory named like an image": filepath.Join(a.task, "dir.png"),
	}
	if err := os.MkdirAll(filepath.Join(a.task, "dir.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range cases {
		att, err := stageAskOperatorImage(a.data, "7", "i3", src)
		if err == nil || att != nil {
			t.Errorf("%s: accepted (%v)", name, att)
		}
	}
	for _, n := range a.uploadNames(t) {
		b, _ := os.ReadFile(filepath.Join(a.uploads, n))
		t.Errorf("a file was copied: %s = %q", n, b)
	}
}

func TestAskOperatorCopiesAnImageOfTheWorkspace(t *testing.T) {
	a := newAskTree(t)
	a.write(t, filepath.Join(a.task, "i3", "shot.png"), "PNGDATA")
	a.write(t, filepath.Join(a.task, "top.jpg"), "JPGDATA")
	for src, want := range map[string]string{
		"shot.png":                                   "PNGDATA", // relative to the intent's directory, as the worker's tools see it
		filepath.Join(a.task, "i3", "shot.png"):      "PNGDATA",
		filepath.Join(a.task, "top.jpg"):             "JPGDATA",
		filepath.Join(a.task, "i3", "..", "top.jpg"): "JPGDATA",
	} {
		att, err := stageAskOperatorImage(a.data, "7", "i3", src)
		if err != nil || len(att) != 1 {
			t.Fatalf("%s: %v %v", src, att, err)
		}
		b, err := os.ReadFile(filepath.Join(a.uploads, att[0].Name))
		if err != nil || string(b) != want {
			t.Errorf("%s: copy %q %v", src, b, err)
		}
		if att[0].Path != "uploads/"+att[0].Name || att[0].Size != int64(len(want)) {
			t.Errorf("%s: descriptor %+v", src, att[0])
		}
	}
}

var askName = regexp.MustCompile(`^ask-operator-\d+-[0-9a-f]{16}\.png$`)

// AC29(b): the write side. With a link planted at every name the next seconds would produce, a
// call writes a new, randomly suffixed file and the link targets are unchanged. The old code
// created the destination with O_TRUNC through the link.
func TestAskOperatorDoesNotWriteThroughPlantedLinks(t *testing.T) {
	a := newAskTree(t)
	a.write(t, filepath.Join(a.task, "shot.png"), "ATTACKER-CHOSEN-IMAGE-BYTES")
	victims := map[string]string{}
	if err := os.MkdirAll(a.uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := int64(-1); i < 12; i++ {
		v := a.write(t, filepath.Join(a.outside, fmt.Sprintf("victim%d", i+1)), "ORIGINAL")
		victims[v] = "ORIGINAL"
		for _, name := range []string{fmt.Sprintf("ask-operator-%d.png", now+i), fmt.Sprintf("ask-operator-%d-0000000000000000.png", now+i)} {
			if err := os.Symlink(v, filepath.Join(a.uploads, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// a dangling link too: creating through it would create the target
	dangling := filepath.Join(a.outside, "never-created")
	if err := os.Symlink(dangling, filepath.Join(a.uploads, fmt.Sprintf("ask-operator-%d.png", now+13))); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		att, err := stageAskOperatorImage(a.data, "7", "", filepath.Join(a.task, "shot.png"))
		if err != nil || len(att) != 1 {
			t.Fatalf("call %d: %v %v", i, att, err)
		}
		if !askName.MatchString(att[0].Name) {
			t.Errorf("name %q has no random suffix", att[0].Name)
		}
		if seen[att[0].Name] {
			t.Errorf("name %q used twice", att[0].Name)
		}
		seen[att[0].Name] = true
		fi, err := os.Lstat(filepath.Join(a.uploads, att[0].Name))
		if err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("the copy is not a regular file: %v %v", fi, err)
		}
	}
	for v, want := range victims {
		if b, err := os.ReadFile(v); err != nil || string(b) != want {
			t.Errorf("%s was written through a planted link: %q %v", v, b, err)
		}
	}
	if _, err := os.Lstat(dangling); err == nil {
		t.Error("a file was created through a dangling link")
	}
}

// The uploads directory itself can be a planted link; nothing is written through it.
func TestAskOperatorRefusesALinkedUploadsDirectory(t *testing.T) {
	a := newAskTree(t)
	a.write(t, filepath.Join(a.task, "shot.png"), "IMG")
	if err := os.Symlink(a.outside, a.uploads); err != nil {
		t.Fatal(err)
	}
	if _, err := stageAskOperatorImage(a.data, "7", "", filepath.Join(a.task, "shot.png")); err == nil {
		t.Fatal("accepted")
	}
	if ents, _ := os.ReadDir(a.outside); len(ents) != 0 {
		t.Errorf("a file was written through the linked directory: %v", ents)
	}
	// and a task directory that is a link
	if err := os.RemoveAll(filepath.Join(a.data, "tasks", "9")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.outside, filepath.Join(a.data, "tasks", "9")); err != nil {
		t.Fatal(err)
	}
	a.write(t, filepath.Join(a.outside, "x.png"), "IMG")
	if _, err := stageAskOperatorImage(a.data, "9", "", filepath.Join(a.data, "tasks", "9", "x.png")); err == nil {
		t.Fatal("a task directory that is a link was accepted")
	}
}

func TestAskOperatorChecksTheOpenFile(t *testing.T) {
	a := newAskTree(t)
	// larger than the limit, found on the open descriptor
	big := filepath.Join(a.task, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(askOperatorMaxImage + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := stageAskOperatorImage(a.data, "7", "", big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("an oversize image: %v", err)
	}
	// a second name for a file that is not the workspace's
	secret := a.write(t, filepath.Join(a.outside, "secret"), "S")
	if err := os.Link(secret, filepath.Join(a.task, "hard.png")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	if _, err := stageAskOperatorImage(a.data, "7", "", filepath.Join(a.task, "hard.png")); err == nil {
		t.Error("a file with a second name was copied")
	}
	// a FIFO does not block the server
	fifo := filepath.Join(a.task, "pipe.png")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := stageAskOperatorImage(a.data, "7", "", fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was copied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
	for _, n := range a.uploadNames(t) {
		t.Errorf("a file was copied: %s", n)
	}
	if b, _ := os.ReadFile(secret); !bytes.Equal(b, []byte("S")) {
		t.Error("the secret changed")
	}
}

// With isolation on the copy belongs to the tool user and so does the uploads directory, so the
// tools can use what the operator is shown.
func TestAskOperatorHandsTheCopyToTheToolUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	activateEnforcedTree(t)
	a := newAskTree(t)
	a.write(t, filepath.Join(a.task, "shot.png"), "IMG")
	att, err := stageAskOperatorImage(a.data, "7", "", filepath.Join(a.task, "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{a.uploads, filepath.Join(a.uploads, att[0].Name)} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != sandbox.ToolUID {
			t.Errorf("%s owner %d", p, st.Uid)
		}
	}
}

// A screenshot the browser MCP server saved in its own working directory (the one the spawn
// function made under the tool home) can be attached, with the same confinement as a file of the
// task: relative to that directory through os.Root, no link, one name, a size cap. Nothing else
// outside the task directory is accepted.
func TestAskOperatorAcceptsAScreenshotInTheBrowserMCPDirectory(t *testing.T) {
	_, l := activateEnforcedTree(t)
	a := newAskTree(t)
	browser := filepath.Join(l.ToolHome, "mcp", "browser")
	secret := a.write(t, filepath.Join(a.outside, "secret.txt"), "TOP-SECRET")
	a.write(t, filepath.Join(browser, "shot.png"), "SHOT")
	a.write(t, filepath.Join(browser, ".playwright-mcp", "page-1.png"), "PAGE")
	a.write(t, filepath.Join(l.ToolHome, "mcp", "other", "x.png"), "OTHER-SERVER")
	a.write(t, filepath.Join(l.ToolHome, "home.png"), "HOME")
	a.write(t, filepath.Join(a.outside, "pic.png"), "OUTSIDE")
	if err := os.Symlink(secret, filepath.Join(browser, "leak.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.outside, filepath.Join(browser, "dl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(browser, "hard.png")); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	big := filepath.Join(browser, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(askOperatorMaxImage + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fifo := filepath.Join(browser, "pipe.png")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}

	for src, want := range map[string]string{
		filepath.Join(browser, "shot.png"):                          "SHOT",
		filepath.Join(browser, ".playwright-mcp", "page-1.png"):     "PAGE",
		filepath.Join(browser, ".playwright-mcp", "..", "shot.png"): "SHOT",
	} {
		att, err := stageAskOperatorImage(a.data, "7", "i3", src)
		if err != nil || len(att) != 1 {
			t.Fatalf("%s: %v %v", src, att, err)
		}
		if b, err := os.ReadFile(filepath.Join(a.uploads, att[0].Name)); err != nil || string(b) != want {
			t.Errorf("%s: copy %q %v", src, b, err)
		}
	}
	n := len(a.uploadNames(t))
	for name, src := range map[string]string{
		"link to a secret":            filepath.Join(browser, "leak.png"),
		"directory link":              filepath.Join(browser, "dl", "pic.png"),
		"hard link":                   filepath.Join(browser, "hard.png"),
		"oversize":                    big,
		"FIFO":                        fifo,
		"another MCP server's dir":    filepath.Join(l.ToolHome, "mcp", "other", "x.png"),
		"the tool home":               filepath.Join(l.ToolHome, "home.png"),
		"dotdot out of the browser":   filepath.Join(browser, "..", "other", "x.png"),
		"outside everything":          filepath.Join(a.outside, "pic.png"),
		"relative path is the task's": "../../../../x.png",
	} {
		if att, err := stageAskOperatorImage(a.data, "7", "i3", src); err == nil {
			t.Errorf("%s: accepted %v", name, att)
		}
	}
	if got := len(a.uploadNames(t)); got != n {
		t.Errorf("a refused source was copied: %d files, want %d", got, n)
	}

	// the browser directory (or the directory above it) replaced by a link: nothing is read through it
	if err := os.Rename(browser, browser+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a.outside, browser); err != nil {
		t.Fatal(err)
	}
	if _, err := stageAskOperatorImage(a.data, "7", "i3", filepath.Join(browser, "pic.png")); err == nil {
		t.Error("a browser directory that is a link was accepted")
	}
	if err := os.Remove(browser); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(l.ToolHome, "mcp")
	if err := os.Rename(mcp, mcp+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(a.outside), mcp); err != nil {
		t.Fatal(err)
	}
	if _, err := stageAskOperatorImage(a.data, "7", "i3", filepath.Join(browser, filepath.Base(a.outside), "pic.png")); err == nil {
		t.Error("an mcp directory that is a link was accepted")
	}
}

// Without the drop chain there is no such directory to accept.
func TestAskOperatorRefusesTheBrowserDirectoryWithoutIsolation(t *testing.T) {
	sandbox.Set(nil)
	a := newAskTree(t)
	src := filepath.Join(a.outside, "mcp", "browser", "shot.png")
	a.write(t, src, "x")
	if _, err := stageAskOperatorImage(a.data, "7", "", src); err == nil {
		t.Fatal("accepted")
	}
}

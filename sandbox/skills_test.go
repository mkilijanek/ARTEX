//go:build !windows

package sandbox

import (
	"bytes"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const secretMarker = "TOPSECRET-ROOT-ONLY-BYTES"

func skillMD(name string) string {
	return "---\nname: " + name + "\ndescription: d " + name + "\n---\nbody of " + name + "\n"
}

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// useTempSnapshotRoot points the snapshots at a temporary directory and captures the log.
func useTempSnapshotRoot(t *testing.T) (root string, logs *bytes.Buffer) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "snap")
	old := SkillSnapshotRoot
	SkillSnapshotRoot = root
	logs = &bytes.Buffer{}
	log.SetOutput(logs)
	t.Cleanup(func() {
		SkillSnapshotRoot = old
		log.SetOutput(os.Stderr)
		skillsWalkHook = nil
	})
	return root, logs
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// noSecretUnder fails when any file below dir holds the secret marker.
func noSecretUnder(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(secretMarker)) {
			t.Errorf("secret bytes found in snapshot file %s", p)
		}
		return nil
	})
}

func TestLoadSkillsCopiesRegularTreeWithModes(t *testing.T) {
	root, _ := useTempSnapshotRoot(t)
	src := t.TempDir()
	write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o600)
	write(t, filepath.Join(src, "a", "scripts", "run.sh"), "#!/bin/sh\n", 0o750)
	write(t, filepath.Join(src, "a", "ref.txt"), "ref", 0o600)

	reg, err := LoadSkills(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 1 || reg.List()[0].Name != "a" {
		t.Fatalf("skills = %+v", reg.List())
	}
	dir := reg.List()[0].Dir
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) || filepath.Base(dir) != "a" {
		t.Fatalf("skill dir %q not inside the snapshot root with its own name", dir)
	}
	for p, want := range map[string]os.FileMode{"SKILL.md": 0o644, "ref.txt": 0o644, "scripts/run.sh": 0o755, "scripts": 0o755} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", p, fi.Mode().Perm(), want)
		}
	}
}

func TestLoadSkillsRefusesLinks(t *testing.T) {
	// AC45. The skills directory is world-writable, as the harness makes it in the degraded case.
	_, logs := useTempSnapshotRoot(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "jwt.key")
	write(t, secret, secretMarker, 0o600)
	otherDir := filepath.Join(outside, "other")
	write(t, filepath.Join(otherDir, "leak.txt"), secretMarker, 0o644)

	src := t.TempDir()
	if err := os.Chmod(src, 0o777); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "good", "SKILL.md"), skillMD("good"), 0o644)
	if err := os.Symlink(otherDir, filepath.Join(src, "good", "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "x", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(src, "bare.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "good"), filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}

	reg, err := LoadSkills(src)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range reg.List() {
		got[s.Name] = true
	}
	if got["x"] || got["bare"] || got["alias"] || !got["good"] || len(got) != 1 {
		t.Fatalf("loaded = %v, want only good", got)
	}
	for _, s := range reg.List() {
		if strings.Contains(s.Instructions, secretMarker) {
			t.Errorf("skill %s text carries the secret", s.Name)
		}
		if _, err := os.Lstat(filepath.Join(s.Dir, "linkdir")); err == nil {
			t.Errorf("directory link was copied into %s", s.Dir)
		}
	}
	noSecretUnder(t, SkillSnapshotRoot)
	for _, rel := range []string{"x/SKILL.md", "bare.md", "alias", "good/linkdir"} {
		want := "[skills] refused non-regular entry " + rel
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

func TestLoadSkillsLinkSwappedDuringWalk(t *testing.T) {
	// The target is outside the tree (os.Root alone refuses it) or inside it (only O_NOFOLLOW
	// and the fstat check refuse it).
	for _, inside := range []bool{false, true} {
		name := "outside"
		if inside {
			name = "inside"
		}
		t.Run(name, func(t *testing.T) {
			_, logs := useTempSnapshotRoot(t)
			outside := t.TempDir()
			secret := filepath.Join(outside, "jwt.key")
			src := t.TempDir()
			write(t, filepath.Join(src, "x", "SKILL.md"), skillMD("x"), 0o644)
			write(t, filepath.Join(src, "y", "SKILL.md"), skillMD("y"), 0o644)
			if inside {
				secret = filepath.Join(src, "zz-private", "secret.md")
			}
			write(t, secret, "---\nname: stolen\n---\n"+secretMarker, 0o600)

			skillsWalkHook = func(rel string) {
				if rel != filepath.Join("x", "SKILL.md") {
					return
				}
				p := filepath.Join(src, rel)
				if err := os.Remove(p); err != nil {
					t.Error(err)
				}
				if err := os.Symlink(secret, p); err != nil {
					t.Error(err)
				}
			}
			reg, err := LoadSkills(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range reg.List() {
				if s.Name == "x" || s.Name == "stolen" || strings.Contains(s.Instructions, secretMarker) {
					t.Fatalf("swapped link was loaded: %+v", s)
				}
			}
			if len(reg.List()) != 1 {
				t.Fatalf("want only y, got %+v", reg.List())
			}
			if !inside { // an inside target is a regular file of the tree and is copied as such
				noSecretUnder(t, SkillSnapshotRoot)
			}
			if !strings.Contains(logs.String(), "[skills] refused non-regular entry x/SKILL.md") {
				t.Errorf("no refusal logged:\n%s", logs.String())
			}
		})
	}
}

func TestLoadSkillsDirectorySwappedDuringWalk(t *testing.T) {
	// "b" is swapped after it was classified as a directory and before it is opened: for a link
	// to another directory of the tree, and for a different real directory. Neither is copied
	// under the name "b".
	for _, mode := range []string{"link", "directory"} {
		t.Run(mode, func(t *testing.T) {
			_, _ = useTempSnapshotRoot(t)
			src := t.TempDir()
			write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o644)
			write(t, filepath.Join(src, "b", "SKILL.md"), skillMD("b"), 0o644)
			write(t, filepath.Join(src, ".new", "SKILL.md"), skillMD("swapped"), 0o644)
			skillsWalkHook = func(rel string) {
				if rel != "b" {
					return
				}
				p := filepath.Join(src, "b")
				if err := os.Rename(p, p+".old"); err != nil {
					t.Error(err)
				}
				if mode == "link" {
					if err := os.Symlink(filepath.Join(src, "a"), p); err != nil {
						t.Error(err)
					}
				} else if err := os.Rename(filepath.Join(src, ".new"), p); err != nil {
					t.Error(err)
				}
			}
			reg, err := LoadSkills(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range reg.List() {
				if filepath.Base(s.Dir) == "b" {
					t.Fatalf("a directory swapped during the walk was copied: %+v", s)
				}
			}
		})
	}
}

func TestLoadSkillsFIFODoesNotBlock(t *testing.T) {
	_, logs := useTempSnapshotRoot(t)
	src := t.TempDir()
	write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o644)
	if err := syscall.Mkfifo(filepath.Join(src, "a", "pipe"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "p.md"), 0o666); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg, err := LoadSkills(src)
		if err != nil || len(reg.List()) != 1 {
			t.Errorf("reg=%v err=%v", reg, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("load blocked on a FIFO")
	}
	if !strings.Contains(logs.String(), "[skills] refused non-regular entry a/pipe") ||
		!strings.Contains(logs.String(), "[skills] refused non-regular entry p.md") {
		t.Errorf("FIFOs not reported:\n%s", logs.String())
	}
}

func TestLoadSkillsFIFOSwappedDuringWalkDoesNotBlock(t *testing.T) {
	_, _ = useTempSnapshotRoot(t)
	src := t.TempDir()
	write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o644)
	skillsWalkHook = func(rel string) {
		if rel != filepath.Join("a", "SKILL.md") {
			return
		}
		p := filepath.Join(src, rel)
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o666); err != nil {
			t.Error(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg, err := LoadSkills(src)
		if err != nil || len(reg.List()) != 0 {
			t.Errorf("reg=%v err=%v", reg, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("load blocked on a swapped-in FIFO")
	}
}

func TestLoadSkillsGenerations(t *testing.T) {
	root, _ := useTempSnapshotRoot(t)
	src := t.TempDir()
	write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o644)

	r1, err := LoadSkills(src)
	if err != nil {
		t.Fatal(err)
	}
	r1b, err := LoadSkills(src)
	if err != nil {
		t.Fatal(err)
	}
	if r1.List()[0].Dir != r1b.List()[0].Dir || len(names(t, root)) != 1 {
		t.Fatalf("unchanged source must reuse the snapshot: %v", names(t, root))
	}

	var dirs []string
	dirs = append(dirs, r1.List()[0].Dir)
	for _, body := range []string{"two", "three", "four"} {
		write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a")+body, 0o644)
		r, err := LoadSkills(src)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.List()[0].Instructions, body) {
			t.Fatalf("load after change does not see %q", body)
		}
		dirs = append(dirs, r.List()[0].Dir)
	}
	if n := len(names(t, root)); n != 2 {
		t.Fatalf("generations on disk = %v, want 2", names(t, root))
	}
	for i, d := range dirs {
		_, err := os.Stat(d)
		if kept := i >= len(dirs)-2; kept != (err == nil) {
			t.Errorf("generation %d (%s): exists=%v, want %v", i, d, err == nil, kept)
		}
	}

	// A new skill and a deleted one show up, a stale stage of a crash is removed.
	if err := os.RemoveAll(filepath.Join(src, "a")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "b", "SKILL.md"), skillMD("b"), 0o644)
	if err := os.Mkdir(filepath.Join(root, skillStagePrefix+"crashed"), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := LoadSkills(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 1 || r.List()[0].Name != "b" {
		t.Fatalf("skills = %+v", r.List())
	}
	for _, n := range names(t, root) {
		if strings.HasPrefix(n, skillStagePrefix) {
			t.Errorf("stale stage %s left", n)
		}
	}
}

func TestLoadSkillsMissingDirectoryIsAnError(t *testing.T) {
	useTempSnapshotRoot(t)
	if _, err := LoadSkills(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want an error for a missing skills directory")
	}
}

func TestSkillSnapshotLocation(t *testing.T) {
	old := skillsEuid
	t.Cleanup(func() { skillsEuid = old })

	skillsEuid = func() int { return 0 }
	got, err := SkillSnapshotLocation("/data")
	if err != nil || got != skillImageRoot {
		t.Fatalf("root: %q %v, want the image path", got, err)
	}
	skillsEuid = func() int { return 1000 }
	got, err = SkillSnapshotLocation("/data")
	if err != nil || got != "/data/.skills-snapshot" {
		t.Fatalf("non-root: %q %v", got, err)
	}
	if _, err := SkillSnapshotLocation(""); err == nil {
		t.Fatal("non-root without a data directory must fail")
	}
}

func TestConfigureSkillSnapshotCreatesDirectories(t *testing.T) {
	_, _ = useTempSnapshotRoot(t)
	oldE, oldI := skillsEuid, skillImageRoot
	t.Cleanup(func() { skillsEuid, skillImageRoot = oldE, oldI })

	// Image layout: the server creates the missing directory, root 0755.
	skillImageRoot = filepath.Join(t.TempDir(), "var", "lib", "artex", "skills")
	skillsEuid = func() int { return 0 }
	if err := ConfigureSkillSnapshot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(skillImageRoot)
	if err != nil || fi.Mode().Perm() != 0o755 || SkillSnapshotRoot != skillImageRoot {
		t.Fatalf("image: %v %v root=%q", fi, err, SkillSnapshotRoot)
	}

	// Binary install: <data>/.skills-snapshot, 0700.
	data := t.TempDir()
	skillsEuid = func() int { return 1000 }
	if err := ConfigureSkillSnapshot(data); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(data, ".skills-snapshot")
	fi, err = os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o700 || SkillSnapshotRoot != want {
		t.Fatalf("non-root: %v %v root=%q", fi, err, SkillSnapshotRoot)
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o644)
	reg, err := LoadSkills(src)
	if err != nil || len(reg.List()) != 1 || !strings.HasPrefix(reg.List()[0].Dir, want) {
		t.Fatalf("load below the data directory: %v %v", reg, err)
	}
}

func TestConfigureSkillSnapshotUnwritableLoadsNothing(t *testing.T) {
	_, _ = useTempSnapshotRoot(t)
	oldE := skillsEuid
	t.Cleanup(func() { skillsEuid = oldE })
	skillsEuid = func() int { return 1000 }

	// The data directory is a regular file, so the snapshot directory cannot exist (this
	// holds for root too, unlike a mode bit).
	data := filepath.Join(t.TempDir(), "notadir")
	write(t, data, "x", 0o600)
	err := ConfigureSkillSnapshot(data)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(data, ".skills-snapshot")) {
		t.Fatalf("want an error naming the path, got %v", err)
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "a", "SKILL.md"), skillMD("a"), 0o644)
	reg, lerr := LoadSkills(src)
	if lerr == nil || reg != nil || !strings.Contains(lerr.Error(), data) {
		t.Fatalf("skills must not load unsafely: reg=%v err=%v", reg, lerr)
	}
	if _, err := SkillSnapshotLocation(""); err == nil {
		t.Fatal("no data directory must be an error")
	}
	if err := ConfigureSkillSnapshot(""); err == nil {
		t.Fatal("ConfigureSkillSnapshot without a data directory must fail")
	}
}

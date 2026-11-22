package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// PermsInput names the trees the permission step covers.
type PermsInput struct {
	Layout  Layout
	AppDir  string // /app: holds the binary, start.sh
	KeyDir  string // directory with jwt.key
	DataDir string
	// Drop is true when tool processes go through the drop chain. The run directories (variables
	// files, the server's TMPDIR) exist for the chain only, so they are touched only then.
	Drop bool
}

// PermsReport is what the step could not do and what it found.
type PermsReport struct {
	// Symlinks are the top-level entries of the data directory that are symbolic links. They
	// are not followed; the self-check probes whether they can be read.
	Symlinks []string
	// Problems are entries the step refused or could not fix. Each is also a reason to look at
	// C3.
	Problems []string
}

// Top-level entries of the data directory that tools may traverse. Everything else is closed.
var (
	dataOpenDirs   = []string{"spill", "drafts", "noa"} // root 0711
	dataWorkspaces = []string{"tasks", "sessions"}      // root 0711, children owned by the tool user
	// dataClosedDirs exist from the first start, root 0700, so the server's later MkdirAll finds
	// them already closed instead of creating them with the default mode.
	dataClosedDirs = []string{"traffic", "transcripts", "evidence", "archives"}
)

// ApplyPermissions applies the permission table at every start. Every change goes through a
// descriptor opened without following a final link, so a link planted before an upgrade is
// reported and never followed. Ownership changes need root; without it only modes are set.
func ApplyPermissions(in PermsInput) (PermsReport, error) {
	var rep PermsReport
	canChown := os.Geteuid() == 0
	problem := func(format string, a ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, a...)) }

	setRoot := func(path string, want os.FileMode, create bool) {
		if create {
			if err := os.MkdirAll(path, 0o700); err != nil {
				problem("%s: %v", path, err)
				return
			}
		}
		if err := secure(func(fl int) (*os.File, error) { return os.OpenFile(path, fl, 0) }, want, 0, 0, canChown); err != nil {
			if errors.Is(err, os.ErrNotExist) && !create {
				return // optional file (no key yet, no start.sh in a source install)
			}
			problem("%s: %v", path, err)
		}
	}

	// outside the data directory
	setRoot(in.AppDir, 0o755, false)
	setRoot(in.Layout.Artex, 0o755, false)
	setRoot(filepath.Join(in.AppDir, "start.sh"), 0o755, false)
	setRoot(filepath.Join(in.KeyDir, "jwt.key"), 0o600, false)
	if in.Drop {
		setRoot(in.Layout.PrivateDir, 0o700, true)
		setRoot(in.Layout.VarsDir, 0o700, true)
		setRoot(in.Layout.ServerTmp, 0o711, true)
	}

	// the data directory
	setRoot(in.DataDir, 0o711, true)
	root, err := os.OpenRoot(in.DataDir)
	if err != nil {
		return rep, fmt.Errorf("open the data directory: %w", err)
	}
	defer root.Close()
	rootEntry := func(name string, want os.FileMode, create bool) {
		if create {
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				problem("%s: %v", filepath.Join(in.DataDir, name), err)
				return
			}
		}
		err := secure(func(fl int) (*os.File, error) { return root.OpenFile(name, fl, 0) }, want, 0, 0, canChown)
		if err != nil {
			problem("%s: %v", filepath.Join(in.DataDir, name), err)
		}
	}
	for _, n := range dataOpenDirs {
		rootEntry(n, 0o711, true)
	}
	rootEntry("public", 0o755, true)
	for _, n := range dataWorkspaces {
		rootEntry(n, 0o711, true)
		handWorkspaces(root, n, canChown, problem)
	}

	for _, n := range dataClosedDirs {
		rootEntry(n, 0o700, true)
	}

	// everything else is closed
	open := map[string]bool{"public": true}
	for _, n := range append(append([]string(nil), dataOpenDirs...), dataWorkspaces...) {
		open[n] = true
	}
	top, err := root.Open(".")
	if err != nil {
		return rep, fmt.Errorf("list the data directory: %w", err)
	}
	ents, err := top.ReadDir(-1)
	top.Close()
	if err != nil {
		return rep, fmt.Errorf("list the data directory: %w", err)
	}
	for _, e := range ents {
		name := e.Name()
		if open[name] {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			rep.Symlinks = append(rep.Symlinks, filepath.Join(in.DataDir, name))
			continue
		}
		want := os.FileMode(0o600)
		if e.IsDir() {
			want = 0o700
		}
		rootEntry(name, want, false)
	}
	return rep, nil
}

// handWorkspaces gives each top-level directory of tasks/ or sessions/ to the tool user at
// 0700. What is inside is the migration's business.
func handWorkspaces(root *os.Root, name string, canChown bool, problem func(string, ...any)) {
	sub, err := root.OpenRoot(name)
	if err != nil {
		problem("%s: %v", name, err)
		return
	}
	defer sub.Close()
	d, err := sub.Open(".")
	if err != nil {
		problem("%s: %v", name, err)
		return
	}
	ents, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		problem("%s: %v", name, err)
		return
	}
	for _, e := range ents {
		child := e.Name()
		if !e.IsDir() {
			if e.Type()&os.ModeSymlink != 0 {
				problem("%s/%s: a link where a workspace should be", name, child)
			}
			continue
		}
		err := secure(func(fl int) (*os.File, error) { return sub.OpenFile(child, fl, 0) }, 0o700, ToolUID, ToolGID, canChown)
		if err != nil {
			problem("%s/%s: %v", name, child, err)
		}
	}
}

// secure opens the entry without following a final link and sets mode and owner on the open
// descriptor. The entry must be a regular file or a directory.
func secure(open func(flags int) (*os.File, error), want os.FileMode, uid, gid int, chown bool) error {
	f, err := open(os.O_RDONLY | oNoFollow | syscall.O_NONBLOCK)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return errors.New("is a symbolic link; not followed")
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		return errors.New("is neither a regular file nor a directory")
	}
	if chown {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	return f.Chmod(want)
}

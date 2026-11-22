package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/skill"
)

// SkillSnapshotRoot is where skill snapshots are written: one directory per generation,
// root 0755, files 0644 (0755 where the source was executable). ConfigureSkillSnapshot picks
// it at start; tests point it elsewhere.
var SkillSnapshotRoot = skillImageRoot

// skillImageRoot is the snapshot root of the shipped image, where the server runs as root.
// Binary installs and non-root servers use <data>/.skills-snapshot instead.
var skillImageRoot = "/var/lib/artex/skills"

// skillsEuid is the effective user id of the server; tests replace it.
var skillsEuid = os.Geteuid

// skillSnapshotDirName is the snapshot directory below the data directory (non-root servers).
const skillSnapshotDirName = ".skills-snapshot"

// SkillSnapshotLocation is the snapshot root for a server running as the current user:
// the image path as root, else <dataDir>/.skills-snapshot.
func SkillSnapshotLocation(dataDir string) (string, error) {
	if skillsEuid() == 0 {
		return skillImageRoot, nil
	}
	if dataDir == "" {
		return "", errors.New("skills: no data directory for the snapshot of a non-root server")
	}
	return filepath.Join(dataDir, skillSnapshotDirName), nil
}

// ConfigureSkillSnapshot chooses the snapshot root and creates it (root 0755 in the image,
// 0700 below the data directory), then checks that a snapshot can be written there. On
// failure it returns an error naming the path and the reason; skills are then not loaded
// (LoadSkills fails the same way), never loaded from the unverified tree.
func ConfigureSkillSnapshot(dataDir string) error {
	root, err := SkillSnapshotLocation(dataDir)
	if err != nil {
		return err
	}
	skillsMu.Lock()
	SkillSnapshotRoot = root
	skillsMu.Unlock()
	mode := os.FileMode(0o700)
	if skillsEuid() == 0 {
		mode = 0o755
	}
	if err := os.MkdirAll(root, mode); err != nil {
		return fmt.Errorf("skill snapshot directory %s cannot be created: %w", root, err)
	}
	probe, err := os.MkdirTemp(root, skillStagePrefix)
	if err != nil {
		return fmt.Errorf("skill snapshot directory %s is not writable: %w", root, err)
	}
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("skill snapshot directory %s: remove probe: %w", root, err)
	}
	return nil
}

// skillGenerationsKept is the number of generations that stay on disk: the current one and
// the previous one, which a running session may still read resources from.
const skillGenerationsKept = 2

const skillStagePrefix = ".stage-"

var (
	skillsMu sync.Mutex
	// skillsWalkHook runs after an entry was classified as a regular file or a directory and
	// before it is opened. Tests use it to swap the entry for a link.
	skillsWalkHook func(rel string)
)

// LoadSkills loads the skills in dir from a verified snapshot. skill.LoadDir follows symbolic
// links as root, and the skills directory can be writable by the tool user, so the tree is
// first copied through os.OpenRoot without following links: directories by descriptor, files
// opened with O_NOFOLLOW and checked with fstat. Every symbolic link, device, FIFO and socket
// is skipped and logged; a skill whose SKILL.md is skipped is not loaded. skill.LoadDir then
// reads only the server-written regular files of the snapshot.
//
// The returned skills' Dir points into the snapshot. A snapshot whose content equals the
// newest one is reused, so the paths stay stable while the skills do not change; older
// generations beyond the current and the previous one are removed after a successful load.
func LoadSkills(dir string) (*skill.Registry, error) {
	skillsMu.Lock()
	defer skillsMu.Unlock()

	src, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("skills: open %s: %w", dir, err)
	}
	defer src.Close()

	base := SkillSnapshotRoot
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("skills: snapshot directory %s cannot be created: %w", base, err)
	}
	removeStaleStages(base)
	stage, err := os.MkdirTemp(base, skillStagePrefix)
	if err != nil {
		return nil, fmt.Errorf("skills: snapshot directory %s is not writable: %w", base, err)
	}
	staged := false
	defer func() {
		if !staged {
			if rerr := os.RemoveAll(stage); rerr != nil {
				log.Printf("[skills] remove unused snapshot %s: %v", stage, rerr)
			}
		}
	}()

	h := sha256.New()
	if err := copyTree(src, "", stage, h); err != nil {
		return nil, fmt.Errorf("skills: snapshot %s: %w", dir, err)
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return nil, fmt.Errorf("skills: snapshot mode: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))[:16]

	gens, err := listGenerations(base)
	if err != nil {
		return nil, err
	}
	final := ""
	if n := len(gens); n > 0 && strings.HasSuffix(gens[n-1], "-"+sum) {
		final = filepath.Join(base, gens[n-1])
	} else {
		name := fmt.Sprintf("%020d-%s", time.Now().UnixNano(), sum)
		final = filepath.Join(base, name)
		if err := os.Rename(stage, final); err != nil {
			return nil, fmt.Errorf("skills: publish snapshot: %w", err)
		}
		staged = true
		gens = append(gens, name)
	}

	reg, err := skill.LoadDir(final)
	if err != nil {
		return nil, fmt.Errorf("skills: load snapshot: %w", err)
	}
	for len(gens) > skillGenerationsKept {
		old := filepath.Join(base, gens[0])
		if err := os.RemoveAll(old); err != nil {
			log.Printf("[skills] remove old snapshot %s: %v", old, err)
			break
		}
		gens = gens[1:]
	}
	return reg, nil
}

// listGenerations returns the generation directory names in base, oldest first. The names
// start with a zero-padded timestamp, so the lexical order is the age.
func listGenerations(base string) ([]string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("skills: list snapshots: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// removeStaleStages deletes half-written snapshots of a crashed earlier call. The caller
// holds skillsMu, so none of them belongs to a live call of this process.
func removeStaleStages(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), skillStagePrefix) {
			p := filepath.Join(base, e.Name())
			if err := os.RemoveAll(p); err != nil {
				log.Printf("[skills] remove stale snapshot %s: %v", p, err)
			}
		}
	}
}

// copyTree copies the regular files and directories under src into dst and feeds what it
// copied to h. rel is src's path relative to the skills directory, for messages.
func copyTree(src *os.Root, rel, dst string, h hash.Hash) error {
	d, err := src.Open(".")
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		name := e.Name()
		erel := filepath.Join(rel, name)
		fi, err := src.Lstat(name)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode().IsDir():
			if skillsWalkHook != nil {
				skillsWalkHook(erel)
			}
			child, err := src.OpenRoot(name)
			if err != nil {
				log.Printf("[skills] refused non-regular entry %s", erel)
				continue
			}
			cfi, err := child.Stat(".")
			if err != nil || !os.SameFile(fi, cfi) {
				child.Close()
				log.Printf("[skills] refused non-regular entry %s", erel)
				continue
			}
			out := filepath.Join(dst, name)
			if err := os.Mkdir(out, 0o755); err != nil {
				child.Close()
				return err
			}
			if err := os.Chmod(out, 0o755); err != nil {
				child.Close()
				return err
			}
			fmt.Fprintf(h, "d\x00%s\x00", erel)
			err = copyTree(child, erel, out, h)
			child.Close()
			if err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := copyFile(src, name, erel, filepath.Join(dst, name), h); err != nil {
				return err
			}
		default:
			log.Printf("[skills] refused non-regular entry %s", erel)
		}
	}
	return nil
}

// copyFile copies one regular file. The open does not follow a link and does not block on a
// FIFO; the descriptor is checked with fstat, so an entry swapped after the Lstat is refused.
func copyFile(src *os.Root, name, rel, dst string, h hash.Hash) error {
	if skillsWalkHook != nil {
		skillsWalkHook(rel)
	}
	f, err := src.OpenFile(name, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err != nil {
		if fi, lerr := src.Lstat(name); lerr == nil && !fi.Mode().IsRegular() {
			log.Printf("[skills] refused non-regular entry %s", rel)
			return nil
		}
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		log.Printf("[skills] refused non-regular entry %s", rel)
		return nil
	}
	mode := os.FileMode(0o644)
	if fi.Mode()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintf(h, "f\x00%s\x00%o\x00%d\x00", rel, mode, fi.Size())
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, fi.Size()))
	if err == nil && n != fi.Size() {
		err = errors.New("file shrank while copying")
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy %s: %w", rel, err)
	}
	return os.Chmod(dst, mode)
}

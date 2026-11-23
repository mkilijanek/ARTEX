package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/mkilijanek/artex/sandbox"
)

// Output files of a session: the spill files the harness writes when a tool's output is too
// large, and the files background tasks write. The server writes them, so they are the server's,
// mode 0600; the model reads them with Bash (grep, sed) or Read as the tool user, so they have to
// be readable. Two rules keep that from becoming a way to make the server write somewhere else:
//
//   - the directories they live in belong to the server and are not writable by the tool user (the
//     spill directory is made here, through os.Root; the background-task directory sits under the
//     server's temporary directory, which is mode 0711), so a tool cannot plant a link at a name the
//     server is about to create;
//   - the only change made afterwards is a chmod, by a PostToolUse hook, of a regular file inside
//     this session's own directories, found through os.Root with no link followed.

// sessionSpillDir returns <data>/spill/<kind>/<id>, making it (and what is missing above it)
// through os.Root. The spill root is 0711 and the two directories below it 0755, all the server's:
// nothing in them can be created, replaced or linked by the tool user. A link or a file where a
// directory belongs is an error.
func sessionSpillDir(dataDir, kind, id string) (string, error) {
	for _, c := range []string{kind, id} {
		if !validSpillName(c) {
			return "", fmt.Errorf("spill directory: %q is not a plain name", c)
		}
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("spill directory: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return "", fmt.Errorf("spill directory: %w", err)
	}
	for i, c := range []string{"spill", kind, id} {
		mode := os.FileMode(0o755)
		if i == 0 {
			mode = 0o711
		}
		next, err := ensureServerDir(root, c, mode)
		root.Close()
		if err != nil {
			return "", fmt.Errorf("spill directory %s: %w", c, err)
		}
		root = next
	}
	root.Close()
	return filepath.Join(abs, "spill", kind, id), nil
}

var spillNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func validSpillName(s string) bool { return spillNameRe.MatchString(s) && s != ".." }

// ensureServerDir makes name inside root a directory with the given mode, owned by the server's
// user, and returns a root for it. A link or a file in the way is an error.
func ensureServerDir(root *os.Root, name string, mode os.FileMode) (*os.Root, error) {
	if err := root.Mkdir(name, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("not a directory (a link or a file is in the way)")
	}
	if os.Geteuid() == 0 {
		if err := root.Lchown(name, 0, 0); err != nil {
			return nil, err
		}
	}
	if err := root.Chmod(name, mode); err != nil {
		return nil, err
	}
	return root.OpenRoot(name)
}

// checkSpillDir is the guard's check of a session's spill directory when isolation is on: it is
// <data>/spill/<kind>/<id> of this session, and neither it nor the two directories above it can be
// written by anyone but the server.
func checkSpillDir(dir string, info SessionInfo) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("spill directory %q is not absolute", dir)
	}
	kindDir := filepath.Dir(dir)
	spillDir := filepath.Dir(kindDir)
	if filepath.Base(dir) != info.ID || filepath.Base(kindDir) != info.Kind || filepath.Base(spillDir) != "spill" {
		return fmt.Errorf("spill directory %q is not <data>/spill/%s/%s", dir, info.Kind, info.ID)
	}
	for _, d := range []string{dir, kindDir, spillDir} {
		fi, err := os.Lstat(d)
		if err != nil {
			return fmt.Errorf("spill directory: %w", err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("spill directory: %s is not a directory", d)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("spill directory: %s can be written by others (mode %o)", d, fi.Mode().Perm())
		}
		if uid, ok := ownerUID(fi); ok && uid == uint32(sandbox.ToolUID) {
			return fmt.Errorf("spill directory: %s belongs to the tool user", d)
		}
	}
	return nil
}

// outputRoots are the places one session's output files may be. Only a regular file inside one of
// them is ever changed.
type outputRoots struct {
	spill string // <data>/spill/<kind>/<id>
	noa   string // <data>/noa/<noa session>
	// sessionID names the SDK's task directory under the temporary directory. It is set after the
	// session exists, because an empty SessionID is replaced by a random one.
	mu        sync.Mutex
	sessionID string
	logged    bool
}

func (r *outputRoots) setSessionID(id string) {
	r.mu.Lock()
	r.sessionID = id
	r.mu.Unlock()
}

// dirs lists the session's directories, those that are known.
func (r *outputRoots) dirs() []string {
	r.mu.Lock()
	id := r.sessionID
	r.mu.Unlock()
	var out []string
	if r.spill != "" {
		out = append(out, r.spill)
	}
	if r.noa != "" {
		out = append(out, r.noa)
	}
	if id != "" && validSpillName(id) {
		// the SDK's background-task directory: os.TempDir()/norma/<session>/tasks
		out = append(out, filepath.Join(os.TempDir(), "norma", id, "tasks"))
	}
	return out
}

func (r *outputRoots) logOnce(info SessionInfo, path string) {
	r.mu.Lock()
	first := !r.logged
	r.logged = true
	r.mu.Unlock()
	if first {
		log.Printf("[output-files] session %s/%s: ignored a path outside the session's output directories: %.200q", info.Kind, info.ID, path)
	}
}

var (
	spillPointerRe = regexp.MustCompile(`Full output saved to\s+(\S[^\n]*?) </persisted-output>`)
	outputFileRe   = regexp.MustCompile(`Output file: ([^\n]+)`)
)

// outputPaths lists the paths a tool result names: the spill pointer, the output file of a
// background task, and any path inside the session's noa directory.
func outputPaths(text, noaDir string) []string {
	var out []string
	for _, m := range spillPointerRe.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	for _, m := range outputFileRe.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	if noaDir != "" {
		prefix := noaDir + string(filepath.Separator)
		for rest := text; ; {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i:]
			end := strings.IndexAny(rest, " \t\r\n\"'`<>")
			if end < 0 {
				end = len(rest)
			}
			out = append(out, rest[:end])
			rest = rest[end:]
		}
	}
	return out
}

// makeReadable makes path world-readable when it is a regular file inside dir. It follows no link:
// the file is reached through os.Root and examined with Lstat. ok reports whether the path was
// inside dir and a regular file (changed or already readable).
func makeReadable(dir, path string) (ok bool, err error) {
	if !filepath.IsAbs(path) {
		return false, nil
	}
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer root.Close()
	fi, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, nil
	}
	if fi.Mode().Perm()&0o444 == 0o444 {
		return true, nil
	}
	if err := root.Chmod(rel, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// readableOutputFiles is the PostToolUse hook the guard installs on every session. It runs after
// the harness has capped the result, so it sees the pointer of a spill file whichever tool made
// it (Bash, Monitor, a shell session, an MCP or custom tool, a file tool).
func readableOutputFiles(_ context.Context, s SessionInfo, _ string, _, result []byte, _ bool) {
	if s.roots == nil {
		return
	}
	var text string
	if err := json.Unmarshal(result, &text); err != nil {
		text = string(result) // not a JSON string: look at the raw bytes
	}
	if !strings.Contains(text, "persisted-output") && !strings.Contains(text, "Output file:") &&
		(s.roots.noa == "" || !strings.Contains(text, s.roots.noa)) {
		return
	}
	dirs := s.roots.dirs()
	for _, p := range outputPaths(text, s.roots.noa) {
		found := false
		for _, d := range dirs {
			ok, err := makeReadable(d, p)
			if err != nil {
				log.Printf("[output-files] session %s/%s: %v", s.Kind, s.ID, err)
				found = true // inside the directory, but could not be changed: not an ignored path
				break
			}
			if ok {
				found = true
				break
			}
		}
		if !found {
			s.roots.logOnce(s, p)
		}
	}
}

func init() { registerPostToolUse("output-files", readableOutputFiles) }

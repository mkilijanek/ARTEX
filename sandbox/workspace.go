package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Workspaces are the directories a model's tools work in: tasks/<id>[/i<intent>] and
// sessions/<id> under the data directory. They belong to the tool user, mode 0700.
//
// A workspace is made with EnsureWorkspace, which refuses a link or a file anywhere in the path.
// What a 0.5.17m server left in it (root's files, from tools that ran as root) is handed to the
// tool user by MigrateWorkspace at every session start, for that workspace only.

// The bounds of one walk (plan 4.6).
const (
	WorkspaceMaxEntries    = 200_000
	WorkspaceMaxTime       = 10 * time.Second
	WorkspaceProgressEvery = 20_000

	// maxWalkDepth bounds the levels below the workspace: every level holds one descriptor
	// while the walk is inside it, and a tool can make a tree as deep as it likes.
	maxWalkDepth = 100
)

// plainName reports whether s can be one path component of a workspace.
func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

// EnsureWorkspace makes base/rel... a directory of the tool user with mode 0700 and returns its
// path. base must exist and is not changed. Every component is made, opened and checked relative
// to its parent through os.Root; a link or a file in the way is an error, so a tool that planted
// one cannot make the server create or change anything through it. Ownership and mode are set on
// the open descriptor. Ownership needs root; without it only the mode is set.
func EnsureWorkspace(base string, rel ...string) (string, error) {
	return ensureWorkspacePath(base, os.Geteuid() == 0, rel)
}

// EnsureWorkspaceKeepOwner is EnsureWorkspace for a server whose tools do not run as the tool
// user (isolation unavailable or off): the directories are made and checked the same way and get
// mode 0700, but their owner is not changed.
func EnsureWorkspaceKeepOwner(base string, rel ...string) (string, error) {
	return ensureWorkspacePath(base, false, rel)
}

// OpenWorkspace is EnsureWorkspace that hands back the final directory as an os.Root, so the
// caller can create files in it relative to the directory it checked, whatever happens to the
// path afterwards. The caller closes it.
func OpenWorkspace(base string, rel ...string) (*os.Root, error) {
	return ensureWorkspace(base, os.Geteuid() == 0, rel)
}

// EnsureWorkspace makes a workspace the way the isolation mode asks for: handed to the tool user
// when the drop chain is on, left with its creator otherwise.
func (i *Isolation) EnsureWorkspace(base string, rel ...string) (string, error) {
	return ensureWorkspacePath(base, i.On() && os.Geteuid() == 0, rel)
}

// OpenWorkspace is EnsureWorkspace returning the directory as an os.Root.
func (i *Isolation) OpenWorkspace(base string, rel ...string) (*os.Root, error) {
	return ensureWorkspace(base, i.On() && os.Geteuid() == 0, rel)
}

func ensureWorkspacePath(base string, chown bool, rel []string) (string, error) {
	r, err := ensureWorkspace(base, chown, rel)
	if err != nil {
		return "", err
	}
	r.Close()
	return filepath.Join(append([]string{base}, rel...)...), nil
}

func ensureWorkspace(base string, chown bool, rel []string) (*os.Root, error) {
	if len(rel) == 0 {
		return nil, errors.New("workspace: no directory name")
	}
	for _, c := range rel {
		if !plainName(c) {
			return nil, fmt.Errorf("workspace: %q is not a plain directory name", c)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	for _, c := range rel {
		next, err := ensureWorkspaceDir(root, c, chown)
		root.Close()
		if err != nil {
			return nil, fmt.Errorf("workspace %s: %w", c, err)
		}
		root = next
	}
	return root, nil
}

func ensureWorkspaceDir(root *os.Root, name string, chown bool) (*os.Root, error) {
	return ensureDir(root, name, 0o700, chown, ToolUID, ToolGID)
}

// afterDirCheck, when set (tests only), runs between the check of a directory and its change, so
// a test can replace the directory there.
var afterDirCheck func(name string)

// ensureDir makes name in root a directory with the given mode and, when chown is set, owner. A
// link or a file in the way is an error. The directory is opened relative to its parent, compared
// with what Lstat saw, and changed through the open descriptor.
func ensureDir(root *os.Root, name string, mode os.FileMode, chown bool, uid, gid int) (*os.Root, error) {
	if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	// os.Root follows a link whose target stays inside the root, even for a final component
	// opened with O_NOFOLLOW, so the type is read with Lstat and the open is compared with it.
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if lfi.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("is a symbolic link")
	}
	if !lfi.IsDir() {
		return nil, errors.New("is not a directory")
	}
	if afterDirCheck != nil {
		afterDirCheck(name)
	}
	d, err := root.OpenFile(name, os.O_RDONLY|oNoFollow|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	dfi, err := d.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(lfi, dfi) {
		return nil, errors.New("changed while it was being made")
	}
	if chown {
		if err := d.Chown(uid, gid); err != nil {
			return nil, err
		}
	}
	if err := d.Chmod(mode); err != nil {
		return nil, err
	}
	next, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	nfi, err := next.Stat(".")
	if err != nil || !os.SameFile(dfi, nfi) {
		next.Close()
		return nil, errors.New("changed while it was being made")
	}
	return next, nil
}

// OpenServerDir makes base/rel... directories of the server (owner root, the given mode) and
// returns the last as an os.Root; the caller closes it. It is for the places the server writes and
// tools only read: spill, script files, drafts. Every component is made, checked and changed
// relative to its parent through os.Root; a link or a file in the way is an error. A directory that
// belongs to someone else is taken back (ownership needs root; without it only the mode is set).
func OpenServerDir(base string, mode os.FileMode, rel ...string) (*os.Root, error) {
	if len(rel) == 0 {
		return nil, errors.New("server directory: no directory name")
	}
	for _, c := range rel {
		if !plainName(c) {
			return nil, fmt.Errorf("server directory: %q is not a plain directory name", c)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("server directory: %w", err)
	}
	for _, c := range rel {
		next, err := ensureDir(root, c, mode, os.Geteuid() == 0, 0, 0)
		root.Close()
		if err != nil {
			return nil, fmt.Errorf("server directory %s: %w", c, err)
		}
		root = next
	}
	return root, nil
}

// OpenDirNoFollow opens base/rel... as an os.Root without making or changing anything. Every
// component is looked at with Lstat (a link or a file is an error), opened relative to its parent
// and compared with what Lstat saw, so a tool that replaced a component by a link makes the open
// fail instead of leading somewhere else. The caller closes the root.
func OpenDirNoFollow(base string, rel ...string) (*os.Root, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	for _, c := range rel {
		if !plainName(c) {
			root.Close()
			return nil, fmt.Errorf("%q is not a plain directory name", c)
		}
		lfi, err := root.Lstat(c)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !lfi.IsDir() {
			root.Close()
			return nil, fmt.Errorf("%s is not a directory (a link or a file)", c)
		}
		next, err := root.OpenRoot(c)
		root.Close()
		if err != nil {
			return nil, err
		}
		nfi, err := next.Stat(".")
		if err != nil || !os.SameFile(lfi, nfi) {
			next.Close()
			return nil, fmt.Errorf("%s changed while it was being opened", c)
		}
		root = next
	}
	return root, nil
}

// MigrateOptions bounds and instruments one ownership walk. The zero value is unbounded, quiet
// apart from the standard log, and without a stamp; DefaultMigrateOptions has the plan's bounds.
type MigrateOptions struct {
	MaxEntries    int           // entries handed over (not merely visited) before the walk stops; 0: no bound
	MaxTime       time.Duration // 0: no bound
	ProgressEvery int           // 0: no progress lines
	Logf          func(format string, args ...any)

	// StampPath is the root-only stamp file and StampKey the name of this workspace in it (for
	// example "tasks/5"). Both empty: no stamp.
	StampPath string
	StampKey  string

	Now func() time.Time // default time.Now

	// Test hooks.
	onChown    func(rel string)
	beforeOpen func(rel string)
}

// DefaultMigrateOptions are the bounds of plan 4.6: 200,000 entries or 10 seconds, progress every
// 20,000 entries.
func DefaultMigrateOptions() MigrateOptions {
	return MigrateOptions{MaxEntries: WorkspaceMaxEntries, MaxTime: WorkspaceMaxTime, ProgressEvery: WorkspaceProgressEvery}
}

// MigrateReport says what a walk did.
type MigrateReport struct {
	Entries   int // entries read from directories
	Chowned   int // entries handed to the tool user
	HardLinks int // files with more than one name, left as they are
	Changed   int // entries replaced between the listing and the change, left as they are
	// Complete is true when every entry was looked at (or skipped on the strength of the stamp).
	// An incomplete walk is not an error: Reason says why it stopped.
	Complete bool
	Reason   string
	Elapsed  time.Duration
}

var walkLocks sync.Map // absolute workspace path -> *sync.Mutex

// MigrateWorkspace hands every entry under dir that is not the tool user's to the tool user.
//
//   - The walk is post-order and works relative to open directories (os.Root); it never follows a
//     link. A link is changed as a link. A directory that is not the one that was classified when
//     it is opened is refused.
//   - A file with more than one name is left alone and logged: the other name may be a file of the
//     server's. With protected_hardlinks on, only links made by root can exist.
//   - The kernel clears the set-id bits of a file whose owner changes.
//   - It stops at the entry or time bound with a logged reason and no error: the session still
//     starts, because a root-owned file in a tool-owned directory gives the tool user nothing.
//   - Concurrent calls for one workspace run one after the other.
//
// The stamp skips the files of a directory that has not changed since the last complete walk: a
// file the server or a rollback created root-owned in a directory changes that directory, so a
// directory older than the stamp holds only entries a walk has seen. Directories are always
// visited, so a deep change is found whatever the directories above it did.
func MigrateWorkspace(ctx context.Context, dir string, opt MigrateOptions) (MigrateReport, error) {
	var rep MigrateReport
	abs, err := filepath.Abs(dir)
	if err != nil {
		return rep, fmt.Errorf("workspace migration: %w", err)
	}
	lfi, err := os.Lstat(abs)
	if err != nil {
		return rep, fmt.Errorf("workspace migration: %w", err)
	}
	if !lfi.IsDir() {
		return rep, fmt.Errorf("workspace migration: %s is not a directory (a link or a file)", logName(abs))
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return rep, fmt.Errorf("workspace migration: %w", err)
	}
	defer root.Close()
	rfi, err := root.Stat(".")
	if err != nil || !os.SameFile(lfi, rfi) {
		return rep, fmt.Errorf("workspace migration: %s changed while it was being opened", logName(abs))
	}

	mu, _ := walkLocks.LoadOrStore(abs, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	defer m.Unlock()

	w := &walker{ctx: ctx, opt: opt, label: abs}
	if opt.StampKey != "" {
		w.label = opt.StampKey
	}
	w.now = opt.Now
	if w.now == nil {
		w.now = time.Now
	}
	w.logf = opt.Logf
	if w.logf == nil {
		w.logf = log.Printf
	}
	w.start = w.now()
	var store *stampStore
	if opt.StampPath != "" && opt.StampKey != "" {
		store = newStampStore(opt.StampPath)
		if at, err := store.get(opt.StampKey); err == nil {
			w.since = at
		}
	}

	werr := w.walk(root, "", 0, false)
	rep = w.rep
	rep.Elapsed = w.now().Sub(w.start)
	switch {
	case errors.Is(werr, errWalkStop):
		rep.Reason = w.stopReason
	case werr != nil:
		return rep, werr
	case w.incomplete != "":
		rep.Reason = w.incomplete
	default:
		rep.Complete = true
	}
	if rep.Complete && store != nil {
		if err := store.set(opt.StampKey, w.start); err != nil {
			// A missing stamp only makes the next walk a full one.
			w.logf("[workspace] %s: could not record the walk: %v", logName(w.label), err)
		}
	}
	switch {
	case !rep.Complete:
		w.logf("[workspace] %s: stopped at the %s after visiting %d entries in %s (%d handed over); the rest is done at the next session start or with `artex __migrate-workspaces`", logName(w.label), rep.Reason, rep.Entries, rep.Elapsed.Round(time.Millisecond), rep.Chowned)
	case rep.Chowned > 0 || rep.HardLinks > 0:
		w.logf("[workspace] %s: handed %d entries to the tool user, left %d hard-linked files alone, %d entries walked in %s", logName(w.label), rep.Chowned, rep.HardLinks, rep.Entries, rep.Elapsed.Round(time.Millisecond))
	}
	return rep, nil
}

var errWalkStop = errors.New("walk stopped at its bound")

// The outcomes of chownEntry that leave an entry alone.
var (
	errEntryChanged     = errors.New("the name leads to another file than the one looked at")
	errEntryHardLinked  = errors.New("the file got a second name")
	errEntryUnsupported = errors.New("this type of entry cannot be changed through a descriptor here")
)

type walker struct {
	ctx   context.Context
	opt   MigrateOptions
	label string
	now   func() time.Time
	logf  func(string, ...any)
	start time.Time
	since time.Time // last complete walk; zero: none

	rep        MigrateReport
	stopReason string
	incomplete string // a reason this walk cannot count as complete, set without stopping it
	logged     int    // per-entry log lines so far
}

// maxEntryLogs caps the per-entry lines of one walk, so a planted tree cannot flood the log.
const maxEntryLogs = 10

func (w *walker) entryLog(format string, a ...any) {
	if w.logged < maxEntryLogs {
		w.logf(format, a...)
	}
	w.logged++
}

func (w *walker) skipIncomplete(reason string) {
	if w.incomplete == "" {
		w.incomplete = reason
	}
}

func (w *walker) check() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if w.opt.MaxTime > 0 && w.now().Sub(w.start) >= w.opt.MaxTime {
		w.stopReason = fmt.Sprintf("time limit of %s", w.opt.MaxTime)
		return errWalkStop
	}
	return nil
}

// unchanged reports whether a directory with this metadata has not changed since the last
// complete walk.
func (w *walker) unchanged(m fileMeta) bool {
	return !w.since.IsZero() && m.ChangedOK && m.Changed.Before(w.since)
}

func (w *walker) walk(r *os.Root, rel string, depth int, unchanged bool) error {
	// d stays open for the whole level: the entries are changed relative to it.
	d, err := r.Open(".")
	if err != nil {
		return fmt.Errorf("workspace migration: open %s: %w", logName(rel), err)
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("workspace migration: read %s: %w", logName(rel), err)
	}
	for _, e := range ents {
		w.rep.Entries++
		if every := w.opt.ProgressEvery; every > 0 && w.rep.Entries%every == 0 {
			w.logf("[workspace] %s: %d entries walked", logName(w.label), w.rep.Entries)
		}
		if err := w.check(); err != nil {
			return err
		}
		name := e.Name()
		erel := path.Join(rel, name)
		if !e.IsDir() && unchanged {
			continue
		}
		fi, err := r.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // gone since the listing
			}
			w.entryLog("[workspace] %s: %s: %v", logName(w.label), logName(erel), err)
			w.skipIncomplete("errors")
			continue
		}
		meta, ok := metaOf(fi)
		if !ok {
			return errors.New("workspace migration: this platform reports no file owners")
		}
		if fi.IsDir() {
			if depth >= maxWalkDepth {
				w.entryLog("[workspace] %s: not walked, deeper than %d levels: %s", logName(w.label), maxWalkDepth, logName(erel))
				w.skipIncomplete("depth bound")
				continue
			}
			if err := w.into(r, name, erel, fi, meta, depth); err != nil {
				return err
			}
		} else if meta.Nlink > 1 {
			w.rep.HardLinks++
			w.entryLog("[workspace] %s: left alone, it has %d names: %s", logName(w.label), meta.Nlink, logName(erel))
			continue
		}
		if err := w.fix(d, r, name, erel, fi, meta); err != nil {
			return err
		}
	}
	return nil
}

// into walks one subdirectory, opened relative to its parent and checked to be the directory that
// was classified.
func (w *walker) into(r *os.Root, name, erel string, fi os.FileInfo, meta fileMeta, depth int) error {
	if w.opt.beforeOpen != nil {
		w.opt.beforeOpen(erel)
	}
	sub, err := r.OpenRoot(name)
	if err != nil {
		w.entryLog("[workspace] %s: not entered, %s: %v", logName(w.label), logName(erel), err)
		w.skipIncomplete("errors")
		return nil
	}
	defer sub.Close()
	nfi, err := sub.Stat(".")
	if err != nil || !os.SameFile(fi, nfi) {
		w.entryLog("[workspace] %s: %s changed while it was being walked; not entered", logName(w.label), logName(erel))
		w.skipIncomplete("errors")
		return nil
	}
	return w.walk(sub, erel, depth+1, w.unchanged(meta))
}

// fix hands one entry to the tool user. It changes the entry itself, never a link's target, and
// only when the name still leads to the entry the walk looked at (fi): the change is made through
// a descriptor opened without following links and compared with fi, never by name. An entry that
// was replaced or got a second name since is left alone and counted in Changed.
//
// The entry bound counts the entries that needed this (the work), not the entries visited: a walk
// that stops at the bound leaves a tree with less to do, so repeated session starts converge, and
// a tree that is already right costs one visiting pass whatever its size.
func (w *walker) fix(dir *os.File, r *os.Root, name, erel string, fi os.FileInfo, meta fileMeta) error {
	if meta.UID == ToolUID && meta.GID == ToolGID {
		return nil
	}
	if w.opt.MaxEntries > 0 && w.rep.Chowned >= w.opt.MaxEntries {
		w.stopReason = fmt.Sprintf("limit of %d entries handed over", w.opt.MaxEntries)
		return errWalkStop
	}
	if w.opt.onChown != nil {
		w.opt.onChown(erel)
	}
	if err := chownEntry(dir, r, name, fi, ToolUID, ToolGID); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if errors.Is(err, errEntryChanged) || errors.Is(err, errEntryHardLinked) {
			w.rep.Changed++
			w.entryLog("[workspace] %s: left alone, it changed while it was being handed over: %s", logName(w.label), logName(erel))
			w.skipIncomplete("changed entries")
			return nil
		}
		w.entryLog("[workspace] %s: %s: %v", logName(w.label), logName(erel), err)
		w.skipIncomplete("errors")
		return nil
	}
	w.rep.Chowned++
	return nil
}

// logName makes a path safe for one log line: control characters become '?', the length is
// capped, and the result is quoted.
func logName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return strconv.Quote(s)
}

// ---- the stamp ----

// maxStampEntries bounds the stamp file: when it grows past this the oldest half is dropped, and
// those workspaces get one full walk again.
const maxStampEntries = 20_000

// stampMu serialises changes to the stamp file within the process.
var stampMu sync.Mutex

type stampStore struct{ path string }

func newStampStore(path string) *stampStore { return &stampStore{path: path} }

type stampFile struct {
	Walks map[string]int64 `json:"walks"` // workspace key -> start of its last complete walk, unix nanoseconds
}

var errNoStamp = errors.New("no stamp")

func (s *stampStore) load() (stampFile, error) {
	sf := stampFile{Walks: map[string]int64{}}
	root, err := os.OpenRoot(filepath.Dir(s.path))
	if err != nil {
		return sf, err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(s.path), os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return sf, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 32<<20))
	if err != nil {
		return sf, err
	}
	if err := json.Unmarshal(data, &sf); err != nil || sf.Walks == nil {
		// An unreadable stamp is the same as none: the next walk is a full one.
		return stampFile{Walks: map[string]int64{}}, nil
	}
	return sf, nil
}

func (s *stampStore) get(key string) (time.Time, error) {
	stampMu.Lock()
	defer stampMu.Unlock()
	sf, err := s.load()
	if err != nil {
		return time.Time{}, err
	}
	ns, ok := sf.Walks[key]
	if !ok {
		return time.Time{}, errNoStamp
	}
	return time.Unix(0, ns), nil
}

func (s *stampStore) set(key string, at time.Time) error {
	stampMu.Lock()
	defer stampMu.Unlock()
	dir := filepath.Dir(s.path)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return err
	}
	if err := parent.Mkdir(filepath.Base(dir), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		parent.Close()
		return err
	}
	if fi, err := parent.Lstat(filepath.Base(dir)); err != nil || !fi.IsDir() {
		parent.Close()
		return fmt.Errorf("%s is not a directory", dir)
	}
	parent.Close()

	sf, err := s.load()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	sf.Walks[key] = at.UnixNano()
	if len(sf.Walks) > maxStampEntries {
		type kv struct {
			k string
			v int64
		}
		all := make([]kv, 0, len(sf.Walks))
		for k, v := range sf.Walks {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
		sf.Walks = map[string]int64{}
		for _, e := range all[:maxStampEntries/2] {
			sf.Walks[e.k] = e.v
		}
	}
	body, err := json.Marshal(sf)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	tmp := filepath.Base(s.path) + ".tmp-" + hex.EncodeToString(raw[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, filepath.Base(s.path)); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}

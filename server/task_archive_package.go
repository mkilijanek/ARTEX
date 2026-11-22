package server

import (
	"archive/tar"
	"crypto/sha256"
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
	"time"

	"github.com/klauspost/compress/zstd"
	pgdb "github.com/mkilijanek/artex/db"
)

const (
	archiveDirMode  = 0o700
	archiveFileMode = 0o600
	maxArchiveFiles = 1_000_000
	maxArchiveBytes = int64(1 << 47) // 128 TiB safety ceiling for corrupt headers.
)

type archiveFileMove struct {
	Source   string `json:"source"`
	Relative string `json:"relative"`
}

type archiveStageJournal struct {
	ArchiveID int64             `json:"archive_id"`
	TaskID    string            `json:"task_id"`
	Moves     []archiveFileMove `json:"moves"`
}

type archiveRestoreJournal struct {
	ArchiveID int64                 `json:"archive_id"`
	TaskID    int64                 `json:"task_id"`
	Moves     []restoredArchivePath `json:"moves"`
}

type taskArchiveFileStage struct {
	root    string
	payload string
	journal archiveStageJournal
	done    bool
}

type restoredArchivePath struct {
	Source      string
	Destination string
}

type taskArchiveRestoreFiles struct {
	extracted string
	moves     []restoredArchivePath
	done      bool
}

func writeArchiveJournal(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func taskArchiveRoot(dataDir string) string {
	return filepath.Join(dataDir, "archives", "tasks")
}

func taskArchivePath(dataDir string, archiveID int64, taskID string) string {
	return filepath.Join(taskArchiveRoot(dataDir), fmt.Sprintf("task-%s-%d.tar.zst", taskID, archiveID))
}

func stageTaskArchiveFiles(dataDir string, archiveID int64, taskID string, explorationID int64) (*taskArchiveFileStage, error) {
	root := filepath.Join(taskArchiveRoot(dataDir), ".staging", strconv.FormatInt(archiveID, 10))
	stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload")}
	journalPath := filepath.Join(root, "journal.json")
	if raw, err := os.ReadFile(journalPath); err == nil {
		if err := json.Unmarshal(raw, &stage.journal); err != nil {
			return nil, fmt.Errorf("read task archive staging journal: %w", err)
		}
		// The journal is written before the first rename, so its presence does not mean
		// the payload is complete: a previous round may have failed mid-loop with a
		// rollback that itself errored, which leaves the root in place. Replay the moves
		// rather than packaging a payload that is missing transcripts or workspace files.
		if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
			return nil, err
		}
		if err := stage.applyMoves(); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		return stage, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(stage.payload, "files"), archiveDirMode); err != nil {
		return nil, err
	}
	stage.journal = archiveStageJournal{ArchiveID: archiveID, TaskID: taskID}
	targets := []archiveFileMove{}
	taskDir := filepath.Join(dataDir, "tasks", taskID)
	if _, err := os.Lstat(taskDir); err == nil {
		targets = append(targets, archiveFileMove{Source: taskDir, Relative: filepath.Join("files", "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcriptDir := filepath.Join(dataDir, "transcripts")
	entries, err := os.ReadDir(transcriptDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	prefix := fmt.Sprintf("exp%d-", explorationID)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || (!entry.IsDir() && !strings.HasSuffix(name, ".jsonl")) {
			continue
		}
		targets = append(targets, archiveFileMove{
			Source: filepath.Join(transcriptDir, name), Relative: filepath.Join("files", "transcripts", name),
		})
	}
	// Persist the complete plan before the first rename. Recovery can therefore
	// roll back any prefix of the moves after an abrupt process termination.
	stage.journal.Moves = targets
	if err := writeArchiveJournal(journalPath, stage.journal); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	if err := stage.applyMoves(); err != nil {
		_ = stage.rollback()
		return nil, err
	}
	return stage, nil
}

// applyMoves performs the renames recorded in the journal. Entries already sitting
// in the payload are skipped, so an interrupted staging round can be resumed in
// place without moving anything twice.
func (s *taskArchiveFileStage) applyMoves() error {
	for _, move := range s.journal.Moves {
		destination := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if _, err := os.Lstat(move.Source); err != nil {
			if os.IsNotExist(err) {
				// Neither side exists: the path was removed outside the archive flow
				// after the journal was written. Nothing can be staged for it.
				continue
			}
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), archiveDirMode); err != nil {
			return err
		}
		if err := os.Rename(move.Source, destination); err != nil {
			return fmt.Errorf("stage task archive path %s: %w", move.Source, err)
		}
	}
	return nil
}

func (s *taskArchiveFileStage) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.journal.Moves) - 1; i >= 0; i-- {
		move := s.journal.Moves[i]
		staged := filepath.Join(s.payload, move.Relative)
		if _, err := os.Lstat(staged); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("archive rollback destination exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), 0o755); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		if err := os.RemoveAll(s.root); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = true
	return errors.Join(errs...)
}

func (s *taskArchiveFileStage) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.root)
}

func installTaskArchiveFiles(dataDir, extractedDir, taskID string, archiveID int64) (*taskArchiveRestoreFiles, error) {
	stage := &taskArchiveRestoreFiles{extracted: extractedDir}
	numericTaskID, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || numericTaskID <= 0 {
		return nil, fmt.Errorf("invalid restore task id %q", taskID)
	}
	sources := []restoredArchivePath{}
	workspace := filepath.Join(extractedDir, "files", "tasks", taskID)
	if _, err := os.Lstat(workspace); err == nil {
		sources = append(sources, restoredArchivePath{Source: workspace, Destination: filepath.Join(dataDir, "tasks", taskID)})
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transcripts := filepath.Join(extractedDir, "files", "transcripts")
	entries, err := os.ReadDir(transcripts)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		sources = append(sources, restoredArchivePath{
			Source: filepath.Join(transcripts, entry.Name()), Destination: filepath.Join(dataDir, "transcripts", entry.Name()),
		})
	}
	// Destinations and sources are resolved through a root on the data directory:
	// a link in a destination component that leaves it is refused by the root, and
	// one that stays inside it is refused by the directory check below.
	dataRoot, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, err
	}
	defer dataRoot.Close()
	relatives := make([][2]string, len(sources))
	for i, move := range sources {
		source, err := filepath.Rel(dataDir, move.Source)
		if err != nil || !filepath.IsLocal(source) {
			return nil, fmt.Errorf("restore source is outside the data directory: %s", move.Source)
		}
		destination, err := filepath.Rel(dataDir, move.Destination)
		if err != nil || !filepath.IsLocal(destination) {
			return nil, fmt.Errorf("restore destination is outside the data directory: %s", move.Destination)
		}
		relatives[i] = [2]string{source, destination}
		if _, err := dataRoot.Lstat(destination); err == nil {
			return nil, fmt.Errorf("restore destination already exists: %s", move.Destination)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	stage.moves = sources
	journal := archiveRestoreJournal{ArchiveID: archiveID, TaskID: numericTaskID, Moves: sources}
	if err := writeArchiveJournal(filepath.Join(extractedDir, "restore-journal.json"), journal); err != nil {
		return nil, err
	}
	for i := range sources {
		parent := filepath.Dir(relatives[i][1])
		if err := ensureRealDirectory(dataRoot, parent); err != nil {
			_ = stage.rollback()
			return nil, err
		}
		if err := dataRoot.Rename(relatives[i][0], relatives[i][1]); err != nil {
			_ = stage.rollback()
			return nil, err
		}
	}
	return stage, nil
}

// ensureRealDirectory creates rel (a path relative to root) and requires every
// component to be a real directory, never a link.
func ensureRealDirectory(root *os.Root, rel string) error {
	if rel == "." {
		return nil
	}
	if err := root.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("restore destination component is not a directory: %s", current)
		}
	}
	return nil
}

func (s *taskArchiveRestoreFiles) rollback() error {
	if s == nil || s.done {
		return nil
	}
	var errs []error
	for i := len(s.moves) - 1; i >= 0; i-- {
		move := s.moves[i]
		if _, err := os.Lstat(move.Destination); os.IsNotExist(err) {
			continue
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(move.Source); err == nil {
			errs = append(errs, fmt.Errorf("restore rollback source exists: %s", move.Source))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(move.Source), archiveDirMode); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(move.Destination, move.Source); err != nil {
			errs = append(errs, err)
		}
	}
	s.done = true
	return errors.Join(errs...)
}

func (s *taskArchiveRestoreFiles) commit() error {
	if s == nil || s.done {
		return nil
	}
	s.done = true
	return os.RemoveAll(s.extracted)
}

// recoverTaskArchiveRestoreStages resolves file installs left by an interrupted
// restore. If PostgreSQL committed, installed files are authoritative and only
// the extraction directory is stale. Otherwise all completed renames are moved
// back so the persistent restore job can retry from a clean destination.
func recoverTaskArchiveRestoreStages(dataDir string, pg *pgdb.DB) error {
	parent := filepath.Join(taskArchiveRoot(dataDir), ".restore")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "restore-journal.json"))
		if os.IsNotExist(err) {
			// Extraction was interrupted before any destination rename.
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveRestoreJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		restored, err := pg.IsTaskArchiveRestored(journal.ArchiveID)
		if errors.Is(err, pgdb.ErrTaskArchiveNotFound) && journal.TaskID > 0 {
			task, taskErr := pg.GetTask(journal.TaskID)
			if taskErr != nil {
				errs = append(errs, taskErr)
				continue
			}
			restored = task != nil
			err = nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if restored {
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveRestoreFiles{extracted: root, moves: journal.Moves}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverTaskArchiveStages resolves file renames left by an interrupted archive.
// A committed cold task already has a verified package, so stale staging can be
// discarded; otherwise files are moved back before the persistent job retries.
func recoverTaskArchiveStages(dataDir string, pg *pgdb.DB) error {
	parent := filepath.Join(taskArchiveRoot(dataDir), ".staging")
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(parent, entry.Name())
		raw, err := os.ReadFile(filepath.Join(root, "journal.json"))
		if os.IsNotExist(err) {
			// Either staging died between creating the directory tree and writing the
			// journal — no rename had run, so payload holds nothing — or commit/rollback
			// failed part-way through removing the root, in which case payload only holds
			// copies already inside the package. Without a journal there is nothing to
			// roll back, and reporting an error here keeps the archive worker from ever
			// starting, so discard the directory instead.
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var journal archiveStageJournal
		if err := json.Unmarshal(raw, &journal); err != nil {
			errs = append(errs, err)
			continue
		}
		archive, getErr := pg.GetTaskArchive(journal.ArchiveID)
		if getErr != nil {
			errs = append(errs, getErr)
			continue
		}
		if archive != nil && archive.State == pgdb.ArchiveReady {
			if err := os.RemoveAll(root); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		stage := &taskArchiveFileStage{root: root, payload: filepath.Join(root, "payload"), journal: journal}
		if err := stage.rollback(); err != nil {
			errs = append(errs, err)
		}
		if archive != nil && archive.ArchivePath != "" {
			_ = os.Remove(archive.ArchivePath)
		}
	}
	return errors.Join(errs...)
}

func recoverTaskArchiveDeletePackages(dataDir string, pg *pgdb.DB) error {
	root := taskArchiveRoot(dataDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		marker := strings.LastIndex(name, ".deleting-")
		if marker < 0 {
			continue
		}
		archiveID, err := strconv.ParseInt(name[marker+len(".deleting-"):], 10, 64)
		if err != nil || archiveID <= 0 {
			continue
		}
		staged := filepath.Join(root, name)
		original := filepath.Join(root, name[:marker])
		archive, err := pg.GetTaskArchive(archiveID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if archive == nil {
			if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		if archive.State == pgdb.DeleteQueued || archive.State == pgdb.Deleting || archive.State == pgdb.DeleteFailed {
			// The idempotent delete worker consumes the staged path directly.
			continue
		}
		if _, err := os.Lstat(original); err == nil {
			errs = append(errs, fmt.Errorf("archive delete recovery destination exists: %s", original))
			continue
		} else if !os.IsNotExist(err) {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(staged, original); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func stageTaskArchivePackageDelete(archivePath string, archiveID int64) (string, bool, error) {
	staged := archivePath + fmt.Sprintf(".deleting-%d", archiveID)
	if _, err := os.Lstat(staged); err == nil {
		if _, originalErr := os.Lstat(archivePath); originalErr == nil {
			return staged, false, errors.New("archive package original and delete-staging files both exist")
		} else if !os.IsNotExist(originalErr) {
			return staged, false, originalErr
		}
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	if err := os.Rename(archivePath, staged); err == nil {
		return staged, true, nil
	} else if !os.IsNotExist(err) {
		return staged, false, err
	}
	return staged, false, nil
}

// errArchiveEntryChanged marks an entry that was not what the directory listing
// and Lstat said it was when it was opened, or that changed while being read.
var errArchiveEntryChanged = errors.New("entry changed during packaging")

// archiveBeforeOpenHook is a test seam, called after an entry was checked with Lstat
// and before it is opened. It is nil in production.
var archiveBeforeOpenHook func(dir, name string)

type archiveWalker struct {
	tw           *tar.Writer
	root         string
	originalSize int64
	skipped      int64
}

func (w *archiveWalker) refuse(rel, cause string) error {
	log.Printf("[task-archive] refused: entry changed during packaging: %q", rel)
	return fmt.Errorf("%w: %s", errArchiveEntryChanged, cause)
}

// walkDir archives the entries of dir in name order. Every directory and file is
// opened relative to its parent through os.Root and compared with the Lstat taken
// before the open, so a swap between the check and the open is detected on the
// descriptor instead of being followed.
func (w *archiveWalker) walkDir(dir *os.Root, rel string) error {
	listing, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := listing.ReadDir(-1)
	if err := errors.Join(readErr, listing.Close()); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		name := entry.Name()
		display := path.Join(rel, name)
		info, err := dir.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return w.refuse(display, "entry vanished")
		}
		if err != nil {
			return err
		}
		if archiveBeforeOpenHook != nil {
			archiveBeforeOpenHook(filepath.Join(w.root, filepath.FromSlash(rel)), name)
		}
		switch {
		case info.IsDir():
			err = w.archiveDir(dir, name, display, info)
		case info.Mode().IsRegular():
			err = w.archiveFile(dir, name, display, info)
		default:
			// The archive format supports only regular files and directories end to end
			// (the unpacker errors out on other types), so links and special files cannot
			// be restored. Skip rather than fail the whole package; the entry is never
			// opened, and it is counted in the manifest.
			w.skipped++
			log.Printf("[task-archive] skipping unsupported entry (type %s, other files unaffected): %q", info.Mode().Type(), display)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *archiveWalker) archiveDir(parent *os.Root, name, display string, info fs.FileInfo) error {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return w.refuse(display, "directory could not be opened in place: "+err.Error())
	}
	defer child.Close()
	opened, err := child.Stat(".")
	if err != nil {
		return err
	}
	if !opened.IsDir() || !os.SameFile(info, opened) {
		return w.refuse(display, "directory was replaced")
	}
	header, err := tar.FileInfoHeader(opened, "")
	if err != nil {
		return err
	}
	header.Name = display
	if err := w.tw.WriteHeader(header); err != nil {
		return err
	}
	return w.walkDir(child, display)
}

func (w *archiveWalker) archiveFile(dir *os.Root, name, display string, info fs.FileInfo) error {
	input, err := dir.OpenFile(name, os.O_RDONLY|archiveOpenFlags, 0)
	if err != nil {
		return w.refuse(display, "file could not be opened in place: "+err.Error())
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) ||
		opened.Size() != info.Size() || !opened.ModTime().Equal(info.ModTime()) {
		return w.refuse(display, "file was replaced or modified")
	}
	// A file with more than one hard link may be the same file as one outside the payload; like a
	// symlink it is skipped and counted, never read.
	if links, ok := archiveLinkCount(opened); ok && links > 1 {
		w.skipped++
		log.Printf("[task-archive] skipping unsupported entry (hard-linked file, other files unaffected): %q", display)
		return nil
	}
	header, err := tar.FileInfoHeader(opened, "")
	if err != nil {
		return err
	}
	header.Name = display
	if err := w.tw.WriteHeader(header); err != nil {
		return err
	}
	copied, err := io.CopyN(w.tw, input, opened.Size())
	if errors.Is(err, io.EOF) {
		return w.refuse(display, fmt.Sprintf("file shrank while reading (%d of %d bytes)", copied, opened.Size()))
	}
	if err != nil {
		return err
	}
	var probe [1]byte
	if n, err := input.Read(probe[:]); n > 0 {
		return w.refuse(display, "file grew while reading")
	} else if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	after, err := input.Stat()
	if err != nil {
		return err
	}
	if after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return w.refuse(display, "file was modified while reading")
	}
	w.originalSize += opened.Size()
	return nil
}

func writeTaskArchivePackage(path, payloadDir string, snapshot *pgdb.TaskArchiveSnapshot) (originalSize, compressedSize int64, checksum string, err error) {
	if snapshot == nil {
		return 0, 0, "", errors.New("nil task archive snapshot")
	}
	if err = os.MkdirAll(filepath.Dir(path), archiveDirMode); err != nil {
		return 0, 0, "", err
	}
	payloadRoot, err := os.OpenRoot(payloadDir)
	if err != nil {
		return 0, 0, "", err
	}
	defer payloadRoot.Close()
	temporary := path + ".partial"
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
	if err != nil {
		return 0, 0, "", err
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporary)
		}
	}()
	hasher := sha256.New()
	multi := io.MultiWriter(file, hasher)
	encoder, err := zstd.NewWriter(multi,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(10)),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		return 0, 0, "", err
	}
	tarWriter := tar.NewWriter(encoder)
	walker := &archiveWalker{tw: tarWriter, root: payloadDir}
	if walkErr := walker.walkDir(payloadRoot, ""); walkErr != nil {
		_ = tarWriter.Close()
		_ = encoder.Close()
		return 0, 0, "", walkErr
	}
	originalSize = walker.originalSize
	// The manifest is written last so that it can count the entries that were skipped.
	manifestSnapshot := *snapshot
	manifestSnapshot.DataCounts = make(map[string]int64, len(snapshot.DataCounts)+1)
	for key, value := range snapshot.DataCounts {
		manifestSnapshot.DataCounts[key] = value
	}
	if walker.skipped > 0 {
		manifestSnapshot.DataCounts["skipped_entries"] = walker.skipped
	}
	manifest, err := json.Marshal(&manifestSnapshot)
	if err != nil {
		return 0, 0, "", err
	}
	manifest = append(manifest, '\n')
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "manifest.json", Typeflag: tar.TypeReg, Mode: archiveFileMode, Size: int64(len(manifest)), ModTime: time.Now(),
	}); err != nil {
		return 0, 0, "", err
	}
	if _, err := tarWriter.Write(manifest); err != nil {
		return 0, 0, "", err
	}
	if err := tarWriter.Close(); err != nil {
		_ = encoder.Close()
		return 0, 0, "", err
	}
	if err := encoder.Close(); err != nil {
		return 0, 0, "", err
	}
	if err := file.Sync(); err != nil {
		return 0, 0, "", err
	}
	if err := file.Close(); err != nil {
		return 0, 0, "", err
	}
	stat, err := os.Stat(temporary)
	if err != nil {
		return 0, 0, "", err
	}
	compressedSize = stat.Size()
	checksum = hex.EncodeToString(hasher.Sum(nil))
	if err := os.Rename(temporary, path); err != nil {
		return 0, 0, "", err
	}
	if err := os.Chmod(path, archiveFileMode); err != nil {
		return 0, 0, "", err
	}
	committed = true
	return originalSize, compressedSize, checksum, nil
}

func extractTaskArchivePackage(path, expectedSHA, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if expectedSHA != "" {
		hasher := sha256.New()
		if _, err := io.Copy(hasher, file); err != nil {
			return err
		}
		if hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(expectedSHA) {
			return errors.New("task archive checksum mismatch")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	decoder, err := zstd.NewReader(file)
	if err != nil {
		return err
	}
	defer decoder.Close()
	tarReader := tar.NewReader(decoder)
	if err := os.MkdirAll(destination, archiveDirMode); err != nil {
		return err
	}
	// Everything is written through the root, so no path can resolve outside the
	// destination and a link is never followed or created.
	destRoot, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer destRoot.Close()
	var entries int
	var total int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxArchiveFiles || header.Size < 0 || total+header.Size > maxArchiveBytes {
			return errors.New("task archive exceeds extraction safety limits")
		}
		total += header.Size
		clean := filepath.Clean(filepath.FromSlash(header.Name))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe task archive path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := destRoot.MkdirAll(clean, archiveDirMode); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := destRoot.MkdirAll(filepath.Dir(clean), archiveDirMode); err != nil {
				return err
			}
			output, err := destRoot.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY|archiveOpenFlags, fs.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(output, tarReader, header.Size)
			closeErr := output.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported task archive entry type %d", header.Typeflag)
		}
	}
	return nil
}

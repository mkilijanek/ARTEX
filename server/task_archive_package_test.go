package server

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	pgdb "github.com/mkilijanek/artex/db"
)

func TestTaskArchivePackageFilesRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	taskID := "42"
	explorationID := int64(73)
	taskFile := filepath.Join(dataDir, "tasks", taskID, "uploads", "evidence.txt")
	transcriptFile := filepath.Join(dataDir, "transcripts", "exp73-worker-1.jsonl")
	unrelatedTranscript := filepath.Join(dataDir, "transcripts", "exp74-worker-1.jsonl")
	for path, body := range map[string]string{
		taskFile:            "task evidence",
		transcriptFile:      "transcript",
		unrelatedTranscript: "leave me hot",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stage, err := stageTaskArchiveFiles(dataDir, 1, taskID, explorationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("task file still hot after staging: %v", err)
	}
	if _, err := os.Stat(transcriptFile); !os.IsNotExist(err) {
		t.Fatalf("task transcript still hot after staging: %v", err)
	}
	if _, err := os.Stat(unrelatedTranscript); err != nil {
		t.Fatalf("unrelated transcript was staged: %v", err)
	}

	archivePath := taskArchivePath(dataDir, 1, taskID)
	streamPath := filepath.Join(stage.payload, filepath.FromSlash(pgdb.TaskArchiveLLMRecordsPath))
	if err := os.MkdirAll(filepath.Dir(streamPath), archiveDirMode); err != nil {
		t.Fatal(err)
	}
	streamBody := []byte("{\"id\":1,\"raw_request\":\"large\"}\n")
	if err := os.WriteFile(streamPath, streamBody, archiveFileMode); err != nil {
		t.Fatal(err)
	}
	snapshot := &pgdb.TaskArchiveSnapshot{
		FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 42, ExplorationID: explorationID,
		StreamedTables: map[string]string{"llm_records": pgdb.TaskArchiveLLMRecordsPath},
	}
	original, compressed, checksum, err := writeTaskArchivePackage(archivePath, stage.payload, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if original == 0 || compressed == 0 || checksum == "" {
		t.Fatalf("invalid package metrics original=%d compressed=%d checksum=%q", original, compressed, checksum)
	}
	if err := stage.commit(); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(dataDir, "restore")
	if err := extractTaskArchivePackage(archivePath, checksum, extracted); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(extracted, filepath.FromSlash(pgdb.TaskArchiveLLMRecordsPath))); err != nil || !bytes.Equal(got, streamBody) {
		t.Fatalf("streamed LLM archive payload=%q err=%v", got, err)
	}
	installed, err := installTaskArchiveFiles(dataDir, extracted, taskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := installed.commit(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{taskFile: "task evidence", transcriptFile: "transcript"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("restored %s = %q, %v; want %q", path, got, err, want)
		}
	}
	if err := extractTaskArchivePackage(archivePath, "deadbeef", filepath.Join(dataDir, "bad-checksum")); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
}

func TestTaskArchivePackageSkipsSymlink(t *testing.T) {
	dataDir := t.TempDir()
	payload := filepath.Join(dataDir, "payload")
	if err := os.MkdirAll(payload, archiveDirMode); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(payload, "keep.txt")
	if err := os.WriteFile(regular, []byte("keep me"), archiveFileMode); err != nil {
		t.Fatal(err)
	}
	// A symlink appearing in the working directory should be skipped instead of failing the whole archive.
	if err := os.Symlink(regular, filepath.Join(payload, "link.txt")); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	archivePath := filepath.Join(dataDir, "archive.tar.zst")
	snapshot := &pgdb.TaskArchiveSnapshot{FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 42}
	_, _, checksum, err := writeTaskArchivePackage(archivePath, payload, snapshot)
	if err != nil {
		t.Fatalf("archive should skip symlink, not fail: %v", err)
	}
	extracted := filepath.Join(dataDir, "restore")
	if err := extractTaskArchivePackage(archivePath, checksum, extracted); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(extracted, "keep.txt")); err != nil || string(got) != "keep me" {
		t.Fatalf("regular file not archived: got=%q err=%v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(extracted, "link.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink should have been skipped, but link.txt exists: %v", err)
	}
}

func TestTaskArchiveStageJournalRollsBackInterruptedMoves(t *testing.T) {
	dataDir := t.TempDir()
	taskFile := filepath.Join(dataDir, "tasks", "42", "evidence.txt")
	transcriptFile := filepath.Join(dataDir, "transcripts", "exp73-worker.jsonl")
	for _, path := range []string{taskFile, transcriptFile} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stage, err := stageTaskArchiveFiles(dataDir, 9, "42", 73)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(stage.root, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal archiveStageJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	if len(journal.Moves) != 2 {
		t.Fatalf("journal moves=%d, want 2", len(journal.Moves))
	}
	recovered := &taskArchiveFileStage{root: stage.root, payload: stage.payload, journal: journal}
	if err := recovered.rollback(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{taskFile, transcriptFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("interrupted archive did not restore %s: %v", path, err)
		}
	}
}

func TestTaskArchiveRestoreJournalRollsBackInterruptedInstall(t *testing.T) {
	dataDir := t.TempDir()
	extracted := filepath.Join(dataDir, "archives", "tasks", ".restore", "11-test")
	source := filepath.Join(extracted, "files", "tasks", "42", "evidence.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := installTaskArchiveFiles(dataDir, extracted, "42", 11)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dataDir, "tasks", "42", "evidence.txt")
	if _, err := os.Stat(destination); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(extracted, "restore-journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var journal archiveRestoreJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	if journal.ArchiveID != 11 || len(journal.Moves) != 1 {
		t.Fatalf("unexpected restore journal: %+v", journal)
	}
	recovered := &taskArchiveRestoreFiles{extracted: extracted, moves: journal.Moves}
	if err := recovered.rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("hot destination remains after restore rollback: %v", err)
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "evidence" {
		t.Fatalf("restore source=%q err=%v", got, err)
	}
}

func TestTaskArchiveDeletePackageCanResumeFromStagedPath(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := taskArchivePath(dataDir, 21, "42")
	if err := os.MkdirAll(filepath.Dir(archivePath), archiveDirMode); err != nil {
		t.Fatal(err)
	}
	staged := archivePath + ".deleting-21"
	if err := os.WriteFile(staged, []byte("archive"), archiveFileMode); err != nil {
		t.Fatal(err)
	}
	got, moved, err := stageTaskArchivePackageDelete(archivePath, 21)
	if err != nil || !moved || got != staged {
		t.Fatalf("resume staged package path=%q moved=%v err=%v", got, moved, err)
	}
}

func TestTaskArchivePackageRejectsTraversal(t *testing.T) {
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(encoder)
	content := []byte("escape")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "malicious.tar.zst")
	if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := extractTaskArchivePackage(path, "", root); err == nil {
		t.Fatal("path traversal archive was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal wrote outside destination: %v", err)
	}
}

// archiveRaceFixture builds a payload with one directory and one file, plus a secret
// outside it that must never appear in a package.
type archiveRaceFixture struct {
	base, payload, secretDir, secretFile string
	secret                               string
}

func newArchiveRaceFixture(t *testing.T) *archiveRaceFixture {
	t.Helper()
	base := t.TempDir()
	f := &archiveRaceFixture{
		base: base, payload: filepath.Join(base, "payload"),
		secretDir: filepath.Join(base, "outside"), secret: "TOP-SECRET-MARKER-0123456789abcdef",
	}
	f.secretFile = filepath.Join(f.secretDir, "secret.txt")
	for dir, files := range map[string]map[string]string{
		filepath.Join(f.payload, "sub"): {"inner.txt": "inner"},
		f.payload:                       {"plain.txt": "x" + strings.Repeat("y", len(f.secret)-1)},
		f.secretDir:                     {"secret.txt": f.secret},
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}

func (f *archiveRaceFixture) hook(t *testing.T, fn func(dir, name string)) {
	t.Helper()
	archiveBeforeOpenHook = fn
	t.Cleanup(func() { archiveBeforeOpenHook = nil })
}

// pack runs the packager and returns the error plus the raw bytes of the package, if any.
func (f *archiveRaceFixture) pack(t *testing.T) (error, []byte) {
	t.Helper()
	archivePath := filepath.Join(f.base, "out", "archive.tar.zst")
	snapshot := &pgdb.TaskArchiveSnapshot{FormatVersion: pgdb.TaskArchiveFormatVersion, TaskID: 42}
	_, _, checksum, err := writeTaskArchivePackage(archivePath, f.payload, snapshot)
	if err != nil {
		if _, statErr := os.Stat(archivePath); !os.IsNotExist(statErr) {
			t.Fatalf("a failed packaging left an archive behind: %v", statErr)
		}
		return err, nil
	}
	extracted := filepath.Join(f.base, "restored")
	if err := extractTaskArchivePackage(archivePath, checksum, extracted); err != nil {
		t.Fatal(err)
	}
	var all bytes.Buffer
	_ = filepath.WalkDir(extracted, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			raw, _ := os.ReadFile(p)
			all.Write(raw)
		}
		return nil
	})
	return nil, all.Bytes()
}

func TestArchivePackagingRefusesFileSwappedForSymlink(t *testing.T) {
	f := newArchiveRaceFixture(t)
	f.hook(t, func(dir, name string) {
		if name != "plain.txt" {
			return
		}
		target := filepath.Join(dir, name)
		if err := os.Remove(target); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(f.secretFile, target); err != nil {
			t.Error(err)
		}
	})
	err, content := f.pack(t)
	if !errors.Is(err, errArchiveEntryChanged) {
		t.Fatalf("packaging a file swapped for a symlink: err=%v, want errArchiveEntryChanged", err)
	}
	if bytes.Contains(content, []byte(f.secret)) {
		t.Fatal("the archive contains the link target")
	}
}

func TestArchivePackagingRefusesDirectorySwappedForSymlink(t *testing.T) {
	f := newArchiveRaceFixture(t)
	f.hook(t, func(dir, name string) {
		if name != "sub" {
			return
		}
		target := filepath.Join(dir, name)
		if err := os.RemoveAll(target); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(f.secretDir, target); err != nil {
			t.Error(err)
		}
	})
	err, content := f.pack(t)
	if !errors.Is(err, errArchiveEntryChanged) {
		t.Fatalf("packaging a directory swapped for a symlink: err=%v, want errArchiveEntryChanged", err)
	}
	if bytes.Contains(content, []byte(f.secret)) {
		t.Fatal("the archive contains the link target")
	}
}

func TestArchivePackagingRefusesDirectorySwappedForOtherDirectory(t *testing.T) {
	f := newArchiveRaceFixture(t)
	other := filepath.Join(f.payload, "zz-other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	f.hook(t, func(dir, name string) {
		if name != "sub" {
			return
		}
		target := filepath.Join(dir, name)
		if err := os.RemoveAll(target); err != nil {
			t.Error(err)
		}
		// A link to another directory inside the tree stays within the root, so only the
		// identity check on the opened directory catches it.
		if err := os.Symlink(other, target); err != nil {
			t.Error(err)
		}
	})
	if err, _ := f.pack(t); !errors.Is(err, errArchiveEntryChanged) {
		t.Fatalf("packaging a directory swapped for an in-tree link: err=%v", err)
	}
}

func TestArchivePackagingRefusesSizeChangeAfterCheck(t *testing.T) {
	f := newArchiveRaceFixture(t)
	f.hook(t, func(dir, name string) {
		if name != "plain.txt" {
			return
		}
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = file.WriteString("more")
		_ = file.Close()
	})
	if err, _ := f.pack(t); !errors.Is(err, errArchiveEntryChanged) {
		t.Fatalf("packaging a file whose size changed: err=%v", err)
	}
}

func TestArchivePackagingSkipsPlantedLinkAndCountsIt(t *testing.T) {
	f := newArchiveRaceFixture(t)
	if err := os.Symlink(f.secretFile, filepath.Join(f.payload, "planted-file")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(f.secretDir, filepath.Join(f.payload, "planted-dir")); err != nil {
		t.Fatal(err)
	}
	err, content := f.pack(t)
	if err != nil {
		t.Fatalf("planted links must be skipped, not fail the package: %v", err)
	}
	if bytes.Contains(content, []byte(f.secret)) {
		t.Fatal("the archive contains the link target")
	}
	var manifest pgdb.TaskArchiveSnapshot
	raw, err2 := os.ReadFile(filepath.Join(f.base, "restored", "manifest.json"))
	if err2 != nil {
		t.Fatal(err2)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if got := manifest.DataCounts["skipped_entries"]; got != 2 {
		t.Fatalf("manifest skipped_entries=%d, want 2", got)
	}
}

func TestArchiveRestoreRefusesSymlinkedDestinationComponent(t *testing.T) {
	dataDir := t.TempDir()
	outside := t.TempDir()
	extracted := filepath.Join(dataDir, "archives", "tasks", ".restore", "11-test")
	source := filepath.Join(extracted, "files", "tasks", "42", "evidence.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "tasks")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := installTaskArchiveFiles(dataDir, extracted, "42", 11); err == nil {
		t.Fatal("restore followed a symlinked destination component")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("restore wrote through the link: %d entries", len(entries))
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("the extracted source must stay in place after a refused restore: %v", err)
	}
}

func TestArchivePackagingSkipsHardLinkedFileAndCountsIt(t *testing.T) {
	f := newArchiveRaceFixture(t)
	if err := os.Link(f.secretFile, filepath.Join(f.payload, "hard-link")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if links, ok := archiveLinkCount(mustLstat(t, f.secretFile)); !ok {
		t.Skip("link count not available on this platform")
	} else if links != 2 {
		t.Fatalf("fixture link count=%d want 2", links)
	}
	err, content := f.pack(t)
	if err != nil {
		t.Fatalf("a hard-linked file must be skipped, not fail the package: %v", err)
	}
	if bytes.Contains(content, []byte(f.secret)) {
		t.Fatal("the archive contains the content of the hard-linked file")
	}
	if !bytes.Contains(content, []byte("inner")) {
		t.Fatal("the other files were not archived")
	}
	if _, err := os.Lstat(filepath.Join(f.base, "restored", "hard-link")); !os.IsNotExist(err) {
		t.Fatalf("the hard-linked file was archived: %v", err)
	}
	var manifest pgdb.TaskArchiveSnapshot
	raw, err2 := os.ReadFile(filepath.Join(f.base, "restored", "manifest.json"))
	if err2 != nil {
		t.Fatal(err2)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if got := manifest.DataCounts["skipped_entries"]; got != 1 {
		t.Fatalf("manifest skipped_entries=%d, want 1", got)
	}
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

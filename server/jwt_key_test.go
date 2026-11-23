package server

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func captureServerLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// AC18 (H5): a key file in the data directory is never adopted as the signing key. It is renamed
// out of the way, root-only, and the neutral warning is logged. The key is generated fresh.
func TestLegacyKeyFileInTheDataDirectoryIsNotAdopted(t *testing.T) {
	keyDir, dataDir := t.TempDir(), t.TempDir()
	planted := "PLANTED-0123456789-0123456789-0123456789"
	legacy := filepath.Join(dataDir, jwtKeyFilename)
	if err := os.WriteFile(legacy, []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := captureServerLog(t)

	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if string(key) == planted || len(key) < 32 {
		t.Fatalf("the planted key was adopted (or the key is short): %q", key)
	}
	if b, err := os.ReadFile(filepath.Join(keyDir, jwtKeyFilename)); err != nil || string(b) != string(key) {
		t.Fatalf("the generated key was not persisted: %v", err)
	}
	if _, err := os.Lstat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the planted file is still at its name: %v", err)
	}
	ents, _ := filepath.Glob(filepath.Join(dataDir, jwtKeyFilename+".ignored-*"))
	if len(ents) != 1 {
		t.Fatalf("renamed copies: %v", ents)
	}
	fi, err := os.Stat(ents[0])
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the renamed file: %v %v, want mode 0600", fi, err)
	}
	if got := logs.String(); !strings.Contains(got, "[auth] ignored a legacy key file in the data directory") {
		t.Errorf("no warning in the log:\n%s", got)
	}
	if strings.Contains(logs.String(), planted) {
		t.Error("the log carries the planted key")
	}
}

// An existing key at the key directory stays, and the planted file is set aside all the same.
func TestLegacyKeyFileIsSetAsideWhenTheKeyExists(t *testing.T) {
	keyDir, dataDir := t.TempDir(), t.TempDir()
	good := "GOODKEY-0123456789-0123456789-0123456789"
	if err := os.WriteFile(filepath.Join(keyDir, jwtKeyFilename), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, jwtKeyFilename), []byte("PLANTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureServerLog(t)
	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil || string(key) != good {
		t.Fatalf("key %q err %v", key, err)
	}
	if ents, _ := filepath.Glob(filepath.Join(dataDir, jwtKeyFilename+".ignored-*")); len(ents) != 1 {
		t.Fatalf("renamed copies: %v", ents)
	}
}

// A link in the data directory under the key's name is set aside as a link, never followed.
func TestLegacyKeyLinkIsNeverFollowed(t *testing.T) {
	keyDir, dataDir, other := t.TempDir(), t.TempDir(), t.TempDir()
	target := filepath.Join(other, "secret")
	if err := os.WriteFile(target, []byte("TARGET-0123456789-0123456789-0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dataDir, jwtKeyFilename)); err != nil {
		t.Fatal(err)
	}
	captureServerLog(t)
	key, err := loadOrCreateJWTKey(keyDir, dataDir)
	if err != nil || strings.HasPrefix(string(key), "TARGET") {
		t.Fatalf("key %q err %v", key, err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("the link target changed: %v %v", fi, err)
	}
}

// With nothing planted nothing is logged about a legacy file.
func TestNoLegacyKeyNoWarning(t *testing.T) {
	logs := captureServerLog(t)
	if _, err := loadOrCreateJWTKey(t.TempDir(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "legacy") {
		t.Errorf("unexpected warning:\n%s", logs.String())
	}
}

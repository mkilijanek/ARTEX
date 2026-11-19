package dbtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The TestMain of this package called MustIsolateFromConfigFile. The configuration file it
// names must not exist and must lie in a private directory of this process, not at a fixed
// path in the shared temporary directory.
func TestUpgradeConfigIsolationUsesAPrivateDirectory(t *testing.T) {
	path := os.Getenv(EnvConfig)
	if path == "" {
		t.Fatalf("%s is not set: the package TestMain did not isolate the tests from the config file", EnvConfig)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the isolation config file must not exist (stat error: %v)", err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the private directory is missing: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("the isolation directory has mode %v, want a directory with 0700", info.Mode())
	}
	if base := filepath.Base(dir); base == "artex-tests-no-config" || !strings.HasPrefix(base, "artex-tests-no-config-") {
		t.Errorf("the isolation directory %q is a fixed name, not a private per-process one", base)
	}
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) {
		t.Errorf("the isolation directory is not directly under the temporary directory")
	}
}

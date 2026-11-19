package db

import (
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// Backlog B-1. With ARTEX_PG_DSN unset, DSN() falls back to the config file, which
// is the operator's own database, and SuffixTestDatabase is a no-op. The package
// TestMain therefore isolates the tests from the config file (dbtest.
// MustIsolateFromConfigFile). The test re-executes this test binary with no
// ARTEX_PG_DSN and a config file whose database.dsn points at a listener of the
// test (the marker): an existing database test must skip, as on a host with no
// configuration, and nothing may dial the marker.
func TestConfigFileDSNIsNotUsedByTestsWhenEnvironmentVariableIsUnset(t *testing.T) {
	res := dbtest.RunChild(t, "TestOpenSeed")
	if res.Dials != 0 {
		t.Fatalf("the test binary connected %d time(s) to the database named in the config file\n%s", res.Dials, res.Output)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "--- SKIP: TestOpenSeed") {
		t.Fatalf("want TestOpenSeed to skip without a database config (exit %d)\n%s", res.ExitCode, res.Output)
	}
}

// AC16: with ARTEX_PG_DSN unset the new tests fail; they do not skip and they do not
// fall back to the config file (the marker is never dialled).
func TestUpgradeNewTestsFailWithoutEnvironmentVariable(t *testing.T) {
	res := dbtest.RunChild(t, "TestUpgradeFreshDatabaseMatchesReference")
	if res.Dials != 0 {
		t.Fatalf("the test binary connected %d time(s) to the database named in the config file\n%s", res.Dials, res.Output)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Output, "--- FAIL: TestUpgradeFreshDatabaseMatchesReference") ||
		!strings.Contains(res.Output, dbtest.EnvDSN+" is not set") {
		t.Fatalf("want the test to fail naming %s (exit %d)\n%s", dbtest.EnvDSN, res.ExitCode, res.Output)
	}
}

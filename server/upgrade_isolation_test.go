package server

import (
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// Backlog B-1: with ARTEX_PG_DSN unset the tests must not use the database named in
// the config file. See db/upgrade_isolation_test.go; here the existing test
// TestFactSourceSchemaIdempotent goes through db.DSN() and has to skip.
func TestConfigFileDSNIsNotUsedByTestsWhenEnvironmentVariableIsUnset(t *testing.T) {
	res := dbtest.RunChild(t, "TestFactSourceSchemaIdempotent")
	if res.Dials != 0 {
		t.Fatalf("the test binary connected %d time(s) to the database named in the config file\n%s", res.Dials, res.Output)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "--- SKIP: TestFactSourceSchemaIdempotent") {
		t.Fatalf("want TestFactSourceSchemaIdempotent to skip without a database config (exit %d)\n%s", res.ExitCode, res.Output)
	}
}

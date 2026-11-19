package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// AC17 (docs/phase-d2-stage2-plan.md): a migration failure stops the server. db.Open runs the llm
// migrations itself, so NewManager no longer calls the Ensure wrappers (where a failure was only
// logged), and the error of Open comes back from the constructor.
func TestNewManagerReturnsMigrationFailure(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "mig_manager")
	// A ledger table without rows: the runner refuses it before it writes anything.
	tmp.Load(t, "empty ledger", `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, checksum TEXT, applied_at TIMESTAMPTZ DEFAULT now());`)
	t.Setenv("ARTEX_PG_DSN", tmp.DSN)

	m, err := NewManager(t.TempDir(), "")
	if err == nil {
		t.Fatal("NewManager succeeded although the migration runner refuses the database")
	}
	if m != nil {
		t.Error("NewManager returned a manager together with an error")
	}
	if !strings.Contains(err.Error(), "ledger was edited") {
		t.Errorf("error %q does not carry the runner's refusal", err)
	}
}

func TestNewManagerDoesNotCallTheEnsureWrappers(t *testing.T) {
	src, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatal(err)
	}
	if loc := regexp.MustCompile(`EnsureLLM\w*Table`).FindIndex(src); loc != nil {
		t.Errorf("server/manager.go still calls %s: a failed llm migration must fail db.Open, not be logged", src[loc[0]:loc[1]])
	}
}

package llmrec

import (
	"os"
	"testing"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Run against this package's own database (db.SuffixTestDatabase, #80): the
// recorder tests require an isolated ARTEX_PG_DSN, and the suffix gives them
// one even inside a full parallel ./... run.
func TestMain(m *testing.M) {
	// Backlog B-1: no test of this package may reach a database named in the config
	// file; they read ARTEX_PG_DSN only, and this keeps it that way.
	dbtest.MustIsolateFromConfigFile()
	db.SuffixTestDatabase("llmrec")
	os.Exit(m.Run())
}

package evidence

import (
	"context"
	"os"
	"testing"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// Initialize an explicitly configured fresh database before taking the same
// suite lock as db, agent and server. Hold it on one pinned connection.
func TestMain(m *testing.M) {
	// Backlog B-1: no test of this package may reach a database named in the config
	// file; they read ARTEX_PG_DSN only, and this keeps it that way.
	dbtest.MustIsolateFromConfigFile()
	// Own database per package (#80): stop contending on the shared tables of
	// a single test database during parallel ./... runs.
	db.SuffixTestDatabase("evidence")
	if os.Getenv("ARTEX_PG_DSN") == "" {
		os.Exit(m.Run())
	}
	os.Exit(runEvidenceSuite(m))
}
func runEvidenceSuite(m *testing.M) int {
	pg, err := db.Open(os.Getenv("ARTEX_PG_DSN"))
	if err != nil {
		panic(err)
	}
	defer pg.Close()
	conn, err := pg.Conn(context.Background())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), `SELECT pg_advisory_lock(7337741002)`); err != nil {
		panic(err)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(7337741002)`)
	return m.Run()
}

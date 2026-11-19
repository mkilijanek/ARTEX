package db

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mkilijanek/artex/db/dbtest"
)

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// db test suite. Packages agent and server hold the same lock, so parallel
// `go test ./...` runs serialize across packages on the shared dev DB and
// avoid cross-package cleanup races (e.g. DELETE FROM assets WHERE id > X
// from one package deleting assets created by another).
func TestMain(m *testing.M) {
	// Backlog B-1: with ARTEX_PG_DSN unset, DSN() would fall back to the config
	// file, which names the operator's own database. Tests must never use it.
	dbtest.MustIsolateFromConfigFile()
	code := runSuite(m)
	// A temporary database of the upgrade tests left behind is a failure (AC11).
	if err := dbtest.CheckNoLeftovers(); err != nil {
		fmt.Fprintln(os.Stderr, "db tests:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func runSuite(m *testing.M) int {
	// Own database per package (#80): with separate databases, packages stop
	// contending on shared tables and the advisory lock below stops
	// serializing go test ./... across packages.
	SuffixTestDatabase("db")
	dsn, _, err := DSN()
	if err != nil {
		// No DB configured — tests that need PG will skip themselves.
		return m.Run()
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		return m.Run()
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		return m.Run()
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	return m.Run()
}

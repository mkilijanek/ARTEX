package server

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
	"github.com/mkilijanek/artex/sandbox"
)

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// server test suite so cross-package DELETE cleanup races with db/agent
// packages are avoided when running `go test ./...`.
func TestMain(m *testing.M) {
	// Skill snapshots go to a temporary tree, not /var/lib/artex/skills.
	snap, err := os.MkdirTemp("", "artex-skill-snap-")
	if err != nil {
		panic(err)
	}
	sandbox.SkillSnapshotRoot = snap
	// Backlog B-1: with ARTEX_PG_DSN unset, db.DSN() would fall back to the config
	// file, which names the operator's own database. Tests must never use it.
	dbtest.MustIsolateFromConfigFile()
	// Own database per package (#80): with separate databases, packages stop
	// contending on shared tables and the advisory lock below stops
	// serializing go test ./... across packages.
	db.SuffixTestDatabase("server")
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	os.Exit(m.Run())
}

// testDSN resolves the connection string the way the binary's main does and hands it to
// NewManager; NewManager no longer reads the environment itself. Empty when none is configured,
// which NewManager reports as an error and the tests skip on.
func testDSN() string {
	dsn, _, _ := db.DSN()
	return dsn
}

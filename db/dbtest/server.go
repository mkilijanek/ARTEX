package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

// Server is the PostgreSQL server named by ARTEX_PG_DSN. It creates, loads and
// drops only the temporary databases of its package database, and runs Guard
// before each of those steps.
type Server struct {
	dsn  string
	base string // parsed database name; temporary databases are <base>_upg_<case>
}

// FromEnv resolves the server from ARTEX_PG_DSN. It fails, and never falls back to
// another source, when the variable is unset or the guard refuses the DSN.
func FromEnv() (*Server, error) {
	dsn := envDSN()
	if err := Guard(OpConnect, dsn, ""); err != nil {
		return nil, err
	}
	name, err := ParsedDatabase(dsn)
	if err != nil {
		return nil, err
	}
	return &Server{dsn: dsn, base: name}, nil
}

// BaseName is the package database name every temporary database name starts with.
func (s *Server) BaseName() string { return s.base }

// TempName is the full name of the temporary database for a case label.
func (s *Server) TempName(label string) string { return s.base + TempMarker + label }

// deriveDSN returns base with the database replaced by name and verifies, by
// parsing the result, that the driver would connect to exactly that database.
func deriveDSN(base, name string) (string, error) {
	var out string
	if isURL(base) {
		u, err := url.Parse(base)
		if err != nil {
			return "", errors.New("DSN cannot be parsed")
		}
		u.Path = "/" + name
		u.RawPath = ""
		out = u.String()
	} else {
		// In a key-value DSN a later key wins.
		out = base + " dbname=" + name
	}
	got, err := ParsedDatabase(out)
	if err != nil || got != name {
		return "", fmt.Errorf("cannot derive a DSN for database %q", name)
	}
	return out, nil
}

// DSN returns the connection string of a temporary database, after the guard has
// accepted a load into it. Callers that hand the string to db.Open use this.
func (s *Server) DSN(name string) (string, error) {
	if err := Guard(OpLoad, s.dsn, name); err != nil {
		return "", err
	}
	return deriveDSN(s.dsn, name)
}

// sqlState reduces a driver error to its SQLSTATE: driver messages can carry the
// user name, host, database and statement text.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "SQLSTATE " + pgErr.Code
	}
	return "no SQLSTATE: connection or driver error"
}

func (s *Server) admin() (*sql.DB, error) {
	dsn, err := deriveDSN(s.dsn, "postgres")
	if err != nil {
		return nil, err
	}
	return sql.Open("pgx", dsn)
}

func (s *Server) execAdmin(ctx context.Context, op Op, name, stmt string) error {
	admin, err := s.admin()
	if err != nil {
		return err
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("%s %q failed (%s)", op, name, sqlState(err))
	}
	return nil
}

// CreateDatabase creates the temporary database <base>_upg_<label>. name is the
// full name, as TempName returns it.
func (s *Server) CreateDatabase(ctx context.Context, name string) error {
	if err := Guard(OpCreate, s.dsn, name); err != nil {
		return err
	}
	return s.execAdmin(ctx, OpCreate, name, `CREATE DATABASE `+quoteIdent(name))
}

// dropRetryWindow bounds how long DropDatabase waits for a session it may not terminate.
const dropRetryWindow = 30 * time.Second

// DropDatabase drops a temporary database, terminating its sessions. A database that a canary role
// owns is dropped by that role, which may not terminate a session of the server user: an autovacuum
// worker (a superuser session) on the database makes the statement fail with SQLSTATE 42501 until
// the worker ends, which takes moments. DropDatabase repeats the statement for that SQLSTATE only,
// for dropRetryWindow; any other error, and a 42501 that stays, is returned.
func (s *Server) DropDatabase(ctx context.Context, name string) error {
	if err := Guard(OpDrop, s.dsn, name); err != nil {
		return err
	}
	deadline := time.Now().Add(dropRetryWindow)
	for {
		err := s.execAdmin(ctx, OpDrop, name, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`)
		if err == nil || !strings.Contains(err.Error(), "SQLSTATE 42501") || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// LoadSQL executes sqlText in a temporary database as one simple-protocol query
// (an implicit transaction), as db.Open does with schema.sql.
func (s *Server) LoadSQL(ctx context.Context, name, sqlText string) error {
	if err := Guard(OpLoad, s.dsn, name); err != nil {
		return err
	}
	dsn, err := deriveDSN(s.dsn, name)
	if err != nil {
		return err
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("%s %q failed (%s)", OpLoad, name, sqlState(err))
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("%s %q failed (%s)", OpLoad, name, sqlState(err))
	}
	return nil
}

// OpenTemp opens a pool on a temporary database for queries and digests.
func (s *Server) OpenTemp(name string) (*sql.DB, error) {
	dsn, err := s.DSN(name)
	if err != nil {
		return nil, err
	}
	return sql.Open("pgx", dsn)
}

// Leftovers lists the temporary databases of this server's package that exist now.
func (s *Server) Leftovers(ctx context.Context) ([]string, error) {
	if err := Guard(OpConnect, s.dsn, ""); err != nil {
		return nil, err
	}
	admin, err := s.admin()
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	rows, err := admin.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE left(datname, length($1::text)) = $1::text ORDER BY datname`,
		s.base+TempMarker)
	if err != nil {
		return nil, fmt.Errorf("list temporary databases failed (%s)", sqlState(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list temporary databases: scan failed")
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list temporary databases failed (%s)", sqlState(err))
	}
	return out, nil
}

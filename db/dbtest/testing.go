package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// RequireServer resolves the server from ARTEX_PG_DSN or fails the test. It never
// skips: a missing variable or a refused DSN is a failure, so a green run means
// the database tests really ran (AC11, AC16).
func RequireServer(t testing.TB) *Server {
	t.Helper()
	srv, err := FromEnv()
	if err != nil {
		t.Fatalf("%v\nset %s to a test PostgreSQL server (see docs/schema-upgrade.md); these tests never read the config file", err, EnvDSN)
	}
	return srv
}

// Temp is a temporary database created by TempDatabase.
type Temp struct {
	Name string
	DSN  string // for db.Open
	srv  *Server
}

// timeout bounds one administrative statement.
const timeout = 2 * time.Minute

// TempDatabase creates <base>_upg_<label>, dropping a stale database of that name
// first, and drops it when the test ends. A failed drop fails the test.
func TempDatabase(t testing.TB, srv *Server, label string) *Temp {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	name := srv.TempName(label)
	if err := srv.DropDatabase(ctx, name); err != nil {
		t.Fatalf("drop stale temporary database: %v", err)
	}
	if err := srv.CreateDatabase(ctx, name); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := srv.DropDatabase(ctx, name); err != nil {
			t.Errorf("drop temporary database: %v", err)
		}
	})
	dsn, err := srv.DSN(name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return &Temp{Name: name, DSN: dsn, srv: srv}
}

// Load executes fixture text in the temporary database; failures name the
// database and the SQLSTATE only.
func (tmp *Temp) Load(t testing.TB, what, sqlText string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := tmp.srv.LoadSQL(ctx, tmp.Name, sqlText); err != nil {
		t.Fatalf("load %s: %v", what, err)
	}
}

// Conn opens a pool on the temporary database; it is closed when the test ends.
func (tmp *Temp) Conn(t testing.TB) *sql.DB {
	t.Helper()
	conn, err := tmp.srv.OpenTemp(tmp.Name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Catalog lists the schema objects of the temporary database.
func (tmp *Temp) Catalog(t testing.TB, q Queryer) []Entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	entries, err := Catalog(ctx, q)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return entries
}

// CheckNoLeftovers is for a package TestMain, after m.Run: it reports a
// temporary database that a test left behind (AC11). With ARTEX_PG_DSN unset it
// does nothing, and when the guard refuses the DSN it does nothing either (the
// new tests report that themselves).
func CheckNoLeftovers() error {
	srv, err := FromEnv()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	left, err := srv.Leftovers(ctx)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("temporary databases left behind: %s", strings.Join(left, ", "))
	}
	return nil
}

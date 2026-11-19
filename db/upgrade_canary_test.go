package db

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// AC12: the PostgreSQL role below has a random password made for this run. It is registered
// with the secret check (dbtest.RegisterSecret), so that neither it nor a row canary may
// appear in a log line, an error from a failed connection, a guard refusal or the failure
// message of a comparison, on the paths S1 covers: the guard, the temporary-database
// helpers, Open (with a wrong password) and the comparisons. The fixed password canary
// appears only as the wrong password of the failed logins below; it is no credential.
// Later slices reuse dbtest.AssertNoSecret and dbtest.CaptureLog for their own output.
//
// The role is server-wide, so the test makes it with a random name suffix and a short
// VALID UNTIL, drops only that role, and dbtest.CheckNoLeftovers reports one that stays.
func TestUpgradeCanaryPasswordDoesNotReachOutput(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	superDSN := os.Getenv(dbtest.EnvDSN)
	u, err := url.Parse(superDSN)
	if err != nil || u.Scheme == "" {
		t.Fatalf("this test needs a URL-form %s", dbtest.EnvDSN)
	}
	super := dbtest.RequireServer(t) // runs the guard before the admin connection of the next line
	role := super.CreateCanaryRole(t)
	ctx := context.Background()

	roleURL := *u
	roleURL.User = url.UserPassword(role.Name, role.Password)
	t.Setenv(dbtest.EnvDSN, roleURL.String())
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "canarypw")
	d := startup(t, tmp.DSN)
	if _, err := d.ExecContext(ctx, `INSERT INTO companies(name, nkey) VALUES ($1, $1)`, dbtest.RowCanary+"-company"); err != nil {
		t.Fatalf("insert canary row: %v", err)
	}
	entries := tmp.Catalog(t, d.DB)
	dbtest.AssertNoSecret(t, "catalog listing", strings.Join(dbtest.Lines(entries), "\n"))

	// A wrong password: Open prints a ping error, the helpers reduce errors to SQLSTATE.
	wrong := roleURL
	wrong.User = url.UserPassword(role.Name, role.Password+"-wrong")
	if _, err := Open(wrong.String()); err == nil {
		t.Fatal("Open accepted a wrong password")
	} else {
		dbtest.AssertNoSecret(t, "Open error for a wrong password", err.Error())
	}
	t.Setenv(dbtest.EnvDSN, wrong.String())
	wrongSrv := dbtest.RequireServer(t)
	if err := wrongSrv.CreateDatabase(ctx, wrongSrv.TempName("neverexists")); err == nil {
		t.Fatal("CreateDatabase accepted a wrong password")
	} else {
		dbtest.AssertNoSecret(t, "CreateDatabase error for a wrong password", err.Error())
	}
	// A guard refusal for a DSN that carries the fixed password canary (no credential).
	fixed := roleURL
	fixed.User = url.UserPassword(role.Name, dbtest.PasswordCanary)
	if err := dbtest.Guard(dbtest.OpDrop, fixed.String(), "operator_prod"); err == nil {
		t.Fatal("guard accepted a foreign target")
	} else {
		dbtest.AssertNoSecret(t, "guard refusal", err.Error())
	}
	dbtest.AssertNoSecret(t, "log output", logs())
}

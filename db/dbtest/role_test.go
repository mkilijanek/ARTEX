package dbtest

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func countRows(t *testing.T, conn *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query failed (%s)", sqlState(err))
	}
	return n
}

// A canary role has a random name suffix, a random registered password and an expiry a few
// minutes ahead, and the leftover check reports it as long as it exists (AC11, AC12).
func TestUpgradeCanaryRoleIsRandomExpiringAndReportedAsLeftover(t *testing.T) {
	srv := RequireServer(t)
	ctx := context.Background()
	admin, err := srv.admin()
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	var name, password string
	t.Run("role exists", func(t *testing.T) {
		a, b := srv.CreateCanaryRole(t), srv.CreateCanaryRole(t)
		name, password = a.Name, a.Password
		if a.Name == b.Name || a.Password == b.Password {
			t.Fatal("two roles share a name or a password: they are not random")
		}
		if a.Password == PasswordCanary || len(a.Password) < 32 {
			t.Fatalf("the role password is not a random per-run value (%d bytes)", len(a.Password))
		}
		if !srv.roleNameOK(a.Name) {
			t.Fatalf("the role name %q is not %s<8 hex>", a.Name, srv.rolePrefix())
		}
		if kind, found := FindSecret("x " + a.Password + " y"); !found || kind != "canary role password" {
			t.Errorf("the role password is not registered as a secret (found %v, kind %q)", found, kind)
		}
		until := countRows(t, admin, `SELECT count(*) FROM pg_roles WHERE rolname = $1 AND rolcanlogin AND rolcreatedb
			AND NOT rolsuper AND rolvaliduntil > now() AND rolvaliduntil < now() + interval '30 minutes'`, a.Name)
		if until != 1 {
			t.Errorf("role %s is not a LOGIN CREATEDB role that expires within 30 minutes", a.Name)
		}
		// The leftover check sees both roles while they exist.
		left, err := srv.LeftoverRoles(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(left, " "), a.Name) || !strings.Contains(strings.Join(left, " "), b.Name) {
			t.Errorf("LeftoverRoles does not list the roles: %v", left)
		}
		if err := CheckNoLeftovers(); err == nil || !strings.Contains(err.Error(), "canary roles left behind") || !strings.Contains(err.Error(), a.Name) {
			t.Errorf("CheckNoLeftovers did not report the role: %v", err)
		}
	})
	// The subtest's cleanup dropped both roles and removed the secret.
	if n := countRows(t, admin, `SELECT count(*) FROM pg_roles WHERE rolname = $1`, name); n != 0 {
		t.Errorf("the canary role %s was not dropped when its test ended", name)
	}
	if _, found := FindSecret(password); found {
		t.Error("the role password is still registered after the test")
	}
	if err := CheckNoLeftovers(); err != nil {
		t.Errorf("a clean server was reported: %v", err)
	}
}

// A role the run did not create is never dropped: a stale role of this package's naming that
// another run made survives, and is what CheckNoLeftovers reports.
func TestUpgradeCanaryRoleCleanupLeavesForeignRolesAlone(t *testing.T) {
	srv := RequireServer(t)
	admin, err := srv.admin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() }) // runs after the cleanup below, which uses it
	suffix, err := randomHex(4)
	if err != nil {
		t.Fatal(err)
	}
	foreign := srv.rolePrefix() + suffix // another run's role: this test did not create it through CreateCanaryRole
	if _, err := admin.Exec(`CREATE ROLE ` + quoteIdent(foreign) + ` NOLOGIN`); err != nil {
		t.Fatalf("create the foreign role failed (%s)", sqlState(err))
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP ROLE IF EXISTS ` + quoteIdent(foreign)); err != nil {
			t.Errorf("drop the foreign role failed (%s)", sqlState(err))
		}
	})
	t.Run("role", func(t *testing.T) { srv.CreateCanaryRole(t) })
	if n := countRows(t, admin, `SELECT count(*) FROM pg_roles WHERE rolname = $1`, foreign); n != 1 {
		t.Fatal("the cleanup of a canary role dropped a role it did not create")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	left, err := srv.LeftoverRoles(ctx)
	if err != nil || len(left) != 1 || left[0] != foreign {
		t.Errorf("LeftoverRoles = %v, %v; want only the foreign role", left, err)
	}
}

package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// RoleMarker follows the package database name in the name of a canary role:
// <package database>_upgrole_<8 hex>. Roles are server-wide, so the name carries the package
// database: a run of another package, or another base database, is never mistaken for this one.
const RoleMarker = "_upgrole_"

// roleValidity is how long a canary role may log in. A run that dies before its cleanup leaves
// a role that cannot be used a few minutes later; CheckNoLeftovers reports it either way.
const roleValidity = "10 minutes"

var roleRandom = regexp.MustCompile(`^[0-9a-f]{8}$`)

func (s *Server) rolePrefix() string { return s.base + RoleMarker }

// roleNameOK reports whether name is a role this package creates for this server's package
// database: the prefix, then 8 lower-case hex digits and nothing else.
func (s *Server) roleNameOK(name string) bool {
	rest, ok := strings.CutPrefix(name, s.rolePrefix())
	return ok && roleRandom.MatchString(rest)
}

func quoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CanaryRole is a login role that exists for one test only. Its password is random, made for
// this run and registered as a secret (RegisterSecret): it is never a value from the
// repository, so the role is no credential an attacker could read from the source.
type CanaryRole struct {
	Name     string
	Password string
}

// CreateCanaryRole creates a LOGIN CREATEDB role with a random name suffix, a random password
// and VALID UNTIL a few minutes ahead, and drops that role, and only that role, when the test
// ends. It runs Guard before it connects to the server. It fails, never skips, when the server
// user of ARTEX_PG_DSN may not create roles.
func (s *Server) CreateCanaryRole(t testing.TB) *CanaryRole {
	t.Helper()
	if err := Guard(OpCreateRole, s.dsn, ""); err != nil {
		t.Fatalf("%v", err)
	}
	suffix, err := randomHex(4)
	if err != nil {
		t.Fatalf("%v", err)
	}
	password, err := randomHex(16)
	if err != nil {
		t.Fatalf("%v", err)
	}
	name := s.rolePrefix() + suffix
	if len(name) > maxIdentifier || !s.roleNameOK(name) {
		t.Fatalf("canary role name %q is not a valid role name of this package (limit %d bytes)", name, maxIdentifier)
	}
	remove, err := RegisterSecret("canary role password", password)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(remove)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	admin, err := s.admin()
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer admin.Close()
	// The server's clock decides when the role expires; the text is a timestamp the server made.
	var until string
	if err := admin.QueryRowContext(ctx, `SELECT (now() + interval '`+roleValidity+`')::text`).Scan(&until); err != nil {
		t.Fatalf("read the server clock failed (%s)", sqlState(err))
	}
	// CREATE ROLE without IF NOT EXISTS: a name that is taken fails, and this run drops only a
	// role it created.
	stmt := `CREATE ROLE ` + quoteIdent(name) + ` LOGIN CREATEDB PASSWORD ` + quoteLiteral(password) + ` VALID UNTIL ` + quoteLiteral(until)
	if _, err := admin.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("create role %q failed (%s); the server user of %s needs CREATEROLE", name, sqlState(err), EnvDSN)
	}
	t.Cleanup(func() {
		if err := s.dropRole(name); err != nil {
			t.Errorf("%v", err)
		}
	})
	return &CanaryRole{Name: name, Password: password}
}

// dropRole drops a role of this package's naming only.
func (s *Server) dropRole(name string) error {
	if err := Guard(OpDropRole, s.dsn, ""); err != nil {
		return err
	}
	if !s.roleNameOK(name) {
		return fmt.Errorf("refusing to drop role %q: it is not %s<8 hex>", name, s.rolePrefix())
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.execAdmin(ctx, OpDropRole, name, `DROP ROLE `+quoteIdent(name))
}

// LeftoverRoles lists the canary roles of this server's package database that exist now.
func (s *Server) LeftoverRoles(ctx context.Context) ([]string, error) {
	if err := Guard(OpListRoles, s.dsn, ""); err != nil {
		return nil, err
	}
	admin, err := s.admin()
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	rows, err := admin.QueryContext(ctx,
		`SELECT rolname FROM pg_roles WHERE left(rolname, length($1::text)) = $1::text ORDER BY rolname`,
		s.rolePrefix())
	if err != nil {
		return nil, fmt.Errorf("list canary roles failed (%s)", sqlState(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list canary roles: scan failed")
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list canary roles failed (%s)", sqlState(err))
	}
	return out, nil
}

// dropRoleIfExists drops a role of this package's naming when it exists. A role that a test never got to
// create (a refused provisioning) is not an error.
func (s *Server) dropRoleIfExists(name string) error {
	if err := Guard(OpDropRole, s.dsn, ""); err != nil {
		return err
	}
	if !s.roleNameOK(name) {
		return fmt.Errorf("refusing to drop role %q: it is not %s<8 hex>", name, s.rolePrefix())
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.execAdmin(ctx, OpDropRole, name, `DROP ROLE IF EXISTS `+quoteIdent(name))
}

// DSNFor is the DSN of the temporary database for another user: the endpoint and the database of tmp.DSN, with
// the user and the password replaced.
func (tmp *Temp) DSNFor(user, password string) (string, error) {
	if !isURL(tmp.DSN) {
		return "", fmt.Errorf("the test DSN is not a URL: a DSN for another user cannot be derived")
	}
	u, err := url.Parse(tmp.DSN)
	if err != nil {
		return "", fmt.Errorf("DSN cannot be parsed")
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

// SidecarRole is the name and the password a test gives to the database role of the Signal sidecar. The role
// itself is created by the code under test.
type SidecarRole struct {
	Name     string
	Password string
}

// NewSidecarRole picks a role name that satisfies the guard (<package database>_upgrole_<8 hex>) and a random
// password, registered as a secret. When the test ends, the temporary database is dropped first (the role's grants
// live in it) and then the role, if the code under test created it. The leftover check covers the name.
func (s *Server) NewSidecarRole(t testing.TB, tmp *Temp) *SidecarRole {
	t.Helper()
	name, password := s.newRoleIdentity(t)
	t.Cleanup(func() { s.cleanupRole(t, tmp, name) })
	return &SidecarRole{Name: name, Password: password}
}

// CreateOwnerRole creates a LOGIN role that owns the temporary database tmp, with CREATEROLE when createRole is
// true, a random password and the name rule of the canary roles. It is how a test runs the server as a database
// user that is neither a superuser nor the bootstrap user. Cleanup as for NewSidecarRole.
func (s *Server) CreateOwnerRole(t testing.TB, tmp *Temp, createRole bool) *SidecarRole {
	t.Helper()
	if err := Guard(OpCreateRole, s.dsn, ""); err != nil {
		t.Fatalf("%v", err)
	}
	name, password := s.newRoleIdentity(t)
	t.Cleanup(func() { s.cleanupRole(t, tmp, name) })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	attrs := "NOCREATEROLE"
	if createRole {
		attrs = "CREATEROLE"
	}
	stmt := `CREATE ROLE ` + quoteIdent(name) + ` LOGIN ` + attrs + ` PASSWORD ` + quoteLiteral(password)
	if err := s.execAdmin(ctx, OpCreateRole, name, stmt); err != nil {
		t.Fatalf("%v", err)
	}
	if err := s.execAdmin(ctx, OpCreateRole, name, `ALTER DATABASE `+quoteIdent(tmp.Name)+` OWNER TO `+quoteIdent(name)); err != nil {
		t.Fatalf("%v", err)
	}
	return &SidecarRole{Name: name, Password: password}
}

func (s *Server) newRoleIdentity(t testing.TB) (name, password string) {
	t.Helper()
	suffix, err := randomHex(4)
	if err != nil {
		t.Fatalf("%v", err)
	}
	password, err = randomHex(16)
	if err != nil {
		t.Fatalf("%v", err)
	}
	name = s.rolePrefix() + suffix
	if len(name) > maxIdentifier || !s.roleNameOK(name) {
		t.Fatalf("role name %q is not a valid role name of this package (limit %d bytes)", name, maxIdentifier)
	}
	remove, err := RegisterSecret("test role password", password)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(remove)
	return name, password
}

func (s *Server) cleanupRole(t testing.TB, tmp *Temp, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.DropDatabase(ctx, tmp.Name); err != nil {
		t.Errorf("%v", err)
	}
	if err := s.dropRoleIfExists(name); err != nil {
		t.Errorf("%v", err)
	}
}

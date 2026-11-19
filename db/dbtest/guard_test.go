package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The guard tests need no database: every case is decided from the DSN text and
// the ARTEX_PG_DSN environment variable alone. The password in the DSNs below is
// the password canary (AC12): no refusal may repeat it.

const (
	urlBase = "postgres://upg:" + PasswordCanary + "@db.invalid:5432/artextest_db?sslmode=disable"
	kvBase  = "host=db.invalid user=upg password=" + PasswordCanary + " dbname=artextest_db sslmode=disable"
)

func mustRefuse(t *testing.T, err error, wantInReason string) *Refusal {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a *Refusal, got %v", err)
	}
	if !strings.Contains(r.Reason, wantInReason) {
		t.Fatalf("refusal reason %q does not mention %q", r.Reason, wantInReason)
	}
	msg := err.Error()
	for _, leak := range []string{PasswordCanary, "postgres://", "password=", "db.invalid"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("refusal message leaks %q: %s", leak, msg)
		}
	}
	return r
}

func TestUpgradeGuardRefusesWhenEnvironmentVariableIsUnset(t *testing.T) {
	t.Setenv(EnvDSN, "")
	// A config file must not be consulted: the guard has no other source.
	mustRefuse(t, Guard(OpCreate, urlBase, "artextest_db_upg_x"), EnvDSN)
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv must fail when the variable is unset")
	}
}

func TestUpgradeGuardRefusesDSNNotTakenFromEnvironment(t *testing.T) {
	t.Setenv(EnvDSN, urlBase)
	other := strings.Replace(urlBase, "artextest_db", "operator_db", 1)
	// Same package suffix and a valid target, but not the value of ARTEX_PG_DSN.
	r := mustRefuse(t, Guard(OpDrop, other, "operator_db_upg_x"), "not the value of")
	if r.Database != "operator_db" {
		t.Fatalf("refusal names %q, want the parsed database name", r.Database)
	}
}

func TestUpgradeGuardRefusesNameWithoutPackageSuffix(t *testing.T) {
	for _, base := range []string{
		"postgres://upg:" + PasswordCanary + "@db.invalid/artextest?sslmode=disable",
		"postgres://upg:" + PasswordCanary + "@db.invalid/postgres",
		"postgres://upg:" + PasswordCanary + "@db.invalid/artextest_database",
	} {
		t.Setenv(EnvDSN, base)
		mustRefuse(t, Guard(OpCreate, base, "artextest_upg_x"), "package suffix")
		mustRefuse(t, Guard(OpConnect, base, ""), "package suffix")
	}
}

func TestUpgradeGuardRefusesTargetWithoutMarkerOrBase(t *testing.T) {
	t.Setenv(EnvDSN, urlBase)
	for _, target := range []string{
		"artextest_db",         // the base itself, no marker
		"artextest_db_x",       // marker missing
		"artextest_db_upg_",    // empty case
		"operator_prod",        // another database entirely
		"other_db_upg_x",       // marker, but not this base
		"artextest_db_upg_X",   // upper case
		"artextest_db_upg_a-b", // outside [a-z0-9_]
		"artextest_db_upg_" + strings.Repeat("a", 60), // over 63 bytes
		`artextest_db_upg_x"; DROP DATABASE postgres; --`,
	} {
		for _, op := range []Op{OpCreate, OpLoad, OpDrop} {
			if err := Guard(op, urlBase, target); err == nil {
				t.Fatalf("%s of %q was accepted", op, target)
			}
		}
	}
	if err := Guard(OpDrop, urlBase, "artextest_db_upg_r0509"); err != nil {
		t.Fatalf("a well-formed temporary name was refused: %v", err)
	}
}

func TestUpgradeGuardRefusesURLPathThatDiffersFromParsedName(t *testing.T) {
	// pgconn lets a dbname query parameter win over the URL path; the guard must see
	// the database the driver would really use.
	base := "postgres://upg:" + PasswordCanary + "@db.invalid/artextest_db?dbname=operator_prod&sslmode=disable"
	t.Setenv(EnvDSN, base)
	r := mustRefuse(t, Guard(OpDrop, base, "artextest_db_upg_x"), "differs")
	if r.Database != "operator_prod" {
		t.Fatalf("refusal names %q, want the parsed name operator_prod", r.Database)
	}
}

func TestUpgradeGuardRefusesDbnameOverrideWithSuffixedPath(t *testing.T) {
	// The URL path is a valid test database, the override is not.
	base := "postgres://upg:" + PasswordCanary + "@db.invalid/artextest_server?database=operator_prod"
	t.Setenv(EnvDSN, base)
	if err := Guard(OpCreate, base, "artextest_server_upg_x"); err == nil {
		t.Fatal("override accepted")
	}
}

func TestUpgradeGuardRefusesKeyValueDSNNamingAnotherDatabase(t *testing.T) {
	base := "host=db.invalid user=upg password=" + PasswordCanary + " dbname=operator_prod"
	t.Setenv(EnvDSN, base)
	r := mustRefuse(t, Guard(OpDrop, base, "operator_prod_upg_x"), "package suffix")
	if r.Database != "operator_prod" {
		t.Fatalf("refusal names %q", r.Database)
	}
	// A target outside the suffixed base is refused for a key-value DSN too.
	t.Setenv(EnvDSN, kvBase)
	if err := Guard(OpDrop, kvBase, "operator_prod"); err == nil {
		t.Fatal("foreign target accepted for a key-value DSN")
	}
}

func TestUpgradeGuardAcceptsWellFormedURLAndKeyValueDSN(t *testing.T) {
	for _, base := range []string{urlBase, kvBase,
		"postgresql://upg@db.invalid/artextest_dbtest", "postgres://upg@db.invalid/x_server"} {
		t.Setenv(EnvDSN, base)
		name, err := ParsedDatabase(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []Op{OpCreate, OpLoad, OpDrop} {
			if err := Guard(op, base, name+"_upg_case1"); err != nil {
				t.Fatalf("%s: %v", op, err)
			}
		}
	}
}

func TestUpgradeGuardRefusesUnparseableDSNWithoutEchoingIt(t *testing.T) {
	bad := "postgres://upg:" + PasswordCanary + "@db.invalid:notaport/artextest_db"
	t.Setenv(EnvDSN, bad)
	mustRefuse(t, Guard(OpCreate, bad, "artextest_db_upg_x"), "parse")
}

// Every operation of Server runs the guard before it touches the server. The server
// below does not exist: reaching the network would return another error type.
func TestUpgradeServerOperationsAreGuardedBeforeAnyStatement(t *testing.T) {
	t.Setenv(EnvDSN, "postgres://upg:"+PasswordCanary+"@127.0.0.1:1/artextest_db?sslmode=disable&connect_timeout=1")
	srv, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The variable changes after resolution: from here every operation must refuse.
	t.Setenv(EnvDSN, "postgres://upg:"+PasswordCanary+"@127.0.0.1:1/operator_prod")
	var r *Refusal
	if err := srv.CreateDatabase(ctx, "case1"); !errors.As(err, &r) || r.Op != OpCreate {
		t.Fatalf("create: %v", err)
	}
	if err := srv.DropDatabase(ctx, "case1"); !errors.As(err, &r) || r.Op != OpDrop {
		t.Fatalf("drop: %v", err)
	}
	if err := srv.LoadSQL(ctx, "case1", "SELECT 1"); !errors.As(err, &r) || r.Op != OpLoad {
		t.Fatalf("load: %v", err)
	}
	if _, err := srv.DSN("case1"); !errors.As(err, &r) || r.Op != OpLoad {
		t.Fatalf("dsn: %v", err)
	}
}

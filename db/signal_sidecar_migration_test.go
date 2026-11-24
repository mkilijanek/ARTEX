package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mkilijanek/artex/db/dbtest"
)

// The guarded drop of migration 5004 (testdata/upgrade/rollback-drop-5004.sql) and the helpers the older recipe
// tests use to get past it: a database this release started has 5004 as its newest fork migration, so the recipes
// of the earlier migrations refuse it until 5004 is dropped.

const rollbackDropSignalSidecarFile = "rollback-drop-5004.sql"

// signalSidecarObjectTokens: every catalog line of migration 5004 carries this name.
const signalSidecarObjectToken = "signal_sidecar"

// referenceWithoutSignalSidecar removes the lines of the objects that migration 5004 creates.
func referenceWithoutSignalSidecar(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.Contains(l, signalSidecarObjectToken) {
			out = append(out, l)
		}
	}
	return out
}

// dropSignalSidecarMigration runs the recipe of 5004 with the default role name.
func dropSignalSidecarMigration(t *testing.T, d *DB) {
	t.Helper()
	if err := rollbackRunScript(t, d.DB, rollbackDropSignalSidecarFile); err != nil {
		t.Fatalf("the 5004 drop failed (%s)", dbtest.AtomicErrorSummary(err))
	}
}

// runSignalRecipe runs the recipe for a database whose sidecar role has a test name.
func runSignalRecipe(t *testing.T, d *DB, role string) error {
	t.Helper()
	script := strings.Replace(readUpgradeFile(t, rollbackDropSignalSidecarFile), "v_role    text := 'artex_signal'", "v_role    text := '"+role+"'", 1)
	if !strings.Contains(script, role) {
		t.Fatal("the role name is not in the recipe: the substitution found nothing")
	}
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.ExecContext(ctx, script)
	if err != nil {
		if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil {
			t.Fatalf("roll back the failed recipe failed (%s)", dbtest.AtomicErrorSummary(rbErr))
		}
	}
	return err
}

func databaseACL(t *testing.T, d *DB) string {
	t.Helper()
	var acl string
	if err := d.QueryRow(`SELECT coalesce(datacl, acldefault('d', datdba))::text FROM pg_database WHERE datname = current_database()`).Scan(&acl); err != nil {
		t.Fatal(err)
	}
	return acl
}

// The recipe removes the objects, the role and the ledger row, and gives TEMPORARY back to PUBLIC exactly when
// provisioning had taken it away: the database privileges end as they were before provisioning, in both variants.
func TestSignalSidecarDropRecipeRestoresTheDatabase(t *testing.T) {
	for _, variant := range []string{"PUBLIC held TEMPORARY", "PUBLIC did not hold TEMPORARY"} {
		t.Run(variant, func(t *testing.T) {
			d, tmp, srv := signalFresh(t, "sigdrop")
			role := srv.NewSidecarRole(t, tmp)
			if variant == "PUBLIC did not hold TEMPORARY" {
				signalExec(t, d, `REVOKE TEMPORARY ON DATABASE `+quoteSQLIdent(tmp.Name)+` FROM PUBLIC`)
			}
			aclBefore := databaseACL(t, d)
			if st := d.ProvisionSignalRole(context.Background(), signalCfg(tmp, role)); st.State != SignalRoleOK {
				t.Fatalf("provisioning: %+v", st)
			}
			s, err := d.ReadSignalSidecar(context.Background())
			wantFlag := variant == "PUBLIC held TEMPORARY"
			if err != nil || s.PublicTempRevoked != wantFlag {
				t.Fatalf("public_temp_revoked = %v (err %v), want %v", s.PublicTempRevoked, err, wantFlag)
			}
			if err := runSignalRecipe(t, d, role.Name); err != nil {
				t.Fatalf("the recipe failed (%s)", dbtest.AtomicErrorSummary(err))
			}
			if roleExists(t, d, role.Name) {
				t.Error("the role still exists after the recipe")
			}
			if after := databaseACL(t, d); after != aclBefore {
				t.Errorf("database privileges after the recipe %q, before provisioning %q", after, aclBefore)
			}
			requireLedgerOf(t, readLedger(t, d.DB), 1, 2, 3, 4, 5001, 5002, 5003)
			if diff := dbtest.DiffLines(referenceWithoutSignalSidecar(loadReference(t)), dbtest.Lines(tmp.Catalog(t, d.DB))); len(diff) > 0 {
				t.Errorf("after the recipe the catalog is not the reference without the objects of 5004:\n  %s", strings.Join(diff, "\n  "))
			}
			// A second run raises; the next start applies 5004 again.
			requireGuardRefusal(t, "a second run", runSignalRecipe(t, d, role.Name), "5004 is not the newest fork migration")
			d2 := startup(t, tmp.DSN)
			requireEmbeddedLedger(t, readLedger(t, d2.DB))
			if diff := dbtest.DiffLines(loadReference(t), dbtest.Lines(tmp.Catalog(t, d2.DB))); len(diff) > 0 {
				t.Errorf("after a start the catalog differs from the reference:\n  %s", strings.Join(diff, "\n  "))
			}
		})
	}
}

// The recipe changes nothing when the ledger is not the one it was written for, or when the role of that name was
// not made for the sidecar.
func TestSignalSidecarDropRecipeIsGuarded(t *testing.T) {
	d, tmp, srv := signalFresh(t, "sigdropguard")
	role := srv.NewSidecarRole(t, tmp)
	ctx := context.Background()

	signalExec(t, d, `UPDATE schema_migrations SET name = '5004_renamed.sql' WHERE version = 5004`)
	before := captureMigrationState(t, tmp)
	requireGuardRefusal(t, "5004 under another name", runSignalRecipe(t, d, role.Name), "5004 is recorded under another name")
	requireMigrationStateUnchanged(t, "after the refusal", before, captureMigrationState(t, tmp))
	signalExec(t, d, `UPDATE schema_migrations SET name = '5004_signal_sidecar.sql' WHERE version = 5004`)

	signalExec(t, d, `INSERT INTO schema_migrations(version, name, checksum) VALUES (5903, '5903_future.sql', 'synthetic')`)
	before = captureMigrationState(t, tmp)
	requireGuardRefusal(t, "a newer fork migration", runSignalRecipe(t, d, role.Name), "5004 is not the newest fork migration")
	requireMigrationStateUnchanged(t, "after the refusal", before, captureMigrationState(t, tmp))
	signalExec(t, d, `DELETE FROM schema_migrations WHERE version = 5903`)

	signalExec(t, d, `CREATE ROLE `+quoteSQLIdent(role.Name)+` LOGIN`)
	before = captureMigrationState(t, tmp)
	if err := runSignalRecipe(t, d, role.Name); err == nil || !strings.Contains(err.Error(), "was not made for the sidecar") {
		t.Errorf("a role without the marker: err = %v, want the refusal", err)
	}
	requireMigrationStateUnchanged(t, "after the refusal", before, captureMigrationState(t, tmp))
	if !roleExists(t, d, role.Name) {
		t.Error("the recipe dropped a role that is not the sidecar's")
	}
	_ = ctx
}

// runSignalRecipeOn runs the recipe on a connection of its own and returns the notices it raised. The wait is
// bounded by the test, not by the recipe.
func runSignalRecipeOn(t *testing.T, tmp *dbtest.Temp, role string, wait time.Duration) ([]string, error) {
	t.Helper()
	script := strings.Replace(readUpgradeFile(t, rollbackDropSignalSidecarFile), "v_role    text := 'artex_signal'", "v_role    text := '"+role+"'", 1)
	cc, err := pgx.ParseConfig(tmp.DSN)
	if err != nil {
		t.Fatal(err)
	}
	var notices []string
	cc.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Severity+": "+n.Message) }
	conn := stdlib.OpenDB(*cc)
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, err = conn.ExecContext(ctx, script)
	if err != nil {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	}
	return notices, err
}

// A configuration row that is already gone does not stop the recipe, and it says so.
func TestSignalSidecarDropRecipeWarnsWhenTheConfigurationRowIsMissing(t *testing.T) {
	d, tmp, srv := signalFresh(t, "sigdropwarn")
	role := srv.NewSidecarRole(t, tmp)
	signalExec(t, d, `DELETE FROM signal_sidecar_config`)
	notices, err := runSignalRecipeOn(t, tmp, role.Name, time.Minute)
	if err != nil {
		t.Fatalf("the recipe failed (%s)", dbtest.AtomicErrorSummary(err))
	}
	var warned bool
	for _, n := range notices {
		if strings.HasPrefix(n, "WARNING:") && strings.Contains(n, "signal_sidecar_config") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about the missing configuration row; notices: %q", notices)
	}
}

// The recipe gives up on a lock another session holds, instead of waiting for ever.
func TestSignalSidecarDropRecipeGivesUpOnALock(t *testing.T) {
	d, tmp, srv := signalFresh(t, "sigdroplock")
	role := srv.NewSidecarRole(t, tmp)
	ctx := context.Background()
	holder, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback() //nolint:errcheck
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaMigrationLockKey); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = runSignalRecipeOn(t, tmp, role.Name, 30*time.Second)
	if pgCode(err) != "55P03" {
		t.Fatalf("the recipe under a held lock: err = %v, want SQLSTATE 55P03", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("the recipe waited %v", time.Since(start))
	}
}

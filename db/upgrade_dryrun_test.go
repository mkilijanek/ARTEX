package db

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db/dbtest"
)

// AC15 (Stage 1 delivers the guarded check, not a run on real data). The dry run is a
// runbook step (docs/schema-upgrade.md): it starts its own PostgreSQL container with
// its own volume and port, restores a dump into a database whose name ends in
// _upgradecheck, and runs TestDryRunRestoredCopy against it with
// ARTEX_UPGRADECHECK_DSN. Without that variable the run does not happen.
const dryRunEnv = "ARTEX_UPGRADECHECK_DSN"

// The server of ARTEX_PG_DSN is shared: it holds other databases, so the inventory
// rule must refuse it.
func TestUpgradeDryRunRefusesAServerThatHoldsOtherDatabases(t *testing.T) {
	srv := dbtest.RequireServer(t)
	tmp := dbtest.TempDatabase(t, srv, "dryrun") // a database the dry run did not create
	admin, err := dbtest.AdminQueryer(os.Getenv(dbtest.EnvDSN))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	err = dbtest.CheckDryRun(context.Background(), admin, "restored_upgradecheck")
	if err == nil {
		t.Fatal("the dry run accepted a server that holds other databases")
	}
	if !strings.Contains(err.Error(), tmp.Name) {
		t.Fatalf("refusal does not name the offending database %s: %v", tmp.Name, err)
	}
	dbtest.AssertNoSecret(t, "dry-run refusal", err.Error())
	if err := dbtest.CheckDryRun(context.Background(), admin, tmp.Name); err == nil {
		t.Fatal("the dry run accepted a target that does not end in _upgradecheck")
	}
}

// TestDryRunRestoredCopy runs startup twice on a restored copy and prints a content-free
// report: row counts per table before and after, catalog differences against the
// reference by object name. Not part of the gate (it needs the user's approval and its
// own server); it does nothing without ARTEX_UPGRADECHECK_DSN.
func TestDryRunRestoredCopy(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(dryRunEnv))
	if dsn == "" {
		t.Skipf("dry run not requested: %s is unset (runbook step, see docs/schema-upgrade.md)", dryRunEnv)
	}
	name, err := dbtest.ParsedDatabase(dsn)
	if err != nil {
		t.Fatalf("%s: %v", dryRunEnv, err)
	}
	ctx := context.Background()
	admin, err := dbtest.AdminQueryer(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := dbtest.CheckDryRun(ctx, admin, name); err != nil {
		t.Fatal(err)
	}

	conn, err := dbtest.OpenDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before, err := dbtest.RowCounts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	startup(t, dsn)
	afterFirst, err := dbtest.RowCounts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	startup(t, dsn)
	afterSecond, err := dbtest.RowCounts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := dbtest.Catalog(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	report := dbtest.Report{
		Database: name,
		Before:   before,
		After:    afterSecond,
		Catalog:  dbtest.DiffLines(loadReference(t), dbtest.Lines(entries)),
	}
	text := report.String()
	dbtest.AssertNoSecret(t, "dry-run report", text)
	t.Log("\n" + text)
	if removed := report.RemovedRows(); len(removed) > 0 {
		t.Errorf("startup removed rows from: %s", strings.Join(removed, ", "))
	}
	if d := dbtest.DiffMaps(afterFirst, afterSecond); len(d) > 0 {
		t.Errorf("the second startup changed row counts: %s", strings.Join(d, ", "))
	}
}

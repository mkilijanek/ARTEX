package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DryRunSuffix ends the name of the database a dry run may touch. The dry run
// (AC15) uses its own rule set: its server is a container the runbook starts for
// the purpose, and it is refused on any server that holds another database.
const DryRunSuffix = "_upgradecheck"

var dryRunName = regexp.MustCompile(`^[a-z0-9_]+$`)

// maintenanceDatabases are the databases a fresh server holds.
var maintenanceDatabases = map[string]bool{"template0": true, "template1": true, "postgres": true}

// DryRunInventoryRefusal returns an error unless databases, the content of
// pg_database, holds only template0, template1, postgres and names that end in
// _upgradecheck. The message lists the offending names (database names, not DSNs).
func DryRunInventoryRefusal(databases []string) error {
	var foreign []string
	for _, n := range databases {
		if maintenanceDatabases[n] || (strings.HasSuffix(n, DryRunSuffix) && len(n) > len(DryRunSuffix) && dryRunName.MatchString(n)) {
			continue
		}
		foreign = append(foreign, n)
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return fmt.Errorf("dry run refused: the server holds databases the dry run did not create: %s", strings.Join(foreign, ", "))
	}
	return nil
}

// DryRunTargetRefusal returns an error unless name is a database name that ends in
// _upgradecheck.
func DryRunTargetRefusal(name string) error {
	if !strings.HasSuffix(name, DryRunSuffix) || len(name) <= len(DryRunSuffix) || !dryRunName.MatchString(name) {
		return fmt.Errorf("dry run refused: database %q does not end in %s", name, DryRunSuffix)
	}
	if len(name) > maxIdentifier {
		return fmt.Errorf("dry run refused: database %q exceeds %d bytes", name, maxIdentifier)
	}
	return nil
}

// ListDatabases returns the names in pg_database.
func ListDatabases(ctx context.Context, admin Queryer) ([]string, error) {
	rows, err := admin.QueryContext(ctx, `SELECT datname FROM pg_database ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("list databases failed (%s)", sqlState(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list databases: scan failed")
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list databases failed (%s)", sqlState(err))
	}
	return out, nil
}

// CheckDryRun applies both dry-run rules: the target name and the server
// inventory, the latter read through admin (a connection to the server's
// maintenance database).
func CheckDryRun(ctx context.Context, admin Queryer, target string) error {
	if err := DryRunTargetRefusal(target); err != nil {
		return err
	}
	names, err := ListDatabases(ctx, admin)
	if err != nil {
		return err
	}
	return DryRunInventoryRefusal(names)
}

// RowCounts returns the exact row count of every public table.
func RowCounts(ctx context.Context, q Queryer) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `
SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("list tables failed (%s)", sqlState(err))
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list tables: scan failed")
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list tables failed (%s)", sqlState(err))
	}
	rows.Close()
	out := make(map[string]int64, len(names))
	for _, n := range names {
		r, err := q.QueryContext(ctx, "SELECT count(*) FROM "+quoteIdent(n))
		if err != nil {
			return nil, fmt.Errorf("count of table %q failed (%s)", n, sqlState(err))
		}
		var c int64
		ok := r.Next()
		if ok {
			err = r.Scan(&c)
		}
		r.Close()
		if !ok || err != nil {
			return nil, fmt.Errorf("count of table %q returned no value", n)
		}
		out[n] = c
	}
	return out, nil
}

// Report is the content-free result of a dry run: row counts per table before and
// after startup, and the catalog differences against the reference by object name.
type Report struct {
	Database string // parsed database name
	Before   map[string]int64
	After    map[string]int64
	Catalog  []string // DiffLines output: object names only
}

// RemovedRows lists the tables whose row count fell.
func (r Report) RemovedRows() []string {
	var out []string
	for n, b := range r.Before {
		if a, ok := r.After[n]; !ok || a < b {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// String renders the report: database name, one line per table with its counts,
// and the catalog differences. No value of any row appears in it.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dry run report for database %s\n", r.Database)
	names := make([]string, 0, len(r.Before))
	for n := range r.Before {
		names = append(names, n)
	}
	for n := range r.After {
		if _, ok := r.Before[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		bc, bok := r.Before[n]
		ac, aok := r.After[n]
		switch {
		case !bok:
			fmt.Fprintf(&b, "table %s: absent -> %d rows\n", n, ac)
		case !aok:
			fmt.Fprintf(&b, "table %s: %d rows -> absent\n", n, bc)
		default:
			fmt.Fprintf(&b, "table %s: %d -> %d rows\n", n, bc, ac)
		}
	}
	if len(r.Catalog) == 0 {
		b.WriteString("catalog: no difference to the reference\n")
	}
	for _, d := range r.Catalog {
		fmt.Fprintf(&b, "catalog: %s\n", d)
	}
	return b.String()
}

// AdminQueryer opens a connection to the maintenance database of the server that
// dsn names, for the inventory check. dsn is the dry-run DSN (not ARTEX_PG_DSN).
func AdminQueryer(dsn string) (*sql.DB, error) {
	admin, err := deriveDSN(dsn, "postgres")
	if err != nil {
		return nil, err
	}
	return sql.Open("pgx", admin)
}

// OpenDSN opens a pool on a DSN the caller has already checked (the dry-run
// database). It is not guarded: the dry run has its own rule set (CheckDryRun).
func OpenDSN(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }

// DiffMaps names the keys whose value differs between two row-count maps.
func DiffMaps(a, b map[string]int64) []string {
	var out []string
	for k, av := range a {
		if bv, ok := b[k]; !ok || av != bv {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

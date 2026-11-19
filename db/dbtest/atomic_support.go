package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// AtomicErrorSummary reduces an error to what is safe to print in a test failure.
//   - A PostgreSQL error: the SQLSTATE and the schema, table and constraint names. Never
//     the message, the detail or the statement: they can quote the row that violated a
//     constraint.
//   - A connection error: a fixed text. The driver puts host, user and database into it.
//   - Any other error (the application's own, such as "evidence snapshot metadata hash
//     mismatch"): its message, unless it contains a canary or the server password, in
//     which case a fixed text names the kind of secret.
func AtomicErrorSummary(err error) string {
	if err == nil {
		return "no error"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		parts := []string{"SQLSTATE " + pgErr.Code}
		if pgErr.SchemaName != "" {
			parts = append(parts, "schema "+pgErr.SchemaName)
		}
		if pgErr.TableName != "" {
			parts = append(parts, "table "+pgErr.TableName)
		}
		if pgErr.ConstraintName != "" {
			parts = append(parts, "constraint "+pgErr.ConstraintName)
		}
		return strings.Join(parts, ", ")
	}
	var connErr *pgconn.ConnectError
	var netErr *net.OpError
	if errors.As(err, &connErr) || errors.As(err, &netErr) {
		return "no SQLSTATE: connection error (details withheld)"
	}
	msg := err.Error()
	if kind, found := FindSecret(msg); found {
		return "no SQLSTATE: message withheld, it contains the " + kind
	}
	return "no SQLSTATE: " + msg
}

// AtomicErrorMentions reports whether the message of a PostgreSQL error in err's chain
// contains name (a constraint or table name). It lets a test identify which
// constraint failed when the error carries no constraint field, without printing the
// message.
func AtomicErrorMentions(err error, name string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.ConstraintName == name || strings.Contains(pgErr.Message, `"`+name+`"`)
}

// AtomicAdvisoryLocks counts, for the current database, the granted and the waiting
// advisory locks on a bigint key. A bigint key is stored as classid (high 32 bits) and
// objid (low 32 bits) with objsubid 1. Session-level and transaction-level locks
// appear in the same view because they share one key space.
func AtomicAdvisoryLocks(ctx context.Context, q Queryer, key int64) (granted, waiting int, err error) {
	rows, err := q.QueryContext(ctx, `
SELECT count(*) FILTER (WHERE granted), count(*) FILTER (WHERE NOT granted)
  FROM pg_locks
 WHERE locktype = 'advisory'
   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND objsubid = 1
   AND ((classid::bigint << 32) | objid::bigint) = $1`, key)
	if err != nil {
		return 0, 0, fmt.Errorf("list advisory locks failed (%s)", sqlState(err))
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, 0, fmt.Errorf("list advisory locks returned no row")
	}
	if err := rows.Scan(&granted, &waiting); err != nil {
		return 0, 0, fmt.Errorf("list advisory locks: scan failed")
	}
	return granted, waiting, rows.Err()
}

// AtomicDataDigests is TakeDigests without what now() makes unequal between two
// databases that hold the same logical rows: every timestamp column, and the
// sequences (they advance on upserts that add no row; callers compare them
// separately).
func AtomicDataDigests(ctx context.Context, q Queryer) (*Digests, error) {
	d, err := TakeDigests(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `
SELECT c.relname, a.attname
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid AND c.relkind = 'r'
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
  JOIN pg_type t ON t.oid = a.atttypid
 WHERE a.attnum > 0 AND NOT a.attisdropped AND t.typname IN ('timestamptz', 'timestamp')`)
	if err != nil {
		return nil, fmt.Errorf("list timestamp columns failed (%s)", sqlState(err))
	}
	defer rows.Close()
	for rows.Next() {
		var table, col string
		if err := rows.Scan(&table, &col); err != nil {
			return nil, fmt.Errorf("list timestamp columns: scan failed")
		}
		delete(d.Tables[table].Cols, col)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list timestamp columns failed (%s)", sqlState(err))
	}
	d.Sequences = map[string]int64{}
	return d, nil
}

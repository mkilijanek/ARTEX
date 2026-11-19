package dbtest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Row-level state for the upgrade matrix (docs/phase-d2-plan.md AC3, AC5). The digests of
// digest.go say that a column changed; the matrix also has to say which row and which
// value, because expected-changes.json lists exact entries. Everything here keeps the
// content-free rule: a description names a table, a primary key and a column, never a value
// (a text key is reduced to a short hash).

// keySep joins the primary key components of one row.
const keySep = "\x1f"

// TableRows is every row of one table, keyed by primary key. Values are the PostgreSQL text
// form of the column (col::text); nil is SQL NULL.
type TableRows struct {
	Cols []string // sorted column names
	PK   []string // primary key columns, in key order
	Rows map[string]map[string]*string

	timestamps map[string]bool // timestamptz columns: read in UTC, microseconds, whatever the session time zone
}

// timestampFormat renders a finite timestamptz as UTC with microseconds, independent of the
// TimeZone and DateStyle of the session, so that an expected value in the file reads the same
// on every server. 'infinity' and '-infinity' read as those words; NULL stays NULL.
const timestampFormat = `YYYY-MM-DD"T"HH24:MI:SS.US"Z"`

// IsTimestamp reports whether a column is of type timestamptz (read as UTC text with
// microseconds, or 'infinity' / '-infinity').
func (t *TableRows) IsTimestamp(col string) bool { return t.timestamps[col] }

// RowSnapshot is the row content of the public schema.
type RowSnapshot struct {
	Tables map[string]*TableRows
}

// TableNames returns the table names, sorted.
func (s *RowSnapshot) TableNames() []string {
	out := make([]string, 0, len(s.Tables))
	for n := range s.Tables {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RowCount is the number of rows of a table (0 for a table the snapshot does not have).
func (s *RowSnapshot) RowCount(table string) int {
	if t, ok := s.Tables[table]; ok {
		return len(t.Rows)
	}
	return 0
}

// CountValuesContaining counts the values that contain substr. It returns a number only.
func (s *RowSnapshot) CountValuesContaining(substr string) int {
	n := 0
	for _, t := range s.Tables {
		for _, row := range t.Rows {
			for _, v := range row {
				if v != nil && strings.Contains(*v, substr) {
					n++
				}
			}
		}
	}
	return n
}

// TakeRows reads every row of every table of the public schema. It is meant for fixtures,
// not for a real database.
func TakeRows(ctx context.Context, q Queryer) (*RowSnapshot, error) {
	snap := &RowSnapshot{Tables: map[string]*TableRows{}}

	rows, err := q.QueryContext(ctx, `
SELECT c.relname, a.attname, a.atttypid = 'timestamptz'::regtype
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped
 ORDER BY c.relname, a.attname`)
	if err != nil {
		return nil, fmt.Errorf("rows: list columns failed (%s)", sqlState(err))
	}
	for rows.Next() {
		var table, col string
		var isTimestamp bool
		if err := rows.Scan(&table, &col, &isTimestamp); err != nil {
			rows.Close()
			return nil, fmt.Errorf("rows: scan failed")
		}
		t := snap.Tables[table]
		if t == nil {
			t = &TableRows{Rows: map[string]map[string]*string{}, timestamps: map[string]bool{}}
			snap.Tables[table] = t
		}
		t.Cols = append(t.Cols, col)
		t.timestamps[col] = isTimestamp
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("rows: list columns failed (%s)", sqlState(err))
	}
	rows.Close()

	pks, err := q.QueryContext(ctx, `
SELECT c.relname, a.attname
  FROM pg_constraint k
  JOIN pg_class c ON c.oid = k.conrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN LATERAL unnest(k.conkey) WITH ORDINALITY AS x(attnum, ord)
  JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = x.attnum
 WHERE n.nspname = 'public' AND k.contype = 'p'
 ORDER BY c.relname, x.ord`)
	if err != nil {
		return nil, fmt.Errorf("rows: list primary keys failed (%s)", sqlState(err))
	}
	for pks.Next() {
		var table, col string
		if err := pks.Scan(&table, &col); err != nil {
			pks.Close()
			return nil, fmt.Errorf("rows: scan failed")
		}
		if t := snap.Tables[table]; t != nil {
			t.PK = append(t.PK, col)
		}
	}
	if err := pks.Err(); err != nil {
		pks.Close()
		return nil, fmt.Errorf("rows: list primary keys failed (%s)", sqlState(err))
	}
	pks.Close()

	for _, name := range snap.TableNames() {
		t := snap.Tables[name]
		if len(t.PK) == 0 {
			return nil, fmt.Errorf("rows: table %q has no primary key; the matrix cannot key its rows", name)
		}
		sel := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			if t.timestamps[c] {
				// to_char of an infinite timestamp returns NULL, which would equal SQL NULL:
				// an infinite value is read as its own text.
				col := quoteIdent(c)
				sel[i] = "CASE WHEN isfinite(" + col + ") THEN to_char(" + col + " AT TIME ZONE 'UTC', '" + timestampFormat + "') ELSE " + col + "::text END"
			} else {
				sel[i] = quoteIdent(c) + "::text"
			}
		}
		r, err := q.QueryContext(ctx, "SELECT "+strings.Join(sel, ", ")+" FROM "+quoteIdent(name))
		if err != nil {
			return nil, fmt.Errorf("rows: read table %q failed (%s)", name, sqlState(err))
		}
		for r.Next() {
			vals := make([]sql.NullString, len(t.Cols))
			dest := make([]any, len(t.Cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := r.Scan(dest...); err != nil {
				r.Close()
				return nil, fmt.Errorf("rows: scan of table %q failed", name)
			}
			row := make(map[string]*string, len(t.Cols))
			for i, c := range t.Cols {
				if vals[i].Valid {
					s := vals[i].String
					row[c] = &s
				} else {
					row[c] = nil
				}
			}
			key, err := t.keyOf(row)
			if err != nil {
				r.Close()
				return nil, fmt.Errorf("rows: table %q: %w", name, err)
			}
			if _, dup := t.Rows[key]; dup {
				r.Close()
				return nil, fmt.Errorf("rows: table %q has two rows with one primary key", name)
			}
			t.Rows[key] = row
		}
		if err := r.Err(); err != nil {
			r.Close()
			return nil, fmt.Errorf("rows: read table %q failed (%s)", name, sqlState(err))
		}
		r.Close()
	}
	return snap, nil
}

func (t *TableRows) keyOf(row map[string]*string) (string, error) {
	parts := make([]string, len(t.PK))
	for i, c := range t.PK {
		v := row[c]
		if v == nil {
			return "", fmt.Errorf("a primary key column is NULL")
		}
		parts[i] = *v
	}
	return strings.Join(parts, keySep), nil
}

// KeyFromPK builds the row key from a primary key given by column name. It fails unless the
// columns are exactly the table's primary key.
func (t *TableRows) KeyFromPK(pk map[string]string) (string, error) {
	if len(pk) != len(t.PK) {
		return "", fmt.Errorf("pk has %d columns, the primary key has %d (%s)", len(pk), len(t.PK), strings.Join(t.PK, ", "))
	}
	parts := make([]string, len(t.PK))
	for i, c := range t.PK {
		v, ok := pk[c]
		if !ok {
			return "", fmt.Errorf("pk lacks column %q of the primary key (%s)", c, strings.Join(t.PK, ", "))
		}
		parts[i] = v
	}
	return strings.Join(parts, keySep), nil
}

// DescribeKey renders a row key for a message: integer components as they are, any other
// component as text#<8 hex of its sha256>, so that a text key never reaches the output.
func (t *TableRows) DescribeKey(key string) string {
	parts := strings.Split(key, keySep)
	out := make([]string, len(parts))
	for i, p := range parts {
		name := "?"
		if i < len(t.PK) {
			name = t.PK[i]
		}
		if _, err := strconv.ParseInt(p, 10, 64); err == nil {
			out[i] = name + "=" + p
			continue
		}
		sum := sha256.Sum256([]byte(p))
		out[i] = name + "=text#" + hex.EncodeToString(sum[:])[:8]
	}
	return strings.Join(out, ",")
}

// DiffKind says what changed.
type DiffKind string

const (
	DiffAdded         DiffKind = "added"          // a row exists after and not before
	DiffDeleted       DiffKind = "deleted"        // a row existed before and not after
	DiffValue         DiffKind = "value"          // a column of a row present on both sides differs
	DiffColumnRemoved DiffKind = "column-removed" // a column present before is gone after
)

// RowDiff is one difference between two snapshots. Before and After carry values for the
// comparison code; Describe never prints them.
type RowDiff struct {
	Kind        DiffKind
	Table       string
	Key         string
	Column      string
	Before      *string
	After       *string
	ColumnAdded bool // the column did not exist before; Before is nil and means "absent"
	meta        *TableRows
}

// Describe is a content-free description.
func (d RowDiff) Describe() string {
	where := "table " + d.Table
	if d.Key != "" && d.meta != nil {
		where += " row " + d.meta.DescribeKey(d.Key)
	}
	switch d.Kind {
	case DiffAdded:
		return where + ": row added"
	case DiffDeleted:
		return where + ": row deleted"
	case DiffColumnRemoved:
		return where + ": column " + d.Column + " removed"
	default:
		if d.ColumnAdded {
			return where + ": column " + d.Column + " is new and has a value"
		}
		return where + ": column " + d.Column + " changed"
	}
}

// DiffRows lists every difference between two snapshots, sorted by table, key and column.
// A table the after side lacks counts as all rows deleted; a table the before side lacks
// (created by startup) as all rows added.
func DiffRows(before, after *RowSnapshot) []RowDiff {
	var out []RowDiff
	names := map[string]bool{}
	for n := range before.Tables {
		names[n] = true
	}
	for n := range after.Tables {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		b, a := before.Tables[name], after.Tables[name]
		switch {
		case b == nil:
			for _, k := range sortedKeys(a.Rows) {
				out = append(out, RowDiff{Kind: DiffAdded, Table: name, Key: k, meta: a})
			}
			continue
		case a == nil:
			for _, k := range sortedKeys(b.Rows) {
				out = append(out, RowDiff{Kind: DiffDeleted, Table: name, Key: k, meta: b})
			}
			continue
		}
		bcols, acols := toSet(b.Cols), toSet(a.Cols)
		for _, c := range b.Cols {
			if !acols[c] {
				out = append(out, RowDiff{Kind: DiffColumnRemoved, Table: name, Column: c, meta: b})
			}
		}
		for _, k := range sortedKeys(b.Rows) {
			arow, ok := a.Rows[k]
			if !ok {
				out = append(out, RowDiff{Kind: DiffDeleted, Table: name, Key: k, meta: b})
				continue
			}
			brow := b.Rows[k]
			for _, c := range a.Cols {
				if !bcols[c] {
					out = append(out, RowDiff{Kind: DiffValue, Table: name, Key: k, Column: c, After: arow[c], ColumnAdded: true, meta: a})
					continue
				}
				if !sameValue(brow[c], arow[c]) {
					out = append(out, RowDiff{Kind: DiffValue, Table: name, Key: k, Column: c, Before: brow[c], After: arow[c], meta: a})
				}
			}
		}
		for _, k := range sortedKeys(a.Rows) {
			if _, ok := b.Rows[k]; !ok {
				out = append(out, RowDiff{Kind: DiffAdded, Table: name, Key: k, meta: a})
			}
		}
	}
	return out
}

func sameValue(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sortedKeys(m map[string]map[string]*string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

// RepeatProblems checks AC5: a further startup changes no row. The only permitted difference
// is agents.updated_at of a built-in agent (db/db.go:258 fires the update trigger of
// db/schema.sql:830-832 on every start); every other column of those rows, and every other
// row, must be equal.
func RepeatProblems(before, after *RowSnapshot) []string {
	var out []string
	for _, d := range DiffRows(before, after) {
		if d.Kind == DiffValue && !d.ColumnAdded && d.Table == "agents" && d.Column == "updated_at" {
			if row := after.Tables["agents"].Rows[d.Key]; row != nil && row["builtin"] != nil && *row["builtin"] == "true" {
				continue
			}
		}
		out = append(out, d.Describe())
	}
	sort.Strings(out)
	return out
}

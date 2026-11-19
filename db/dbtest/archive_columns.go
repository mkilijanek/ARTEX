package dbtest

import (
	"context"
	"fmt"
	"sort"
)

// ArchiveColumn is the part of a column definition that task-archive restore depends
// on: INSERT ... SELECT * FROM json_populate_recordset(NULL::table, ...) maps keys to
// columns by name and converts to the column type, and a NULL for a NOT NULL column
// fails the restore.
type ArchiveColumn struct {
	Type    string // format_type
	NotNull bool
}

// ArchiveColumns lists the columns of the named tables of the public schema, keyed by
// table and column. A table that does not exist is absent from the result.
func ArchiveColumns(ctx context.Context, q Queryer, tables []string) (map[string]map[string]ArchiveColumn, error) {
	want := map[string]bool{}
	for _, t := range tables {
		want[t] = true
	}
	rows, err := q.QueryContext(ctx, `
SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid AND c.relkind = 'r'
  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
 WHERE a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		return nil, fmt.Errorf("list archived columns failed (%s)", sqlState(err))
	}
	defer rows.Close()
	out := map[string]map[string]ArchiveColumn{}
	for rows.Next() {
		var table, col string
		var c ArchiveColumn
		if err := rows.Scan(&table, &col, &c.Type, &c.NotNull); err != nil {
			return nil, fmt.Errorf("list archived columns: scan failed")
		}
		if !want[table] {
			continue
		}
		if out[table] == nil {
			out[table] = map[string]ArchiveColumn{}
		}
		out[table][col] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list archived columns failed (%s)", sqlState(err))
	}
	return out, nil
}

// ArchiveColumnGuard compares the archived tables of an old shape with the current
// one and returns one line per breakage, sorted:
//
//   - an archived table that exists in the old shape and not now;
//   - a column of the old shape that is missing now (dropped or renamed: restore
//     ignores unknown keys, so its data would be lost without an error);
//   - a column whose type or nullability changed;
//   - a column that is NOT NULL now and absent from the old shape, unless
//     restoreDefaults has an entry "table.column" naming the normaliser of the
//     restore path that fills it (json_populate_recordset inserts NULL for an
//     absent key and ignores SQL defaults).
//
// A table that is new since the old shape is not a breakage: an old package has no
// key for it and restore treats a missing key as zero rows. Lines name tables and
// columns only.
func ArchiveColumnGuard(old, cur map[string]map[string]ArchiveColumn, restoreDefaults map[string]string) []string {
	var out []string
	for table, oldCols := range old {
		curCols, ok := cur[table]
		if !ok {
			out = append(out, fmt.Sprintf("table %s: archived in the old shape but no longer exists", table))
			continue
		}
		for col, oc := range oldCols {
			cc, ok := curCols[col]
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("%s.%s: dropped or renamed (restore would silently lose its data)", table, col))
			case cc.Type != oc.Type:
				out = append(out, fmt.Sprintf("%s.%s: type %s -> %s", table, col, oc.Type, cc.Type))
			case cc.NotNull != oc.NotNull:
				out = append(out, fmt.Sprintf("%s.%s: nullability %s -> %s", table, col, nullability(oc.NotNull), nullability(cc.NotNull)))
			}
		}
		for col, cc := range curCols {
			if _, existed := oldCols[col]; existed || !cc.NotNull {
				continue
			}
			if _, ok := restoreDefaults[table+"."+col]; !ok {
				out = append(out, fmt.Sprintf("%s.%s: new NOT NULL column without a restore default (old packages have no key for it)", table, col))
			}
		}
	}
	sort.Strings(out)
	return out
}

func nullability(notNull bool) string {
	if notNull {
		return "NOT NULL"
	}
	return "nullable"
}

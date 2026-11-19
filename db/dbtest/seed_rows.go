package dbtest

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RowState is the content-free, row-level state of the public tables: table ->
// primary key text -> column -> sha256 of the column's JSON value. Primary-key
// text and column names are structure (ids and keys the fixture chose), not row
// content; values appear only as hashes and are never printed.
type RowState map[string]map[string]map[string]string

// TakeRowState reads every row of every table of the public schema. It is meant
// for fixtures. A table without a primary key is keyed by the md5 of the row text.
func TakeRowState(ctx context.Context, q Queryer) (RowState, error) {
	type tbl struct {
		name string
		pk   []string
	}
	var tables []tbl
	rows, err := q.QueryContext(ctx, `
SELECT c.relname,
       coalesce((SELECT array_to_string(array(SELECT a.attname FROM pg_attribute a
            WHERE a.attrelid = k.conrelid AND a.attnum = ANY (k.conkey) ORDER BY a.attname), ',')
            FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'p'), '')
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r'
 ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("row state: list tables failed (%s)", sqlState(err))
	}
	for rows.Next() {
		var name, pk string
		if err := rows.Scan(&name, &pk); err != nil {
			rows.Close()
			return nil, fmt.Errorf("row state: scan failed")
		}
		t := tbl{name: name}
		if pk != "" {
			t.pk = strings.Split(pk, ",")
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("row state: list tables failed (%s)", sqlState(err))
	}
	rows.Close()

	state := RowState{}
	for _, t := range tables {
		keyExpr := "md5(t::text)"
		if len(t.pk) > 0 {
			parts := make([]string, len(t.pk))
			for i, c := range t.pk {
				// pk values are ids and keys; a text key is JSON-quoted by ::text
				parts[i] = fmt.Sprintf("'%s=' || (to_jsonb(t) -> '%s')::text", c, c)
			}
			keyExpr = strings.Join(parts, " || ',' || ")
		}
		query := fmt.Sprintf(
			`SELECT %s, e.key, encode(sha256(convert_to(e.value::text, 'UTF8')), 'hex')
  FROM %s t, LATERAL jsonb_each(to_jsonb(t)) e`, keyExpr, quoteIdent(t.name))
		r, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("row state of table %q failed (%s)", t.name, sqlState(err))
		}
		byPK := map[string]map[string]string{}
		for r.Next() {
			var pk, col, h string
			if err := r.Scan(&pk, &col, &h); err != nil {
				r.Close()
				return nil, fmt.Errorf("row state of table %q: scan failed", t.name)
			}
			if byPK[pk] == nil {
				byPK[pk] = map[string]string{}
			}
			byPK[pk][col] = h
		}
		if err := r.Err(); err != nil {
			r.Close()
			return nil, fmt.Errorf("row state of table %q failed (%s)", t.name, sqlState(err))
		}
		r.Close()
		state[t.name] = byPK
	}
	return state, nil
}

// Row-change kinds reported by DiffRowState.
const (
	KindValue      = "value"       // an existing row's column changed
	KindDeleted    = "deleted"     // a row is gone
	KindAdded      = "added"       // a row is new
	KindTableAdded = "table-added" // a table exists only after
	KindTableGone  = "table-gone"  // a table exists only before
)

// RowChange is one difference between two row states. Column is set for
// KindValue only; PK is empty for the table kinds.
type RowChange struct {
	Kind, Table, PK, Column string
}

// String is the exact entry form the tests assert: kind, table, primary key and
// column, never a value.
func (c RowChange) String() string {
	parts := []string{c.Kind, c.Table}
	if c.PK != "" {
		parts = append(parts, "["+c.PK+"]")
	}
	if c.Column != "" {
		parts = append(parts, c.Column)
	}
	return strings.Join(parts, " ")
}

// DiffRowState lists every difference between before and after, sorted by entry
// text. A column that exists on one side only is reported as a value change.
func DiffRowState(before, after RowState) []RowChange {
	var out []RowChange
	names := map[string]bool{}
	for n := range before {
		names[n] = true
	}
	for n := range after {
		names[n] = true
	}
	for n := range names {
		b, bok := before[n]
		a, aok := after[n]
		switch {
		case !bok:
			out = append(out, RowChange{Kind: KindTableAdded, Table: n})
			for pk := range a {
				out = append(out, RowChange{Kind: KindAdded, Table: n, PK: pk})
			}
			continue
		case !aok:
			out = append(out, RowChange{Kind: KindTableGone, Table: n})
			continue
		}
		for pk, bcols := range b {
			acols, ok := a[pk]
			if !ok {
				out = append(out, RowChange{Kind: KindDeleted, Table: n, PK: pk})
				continue
			}
			cols := map[string]bool{}
			for c := range bcols {
				cols[c] = true
			}
			for c := range acols {
				cols[c] = true
			}
			for c := range cols {
				if bcols[c] != acols[c] {
					out = append(out, RowChange{Kind: KindValue, Table: n, PK: pk, Column: c})
				}
			}
		}
		for pk := range a {
			if _, ok := b[pk]; !ok {
				out = append(out, RowChange{Kind: KindAdded, Table: n, PK: pk})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

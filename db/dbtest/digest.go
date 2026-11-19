package dbtest

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// TableDigest summarises one table without exposing a value: the row count and a
// sha256 per column over the column's values in primary-key order.
type TableDigest struct {
	Rows int64
	Cols map[string]string // column name -> hex sha256 of the ordered values
}

// Digests is the content-free state of a database: per-table digests and the
// last_value of every sequence (0 for a sequence that was never used).
type Digests struct {
	Tables    map[string]TableDigest
	Sequences map[string]int64
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// TakeDigests computes Digests for the public schema. It reads every row of every
// table, so it is meant for fixtures, not for a real database (the dry run uses
// row counts only).
func TakeDigests(ctx context.Context, q Queryer) (*Digests, error) {
	d := &Digests{Tables: map[string]TableDigest{}, Sequences: map[string]int64{}}

	type tbl struct {
		name string
		cols []string
		pk   []string
	}
	var tables []tbl
	rows, err := q.QueryContext(ctx, `
SELECT c.relname,
       array_to_string(array(SELECT a.attname FROM pg_attribute a
            WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attname), ','),
       coalesce((SELECT array_to_string(array(SELECT a.attname FROM pg_attribute a
            WHERE a.attrelid = k.conrelid AND a.attnum = ANY (k.conkey) ORDER BY a.attname), ',')
            FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'p'), '')
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind = 'r'
 ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("digest: list tables failed (%s)", sqlState(err))
	}
	for rows.Next() {
		var name, cols, pk string
		if err := rows.Scan(&name, &cols, &pk); err != nil {
			rows.Close()
			return nil, fmt.Errorf("digest: scan failed")
		}
		t := tbl{name: name, cols: strings.Split(cols, ",")}
		if pk != "" {
			t.pk = strings.Split(pk, ",")
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("digest: list tables failed (%s)", sqlState(err))
	}
	rows.Close()

	for _, t := range tables {
		// Order by the primary key; a table without one is ordered by the whole row.
		order := "t::text"
		if len(t.pk) > 0 {
			qs := make([]string, len(t.pk))
			for i, c := range t.pk {
				qs[i] = "t." + quoteIdent(c)
			}
			order = strings.Join(qs, ", ")
		}
		sel := []string{"count(*)"}
		for _, c := range t.cols {
			sel = append(sel, fmt.Sprintf(
				`encode(sha256(convert_to(coalesce(string_agg(coalesce(t.%s::text, chr(1) || 'NULL'), chr(31) ORDER BY %s), ''), 'UTF8')), 'hex')`,
				quoteIdent(c), order))
		}
		query := "SELECT " + strings.Join(sel, ", ") + " FROM " + quoteIdent(t.name) + " t"
		dest := make([]any, len(sel))
		var td TableDigest
		td.Cols = map[string]string{}
		hashes := make([]string, len(t.cols))
		dest[0] = &td.Rows
		for i := range t.cols {
			dest[i+1] = &hashes[i]
		}
		r, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("digest of table %q failed (%s)", t.name, sqlState(err))
		}
		if !r.Next() {
			r.Close()
			return nil, fmt.Errorf("digest of table %q returned no row", t.name)
		}
		if err := r.Scan(dest...); err != nil {
			r.Close()
			return nil, fmt.Errorf("digest of table %q: scan failed", t.name)
		}
		r.Close()
		for i, c := range t.cols {
			td.Cols[c] = hashes[i]
		}
		d.Tables[t.name] = td
	}

	srows, err := q.QueryContext(ctx,
		`SELECT sequencename, coalesce(last_value, 0) FROM pg_sequences WHERE schemaname = 'public'`)
	if err != nil {
		return nil, fmt.Errorf("digest: list sequences failed (%s)", sqlState(err))
	}
	defer srows.Close()
	for srows.Next() {
		var name string
		var last int64
		if err := srows.Scan(&name, &last); err != nil {
			return nil, fmt.Errorf("digest: scan failed")
		}
		d.Sequences[name] = last
	}
	if err := srows.Err(); err != nil {
		return nil, fmt.Errorf("digest: list sequences failed (%s)", sqlState(err))
	}
	return d, nil
}

// DiffDigests returns one line per difference between two states, naming tables,
// columns and sequences and printing counts and sequence values; it never prints a
// column value or a hash.
func DiffDigests(before, after *Digests) []string {
	var out []string
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
	for _, n := range sorted {
		b, bok := before.Tables[n]
		a, aok := after.Tables[n]
		switch {
		case !bok:
			out = append(out, fmt.Sprintf("table %s: added with %d rows", n, a.Rows))
			continue
		case !aok:
			out = append(out, fmt.Sprintf("table %s: removed (had %d rows)", n, b.Rows))
			continue
		}
		if b.Rows != a.Rows {
			out = append(out, fmt.Sprintf("table %s: rows %d -> %d", n, b.Rows, a.Rows))
		}
		cols := map[string]bool{}
		for c := range b.Cols {
			cols[c] = true
		}
		for c := range a.Cols {
			cols[c] = true
		}
		cs := make([]string, 0, len(cols))
		for c := range cols {
			cs = append(cs, c)
		}
		sort.Strings(cs)
		for _, c := range cs {
			bh, bo := b.Cols[c]
			ah, ao := a.Cols[c]
			switch {
			case !bo:
				out = append(out, fmt.Sprintf("table %s: column %s added", n, c))
			case !ao:
				out = append(out, fmt.Sprintf("table %s: column %s removed", n, c))
			case bh != ah:
				out = append(out, fmt.Sprintf("table %s: column %s values changed", n, c))
			}
		}
	}
	seqs := map[string]bool{}
	for n := range before.Sequences {
		seqs[n] = true
	}
	for n := range after.Sequences {
		seqs[n] = true
	}
	ss := make([]string, 0, len(seqs))
	for n := range seqs {
		ss = append(ss, n)
	}
	sort.Strings(ss)
	for _, n := range ss {
		b, bok := before.Sequences[n]
		a, aok := after.Sequences[n]
		switch {
		case !bok:
			out = append(out, fmt.Sprintf("sequence %s: added at %d", n, a))
		case !aok:
			out = append(out, fmt.Sprintf("sequence %s: removed", n))
		case a < b:
			out = append(out, fmt.Sprintf("sequence %s: last_value fell %d -> %d", n, b, a))
		case a != b:
			out = append(out, fmt.Sprintf("sequence %s: last_value %d -> %d", n, b, a))
		}
	}
	return out
}

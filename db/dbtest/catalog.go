package dbtest

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Queryer is what Catalog and Digests need from *sql.DB, *sql.Conn or *sql.Tx.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Entry is one catalog object: a kind, a name without spaces, and the definition
// text the hash is taken over. Definitions are schema text, never row content, but
// only the hash is written to reference files and reports.
type Entry struct {
	Kind string
	Name string
	Def  string
}

// Hash is the sha256 of the definition, hex encoded.
func (e Entry) Hash() string {
	sum := sha256.Sum256([]byte(e.Def))
	return hex.EncodeToString(sum[:])
}

// Line is the listing line: "kind name sha256(definition)".
func (e Entry) Line() string { return e.Kind + " " + e.Name + " " + e.Hash() }

// Lines returns the sorted listing lines of entries.
func Lines(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Line()
	}
	sort.Strings(out)
	return out
}

// catalogQueries each return (kind, name, definition). Only the public schema is
// listed; a schema other than public, an extension other than plpgsql, or a
// relation kind the schema is not meant to have shows up as its own line, so that
// it differs from the reference.
//
// Out of scope, as the reference header states: owners, grants, comments, user types,
// tablespaces. Plain views are listed (definition and columns); other relation kinds
// show up as "relation" lines. PostgreSQL 16 only.
//
// What the listing does not cover, so that a green comparison is not read as more than it is.
// Two databases with an equal listing can still differ in:
//   - sequence persistence (UNLOGGED sequences) and sequence values (the digests compare last_value,
//     the listing does not);
//   - table storage parameters (pg_class.reloptions, for example fillfactor, autovacuum settings);
//   - row-level security: the relrowsecurity and relforcerowsecurity flags and the policies;
//   - rewrite rules (pg_rewrite);
//   - extended statistics (CREATE STATISTICS);
//   - enum, domain and composite types (user types);
//   - event triggers;
//   - the index flags indisclustered and indisreplident (CLUSTER and REPLICA IDENTITY USING INDEX),
//     and the table's replica identity setting;
//   - owners, grants and default privileges (the least-privilege test of the Signal sidecar role reads them);
//   - comments (COMMENT ON ...);
//   - tablespaces, and anything outside the public schema.
var catalogQueries = []string{
	// schemas other than public and the system schemas
	`SELECT 'schema', nspname, '' FROM pg_namespace
	   WHERE nspname <> 'public' AND nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'`,
	// extensions other than plpgsql
	`SELECT 'extension', extname, extversion FROM pg_extension WHERE extname <> 'plpgsql'`,
	// tables with persistence
	`SELECT 'table', c.relname, c.relkind::text || ' ' || c.relpersistence::text
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')`,
	// relation kinds that are neither tables, views, indexes nor sequences
	`SELECT 'relation', c.relname, c.relkind::text
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = 'public' AND c.relkind NOT IN ('r', 'p', 'v', 'i', 'I', 'S')`,
	// plain views: definition and columns (name and type, in order)
	`SELECT 'view', c.relname,
	        pg_get_viewdef(c.oid) || '|' ||
	        (SELECT string_agg(a.attname || ' ' || format_type(a.atttypid, a.atttypmod), ',' ORDER BY a.attnum)
	           FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped)
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = 'public' AND c.relkind = 'v'`,
	// columns: type, nullability, default, collation, identity, generated
	`SELECT 'column', c.relname || '.' || a.attname,
	        format_type(a.atttypid, a.atttypmod) || '|' || a.attnotnull::text || '|' ||
	        coalesce(pg_get_expr(d.adbin, d.adrelid), '') || '|' ||
	        coalesce(co.collname, '') || '|' || a.attidentity::text || '|' || a.attgenerated::text
	   FROM pg_attribute a
	   JOIN pg_class c ON c.oid = a.attrelid
	   JOIN pg_namespace n ON n.oid = c.relnamespace
	   LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	   LEFT JOIN pg_collation co ON co.oid = a.attcollation AND a.attcollation <> 0
	  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped`,
	// constraints: definition and validity
	`SELECT 'constraint', c.relname || '.' || k.conname, k.contype::text || '|' || pg_get_constraintdef(k.oid) || '|' || k.convalidated::text
	   FROM pg_constraint k
	   JOIN pg_class c ON c.oid = k.conrelid
	   JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = 'public'`,
	// indexes: definition, validity, readiness
	`SELECT 'index', ic.relname, pg_get_indexdef(i.indexrelid) || '|' || i.indisvalid::text || '|' || i.indisready::text
	   FROM pg_index i
	   JOIN pg_class ic ON ic.oid = i.indexrelid
	   JOIN pg_namespace n ON n.oid = ic.relnamespace
	  WHERE n.nspname = 'public'`,
	// triggers that are not internal: definition and enabled state
	`SELECT 'trigger', c.relname || '.' || t.tgname, pg_get_triggerdef(t.oid) || '|' || t.tgenabled::text
	   FROM pg_trigger t
	   JOIN pg_class c ON c.oid = t.tgrelid
	   JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE n.nspname = 'public' AND NOT t.tgisinternal`,
	// functions
	`SELECT 'function', replace(p.oid::regprocedure::text, ' ', ''), pg_get_functiondef(p.oid)
	   FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	  WHERE n.nspname = 'public' AND p.prokind = 'f'`,
	// sequences: owner column and parameters
	`SELECT 'sequence', s.relname,
	        coalesce(t.relname || '.' || a.attname, 'NONE') || '|' || q.seqtypid::regtype::text || '|' ||
	        q.seqstart::text || '|' || q.seqincrement::text || '|' || q.seqmin::text || '|' ||
	        q.seqmax::text || '|' || q.seqcache::text || '|' || q.seqcycle::text
	   FROM pg_class s
	   JOIN pg_namespace n ON n.oid = s.relnamespace
	   JOIN pg_sequence q ON q.seqrelid = s.oid
	   LEFT JOIN pg_depend d ON d.objid = s.oid AND d.classid = 'pg_class'::regclass
	        AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a'
	   LEFT JOIN pg_class t ON t.oid = d.refobjid
	   LEFT JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
	  WHERE n.nspname = 'public' AND s.relkind = 'S'`,
}

// Catalog lists the schema objects of the connected database, sorted by kind and
// name. Column order is excluded on purpose: restore uses the same row type on both
// sides of INSERT ... SELECT *.
func Catalog(ctx context.Context, q Queryer) ([]Entry, error) {
	var out []Entry
	for i, query := range catalogQueries {
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("catalog query %d failed (%s)", i, sqlState(err))
		}
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.Kind, &e.Name, &e.Def); err != nil {
				rows.Close()
				return nil, fmt.Errorf("catalog query %d: scan failed", i)
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalog query %d failed (%s)", i, sqlState(err))
		}
		rows.Close()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ReadReference parses a reference listing: blank lines and lines that start with
// '#' are ignored; every other line is "kind name sha256".
func ReadReference(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if f := strings.Fields(line); len(f) < 3 || len(f[len(f)-1]) != sha256.Size*2 {
			return nil, fmt.Errorf("reference line %d is not \"kind name sha256\"", n)
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read reference: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// keyOf is "kind name" of a listing line: everything but the trailing hash.
func keyOf(line string) string {
	if i := strings.LastIndexByte(line, ' '); i >= 0 {
		return line[:i]
	}
	return line
}

// DiffLines compares two sorted listings and returns one line per difference, as
// "missing <kind> <name>" (in want, not in got), "unexpected <kind> <name>" (in
// got only) or "changed <kind> <name>". It names objects and never prints
// definitions or hashes, so it is safe to put in a failure message.
func DiffLines(want, got []string) []string {
	w := make(map[string]string, len(want))
	for _, l := range want {
		w[keyOf(l)] = l
	}
	g := make(map[string]string, len(got))
	for _, l := range got {
		g[keyOf(l)] = l
	}
	var out []string
	for k, l := range w {
		gl, ok := g[k]
		switch {
		case !ok:
			out = append(out, "missing "+k)
		case gl != l:
			out = append(out, "changed "+k)
		}
	}
	for k := range g {
		if _, ok := w[k]; !ok {
			out = append(out, "unexpected "+k)
		}
	}
	sort.Strings(out)
	return out
}

// Count returns how many entries have the given kind.
func Count(entries []Entry, kind string) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

package dbtest

import (
	"context"
	"fmt"
	"strings"
)

// LedgerState is the content-free state of the migration ledger table schema_migrations of a database:
// whether the table exists and which versions it holds. It carries no file name, no checksum and no
// timestamp, so the dry run on a copy of a real database can print it (docs/phase-d2-stage2-plan.md AC26).
type LedgerState struct {
	Present  bool
	Versions []int // ascending; empty when the table is absent or holds no row
}

// LedgerStateOf reads the ledger state through q.
func LedgerStateOf(ctx context.Context, q Queryer) (LedgerState, error) {
	var st LedgerState
	r, err := q.QueryContext(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`)
	if err != nil {
		return st, fmt.Errorf("look up the ledger table failed (%s)", sqlState(err))
	}
	ok := r.Next()
	if ok {
		err = r.Scan(&st.Present)
	}
	r.Close()
	if !ok || err != nil {
		return st, fmt.Errorf("look up the ledger table returned no value")
	}
	if !st.Present {
		return st, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return st, fmt.Errorf("read the ledger versions failed (%s)", sqlState(err))
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return st, fmt.Errorf("read the ledger versions: scan failed")
		}
		st.Versions = append(st.Versions, v)
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("read the ledger versions failed (%s)", sqlState(err))
	}
	return st, nil
}

// String renders the state: "absent", "empty" or the versions as four digits.
func (s LedgerState) String() string {
	switch {
	case !s.Present:
		return "absent"
	case len(s.Versions) == 0:
		return "present, no version"
	}
	parts := make([]string, len(s.Versions))
	for i, v := range s.Versions {
		parts[i] = fmt.Sprintf("%04d", v)
	}
	return "present, versions " + strings.Join(parts, " ")
}

// LedgerHas reports whether every version of want is in the ledger.
func (s LedgerState) LedgerHas(want ...int) bool {
	have := map[int]bool{}
	for _, v := range s.Versions {
		have[v] = true
	}
	for _, v := range want {
		if !have[v] {
			return false
		}
	}
	return true
}

// LedgerIs reports whether the ledger holds exactly the versions of want (and exists).
func (s LedgerState) LedgerIs(want ...int) bool {
	if !s.Present || len(s.Versions) != len(want) {
		return false
	}
	return s.LedgerHas(want...)
}

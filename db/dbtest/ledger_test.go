package dbtest

import (
	"context"
	"testing"
)

func TestUpgradeLedgerStateRendersVersionsOnly(t *testing.T) {
	for _, c := range []struct {
		st   LedgerState
		want string
	}{
		{LedgerState{}, "absent"},
		{LedgerState{Present: true}, "present, no version"},
		{LedgerState{Present: true, Versions: []int{1, 2, 3, 5001}}, "present, versions 0001 0002 0003 5001"},
	} {
		if got := c.st.String(); got != c.want {
			t.Errorf("%+v renders %q, want %q", c.st, got, c.want)
		}
	}
	st := LedgerState{Present: true, Versions: []int{1, 2, 3, 5001}}
	if !st.LedgerIs(5001, 3, 2, 1) || st.LedgerIs(1, 2, 3) || st.LedgerIs(1, 2, 3, 5001, 5002) || (LedgerState{}).LedgerIs() {
		t.Error("LedgerIs does not compare the exact set of versions of an existing ledger")
	}
	if !st.LedgerHas(1, 5001) || st.LedgerHas(4) {
		t.Error("LedgerHas does not test membership")
	}
}

// The state is read from a real table: absent, empty, filled. Names and checksums of the rows
// never enter it.
func TestUpgradeLedgerStateReadsTheTable(t *testing.T) {
	srv := RequireServer(t)
	tmp := TempDatabase(t, srv, "ledgerstate")
	conn := tmp.Conn(t)
	ctx := context.Background()
	read := func() LedgerState {
		t.Helper()
		st, err := LedgerStateOf(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := read(); st.Present || len(st.Versions) != 0 {
		t.Errorf("a database without the table reads %+v", st)
	}
	tmp.Load(t, "ledger table", `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT, checksum TEXT);`)
	if st := read(); !st.Present || len(st.Versions) != 0 || st.String() != "present, no version" {
		t.Errorf("an empty ledger reads %+v", st)
	}
	tmp.Load(t, "ledger rows", `INSERT INTO schema_migrations VALUES (5001, 'CANARY-D2-ROW-name', 'CANARY-D2-ROW-sum'), (1, 'a', 'b');`)
	st := read()
	if !st.LedgerIs(1, 5001) || st.String() != "present, versions 0001 5001" {
		t.Errorf("a filled ledger reads %+v (%s)", st, st)
	}
	AssertNoSecret(t, "ledger state", st.String())
}

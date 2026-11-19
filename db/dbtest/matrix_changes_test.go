package dbtest

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests need no database: they feed the comparison hand-made snapshots.

func mxStr(s string) *string { return &s }

func mxRaw(s *string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func mxSnapshot(tables map[string]*TableRows) *RowSnapshot { return &RowSnapshot{Tables: tables} }

func mxTable(pk []string, cols []string, rows ...map[string]*string) *TableRows {
	t := &TableRows{Cols: cols, PK: pk, Rows: map[string]map[string]*string{}, timestamps: map[string]bool{}}
	for _, r := range rows {
		key, err := t.keyOf(r)
		if err != nil {
			panic(err)
		}
		t.Rows[key] = r
	}
	return t
}

func mxRow(kv ...string) map[string]*string {
	m := map[string]*string{}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "<NULL>" {
			m[kv[i]] = nil
		} else {
			m[kv[i]] = mxStr(kv[i+1])
		}
	}
	return m
}

var agentCols = []string{"builtin", "id", "name", "updated_at"}

func mxBefore() *RowSnapshot {
	return mxSnapshot(map[string]*TableRows{
		"agents": mxTable([]string{"id"}, agentCols,
			mxRow("id", "1", "name", "Planner", "updated_at", "T0", "builtin", "true"),
			mxRow("id", "2", "name", "CANARY-D2-ROW mine", "updated_at", "T0", "builtin", "false")),
		"notes": mxTable([]string{"id"}, []string{"id", "text"},
			mxRow("id", "7", "text", "CANARY-D2-ROW note")),
	})
}

func mxValueEntry(tbl, id, col string, b, a *string, class string) ExpectedChange {
	return ExpectedChange{Kind: KindValue, Table: tbl, PK: map[string]string{"id": id}, Column: col,
		Before: mxRaw(b), After: mxRaw(a), Cause: "db/db.go:258", Class: class}
}

func TestUpgradeCompareRowsAcceptsExactlyTheListedChanges(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["1"]["updated_at"] = mxStr("T1")
	delete(after.Tables["notes"].Rows, "7")
	after.Tables["notes"].Rows["8"] = mxRow("id", "8", "text", "new")
	changes := []ExpectedChange{
		mxValueEntry("agents", "1", "updated_at", mxStr("T0"), mxStr(AfterStartup), ClassIntended),
		{Kind: KindDeleted, Table: "notes", PK: map[string]string{"id": "7"}, Cause: "db/db.go:280", Class: ClassIntended},
		{Kind: KindAdded, Table: "notes", Count: 1, Cause: "db/schema.sql:712-717", Class: ClassIntended},
	}
	res := CompareRows(mxBefore(), after, changes, func(string) (bool, error) { return true, nil })
	if !res.OK() {
		t.Fatalf("an exact list was refused:\n%s", res.Report())
	}
}

func TestUpgradeCompareRowsReportsWhatTheFileDoesNotAccountFor(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["1"]["name"] = mxStr("CANARY-D2-ROW changed")
	delete(after.Tables["notes"].Rows, "7")
	after.Tables["notes"].Rows["8"] = mxRow("id", "8", "text", "new")
	after.Tables["notes"].Rows["9"] = mxRow("id", "9", "text", "new")
	res := CompareRows(mxBefore(), after, nil, nil)
	got := strings.Join(res.Problems, "\n")
	for _, want := range []string{
		"table agents row id=1: column name changed, no entry",
		"table notes row id=7: row deleted, no entry",
		"table notes: 2 row(s) added, no entry",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
	if res.OK() {
		t.Fatal("a difference that no entry accounts for was accepted")
	}
}

func TestUpgradeCompareRowsChecksBeforeAndAfterValuesAndStaleEntries(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["1"]["name"] = mxStr("Renamed")
	res := CompareRows(mxBefore(), after, []ExpectedChange{
		mxValueEntry("agents", "1", "name", mxStr("Wrong"), mxStr("Renamed"), ClassIntended),
		mxValueEntry("agents", "2", "name", mxStr("CANARY-D2-ROW mine"), mxStr("Other"), ClassIntended),
	}, nil)
	got := strings.Join(res.Problems, "\n")
	if !strings.Contains(got, "the value before differs from the entry") {
		t.Errorf("a wrong before value was not reported:\n%s", got)
	}
	if !strings.Contains(got, "entry 2 (value agents.name) was not confirmed by the run") {
		t.Errorf("a stale entry was not reported:\n%s", got)
	}
	res = CompareRows(mxBefore(), after, []ExpectedChange{
		mxValueEntry("agents", "1", "name", mxStr("Planner"), mxStr("Not renamed"), ClassIntended),
	}, nil)
	if !strings.Contains(strings.Join(res.Problems, "\n"), "the value after differs from the entry") {
		t.Errorf("a wrong after value was not reported:\n%s", res.Report())
	}
}

func TestUpgradeCompareRowsChecksTheCountOfAddedRows(t *testing.T) {
	after := mxBefore()
	after.Tables["notes"].Rows["8"] = mxRow("id", "8", "text", "new")
	res := CompareRows(mxBefore(), after, []ExpectedChange{
		{Kind: KindAdded, Table: "notes", Count: 2, Cause: "db/schema.sql:712-717", Class: ClassIntended},
	}, nil)
	if got := strings.Join(res.Problems, "\n"); !strings.Contains(got, "1 row(s) added, the entry says 2") {
		t.Errorf("a wrong count was not reported:\n%s", got)
	}
}

func TestUpgradeCompareRowsDefectEntriesAwaitTheUserAndStayRed(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["2"]["name"] = mxStr("Overwritten")
	res := CompareRows(mxBefore(), after, []ExpectedChange{
		mxValueEntry("agents", "2", "name", mxStr("CANARY-D2-ROW mine"), mxStr("Overwritten"), ClassDefect),
	}, nil)
	if res.OK() || len(res.Problems) != 0 || len(res.Awaiting) != 1 {
		t.Fatalf("a confirmed defect entry must fail without a problem (problems %d, awaiting %d)", len(res.Problems), len(res.Awaiting))
	}
	if !strings.Contains(res.Report(), "awaiting user decision") {
		t.Errorf("the report does not say the entry awaits a user decision:\n%s", res.Report())
	}
	// A confirmed entry of another class is quiet.
	res = CompareRows(mxBefore(), after, []ExpectedChange{
		mxValueEntry("agents", "2", "name", mxStr("CANARY-D2-ROW mine"), mxStr("Overwritten"), ClassAccepted),
	}, nil)
	if !res.OK() {
		t.Errorf("an accepted entry was refused:\n%s", res.Report())
	}
}

func TestUpgradeCompareRowsStartupMarkerNeedsAFreshTimestamp(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["1"]["updated_at"] = mxStr("T-old")
	entry := mxValueEntry("agents", "1", "updated_at", mxStr("T0"), mxStr(AfterStartup), ClassIntended)
	res := CompareRows(mxBefore(), after, []ExpectedChange{entry}, func(string) (bool, error) { return false, nil })
	if got := strings.Join(res.Problems, "\n"); !strings.Contains(got, "not a timestamp from startup") {
		t.Errorf("a stale timestamp was accepted for @startup:\n%s", got)
	}
}

func TestUpgradeCompareRowsColumnAddedEntries(t *testing.T) {
	b := mxSnapshot(map[string]*TableRows{"p": mxTable([]string{"id"}, []string{"id"}, mxRow("id", "1"))})
	a := mxSnapshot(map[string]*TableRows{"p": mxTable([]string{"id"}, []string{"id", "kind"}, mxRow("id", "1", "kind", "x"))})
	if res := CompareRows(b, a, nil, nil); len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "column kind is new and has a value, no entry") {
		t.Fatalf("a new column without an entry was not reported:\n%s", res.Report())
	}
	ok := ExpectedChange{Kind: KindValue, Table: "p", PK: map[string]string{"id": "1"}, Column: "kind",
		ColumnAdded: true, After: mxRaw(mxStr("x")), Cause: "db/schema.sql:478-490", Class: ClassIntended}
	if res := CompareRows(b, a, []ExpectedChange{ok}, nil); !res.OK() {
		t.Fatalf("a column_added entry was refused:\n%s", res.Report())
	}
	// A column that is gone is always a problem.
	if res := CompareRows(a, b, nil, nil); len(res.Problems) == 0 || !strings.Contains(res.Problems[0], "column kind removed") {
		t.Fatalf("a removed column was not reported:\n%s", res.Report())
	}
}

// AC12 at unit level: nothing a comparison prints holds a value; a text key is hashed.
func TestUpgradeCompareRowsReportNeverHoldsValues(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["2"]["name"] = mxStr("CANARY-D2-ROW changed")
	after.Tables["agents"].Rows["text-key"] = mxRow("id", "CANARY-D2-ROW-key", "name", "x", "updated_at", "T0", "builtin", "true")
	res := CompareRows(mxBefore(), after, []ExpectedChange{
		mxValueEntry("agents", "2", "name", mxStr("CANARY-D2-ROW wrong"), mxStr("CANARY-D2-ROW wrong"), ClassDefect),
		{Kind: KindDeleted, Table: "agents", PK: map[string]string{"id": "CANARY-D2-ROW-key"}, Cause: "db/db.go:280", Class: ClassDefect},
	}, nil)
	AssertNoSecret(t, "report", res.Report())
	if len(res.Problems) == 0 {
		t.Fatal("expected problems")
	}
	if got := mxBefore().Tables["agents"].DescribeKey("CANARY-D2-ROW-key"); strings.Contains(got, "CANARY") || !strings.Contains(got, "id=text#") {
		t.Errorf("a text key must be reduced to a hash, got %q", got)
	}
}

func TestUpgradeRepeatProblemsPermitOnlyBuiltinAgentUpdatedAt(t *testing.T) {
	after := mxBefore()
	after.Tables["agents"].Rows["1"]["updated_at"] = mxStr("T1")
	if p := RepeatProblems(mxBefore(), after); len(p) != 0 {
		t.Errorf("agents.updated_at of a built-in agent was refused: %v", p)
	}
	after.Tables["agents"].Rows["1"]["name"] = mxStr("x")
	if p := RepeatProblems(mxBefore(), after); len(p) != 1 || !strings.Contains(p[0], "column name changed") {
		t.Errorf("another column of a built-in agent was permitted: %v", p)
	}
	after = mxBefore()
	after.Tables["agents"].Rows["2"]["updated_at"] = mxStr("T1") // not built-in
	if p := RepeatProblems(mxBefore(), after); len(p) != 1 {
		t.Errorf("updated_at of a custom agent was permitted: %v", p)
	}
	after = mxBefore()
	after.Tables["notes"].Rows["9"] = mxRow("id", "9", "text", "new")
	if p := RepeatProblems(mxBefore(), after); len(p) != 1 || !strings.Contains(p[0], "row added") {
		t.Errorf("an added row was permitted: %v", p)
	}
}

func TestUpgradeExpectedChangesReaderRejectsMalformedEntries(t *testing.T) {
	ok := `{"note":"x","shapes":{"s":[{"kind":"added","table":"t","count":1,"cause":"db/db.go:1","class":"intended"}]}}`
	if _, err := ReadExpectedChanges(strings.NewReader(ok)); err != nil {
		t.Fatalf("a valid file was refused: %v", err)
	}
	cases := map[string]string{
		"unknown field":             `{"shapes":{"s":[{"kind":"added","table":"t","count":1,"cause":"db/db.go:1","class":"intended","why":"x"}]}}`,
		"no cause":                  `{"shapes":{"s":[{"kind":"added","table":"t","count":1,"class":"intended"}]}}`,
		"cause without a line":      `{"shapes":{"s":[{"kind":"added","table":"t","count":1,"cause":"db/db.go","class":"intended"}]}}`,
		"unknown class":             `{"shapes":{"s":[{"kind":"added","table":"t","count":1,"cause":"db/db.go:1","class":"fine"}]}}`,
		"unknown kind":              `{"shapes":{"s":[{"kind":"column","table":"t","cause":"db/db.go:1","class":"intended"}]}}`,
		"whole-column value entry":  `{"shapes":{"s":[{"kind":"value","table":"t","column":"c","before":"a","after":"b","cause":"db/db.go:1","class":"intended"}]}}`,
		"whole-table deleted entry": `{"shapes":{"s":[{"kind":"deleted","table":"t","cause":"db/db.go:1","class":"intended"}]}}`,
		"value without before":      `{"shapes":{"s":[{"kind":"value","table":"t","pk":{"id":"1"},"column":"c","after":"b","cause":"db/db.go:1","class":"intended"}]}}`,
		"added without count":       `{"shapes":{"s":[{"kind":"added","table":"t","cause":"db/db.go:1","class":"intended"}]}}`,
		"added with a pk":           `{"shapes":{"s":[{"kind":"added","table":"t","count":1,"pk":{"id":"1"},"cause":"db/db.go:1","class":"intended"}]}}`,
		"non-string value":          `{"shapes":{"s":[{"kind":"value","table":"t","pk":{"id":"1"},"column":"c","before":1,"after":"b","cause":"db/db.go:1","class":"intended"}]}}`,
		"column_added with before":  `{"shapes":{"s":[{"kind":"value","table":"t","pk":{"id":"1"},"column":"c","column_added":true,"before":"a","after":"b","cause":"db/db.go:1","class":"intended"}]}}`,
		"the same change twice":     `{"shapes":{"s":[{"kind":"added","table":"t","count":1,"cause":"db/db.go:1","class":"intended"},{"kind":"added","table":"t","count":2,"cause":"db/db.go:1","class":"intended"}]}}`,
		"value without after":       `{"shapes":{"s":[{"kind":"value","table":"t","pk":{"id":"1"},"column":"c","before":"a","cause":"db/db.go:1","class":"intended"}]}}`,
	}
	for name, doc := range cases {
		if _, err := ReadExpectedChanges(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	nulls := `{"shapes":{"s":[{"kind":"value","table":"t","pk":{"id":"1"},"column":"c","before":null,"after":"b","cause":"db/db.go:1; db/db.go:5-7","class":"accepted"}]}}`
	if _, err := ReadExpectedChanges(strings.NewReader(nulls)); err != nil {
		t.Errorf("a NULL before value was refused: %v", err)
	}
}

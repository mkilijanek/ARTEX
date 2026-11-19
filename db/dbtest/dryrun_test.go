package dbtest

import (
	"strings"
	"testing"
)

// AC15: the dry run refuses to touch a server that holds anything but
// template0, template1, postgres and databases named *_upgradecheck.
func TestUpgradeDryRunInventoryRefusal(t *testing.T) {
	ok := [][]string{
		{"postgres", "template0", "template1"},
		{"postgres", "template0", "template1", "prod_upgradecheck"},
		{"postgres", "template0", "template1", "a_upgradecheck", "b_upgradecheck"},
	}
	for _, names := range ok {
		if err := DryRunInventoryRefusal(names); err != nil {
			t.Errorf("%v refused: %v", names, err)
		}
	}
	refused := [][]string{
		{"postgres", "template0", "template1", "artex"},
		{"postgres", "template0", "template1", "artextest"},
		{"postgres", "template0", "template1", "x_upgradecheck", "artex"},
		{"postgres", "template0", "template1", "_upgradecheck"},       // suffix only
		{"postgres", "template0", "template1", "x_upgradecheck_2"},    // not a suffix
		{"postgres", "template0", "template1", "X_upgradecheck"},      // upper case
		{"postgres", "template0", "template1", `x"; --_upgradecheck`}, // outside the name set
	}
	for _, names := range refused {
		err := DryRunInventoryRefusal(names)
		if err == nil {
			t.Errorf("%v accepted", names)
			continue
		}
		if strings.Contains(err.Error(), "postgres://") || strings.Contains(err.Error(), "password") {
			t.Errorf("message looks like a DSN: %v", err)
		}
	}
}

func TestUpgradeDryRunTargetRefusal(t *testing.T) {
	if err := DryRunTargetRefusal("prod_upgradecheck"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"artex", "postgres", "_upgradecheck", "prod_upgradecheck_x", "prod upgradecheck", strings.Repeat("a", 60) + "_upgradecheck"} {
		if err := DryRunTargetRefusal(n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestUpgradeDryRunReportIsContentFree(t *testing.T) {
	r := Report{
		Database: "prod_upgradecheck",
		Before:   map[string]int64{"assets": 3, "tasks": 2, "gone": 1},
		After:    map[string]int64{"assets": 3, "tasks": 1, "new_table": 0},
		Catalog:  DiffLines([]string{Entry{"table", "a", "x"}.Line()}, nil),
	}
	out := r.String()
	for _, want := range []string{
		"database prod_upgradecheck", "table assets: 3 -> 3 rows", "table tasks: 2 -> 1 rows",
		"table gone: 1 rows -> absent", "table new_table: absent -> 0 rows", "catalog: missing table a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if got := strings.Join(r.RemovedRows(), ","); got != "gone,tasks" {
		t.Errorf("RemovedRows = %q", got)
	}
	AssertNoSecret(t, "dry-run report", out)
	if (Report{Database: "x"}).String() == "" {
		t.Error("empty report renders nothing")
	}
}

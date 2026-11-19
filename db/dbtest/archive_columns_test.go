package dbtest

import (
	"strings"
	"testing"
)

func archiveCol(typ string, notNull bool) ArchiveColumn {
	return ArchiveColumn{Type: typ, NotNull: notNull}
}

// The column guard of AC8(b) must itself fail when an archived column breaks: dropped,
// retyped, nullability changed, a new NOT NULL column without a restore default, an
// archived table that is gone. A table that is new (absent from the old shape) and a
// new nullable column are not violations.
func TestUpgradeArchiveColumnGuardReportsEveryBreakage(t *testing.T) {
	old := map[string]map[string]ArchiveColumn{
		"tasks":    {"id": archiveCol("bigint", true), "name": archiveCol("text", true), "note": archiveCol("text", false)},
		"findings": {"id": archiveCol("bigint", true)},
		"gone":     {"id": archiveCol("bigint", true)},
	}
	cur := map[string]map[string]ArchiveColumn{
		"tasks": {
			"id":       archiveCol("bigint", true),
			"name":     archiveCol("integer", true), // retyped
			"note":     archiveCol("text", true),    // nullability changed
			"added_ok": archiveCol("text", false),   // new nullable: fine
			"added_nn": archiveCol("text", true),    // new NOT NULL, no default
			"added_df": archiveCol("text", true),    // new NOT NULL, has a restore default
		},
		"findings":  {}, // id dropped
		"new_table": {"id": archiveCol("bigint", true)},
	}
	got := ArchiveColumnGuard(old, cur, map[string]string{"tasks.added_df": "normalizer"})
	// Each violation line starts with its subject.
	want := []string{"table gone", "findings.id", "tasks.name", "tasks.note", "tasks.added_nn"}
	for _, w := range want {
		found := false
		for _, g := range got {
			if strings.HasPrefix(g, w+":") || strings.HasPrefix(g, w+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("no violation for %q in %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d violations %v, want %d", len(got), got, len(want))
	}
	if v := ArchiveColumnGuard(old, old, nil); len(v) != 0 {
		t.Errorf("identical shapes reported violations: %v", v)
	}
}

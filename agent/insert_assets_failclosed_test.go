package agent

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/db"
)

// dropDatabase drops a database by name through a plain admin connection. The connection goes
// to the postgres maintenance database, as db.Open does, because the base test database may
// not exist yet.
func dropDatabase(base, name string) error {
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	u.Path = "/postgres"
	admin, err := sql.Open("pgx", u.String())
	if err != nil {
		return err
	}
	defer admin.Close()
	_, err = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	return err
}

// gateFixtureDB opens a private sibling of the test database (<dbname>_insert_gate) for one
// subtest. Tables may be altered only inside it, so the shared schema is never touched. Any
// leftover from an earlier run is dropped first; the database is dropped again on cleanup.
func gateFixtureDB(t *testing.T) *db.DB {
	t.Helper()
	base, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/") + "_insert_gate"
	u.Path = "/" + name
	if err := dropDatabase(base, name); err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() {
		if err := dropDatabase(base, name); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	d, err := db.Open(u.String())
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	// Registered after the drop, so it runs first: the connection is closed before the drop.
	t.Cleanup(func() { d.Close() })
	return d
}

// breakTable renames a table inside the private fixture database so that reading it fails
// the way a DB outage on that one query would, while every other statement still works.
func breakTable(t *testing.T, d *db.DB, table string) {
	t.Helper()
	if _, err := d.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_t_ins_1`); err != nil {
		t.Fatalf("cannot break %s for the fixture: %v", table, err)
	}
}

// T-INS-1 (AC21, C7): the insert_assets asset gate fails closed. When the rules cannot be
// read, no asset is inserted and the caller is told why, instead of the gate being skipped.
func TestInsertAssetsGateFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		table  string
		taskID int64
	}{
		{"global rules unreadable", "asset_intercept_rules", 0},
		{"task rules unreadable", "task_intercept_rules", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := gateFixtureDB(t)
			domain := "t-ins-1-failclosed.example"
			ts := NewToolSet(nil, "")
			ts.SetAssetStore(d.Assets(), d.Companies())
			ts.SetTaskID(tc.taskID)
			breakTable(t, d, tc.table)
			logs := captureLog(t)

			raw, _ := json.Marshal(map[string]any{"assets": []any{map[string]any{"type": "root_domain", "domain": domain}}})
			res, err := ts.insertAssets().Call(t.Context(), raw, nil)
			if err != nil {
				t.Fatalf("insert_assets returned a Go error instead of a tool result: %v", err)
			}
			var inserted int
			if err := d.QueryRow(`SELECT COUNT(*) FROM assets WHERE domain=$1`, domain).Scan(&inserted); err != nil {
				t.Fatal(err)
			}
			if inserted != 0 {
				t.Fatalf("asset was inserted although the gate rules could not be read (%d rows); result: %s", inserted, truncateForLog(res.Flatten()))
			}
			var out struct {
				Results []any `json:"results"`
				Errors  []any `json:"errors"`
			}
			_ = json.Unmarshal([]byte(res.Flatten()), &out)
			if !res.IsError && len(out.Errors) == 0 {
				t.Fatalf("no error reported to the caller for the refused asset: %s", truncateForLog(res.Flatten()))
			}
			if len(out.Results) != 0 {
				t.Fatalf("results reported for a refused asset: %s", truncateForLog(res.Flatten()))
			}
			if txt := res.Flatten(); strings.Contains(txt, "_t_ins_1") || strings.Contains(txt, "does not exist") || strings.Contains(txt, "relation") {
				t.Fatalf("the driver error reached the caller: %s", truncateForLog(txt))
			}
			line := logs.String()
			if !strings.Contains(line, "[assets] gate_unavailable") {
				t.Fatalf("no [assets] gate_unavailable log line: %q", line)
			}
			if strings.Contains(line, domain) {
				t.Fatalf("gate_unavailable log line carries the asset: %q", line)
			}
		})
	}
}

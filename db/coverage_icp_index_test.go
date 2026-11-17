package db

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestCoverageICPIndexUpgradeIsIdempotentAndPreservesScope(t *testing.T) {
	// Own database: dropping the index must not affect another test's workload.
	u, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "_coverage_icp_upgrade"
	dsn := u.String()
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	task, err := d.CreateTask("ICP index upgrade", "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	defer func() {
		if d == nil {
			return
		}
		for _, id := range ids {
			_, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, id)
		}
		_ = d.DeleteTask(task.ID)
		_, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, task.ExplorationID)
	}()
	var root, app, other int64
	if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,icp) VALUES('root_domain',$1,$1,' AB 123 ') RETURNING id`, fmt.Sprintf("icp-upgrade-%d.invalid", task.ID)).Scan(&root); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, root)
	if err := d.QueryRow(`INSERT INTO assets(type,app_name,app_icp) VALUES('app',$1,'ab123') RETURNING id`, fmt.Sprintf("ICP match %d", task.ID)).Scan(&app); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, app)
	if err := d.QueryRow(`INSERT INTO assets(type,app_name,app_icp) VALUES('app',$1,'ab124') RETURNING id`, fmt.Sprintf("ICP outside %d", task.ID)).Scan(&other); err != nil {
		t.Fatal(err)
	}
	ids = append(ids, other)
	if _, err := d.Exec(`INSERT INTO task_scope(task_id,kind,value,source) VALUES($1,'icp','ab123','manual')`, task.ID); err != nil {
		t.Fatal(err)
	}
	graph := func() []byte {
		t.Helper()
		g, err := d.Assets().BuildCoverageGraph(task.ID, task.ExplorationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(g.Nodes) != 2 {
			t.Fatalf("ICP matching changed: %+v", g.Nodes)
		}
		for _, n := range g.Nodes {
			if !n.InScope || (n.AssetID != root && n.AssetID != app) {
				t.Fatal("scope widened/metadata lost", n)
			}
		}
		return canonicalCoverageGraph(g)
	}
	original := graph()
	assertIndex := func() {
		t.Helper()
		var definition string
		if err := d.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema() AND tablename='task_scope' AND indexname='idx_ts_icp'`).Scan(&definition); err != nil {
			t.Fatal("ICP index missing", err)
		}
		if !strings.Contains(definition, "(value)") || !strings.Contains(definition, "kind = 'icp'") {
			t.Fatal("incorrect ICP partial index", definition)
		}
	}
	assertIndex()
	if _, err := d.Exec(`DROP INDEX idx_ts_icp`); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, graph()) {
		t.Fatal("index removal changed graph semantics")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	assertIndex()
	if !bytes.Equal(original, graph()) {
		t.Fatal("existing-store upgrade changed graph")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	assertIndex()
	if !bytes.Equal(original, graph()) {
		t.Fatal("repeated startup changed graph")
	}
	selected, err := d.Assets().GetByIDsInScope(task.ID, ids)
	if err != nil || len(selected) != 2 {
		t.Fatal("ICP authorization selection changed", len(selected), err)
	}
}

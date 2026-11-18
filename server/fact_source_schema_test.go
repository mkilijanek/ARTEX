package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// T-SCHEMA-1 (AC19, C4; PostgreSQL): on an existing database whose stored record_fact row
// predates the source field and was edited by the operator, the startup seeders add the
// source enum once, keep the operator's description text, and a second run changes
// nothing. ToolResolve then serves the source property to the worker.
func TestFactSourceSchemaIdempotent(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatalf("ARTEX_PG_DSN is set but unusable: %v", err)
		}
		t.Skipf("no database config (%v)", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatalf("ARTEX_PG_DSN is set but postgres is unreachable: %v", err)
		}
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { pg.Close() })
	m := &Manager{pg: pg}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{m: m, ctx: ctx}
	prevResolve := agent.ToolResolve()
	wireTools(pg, nil) // seeds the catalog rows and installs ToolResolve, as at startup
	t.Cleanup(func() { agent.SetToolResolve(prevResolve) })

	// Run the seeders once first, as a normal startup would: the one-time refresh flags are
	// then set, so only the additive source upgrade can touch the edited row below.
	s.seedOrchestrationTools()

	old, err := pg.GetTool("record_fact")
	if err != nil || old == nil {
		t.Fatalf("record_fact is not seeded: %v", err)
	}
	bindings, _ := json.Marshal(old.Agents)
	t.Cleanup(func() { _ = pg.UpdateTool(old.Key, old.Description, old.Schema, bindings, old.Enabled) })

	// A pre-provenance row customised by the operator: no source property, an edited
	// description and an edited parameter description.
	var schema map[string]any
	if err := json.Unmarshal(old.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	delete(props, "source")
	props["confidence"].(map[string]any)["description"] = "operator confidence text"
	custom, _ := json.Marshal(schema)
	if err := pg.UpdateTool("record_fact", "operator description", custom, bindings, true); err != nil {
		t.Fatal(err)
	}
	if src := resolvedRecordFactProps(t)["source"]; src != nil {
		t.Fatalf("precondition: the stored schema should not expose source yet: %v", src)
	}

	snapshot := func() (string, string, time.Time) {
		var desc, sch string
		var upd time.Time
		if err := pg.QueryRow(`SELECT description, schema::text, updated_at FROM tools WHERE key='record_fact'`).Scan(&desc, &sch, &upd); err != nil {
			t.Fatal(err)
		}
		return desc, sch, upd
	}
	s.seedOrchestrationTools()
	desc1, sch1, upd1 := snapshot()
	s.seedOrchestrationTools()
	desc2, sch2, upd2 := snapshot()
	if desc1 != desc2 || sch1 != sch2 || !upd1.Equal(upd2) {
		t.Fatalf("the second run changed the row: %q %s %v -> %q %s %v", desc1, sch1, upd1, desc2, sch2, upd2)
	}

	tool, err := pg.GetTool("record_fact")
	if err != nil || tool == nil {
		t.Fatal(err)
	}
	var got struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema, &got); err != nil {
		t.Fatal(err)
	}
	// operator_input is reserved for a future UI/API path; models are never offered it.
	wantEnum := "[target_response tool_output third_party_intel inference]"
	if src := got.Properties["source"]; src == nil || fmt.Sprint(src["enum"]) != wantEnum {
		t.Fatalf("the refresh did not add the source enum %s: %s", wantEnum, tool.Schema)
	}
	if tool.Description != "operator description" || got.Properties["confidence"]["description"] != "operator confidence text" {
		t.Fatalf("operator-edited text was overwritten: %q %s", tool.Description, tool.Schema)
	}
	if len(got.Properties) != len(props)+1 {
		t.Fatalf("the refresh added %d properties, want exactly source: %s", len(got.Properties)-len(props), tool.Schema)
	}

	resolved := resolvedRecordFactProps(t)
	src, _ := resolved["source"].(map[string]any)
	if src == nil || fmt.Sprint(src["enum"]) != wantEnum {
		t.Fatalf("ToolResolve does not serve the source enum to the worker: %v", resolved)
	}
}

// resolvedRecordFactProps returns the properties of record_fact as ToolResolve serves it
// to the worker.
func resolvedRecordFactProps(t *testing.T) map[string]any {
	t.Helper()
	tools := agent.ToolResolve()(context.Background(), "worker", agent.NewToolSet(nil, "").WorkerTools())
	for _, tl := range tools {
		if tl.Name() == "record_fact" {
			props, _ := tl.InputSchema()["properties"].(map[string]any)
			return props
		}
	}
	t.Fatal("the worker lost record_fact")
	return nil
}

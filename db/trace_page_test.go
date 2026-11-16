package db

import "testing"

func TestTracePageInheritedOwnershipAndReopen(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	source, err := d.CreateTask("trace source", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(source.ID)
	current, err := d.CreateTaskWithOptions("trace current", "goal", TaskCreateOptions{SourceTaskIDs: []int64{source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(current.ID)
	s := d.Exploration(source.ExplorationID)
	intent, err := s.AddIntent(map[string]any{"summary": "source"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"thinking", "tool_result"} {
		if _, err := s.AppendActivity(Activity{NodeID: &intent, Kind: kind, Summary: "search", Detail: "evidence"}); err != nil {
			t.Fatal(err)
		}
	}
	c := d.Exploration(current.ExplorationID)
	rows, _, err := c.ActivityTracePageWithSources(intent, 0, "search", 2)
	if err != nil || len(rows) != 1 || !rows[0].Inherited || rows[0].SourceTaskID != source.ID {
		t.Fatalf("inherited page: %+v %v", rows, err)
	}
	if err := s.SetIntentState(intent, "running"); err != nil {
		t.Fatal(err)
	}
	rows, _, err = c.ActivityTracePageWithSources(intent, 0, "", 2)
	if err != nil || len(rows) != 0 {
		t.Fatalf("reopened source leaked: %+v %v", rows, err)
	}
}

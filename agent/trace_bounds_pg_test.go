package agent

import (
	"encoding/json"
	"github.com/Autumn-27/artex/db"
	"strings"
	"testing"
)

func TestBoundedTraceReconstructsStoredEvidence(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("trace pages", "test", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	intent, err := store.AddIntent(map[string]any{"summary": "test"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("秘密\x01", 5000)
	// PostgreSQL text permits this control byte but rejects NUL.
	step, err := store.AppendActivity(db.Activity{NodeID: &intent, Worker: "worker", Kind: "tool_result", Tool: strings.Repeat("\x01", 2000), Detail: text})
	if err != nil {
		t.Fatal(err)
	}
	tools := NewToolSet(store, "worker")
	var all strings.Builder
	for offset := 0; offset < len(text); {
		in, _ := json.Marshal(map[string]any{"intent_id": intent, "step_ids": []int64{step}, "offset": offset})
		res := callReadJSON(t, tools.getWorkerTrace(), string(in)).(map[string]any)
		encoded, _ := json.Marshal(res)
		if len(encoded) > 64*1024 {
			t.Fatal("response budget exceeded")
		}
		s := res["steps"].([]any)[0].(map[string]any)
		all.WriteString(s["detail"].(string))
		next := int(s["next_offset"].(float64))
		if next <= offset {
			t.Fatal("no progress")
		}
		offset = next
	}
	if all.String() != text {
		t.Fatal("paged evidence changed")
	}
	stored, err := store.ActivityDetail(step)
	if err != nil || stored != text {
		t.Fatal("storage was truncated")
	}
	for range 3 {
		if _, err := store.AppendActivity(db.Activity{NodeID: &intent, Kind: "text", Summary: "page"}); err != nil {
			t.Fatal(err)
		}
	}
	first := callReadJSON(t, tools.getWorkerTrace(), `{"intent_id":`+jsonNumber(intent)+`,"limit":2}`).(map[string]any)
	if first["has_more"] != true {
		t.Fatal("missing page cursor")
	}
	before := int64(first["next_before_step_id"].(float64))
	second := callReadJSON(t, tools.getWorkerTrace(), `{"intent_id":`+jsonNumber(intent)+`,"limit":2,"before_step_id":`+jsonNumber(before)+`}`).(map[string]any)
	for _, v := range second["steps"].([]any) {
		if int64(v.(map[string]any)["step_id"].(float64)) >= before {
			t.Fatal("page overlapped")
		}
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }

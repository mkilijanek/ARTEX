package server

import (
	"encoding/json"
	"github.com/Autumn-27/artex/db"
	"net/http/httptest"
	"testing"
)

func TestUsageDimensionsHTTP(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.pg == nil {
		t.Skip("requires isolated PostgreSQL")
	}
	task := "dimensions-http-fixture"
	defer m.pg.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, task)
	for i := range 2 {
		if err := m.pg.InsertLLMUsage(&db.LLMUsage{TaskID: task, CallID: "same", Purpose: "agent", Round: 1, Attempt: i, ToolBytes: map[string]db.ToolBytes{"Read": {Result: 9}}}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.pgUsageDimensions(w, httptest.NewRequest("GET", "/api/tokens/dimensions?task="+task+"&purpose=agent&limit=1", nil))
	var got struct {
		Rows []db.UsageDimension `json:"rows"`
		Next int64               `json:"next_after"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(got.Rows) != 1 || got.Next != got.Rows[0].ID || got.Rows[0].ToolBytes["Read"].Result != 9 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

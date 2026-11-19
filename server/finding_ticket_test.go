package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/intercept"
)

// ticketAPI is the harness for the finding ticket id tests: the traffic evidence
// fixture (one task, one finding with a node) and request helpers.
type ticketAPI struct {
	*commentAPI
	taskID int64
	nodeID int64
}

func newTicketAPI(t *testing.T) *ticketAPI {
	t.Helper()
	a := newCommentAPI(t)
	f, err := a.s.m.pg.GetFinding(a.finding)
	if err != nil || f == nil || f.TaskID == nil || f.NodeID == nil {
		t.Fatalf("fixture finding: %+v %v", f, err)
	}
	return &ticketAPI{commentAPI: a, taskID: *f.TaskID, nodeID: *f.NodeID}
}

func (a *ticketAPI) path() string { return fmt.Sprintf("/api/exploration/findings/%d", a.finding) }

func (a *ticketAPI) patch(body string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("PATCH", a.path(), "application/json", body, "")
}

func (a *ticketAPI) patchTicket(value string) *httptest.ResponseRecorder {
	a.t.Helper()
	b, err := json.Marshal(map[string]string{"ticket_id": value})
	if err != nil {
		a.t.Fatal(err)
	}
	return a.patch(string(b))
}

func (a *ticketAPI) stored() string {
	a.t.Helper()
	var v string
	if err := a.s.m.pg.QueryRow(`SELECT ticket_id FROM findings WHERE id=$1`, a.finding).Scan(&v); err != nil {
		a.t.Fatal(err)
	}
	return v
}

func (a *ticketAPI) events() int {
	a.t.Helper()
	var n int
	if err := a.s.m.pg.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, a.finding).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

// ticketField decodes a finding JSON object and returns its ticket_id, failing when
// the key is absent (the frontend relies on it always being present).
func ticketField(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a finding object: %v: %s", err, raw)
	}
	v, ok := m["ticket_id"]
	if !ok {
		t.Fatalf("ticket_id key missing: %s", raw)
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		t.Fatalf("ticket_id is not a string: %s", v)
	}
	return s
}

// ticketInList finds the finding in a list response (bare array or {items}) and returns its ticket_id.
func ticketInList(t *testing.T, raw []byte, findingID int64) string {
	t.Helper()
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		var paged struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &paged); err != nil {
			t.Fatalf("not a list: %v: %s", err, raw)
		}
		items = paged.Items
	}
	for _, item := range items {
		var id struct {
			FindingID string `json:"finding_id"`
		}
		if err := json.Unmarshal(item, &id); err != nil {
			t.Fatal(err)
		}
		if id.FindingID == strconv.FormatInt(findingID, 10) {
			return ticketField(t, item)
		}
	}
	t.Fatalf("finding %d not in list: %s", findingID, raw)
	return ""
}

func TestPatchFindingTicketIDStoresAndReturns(t *testing.T) {
	a := newTicketAPI(t)
	if got := ticketField(t, a.get(a.path()).Body.Bytes()); got != "" {
		t.Fatalf("a new finding has ticket_id %q, want empty", got)
	}

	w := a.patchTicket("SEC-1234")
	if w.Code != 200 || ticketField(t, w.Body.Bytes()) != "SEC-1234" {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	if a.stored() != "SEC-1234" {
		t.Fatalf("stored %q", a.stored())
	}
	if got := ticketField(t, a.get(a.path()).Body.Bytes()); got != "SEC-1234" {
		t.Fatalf("detail: %q", got)
	}
	if got := ticketInList(t, a.get("/api/exploration/findings").Body.Bytes(), a.finding); got != "SEC-1234" {
		t.Fatalf("bare list: %q", got)
	}
	if got := ticketInList(t, a.get("/api/exploration/findings?page=1&limit=200").Body.Bytes(), a.finding); got != "SEC-1234" {
		t.Fatalf("paged list: %q", got)
	}
	if got := ticketInList(t, a.get("/api/exploration/findings?task="+strconv.FormatInt(a.taskID, 10)).Body.Bytes(), a.finding); got != "SEC-1234" {
		t.Fatalf("per-task list: %q", got)
	}

	// Surrounding whitespace is trimmed.
	w = a.patchTicket("  SEC-1  ")
	if w.Code != 200 || ticketField(t, w.Body.Bytes()) != "SEC-1" || a.stored() != "SEC-1" {
		t.Fatalf("trim: %d %s stored=%q", w.Code, w.Body, a.stored())
	}
	// Unicode text, exactly 128 code points.
	long := strings.Repeat("ż", 128)
	if w = a.patchTicket(long); w.Code != 200 || a.stored() != long {
		t.Fatalf("128 code points: %d %s", w.Code, w.Body)
	}
	// The empty string clears.
	w = a.patchTicket("")
	if w.Code != 200 || ticketField(t, w.Body.Bytes()) != "" || a.stored() != "" {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if got := ticketField(t, a.get(a.path()).Body.Bytes()); got != "" {
		t.Fatalf("detail after clear: %q", got)
	}
}

func TestPatchFindingTicketIDRejectsInvalidValues(t *testing.T) {
	a := newTicketAPI(t)
	logs := captureLog(t)
	if w := a.patchTicket("KEEP-1"); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	const canary = "TKCANARY-7e41"
	for name, value := range map[string]string{
		"line feed":       canary + "\nSEC-2",
		"carriage return": canary + "\rSEC-2",
		"tab":             canary + "\tSEC-2",
		"nul":             canary + "\x00",
		"escape":          canary + "\x1b[31m",
		"line separator":  canary + " x",
		"too long":        canary + strings.Repeat("a", 128),
	} {
		w := a.patchTicket(value)
		if w.Code != 400 {
			t.Fatalf("%s: status %d body %s", name, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), canary) {
			t.Fatalf("%s: the response echoes the value: %s", name, w.Body)
		}
		if got := errField(t, w); got != "invalid ticket_id: must be one line of text, at most 128 characters" {
			t.Fatalf("%s: message %q", name, got)
		}
		if a.stored() != "KEEP-1" {
			t.Fatalf("%s: stored value changed to %q", name, a.stored())
		}
	}
	// A wrong JSON type is rejected without echoing the value either.
	if w := a.patch(`{"ticket_id":{"` + canary + `":1}}`); w.Code != 400 || strings.Contains(w.Body.String(), canary) {
		t.Fatalf("wrong type: %d %s", w.Code, w.Body)
	}
	if a.stored() != "KEEP-1" {
		t.Fatalf("stored value changed to %q", a.stored())
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatalf("the log contains the rejected value: %q", logs)
	}

	// An invalid ticket id next to a valid status must not apply the status.
	before := a.get(a.path()).Body.String()
	var cur struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(before), &cur); err != nil {
		t.Fatal(err)
	}
	if w := a.patch(`{"status":"confirmed","ticket_id":"bad\nvalue"}`); w.Code != 400 {
		t.Fatalf("mixed: %d %s", w.Code, w.Body)
	}
	if after := a.get(a.path()).Body.String(); after != before {
		t.Fatalf("a rejected request changed the finding:\n%s\n%s", before, after)
	}

	// Unknown finding.
	r := httptest.NewRequest("PATCH", "/api/exploration/findings/999999999", strings.NewReader(`{"ticket_id":"SEC-1"}`))
	r.Header.Set("Authorization", "Bearer "+a.token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.s.Handler().ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatalf("unknown finding: %d %s", w.Code, w.Body)
	}
}

func TestPatchFindingTicketIDOnlyFieldAndNothingToUpdate(t *testing.T) {
	a := newTicketAPI(t)
	if w := a.patch(`{"ticket_id":"SEC-9"}`); w.Code != 200 {
		t.Fatalf("a patch with only ticket_id: %d %s", w.Code, w.Body)
	}
	w := a.patch(`{}`)
	if w.Code != 400 || !strings.Contains(errField(t, w), "ticket_id") {
		t.Fatalf("empty patch: %d %s", w.Code, w.Body)
	}
	if w := a.patch(`{"ticket_id":null}`); w.Code != 400 {
		t.Fatalf("null ticket_id counts as absent: %d %s", w.Code, w.Body)
	}
}

func TestPatchFindingTicketIDLeavesEverythingElseAndNotifiesNobody(t *testing.T) {
	a := newTicketAPI(t)
	pg := a.s.m.pg
	// Give the finding a distinctive state first.
	if w := a.patch(`{"status":"confirmed","severity":"high","name":"Named","vulnclass":"IDOR"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if _, err := pg.Exec(`UPDATE findings SET report='# Report', evidence='proof' WHERE id=$1`, a.finding); err != nil {
		t.Fatal(err)
	}
	type row struct{ status, severity, name, vulnclass, evidence, report, summary string }
	read := func() (r row) {
		t.Helper()
		if err := pg.QueryRow(`SELECT status,severity,name,vulnclass,evidence,report,summary FROM findings WHERE id=$1`, a.finding).
			Scan(&r.status, &r.severity, &r.name, &r.vulnclass, &r.evidence, &r.report, &r.summary); err != nil {
			t.Fatal(err)
		}
		return
	}
	var payloadBefore string
	if err := pg.QueryRow(`SELECT payload::text FROM exploration_nodes WHERE id=$1`, a.nodeID).Scan(&payloadBefore); err != nil {
		t.Fatal(err)
	}
	before, eventsBefore := read(), a.events()
	if eventsBefore == 0 {
		t.Fatal("control: the status change should have registered a notification event")
	}
	for _, v := range []string{"SEC-5", "SEC-6", ""} {
		if w := a.patchTicket(v); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if after := read(); after != before {
		t.Fatalf("other fields changed: %+v -> %+v", before, after)
	}
	if n := a.events(); n != eventsBefore {
		t.Fatalf("notification events %d -> %d", eventsBefore, n)
	}
	var payloadAfter string
	if err := pg.QueryRow(`SELECT payload::text FROM exploration_nodes WHERE id=$1`, a.nodeID).Scan(&payloadAfter); err != nil {
		t.Fatal(err)
	}
	if payloadAfter != payloadBefore {
		t.Fatalf("node payload changed: %s -> %s", payloadBefore, payloadAfter)
	}
	// And a status change right after still notifies (the ticket update did not break it).
	if w := a.patch(`{"status":"fixed","ticket_id":"SEC-7"}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if a.events() != eventsBefore+1 || a.stored() != "SEC-7" {
		t.Fatalf("events=%d stored=%q", a.events(), a.stored())
	}
}

func TestFindingsExportIncludesTicketID(t *testing.T) {
	a := newTicketAPI(t)
	const ticket = "SEC-EXPORT-4711"
	if w := a.patchTicket(ticket); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, format := range []string{"json", "csv", "md-single", "md-zip"} {
		w := a.get(fmt.Sprintf("/api/exploration/findings/export?format=%s&scope=selected&ids=%d", format, a.finding))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", format, w.Code, w.Body)
		}
		content := w.Body.String()
		if format == "md-zip" {
			zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			for _, e := range zr.File {
				rc, err := e.Open()
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(rc)
				rc.Close()
				if err != nil {
					t.Fatal(err)
				}
				sb.Write(raw)
			}
			content = sb.String()
		}
		if !strings.Contains(content, ticket) {
			t.Fatalf("%s export lacks the ticket id:\n%s", format, content)
		}
		if format == "json" {
			var out []json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 1 || ticketField(t, out[0]) != ticket {
				t.Fatalf("json export: %v %s", err, w.Body)
			}
		}
	}
	// A finding without a ticket id still exports (empty cell, no label noise required).
	if w := a.patchTicket(""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, format := range []string{"json", "csv", "md-single", "md-zip"} {
		if w := a.get(fmt.Sprintf("/api/exploration/findings/export?format=%s&scope=selected&ids=%d", format, a.finding)); w.Code != 200 {
			t.Fatalf("%s without ticket: %d %s", format, w.Code, w.Body)
		}
	}
}

// The ticket id is operator metadata: nothing an agent or an LLM prompt can read
// may carry it. Each surface below reads findings through a different path.
func TestFindingTicketIDStaysOutOfAgentFacingOutput(t *testing.T) {
	a := newTicketAPI(t)
	pg := a.s.m.pg
	ctx := context.Background()
	const canary = "TKLEAK-5d93a1c7"
	if w := a.patchTicket(canary); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	check := func(name, content string) {
		t.Helper()
		if content == "" {
			t.Fatalf("%s: empty output, the check would be vacuous", name)
		}
		if strings.Contains(content, canary) || strings.Contains(strings.ToLower(content), "ticket_id") || strings.Contains(content, "TicketID") {
			t.Fatalf("%s exposes the ticket id: %s", name, content)
		}
	}
	taskID := strconv.FormatInt(a.taskID, 10)

	// Cross-task orchestration tools (planner / main agent) running the per-task read tools.
	list := workflowCall(t, ctx, a.s.toolListTaskFindings(), map[string]any{"task_id": taskID}, false)
	if !strings.Contains(list, strconv.FormatInt(a.finding, 10)) {
		t.Fatalf("list_task_findings does not list the fixture finding: %s", list)
	}
	check("list_task_findings", list)
	check("get_task_node_detail", workflowCall(t, ctx, a.s.toolGetTaskNodeDetail(), map[string]any{"task_id": taskID, "id": a.nodeID}, false))
	check("get_task_graph", workflowCall(t, ctx, a.s.toolGetTaskGraph(), map[string]any{"task_id": taskID}, false))
	check("get_finding_traffic", workflowCall(t, ctx, a.s.toolGetFindingTraffic(), map[string]any{"finding_id": a.finding}, false))

	// Chat mention: the whole finding row is serialised into the prompt.
	mention, err := loadChatMention(pg, chatMentionRef{Kind: "finding", ID: a.finding})
	if err != nil || mention == nil {
		t.Fatal(mention, err)
	}
	blob, err := json.Marshal(mention)
	if err != nil {
		t.Fatal(err)
	}
	check("chat mention payload", string(blob))
	// Chat mention search results.
	page, err := pg.SearchChatMentionsPage(ctx, "finding", strconv.FormatInt(a.finding, 10), "")
	if err != nil {
		t.Fatal(err)
	}
	blob, err = json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	check("chat mention search", string(blob))

	// Retest: the snapshot the retester agent reads through get_finding_retest_context.
	r, _, _, err := pg.CreateFindingRetest(ctx, a.finding, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM finding_retests WHERE id=$1`, r.ID)
		pg.Exec(`DELETE FROM conversations WHERE id=$1`, *r.ConversationID)
	})
	var detail string
	if err := pg.QueryRow(`SELECT COALESCE(string_agg(detail||summary,' '),'') FROM conversation_activities WHERE conversation_id=$1`, *r.ConversationID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	check("retest initial message", detail+r.InitialMessage())
	// The tool the retester calls, bound to the retest conversation like at runtime.
	var contextTool actool.CoreTool
	for _, tool := range a.s.findingRetestTools() {
		if tool.Name() == "get_finding_retest_context" {
			contextTool = tool
		}
	}
	if contextTool == nil {
		t.Fatal("get_finding_retest_context is not defined")
	}
	out := workflowCall(t, intercept.WithConvID(ctx, *r.ConversationID), contextTool, map[string]any{}, false)
	if !strings.Contains(out, `"vulnclass"`) {
		t.Fatalf("retest context lost the finding: %s", out)
	}
	check("get_finding_retest_context", out)
}

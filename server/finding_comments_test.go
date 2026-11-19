package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// commentAPI is the harness for the finding comment tests: the traffic
// evidence fixture (one task, one finding) plus a raw request helper that
// lets a test choose the Content-Type and the token.
type commentAPI struct {
	t       *testing.T
	s       *Server
	finding int64
	token   string
}

func newCommentAPI(t *testing.T) *commentAPI {
	t.Helper()
	s, f, _ := trafficEvidenceServer(t)
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	return &commentAPI{t: t, s: s, finding: f.FindingID, token: token}
}

func (a *commentAPI) base() string {
	return fmt.Sprintf("/api/exploration/findings/%d/comments", a.finding)
}

// do sends a request. contentType "" sends none; token "" uses the fixture token.
func (a *commentAPI) do(method, path, contentType, body, token string) *httptest.ResponseRecorder {
	a.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token == "" {
		token = a.token
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	a.s.Handler().ServeHTTP(w, r)
	return w
}

func (a *commentAPI) post(path, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("POST", path, "application/json", body, "")
}

func (a *commentAPI) get(path string) *httptest.ResponseRecorder {
	a.t.Helper()
	return a.do("GET", path, "", "", "")
}

func (a *commentAPI) postBody(path, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	b, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		a.t.Fatal(err)
	}
	return a.post(path, string(b))
}

func (a *commentAPI) count() int {
	a.t.Helper()
	var n int
	if err := a.s.m.pg.QueryRow(`SELECT count(*) FROM finding_comments WHERE finding_id=$1`, a.finding).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func errField(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %q", w.Body)
	}
	return e.Error
}

type commentListResp struct {
	Comments []struct {
		ID        int64  `json:"id"`
		FindingID int64  `json:"finding_id"`
		Body      string `json:"body"`
		Author    struct {
			Kind    string `json:"kind"`
			Subject string `json:"subject"`
		} `json:"author"`
		CreatedAt string `json:"created_at"`
	} `json:"comments"`
	NextCursor   string `json:"next_cursor"`
	MaxBodyRunes int    `json:"max_body_runes"`
}

func (a *commentAPI) list(path string) commentListResp {
	a.t.Helper()
	w := a.get(path)
	if w.Code != 200 {
		a.t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	var out commentListResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		a.t.Fatal(err)
	}
	return out
}

// T-API-1
func TestFindingCommentsCreateAndList(t *testing.T) {
	a := newCommentAPI(t)
	empty := a.get(a.base())
	if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"comments":[]`) {
		t.Fatalf("empty list: %d %s", empty.Code, empty.Body)
	}
	w := a.post(a.base(), `{"body":"first note"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created struct {
		Comment struct {
			ID        int64  `json:"id"`
			FindingID int64  `json:"finding_id"`
			Body      string `json:"body"`
			Author    struct {
				Kind    string `json:"kind"`
				Subject string `json:"subject"`
			} `json:"author"`
			CreatedAt string `json:"created_at"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	c := created.Comment
	if c.ID <= 0 || c.FindingID != a.finding || c.Body != "first note" || c.Author.Kind != "operator" || c.Author.Subject != "ARTEX" {
		t.Fatalf("unexpected comment: %+v", c)
	}
	ts, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
	if err != nil || !strings.HasSuffix(c.CreatedAt, "Z") || ts.Location() != time.UTC {
		t.Fatalf("created_at %q is not RFC 3339 UTC: %v", c.CreatedAt, err)
	}
	// CRLF is normalised and text is trimmed.
	if w = a.postBody(a.base(), "  line1\r\nline2\t \n"); w.Code != 201 || !strings.Contains(w.Body.String(), `"body":"line1\nline2"`) {
		t.Fatalf("normalised create: %d %s", w.Code, w.Body)
	}
	got := a.list(a.base())
	if len(got.Comments) != 2 || got.Comments[0].ID != c.ID || got.Comments[0].Body != "first note" || got.NextCursor != "" || got.MaxBodyRunes != db.MaxFindingCommentRunes {
		t.Fatalf("list: %+v", got)
	}
	if got.Comments[1].ID <= got.Comments[0].ID {
		t.Fatal("list is not in id order")
	}
	// The finding itself is untouched by comments.
	if f, err := a.s.m.pg.GetFinding(a.finding); err != nil || f == nil {
		t.Fatal(f, err)
	}
}

// T-API-2: body rules, media type, size.
func TestFindingCommentsValidation(t *testing.T) {
	a := newCommentAPI(t)
	cases := []struct {
		name string
		body string
		code int
		msg  string
	}{
		{"empty", "", 400, "body must not be empty"},
		{"whitespace", " \n\t\r\n ", 400, "body must not be empty"},
		{"too long", strings.Repeat("a", db.MaxFindingCommentRunes+1), 400, "body is too long"},
		{"too long in code points not bytes", strings.Repeat("é", db.MaxFindingCommentRunes+1), 400, "body is too long"},
		{"nul", "a\x00b", 400, "body contains control characters"},
		{"escape", "a\x1bb", 400, "body contains control characters"},
		{"del", "a\x7fb", 400, "body contains control characters"},
		{"bidi override", "a\u202eb", 400, "body contains bidirectional control characters"},
		{"bidi isolate", "a\u2066b", 400, "body contains bidirectional control characters"},
		{"zero width space", "a\u200bb", 400, "body contains zero-width characters"},
		{"word joiner", "a\u2060b", 400, "body contains zero-width characters"},
		{"bom", "a\ufeffb", 400, "body contains zero-width characters"},
		{"arabic letter mark", "a\u061cb", 400, "body contains bidirectional control characters"},
		{"left-to-right mark", "a\u200eb", 400, "body contains bidirectional control characters"},
		{"right-to-left mark", "a\u200fb", 400, "body contains bidirectional control characters"},
		{"c1 control", "a\u0085b", 400, "body contains control characters"},
		{"c1 control last", "a\u009fb", 400, "body contains control characters"},
		{"tag character", "a\U000e0041b", 400, "body contains zero-width characters"},
		{"invisible operator", "a\u2062b", 400, "body contains zero-width characters"},
		{"interlinear annotation", "a\ufffab", 400, "body contains zero-width characters"},
		{"vs16 emoji ok", "love \u2764\ufe0f", 201, ""},
		{"soft hyphen ok", "co\u00adoperate", 201, ""},
		{"max length ok", strings.Repeat("é", db.MaxFindingCommentRunes), 201, ""},
		{"zwj emoji ok", "family \U0001F468\u200d\U0001F469\u200d\U0001F467 ok", 201, ""},
		{"zwnj ok", "می\u200cخواهم", 201, ""},
		{"tab and newline ok", "a\tb\nc", 201, ""},
	}
	for _, tc := range cases {
		before := a.count()
		w := a.postBody(a.base(), tc.body)
		if w.Code != tc.code {
			t.Fatalf("%s: status %d want %d: %.200s", tc.name, w.Code, tc.code, w.Body)
		}
		want := before
		if tc.code == 201 {
			want++
		} else if got := errField(t, w); got != tc.msg {
			t.Fatalf("%s: message %q want %q", tc.name, got, tc.msg)
		}
		if a.count() != want {
			t.Fatalf("%s: row count changed unexpectedly", tc.name)
		}
	}
	// Invalid UTF-8 on the wire is rejected, not silently replaced.
	before := a.count()
	if w := a.post(a.base(), "{\"body\":\"a\xffb\"}"); w.Code != 400 || errField(t, w) != "body is not valid UTF-8" || a.count() != before {
		t.Fatalf("invalid utf-8: %d %s", w.Code, w.Body)
	}

	// Content-Type must be exactly application/json (parameters allowed).
	for _, ct := range []string{"", "text/plain", "text/plain; x=application/json", "application/jsonx", "application/json-patch+json", "multipart/form-data; boundary=application/json", "application/json;;;", "x application/json"} {
		before := a.count()
		w := a.do("POST", a.base(), ct, `{"body":"via bad type"}`, "")
		if w.Code != 415 || a.count() != before {
			t.Fatalf("content-type %q: status %d rows %d->%d", ct, w.Code, before, a.count())
		}
	}
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
		if w := a.do("POST", a.base(), ct, `{"body":"good type"}`, ""); w.Code != 201 {
			t.Fatalf("content-type %q: %d %s", ct, w.Code, w.Body)
		}
	}
	// 415 is decided before the body is read: a body that would be 413 still gets 415.
	huge := `{"body":"` + strings.Repeat("a", 129<<10) + `"}`
	if w := a.do("POST", a.base(), "text/plain", huge, ""); w.Code != 415 {
		t.Fatalf("415 before body read: %d", w.Code)
	}

	// Size cap.
	before = a.count()
	if w := a.post(a.base(), huge); w.Code != 413 || errField(t, w) != "request body too large" || a.count() != before {
		t.Fatalf("oversize: %d", w.Code)
	}
	// JSON errors have fixed messages.
	for name, body := range map[string]string{
		"malformed":     `{"body":`,
		"not an object": `[]`,
		"wrong type":    `{"body":5}`,
		"trailing":      `{"body":"a"} {"body":"b"}`,
		"empty request": ``,
	} {
		w := a.post(a.base(), body)
		if w.Code != 400 || errField(t, w) != "invalid JSON" {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if a.count() != before {
		t.Fatal("rejected requests wrote rows")
	}
}

// T-API-3
func TestFindingCommentsMissingFinding(t *testing.T) {
	a := newCommentAPI(t)
	for _, method := range []string{"GET", "POST"} {
		path := "/api/exploration/findings/999999999/comments"
		if w := a.do(method, path, "application/json", `{"body":"x"}`, ""); w.Code != 404 {
			t.Fatalf("%s missing finding: %d %s", method, w.Code, w.Body)
		}
		for _, id := range []string{"abc", "0", "-3"} {
			if w := a.do(method, "/api/exploration/findings/"+id+"/comments", "application/json", `{"body":"x"}`, ""); w.Code != 400 {
				t.Fatalf("%s id %q: %d %s", method, id, w.Code, w.Body)
			}
		}
	}
	if w := a.get(a.base() + "?cursor=not-a-cursor"); w.Code != 400 || errField(t, w) != "invalid cursor" {
		t.Fatalf("bad cursor: %d %s", w.Code, w.Body)
	}
}

// T-API-4
func TestFindingCommentsRequireAuth(t *testing.T) {
	a := newCommentAPI(t)
	other := subjectTestToken(t, []byte("another-key-another-key-another-key"), "ARTEX", time.Hour)
	empty := subjectTestToken(t, a.s.jwtKey, "", time.Hour)
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, a.base(), strings.NewReader(`{"body":"x"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s without token: %d", method, w.Code)
		}
		if w = a.do(method, a.base(), "application/json", `{"body":"x"}`, other); w.Code != 401 {
			t.Fatalf("%s bad token: %d", method, w.Code)
		}
	}
	// A valid token without a subject has no identity: no comment may be written.
	if w := a.do("POST", a.base(), "application/json", `{"body":"x"}`, empty); w.Code != 401 {
		t.Fatalf("empty subject: %d %s", w.Code, w.Body)
	}
	if a.count() != 0 {
		t.Fatal("unauthenticated request wrote a row")
	}
}

// T-API-5
func TestFindingCommentsRejectClientAuthor(t *testing.T) {
	a := newCommentAPI(t)
	for _, field := range []string{`"author":{"kind":"operator","subject":"bob"}`, `"author_subject":"bob"`, `"author_kind":"agent"`, `"created_at":"2001-01-01T00:00:00Z"`, `"id":1`} {
		w := a.post(a.base(), `{"body":"x",`+field+`}`)
		if w.Code != 400 || errField(t, w) != "unknown field" {
			t.Fatalf("%s: %d %s", field, w.Code, w.Body)
		}
	}
	if a.count() != 0 {
		t.Fatal("rejected request stored a row")
	}
	tok := subjectTestToken(t, a.s.jwtKey, "carol", time.Hour)
	w := a.do("POST", a.base(), "application/json", `{"body":"signed by token"}`, tok)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	got := a.list(a.base())
	if len(got.Comments) != 1 || got.Comments[0].Author.Kind != "operator" || got.Comments[0].Author.Subject != "carol" {
		t.Fatalf("author must come from the token: %+v", got.Comments)
	}
}

// T-API-6
func TestFindingCommentsTaskContext(t *testing.T) {
	a := newCommentAPI(t)
	pg := a.s.m.pg
	finding, err := pg.GetFinding(a.finding)
	if err != nil || finding == nil || finding.TaskID == nil {
		t.Fatal(finding, err)
	}
	tid := *finding.TaskID
	if w := a.post(a.base(), `{"body":"seed"}`); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	// Own task context: reads and writes work.
	own := a.base() + "?context_task=" + strconv.FormatInt(tid, 10)
	if w := a.post(own, `{"body":"in own task"}`); w.Code != 201 {
		t.Fatalf("own context write: %d %s", w.Code, w.Body)
	}
	if w := a.get(a.base() + "?context_task=999999999"); w.Code != 404 {
		t.Fatalf("unknown context task: %d", w.Code)
	}
	unrelated, err := a.s.m.CreateTask("unrelated comment ctx", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.ParseInt(unrelated.ID, 10, 64)
	defer pg.DeleteTask(uid)
	for _, method := range []string{"GET", "POST"} {
		if w := a.do(method, a.base()+"?context_task="+unrelated.ID, "application/json", `{"body":"x"}`, ""); w.Code != 404 {
			t.Fatalf("%s finding not visible in task: %d %s", method, w.Code, w.Body)
		}
	}
	child, err := a.s.m.CreateTaskWithOptions("inherited comment ctx", "fixture", db.TaskCreateOptions{SourceTaskIDs: []int64{tid}})
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := strconv.ParseInt(child.ID, 10, 64)
	inherited := a.base() + "?context_task=" + child.ID
	before := a.count()
	if w := a.post(inherited, `{"body":"x"}`); w.Code != 403 || a.count() != before {
		t.Fatalf("inherited write: %d %s", w.Code, w.Body)
	}
	if got := a.list(inherited); len(got.Comments) != before {
		t.Fatalf("inherited read: %d comments, want %d", len(got.Comments), before)
	}
	// Archiving: reads still work, writes get 409. A task inherited by a live
	// task cannot be archived, so the child goes first.
	if err = pg.DeleteTask(cid); err != nil {
		t.Fatal(err)
	}
	if err = a.s.m.SetTaskPaused(strconv.FormatInt(tid, 10), true); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.QueueTaskArchive(tid); err != nil {
		t.Fatal(err)
	}
	if w := a.post(a.base(), `{"body":"during archive"}`); w.Code != 409 || a.count() != before {
		t.Fatalf("archiving: %d %s", w.Code, w.Body)
	}
	if got := a.list(a.base()); len(got.Comments) != before {
		t.Fatal("read blocked while archiving")
	}
}

// T-API-7
func TestFindingCommentsPaginationHTTP(t *testing.T) {
	a := newCommentAPI(t)
	for i := 0; i < 7; i++ {
		if w := a.postBody(a.base(), fmt.Sprintf("note %d", i)); w.Code != 201 {
			t.Fatal(w.Code, w.Body)
		}
	}
	// Equal created_at must not disturb the order.
	if _, err := a.s.m.pg.Exec(`UPDATE finding_comments SET created_at='2026-01-01T00:00:00Z' WHERE finding_id=$1`, a.finding); err != nil {
		t.Fatal(err)
	}
	var seen []int64
	cursor := ""
	pages := 0
	for {
		path := a.base() + "?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := a.list(path)
		pages++
		for _, c := range page.Comments {
			seen = append(seen, c.ID)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Comments) != 3 {
			t.Fatalf("short page with a cursor: %d", len(page.Comments))
		}
		cursor = page.NextCursor
	}
	if pages != 3 || len(seen) != 7 || !sort.SliceIsSorted(seen, func(i, j int) bool { return seen[i] < seen[j] }) {
		t.Fatalf("walk: pages=%d ids=%v", pages, seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[i-1] {
			t.Fatal("repeated id", seen)
		}
	}
	// limit=200 is the cap, an oversize or junk limit falls back to a valid one.
	if got := a.list(a.base() + "?limit=100000"); len(got.Comments) != 7 {
		t.Fatalf("clamped limit: %d", len(got.Comments))
	}
	if got := a.list(a.base() + "?limit=abc"); len(got.Comments) != 7 {
		t.Fatalf("junk limit: %d", len(got.Comments))
	}
	if w := a.get(a.base() + "?cursor=" + "AAAA"); w.Code != 400 {
		t.Fatalf("bad cursor: %d", w.Code)
	}
}

// T-API-8
func TestFindingCommentsLogAndErrorHygiene(t *testing.T) {
	a := newCommentAPI(t)
	logs := dbtest.CaptureLog(t)
	const canary = "FCCANARY7d1e4b"
	w := a.postBody(a.base(), "hello "+canary)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var created struct {
		Comment struct {
			ID int64 `json:"id"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("finding=%d comment=%d", a.finding, created.Comment.ID)
	if !strings.Contains(logs(), line) || !strings.Contains(logs(), "author=operator/ARTEX") {
		t.Fatalf("missing content-free create line %q in %q", line, logs())
	}
	// Decoder errors never echo the input.
	for name, body := range map[string]string{
		"truncated":     `{"body":"` + canary,
		"unknown field": `{"` + canary + `":1}`,
		"wrong type":    `{"body":{"` + canary + `":1}}`,
		"trailing":      `{"body":"x"} ` + canary,
	} {
		w := a.post(a.base(), body)
		msg := errField(t, w)
		if w.Code != 400 || strings.Contains(w.Body.String(), canary) || (msg != "invalid JSON" && msg != "unknown field") {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	// A body rule error does not echo the body.
	if w := a.postBody(a.base(), canary+"\u202e"); w.Code != 400 || strings.Contains(w.Body.String(), canary) {
		t.Fatalf("rule error: %d %s", w.Code, w.Body)
	}
	// Forced CHECK violation: the validation passes but the database refuses.
	const constraint = "fc_test_block"
	if _, err := a.s.m.pg.Exec(`ALTER TABLE finding_comments ADD CONSTRAINT ` + constraint + ` CHECK (body NOT LIKE '%FCFORCED%') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.s.m.pg.Exec(`ALTER TABLE finding_comments DROP CONSTRAINT IF EXISTS ` + constraint) })
	before := a.count()
	w = a.postBody(a.base(), "secret FCFORCED "+canary)
	if w.Code != 400 || errField(t, w) != "comment rejected" || strings.Contains(w.Body.String(), "FCFORCED") || a.count() != before {
		t.Fatalf("forced check: %d %s", w.Code, w.Body)
	}
	out := logs()
	if !strings.Contains(out, "sqlstate=23514") || !strings.Contains(out, constraint) {
		t.Fatalf("log lacks sqlstate/constraint: %q", out)
	}
	if strings.Contains(out, canary) || strings.Contains(out, "FCFORCED") || strings.Contains(out, "Failing row") {
		t.Fatalf("log leaks the body or driver detail: %q", out)
	}
}

// T-API-9
func TestFindingCommentsStayOutOfPromptsAndExports(t *testing.T) {
	a := newCommentAPI(t)
	pg := a.s.m.pg
	const canary = "FCLEAK-5b9c0e2a41"
	if w := a.postBody(a.base(), "analyst only "+canary); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	check := func(name, content string) {
		t.Helper()
		if strings.Contains(content, canary) {
			t.Fatalf("%s contains the comment text", name)
		}
	}
	// Finding detail and list.
	w := a.get(fmt.Sprintf("/api/exploration/findings/%d", a.finding))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id"`) {
		t.Fatal("detail", w.Code, w.Body)
	}
	check("finding detail", w.Body.String())
	w = a.get("/api/exploration/findings")
	if w.Code != 200 {
		t.Fatal("list", w.Code, w.Body)
	}
	check("finding list", w.Body.String())
	// Exports, each format.
	for _, format := range []string{"json", "csv", "md-single", "md-zip"} {
		w := a.get(fmt.Sprintf("/api/exploration/findings/export?format=%s&scope=selected&ids=%d", format, a.finding))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("export %s: %d", format, w.Code)
		}
		if format == "md-zip" {
			zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
			if err != nil {
				t.Fatal(err)
			}
			if len(zr.File) == 0 {
				t.Fatal("empty zip")
			}
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
				check("export zip entry "+e.Name, e.Name+string(raw))
			}
			continue
		}
		check("export "+format, w.Body.String())
	}
	// Chat mention payload.
	mention, err := loadChatMention(pg, chatMentionRef{Kind: "finding", ID: a.finding})
	if err != nil || mention == nil {
		t.Fatal(mention, err)
	}
	blob, err := json.Marshal(mention)
	if err != nil {
		t.Fatal(err)
	}
	check("chat mention payload", string(blob))
	// Retest snapshot and initial message.
	r, _, _, err := pg.CreateFindingRetest(t.Context(), a.finding, "")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot, detail string
	if err := pg.QueryRow(`SELECT snapshot::text FROM finding_retests WHERE id=$1`, r.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	check("retest snapshot", snapshot)
	if err := pg.QueryRow(`SELECT COALESCE(string_agg(detail||summary,' '),'') FROM conversation_activities WHERE conversation_id=$1`, *r.ConversationID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	check("retest initial message", detail+r.InitialMessage())
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM finding_retests WHERE id=$1`, r.ID)
		pg.Exec(`DELETE FROM conversations WHERE id=$1`, *r.ConversationID)
	})
	// Neither finding type has a comment field.
	for _, typ := range []reflect.Type{reflect.TypeOf(db.DBFinding{}), reflect.TypeOf(FindingDTO{})} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(strings.ToLower(typ.Field(i).Name), "comment") {
				t.Fatalf("%s has a comment field %s", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

// T-API-10
func TestFindingCommentsCapHTTP(t *testing.T) {
	a := newCommentAPI(t)
	if _, err := a.s.m.pg.Exec(`INSERT INTO finding_comments(finding_id, body, author_subject)
SELECT $1, 'filler '||g, 'ARTEX' FROM generate_series(1,$2) g`, a.finding, db.MaxFindingCommentsPerFinding); err != nil {
		t.Fatal(err)
	}
	w := a.postBody(a.base(), "one too many")
	if w.Code != 409 || errField(t, w) != "comment limit reached for this finding" || a.count() != db.MaxFindingCommentsPerFinding {
		t.Fatalf("cap: %d %s count=%d", w.Code, w.Body, a.count())
	}
}

// The default page size (50) and the maximum (200) of the list endpoint are
// pinned over HTTP with more rows than the maximum. Zero, negative and junk
// limits fall back to the default (findingPaginationParam).
func TestFindingCommentsPageSizeBoundsHTTP(t *testing.T) {
	const total = 205
	if db.MaxFindingCommentsPerFinding < total {
		t.Fatalf("fixture needs %d rows, cap is %d", total, db.MaxFindingCommentsPerFinding)
	}
	a := newCommentAPI(t)
	if _, err := a.s.m.pg.Exec(`INSERT INTO finding_comments(finding_id, body, author_subject)
SELECT $1, 'bulk '||g, 'ARTEX' FROM generate_series(1,$2) g`, a.finding, total); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		query string
		want  int
	}{
		{"", 50},
		{"?limit=100000", 200},
		{"?limit=200", 200},
		{"?limit=0", 50},
		{"?limit=-5", 50},
		{"?limit=abc", 50},
	} {
		got := a.list(a.base() + c.query)
		if len(got.Comments) != c.want || got.NextCursor == "" {
			t.Fatalf("query %q: %d comments, cursor %q, want %d with a cursor", c.query, len(got.Comments), got.NextCursor, c.want)
		}
	}
	var seen []int64
	cursor := ""
	pages := 0
	for {
		path := a.base()
		if cursor != "" {
			path += "?cursor=" + cursor
		}
		page := a.list(path)
		pages++
		if pages > 10 {
			t.Fatal("cursor walk does not terminate")
		}
		for _, c := range page.Comments {
			seen = append(seen, c.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 5 || len(seen) != total || !sort.SliceIsSorted(seen, func(i, j int) bool { return seen[i] < seen[j] }) {
		t.Fatalf("walk: pages=%d n=%d", pages, len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[i-1] {
			t.Fatal("repeated id", seen[i])
		}
	}
}

// T-SRC-1: finding comment identifiers stay in the files listed here. A later
// change that joins comments into another path (a prompt, an export, a DTO)
// fails this test on purpose.
func TestFindingCommentIdentifiersConfined(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?i)finding_?comment`)
	// Exact number of matching lines per file; -1 means any number. No prefix
	// rules: a new file under db/ or server/ must be listed on purpose.
	allowed := map[string]int{
		"server/server.go":               1, // the registerFindingComments call in Handler()
		"db/task_archives.go":            1, // the snapshot query of the archive
		"db/task_archives_restore.go":    1, // the restoreFindingCommentsTx call
		"db/schema.sql":                  -1,
		"db/finding_comments.go":         -1,
		"db/finding_comments_archive.go": -1,
		"server/finding_comments.go":     -1,
	}
	counts := map[string]int{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || rel == "web" || rel == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if rel != "db/schema.sql" && (!strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go")) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if re.MatchString(line) {
				counts[rel]++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const howTo = "If this reference is intended, review that the comment text cannot reach an agent prompt, retest snapshot, chat mention or export, then update the allowlist in TestFindingCommentIdentifiersConfined (server/finding_comments_test.go)."
	for rel, n := range counts {
		if strings.HasPrefix(rel, "agent/") || strings.HasPrefix(rel, "third_party/") {
			t.Errorf("%s mentions finding comments (%d lines); agent/ and third_party/ must never. %s", rel, n, howTo)
			continue
		}
		want, ok := allowed[rel]
		switch {
		case !ok:
			t.Errorf("%s mentions finding comments (%d lines) but is not on the allowlist. %s", rel, n, howTo)
		case want >= 0 && n != want:
			t.Errorf("%s has %d lines mentioning finding comments, the allowlist says exactly %d. %s", rel, n, want, howTo)
		}
	}
	for rel, want := range allowed {
		if want > 0 && counts[rel] == 0 {
			t.Errorf("%s no longer mentions finding comments but the allowlist expects %d lines; update the allowlist in TestFindingCommentIdentifiersConfined", rel, want)
		}
	}
}

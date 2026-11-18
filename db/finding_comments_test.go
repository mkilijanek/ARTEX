package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// privateCommentDB opens a private sibling database (<package db>_<suffix>) that
// is dropped before use and after the test. The pool is closed before
// DROP DATABASE ... WITH (FORCE). The suffix must be short and fixed so the name
// stays under PostgreSQL's 63-byte identifier limit. It never skips when
// ARTEX_PG_DSN is set; an unreachable database fails the test.
func privateCommentDB(t *testing.T, suffix string) (*DB, string) {
	t.Helper()
	base := testDSN(t)
	u, err := url.Parse(base)
	if err != nil || u.Path == "" || u.Path == "/" {
		t.Fatalf("unusable DSN for private database: %v", err)
	}
	name := strings.TrimPrefix(u.Path, "/") + "_" + suffix
	if len(name) > 63 {
		t.Fatalf("database name %q exceeds 63 bytes", name)
	}
	u.Path = "/" + name
	dsn := u.String()
	drop := func() {
		admin := *u
		admin.Path = "/postgres"
		conn, err := sql.Open("pgx", admin.String())
		if err != nil {
			t.Errorf("open admin connection: %v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	}
	drop()
	d, err := Open(dsn)
	if err != nil {
		t.Fatalf("open private database %s: %v", name, err)
	}
	var closed atomic.Bool
	t.Cleanup(func() {
		if closed.CompareAndSwap(false, true) {
			d.Close()
		}
		drop()
	})
	return d, dsn
}

func mustFinding(t *testing.T, d *DB, taskID int64, label string) int64 {
	t.Helper()
	id, err := d.AddFinding(taskID, 0, "xss", label, "high", "summary "+label, "evidence "+label, "tester", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustComment(t *testing.T, d *DB, fid int64, body string) *FindingComment {
	t.Helper()
	c, err := d.AddFindingComment(context.Background(), fid, "operator", "ARTEX", body)
	if err != nil {
		t.Fatalf("AddFindingComment: %v", err)
	}
	return c
}

func countComments(t *testing.T, d *DB, fid int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM finding_comments WHERE finding_id=$1`, fid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func findingJSON(t *testing.T, d *DB, fid int64) string {
	t.Helper()
	var s string
	if err := d.QueryRow(`SELECT to_jsonb(f)::text FROM findings f WHERE id=$1`, fid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// T-DB-1 (AC12)
func TestFindingCommentsSchemaUpgradeIdempotent(t *testing.T) {
	d, dsn := privateCommentDB(t, "fc1")
	f1 := mustFinding(t, d, 0, "one")
	f2 := mustFinding(t, d, 0, "two")
	before := findingJSON(t, d, f1) + findingJSON(t, d, f2)
	if _, err := d.Exec(`DROP TABLE finding_comments`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	check := func(d *DB) {
		t.Helper()
		var idxDef string
		if err := d.QueryRow(`SELECT indexdef FROM pg_indexes WHERE tablename='finding_comments' AND indexname='idx_finding_comments_page'`).Scan(&idxDef); err != nil {
			t.Fatalf("index missing: %v", err)
		}
		if !strings.Contains(idxDef, "(finding_id, id)") {
			t.Fatalf("index def %q", idxDef)
		}
		var del string
		if err := d.QueryRow(`SELECT confdeltype::text FROM pg_constraint WHERE conrelid='finding_comments'::regclass AND contype='f' AND confrelid='findings'::regclass`).Scan(&del); err != nil || del != "c" {
			t.Fatalf("fk confdeltype=%q err=%v", del, err)
		}
		if got := findingJSON(t, d, f1) + findingJSON(t, d, f2); got != before {
			t.Fatal("findings rows changed by schema apply")
		}
	}
	for i := range 3 {
		r, err := Open(dsn)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		check(r)
		r.Close()
	}

	// Two simultaneous Open calls on the same database.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	dbs := make([]*DB, 2)
	for i := range 2 {
		wg.Go(func() { dbs[i], errs[i] = Open(dsn) })
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("concurrent Open %d: %v", i, errs[i])
		}
		defer dbs[i].Close()
	}
	check(dbs[0])
}

// T-DB-2 (AC3)
func TestFindingCommentsPersistAcrossReopens(t *testing.T) {
	d, dsn := privateCommentDB(t, "fc2")
	fid := mustFinding(t, d, 0, "persist")
	want := []*FindingComment{
		mustComment(t, d, fid, "first"),
		mustComment(t, d, fid, "second\nline"),
		mustComment(t, d, fid, "third"),
	}
	d.Close()
	for i := range 2 {
		r, err := Open(dsn)
		if err != nil {
			t.Fatal(err)
		}
		got, next, err := r.ListFindingComments(context.Background(), fid, "", 50)
		if err != nil || next != "" {
			t.Fatalf("reopen %d: next=%q err=%v", i, next, err)
		}
		if len(got) != len(want) {
			t.Fatalf("reopen %d: got %d comments", i, len(got))
		}
		for j := range want {
			if !reflect.DeepEqual(got[j], want[j]) {
				t.Fatalf("reopen %d comment %d: got %+v want %+v", i, j, got[j], want[j])
			}
		}
		r.Close()
	}
}

// T-DB-3 (AC6, AC8)
func TestFindingCommentsKeysetPagination(t *testing.T) {
	d, _ := privateCommentDB(t, "fc3")
	ctx := context.Background()
	fid := mustFinding(t, d, 0, "page")
	other := mustFinding(t, d, 0, "other")
	var ids []int64
	for i := range 7 {
		ids = append(ids, mustComment(t, d, fid, fmt.Sprintf("c%d", i)).ID)
		mustComment(t, d, other, fmt.Sprintf("o%d", i))
	}
	walk := func() []int64 {
		t.Helper()
		var seen []int64
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("walk does not terminate")
			}
			got, next, err := d.ListFindingComments(ctx, fid, cursor, 3)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got {
				if c.FindingID != fid {
					t.Fatalf("comment of finding %d leaked into %d", c.FindingID, fid)
				}
				seen = append(seen, c.ID)
			}
			if next == "" {
				if len(got) == 0 && pages == 0 {
					t.Fatal("empty first page")
				}
				return seen
			}
			if len(got) != 3 {
				t.Fatalf("page %d has %d rows with a next cursor", pages, len(got))
			}
			cursor = next
		}
	}
	if got := walk(); !reflect.DeepEqual(got, ids) {
		t.Fatalf("walk=%v want %v", got, ids)
	}
	if _, err := d.Exec(`UPDATE finding_comments SET created_at='2026-01-01T00:00:00Z' WHERE finding_id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	if got := walk(); !reflect.DeepEqual(got, ids) {
		t.Fatalf("equal created_at: walk=%v want %v", got, ids)
	}
	// exact multiple of limit: last page must have an empty cursor
	got, next, err := d.ListFindingComments(ctx, fid, "", 7)
	if err != nil || len(got) != 7 || next != "" {
		t.Fatalf("limit==count: len=%d next=%q err=%v", len(got), next, err)
	}
	// UTC
	if got[0].CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at location %v", got[0].CreatedAt.Location())
	}
	// empty list is a non-nil slice
	empty, next, err := d.ListFindingComments(ctx, mustFinding(t, d, 0, "empty"), "", 3)
	if err != nil || empty == nil || len(empty) != 0 || next != "" {
		t.Fatalf("empty: %v %q %v", empty, next, err)
	}
	for _, bad := range []string{"%%%", "bm90LWpzb24", "e30", "eyJpZCI6MH0", "eyJpZCI6LTF9", "eyJpZCI6MSwieCI6Mn0", strings.Repeat("A", 4000)} {
		if _, _, err := d.ListFindingComments(ctx, fid, bad, 3); !errors.Is(err, ErrFindingCommentCursor) {
			t.Fatalf("cursor %q: err=%v", bad, err)
		}
	}
}

// T-DB-4 (AC4)
func TestFindingCommentDoesNotTouchFinding(t *testing.T) {
	d, _ := privateCommentDB(t, "fc4")
	fid := mustFinding(t, d, 0, "untouched")
	counts := func() string {
		var b, n, a int
		if err := d.QueryRow(`SELECT (SELECT count(*) FROM finding_traffic_bindings), (SELECT count(*) FROM notification_events), (SELECT count(*) FROM activity)`).Scan(&b, &n, &a); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(b, n, a)
	}
	beforeF, beforeC := findingJSON(t, d, fid), counts()
	mustComment(t, d, fid, "a note")
	mustComment(t, d, fid, "another note")
	if got := findingJSON(t, d, fid); got != beforeF {
		t.Fatalf("finding row changed:\n%s\n%s", beforeF, got)
	}
	if got := counts(); got != beforeC {
		t.Fatalf("side-effect rows changed: %s -> %s", beforeC, got)
	}
}

// T-DB-5 (AC7, AC16 store half)
func TestFindingCommentValidation(t *testing.T) {
	ok := []struct{ name, in, want string }{
		{"plain", "hello", "hello"},
		{"crlf", "a\r\nb\r\n", "a\nb"},
		{"tab and newline kept", "a\tb\nc", "a\tb\nc"},
		{"trimmed", "  \n x \t ", "x"},
		{"zwj emoji", "family \U0001F468\u200D\U0001F469\u200D\U0001F467", "family \U0001F468\u200D\U0001F469\u200D\U0001F467"},
		{"zwnj persian", "می\u200Cخواهم", "می\u200Cخواهم"},
		{"10000 runes", strings.Repeat("é", 10000), strings.Repeat("é", 10000)},
		{"10000 emoji runes", strings.Repeat("\U0001F600", 10000), strings.Repeat("\U0001F600", 10000)},
		{"vs16 emoji", "love \u2764\uFE0F ok", "love \u2764\uFE0F ok"},
		{"vs15 and soft hyphen", "co\u00ADoperate \u2764\uFE0E", "co\u00ADoperate \u2764\uFE0E"},
		{"line and paragraph separators", "a\u2028b\u2029c", "a\u2028b\u2029c"},
		{"NEL trimmed at the edges", "\u0085 x \u0085", "x"},
	}
	for _, c := range ok {
		got, err := NormalizeFindingCommentBody(c.in)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q err=%v", c.name, got, err)
		}
	}
	bad := []struct{ name, in string }{
		{"empty", ""},
		{"whitespace only", " \n\t\r\n "},
		{"10001 runes", strings.Repeat("a", 10001)},
		{"invalid utf8", "ab\xffcd"},
		{"NUL", "a\x00b"},
		{"BEL", "a\x07b"},
		{"ESC", "a\x1bb"},
		{"lone CR", "a\rb"},
		{"DEL", "a\x7fb"},
		{"RLO", "a\u202Eb"},
		{"LRE", "a\u202Ab"},
		{"LRI", "a\u2066b"},
		{"PDI", "a\u2069b"},
		{"ZWSP", "a\u200Bb"},
		{"WJ", "a\u2060b"},
		{"BOM", "a\uFEFFb"},
		{"ALM", "a\u061Cb"},
		{"LRM", "a\u200Eb"},
		{"RLM", "a\u200Fb"},
		{"C1 first", "a\u0080b"},
		{"NEL inside", "a\u0085b"},
		{"C1 last", "a\u009Fb"},
		{"tag space", "a\U000E0020b"},
		{"tag cancel", "a\U000E007Fb"},
		{"function application", "a\u2061b"},
		{"invisible plus", "a\u2064b"},
		{"interlinear anchor", "a\uFFF9b"},
		{"interlinear terminator", "a\uFFFBb"},
	}
	for _, c := range bad {
		got, err := NormalizeFindingCommentBody(c.in)
		if !errors.Is(err, ErrFindingCommentInvalid) || got != "" {
			t.Errorf("%s: got %q err=%v", c.name, got, err)
			continue
		}
		if len(c.in) > 3 && strings.Contains(err.Error(), c.in) {
			t.Errorf("%s: error carries the body: %v", c.name, err)
		}
	}

	d, _ := privateCommentDB(t, "fc5")
	ctx := context.Background()
	fid := mustFinding(t, d, 0, "validate")
	if c, err := d.AddFindingComment(ctx, fid, "operator", "ARTEX", "x\r\ny"); err != nil || c.Body != "x\ny" {
		t.Fatalf("stored body %v err=%v", c, err)
	}
	for _, in := range []string{"", "a\u202Eb", strings.Repeat("a", 10001)} {
		if _, err := d.AddFindingComment(ctx, fid, "operator", "ARTEX", in); !errors.Is(err, ErrFindingCommentInvalid) {
			t.Fatalf("add %.10q: %v", in, err)
		}
	}
	for _, a := range [][2]string{{"", "ARTEX"}, {"operator", ""}, {"operator", strings.Repeat("s", 201)}} {
		if _, err := d.AddFindingComment(ctx, fid, a[0], a[1], "body"); !errors.Is(err, ErrFindingCommentInvalid) {
			t.Fatalf("author %q/%.10q: %v", a[0], a[1], err)
		}
	}
	if n := countComments(t, d, fid); n != 1 {
		t.Fatalf("rejections wrote rows: count=%d", n)
	}
	// CHECK constraints behind the validation
	for name, args := range map[string][]any{
		"too long":     {fid, strings.Repeat("a", 10001), "ARTEX"},
		"blank body":   {fid, "   ", "ARTEX"},
		"newline body": {fid, " \n\t\r\n ", "ARTEX"},
		"empty author": {fid, "x", ""},
		"long author":  {fid, "x", strings.Repeat("s", 201)},
	} {
		if _, err := d.Exec(`INSERT INTO finding_comments(finding_id, body, author_subject) VALUES ($1,$2,$3)`, args...); err == nil {
			t.Errorf("CHECK %s not enforced", name)
		}
	}

	// A constraint violation despite validation must not leak the body.
	const canary = "CANARY-7f3a91-body"
	if _, err := d.Exec(`ALTER TABLE finding_comments ADD CONSTRAINT fc_test_no_canary CHECK (body NOT LIKE '%CANARY-7f3a91%')`); err != nil {
		t.Fatal(err)
	}
	_, err := d.AddFindingComment(ctx, fid, "operator", "ARTEX", "secret "+canary)
	if !errors.Is(err, ErrFindingCommentInvalid) {
		t.Fatalf("forced CHECK violation: %v", err)
	}
	if strings.Contains(err.Error(), "CANARY") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error carries the body: %v", err)
	}
	if !strings.Contains(err.Error(), "fc_test_no_canary") {
		t.Fatalf("error lacks constraint name: %v", err)
	}
}

// T-DB-6 (AC13, AC8)
func TestFindingCommentsCascade(t *testing.T) {
	d, _ := privateCommentDB(t, "fc6")
	ctx := context.Background()
	if _, err := d.AddFindingComment(ctx, 999999, "operator", "ARTEX", "x"); !errors.Is(err, ErrFindingNotFound) {
		t.Fatalf("missing finding: %v", err)
	}
	orphans := func() int {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM finding_comments c LEFT JOIN findings f ON f.id=c.finding_id WHERE f.id IS NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	survivor := mustFinding(t, d, 0, "survivor")
	mustComment(t, d, survivor, "stay")
	mustComment(t, d, survivor, "stay too")
	newTask := func(name string) int64 {
		tk, err := d.CreateTask(name, "goal", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	paths := map[string]func(fid, tid int64) error{
		"DeleteFinding": func(fid, _ int64) error { _, err := d.DeleteFinding(fid); return err },
		"DeleteFindingsByTask": func(_, tid int64) error {
			_, err := d.DeleteFindingsByTask(tid)
			return err
		},
		"DeleteTaskCascade": func(_, tid int64) error {
			_, err := d.DeleteTaskCascade(tid, false, true)
			return err
		},
		"raw delete": func(_, tid int64) error {
			_, err := d.Exec(`DELETE FROM findings WHERE task_id=$1`, tid)
			return err
		},
	}
	for name, del := range paths {
		tid := newTask("cascade " + name)
		fid := mustFinding(t, d, tid, name)
		mustComment(t, d, fid, "gone 1")
		mustComment(t, d, fid, "gone 2")
		if err := del(fid, tid); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n := countComments(t, d, fid); n != 0 {
			t.Fatalf("%s: %d comments remain", name, n)
		}
		if n := countComments(t, d, survivor); n != 2 {
			t.Fatalf("%s: survivor has %d comments", name, n)
		}
		if n := orphans(); n != 0 {
			t.Fatalf("%s: %d orphans", name, n)
		}
	}
}

// T-DB-7 (AC14, non-archive part)
func TestFindingCommentsSurviveFindingUpdates(t *testing.T) {
	d, _ := privateCommentDB(t, "fc7")
	ctx := context.Background()
	tk, err := d.CreateTask("survive", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	es := d.Exploration(tk.ExplorationID)
	nodeID, err := es.AddNode(KindFinding, map[string]any{"summary": "x", "severity": "high"}, 5, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(tk.ID, nodeID, "xss", "name", "high", "summary", "evidence", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		mustComment(t, d, fid, fmt.Sprintf("note %d", i))
	}
	snapshot := func() []*FindingComment {
		got, _, err := d.ListFindingComments(ctx, fid, "", 50)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := snapshot()
	same := func(step string) {
		t.Helper()
		if got := snapshot(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed comments:\n%+v\n%+v", step, got, want)
		}
	}
	if _, _, _, err := d.SetFindingStatusWithNotify(ctx, fid, FindingConfirmed); err != nil {
		t.Fatal(err)
	}
	same("status")
	if _, err := d.SetFindingSeverity(fid, SeverityLow); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetFindingName(fid, "renamed"); err != nil {
		t.Fatal(err)
	}
	same("severity/name")
	if n, err := d.SetFindingReportByNodeID(nodeID, "# report"); err != nil || n != 1 {
		t.Fatalf("report n=%d err=%v", n, err)
	}
	same("report")

	r, _, created, err := d.CreateFindingRetest(ctx, fid, "notes")
	if err != nil || !created {
		t.Fatalf("retest created=%t err=%v", created, err)
	}
	if ok, err := d.StartFindingRetest(ctx, r.ID); err != nil || !ok {
		t.Fatalf("start=%t %v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *r.ConversationID, "fixed", "summary", "proof"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(r.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	same("retest")

	if err := d.DeleteTask(tk.ID); err != nil {
		t.Fatal(err)
	}
	if f, _ := d.GetFinding(fid); f == nil {
		t.Fatal("plain task delete removed the finding")
	}
	same("plain task delete")
}

// T-DB-8 (AC6, concurrency)
func TestFindingCommentsConcurrentWritersAndReader(t *testing.T) {
	d, _ := privateCommentDB(t, "fc8")
	const writers, perWriter, seed = 8, 6, 10
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fid := mustFinding(t, d, 0, "concurrent")
	for i := range seed {
		mustComment(t, d, fid, fmt.Sprintf("seed %d", i))
	}
	var wg sync.WaitGroup
	var done atomic.Int32
	for w := range writers {
		wg.Go(func() {
			defer done.Add(1)
			for i := range perWriter {
				if _, err := d.AddFindingComment(ctx, fid, "operator", "ARTEX", fmt.Sprintf("w%d-%d", w, i)); err != nil {
					t.Errorf("writer %d: %v", w, err)
					return
				}
			}
		})
	}
	walks := 0
	for {
		finished := done.Load() == writers
		var before []int64
		rows, err := d.QueryContext(ctx, `SELECT id FROM finding_comments WHERE finding_id=$1 ORDER BY id`, fid)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			before = append(before, id)
		}
		rows.Close()
		seen := map[int64]bool{}
		var last int64
		cursor := ""
		for {
			got, next, err := d.ListFindingComments(ctx, fid, cursor, 3)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got {
				if seen[c.ID] || c.ID <= last {
					t.Fatalf("repeat or disorder: id %d after %d", c.ID, last)
				}
				seen[c.ID], last = true, c.ID
			}
			if next == "" {
				break
			}
			cursor = next
		}
		for _, id := range before {
			if !seen[id] {
				t.Fatalf("id %d existed before the walk but was not returned", id)
			}
		}
		walks++
		if finished {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("deadline: %v", ctx.Err())
		}
	}
	wg.Wait()
	if n := countComments(t, d, fid); n != seed+writers*perWriter {
		t.Fatalf("count=%d want %d (walks=%d)", n, seed+writers*perWriter, walks)
	}
}

// T-DB-9 (I10)
func TestFindingCommentAddDoesNotWaitForEvidenceLock(t *testing.T) {
	d, _ := privateCommentDB(t, "fc9")
	a := mustFinding(t, d, 0, "held")
	b := mustFinding(t, d, 0, "free")
	held, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- d.WithEvidenceTx(context.Background(), func(tx *sql.Tx) error {
			if err := LockFindingEvidenceTx(tx, a, nil); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-finished:
		t.Fatalf("evidence tx ended early: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("evidence tx did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.AddFindingComment(ctx, b, "operator", "ARTEX", "does not wait")
	close(release)
	if err != nil {
		t.Fatalf("AddFindingComment while evidence lock held: %v", err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

// T-DB-10 (AC19) and archive exclusion (AC10, store half)
func TestFindingCommentCap(t *testing.T) {
	d, _ := privateCommentDB(t, "fc10")
	ctx := context.Background()
	fid := mustFinding(t, d, 0, "cap")
	other := mustFinding(t, d, 0, "cap other")
	if _, err := d.Exec(`INSERT INTO finding_comments(finding_id, body, author_subject) SELECT $1, 'c'||g, 'ARTEX' FROM generate_series(1,$2) g`, fid, MaxFindingCommentsPerFinding-1); err != nil {
		t.Fatal(err)
	}
	mustComment(t, d, fid, "the 2000th")
	if _, err := d.AddFindingComment(ctx, fid, "operator", "ARTEX", "one too many"); !errors.Is(err, ErrFindingCommentLimit) {
		t.Fatalf("over cap: %v", err)
	}
	if n := countComments(t, d, fid); n != MaxFindingCommentsPerFinding {
		t.Fatalf("count=%d", n)
	}
	mustComment(t, d, other, "other finding unaffected")
}

func TestFindingCommentBlockedWhileTaskArchived(t *testing.T) {
	d, _ := privateCommentDB(t, "fc11")
	tk, err := d.CreateTask("archive guard", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fid := mustFinding(t, d, tk.ID, "guarded")
	mustComment(t, d, fid, "before archive")
	if _, err := d.Exec(`INSERT INTO task_archives(task_id, state) VALUES ($1,'archiving')`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFindingComment(context.Background(), fid, "operator", "ARTEX", "blocked"); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatalf("archiving: %v", err)
	}
	if _, err := d.Exec(`UPDATE task_archives SET state='archive_failed' WHERE task_id=$1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	mustComment(t, d, fid, "failed archive does not block")
	if n := countComments(t, d, fid); n != 2 {
		t.Fatalf("count=%d", n)
	}
}

// Eight writers race for the last three slots of a pre-filled finding: exactly
// three win, the rest see ErrFindingCommentLimit, the count never passes the cap.
func TestFindingCommentCapConcurrent(t *testing.T) {
	d, _ := privateCommentDB(t, "fc19")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fid := mustFinding(t, d, 0, "cap concurrent")
	const room, writers = 3, 8
	if _, err := d.Exec(`INSERT INTO finding_comments(finding_id, body, author_subject) SELECT $1, 'c'||g, 'ARTEX' FROM generate_series(1,$2) g`, fid, MaxFindingCommentsPerFinding-room); err != nil {
		t.Fatal(err)
	}
	var ok, limited, other atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range writers {
		wg.Go(func() {
			<-start
			_, err := d.AddFindingComment(ctx, fid, "operator", "ARTEX", fmt.Sprintf("racer %d", w))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrFindingCommentLimit):
				limited.Add(1)
			default:
				other.Add(1)
				t.Errorf("writer %d: %v", w, err)
			}
		})
	}
	close(start)
	wg.Wait()
	if ok.Load() != room || limited.Load() != writers-room || other.Load() != 0 {
		t.Fatalf("ok=%d limited=%d other=%d, want %d/%d/0", ok.Load(), limited.Load(), other.Load(), room, writers-room)
	}
	if n := countComments(t, d, fid); n != MaxFindingCommentsPerFinding {
		t.Fatalf("count=%d want %d", n, MaxFindingCommentsPerFinding)
	}
}

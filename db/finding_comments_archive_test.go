package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Archive and restore of analyst comments (issue #65, slice S2). Every test uses
// its own private database (suffixes fc12..fc17) and never skips when the
// database is configured.

const archiveCommentCanary = "CANARY-COMMENT-BODY-7731"

type commentRow struct {
	ID        int64
	FindingID int64
	Body      string
	Kind      string
	Subject   string
	CreatedAt time.Time
}

func loadCommentRows(t *testing.T, d *DB, findingIDs ...int64) []commentRow {
	t.Helper()
	rows, err := d.Query(`SELECT id, finding_id, body, author_kind, author_subject, created_at
FROM finding_comments WHERE finding_id = ANY($1::bigint[]) ORDER BY finding_id, id`, findingIDs)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []commentRow
	for rows.Next() {
		var r commentRow
		if err := rows.Scan(&r.ID, &r.FindingID, &r.Body, &r.Kind, &r.Subject, &r.CreatedAt); err != nil {
			t.Fatal(err)
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameCommentRows(a, b []commentRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.ID != y.ID || x.FindingID != y.FindingID || x.Body != y.Body || x.Kind != y.Kind || x.Subject != y.Subject || !x.CreatedAt.Equal(y.CreatedAt) {
			return false
		}
	}
	return true
}

func commentRowsOf(t *testing.T, d *DB, taskID int64) []commentRow {
	t.Helper()
	var ids []int64
	rows, err := d.Query(`SELECT id FROM findings WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return loadCommentRows(t, d, ids...)
}

type archivedComments struct {
	taskID   int64
	archive  *TaskArchive
	snapshot *TaskArchiveSnapshot
	findings []int64
	before   []commentRow
}

// archiveTaskWithComments builds a paused task with one finding per entry of
// perFinding (that many comments each), archives it completely (findings and
// comments are deleted from the hot store) and returns the snapshot as it would
// be read back from the package (JSON round trip).
func archiveTaskWithComments(t *testing.T, d *DB, label string, perFinding ...int) *archivedComments {
	t.Helper()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("archive comments "+label, "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fx := &archivedComments{taskID: task.ID}
	for i, n := range perFinding {
		fid := mustFinding(t, d, task.ID, fmt.Sprintf("%s-%d", label, i))
		fx.findings = append(fx.findings, fid)
		for j := 0; j < n; j++ {
			subject := "ARTEX"
			if j%2 == 1 {
				subject = "second-author"
			}
			if _, err := d.AddFindingComment(context.Background(), fid, "operator", subject,
				fmt.Sprintf("%s note %d.%d\nline two é中", archiveCommentCanary, i, j)); err != nil {
				t.Fatal(err)
			}
		}
	}
	fx.before = loadCommentRows(t, d, fx.findings...)
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	if fx.archive, err = d.QueueTaskArchive(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteTaskArchive(fx.archive.ID, snapshot, "/tmp/finding-comments-test.tar.zst", "fixture", 1, 1); err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	fx.snapshot = &TaskArchiveSnapshot{}
	if err = json.Unmarshal(blob, fx.snapshot); err != nil {
		t.Fatal(err)
	}
	return fx
}

func startRestore(t *testing.T, d *DB, fx *archivedComments) {
	t.Helper()
	if _, err := d.QueueTaskArchiveRestore(fx.archive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func cloneSnapshot(s *TaskArchiveSnapshot) *TaskArchiveSnapshot {
	c := *s
	c.Tables = make(map[string]json.RawMessage, len(s.Tables))
	for k, v := range s.Tables {
		c.Tables[k] = v
	}
	return &c
}

// editCommentRows rewrites the finding_comments table of a cloned snapshot.
func editCommentRows(t *testing.T, s *TaskArchiveSnapshot, edit func(rows []map[string]any) []map[string]any) *TaskArchiveSnapshot {
	t.Helper()
	c := cloneSnapshot(s)
	var rows []map[string]any
	if err := json.Unmarshal(c.Tables["finding_comments"], &rows); err != nil {
		t.Fatalf("snapshot has no usable finding_comments table: %v (%s)", err, c.Tables["finding_comments"])
	}
	blob, err := json.Marshal(edit(rows))
	if err != nil {
		t.Fatal(err)
	}
	c.Tables["finding_comments"] = blob
	return c
}

func countTaskFindings(t *testing.T, d *DB, taskID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// T-ARC-1 (AC14): the archive carries comments and restore returns them
// unchanged; comments of another task are not touched.
func TestTaskArchiveRoundTripKeepsFindingComments(t *testing.T) {
	d, _ := privateCommentDB(t, "fc12")
	other, err := d.CreateTask("other task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherFinding := mustFinding(t, d, other.ID, "other")
	mustComment(t, d, otherFinding, "belongs to the other task")
	mustComment(t, d, otherFinding, "so does this")
	otherBefore := loadCommentRows(t, d, otherFinding)

	fx := archiveTaskWithComments(t, d, "roundtrip", 3, 0, 2)
	if len(fx.before) != 5 {
		t.Fatalf("fixture rows = %d", len(fx.before))
	}
	if got := fx.snapshot.DataCounts["finding_comments"]; got != 5 {
		t.Fatalf("manifest data_counts.finding_comments = %d, want 5 (counts=%v)", got, fx.snapshot.DataCounts)
	}
	if rowsInHotStore := loadCommentRows(t, d, fx.findings...); len(rowsInHotStore) != 0 {
		t.Fatalf("comments remain after the findings were archived: %d", len(rowsInHotStore))
	}
	if got := loadCommentRows(t, d, otherFinding); !sameCommentRows(got, otherBefore) {
		t.Fatalf("archiving changed the other task's comments: %+v", got)
	}

	startRestore(t, d, fx)
	if _, err := d.RestoreTaskArchive(fx.archive.ID, fx.snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if got := loadCommentRows(t, d, fx.findings...); !sameCommentRows(got, fx.before) {
		t.Fatalf("restored comments differ:\n got  %+v\n want %+v", got, fx.before)
	}
	if got := loadCommentRows(t, d, otherFinding); !sameCommentRows(got, otherBefore) {
		t.Fatalf("restoring changed the other task's comments: %+v", got)
	}
	// Order inside each finding is id order, and the findings keep their ids.
	for _, fid := range fx.findings {
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM findings WHERE id=$1 AND task_id=$2`, fid, fx.taskID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("finding %d not restored under its original id (n=%d err=%v)", fid, n, err)
		}
	}
}

// T-ARC-2 (AC14): older packages and rows from before a column existed.
func TestTaskArchiveRestoreWithoutFindingCommentsKey(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		d, _ := privateCommentDB(t, "fc13")
		fx := archiveTaskWithComments(t, d, "nokey", 2, 1)
		snap := cloneSnapshot(fx.snapshot)
		delete(snap.Tables, "finding_comments")
		delete(snap.DataCounts, "finding_comments")
		startRestore(t, d, fx)
		if _, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0); err != nil {
			t.Fatal(err)
		}
		if n := countTaskFindings(t, d, fx.taskID); n != 2 {
			t.Fatalf("findings restored = %d, want 2", n)
		}
		if got := loadCommentRows(t, d, fx.findings...); len(got) != 0 {
			t.Fatalf("package without the key restored %d comments", len(got))
		}
	})
	for _, raw := range []string{`null`, `[]`} {
		t.Run("table "+raw, func(t *testing.T) {
			d, _ := privateCommentDB(t, "fc14")
			fx := archiveTaskWithComments(t, d, "emptykey", 1)
			snap := cloneSnapshot(fx.snapshot)
			snap.Tables["finding_comments"] = json.RawMessage(raw)
			startRestore(t, d, fx)
			if _, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0); err != nil {
				t.Fatalf("table %s: %v", raw, err)
			}
			if n := countTaskFindings(t, d, fx.taskID); n != 1 {
				t.Fatalf("findings restored = %d, want 1", n)
			}
			if got := loadCommentRows(t, d, fx.findings...); len(got) != 0 {
				t.Fatalf("table %s restored %d comments", raw, len(got))
			}
		})
	}
	t.Run("rows without author_kind", func(t *testing.T) {
		d, _ := privateCommentDB(t, "fc15")
		fx := archiveTaskWithComments(t, d, "nokind", 3)
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			delete(rows[0], "author_kind")
			rows[1]["author_kind"] = nil
			return rows
		})
		startRestore(t, d, fx)
		if _, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0); err != nil {
			t.Fatal(err)
		}
		got := loadCommentRows(t, d, fx.findings...)
		if len(got) != 3 {
			t.Fatalf("restored %d comments", len(got))
		}
		for _, r := range got {
			if r.Kind != "operator" {
				t.Fatalf("comment %d author_kind = %q, want operator", r.ID, r.Kind)
			}
		}
	})
}

// T-ARC-3 (AC14): bad packages abort the whole restore and change nothing.
func TestTaskArchiveRestoreRejectsBadFindingComments(t *testing.T) {
	d, _ := privateCommentDB(t, "fc16")
	// A live finding of another task, with a comment whose id will collide.
	other, err := d.CreateTask("bystander", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign := mustFinding(t, d, other.ID, "bystander")
	bystander := mustComment(t, d, foreign, "bystander comment")

	fx := archiveTaskWithComments(t, d, "reject", 2)
	startRestore(t, d, fx)
	foreignBefore := loadCommentRows(t, d, foreign)

	assertUntouched := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil {
			t.Fatal("restore succeeded")
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
		if strings.Contains(err.Error(), archiveCommentCanary) {
			t.Fatalf("error carries a comment body: %v", err)
		}
		if n := countTaskFindings(t, d, fx.taskID); n != 0 {
			t.Fatalf("failed restore left %d findings", n)
		}
		if got := loadCommentRows(t, d, fx.findings...); len(got) != 0 {
			t.Fatalf("failed restore left %d comments", len(got))
		}
		if got := loadCommentRows(t, d, foreign); !sameCommentRows(got, foreignBefore) {
			t.Fatalf("failed restore changed another finding's comments: %+v", got)
		}
	}

	t.Run("foreign finding id", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[1]["finding_id"] = foreign
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "outside the archived task")
	})
	t.Run("null finding id", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[0]["finding_id"] = nil
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "outside the archived task")
	})
	t.Run("nonexistent finding id", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[0]["finding_id"] = int64(987654321)
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "outside the archived task")
	})
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("format version %d with comment rows", version), func(t *testing.T) {
			snap := cloneSnapshot(fx.snapshot)
			snap.FormatVersion = version
			_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
			if !errors.Is(err, ErrTaskArchiveFormatMismatch) {
				t.Fatalf("err = %v, want ErrTaskArchiveFormatMismatch", err)
			}
			assertUntouched(t, err, "format")
		})
	}
	t.Run("id collision", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[1]["id"] = bystander.ID
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "23505")
	})
	badBody := func(name, body, want string) {
		t.Run(name, func(t *testing.T) {
			snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
				rows[0]["body"] = body
				return rows
			})
			_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
			assertUntouched(t, err, want)
		})
	}
	badBody("whitespace-only body", "   ", "body must not be empty")
	badBody("newline-only body", "\n\n\t\n", "body must not be empty")
	badBody("over-long body without echoing it", archiveCommentCanary+strings.Repeat("x", MaxFindingCommentRunes+1), "body is too long")
	badBody("bidi body", archiveCommentCanary+"\u202e", "bidirectional control characters")
	badBody("c1 control body", archiveCommentCanary+"\u0085x", "control characters")
	badBody("body changed by normalisation", " "+archiveCommentCanary+" ", "not in normal form")
	badBody("CRLF body", archiveCommentCanary+"\r\nx", "not in normal form")
	t.Run("non-string body", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[0]["body"] = 42
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "body is not a string")
	})
	t.Run("empty author subject", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[0]["author_subject"] = ""
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "author subject")
	})
	t.Run("empty author kind", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[0]["author_kind"] = ""
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "author kind")
	})
	t.Run("over-long author kind", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[0]["author_kind"] = strings.Repeat("k", 201)
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "author kind")
	})
	t.Run("finding over the per-finding cap", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			for i := 0; len(rows) <= MaxFindingCommentsPerFinding; i++ {
				extra := map[string]any{}
				for k, v := range rows[0] {
					extra[k] = v
				}
				extra["id"] = int64(900000000 + i)
				rows = append(rows, extra)
			}
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "per-finding comment limit")
	})
	t.Run("duplicate ids inside one snapshot", func(t *testing.T) {
		snap := editCommentRows(t, fx.snapshot, func(rows []map[string]any) []map[string]any {
			rows[1]["id"] = rows[0]["id"]
			return rows
		})
		_, err := d.RestoreTaskArchive(fx.archive.ID, snap, 0)
		assertUntouched(t, err, "23505")
	})

	// Nothing was half-applied: the untouched snapshot still restores cleanly.
	if _, err := d.RestoreTaskArchive(fx.archive.ID, fx.snapshot, 0); err != nil {
		t.Fatalf("valid snapshot after the rejections: %v", err)
	}
	if got := loadCommentRows(t, d, fx.findings...); !sameCommentRows(got, fx.before) {
		t.Fatalf("restored comments differ: %+v", got)
	}
}

// T-ARC-5 (AC10): a restored finding is writable again once the archive row is gone.
func TestFindingCommentAfterRestore(t *testing.T) {
	d, _ := privateCommentDB(t, "fc17")
	fx := archiveTaskWithComments(t, d, "afterrestore", 2)
	startRestore(t, d, fx)
	if _, err := d.RestoreTaskArchive(fx.archive.ID, fx.snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddFindingComment(context.Background(), fx.findings[0], "operator", "ARTEX", "during restore"); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatalf("add while the archive row still exists: %v", err)
	}
	if err := d.CompleteTaskArchiveRestore(fx.archive.ID); err != nil {
		t.Fatal(err)
	}
	c, err := d.AddFindingComment(context.Background(), fx.findings[0], "operator", "ARTEX", "after restore")
	if err != nil {
		t.Fatalf("add after restore: %v", err)
	}
	var maxRestored int64
	for _, r := range fx.before {
		if r.ID > maxRestored {
			maxRestored = r.ID
		}
	}
	if c.ID <= maxRestored {
		t.Fatalf("new comment id %d collides with or precedes restored ids (max %d)", c.ID, maxRestored)
	}
	list, next, err := d.ListFindingComments(context.Background(), fx.findings[0], "", 50)
	if err != nil || next != "" {
		t.Fatalf("list: %v next=%q", err, next)
	}
	if len(list) != 3 || list[2].ID != c.ID || list[0].ID != fx.before[0].ID {
		t.Fatalf("unexpected list after restore: %+v", list)
	}
}

// AC13: the delete paths used around archiving remove comments with their
// findings and leave other tasks' comments alone (task with several findings).
func TestFindingCommentsCascadeOnTaskFindingDeletes(t *testing.T) {
	d, _ := privateCommentDB(t, "fc18")
	survivorTask, err := d.CreateTask("survivor", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	survivor := mustFinding(t, d, survivorTask.ID, "survivor")
	mustComment(t, d, survivor, "stays")
	before := loadCommentRows(t, d, survivor)
	for name, del := range map[string]func(taskID int64) error{
		"DeleteFindingsByTask": func(id int64) error { _, err := d.DeleteFindingsByTask(id); return err },
		"raw DELETE":           func(id int64) error { _, err := d.Exec(`DELETE FROM findings WHERE task_id=$1`, id); return err },
	} {
		tk, err := d.CreateTask("cascade "+name, "goal", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		a, b := mustFinding(t, d, tk.ID, name+" a"), mustFinding(t, d, tk.ID, name+" b")
		mustComment(t, d, a, "a1")
		mustComment(t, d, a, "a2")
		mustComment(t, d, b, "b1")
		if err := del(tk.ID); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := loadCommentRows(t, d, a, b); len(got) != 0 {
			t.Fatalf("%s: %d comments remain", name, len(got))
		}
		if got := loadCommentRows(t, d, survivor); !sameCommentRows(got, before) {
			t.Fatalf("%s: survivor comments changed: %+v", name, got)
		}
	}
}

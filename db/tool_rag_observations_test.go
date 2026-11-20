package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func toolRagObservation(taskID int64) *ToolRAGObservation {
	return &ToolRAGObservation{
		TaskID: taskID, AgentKey: "planner", Scorer: "bm25-v1", Mode: "observe", Outcome: "projected",
		K: 12, HostListCount: 30, KeepCount: 8, CandidateCount: 22, NonzeroCount: 5, WouldHideCount: 10,
		DeferredCount: 4, LockedAtStart: 2, UnlockedDuringRun: 1, QueryTokens: 17, QueryTruncated: true, DurationUS: 310,
		Ranking: []string{"nmap", "httpx"},
		Called:  map[string]ToolRAGCall{"nmap": {N: 3, Class: "candidate"}},
	}
}

func toolRagCount(t *testing.T, d *DB, taskID int64) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM tool_rag_observations WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func toolRagTask(t *testing.T, d *DB, label string) *Task {
	t.Helper()
	task, err := d.CreateTaskWithOptions("tool rag "+label, "observation rows", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func toolRagDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestToolRagInsertStoresEveryColumn(t *testing.T) {
	d := toolRagDB(t)
	task := toolRagTask(t, d, "columns")
	defer d.DeleteTask(task.ID)

	obs := toolRagObservation(task.ID)
	obs.ExplorationID, obs.IntentID = task.ExplorationID, 77
	if err := d.InsertToolRAGObservation(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	var (
		expID, intentID                  *int64
		agent, scorer, mode, outcome     string
		k, host, keep, cand, nz, hide    int
		deferred, locked, unlocked, toks int
		truncated                        bool
		dur                              int
		ranking                          string
		called                           string
		ts                               time.Time
	)
	if err := d.QueryRow(`SELECT ts, exploration_id, intent_id, agent_key, scorer, mode, outcome, k, host_list_count,
keep_count, candidate_count, nonzero_count, would_hide_count, deferred_count, locked_at_start, unlocked_during_run,
query_tokens, query_truncated, duration_us, array_to_string(ranking, ','), called::text
FROM tool_rag_observations WHERE task_id=$1`, task.ID).Scan(&ts, &expID, &intentID, &agent, &scorer, &mode, &outcome,
		&k, &host, &keep, &cand, &nz, &hide, &deferred, &locked, &unlocked, &toks, &truncated, &dur, &ranking, &called); err != nil {
		t.Fatal(err)
	}
	if expID == nil || *expID != task.ExplorationID || intentID == nil || *intentID != 77 {
		t.Errorf("ids = %v %v", expID, intentID)
	}
	if agent != "planner" || scorer != "bm25-v1" || mode != "observe" || outcome != "projected" || k != 12 || host != 30 ||
		keep != 8 || cand != 22 || nz != 5 || hide != 10 || deferred != 4 || locked != 2 || unlocked != 1 || toks != 17 ||
		!truncated || dur != 310 || ranking != "nmap,httpx" || called != `{"nmap": {"n": 3, "class": "candidate"}}` {
		t.Errorf("row mismatch: %s %s %s %s k=%d ranking=%q called=%s", agent, scorer, mode, outcome, k, ranking, called)
	}
	if time.Since(ts) > time.Minute {
		t.Errorf("ts %v is not the insert time", ts)
	}
}

func TestToolRagInsertUnknownIDsAreNullAndEmptyCollectionsDefault(t *testing.T) {
	d := toolRagDB(t)
	task := toolRagTask(t, d, "nulls")
	defer d.DeleteTask(task.ID)

	obs := toolRagObservation(task.ID)
	obs.Ranking, obs.Called = nil, nil
	if err := d.InsertToolRAGObservation(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	var expNull, intentNull bool
	var rankLen int
	var called string
	if err := d.QueryRow(`SELECT exploration_id IS NULL, intent_id IS NULL, cardinality(ranking), called::text
FROM tool_rag_observations WHERE task_id=$1`, task.ID).Scan(&expNull, &intentNull, &rankLen, &called); err != nil {
		t.Fatal(err)
	}
	if !expNull || !intentNull || rankLen != 0 || called != "{}" {
		t.Errorf("exploration null=%v intent null=%v ranking len=%d called=%s", expNull, intentNull, rankLen, called)
	}
}

func TestToolRagInsertRefusesRowsBreakingTheCaps(t *testing.T) {
	d := toolRagDB(t)
	task := toolRagTask(t, d, "caps")
	defer d.DeleteTask(task.ID)

	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("tool_%d", i)
		}
		return out
	}
	calledOf := func(n int) map[string]ToolRAGCall {
		out := map[string]ToolRAGCall{}
		for _, name := range names(n) {
			out[name] = ToolRAGCall{N: 1, Class: "other"}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ToolRAGObservation)
	}{
		{"ranking over 256", func(o *ToolRAGObservation) { o.Ranking = names(ToolRAGMaxRanking + 1) }},
		{"called over 257", func(o *ToolRAGObservation) { o.Called = calledOf(ToolRAGMaxCalled + 1) }},
		{"ranking name over 128 bytes", func(o *ToolRAGObservation) { o.Ranking = []string{strings.Repeat("a", 129)} }},
		{"called name over 128 bytes", func(o *ToolRAGObservation) { o.Called = map[string]ToolRAGCall{strings.Repeat("a", 129): {N: 1}} }},
		{"ranking control character", func(o *ToolRAGObservation) { o.Ranking = []string{"a\nb"} }},
		{"called control character", func(o *ToolRAGObservation) { o.Called = map[string]ToolRAGCall{"a\x00b": {N: 1}} }},
		{"ranking invalid UTF-8", func(o *ToolRAGObservation) { o.Ranking = []string{"a\xffb"} }},
		{"empty name", func(o *ToolRAGObservation) { o.Ranking = []string{""} }},
		{"no task", func(o *ToolRAGObservation) { o.TaskID = 0 }},
	} {
		obs := toolRagObservation(task.ID)
		tc.mutate(obs)
		if err := d.InsertToolRAGObservation(context.Background(), obs); !errors.Is(err, ErrToolRAGObservationInvalid) {
			t.Errorf("%s: err = %v, want ErrToolRAGObservationInvalid", tc.name, err)
		}
	}
	if n := toolRagCount(t, d, task.ID); n != 0 {
		t.Fatalf("%d rows stored by refused inserts", n)
	}
	// The limits themselves are accepted: 256 ranked names of 128 bytes, 257 called entries.
	obs := toolRagObservation(task.ID)
	obs.Ranking = []string{strings.Repeat("é", 64)}
	for i := 1; i < ToolRAGMaxRanking; i++ {
		obs.Ranking = append(obs.Ranking, fmt.Sprintf("%0128d", i))
	}
	obs.Called = calledOf(ToolRAGMaxCalled)
	if err := d.InsertToolRAGObservation(context.Background(), obs); err != nil {
		t.Fatalf("a row at the limits was refused: %v", err)
	}
}

// AC19: deleting a task through either path removes its rows, and a late insert fails with 23503.
func TestToolRagTaskDeletionRemovesRows(t *testing.T) {
	d := toolRagDB(t)
	for _, path := range []string{"DeleteTask", "DeleteTaskCascade"} {
		t.Run(path, func(t *testing.T) {
			task := toolRagTask(t, d, path)
			other := toolRagTask(t, d, path+" other")
			defer d.DeleteTask(other.ID)
			defer d.DeleteTask(task.ID)
			for _, id := range []int64{task.ID, task.ID, other.ID} {
				if err := d.InsertToolRAGObservation(context.Background(), toolRagObservation(id)); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if path == "DeleteTask" {
				err = d.DeleteTask(task.ID)
			} else {
				_, err = d.DeleteTaskCascade(task.ID, true, true, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			if n := toolRagCount(t, d, task.ID); n != 0 {
				t.Errorf("%d rows left after %s", n, path)
			}
			if n := toolRagCount(t, d, other.ID); n != 1 {
				t.Errorf("another task's rows = %d, want 1", n)
			}
			err = d.InsertToolRAGObservation(context.Background(), toolRagObservation(task.ID))
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
				t.Errorf("insert for a deleted task: err = %v, want SQLSTATE 23503", err)
			}
			if n := toolRagCount(t, d, task.ID); n != 0 {
				t.Errorf("the refused insert left %d rows", n)
			}
		})
	}
}

// AC24: archiving removes the observations (the tasks row stays), leaves other tasks alone, and a restore brings none back.
func TestToolRagArchiveRemovesObservationsAndRestoreAddsNone(t *testing.T) {
	d := toolRagDB(t)
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task := toolRagTask(t, d, "archive")
	other := toolRagTask(t, d, "archive other")
	defer func() {
		_, _ = d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		_ = d.DeleteTask(task.ID)
		_ = d.DeleteTask(other.ID)
	}()
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{task.ID, task.ID, other.ID} {
		obs := toolRagObservation(id)
		if id == task.ID {
			obs.ExplorationID = task.ExplorationID
		}
		if err := d.InsertToolRAGObservation(context.Background(), obs); err != nil {
			t.Fatal(err)
		}
	}
	// One row that names the exploration only (task_id belongs to the other task): the shared delete predicate
	// is "task_id OR exploration_id", as for the usage ledgers.
	if _, err := d.Exec(`INSERT INTO tool_rag_observations(task_id, exploration_id, agent_key, scorer, mode, outcome, k,
host_list_count, keep_count, candidate_count, nonzero_count, would_hide_count, deferred_count, locked_at_start,
unlocked_during_run, query_tokens, query_truncated, duration_us)
VALUES($1,$2,'worker','bm25-v1','observe','projected',12,0,0,0,0,0,0,0,0,0,false,0)`, other.ID, task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	otherBefore := toolRagCount(t, d, other.ID)

	archive, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimTaskArchiveJob(t.Context()); err != nil || claimed == nil || claimed.ID != archive.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	var llmRecords bytes.Buffer
	snapshot, err := d.SnapshotTaskArchiveWithLLMRecords(task.ID, &llmRecords)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Tables["tool_rag_observations"]; ok {
		t.Fatal("observations are part of the archive snapshot")
	}
	if err := d.CompleteTaskArchive(archive.ID, snapshot, "/tmp/test-tool-rag.tar.zst", "abc", 100, 50); err != nil {
		t.Fatal(err)
	}
	var taskRows int
	if err := d.QueryRow(`SELECT count(*) FROM tasks WHERE id=$1`, task.ID).Scan(&taskRows); err != nil || taskRows != 1 {
		t.Fatalf("tasks row after archiving: %d, %v", taskRows, err)
	}
	var left int
	if err := d.QueryRow(`SELECT count(*) FROM tool_rag_observations WHERE task_id=$1 OR exploration_id=$2`, task.ID, task.ExplorationID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("archiving left %d observation rows", left)
	}
	// The exploration-only row went with the archived exploration; the other task's own row stays.
	if n := toolRagCount(t, d, other.ID); n != otherBefore-1 || n != 1 {
		t.Errorf("other task rows = %d (before %d), want 1", n, otherBefore)
	}

	if _, err := d.QueueTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
	if claimed, err := d.ClaimTaskArchiveJob(t.Context()); err != nil || claimed == nil || claimed.State != Restoring {
		t.Fatalf("restore claim = %+v, %v", claimed, err)
	}
	if _, err := d.RestoreTaskArchiveWithLLMRecords(archive.ID, snapshot, int64((10 * time.Minute).Seconds()), bytes.NewReader(llmRecords.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM tool_rag_observations WHERE task_id=$1 OR exploration_id=$2`, task.ID, task.ExplorationID).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("restore brought back %d observation rows", left)
	}
	if err := d.CompleteTaskArchiveRestore(archive.ID); err != nil {
		t.Fatal(err)
	}
}

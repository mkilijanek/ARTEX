package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// fakeInsert stands in for db.InsertToolRAGObservation: it records every call and
// lets a test block, fail or slow it down.
type fakeInsert struct {
	fn       func(ctx context.Context, o *db.ToolRAGObservation) error
	calls    atomic.Int32
	inflight atomic.Int32
}

func (f *fakeInsert) insert(ctx context.Context, o *db.ToolRAGObservation) error {
	f.calls.Add(1)
	f.inflight.Add(1)
	defer f.inflight.Add(-1)
	if f.fn == nil {
		return nil
	}
	return f.fn(ctx, o)
}

func ragRow(taskID int64) db.ToolRAGObservation {
	return db.ToolRAGObservation{
		TaskID: taskID, AgentKey: "worker", Scorer: "bm25-v1", Mode: "observe", Outcome: "projected", K: 12,
		Ranking: []string{"nmap"}, Called: map[string]db.ToolRAGCall{"nmap": {N: 1, Class: "candidate"}},
	}
}

// ragLines returns the [tool-rag] lines of a captured log.
func ragLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "[tool-rag]") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

func setToolRAGSeams(t *testing.T, queue int, drain, insert time.Duration) {
	t.Helper()
	q, d, i := toolRAGQueueSize, toolRAGDrainDeadline, toolRAGInsertTimeout
	toolRAGQueueSize, toolRAGDrainDeadline, toolRAGInsertTimeout = queue, drain, insert
	t.Cleanup(func() { toolRAGQueueSize, toolRAGDrainDeadline, toolRAGInsertTimeout = q, d, i })
}

// writerStacks returns the goroutine dump of the whole process.
func writerStacks() string {
	var buf strings.Builder
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		return err.Error()
	}
	return buf.String()
}

// waitNoWriter polls dump until no writer goroutine is in it, for at most timeout. Close
// returns when the goroutine has called wg.Done (a deferred call), but the goroutine is
// still in its epilogue for a moment after that, and a dump taken in that window (likely
// under -race and load) lists it. The bound keeps the check honest: a goroutine that
// never ends is still reported.
func waitNoWriter(dump func() string, timeout, step time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		out := dump()
		if !strings.Contains(out, "(*toolRAGWriter).run") {
			return out, true
		}
		if !time.Now().Before(deadline) {
			return out, false
		}
		time.Sleep(step)
	}
}

// noWriterGoroutine fails if a writer goroutine is still alive: Close must have waited for it.
func noWriterGoroutine(t *testing.T) {
	t.Helper()
	if out, ok := waitNoWriter(writerStacks, 5*time.Second, 5*time.Millisecond); !ok {
		t.Fatalf("a tool RAG writer goroutine is still running:\n%s", out)
	}
}

func TestWaitNoWriterToleratesAGoroutineThatIsStillLeaving(t *testing.T) {
	calls := 0
	dump := func() string {
		calls++
		if calls < 4 {
			return "goroutine 7 [runnable]:\nserver.(*toolRAGWriter).run(0x1)\n"
		}
		return "goroutine 1 [running]:\n"
	}
	if out, ok := waitNoWriter(dump, 5*time.Second, time.Millisecond); !ok || calls != 4 {
		t.Fatalf("ok=%v calls=%d out=%q, want ok after 4 dumps", ok, calls, out)
	}
}

func TestWaitNoWriterStillReportsAGoroutineThatNeverEnds(t *testing.T) {
	dump := func() string { return "goroutine 7 [select]:\nserver.(*toolRAGWriter).run(0x1)\n" }
	start := time.Now()
	if _, ok := waitNoWriter(dump, 50*time.Millisecond, time.Millisecond); ok {
		t.Fatal("a writer goroutine that never ends was reported as gone")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("gave up before the timeout")
	}
}

func TestToolRAGWriterWritesQueuedRowsOnClose(t *testing.T) {
	setToolRAGSeams(t, 256, 3*time.Second, 2*time.Second)
	logs := dbtest.CaptureLog(t)
	f := &fakeInsert{fn: func(ctx context.Context, o *db.ToolRAGObservation) error {
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > 2*time.Second {
			return fmt.Errorf("insert context has no own 2 s deadline (%v, %v)", dl, ok)
		}
		time.Sleep(5 * time.Millisecond)
		return nil
	}}
	w := newToolRAGWriter(f.insert)
	for i := 1; i <= 5; i++ {
		w.Enqueue(ragRow(int64(i)))
	}
	w.Close()
	if got := f.calls.Load(); got != 5 {
		t.Fatalf("inserts = %d, want 5", got)
	}
	want := []string{"[tool-rag] writer stopped: written=5 dropped_shutdown=0 task_gone=0 invalid_row=0 writer_panic=0"}
	if got := ragLines(logs()); !equalStrings(got, want) {
		t.Fatalf("log = %q, want %q", got, want)
	}
	noWriterGoroutine(t)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestToolRAGWriterNeverStartedClosesQuietly(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	w := newToolRAGWriter((&fakeInsert{}).insert)
	w.Close()
	w.Close() // Manager.Close can run twice; only the first one reports
	if got := ragLines(logs()); !equalStrings(got, []string{"[tool-rag] writer stopped: written=0 dropped_shutdown=0 task_gone=0 invalid_row=0 writer_panic=0"}) {
		t.Fatalf("log = %q", got)
	}
	var nilWriter *toolRAGWriter
	nilWriter.Close() // a Manager built without one (tests) closes fine
	noWriterGoroutine(t)
}

// Enqueue is on the cleanup path of every planner round and worker run: it must not
// wait for the database, whatever the database is doing.
func TestToolRAGWriterEnqueueNeverBlocks(t *testing.T) {
	setToolRAGSeams(t, 1, 3*time.Second, 2*time.Second)
	logs := dbtest.CaptureLog(t)
	release := make(chan struct{})
	f := &fakeInsert{fn: func(ctx context.Context, _ *db.ToolRAGObservation) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	w := newToolRAGWriter(f.insert)
	start := time.Now()
	const rows = 1000
	for i := range rows {
		w.Enqueue(ragRow(int64(i + 1)))
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("%d enqueues took %v while the insert was stalled", rows, d)
	}
	if drops := w.log.Count("drop"); drops < rows-3 {
		t.Fatalf("dropped = %d of %d with a queue of 1", drops, rows)
	}
	close(release)
	w.Close()
	want := "[tool-rag] observation drop; 1 since start (queue full)"
	lines := ragLines(logs())
	if len(lines) != 2 || lines[0] != want || !strings.HasPrefix(lines[1], "[tool-rag] writer stopped: written=") {
		t.Fatalf("log = %q, want one drop line (rate limit) and the stop line", lines)
	}
	noWriterGoroutine(t)
}

// Failures are counted and logged once a minute per kind with the SQLSTATE or the Go
// type only: the server message (class 22 quotes values) and any other text never appear.
func TestToolRAGWriterFailuresAreContentFree(t *testing.T) {
	setToolRAGSeams(t, 256, 3*time.Second, 50*time.Millisecond)
	for name, tc := range map[string]struct {
		err  func(ctx context.Context) error
		want string
	}{
		"postgres error": {func(context.Context) error {
			return &pgconn.PgError{Code: "08006", Message: "CANARY secret value", Detail: "CANARY detail"}
		}, "[tool-rag] observation insert error; 1 since start (SQLSTATE 08006)"},
		"wrapped postgres error": {func(context.Context) error {
			return fmt.Errorf("insert: %w", &pgconn.PgError{Code: "22001", Message: "CANARY value too long: CANARY"})
		}, "[tool-rag] observation insert error; 1 since start (SQLSTATE 22001)"},
		"other error": {func(context.Context) error { return errors.New("CANARY dsn=postgres://u:p@h/db") },
			"[tool-rag] observation insert error; 1 since start (type *errors.errorString)"},
		"timeout": {func(ctx context.Context) error { <-ctx.Done(); return fmt.Errorf("CANARY: %w", ctx.Err()) },
			"[tool-rag] observation timeout; 1 since start"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := dbtest.CaptureLog(t)
			f := &fakeInsert{fn: func(ctx context.Context, _ *db.ToolRAGObservation) error { return tc.err(ctx) }}
			w := newToolRAGWriter(f.insert)
			for i := range 3 {
				w.Enqueue(ragRow(int64(i + 1)))
			}
			w.Close()
			lines := ragLines(logs())
			if len(lines) != 2 || lines[0] != tc.want || !strings.HasSuffix(lines[1], "written=0 dropped_shutdown=0 task_gone=0 invalid_row=0 writer_panic=0") {
				t.Fatalf("log = %q, want %q once, then the stop line", lines, tc.want)
			}
			if strings.Contains(logs(), "CANARY") {
				t.Fatal("log carries error text")
			}
		})
	}
}

// A row the database layer refuses (it breaks the name or size rules) is a defect of
// the sender, not a database failure: its own kind, counted apart from insert errors.
func TestToolRAGWriterClassifiesARefusedRowSeparately(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	f := &fakeInsert{fn: func(context.Context, *db.ToolRAGObservation) error {
		return fmt.Errorf("insert: %w: CANARY name", db.ErrToolRAGObservationInvalid)
	}}
	w := newToolRAGWriter(f.insert)
	w.Enqueue(ragRow(1))
	w.Enqueue(ragRow(2))
	w.Close()
	want := []string{
		"[tool-rag] observation invalid row; 1 since start",
		"[tool-rag] writer stopped: written=0 dropped_shutdown=0 task_gone=0 invalid_row=2 writer_panic=0",
	}
	if got := ragLines(logs()); !equalStrings(got, want) || strings.Contains(logs(), "CANARY") {
		t.Fatalf("log = %q, want %q", got, want)
	}
	if w.log.Count("invalid row") != 2 || w.log.Count("insert error") != 0 {
		t.Fatalf("invalid=%d insert errors=%d", w.log.Count("invalid row"), w.log.Count("insert error"))
	}
}

// A panic inside insert must not take the process down: it is recovered, counted, logged
// with the type only, and the writer carries on with the next row and still stops cleanly.
func TestToolRAGWriterSurvivesAPanickingInsert(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	f := &fakeInsert{fn: func(_ context.Context, o *db.ToolRAGObservation) error {
		if o.TaskID == 2 {
			panic("CANARY panic value")
		}
		return nil
	}}
	w := newToolRAGWriter(f.insert)
	for i := 1; i <= 3; i++ {
		w.Enqueue(ragRow(int64(i)))
	}
	w.Close()
	want := []string{
		"[tool-rag] observation writer panic; 1 since start (type string)",
		"[tool-rag] writer stopped: written=2 dropped_shutdown=0 task_gone=0 invalid_row=0 writer_panic=1",
	}
	if got := ragLines(logs()); !equalStrings(got, want) || strings.Contains(logs(), "CANARY") {
		t.Fatalf("log = %q, want %q", got, want)
	}
	if w.log.Count("writer panic") != 1 || f.calls.Load() != 3 {
		t.Fatalf("panics=%d inserts=%d", w.log.Count("writer panic"), f.calls.Load())
	}
	noWriterGoroutine(t)
}

// SQLSTATE 23503: the task was deleted or archived while the row waited. Not a failure.
func TestToolRAGWriterCountsATaskThatIsGone(t *testing.T) {
	logs := dbtest.CaptureLog(t)
	f := &fakeInsert{fn: func(context.Context, *db.ToolRAGObservation) error {
		return fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23503", Message: "CANARY foreign key"})
	}}
	w := newToolRAGWriter(f.insert)
	w.Enqueue(ragRow(1))
	w.Enqueue(ragRow(2))
	w.Close()
	want := []string{"[tool-rag] writer stopped: written=0 dropped_shutdown=0 task_gone=2 invalid_row=0 writer_panic=0"}
	if got := ragLines(logs()); !equalStrings(got, want) {
		t.Fatalf("log = %q, want only %q", got, want)
	}
}

// AC15 shutdown with a blocked insert: Close returns shortly after the drain deadline,
// what is left is counted as dropped, a flush afterwards neither panics nor logs, and
// no insert is running or starts once Close has returned.
func TestToolRAGWriterBlockedInsertIsCutAtTheDrainDeadline(t *testing.T) {
	setToolRAGSeams(t, 256, 100*time.Millisecond, 2*time.Second)
	logs := dbtest.CaptureLog(t)
	f := &fakeInsert{fn: func(ctx context.Context, _ *db.ToolRAGObservation) error { <-ctx.Done(); return ctx.Err() }}
	w := newToolRAGWriter(f.insert)
	for i := range 5 {
		w.Enqueue(ragRow(int64(i + 1)))
	}
	start := time.Now()
	w.Close()
	if d := time.Since(start); d > 100*time.Millisecond+time.Second {
		t.Fatalf("Close took %v with a 100 ms drain deadline", d)
	}
	want := []string{"[tool-rag] writer stopped: written=0 dropped_shutdown=5 task_gone=0 invalid_row=0 writer_panic=0"}
	if got := ragLines(logs()); !equalStrings(got, want) {
		t.Fatalf("log = %q, want only %q", got, want)
	}
	calls := f.calls.Load()
	w.Enqueue(ragRow(99)) // a run ending during shutdown
	time.Sleep(50 * time.Millisecond)
	if f.inflight.Load() != 0 || f.calls.Load() != calls {
		t.Fatalf("an insert ran after Close (inflight=%d calls=%d, was %d)", f.inflight.Load(), f.calls.Load(), calls)
	}
	if got := ragLines(logs()); len(got) != 1 {
		t.Fatalf("a flush after Close logged: %q", got)
	}
	if w.droppedShutdown.Load() != 6 {
		t.Fatalf("dropped_shutdown = %d, want 5 + the late one", w.droppedShutdown.Load())
	}
	noWriterGoroutine(t)
}

// ---- Manager and database (PostgreSQL 16) ----

func ragManager(t *testing.T) (*Manager, *db.Task) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := m.pg.CreateTaskWithOptions("tool rag writer", "observation rows", db.TaskCreateOptions{})
	if err != nil {
		m.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dsn, _, err := db.DSN()
		if err != nil {
			return
		}
		if d, err := db.Open(dsn); err == nil {
			d.DeleteTask(task.ID)
			d.Close()
		}
	})
	return m, task
}

func ragRowCount(t *testing.T, taskID int64) int {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM tool_rag_observations WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC15: Manager.Close stops the writer first, drains the queue into the still-open
// database, and the only [tool-rag] line is the stop line.
func TestManagerCloseDrainsToolRAGRowsBeforeTheDatabaseCloses(t *testing.T) {
	setToolRAGSeams(t, 256, 3*time.Second, 2*time.Second)
	m, task := ragManager(t)
	logs := dbtest.CaptureLog(t)
	real := m.toolRAG.insert
	m.toolRAG.insert = func(ctx context.Context, o *db.ToolRAGObservation) error {
		time.Sleep(20 * time.Millisecond) // rows are still queued when Close starts
		return real(ctx, o)
	}
	for range 5 {
		m.toolRAG.Enqueue(ragRow(task.ID))
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if n := ragRowCount(t, task.ID); n != 5 {
		t.Fatalf("rows after Close = %d, want 5", n)
	}
	want := []string{"[tool-rag] writer stopped: written=5 dropped_shutdown=0 task_gone=0 invalid_row=0 writer_panic=0"}
	if got := ragLines(logs()); !equalStrings(got, want) {
		t.Fatalf("log = %q, want only %q", got, want)
	}
	m.toolRAG.Enqueue(ragRow(task.ID)) // a run that ends after Close: no panic, no log, no row
	if got := ragLines(logs()); len(got) != 1 {
		t.Fatalf("log after a late flush = %q", got)
	}
	if n := ragRowCount(t, task.ID); n != 5 {
		t.Fatalf("a late flush wrote a row: %d", n)
	}
	noWriterGoroutine(t)
}

func TestManagerCloseWithBlockedToolRAGInsertReturnsAtTheDeadline(t *testing.T) {
	setToolRAGSeams(t, 256, 100*time.Millisecond, 2*time.Second)
	m, task := ragManager(t)
	logs := dbtest.CaptureLog(t)
	f := &fakeInsert{fn: func(ctx context.Context, _ *db.ToolRAGObservation) error { <-ctx.Done(); return ctx.Err() }}
	m.toolRAG.insert = f.insert
	for range 5 {
		m.toolRAG.Enqueue(ragRow(task.ID))
	}
	start := time.Now()
	m.Close()
	if d := time.Since(start); d > 100*time.Millisecond+time.Second {
		t.Fatalf("Close took %v", d)
	}
	want := []string{"[tool-rag] writer stopped: written=0 dropped_shutdown=5 task_gone=0 invalid_row=0 writer_panic=0"}
	if got := ragLines(logs()); !equalStrings(got, want) {
		t.Fatalf("log = %q, want only %q", got, want)
	}
	m.toolRAG.Enqueue(ragRow(task.ID))
	time.Sleep(50 * time.Millisecond)
	if f.inflight.Load() != 0 || f.calls.Load() != 1 {
		t.Fatalf("insert activity after Close: inflight=%d calls=%d", f.inflight.Load(), f.calls.Load())
	}
	if n := ragRowCount(t, task.ID); n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
	noWriterGoroutine(t)
}

// AC19 (server side): a row for a task deleted while it waited is task_gone, with no
// failure line, and the rows of a live task are written.
func TestToolRAGWriterTaskDeletedWhileQueued(t *testing.T) {
	m, task := ragManager(t)
	gone, err := m.pg.CreateTaskWithOptions("tool rag gone", "deleted before the insert", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.pg.DeleteTask(gone.ID); err != nil {
		t.Fatal(err)
	}
	logs := dbtest.CaptureLog(t)
	m.toolRAG.Enqueue(ragRow(gone.ID))
	m.toolRAG.Enqueue(ragRow(task.ID))
	m.Close()
	want := []string{"[tool-rag] writer stopped: written=1 dropped_shutdown=0 task_gone=1 invalid_row=0 writer_panic=0"}
	if got := ragLines(logs()); !equalStrings(got, want) {
		t.Fatalf("log = %q, want only %q", got, want)
	}
	if n := ragRowCount(t, task.ID); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

// A database that is closed (or failing) must not reach the run: the insert error is
// counted and logged once with the Go type, and the run's own list is unchanged.
func TestToolRAGWriterWithAClosedDatabase(t *testing.T) {
	m, task := ragManager(t)
	m.pg.Close()
	logs := dbtest.CaptureLog(t)
	for range 3 {
		m.toolRAG.Enqueue(ragRow(task.ID))
	}
	m.toolRAG.Close()
	lines := ragLines(logs())
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "[tool-rag] observation insert error; 1 since start (type ") ||
		strings.Contains(lines[0], "closed") || !strings.HasSuffix(lines[1], "written=0 dropped_shutdown=0 task_gone=0 invalid_row=0 writer_panic=0") {
		t.Fatalf("log = %q", lines)
	}
	if m.toolRAG.log.Count("insert error") != 3 {
		t.Fatalf("failures counted = %d, want 3", m.toolRAG.log.Count("insert error"))
	}
}

// The wired observer end to end: a planner or worker assembly with a task id becomes one
// row in the table, written by the writer, after Close the row is there; main agent and chat
// leave none. No canary from the query, a description or an argument reaches the row, the
// captured log or server_logs (AC16).
func TestWireToolRAGWritesOneRowPerRunAndNoContent(t *testing.T) {
	setToolRAGSeams(t, 256, 3*time.Second, 2*time.Second)
	m, task := ragManager(t)
	t.Setenv("ARTEX_TOOL_RAG", "")
	wireToolRAG(m)
	t.Cleanup(func() { agent.SetToolRAGObserver(nil) })

	var captured strings.Builder
	var capMu sync.Mutex
	sink := &logSinkT{cap: 100, subs: map[chan LogLine]struct{}{}, out: io.Discard}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		capMu.Lock()
		captured.Write(p)
		capMu.Unlock()
		return sink.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	// The sink gets its own handle: the writer's stop line is logged while m.pg is still
	// open but the async insert of that line must not race with m.Close.
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	sinkDB, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sinkDB.Close() })
	sink.SetDB(t.Context(), sinkDB)

	tools := ragTools(25, "DESC-CANARY-5 password=hunter2 https://canary.example/x")
	query := "scan canary-host.example with password=hunter2 DESC-CANARY-5 at https://canary.example/x"
	for _, tc := range []struct {
		key    string
		intent int64
	}{{"planner", 0}, {"worker", 11}, {"mainagent", 0}, {"chat-assistant", 0}} {
		out := augmentForTest(withRagRun(query, task.ID, task.ExplorationID, tc.intent), tc.key, tools)
		if len(out) != len(tools) {
			t.Fatalf("%s: %d tools, want %d", tc.key, len(out), len(tools))
		}
	}
	m.Close()
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(`SELECT agent_key, coalesce(intent_id,0), row_to_json(t)::text FROM tool_rag_observations t WHERE task_id=$1 ORDER BY id`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	var all strings.Builder
	for rows.Next() {
		var key, js string
		var intent int64
		if err := rows.Scan(&key, &intent, &js); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, fmt.Sprintf("%s/%d", key, intent))
		all.WriteString(js)
	}
	if !equalStrings(keys, []string{"planner/0", "worker/11"}) {
		t.Fatalf("rows = %v, want one planner row and one worker row, none for main agent or chat", keys)
	}
	var serverLogs string
	// The sink persists asynchronously; its own stop line proves it is live.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.QueryRow(`SELECT coalesce(string_agg(text, E'\n'), '') FROM server_logs WHERE text LIKE '%[tool-rag] writer stopped%'`).Scan(&serverLogs); err != nil {
			t.Fatal(err)
		}
		if serverLogs != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if serverLogs == "" {
		t.Fatal("the log sink never persisted the writer stop line, so the server_logs check would be vacuous")
	}
	if err := d.QueryRow(`SELECT coalesce(string_agg(text, E'\n'), '') FROM server_logs WHERE text LIKE '%[tool-rag]%'`).Scan(&serverLogs); err != nil {
		t.Fatal(err)
	}
	capMu.Lock()
	stderr := captured.String()
	capMu.Unlock()
	for _, canary := range []string{"canary-host.example", "hunter2", "DESC-CANARY-5", "canary.example"} {
		for what, text := range map[string]string{"row": all.String(), "server_logs": serverLogs, "stderr": stderr} {
			if strings.Contains(text, canary) {
				t.Errorf("%s holds %q", what, canary)
			}
		}
	}
}

// ragTools: n callable-less tools; the first carries the given description.
func ragTools(n int, firstDesc string) []actool.CoreTool {
	tools := make([]actool.CoreTool, n)
	for i := range tools {
		desc := "Unrelated helper"
		if i == 0 {
			desc = firstDesc
		}
		tools[i] = actool.Build(actool.Spec{Name: fmt.Sprintf("tool_%02d", i), Description: desc, Schema: map[string]any{"type": "object"}})
	}
	return tools
}

func withRagRun(query string, taskID, explorationID, intentID int64) context.Context {
	ctx := agent.WithToolQuery(context.Background(), query)
	return agent.WithRunInfo(ctx, agent.RunInfo{TaskID: taskID, ExplorationID: explorationID, IntentID: intentID})
}

// augmentForTest runs AugmentTools with no other hook installed (other tests wire the
// server's own) and delivers the run's cleanup, as a finished run would.
func augmentForTest(ctx context.Context, key string, base []actool.CoreTool) []actool.CoreTool {
	oldAug, oldRes, oldFind := agent.ToolAugment(), agent.ToolResolve(), agent.FindingTrafficBindingEnabled()
	agent.SetToolAugment(nil)
	agent.SetToolResolve(nil)
	agent.SetFindingTrafficBindingEnabled(nil)
	defer func() {
		agent.SetToolAugment(oldAug)
		agent.SetToolResolve(oldRes)
		agent.SetFindingTrafficBindingEnabled(oldFind)
	}()
	out, _, cleanup := agent.AugmentTools(ctx, key, base)
	cleanup()
	return out
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

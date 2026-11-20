package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mkilijanek/artex/agent"
	"github.com/mkilijanek/artex/db"
)

// toolRAGWriter persists Tool RAG observations (docs/phase-d3-plan.md 3.5). Enqueue is
// called on the cleanup path of every planner round and worker run and never waits for
// the database: it is a non-blocking send into a bounded queue, and a full queue drops
// the row and counts it. One goroutine, started by the first row, inserts the rows with
// a 2 s timeout each, on a context of its own (not the run's). Close stops it before the
// database closes.
type toolRAGWriter struct {
	insert func(context.Context, *db.ToolRAGObservation) error
	log    *agent.ToolRAGLog // insert error, timeout and drop lines, once a minute per kind

	ctx    context.Context // cancelled when the drain deadline passes
	cancel context.CancelFunc
	ch     chan db.ToolRAGObservation
	wg     sync.WaitGroup

	mu      sync.Mutex // guards closed, started and the send on ch (so Close never races a send)
	closed  bool
	started bool

	written, droppedShutdown, taskGone atomic.Int64
}

// Seams for tests.
var (
	toolRAGQueueSize     = 256
	toolRAGDrainDeadline = 3 * time.Second
	toolRAGInsertTimeout = 2 * time.Second
)

func newToolRAGWriter(insert func(context.Context, *db.ToolRAGObservation) error) *toolRAGWriter {
	ctx, cancel := context.WithCancel(context.Background())
	return &toolRAGWriter{insert: insert, log: agent.NewToolRAGLog(), ctx: ctx, cancel: cancel, ch: make(chan db.ToolRAGObservation, toolRAGQueueSize)}
}

// Enqueue hands one observation to the writer. It never blocks. After Close the row is
// dropped and counted, silently: a run that ends during shutdown is not a failure.
func (w *toolRAGWriter) Enqueue(o db.ToolRAGObservation) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		w.droppedShutdown.Add(1)
		return
	}
	if !w.started {
		w.started = true
		w.wg.Add(1)
		go w.run()
	}
	select {
	case w.ch <- o:
	default:
		w.log.Report("drop", "queue full")
	}
}

func (w *toolRAGWriter) run() {
	defer w.wg.Done()
	for o := range w.ch {
		w.write(&o)
	}
}

// write inserts one row. Past the drain deadline the row is dropped and counted; an
// insert cut by that deadline is a drop too, not a failure. A panic is recovered here
// (this goroutine has no caller to do it, and an unrecovered one ends the process):
// counted, logged with its type only, and the writer moves on to the next row.
func (w *toolRAGWriter) write(o *db.ToolRAGObservation) {
	if w.ctx.Err() != nil {
		w.droppedShutdown.Add(1)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			w.log.Report("writer panic", fmt.Sprintf("type %T", r))
		}
	}()
	ctx, cancel := context.WithTimeout(w.ctx, toolRAGInsertTimeout)
	defer cancel()
	err := w.insert(ctx, o)
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		w.written.Add(1)
	case w.ctx.Err() != nil:
		w.droppedShutdown.Add(1)
	case isFKViolation(err):
		w.taskGone.Add(1) // the task was deleted while the row waited
	case errors.Is(err, db.ErrToolRAGObservationInvalid):
		w.log.Report("invalid row", "") // refused before any SQL: a defect of the sender, not of the database
	case errors.Is(err, context.DeadlineExceeded):
		w.log.Report("timeout", "")
	case errors.As(err, &pgErr):
		w.log.Report("insert error", "SQLSTATE "+pgErr.Code)
	default:
		w.log.Report("insert error", fmt.Sprintf("type %T", err))
	}
}

// Close stops the writer: later rows are dropped, the queue is drained for at most
// toolRAGDrainDeadline (the insert in flight is cancelled at the deadline and the rest
// dropped), and Close returns only when the goroutine has ended, so no insert can run
// against a closed database. Safe on a nil writer and safe to call twice.
func (w *toolRAGWriter) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.wg.Wait()
		return
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()
	deadline := time.AfterFunc(toolRAGDrainDeadline, w.cancel)
	w.wg.Wait()
	deadline.Stop()
	w.cancel()
	// The summary is taken when the goroutine has ended. A run that ends later still
	// calls Enqueue: that row is counted in droppedShutdown but not printed again.
	log.Printf("[tool-rag] writer stopped: written=%d dropped_shutdown=%d task_gone=%d invalid_row=%d writer_panic=%d",
		w.written.Load(), w.droppedShutdown.Load(), w.taskGone.Load(), w.log.Count("invalid row"), w.log.Count("writer panic"))
}

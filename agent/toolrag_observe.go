package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
)

// Observation wiring for Tool RAG (docs/phase-d3-plan.md 3.1-3.5): the mode switch,
// the observer hook, the per-run flush and the content-free problem log. Nothing here
// can change the tool list AugmentTools returns.

const (
	toolRAGEnv         = "ARTEX_TOOL_RAG"
	toolRAGModeObserve = "observe"
	toolRAGModeOff     = "off"
)

// ToolRAGObservation is what one planner round or worker run reports: names and
// counts only (see db.ToolRAGObservation).
type ToolRAGObservation = db.ToolRAGObservation

// ToolRAGObserver receives one observation per planner round and worker run, when the
// run's cleanup runs. It must not block: it runs on the cleanup path, before the MCP
// clients are closed.
type ToolRAGObserver func(ToolRAGObservation)

var (
	toolRAGObserverMu sync.RWMutex
	toolRAGObserverFn ToolRAGObserver
)

// SetToolRAGObserver installs (or, with nil, removes) the observer. Without one,
// nothing is scored, counted or recorded.
func SetToolRAGObserver(f ToolRAGObserver) {
	toolRAGObserverMu.Lock()
	toolRAGObserverFn = f
	toolRAGObserverMu.Unlock()
}

func currentToolRAGObserver() ToolRAGObserver {
	toolRAGObserverMu.RLock()
	defer toolRAGObserverMu.RUnlock()
	return toolRAGObserverFn
}

// Test seams: the scorer (nil = bm25Score), the problem log, and the once-per-process
// flags for the mode warning and the mode line.
var (
	toolRAGScorer     ToolScorer
	toolRAGProblems   = NewToolRAGLog()
	toolRAGWarned     atomic.Bool
	toolRAGModeLogged atomic.Bool
)

// parseToolRAGMode maps the (trimmed, lowercased) variable to a mode and, for values
// that are not plainly observe or off, which warning applies: "filter" for upstream's
// enabling values and "filter" itself (this build only observes), "unknown" for the rest.
func parseToolRAGMode(raw string) (mode, note string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", toolRAGModeObserve:
		return toolRAGModeObserve, ""
	case toolRAGModeOff, "0", "false", "no":
		return toolRAGModeOff, ""
	case "1", "true", "on", "yes", "filter":
		return toolRAGModeObserve, "filter"
	}
	return toolRAGModeObserve, "unknown"
}

// toolRAGMode reads ARTEX_TOOL_RAG on every assembly. Values that do not mean
// observe or off earn one warning per process; they still observe.
func toolRAGMode() string {
	raw := os.Getenv(toolRAGEnv)
	mode, note := parseToolRAGMode(raw)
	if note != "" && toolRAGWarned.CompareAndSwap(false, true) {
		if note == "filter" {
			log.Printf("[tool-rag] ARTEX_TOOL_RAG=%s requests filtering, which this build does not implement; observing only", strings.ToLower(strings.TrimSpace(raw)))
		} else {
			log.Printf("[tool-rag] ARTEX_TOOL_RAG has an unrecognised value (not shown); accepted values: observe, off, 0, false, no, 1, true, on, yes, filter; observing only")
		}
	}
	return mode
}

// LogToolRAGMode logs the effective mode once per process (and the mode warning, if
// one applies). The server calls it when it wires the observer.
func LogToolRAGMode() {
	mode := toolRAGMode()
	if !toolRAGModeLogged.CompareAndSwap(false, true) {
		return
	}
	if mode == toolRAGModeOff {
		log.Printf("[tool-rag] mode=off")
		return
	}
	log.Printf("[tool-rag] mode=%s scorer=%s k=%d", mode, toolRAGScorerName, toolRAGTopK())
}

// ToolRAGLog counts observation problems by kind and logs each kind at most once a
// minute. A line carries the kind, the running count and a content-free detail (an
// SQLSTATE or a Go type), never text from an error, a query or a tool.
type ToolRAGLog struct {
	mu    sync.Mutex
	now   func() time.Time
	kinds map[string]*toolRAGKind
}

type toolRAGKind struct {
	n    int
	last time.Time
}

func NewToolRAGLog() *ToolRAGLog {
	return &ToolRAGLog{now: time.Now, kinds: map[string]*toolRAGKind{}}
}

// Report counts one problem of the given kind and logs it unless the kind was logged
// within the last minute. detail may be empty.
func (l *ToolRAGLog) Report(kind, detail string) {
	l.mu.Lock()
	k := l.kinds[kind]
	if k == nil {
		k = &toolRAGKind{}
		l.kinds[kind] = k
	}
	k.n++
	n, now := k.n, l.now()
	emit := n == 1 || now.Sub(k.last) >= time.Minute
	if emit {
		k.last = now
	}
	l.mu.Unlock()
	if !emit {
		return
	}
	if detail != "" {
		detail = " (" + detail + ")"
	}
	log.Printf("[tool-rag] observation %s; %d since start%s", kind, n, detail)
}

// Count returns how many problems of kind were reported, logged or not.
func (l *ToolRAGLog) Count(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if k := l.kinds[kind]; k != nil {
		return k.n
	}
	return 0
}

// toolRAGRun is one planner round or worker run that is being observed.
type toolRAGRun struct {
	obs      *toolObservation
	observer ToolRAGObserver
	ri       RunInfo
	once     sync.Once
}

// startToolRAG scores the resolved tool list for a planner round or worker run and
// returns the run's observation state, or nil when nothing is to be observed: other
// agents, mode off, no observer, or no task id (the database requires one). Whatever
// goes wrong inside is counted and logged; it never reaches the caller.
func startToolRAG(ctx context.Context, agentKey string, tools []actool.CoreTool, def DeferredInfo) (run *toolRAGRun) {
	if agentKey != "planner" && agentKey != "worker" || toolRAGMode() != toolRAGModeObserve {
		return nil
	}
	observer := currentToolRAGObserver()
	ri := RunInfoFrom(ctx)
	if observer == nil || ri.TaskID <= 0 {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			toolRAGProblems.Report("observer panic", fmt.Sprintf("type %T", r))
			run = nil
		}
	}()
	obs := observeToolRAG(ctx, agentKey, tools, def, toolRAGScorer)
	switch obs.Outcome {
	case toolRAGScorerError:
		toolRAGProblems.Report("scorer error", "type "+obs.FailType)
	case toolRAGObserverPanic:
		toolRAGProblems.Report("observer panic", "type "+obs.FailType)
	}
	return &toolRAGRun{obs: obs, observer: observer, ri: ri}
}

// wrapCleanup returns cleanup extended by the flush. The original cleanup (it closes
// the MCP clients) is deferred, so it runs even if the flush panics or its goroutine
// exits.
func (r *toolRAGRun) wrapCleanup(orig func()) func() {
	return func() {
		defer orig()
		r.flush()
	}
}

// flush seals the call counter and hands one row to the observer, once. It recovers
// its own panics: a failed observation is counted and logged, never a failed run.
func (r *toolRAGRun) flush() {
	r.once.Do(func() {
		defer func() {
			if p := recover(); p != nil {
				toolRAGProblems.Report("flush panic", fmt.Sprintf("type %T", p))
			}
		}()
		r.observer(r.obs.row(r.ri))
	})
}

// countCall adds one call of the tool called raw. Names pass the name rule, at most
// toolRAGMaxNames distinct names are kept and the rest count under "_overflow". Calls
// after the flush are ignored.
func (o *toolObservation) countCall(raw string) {
	name := toolObsName(raw)
	class := toolClassOther
	if name != toolRAGUnknownName {
		if c, ok := o.Classes[raw]; ok {
			class = c
		}
	}
	o.callMu.Lock()
	defer o.callMu.Unlock()
	if o.sealed {
		return
	}
	e, seen := o.called[name]
	if !seen && len(o.called) >= toolRAGMaxNames {
		name, class = toolRAGOverflowName, toolClassOther
		e = o.called[name]
	}
	if o.called == nil {
		o.called = map[string]db.ToolRAGCall{}
	}
	e.N++
	if e.Class == "" {
		e.Class = class
	}
	o.called[name] = e
}

// row seals the counter and returns the observation as stored: identity from the run,
// counts and names from the assembly, calls from the counter.
func (o *toolObservation) row(ri RunInfo) ToolRAGObservation {
	o.callMu.Lock()
	o.sealed = true
	called := o.called
	o.callMu.Unlock()
	if called == nil {
		called = map[string]db.ToolRAGCall{}
	}
	return ToolRAGObservation{
		TaskID: ri.TaskID, ExplorationID: ri.ExplorationID, IntentID: ri.IntentID,
		AgentKey: o.AgentKey, Scorer: o.Scorer, Mode: o.Mode, Outcome: o.Outcome, K: o.K,
		HostListCount: o.HostListCount, KeepCount: o.KeepCount, CandidateCount: o.CandidateCount,
		NonzeroCount: o.NonzeroCount, WouldHideCount: o.WouldHideCount,
		DeferredCount: o.DeferredCount, LockedAtStart: o.LockedAtStart, UnlockedDuringRun: o.UnlockedDuringRun(),
		QueryTokens: o.QueryTokens, QueryTruncated: o.QueryTruncated,
		DurationUS: int(min(o.DurationUS, 1<<31-1)),
		Ranking:    append([]string{}, o.Ranking...), Called: called,
	}
}

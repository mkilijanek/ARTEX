package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
)

// This file is the one place an agent session is built. newSession and runOnce run after the
// last rewrite of the options (noa, the WebFetch policy, side-question capture), so the checks
// see the list the SDK will use. The architecture test (session_guard_arch_test.go) fails the
// build when another file calls agentcore.NewSession or agentcore.Run.

// errNilToolList is returned for options without a tool list. The SDK turns a nil list into its
// default tools (a shell and the file tools), so a session must always name its tools; an empty
// but non-nil list means "no tools".
var errNilToolList = errors.New("session options carry no tool list: every session names its tools explicitly")

// SessionInfo identifies a session in the isolation warnings and to the PostToolUse hooks.
type SessionInfo struct {
	Kind string // worker, chat, main, run
	ID   string
	// Emit receives the warning record the guard adds to the session's activity trace when
	// isolation is not enforced. It may be nil.
	Emit func(db.Activity)
	// NoaDir is the session's noa archive directory (<data>/noa/<noa session>), "" when the
	// session has none. A tool result that names a file in it makes the file readable.
	NoaDir string

	roots *outputRoots // set by newSession
}

// newSession builds a session. It refuses a nil tool list and any option set that would hand the
// session a tool the policy layer has not seen; it takes a lease on tool processes for the life
// of the session; it installs the PostToolUse hook registry; and when isolation is not enforced
// it says so in the log and in the session's activity trace. The returned function closes the
// session and releases the lease; call it exactly once, normally with defer.
func newSession(ctx context.Context, opts agentcore.Options, info SessionInfo) (*agentcore.Session, func(), error) {
	if opts.Tools == nil {
		return nil, nil, errNilToolList
	}
	if opts.EnableWebFetch {
		// attachWebFetchPolicy clears this flag after substituting the policy-checked tool; still
		// set here means the SDK's own WebFetch (no deny-list) would run.
		return nil, nil, errors.New("session options enable the SDK's WebFetch, which bypasses the egress policy")
	}
	iso := sandbox.Current()
	if iso.On() {
		// The spill directory must be the session's own, made by the server and out of the tool
		// user's reach; and every file tool must run in the helper.
		if err := checkSpillDir(opts.ToolOutputDir, info); err != nil {
			return nil, nil, err
		}
		opts.Tools = isolateSessionTools(opts.Tools)
	}
	lease, err := iso.Leases().Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("take a tool-process lease: %w", err)
	}
	info.roots = &outputRoots{spill: opts.ToolOutputDir, noa: info.NoaDir}
	opts.Hooks = newGuardHooks(opts.Hooks, info)
	warnSession(iso, info)
	s := agentcore.NewSession(opts)
	info.roots.setSessionID(s.SessionID())
	var once sync.Once
	return s, func() {
		once.Do(func() {
			s.Close()
			lease.Release()
		})
	}, nil
}

// warnSession stamps a session that does not run fully isolated.
func warnSession(iso *sandbox.Isolation, info SessionInfo) {
	w := iso.SessionWarning()
	if w == "" {
		return
	}
	log.Printf("[isolation] WARNING session %s/%s: %s", info.Kind, info.ID, w)
	if info.Emit != nil {
		info.Emit(db.Activity{Worker: "system", Kind: "text", IsError: true, Summary: w, Detail: w})
	}
}

// runOnce runs a single prompt with no tools at all: the provider connection test. The options
// must carry an empty, non-nil tool list, disable the SDK's background tools (which include
// Monitor, a shell) and enable neither WebFetch nor WebSearch, so a provider's tool call finds
// nothing to run. These rules hold in every isolation mode.
func runOnce(ctx context.Context, opts agentcore.Options, input string) (string, error) {
	switch {
	case opts.Tools == nil:
		return "", errNilToolList
	case len(opts.Tools) != 0 || len(opts.DeferredTools) != 0:
		return "", errors.New("runOnce takes no tools")
	case !opts.DisableBackgroundTasks:
		return "", errors.New("runOnce needs the SDK's background tools disabled")
	case opts.EnableWebFetch || opts.EnableWebSearch:
		return "", errors.New("runOnce takes no web tools")
	}
	lease, err := sandbox.Current().Leases().Acquire(ctx)
	if err != nil {
		return "", fmt.Errorf("take a tool-process lease: %w", err)
	}
	defer lease.Release()
	return agentcore.Run(ctx, opts, input)
}

// PostToolUseFunc runs after every tool call of every guarded session, with the final result
// (after the SDK's output cap). It must not block for long and must not follow links in any
// tree a tool can write.
type PostToolUseFunc func(ctx context.Context, s SessionInfo, tool string, input, result []byte, isErr bool)

var postToolUse = struct {
	sync.RWMutex
	hooks []postHook
}{}

type postHook struct {
	name string
	fn   PostToolUseFunc
}

// registerPostToolUse adds a hook to the registry the guard installs on every session. The
// returned function removes it again (tests use it). The spill, background-output and noa
// readability hook registers here.
func registerPostToolUse(name string, fn PostToolUseFunc) (remove func()) {
	postToolUse.Lock()
	defer postToolUse.Unlock()
	h := postHook{name: name, fn: fn}
	postToolUse.hooks = append(postToolUse.hooks, h)
	return func() {
		postToolUse.Lock()
		defer postToolUse.Unlock()
		for i := range postToolUse.hooks {
			if postToolUse.hooks[i].name == name {
				postToolUse.hooks = append(postToolUse.hooks[:i:i], postToolUse.hooks[i+1:]...)
				return
			}
		}
	}
}

// guardHooks composes the session's own hooks (workers and chats have a registry; the planner,
// main agent, goals and the connection test have none) with the PostToolUse registry.
type guardHooks struct {
	inner harness.HookRunner
	info  SessionInfo
}

func newGuardHooks(inner harness.HookRunner, info SessionInfo) *guardHooks {
	return &guardHooks{inner: inner, info: info}
}

func (h *guardHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if h.inner == nil {
		return false, "", nil
	}
	return h.inner.PreToolUse(ctx, name, input)
}

func (h *guardHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.inner != nil {
		h.inner.PostToolUse(ctx, name, input, result, isErr)
	}
	postToolUse.RLock()
	hooks := append([]postHook(nil), postToolUse.hooks...)
	postToolUse.RUnlock()
	for _, p := range hooks {
		runPostHook(ctx, p, h.info, name, input, result, isErr)
	}
}

// runPostHook isolates a failing hook: a panic is logged and the session goes on.
func runPostHook(ctx context.Context, p postHook, info SessionInfo, name string, input, result []byte, isErr bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[session-guard] post-tool hook %s panicked: %v", p.name, r)
		}
	}()
	p.fn(ctx, info, name, input, result, isErr)
}

func (h *guardHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.inner == nil {
		return false, nil, ""
	}
	return h.inner.Stop(ctx, messages)
}

// FireSessionStart and FireUserPromptSubmit keep the SDK's optional host-fired hooks working
// when the session's own runner has them.
func (h *guardHooks) FireSessionStart(ctx context.Context) {
	if f, ok := h.inner.(interface{ FireSessionStart(context.Context) }); ok {
		f.FireSessionStart(ctx)
	}
}

func (h *guardHooks) FireUserPromptSubmit(ctx context.Context, prompt string) string {
	if f, ok := h.inner.(interface {
		FireUserPromptSubmit(context.Context, string) string
	}); ok {
		return f.FireUserPromptSubmit(ctx, prompt)
	}
	return ""
}

// spillMaxAge is how old a spill directory must be (by the time of its last change) before the
// sweep removes it. Spill directories are kept after their session ends: a resumed chat, main agent
// or planner round still holds pointers to files an earlier session spilled.
const spillMaxAge = 7 * 24 * time.Hour

// SweepSpill removes the session spill directories under <dataDir>/spill that have not changed for
// spillMaxAge. The server calls it once at start, before any session runs, and logs the count.
func SweepSpill(dataDir string) {
	root := filepath.Join(dataDir, "spill")
	n, err := sweepSpill(root, spillMaxAge, time.Now())
	if err != nil {
		log.Printf("[spill] sweep: %v", err)
	}
	log.Printf("[spill] removed %d spill directories older than %s", n, spillMaxAge)
}

// sweepSpill removes the session directories under <spillRoot>/<kind>/ whose last change is older
// than maxAge and returns how many it removed. It looks at nothing outside the spill root and
// follows no link (an entry that is not a directory is left alone). Kind directories stay.
func sweepSpill(spillRoot string, maxAge time.Duration, now time.Time) (int, error) {
	root, err := os.OpenRoot(spillRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer root.Close()
	kinds, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, k := range kinds {
		if !validSpillName(k.Name()) {
			continue
		}
		kfi, err := root.Lstat(k.Name())
		if err != nil || !kfi.IsDir() {
			continue
		}
		ids, err := fs.ReadDir(root.FS(), k.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range ids {
			rel := filepath.Join(k.Name(), id.Name())
			fi, err := root.Lstat(rel)
			if err != nil || !fi.IsDir() || now.Sub(fi.ModTime()) < maxAge {
				continue
			}
			if err := root.RemoveAll(rel); err != nil {
				errs = append(errs, err)
				continue
			}
			removed++
		}
	}
	return removed, errors.Join(errs...)
}

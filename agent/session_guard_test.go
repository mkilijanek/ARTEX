package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/sandbox"
)

// noTools is the explicit empty tool list: a session without tools. Sessions never default to the
// SDK's tools, so tests that need none say so.
var noTools = []tool.CoreTool{}

// scriptedProvider answers its first call with the given tool uses and every later call with
// "done". It needs no network.
type scriptedProvider struct {
	captureUsageProvider
	mu    sync.Mutex
	calls int
	uses  []llm.ContentBlock
}

func (p *scriptedProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 && len(p.uses) > 0 {
		return llm.Message{Role: llm.RoleAssistant, Content: p.uses}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
}

func toolUse(id, name string, input any) llm.ContentBlock {
	b, _ := json.Marshal(input)
	return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: name, Input: b}
}

// recordTool is a tool that records that it ran.
func recordTool(name string, ran *int) tool.CoreTool {
	return tool.Build(tool.Spec{Name: name, Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			*ran++
			return tool.Text("ran " + name), nil
		}})
}

// --- the tool list ---

func TestNewSessionRefusesANilToolList(t *testing.T) {
	_, closeFn, err := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}}, SessionInfo{Kind: "test"})
	if !errors.Is(err, errNilToolList) || closeFn != nil {
		t.Fatalf("got err=%v close=%v, want errNilToolList", err, closeFn != nil)
	}
	// an explicit empty list is a session without tools, and is accepted
	s, closeFn, err := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}}, SessionInfo{Kind: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("no session")
	}
	closeFn()
}

// A refused session leaves no lease behind.
func TestRefusedSessionTakesNoLease(t *testing.T) {
	iso := activateForTest(t, goodPlatform())
	_, _, _ = newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}}, SessionInfo{Kind: "test"})
	if iso.Leases().Held() != 0 {
		t.Fatalf("held %d", iso.Leases().Held())
	}
}

// The SDK's own WebFetch has no deny-list; if the policy rewrite did not clear the flag, the
// guard refuses.
func TestNewSessionRefusesTheSDKWebFetch(t *testing.T) {
	_, _, err := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}, EnableWebFetch: true}, SessionInfo{Kind: "test"})
	if err == nil || !strings.Contains(err.Error(), "WebFetch") {
		t.Fatalf("err %v", err)
	}
}

// AC27: the WebFetch policy no longer turns a nil list into the SDK's default tools.
func TestAttachWebFetchPolicyRefusesANilList(t *testing.T) {
	opts := agentcore.Options{EnableWebFetch: true}
	if err := attachWebFetchPolicy(&opts); !errors.Is(err, errNilToolList) {
		t.Fatalf("err %v", err)
	}
	if opts.Tools != nil || !opts.EnableWebFetch {
		t.Fatalf("the options were rewritten: tools=%v web=%v", opts.Tools, opts.EnableWebFetch)
	}
	opts = agentcore.Options{EnableWebFetch: true, Tools: []tool.CoreTool{recordTool("Mine", new(int))}}
	if err := attachWebFetchPolicy(&opts); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range opts.Tools {
		names = append(names, tl.Name())
	}
	if strings.Join(names, ",") != "Mine,WebFetch" || opts.EnableWebFetch {
		t.Fatalf("tools %v web=%v: the list must be the caller's plus the policy WebFetch, nothing from the defaults", names, opts.EnableWebFetch)
	}
	// without WebFetch the options are not touched at all
	opts = agentcore.Options{}
	if err := attachWebFetchPolicy(&opts); err != nil || opts.Tools != nil {
		t.Fatalf("err %v tools %v", err, opts.Tools)
	}
}

// The side-capture and metrics rewrites pass the refusal on instead of swallowing it.
func TestAttachModelMetricsPassesTheRefusalOn(t *testing.T) {
	opts := agentcore.Options{Provider: &scriptedProvider{}, EnableWebFetch: true}
	if err := attachModelMetrics(&opts); !errors.Is(err, errNilToolList) {
		t.Fatalf("err %v", err)
	}
	if _, err := attachSideCapture(t.Context(), &opts); !errors.Is(err, errNilToolList) {
		t.Fatalf("err %v", err)
	}
	if _, _, err := captureRun(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, EnableWebFetch: true}, "x", nil); !errors.Is(err, errNilToolList) {
		t.Fatalf("captureRun: err %v", err)
	}
	if _, _, err := captureRun(t.Context(), agentcore.Options{Provider: &scriptedProvider{}}, "x", nil); !errors.Is(err, errNilToolList) {
		t.Fatalf("captureRun without web: err %v", err)
	}
}

// AC27: a session built with noa keeps its full tool list: noa appends its compress tool, and
// the guard's check runs after that rewrite. Every caller passes an explicit list, so noa never
// has to create one.
func TestNoaKeepsTheToolList(t *testing.T) {
	dir := t.TempDir()
	opts := agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{recordTool("A", new(int)), recordTool("B", new(int))}}
	enableNoa(&opts, func() bool { return true }, dir, "s1", func(m string) { t.Log(m) })
	names := map[string]bool{}
	for _, tl := range opts.Tools {
		names[tl.Name()] = true
	}
	if !names["A"] || !names["B"] || len(opts.Tools) < 3 {
		t.Fatalf("noa dropped or replaced the list: %v", names)
	}
	_, closeFn, err := newSession(t.Context(), opts, SessionInfo{Kind: "test"})
	if err != nil {
		t.Fatal(err)
	}
	closeFn()
	fi, err := os.Stat(filepath.Join(dir, "noa", "s1"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("noa session directory: %v %v, want 0755 (tools read the archive)", fi, err)
	}
}

// --- the connection test ---

// AC27 / K1: a provider that answers the connection test with tool calls starts no process.
func TestConnectionTestCannotRunATool(t *testing.T) {
	dir := t.TempDir()
	k1, k1m := filepath.Join(dir, "k1"), filepath.Join(dir, "k1m")
	p := &scriptedProvider{uses: []llm.ContentBlock{
		toolUse("c1", "Bash", map[string]any{"command": "touch " + k1}),
		toolUse("c2", "Monitor", map[string]any{"command": "touch " + k1m}),
		toolUse("c3", "Read", map[string]any{"file_path": "/proc/1/environ"}),
	}}
	opts := connectionTestOptions(p, false)
	if _, err := runOnce(t.Context(), opts, "ping"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{k1, k1m} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("the provider's tool call ran: %s exists", f)
		}
	}
	// the options themselves: no tools, no background tools, no web
	if opts.Tools == nil || len(opts.Tools) != 0 || !opts.DisableBackgroundTasks || opts.EnableWebFetch || opts.EnableWebSearch {
		t.Fatalf("options %+v", opts)
	}
}

// The control for the test above: the same provider against the options the connection test had
// before (no tool list, background tools on) does run the shell. This is what makes the test
// above able to fail.
func TestOldConnectionTestOptionsDidRunTools(t *testing.T) {
	dir := t.TempDir()
	k1 := filepath.Join(dir, "k1")
	p := &scriptedProvider{uses: []llm.ContentBlock{toolUse("c1", "Bash", map[string]any{"command": "touch " + k1})}}
	if _, err := agentcore.Run(t.Context(), agentcore.Options{Provider: p, PermissionMode: permission.ModeBypass, MaxTurns: 1, NonStreaming: true, WorkingDir: dir}, "ping"); err != nil {
		t.Log(err)
	}
	if _, err := os.Stat(k1); err != nil {
		t.Skipf("the control did not run the tool here (%v); the assertion above is then vacuous on this host", err)
	}
}

func TestRunOnceEnforcesItsRules(t *testing.T) {
	good := connectionTestOptions(&scriptedProvider{}, false)
	for name, mut := range map[string]func(*agentcore.Options){
		"nil list":            func(o *agentcore.Options) { o.Tools = nil },
		"a tool":              func(o *agentcore.Options) { o.Tools = []tool.CoreTool{recordTool("X", new(int))} },
		"background tools on": func(o *agentcore.Options) { o.DisableBackgroundTasks = false },
		"SDK WebFetch":        func(o *agentcore.Options) { o.EnableWebFetch = true },
		"web search":          func(o *agentcore.Options) { o.EnableWebSearch = true },
		"deferred tools":      func(o *agentcore.Options) { o.DeferredTools = []string{"D"} },
	} {
		o := good
		mut(&o)
		if _, err := runOnce(t.Context(), o, "ping"); err == nil {
			t.Errorf("%s: runOnce accepted the options", name)
		}
	}
	if reply, err := runOnce(t.Context(), good, "ping"); err != nil || reply != "done" {
		t.Fatalf("reply %q err %v", reply, err)
	}
}

// --- hooks ---

type recordingHooks struct {
	mu   sync.Mutex
	pre  []string
	post []string
	fire int
}

func (h *recordingHooks) PreToolUse(_ context.Context, n string, _ []byte) (bool, string, []byte) {
	h.mu.Lock()
	h.pre = append(h.pre, n)
	h.mu.Unlock()
	return false, "", nil
}
func (h *recordingHooks) PostToolUse(_ context.Context, n string, _, _ []byte, _ bool) {
	h.mu.Lock()
	h.post = append(h.post, n)
	h.mu.Unlock()
}
func (h *recordingHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}
func (h *recordingHooks) FireSessionStart(context.Context) { h.mu.Lock(); h.fire++; h.mu.Unlock() }
func (h *recordingHooks) FireUserPromptSubmit(context.Context, string) string {
	h.mu.Lock()
	h.fire++
	h.mu.Unlock()
	return ""
}

func runProbeSession(t *testing.T, inner *recordingHooks, info SessionInfo) (ran int) {
	t.Helper()
	p := &scriptedProvider{uses: []llm.ContentBlock{toolUse("u1", "Probe", map[string]any{})}}
	opts := agentcore.Options{Provider: p, Tools: []tool.CoreTool{recordTool("Probe", &ran)}, NonStreaming: true, MaxTurns: 3, PermissionMode: permission.ModeBypass}
	if inner != nil {
		opts.Hooks = inner
	}
	s, closeFn, err := newSession(t.Context(), opts, info)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	for _, err := range s.Prompt(t.Context(), "go") {
		if err != nil {
			t.Fatal(err)
		}
	}
	return ran
}

// The guard installs the PostToolUse registry on every session, composing with the session's own
// hooks, and keeps the host-fired hooks working.
func TestGuardComposesHooksWithTheRegistry(t *testing.T) {
	var mu sync.Mutex
	var got []string
	remove := registerPostToolUse("test", func(_ context.Context, s SessionInfo, tl string, _, result []byte, isErr bool) {
		mu.Lock()
		got = append(got, s.Kind+"/"+s.ID+" "+tl+" "+string(result))
		mu.Unlock()
	})
	defer remove()

	inner := &recordingHooks{}
	if ran := runProbeSession(t, inner, SessionInfo{Kind: "chat", ID: "c1"}); ran != 1 {
		t.Fatalf("the tool ran %d times", ran)
	}
	if len(inner.pre) != 1 || len(inner.post) != 1 || inner.fire == 0 {
		t.Errorf("the session's own hooks were not kept: pre=%v post=%v fire=%d", inner.pre, inner.post, inner.fire)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "chat/c1 Probe ") || !strings.Contains(got[0], "ran Probe") {
		t.Errorf("registry hook calls %v", got)
	}

	// a session that has no hooks of its own (planner, main agent, goals) gets the registry too
	got = nil
	runProbeSession(t, nil, SessionInfo{Kind: "run", ID: "exp1-planner"})
	if len(got) != 1 || !strings.HasPrefix(got[0], "run/exp1-planner Probe ") {
		t.Errorf("a session without hooks got %v", got)
	}
}

// A failing hook does not take the session down, and removing it stops the calls.
func TestPostToolUseHookPanicIsContainedAndRemovable(t *testing.T) {
	calls := 0
	remove := registerPostToolUse("boom", func(context.Context, SessionInfo, string, []byte, []byte, bool) {
		calls++
		panic("hook failure")
	})
	if ran := runProbeSession(t, nil, SessionInfo{Kind: "run", ID: "x"}); ran != 1 || calls != 1 {
		t.Fatalf("ran %d calls %d", ran, calls)
	}
	remove()
	runProbeSession(t, nil, SessionInfo{Kind: "run", ID: "x"})
	if calls != 1 {
		t.Fatalf("the removed hook was called again (%d)", calls)
	}
}

// --- isolation status in the session ---

// fakePlatform satisfies the preconditions (or not) without a real host.
type fakePlatform struct {
	euid int
	env  string
	l    sandbox.Layout
}

func goodPlatform() *fakePlatform { return &fakePlatform{} }

func (p *fakePlatform) EUID() int { return p.euid }
func (p *fakePlatform) StatusFile() ([]byte, error) {
	return []byte("CapEff:\t00000000a80425fb\n"), nil
}
func (p *fakePlatform) Stat(path string) (fs.FileMode, uint32, error) {
	if path == p.l.ShimDir {
		return fs.ModeDir | 0o755, 0, nil
	}
	return 0o755, 0, nil
}
func (p *fakePlatform) IsStatic(string) (bool, error) { return true, nil }
func (p *fakePlatform) Getenv(string) string          { return p.env }

type noKill struct{}

func (noKill) KillAll(context.Context) error     { return nil }
func (noKill) Left(context.Context) (int, error) { return 0, nil }

// activateForTest installs an isolation built from fakes, for the life of the test.
func activateForTest(t *testing.T, pf *fakePlatform) *sandbox.Isolation {
	t.Helper()
	root := t.TempDir()
	l := sandbox.DefaultLayout()
	l.Artex = filepath.Join(root, "app", "artex")
	l.ShimDir = filepath.Join(root, "shims")
	l.PrivateDir = filepath.Join(root, "run", "private")
	l.VarsDir = filepath.Join(l.PrivateDir, "vars")
	l.ServerTmp = filepath.Join(root, "run", "tmp")
	l.ProcRoot = filepath.Join(root, "proc")
	pf.l = l
	data := filepath.Join(root, "app", "data")
	for _, d := range []string{filepath.Dir(l.Artex), data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("TMPDIR", os.Getenv("TMPDIR"))
	iso, err := sandbox.Activate(t.Context(), sandbox.Config{
		Layout: l, DataDir: data, KeyDir: filepath.Dir(l.Artex), Probe: pf, Killer: noKill{},
		Runner: func(context.Context, string, string) (string, error) { return "", errors.New("no shell in this test") },
		Logf:   func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox.Set(iso)
	t.Cleanup(func() { sandbox.Set(nil) })
	return iso
}

// A session holds a lease for its life and gives it back when closed, once.
func TestSessionHoldsALeaseUntilClosed(t *testing.T) {
	iso := activateForTest(t, &fakePlatform{euid: 1000})
	_, closeFn, err := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}}, SessionInfo{Kind: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if iso.Leases().Held() != 1 {
		t.Fatalf("held %d during the session", iso.Leases().Held())
	}
	closeFn()
	closeFn()
	if iso.Leases().Held() != 0 {
		t.Fatalf("held %d after close", iso.Leases().Held())
	}
}

// AC22(b): when the platform cannot drop, the session starts and says so, in the activity trace
// and in the log, once.
func TestSessionStampsAnUnavailableIsolation(t *testing.T) {
	activateForTest(t, &fakePlatform{euid: 1000})
	logs := captureLog(t)
	var rec []db.Activity
	_, closeFn, err := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}},
		SessionInfo{Kind: "chat", ID: "conv-9", Emit: func(a db.Activity) { rec = append(rec, a) }})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if len(rec) != 1 || !rec[0].IsError || !strings.HasPrefix(rec[0].Summary, "tool isolation unavailable: tools run as the server user (") {
		t.Fatalf("trace records %+v", rec)
	}
	if !strings.Contains(logs.String(), "[isolation] WARNING session chat/conv-9") {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestSessionStampsDisabledAndStaysQuietWhenEnforced(t *testing.T) {
	activateForTest(t, &fakePlatform{env: "off"})
	var rec []db.Activity
	emit := func(a db.Activity) { rec = append(rec, a) }
	_, c1, err := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}}, SessionInfo{Kind: "run", ID: "x", Emit: emit})
	if err != nil {
		t.Fatal(err)
	}
	c1()
	if len(rec) != 1 || !strings.Contains(rec[0].Summary, "disabled") {
		t.Fatalf("records %+v", rec)
	}

	// the same session under an inactive isolation (never set up) is not stamped, and one whose
	// self-check failed with no shell is degraded, which is stamped too
	sandbox.Set(nil)
	rec = nil
	_, c2, _ := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}}, SessionInfo{Kind: "run", ID: "x", Emit: emit})
	c2()
	if len(rec) != 0 {
		t.Fatalf("an inactive isolation stamped a session: %+v", rec)
	}
	iso := activateForTest(t, goodPlatform())
	if iso.Status() != sandbox.Degraded {
		t.Fatalf("status %s", iso.Status())
	}
	_, c3, _ := newSession(t.Context(), agentcore.Options{Provider: &scriptedProvider{}, Tools: []tool.CoreTool{}}, SessionInfo{Kind: "run", ID: "x", Emit: emit})
	c3()
	if len(rec) != 1 || !strings.HasPrefix(rec[0].Summary, "tool isolation degraded: ") {
		t.Fatalf("records %+v", rec)
	}
}

// --- leases outside sessions ---

// AugmentTools spawns MCP servers long before the session exists, so it takes the lease: held
// from before the hook runs until the cleanup it returns has closed the clients.
func TestAugmentToolsHoldsALeaseUntilItsCleanupHasRun(t *testing.T) {
	iso := activateForTest(t, goodPlatform())
	old := ToolAugment()
	defer SetToolAugment(old)
	heldInHook, heldInCleanup := -1, -1
	SetToolAugment(func(context.Context, string) ([]tool.CoreTool, DeferredInfo, func()) {
		heldInHook = iso.Leases().Held()
		return nil, DeferredInfo{}, func() { heldInCleanup = iso.Leases().Held() }
	})
	_, _, cleanup := AugmentTools(t.Context(), "assistant", []tool.CoreTool{})
	if iso.Leases().Held() != 1 {
		t.Fatalf("held %d after AugmentTools", iso.Leases().Held())
	}
	cleanup()
	if heldInHook != 1 || heldInCleanup != 1 || iso.Leases().Held() != 0 {
		t.Fatalf("held in hook %d, in cleanup %d, after %d", heldInHook, heldInCleanup, iso.Leases().Held())
	}
}

// A cancelled context cannot take a lease, and then nothing is spawned: the hook is not called.
func TestAugmentToolsSpawnsNothingWithoutALease(t *testing.T) {
	activateForTest(t, goodPlatform())
	old := ToolAugment()
	defer SetToolAugment(old)
	called := false
	SetToolAugment(func(context.Context, string) ([]tool.CoreTool, DeferredInfo, func()) {
		called = true
		return nil, DeferredInfo{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	base := []tool.CoreTool{recordTool("A", new(int))}
	out, _, cleanup := AugmentTools(ctx, "assistant", base)
	cleanup()
	if called {
		t.Fatal("the augment hook ran without a lease")
	}
	if len(out) != 1 {
		t.Fatalf("tools %v", out)
	}
}

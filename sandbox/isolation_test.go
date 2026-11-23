//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// actEnv is a temporary tree plus fakes for everything Activate touches.
type actEnv struct {
	cfg    Config
	probe  *fakeProbe
	killer *fakeKiller
	steps  []string
	runs   int
	out    string
	runErr error
	logs   []string
	mu     sync.Mutex
}

func newActEnv(t *testing.T) *actEnv {
	t.Helper()
	p := newPermTree(t)
	l := p.l
	l.ShimDir = filepath.Join(filepath.Dir(p.app), "shims")
	l.ProcRoot = filepath.Join(filepath.Dir(p.app), "proc")
	if err := os.MkdirAll(filepath.Join(l.ProcRoot, "sys", "fs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"protected_hardlinks", "protected_symlinks"} {
		if err := os.WriteFile(filepath.Join(l.ProcRoot, "sys", "fs", name), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &actEnv{probe: goodProbe(), killer: &fakeKiller{}, out: goodOutput}
	e.probe.l = &l
	e.cfg = Config{
		Layout: l, DataDir: p.data, KeyDir: p.app, ConfigPath: filepath.Join(p.app, "config.json"), SkillDir: filepath.Join(p.app, "skills"),
		SecretNames: []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ARTEX_LLM_PROXY"},
		Probe:       e.probe, Killer: e.killer,
		Runner: func(ctx context.Context, dir, script string) (string, error) {
			e.mu.Lock()
			e.runs++
			e.mu.Unlock()
			e.steps = append(e.steps, "runner")
			return e.out, e.runErr
		},
		Steps: func(s string) { e.steps = append(e.steps, s) },
		Logf:  func(f string, a ...any) { e.mu.Lock(); e.logs = append(e.logs, sprintf(f, a...)); e.mu.Unlock() },
	}
	// secrets are gone, as main leaves them
	for _, k := range e.cfg.SecretNames {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("TMPDIR", "/tmp")
	return e
}

func (e *actEnv) joinedLogs() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.logs, "\n")
}

func indexOf(steps []string, s string) int {
	for i, x := range steps {
		if x == s {
			return i
		}
	}
	return -1
}

// AC30 (order): secrets are gone before the kill, before the check and before any tool
// process can exist; the fixed order of the activation steps holds.
func TestActivateStepOrder(t *testing.T) {
	needRootForOwnership(t)
	e := newActEnv(t)
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"secrets", "preconditions", "kill", "tmpdir", "path", "perms", "selfcheck", "runner", "mode"}
	if strings.Join(e.steps, ",") != strings.Join(want, ",") {
		t.Fatalf("steps %v, want %v", e.steps, want)
	}
	if iso.Status() != Enforced || !iso.On() {
		t.Fatalf("status %s on=%v: %+v", iso.Status(), iso.On(), iso.View())
	}
	if !strings.HasPrefix(os.Getenv("PATH"), e.cfg.Layout.ShimDir+":") {
		t.Errorf("the shim directory is not first on PATH: %s", os.Getenv("PATH"))
	}
	if os.Getenv("TMPDIR") != e.cfg.Layout.ServerTmp {
		t.Errorf("TMPDIR %q", os.Getenv("TMPDIR"))
	}
	if !strings.Contains(e.joinedLogs(), "[isolation] status=enforced tool_uid=30002") {
		t.Errorf("no status line in the log:\n%s", e.joinedLogs())
	}
}

// The cleanup is logged before the status line (AC17 a).
func TestActivateLogsTheCleanupBeforeTheStatus(t *testing.T) {
	e := newActEnv(t)
	if _, err := Activate(t.Context(), e.cfg); err != nil {
		t.Fatal(err)
	}
	logs := e.joinedLogs()
	c := strings.Index(logs, "[isolation] cleanup")
	s := strings.Index(logs, "[isolation] status=")
	if c < 0 || s < 0 || c > s {
		t.Fatalf("cleanup at %d, status at %d:\n%s", c, s, logs)
	}
}

// A secret still in the environment stops the activation before any step runs: nothing can
// start a tool process while a secret is there.
func TestActivateRefusesWhileASecretIsStillSet(t *testing.T) {
	for _, name := range []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "ARTEX_LLM_PROXY"} {
		e := newActEnv(t)
		t.Setenv(name, "x")
		_, err := Activate(t.Context(), e.cfg)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s set: err %v", name, err)
		}
		if strings.Contains(err.Error(), "=x") {
			t.Errorf("the error carries a value")
		}
		if len(e.steps) != 1 || e.steps[0] != "secrets" || e.runs != 0 || e.killer.kills() != 0 {
			t.Errorf("%s set: steps %v runs %d kills %d", name, e.steps, e.runs, e.killer.kills())
		}
	}
}

// An empty variable is not a secret: Compose passes empty strings for unset optional values.
func TestActivateAcceptsAnEmptySecretVariable(t *testing.T) {
	e := newActEnv(t)
	t.Setenv("ARTEX_PG_DSN", "")
	if _, err := Activate(t.Context(), e.cfg); err != nil {
		t.Fatal(err)
	}
}

// P1-P4 failing: unavailable. No tool process is started for the kill or the check, the shim is
// not on PATH, TMPDIR is untouched, and the permission step still ran.
func TestActivateUnavailableWhenThePlatformCannotDrop(t *testing.T) {
	for name, mut := range map[string]func(*fakeProbe){
		"P1": func(p *fakeProbe) { p.euid = 1000 },
		"P2": func(p *fakeProbe) { p.capEff = "0000000000000000" },
		"P3": func(p *fakeProbe) { p.setpriv = nil },
		"P4": func(p *fakeProbe) { p.static = false },
	} {
		e := newActEnv(t)
		mut(e.probe)
		t.Setenv("PATH", e.cfg.Layout.ShimDir+":/usr/bin:/bin") // a leftover shim entry must be removed
		iso, err := Activate(t.Context(), e.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if iso.Status() != Unavailable || iso.On() {
			t.Fatalf("%s: status %s on=%v", name, iso.Status(), iso.On())
		}
		if strings.Contains(os.Getenv("PATH"), e.cfg.Layout.ShimDir) {
			t.Errorf("%s: the shim directory is still on PATH: %s", name, os.Getenv("PATH"))
		}
		if os.Getenv("TMPDIR") != "/tmp" {
			t.Errorf("%s: TMPDIR changed to %s", name, os.Getenv("TMPDIR"))
		}
		if e.runs != 0 || e.killer.kills() != 0 {
			t.Errorf("%s: a tool process was started (runs %d, kills %d)", name, e.runs, e.killer.kills())
		}
		if indexOf(e.steps, "perms") < 0 {
			t.Errorf("%s: the permission step did not run: %v", name, e.steps)
		}
		if !strings.Contains(iso.View().Reason, name) {
			t.Errorf("%s: reason %q", name, iso.View().Reason)
		}
		if !strings.Contains(e.joinedLogs(), "[isolation] WARNING") {
			t.Errorf("%s: no warning block:\n%s", name, e.joinedLogs())
		}
	}
}

// ARTEX_TOOL_ISOLATION=off: disabled, same behaviour.
func TestActivateDisabledByTheOperator(t *testing.T) {
	e := newActEnv(t)
	e.probe.env = "off"
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if iso.Status() != Disabled || iso.On() || e.runs != 0 || strings.Contains(os.Getenv("PATH"), e.cfg.Layout.ShimDir) {
		t.Fatalf("status %s on=%v runs=%d PATH=%s", iso.Status(), iso.On(), e.runs, os.Getenv("PATH"))
	}
	if !strings.Contains(e.joinedLogs(), "[isolation] WARNING") {
		t.Error("no warning block")
	}
}

// AC22(e), (f): a failure a tool can cause keeps isolation. The check shell cannot run (no
// process can be forked, the disk is full, ...), the status is degraded, the shim stays on PATH.
func TestActivateToolCausedFailuresKeepIsolation(t *testing.T) {
	for name, mut := range map[string]func(*actEnv){
		"runner fails":          func(e *actEnv) { e.runErr = errors.New("fork: resource temporarily unavailable") },
		"disk full (C4)":        func(e *actEnv) { e.out = strings.Replace(goodOutput, "c4.write=ok", "c4.write=fail", 1) },
		"root-looking shell":    func(e *actEnv) { e.out = strings.Replace(goodOutput, "uid=30002", "uid=0", 1) },
		"secret reachable (C3)": func(e *actEnv) { e.out = strings.Replace(goodOutput, "c3.key=denied", "c3.key=readable", 1) },
		"leftovers (C10)":       func(e *actEnv) { e.killer.left = []int{4} },
	} {
		e := newActEnv(t)
		mut(e)
		iso, err := Activate(t.Context(), e.cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if iso.Status() != Degraded || !iso.On() {
			t.Errorf("%s: status %s on=%v, want degraded with isolation on", name, iso.Status(), iso.On())
		}
		if !strings.HasPrefix(os.Getenv("PATH"), e.cfg.Layout.ShimDir+":") {
			t.Errorf("%s: the shim left PATH", name)
		}
		if w := iso.SessionWarning(); w == "" || !strings.Contains(w, "degraded") {
			t.Errorf("%s: session warning %q", name, w)
		}
	}
}

// The warnings-only checks leave the status enforced.
func TestActivateWarningsOnlyStaysEnforced(t *testing.T) {
	needRootForOwnership(t)
	e := newActEnv(t)
	e.out = strings.Replace(goodOutput, "c9.meta=closed", "c9.meta=open", 1)
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	v := iso.View()
	if v.Status != Enforced || len(v.Warnings) != 1 {
		t.Fatalf("view %+v, want enforced with the C9 warning", v)
	}
	if iso.SessionWarning() != "" {
		t.Errorf("a warnings-only status must not stamp every session: %q", iso.SessionWarning())
	}
}

// C11: without the kernel's hard-link and symbolic-link protection a tool can lead the server's
// own file operations in shared directories to other files, so the status is degraded (isolation
// stays on), whichever of the two is off or unreadable.
func TestActivateLinkProtectionOffDegrades(t *testing.T) {
	needRootForOwnership(t)
	for name, c := range map[string]struct{ file, value string }{ // value "": the file is missing
		"hard links off":   {"protected_hardlinks", "0\n"},
		"symlinks off":     {"protected_symlinks", "0\n"},
		"hard links unset": {"protected_hardlinks", ""},
		"symlinks unset":   {"protected_symlinks", ""},
	} {
		e := newActEnv(t)
		p := filepath.Join(e.cfg.Layout.ProcRoot, "sys", "fs", c.file)
		var err error
		if c.value == "" {
			err = os.Remove(p)
		} else {
			err = os.WriteFile(p, []byte(c.value), 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		iso, err := Activate(t.Context(), e.cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v := iso.View()
		if v.Status != Degraded || !iso.On() || !strings.Contains(v.Reason, "C11") {
			t.Errorf("%s: view %+v, want degraded by C11 with isolation on", name, v)
		}
	}
	e := newActEnv(t)
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := iso.View(); v.Status != Enforced || len(v.Warnings) != 0 {
		t.Errorf("both protections on: view %+v, want enforced without warnings", v)
	}
}

// C10: leftovers keep being killed in the background and the reason clears when they are gone.
func TestActivateLeftoversClearWhenTheyAreGone(t *testing.T) {
	needRootForOwnership(t)
	e := newActEnv(t)
	e.killer.left = []int{3, 3, 0} // the first two kills leave processes behind
	e.cfg.Lease = LeaseOptions{PollEvery: time.Millisecond, PollWait: 10 * time.Millisecond, RetryEvery: 5 * time.Millisecond}
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if iso.Status() != Degraded || !strings.Contains(iso.View().Reason, "C10") {
		t.Fatalf("view %+v", iso.View())
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go iso.Run(ctx)
	waitFor(t, func() bool { return iso.Status() == Enforced })
	if iso.View().Reason != "" || !iso.On() {
		t.Errorf("view %+v", iso.View())
	}
}

// C10: an idle sweep that fails (the kill or the /proc read) degrades the status like a failed
// start-up sweep, is tried again on the next tick, and the reason clears once a sweep succeeds.
func TestActivateIdleSweepFailureDegradesAndIsRetried(t *testing.T) {
	needRootForOwnership(t)
	three := func(msg string) []error { return []error{errors.New(msg), errors.New(msg), errors.New(msg)} }
	for name, fail := range map[string]func(k *fakeKiller){
		"kill":      func(k *fakeKiller) { k.failNext(three("kill failed"), nil) },
		"proc read": func(k *fakeKiller) { k.failNext(nil, three("no /proc")) },
	} {
		e := newActEnv(t)
		e.cfg.Lease = LeaseOptions{PollEvery: time.Millisecond, PollWait: 10 * time.Millisecond, RetryEvery: 5 * time.Millisecond}
		iso, err := Activate(t.Context(), e.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if iso.Status() != Enforced {
			t.Fatalf("%s: view %+v before the failure", name, iso.View())
		}
		fail(e.killer)
		ctx, cancel := context.WithCancel(t.Context())
		go iso.Run(ctx)
		before := e.killer.kills()
		ls, err := iso.Leases().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ls.Release()
		var sawDegraded bool
		for i := 0; i < 400 && !sawDegraded; i++ {
			if v := iso.View(); v.Status == Degraded && strings.Contains(v.Reason, "C10") {
				sawDegraded = true
			}
			time.Sleep(time.Millisecond)
		}
		if !sawDegraded {
			t.Errorf("%s: a failed idle sweep left the status at %+v", name, iso.View())
		}
		waitFor(t, func() bool { return iso.Status() == Enforced })
		if n := e.killer.kills() - before; n < 3 {
			t.Errorf("%s: %d kills, want the failed sweeps tried again until one succeeded", name, n)
		}
		cancel()
	}
}

// The kill through the drop chain is bounded: a helper that does not end is given up on, and that
// counts as a failed kill, not as the SIGKILL the helper normally ends with.
func TestProcKillerKillAllTimesOut(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "artex")
	// the child keeps the output pipe open after the helper itself is killed
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := killAllTimeout
	killAllTimeout = 200 * time.Millisecond
	t.Cleanup(func() { killAllTimeout = old })
	start := time.Now()
	err := ProcKiller{Layout: Layout{Artex: bin}}.KillAll(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v, want a timeout", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("KillAll took %s", d)
	}
}

// The stored view is a copy: a caller cannot change the status by editing what it got.
func TestViewIsACopy(t *testing.T) {
	e := newActEnv(t)
	e.out = strings.Replace(goodOutput, "c9.meta=closed", "c9.meta=open", 1)
	iso, _ := Activate(t.Context(), e.cfg)
	v := iso.View()
	v.Warnings[0] = "changed"
	if iso.View().Warnings[0] == "changed" {
		t.Fatal("View shares its slice with the state")
	}
}

func TestSessionWarningTexts(t *testing.T) {
	e := newActEnv(t)
	e.probe.euid = 1000
	iso, _ := Activate(t.Context(), e.cfg)
	w := iso.SessionWarning()
	if !strings.HasPrefix(w, "tool isolation unavailable: tools run as the server user (") {
		t.Fatalf("%q", w)
	}
	if (&Isolation{}).SessionWarning() != "" {
		t.Error("an isolation that was never activated must not warn")
	}
}

// --- Command ---

func activated(t *testing.T) (*Isolation, *actEnv) {
	t.Helper()
	e := newActEnv(t)
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return iso, e
}

func TestCommandRequiresALease(t *testing.T) {
	iso, _ := activated(t)
	if _, _, err := iso.Command(t.Context(), nil, Spec{Program: "true"}); err == nil {
		t.Fatal("a spawn without a lease was accepted")
	}
}

func TestCommandBuildsTheDropChainWithoutValuesInArguments(t *testing.T) {
	iso, e := activated(t)
	lease, _ := iso.Leases().Acquire(t.Context())
	defer lease.Release()
	cmd, cleanup, err := iso.Command(t.Context(), lease, Spec{Dir: "/home/artex-tool/mcp/x", Program: "npx", Args: []string{"-y", "pkg"},
		Vars: map[string]string{"FOO": "canary-value"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if cmd.Path != e.cfg.Layout.Artex {
		t.Errorf("path %s", cmd.Path)
	}
	args := cmd.Args
	if len(args) < 9 || args[1] != CmdAstool || args[2] != "--cd" || args[3] != "/home/artex-tool/mcp/x" || args[4] != "--vars" || args[6] != "--" || args[7] != "npx" || args[9] != "pkg" {
		t.Fatalf("argv %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "canary-value") {
			t.Fatalf("a value is on the command line: %v", args)
		}
	}
	if _, err := os.Lstat(filepath.Join(e.cfg.Layout.VarsDir, args[5])); err != nil {
		t.Errorf("the variables file %s is missing: %v", args[5], err)
	}
	cleanup()
	if _, err := os.Lstat(filepath.Join(e.cfg.Layout.VarsDir, args[5])); !os.IsNotExist(err) {
		t.Errorf("the cleanup left the variables file behind")
	}
	if cmd.Env != nil {
		t.Errorf("the root stage must inherit the (scrubbed) server environment only, got %v", cmd.Env)
	}
}

func TestCommandRefusesADeniedKeyBeforeWritingAnything(t *testing.T) {
	iso, e := activated(t)
	lease, _ := iso.Leases().Acquire(t.Context())
	defer lease.Release()
	_, _, err := iso.Command(t.Context(), lease, Spec{Program: "true", Vars: map[string]string{"LD_PRELOAD": "/tmp/x.so"}})
	var de *DeniedVarError
	if !errors.As(err, &de) || de.Key != "LD_PRELOAD" {
		t.Fatalf("err %v", err)
	}
	if ents, _ := os.ReadDir(e.cfg.Layout.VarsDir); len(ents) != 0 {
		t.Fatalf("files in the variables directory: %v", ents)
	}
}

// Without the drop, the program starts as it did before, but the key check still applies.
func TestCommandWithoutIsolationRunsDirectlyAndStillValidates(t *testing.T) {
	e := newActEnv(t)
	e.probe.env = "off"
	iso, _ := Activate(t.Context(), e.cfg)
	lease, _ := iso.Leases().Acquire(t.Context())
	defer lease.Release()
	cmd, cleanup, err := iso.Command(t.Context(), lease, Spec{Dir: "/tmp", Program: "/bin/true", Args: []string{"a"}, Vars: map[string]string{"FOO": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if cmd.Args[0] != "/bin/true" || cmd.Dir != "/tmp" {
		t.Errorf("argv %v dir %s", cmd.Args, cmd.Dir)
	}
	found := false
	for _, kv := range cmd.Env {
		if kv == "FOO=1" {
			found = true
		}
	}
	if !found {
		t.Error("the per-call variable was dropped")
	}
	if _, _, err := iso.Command(t.Context(), lease, Spec{Program: "/bin/true", Vars: map[string]string{"LD_PRELOAD": "x"}}); err == nil {
		t.Error("the deny-list is validation and applies in every mode")
	}
}

// --- ChainArgs, the part of Command the MCP spawn function shares ---

func TestChainArgsForTheMCPSpawnFunction(t *testing.T) {
	iso, e := activated(t)
	lease, _ := iso.Leases().Acquire(t.Context())
	defer lease.Release()
	prog, args, cleanup, err := iso.ChainArgs(lease, Spec{Dir: "/d", Program: "npx", Args: []string{"-y", "p"}, Vars: map[string]string{"FOO": "canary-value"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if prog != e.cfg.Layout.Artex || args[0] != CmdAstool || args[1] != "--cd" || args[2] != "/d" || args[3] != "--vars" || args[5] != "--" || args[6] != "npx" {
		t.Fatalf("%s %v", prog, args)
	}
	for _, a := range args {
		if strings.Contains(a, "canary-value") {
			t.Fatalf("a value is on the command line: %v", args)
		}
	}
	if _, _, _, err := iso.ChainArgs(nil, Spec{Program: "x"}); err == nil {
		t.Error("a spawn without a lease was accepted")
	}
	// without the drop the same call hands the program back unchanged, variables untouched
	e2 := newActEnv(t)
	e2.probe.env = "off"
	off, _ := Activate(t.Context(), e2.cfg)
	l2, _ := off.Leases().Acquire(t.Context())
	defer l2.Release()
	prog, args, _, err = off.ChainArgs(l2, Spec{Program: "npx", Args: []string{"a"}, Vars: map[string]string{"FOO": "1"}})
	if err != nil || prog != "npx" || len(args) != 1 || args[0] != "a" {
		t.Fatalf("%s %v %v", prog, args, err)
	}
	if _, _, _, err := off.ChainArgs(l2, Spec{Program: "npx", Vars: map[string]string{"LD_PRELOAD": "x"}}); err == nil {
		t.Error("the deny-list applies in every mode")
	}
}

func TestEnsureToolDir(t *testing.T) {
	base := t.TempDir()
	dir, err := EnsureToolDir(base, "mcp", "browser")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(base, "mcp", "browser") {
		t.Fatalf("dir %s", dir)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Errorf("mode %o", got)
	}
	if os.Geteuid() == 0 {
		if u, g := owner(t, dir); u != ToolUID || g != ToolGID {
			t.Errorf("owner %d:%d", u, g)
		}
	}
	if _, err := EnsureToolDir(base, "mcp", "browser"); err != nil {
		t.Errorf("second call: %v", err)
	}
	// a link where a directory should be is refused, and its target is left alone
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "linked")); err != nil {
		t.Fatal(err)
	}
	before := mode(t, outside)
	if _, err := EnsureToolDir(base, "linked", "x"); err == nil {
		t.Error("a symbolic link was accepted")
	}
	if got := mode(t, outside); got != before {
		t.Errorf("the link target changed from %o to %o", before, got)
	}
	// a file where a directory should be
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureToolDir(base, "file"); err == nil {
		t.Error("a file was accepted as a directory")
	}
	for _, bad := range [][]string{{".."}, {"a/b"}, {""}, {"."}} {
		if _, err := EnsureToolDir(base, bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// The parent of an MCP working directory is the tool user's: between the check of a component
// and its change, a tool can put a hard link to a server file at that name. The change must land
// on the directory that was checked or nowhere.
func TestEnsureToolDirChangesOnlyTheCheckedDirectory(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, secret, "x", 0o600)
	if err := os.MkdirAll(filepath.Join(base, "mcp", "srv"), 0o755); err != nil {
		t.Fatal(err)
	}
	beforeUID, beforeGID := owner(t, secret)
	afterDirCheck = func(name string) {
		if name != "srv" {
			return
		}
		p := filepath.Join(base, "mcp", "srv")
		if err := os.Rename(p, p+".moved"); err != nil {
			t.Error(err)
		}
		if err := os.Link(secret, p); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterDirCheck = nil })
	if _, err := EnsureToolDir(base, "mcp", "srv"); err == nil {
		t.Error("a component swapped after its check was accepted")
	}
	if got := mode(t, secret); got != 0o600 {
		t.Errorf("the linked file's mode changed to %o", got)
	}
	if u, g := owner(t, secret); u != beforeUID || g != beforeGID {
		t.Errorf("the linked file's owner changed to %d:%d", u, g)
	}
}

// Entries the server creates after start-up are closed by the next pass.
func TestReapplyPermissionsClosesWhatAppearedLater(t *testing.T) {
	iso, e := activated(t)
	late := filepath.Join(e.cfg.DataDir, "created-later")
	if err := os.MkdirAll(filepath.Join(late, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, late); got != 0o755 {
		t.Fatalf("fixture mode %o", got)
	}
	iso.ReapplyPermissions()
	if got := mode(t, late); got != 0o700 {
		t.Errorf("mode %o after the pass, want 0700", got)
	}
	(*Isolation)(nil).ReapplyPermissions() // a missing isolation is not a crash
}

// AC17(c): the graceful stop sweeps and says so; with no drop there is nothing to sweep.
func TestDrainLogsTheStopTimeCleanup(t *testing.T) {
	e := newActEnv(t)
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	kills := e.killer.kills()
	iso.Drain(t.Context())
	if e.killer.kills() != kills+1 {
		t.Fatalf("kills %d -> %d", kills, e.killer.kills())
	}
	if !strings.Contains(e.joinedLogs(), "[isolation] stop: no tool process left") {
		t.Errorf("no stop line:\n%s", e.joinedLogs())
	}
	e2 := newActEnv(t)
	e2.probe.env = "off"
	off, _ := Activate(t.Context(), e2.cfg)
	off.Drain(t.Context())
	if e2.killer.kills() != 0 {
		t.Error("a disabled isolation swept")
	}
}

// A part started after activation (the egress forwarder) that fails makes the status degraded
// with its own reason; it never switches isolation off. Reporting again replaces the entry.
func TestReportFailureDegradesWithTheReasonAndKeepsIsolation(t *testing.T) {
	needRootForOwnership(t)
	e := newActEnv(t)
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if iso.Status() != Enforced {
		t.Fatalf("status %s before the report", iso.Status())
	}
	iso.ReportFailure("E1", "the egress forwarder cannot listen on 127.0.0.1:8789")
	iso.ReportFailure("E1", "the egress forwarder cannot listen on 127.0.0.1:8789")
	v := iso.View()
	if v.Status != Degraded || !iso.On() || !strings.Contains(v.Reason, "E1: the egress forwarder cannot listen") {
		t.Fatalf("view %+v on=%v", v, iso.On())
	}
	if strings.Count(v.Reason, "E1") != 1 {
		t.Errorf("the entry was added twice: %q", v.Reason)
	}
	if !strings.HasPrefix(os.Getenv("PATH"), e.cfg.Layout.ShimDir+":") {
		t.Error("the shim left PATH")
	}
}

// needRootForOwnership skips a test that drives Activate to the enforced state: the self-check
// hands a probe workspace to the tool user (lchown), which only root may do. Hosted CI runs these
// tests again as root (see the "root-only tests" step of .github/workflows/ci.yml).
func needRootForOwnership(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: the self-check hands a workspace to the tool user")
	}
}

// Test of the skip: as a non-root user the guard skips, and as root it does not.
func TestNeedRootForOwnershipSkipsWithoutRoot(t *testing.T) {
	var sub *testing.T
	t.Run("guard", func(t *testing.T) { sub = t; needRootForOwnership(t) })
	if got, want := sub.Skipped(), os.Geteuid() != 0; got != want {
		t.Fatalf("skipped=%v with euid %d", got, os.Geteuid())
	}
}

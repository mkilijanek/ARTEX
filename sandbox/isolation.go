package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Inactive is the status of an Isolation that was never activated. The server always
// activates; tests that do not are not warned.
const Inactive Status = "inactive"

// Config is what Activate needs. Nothing in it is a secret value.
type Config struct {
	Layout     Layout
	DataDir    string
	KeyDir     string // directory with jwt.key
	ConfigPath string // config.Path()
	SkillDir   string
	DBSockets  []string // database socket files, from the DSN
	DBTCP      []string // database host:port endpoints reached over TCP, from the DSN

	// SignalSocket is the Signal bridge socket file, from ARTEX_SIGNAL_API. Empty: no C12.
	SignalSocket string

	// SecretNames are the variables main read once and removed. Activation refuses to go on
	// while one is still set.
	SecretNames []string

	Probe  Probe        // default: the host
	Killer Killer       // default: the real kill through the drop chain
	Runner ScriptRunner // default: the SDK's Bash tool
	Lease  LeaseOptions
	Logf   func(format string, args ...any) // default: the standard logger

	// Steps, when set, is told the name of each activation step as it starts.
	Steps func(step string)
}

// Isolation is the process-wide isolation state: whether the drop chain is on (fixed for the
// life of the process), the status and its reasons, and the lease counter.
type Isolation struct {
	layout Layout
	on     bool
	leases *Leases
	logf   func(string, ...any)

	permsIn   PermsInput
	permsSeen string // the problems of the last pass, to log a change only

	// What a C12 re-run needs, set at activation when the drop chain is on and a Signal socket
	// is configured. c12mu keeps two re-runs from overlapping; the result goes through setCheck
	// and clearCheck, which take mu.
	c12in  SelfCheckInput
	runner ScriptRunner
	c12mu  sync.Mutex

	mu       sync.Mutex
	pre      Preconditions
	failed   []CheckResult
	status   Status
	view     View
	lastLeft time.Time
}

var current atomic.Pointer[Isolation]

// Set installs the isolation the guard and the spawners consult.
func Set(i *Isolation) { current.Store(i) }

// Current returns the installed isolation, or an inactive one.
func Current() *Isolation {
	if i := current.Load(); i != nil {
		return i
	}
	return &Isolation{status: Inactive, view: View{Status: Inactive, Warnings: []string{}}, logf: log.Printf}
}

// On reports whether tool processes go through the drop chain. It is fixed at activation.
func (i *Isolation) On() bool { return i != nil && i.on }

// Layout returns the paths in use.
func (i *Isolation) Layout() Layout { return i.layout }

// Leases returns the lease counter. It is nil for an inactive isolation, which hands out
// leases that count nothing.
func (i *Isolation) Leases() *Leases {
	if i == nil {
		return nil
	}
	return i.leases
}

// Status returns the current status.
func (i *Isolation) Status() Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

// View returns a copy of the status, reason and warnings.
func (i *Isolation) View() View {
	i.mu.Lock()
	defer i.mu.Unlock()
	v := i.view
	v.Warnings = append([]string{}, v.Warnings...)
	return v
}

// SessionWarning is the text stamped on every session's activity trace, or "" when the status
// needs none (enforced, or inactive).
func (i *Isolation) SessionWarning() string {
	v := i.View()
	switch v.Status {
	case Unavailable, Disabled:
		return "tool isolation " + string(v.Status) + ": tools run as the server user (" + v.Reason + ")"
	case Degraded:
		return "tool isolation degraded: " + v.Reason
	}
	return ""
}

// recompute derives the status and view from the stored facts. The caller holds i.mu.
func (i *Isolation) recompute() {
	i.status, i.view = ComputeStatus(i.pre, i.failed)
}

func (i *Isolation) setCheck(c CheckResult) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for k := range i.failed {
		if i.failed[k].ID == c.ID {
			i.failed[k] = c
			i.recompute()
			return
		}
	}
	i.failed = append(i.failed, c)
	i.recompute()
}

func (i *Isolation) clearCheck(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	kept := i.failed[:0:0]
	for _, c := range i.failed {
		if c.ID != id {
			kept = append(kept, c)
		}
	}
	i.failed = kept
	i.recompute()
}

// Activate runs the activation steps in their fixed order and returns the isolation. Step 1
// of the order (read every secret once, then remove it) belongs to main; Activate refuses to
// go on while any of them is still in the environment, so the order cannot be broken by a
// later edit of main.
//
// The platform preconditions are read first because they only read; the kill and the check
// start tool processes and run only when they hold. Whether isolation is on is decided by the
// preconditions alone, never by a check a tool could make fail.
func Activate(ctx context.Context, cfg Config) (*Isolation, error) {
	l := cfg.Layout
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	step := func(s string) {
		if cfg.Steps != nil {
			cfg.Steps(s)
		}
	}
	probe := cfg.Probe
	if probe == nil {
		probe = HostProbe{Layout: l}
	}

	step("secrets")
	for _, name := range cfg.SecretNames {
		if os.Getenv(name) != "" {
			return nil, fmt.Errorf("%s is still set in the environment: secrets must be read and removed before isolation starts", name)
		}
	}

	step("preconditions")
	pre := CheckPreconditions(l, probe)
	on := pre.DropPossible() && pre.P5
	if v := strings.TrimSpace(probe.Getenv(EnvIsolation)); v != "" && !strings.EqualFold(v, "off") && !strings.EqualFold(v, "on") {
		cfg.Logf("[isolation] %s=%q is not recognised and is ignored; only \"off\" changes the behaviour", EnvIsolation, v)
	}

	opt := cfg.Lease
	if v := os.Getenv("ARTEX_TEST_IDLE_SWEEP_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			opt.Delay = d
		} else {
			cfg.Logf("[isolation] ARTEX_TEST_IDLE_SWEEP_DELAY=%q is ignored", v)
		}
	}
	killer := cfg.Killer
	if killer == nil {
		killer = ProcKiller{Layout: l}
	}
	iso := &Isolation{layout: l, on: on, logf: cfg.Logf, pre: pre}
	iso.status, iso.view = ComputeStatus(pre, nil)
	userLeft, userClear, userFailure := opt.OnLeftover, opt.OnClear, opt.OnFailure
	opt.OnLeftover = func(n int) {
		iso.setCheck(CheckResult{ID: "C10", Detail: fmt.Sprintf("%d tool processes remain after the kill", n), Degrades: true})
		iso.mu.Lock()
		due := time.Since(iso.lastLeft) >= time.Minute
		if due {
			iso.lastLeft = time.Now()
		}
		iso.mu.Unlock()
		if due {
			cfg.Logf("[isolation] %d leftover tool processes; killing again every few seconds", n)
		}
		if userLeft != nil {
			userLeft(n)
		}
	}
	opt.OnFailure = func(err error) {
		iso.setCheck(CheckResult{ID: "C10", Detail: "the cleanup of leftover tool processes failed: " + err.Error(), Degrades: true})
		if userFailure != nil {
			userFailure(err)
		}
	}
	opt.OnClear = func() {
		iso.clearCheck("C10")
		cfg.Logf("[isolation] leftover tool processes are gone")
		if userClear != nil {
			userClear()
		}
	}
	iso.leases = NewLeases(killer, opt)

	if on {
		step("kill")
		res, err := iso.leases.Sweep(ctx, true)
		switch {
		case err != nil:
			cfg.Logf("[isolation] cleanup of leftover tool processes failed: %v", err)
			iso.leases.sweepFailed(err)
		case res.Left > 0:
			cfg.Logf("[isolation] cleanup: %d tool processes survived the kill", res.Left)
		default:
			cfg.Logf("[isolation] cleanup: no tool process left")
		}

		step("tmpdir")
		if err := os.Setenv("TMPDIR", l.ServerTmp); err != nil {
			return nil, fmt.Errorf("set TMPDIR: %w", err)
		}
		step("path")
		if err := os.Setenv("PATH", l.ShimDir+string(os.PathListSeparator)+withoutPathEntry(os.Getenv("PATH"), l.ShimDir)); err != nil {
			return nil, fmt.Errorf("set PATH: %w", err)
		}
	} else {
		// the shim must never be reachable when the drop is not available
		if p := os.Getenv("PATH"); p != withoutPathEntry(p, l.ShimDir) {
			_ = os.Setenv("PATH", withoutPathEntry(p, l.ShimDir))
		}
	}

	step("perms")
	iso.permsIn = PermsInput{Layout: l, AppDir: filepath.Dir(l.Artex), KeyDir: cfg.KeyDir, DataDir: cfg.DataDir, Drop: on}
	rep, err := ApplyPermissions(iso.permsIn)
	if err != nil {
		return nil, fmt.Errorf("permissions: %w", err)
	}
	for _, p := range rep.Problems {
		cfg.Logf("[isolation] permissions: %s", p)
	}
	for _, s := range rep.Symlinks {
		cfg.Logf("[isolation] permissions: %s is a symbolic link and was not followed", s)
	}

	// The offline ownership command refuses to run while this lock is held.
	if cfg.DataDir != "" {
		if err := HoldServerLock(cfg.DataDir); err != nil {
			cfg.Logf("[workspace] the server lock could not be taken: %v (the offline ownership command cannot tell that this server is running)", err)
		}
	}

	if on {
		step("selfcheck")
		in := SelfCheckInput{
			ServerUID: os.Geteuid(), ServerPID: os.Getpid(),
			Key: filepath.Join(cfg.KeyDir, "jwt.key"), ConfigPath: cfg.ConfigPath, Config: fileExists(cfg.ConfigPath),
			DataDir: cfg.DataDir, VarsDir: l.VarsDir, SkillsDir: cfg.SkillDir, Skills: dirExists(cfg.SkillDir),
			Symlinks: rep.Symlinks, Sockets: cfg.DBSockets, TCP: cfg.DBTCP, SignalSocket: cfg.SignalSocket,
			WorkDirParent: filepath.Join(cfg.DataDir, "tasks"), FallbackDir: l.ToolHome,
		}
		run := cfg.Runner
		if run == nil {
			run = SDKRunner
		}
		iso.c12in, iso.runner = in, run
		lease, lerr := iso.leases.Acquire(ctx)
		if lerr != nil {
			return nil, lerr
		}
		failed := RunSelfCheck(ctx, run, in)
		lease.Release()
		if c, bad := linkProtection(l.ProcRoot); bad {
			failed = append(failed, c)
		}
		iso.mu.Lock()
		iso.failed = append(iso.failed, failed...)
		iso.recompute()
		iso.mu.Unlock()
	}

	step("mode")
	iso.mu.Lock()
	iso.recompute()
	iso.mu.Unlock()
	iso.logStatus()
	return iso, nil
}

// linkProtection is check C11: the kernel's hard-link and symbolic-link protection
// (fs.protected_hardlinks, fs.protected_symlinks; host-wide, readable in the container). Without
// them a tool can put a link to a server file where the server works in a tool-writable directory,
// so a value other than 1, or one that cannot be read, degrades the status.
func linkProtection(procRoot string) (CheckResult, bool) {
	var off []string
	for _, p := range []struct{ file, what string }{
		{"protected_hardlinks", "hard-link protection (fs.protected_hardlinks)"},
		{"protected_symlinks", "symbolic-link protection (fs.protected_symlinks)"},
	} {
		if v, err := os.ReadFile(filepath.Join(procRoot, "sys", "fs", p.file)); err != nil || strings.TrimSpace(string(v)) != "1" {
			off = append(off, p.what)
		}
	}
	if len(off) == 0 {
		return CheckResult{}, false
	}
	return CheckResult{ID: "C11", Detail: strings.Join(off, " and ") + " not enabled on the host", Degrades: true}, true
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// withoutPathEntry removes every occurrence of dir from a PATH value.
func withoutPathEntry(path, dir string) string {
	var out []string
	for _, e := range filepath.SplitList(path) {
		if e != dir {
			out = append(out, e)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// logStatus writes the status line, and the warning block when the status is not enforced.
func (i *Isolation) logStatus() {
	v := i.View()
	uid := "30002"
	if !v.Status.IsolationOn() {
		uid = "none"
	}
	i.logf("[isolation] status=%s tool_uid=%s", v.Status, uid)
	if v.Status == Enforced {
		for _, w := range v.Warnings {
			i.logf("[isolation] warning %s", w)
		}
		return
	}
	i.logWarningBlock()
}

// logWarningBlock is the loud block of every start and of every six hours.
func (i *Isolation) logWarningBlock() {
	v := i.View()
	if v.Status == Enforced || v.Status == Inactive || v.Status == "" {
		return
	}
	i.logf("[isolation] WARNING tool isolation is %s: %s", v.Status, v.Reason)
	if v.Status == Degraded {
		i.logf("[isolation] WARNING agent tools are isolated, but the check above failed; they can still reach what it names")
	} else {
		i.logf("[isolation] WARNING agent tools run with the access of the server user, including its files and its database")
	}
	for _, w := range v.Warnings {
		i.logf("[isolation] WARNING %s", w)
	}
	i.logf("[isolation] WARNING see the \"Tool isolation\" section of the documentation")
}

// ReapplyPermissions runs the permission step again. The server creates top-level entries of the
// data directory lazily after start-up; every pass closes what appeared since. A problem is logged
// when it first shows up, not on every pass.
func (i *Isolation) ReapplyPermissions() {
	if i == nil || i.permsIn.DataDir == "" {
		return
	}
	rep, err := ApplyPermissions(i.permsIn)
	seen := strings.Join(rep.Problems, "\n")
	if err != nil {
		seen = err.Error()
	}
	i.mu.Lock()
	changed := seen != i.permsSeen
	i.permsSeen = seen
	i.mu.Unlock()
	if !changed || seen == "" {
		return
	}
	if err != nil {
		i.logf("[isolation] permissions: %v", err)
		return
	}
	for _, p := range rep.Problems {
		i.logf("[isolation] permissions: %s", p)
	}
}

// RecheckSignal runs check C12 alone again and publishes the result like any other check: a
// "not verified" that was set because the bridge socket was not there yet clears once the socket
// exists and the tool user is kept out of it, and a socket that turns out reachable degrades the
// status. The self-check has no periodic loop (it runs once, at start), so the server calls this
// after the first successful Signal send and on each Signal channel test. It does nothing when
// the drop chain is off or no Signal socket is configured, and is safe to call concurrently.
func (i *Isolation) RecheckSignal(ctx context.Context) {
	if i == nil || !i.on || i.c12in.SignalSocket == "" || i.runner == nil {
		return
	}
	i.c12mu.Lock()
	defer i.c12mu.Unlock()
	lease, err := i.leases.Acquire(ctx)
	if err != nil {
		i.logf("[isolation] C12 re-run skipped: %v", err)
		return
	}
	failed := RunSignalCheck(ctx, i.runner, i.c12in)
	lease.Release()

	before := i.checkDetail("C12")
	if len(failed) == 0 {
		i.clearCheck("C12")
	} else {
		i.setCheck(failed[0])
	}
	if after := i.checkDetail("C12"); after != before {
		i.logStatus()
	}
}

// checkDetail is the detail of the failed check id, or "" when it has not failed.
func (i *Isolation) checkDetail(id string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, c := range i.failed {
		if c.ID == id {
			return c.Detail
		}
	}
	return ""
}

// Run starts the sweeper, repeats the permission step every five minutes and the warning block
// every six hours. It returns when ctx ends.
func (i *Isolation) Run(ctx context.Context) {
	if i == nil || i.leases == nil {
		return
	}
	if i.on {
		go i.leases.Run(ctx)
	}
	warn := time.NewTicker(6 * time.Hour)
	defer warn.Stop()
	perms := time.NewTicker(5 * time.Minute)
	defer perms.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-warn.C:
			i.logWarningBlock()
		case <-perms.C:
			i.ReapplyPermissions()
		}
	}
}

// Drain is the graceful stop: wait up to five seconds for the leases, then sweep.
func (i *Isolation) Drain(ctx context.Context) {
	if !i.On() {
		return
	}
	res, err := i.leases.Drain(ctx, 5*time.Second)
	switch {
	case err != nil:
		i.logf("[isolation] stop: cleanup failed: %v", err)
	case res.Left > 0:
		i.logf("[isolation] stop: %d tool processes survived the kill", res.Left)
	default:
		i.logf("[isolation] stop: no tool process left")
	}
}

// Spec describes one tool-user process.
type Spec struct {
	Dir     string            // working directory
	Vars    map[string]string // per-call variables; never on the command line
	Program string
	Args    []string
}

// ChainArgs returns the program and arguments that start spec: through the drop chain when
// isolation is on, the program itself otherwise. It is what the MCP spawn function hands to the
// SDK's stdio client, which builds the process itself. The variables travel in a root-only file
// whose name is the only part on the command line (isolation on); the returned cleanup removes
// that file if the spawn did not consume it. The key check applies in every mode.
func (i *Isolation) ChainArgs(lease *Lease, spec Spec) (program string, args []string, cleanup func(), err error) {
	noop := func() {}
	if lease == nil {
		return "", nil, nil, errors.New("a tool process needs a lease")
	}
	if spec.Program == "" {
		return "", nil, nil, errors.New("no program")
	}
	if err := ValidateVars(spec.Vars); err != nil {
		return "", nil, nil, err
	}
	if !i.On() {
		return spec.Program, spec.Args, noop, nil
	}
	args = []string{CmdAstool}
	cleanup = noop
	if spec.Dir != "" {
		args = append(args, "--cd", spec.Dir)
	}
	if len(spec.Vars) > 0 {
		name, err := WriteVarsFile(i.layout.VarsDir, spec.Vars)
		if err != nil {
			return "", nil, nil, err
		}
		dir := i.layout.VarsDir
		cleanup = func() { _ = os.Remove(filepath.Join(dir, name)) }
		args = append(args, "--vars", name)
	}
	args = append(args, "--", spec.Program)
	return i.layout.Artex, append(args, spec.Args...), cleanup, nil
}

// Command builds the process for spec. Every ARTEX-side spawn of a tool-user process goes
// through here or through the MCP spawn function, and both need a lease. With isolation on the
// program starts through the drop chain (see ChainArgs); with isolation off it starts directly,
// as before, with the variables in its environment.
func (i *Isolation) Command(ctx context.Context, lease *Lease, spec Spec) (*exec.Cmd, func(), error) {
	prog, args, cleanup, err := i.ChainArgs(lease, spec)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, prog, args...)
	if !i.On() {
		cmd.Dir = spec.Dir
		cmd.Env = os.Environ()
		for k, v := range spec.Vars {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return cmd, cleanup, nil
}

// EnsureToolDir makes base/rel... a directory owned by the tool user with mode 0700, creating
// what is missing. base must exist. Every component is made and checked relative to its parent
// and refuses a link or a file in the way; ownership and mode are changed on a descriptor opened
// without following links and compared with what was checked, never by name, so a tool that
// replaced a component (by a link, or a hard link to a server file) cannot make the server change
// anything else. Ownership needs root; without it only the mode is set.
func EnsureToolDir(base string, rel ...string) (string, error) {
	for _, c := range rel {
		if !plainName(c) {
			return "", fmt.Errorf("%q is not a plain directory name", c)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return "", err
	}
	for _, c := range rel {
		next, err := ensureDir(root, c, 0o700, os.Geteuid() == 0, ToolUID, ToolGID)
		root.Close()
		if err != nil {
			return "", fmt.Errorf("%s: %w", c, err)
		}
		root = next
	}
	root.Close()
	return filepath.Join(append([]string{base}, rel...)...), nil
}

// killAllTimeout bounds one KillAll.
var killAllTimeout = 10 * time.Second

// ProcKiller kills the tool user's processes through the drop chain and counts them in /proc.
type ProcKiller struct{ Layout Layout }

// KillAll runs `__astool -- artex __killall`. The kill signals the helper itself, so ending by
// SIGKILL is the success case. The helper gets killAllTimeout; running out of it is a failure
// (the helper is then ended by SIGKILL too, which must not pass for success).
func (k ProcKiller) KillAll(ctx context.Context) error {
	tctx, cancel := context.WithTimeout(ctx, killAllTimeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, k.Layout.Artex, CmdAstool, "--", k.Layout.Artex, CmdKillall)
	cmd.WaitDelay = time.Second // a child left holding the output pipe does not hold the kill up
	out, err := cmd.CombinedOutput()
	if cerr := tctx.Err(); cerr != nil {
		return fmt.Errorf("kill the tool user's processes: not finished in %s: %w", killAllTimeout, cerr)
	}
	if err == nil || killedBySIGKILL(err) {
		return nil
	}
	return fmt.Errorf("kill the tool user's processes: %w (%s)", err, firstLine(string(out)))
}

// Left counts the tool user's processes that are not zombies.
func (k ProcKiller) Left(context.Context) (int, error) {
	return CountToolProcesses(k.Layout.ProcRoot, ToolUID)
}

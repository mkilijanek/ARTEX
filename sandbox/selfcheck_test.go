//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const goodOutput = `uid=30002
gid=30002
groups=30002
CapInh=0000000000000000
CapPrm=0000000000000000
CapEff=0000000000000000
CapBnd=0000000000000000
CapAmb=0000000000000000
NoNewPrivs=1
envnames=HOME LANG LOGNAME PATH PWD SHELL SHLVL TERM TMPDIR USER _
c3.proc1=denied
c3.procself=denied
c3.key=denied
c3.config=denied
c3.traffic=denied
c3.vars=denied
c3.link0=denied
c4.write=ok
c5.skills=notwritable
c6.0=refused
c7.0=closed
c8.0=refused
c9.meta=closed
`

func checkInput() SelfCheckInput {
	return SelfCheckInput{
		ServerUID: 0, Sockets: []string{"/run/artex-private/postgres/.s.PGSQL.5432"}, TCP: []string{"postgres:5432"},
		Config: true, Skills: true, Symlinks: []string{"/app/data/odd"},
	}
}

func failedIDs(rs []CheckResult) string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ",")
}

func TestEvaluateAcceptsAnEnforcedShell(t *testing.T) {
	if got := EvaluateSelfCheck(goodOutput, checkInput()); len(got) != 0 {
		t.Fatalf("failed checks on a good shell: %+v", got)
	}
}

// Every way a tool shell can differ from the intended one is a failed check, and a failed
// check never says anything but degrade or warn.
func TestEvaluateFlagsEachDeviation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		want     string
		degrades bool
	}{
		{"root shell", "uid=30002", "uid=0", "C1,C2", true},
		{"wrong gid", "gid=30002", "gid=0", "C1", true},
		{"extra group", "groups=30002", "groups=30002 4", "C1", true},
		{"capability kept", "CapEff=0000000000000000", "CapEff=00000000a80425fb", "C1", true},
		{"bounding set kept", "CapBnd=0000000000000000", "CapBnd=00000000a80425fb", "C1", true},
		{"no_new_privs missing", "NoNewPrivs=1", "NoNewPrivs=0", "C1", true},
		{"unexpected variable", "envnames=HOME", "envnames=ARTEX_PG_DSN HOME", "C1", true},
		{"server environ readable", "c3.proc1=denied", "c3.proc1=readable", "C3", true},
		{"key readable", "c3.key=denied", "c3.key=readable", "C3", true},
		{"config readable", "c3.config=denied", "c3.config=readable", "C3", true},
		{"traffic listable", "c3.traffic=denied", "c3.traffic=readable", "C3", true},
		{"vars listable", "c3.vars=denied", "c3.vars=readable", "C3", true},
		{"symlinked entry readable", "c3.link0=denied", "c3.link0=readable", "C3", true},
		{"workspace not writable", "c4.write=ok", "c4.write=fail", "C4", true},
		{"skills writable", "c5.skills=notwritable", "c5.skills=writable", "C5", true},
		{"socket reachable", "c6.0=refused", "c6.0=connected", "C6", true},
		{"socket unverifiable", "c6.0=refused", "c6.0=unknown", "C6", true},
		{"database over tcp", "c7.0=closed", "c7.0=open", "C7", false},
		{"docker socket", "c8.0=refused", "c8.0=connected", "C8", false},
		{"metadata", "c9.meta=closed", "c9.meta=open", "C9", false},
		// a probe that could not be run proves nothing, so it degrades like C6
		{"database over tcp unverifiable", "c7.0=closed", "c7.0=unknown", "C7", true},
		{"docker socket unverifiable", "c8.0=refused", "c8.0=unknown", "C8", true},
		{"docker socket not answered", "c8.0=refused\n", "", "C8", true},
		{"metadata unverifiable", "c9.meta=closed", "c9.meta=unknown", "C9", true},
		{"metadata not answered", "c9.meta=closed\n", "", "C9", true},
	} {
		out := strings.Replace(goodOutput, tc.from, tc.to, 1)
		if out == goodOutput {
			t.Fatalf("%s: fixture did not change", tc.name)
		}
		got := EvaluateSelfCheck(out, checkInput())
		if failedIDs(got) != tc.want {
			t.Errorf("%s: failed %q, want %q", tc.name, failedIDs(got), tc.want)
			continue
		}
		for _, r := range got {
			if r.Degrades != tc.degrades {
				t.Errorf("%s: %s degrades=%v, want %v", tc.name, r.ID, r.Degrades, tc.degrades)
			}
		}
	}
}

// A shell the server itself runs as (same uid) is the C2 failure even when the id is 30002.
func TestEvaluateFlagsAToolUidEqualToTheServer(t *testing.T) {
	in := checkInput()
	in.ServerUID = 30002
	if got := failedIDs(EvaluateSelfCheck(goodOutput, in)); got != "C2" {
		t.Fatalf("got %q, want C2", got)
	}
}

// Missing output for a probe is a failure, not a pass.
func TestEvaluateTreatsMissingAnswersAsFailures(t *testing.T) {
	out := strings.Replace(goodOutput, "c3.key=denied\n", "", 1)
	if got := failedIDs(EvaluateSelfCheck(out, checkInput())); got != "C3" {
		t.Fatalf("a missing key probe gave %q, want C3", got)
	}
	if got := failedIDs(EvaluateSelfCheck("", checkInput())); !strings.Contains(got, "C1") {
		t.Fatalf("empty output gave %q, want C1", got)
	}
}

// The probe lists never name a value: only paths and ids reach the details.
func TestEvaluateDetailsNameNoSecrets(t *testing.T) {
	out := strings.Replace(goodOutput, "c3.key=denied", "c3.key=readable", 1)
	got := EvaluateSelfCheck(out, checkInput())
	if len(got) != 1 || !strings.Contains(got[0].Detail, "key") {
		t.Fatalf("%+v", got)
	}
}

// The script must at least run and print every key; its answers are the ones of the shell it
// runs in (here: the test's own user, so the drop checks fail).
func TestSelfCheckScriptRunsAndPrintsEveryProbe(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	in := SelfCheckInput{
		ServerPID: os.Getpid(), Key: dir + "/jwt.key", ConfigPath: dir + "/config.json", Config: true,
		DataDir: dir, VarsDir: dir + "/vars", SkillsDir: dir, Skills: true,
		Symlinks: []string{dir + "/l"}, Sockets: []string{dir + "/no-such.sock"}, TCP: []string{"127.0.0.1:1"},
	}
	out, err := exec.Command(bash, "-c", SelfCheckScript(in)).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	for _, k := range []string{"uid=", "gid=", "groups=", "NoNewPrivs=", "envnames=", "c3.proc1=", "c3.procself=", "c3.key=", "c3.config=", "c3.traffic=", "c3.vars=", "c3.link0=", "c4.write=", "c5.skills=", "c6.0=", "c7.0=", "c8.0=", "c9.meta="} {
		if !strings.Contains("\n"+string(out), "\n"+k) {
			t.Errorf("no %q line in:\n%s", k, out)
		}
	}
}

// toolsOnly makes a directory with links to the named programs only, to run the script with a
// PATH that lacks the others.
func toolsOnly(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("no %s", n)
		}
		if err := os.Symlink(p, filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Without timeout (or bash) the TCP probes cannot run, and without python3 the socket probes
// cannot: they answer unknown, never closed or refused, and the evaluation degrades.
func TestSelfCheckScriptAnswersUnknownWhenAProbeCannotRun(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	bin := toolsOnly(t, "head", "ls", "id", "grep", "tr", "sed", "env", "mktemp", "rm", "bash")
	dir := t.TempDir()
	in := SelfCheckInput{
		ServerPID: os.Getpid(), Key: dir + "/jwt.key", DataDir: dir, VarsDir: dir + "/vars",
		Sockets: []string{dir + "/no-such.sock"}, TCP: []string{"127.0.0.1:1"},
	}
	cmd := exec.Command(bash, "-c", SelfCheckScript(in))
	cmd.Env = []string{"PATH=" + bin}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	for _, k := range []string{"c6.0=unknown", "c7.0=unknown", "c9.meta=unknown"} {
		if !strings.Contains("\n"+string(out), "\n"+k+"\n") {
			t.Errorf("no %q line in:\n%s", k, out)
		}
	}
	got := EvaluateSelfCheck(string(out), in)
	for _, id := range []string{"C6", "C7", "C9"} {
		found := false
		for _, r := range got {
			if r.ID == id && r.Degrades {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not degrade: %+v", id, got)
		}
	}
}

// The socket probe runs python isolated from its working directory (the tool-writable workspace)
// and the environment: a module planted there cannot make a reachable socket look refused.
func TestSelfCheckSocketProbeIgnoresPlantedModules(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "db.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	planted := "def socket(*a, **k):\n    raise OSError('planted')\nAF_UNIX = 1\n"
	for _, p := range []string{filepath.Join(dir, "socket.py"), filepath.Join(dir, "site", "socket.py")} {
		mustWrite(t, p, planted, 0o644)
	}
	in := SelfCheckInput{ServerPID: os.Getpid(), Key: dir + "/jwt.key", DataDir: dir, VarsDir: dir + "/vars", Sockets: []string{sock}}
	cmd := exec.Command(bash, "-c", SelfCheckScript(in))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONPATH=" + filepath.Join(dir, "site")}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if !strings.Contains("\n"+string(out), "\nc6.0=connected\n") {
		t.Errorf("a reachable socket was not reported as connected:\n%s", out)
	}
}

// Paths with quotes or spaces cannot break out of the script.
func TestSelfCheckScriptQuotesPaths(t *testing.T) {
	s := SelfCheckScript(SelfCheckInput{Key: "/a b/'; touch /tmp/x; '"})
	if strings.Contains(s, "'; touch /tmp/x; '") && !strings.Contains(s, `'\''`) {
		t.Fatal("a path with a quote is not escaped")
	}
}

// The runner failing is a degraded status with a reason, never a refusal to start.
func TestRunSelfCheckReportsARunnerFailure(t *testing.T) {
	dir := t.TempDir()
	run := func(ctx context.Context, d, script string) (string, error) { return "", os.ErrPermission }
	got := RunSelfCheck(t.Context(), run, SelfCheckInput{DataDir: dir, WorkDirParent: dir})
	if !strings.Contains(failedIDs(got), "C1") {
		t.Fatalf("%+v", got)
	}
	for _, r := range got {
		if !r.Degrades {
			t.Errorf("%s: a runner failure must degrade, not just warn", r.ID)
		}
	}
}

func TestRunSelfCheckWorksInAFreshWorkspaceAndRemovesIt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to hand the directory to the tool user")
	}
	dir := t.TempDir()
	var seen string
	run := func(ctx context.Context, d, script string) (string, error) {
		seen = d
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			t.Errorf("the workspace does not exist: %v", err)
		}
		return goodOutput, nil
	}
	in := checkInput()
	in.DataDir, in.WorkDirParent = dir, dir
	if got := RunSelfCheck(t.Context(), run, in); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("the check workspace %s was left behind", seen)
	}
}

// A full disk (the check workspace cannot be created) is a C4 failure, and the other checks still
// run in the tool user's home instead of being skipped.
func TestRunSelfCheckWithoutAWorkspaceStillRunsTheShell(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	home := t.TempDir()
	ran := ""
	run := func(ctx context.Context, d, script string) (string, error) {
		ran = d
		return strings.Replace(goodOutput, "c3.key=denied", "c3.key=readable", 1), nil
	}
	parent := filepath.Join(t.TempDir(), "file-in-the-way")
	if err := os.WriteFile(parent, nil, 0o600); err != nil { // MkdirAll under a file fails, as ENOSPC would
		t.Fatal(err)
	}
	in := checkInput()
	in.WorkDirParent, in.FallbackDir = parent, home
	got := RunSelfCheck(t.Context(), run, in)
	if ran != home {
		t.Errorf("the shell ran in %q, want the fallback", ran)
	}
	if ids := failedIDs(got); ids != "C3,C4" {
		t.Fatalf("failed %q, want C3 (found in the fallback) and C4 (no workspace)", ids)
	}
	for _, r := range got {
		if !r.Degrades {
			t.Errorf("%s does not degrade", r.ID)
		}
	}
	// with no fallback the check cannot look at the shell and says so
	in.FallbackDir = ""
	if ids := failedIDs(RunSelfCheck(t.Context(), run, in)); ids != "C4,C1" {
		t.Errorf("failed %q", ids)
	}
}

// ---- C12: the Signal bridge socket (plan AC10) ----

// c12Sock listens on a unix socket in a short temp directory (a socket path is limited to 108
// bytes) and returns its path and the directory holding it. The listener is closed with the test.
func c12Sock(t *testing.T) (path, dir string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "c12")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return path, dir
}

// The probe lines exist only when the path is set, and only while the server's own pre-check finds
// a socket there (an absent socket is reported without the tool shell).
func TestSelfCheckScriptHasC12ProbesOnlyWhenTheSocketPathIsSet(t *testing.T) {
	sock, dir := c12Sock(t)
	in := checkInput()

	if s := SelfCheckScript(in); strings.Contains(s, "c12") {
		t.Errorf("no Signal path, yet the script has C12 lines:\n%s", s)
	}
	in.SignalSocket = sock
	s := SelfCheckScript(in)
	for _, want := range []string{"probe_unix_strict c12.sock " + shq(sock), "probe_list c12.dir " + shq(dir)} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// C6 keeps the shared probe; only C12 uses the strict one
	if strings.Contains(s, "probe_unix c12.sock") {
		t.Errorf("C12 must not use the shared probe_unix:\n%s", s)
	}
	in.Sockets = []string{sock}
	if s := SelfCheckScript(in); !strings.Contains(s, "probe_unix c6.0 "+shq(sock)) {
		t.Errorf("C6 must keep probe_unix:\n%s", s)
	}
	in.Sockets = nil
	in.SignalSocket = filepath.Join(dir, "absent.sock")
	if s := SelfCheckScript(in); strings.Contains(s, "c12") {
		t.Errorf("the socket is absent, yet the script probes it:\n%s", s)
	}
}

// The probes run in a real shell and print both keys (here as the test's own user, so the answers
// are "connected" and "readable").
func TestSelfCheckScriptC12ProbesAnswer(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	sock, _ := c12Sock(t)
	in := checkInput()
	in.SignalSocket = sock
	out, err := exec.Command(bash, "-c", SelfCheckScript(in)).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	kv := parseProbeOutput(string(out))
	if kv["c12.dir"] != "readable" {
		t.Errorf("c12.dir=%q, want readable:\n%s", kv["c12.dir"], out)
	}
	switch _, perr := exec.LookPath("python3"); {
	case perr == nil && kv["c12.sock"] != "connected":
		t.Errorf("c12.sock=%q, want connected:\n%s", kv["c12.sock"], out)
	case perr != nil && kv["c12.sock"] != "unknown":
		t.Errorf("no python3: c12.sock=%q, want unknown:\n%s", kv["c12.sock"], out)
	}
}

// A socket file with nobody listening: the tool user got to the socket (ECONNREFUSED), which is not
// a refusal. The strict probe says "unknown", and C12 degrades. Runs as any user that can reach the
// path (the test's own), so it needs no root.
func TestC12ProbeStrictConnectionRefusedIsUnknown(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	dir, err := os.MkdirTemp("", "c12")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the socket file stays, nobody listens
	if fi, err := os.Lstat(sock); err != nil || fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("a stale socket file is expected: %v", err)
	}
	in := checkInput()
	in.SignalSocket = sock
	out, err := exec.Command(bash, "-c", c12Script(in)).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	kv := parseProbeOutput(string(out))
	if kv["c12.sock"] != "unknown" {
		t.Fatalf("c12.sock=%q, want unknown:\n%s", kv["c12.sock"], out)
	}
	got := EvaluateSelfCheck(goodOutput+string(out), in)
	if failedIDs(got) != "C12" || !got[0].Degrades || !strings.Contains(got[0].Detail, "could not be verified") {
		t.Fatalf("want a degraded C12 that could not be verified, got %+v", got)
	}
}

// The strict probe keeps "refused" for what a tool user that is kept out sees: a path it may not
// traverse (PermissionError) or that is not there (FileNotFoundError). Anything else is unknown.
func TestC12ProbeStrictRefusedAndConnected(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	sock, dir := c12Sock(t)
	run := func(path string) string {
		in := checkInput()
		in.SignalSocket = path
		var b strings.Builder
		writeProbeHelpers(func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) })
		fmt.Fprintf(&b, "probe_unix_strict c12.sock %s\n", shq(path))
		out, err := exec.Command(bash, "-c", b.String()).CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		return parseProbeOutput(string(out))["c12.sock"]
	}
	if got := run(sock); got != "connected" {
		t.Errorf("live socket: %q, want connected", got)
	}
	if got := run(filepath.Join(dir, "absent.sock")); got != "refused" {
		t.Errorf("absent path: %q, want refused", got)
	}
}

// C12 in the evaluation: every way to fail is degraded and never worse; the pass is a refused
// connection with the parent directory denied.
func TestEvaluateC12(t *testing.T) {
	sock, dir := c12Sock(t)
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	asLink := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, asLink); err != nil {
		t.Fatal(err)
	}
	const refused, none = "c12.sock=refused\nc12.dir=denied\n", ""
	for _, tc := range []struct {
		name    string
		path    string
		answers string // appended to a good output
		want    string // "" = no C12; else a substring of the detail
	}{
		{"absent", filepath.Join(dir, "absent.sock"), refused, "not verified: socket absent"},
		{"absent, no answers", filepath.Join(dir, "absent.sock"), none, "not verified: socket absent"},
		{"a regular file at the path", regular, refused, "not verified: socket absent"},
		{"a symbolic link to the socket is not the socket", asLink, refused, "not verified: socket absent"},
		{"parent is not a directory", filepath.Join(regular, "x.sock"), refused, "not verified: socket absent"},
		{"connected", sock, "c12.sock=connected\nc12.dir=denied\n", "tool user can connect to " + sock},
		{"unknown", sock, "c12.sock=unknown\nc12.dir=denied\n", "could not be verified"},
		{"no socket answer", sock, "c12.dir=denied\n", "could not be verified"},
		{"directory readable", sock, "c12.sock=refused\nc12.dir=readable\n", sock},
		{"no directory answer", sock, "c12.sock=refused\n", "could not be verified"},
		{"connected and listable", sock, "c12.sock=connected\nc12.dir=readable\n", "tool user can connect to " + sock},
		{"refused, directory denied", sock, refused, ""},
	} {
		in := checkInput()
		in.SignalSocket = tc.path
		got := EvaluateSelfCheck(goodOutput+tc.answers, in)
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: failed %+v, want no C12", tc.name, got)
			}
			continue
		}
		if failedIDs(got) != "C12" {
			t.Errorf("%s: failed %q, want C12", tc.name, failedIDs(got))
			continue
		}
		if !got[0].Degrades {
			t.Errorf("%s: C12 must degrade", tc.name)
		}
		if !strings.Contains(got[0].Detail, tc.want) {
			t.Errorf("%s: detail %q lacks %q", tc.name, got[0].Detail, tc.want)
		}
	}

	// without a Signal socket the probe keys, if any, mean nothing: no C12 line at all
	in := checkInput()
	if got := EvaluateSelfCheck(goodOutput+"c12.sock=connected\nc12.dir=readable\n", in); len(got) != 0 {
		t.Errorf("no Signal path, yet %+v", got)
	}
}

// The re-run on its own: absent is "not verified" without starting the tool shell; once the
// socket exists the shell runs and C12 passes or fails on its answers.
func TestRunSignalCheck(t *testing.T) {
	sock, dir := c12Sock(t)
	// a workspace parent that cannot be created, so the fallback directory is used and no root
	// is needed to hand a directory to the tool user
	blocker := filepath.Join(dir, "file-in-the-way")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	in := SelfCheckInput{WorkDirParent: blocker, FallbackDir: dir}

	runs := 0
	var script string
	answer := ""
	run := func(ctx context.Context, d, s string) (string, error) { runs++; script = s; return answer, nil }

	if got := RunSignalCheck(t.Context(), run, in); got != nil || runs != 0 {
		t.Fatalf("no Signal path: %+v, %d runs", got, runs)
	}
	in.SignalSocket = filepath.Join(dir, "absent.sock")
	got := RunSignalCheck(t.Context(), run, in)
	if failedIDs(got) != "C12" || !strings.Contains(got[0].Detail, "not verified") || runs != 0 {
		t.Fatalf("absent: %+v, %d runs", got, runs)
	}

	in.SignalSocket = sock
	answer = "c12.sock=refused\nc12.dir=denied\n"
	if got := RunSignalCheck(t.Context(), run, in); got != nil || runs != 1 {
		t.Fatalf("present and closed to tools: %+v, %d runs", got, runs)
	}
	if !strings.Contains(script, "probe_unix_strict c12.sock") || !strings.Contains(script, "probe_list c12.dir") || strings.Contains(script, "c3.") {
		t.Errorf("the re-run script is not the two C12 probes:\n%s", script)
	}
	answer = "c12.sock=connected\nc12.dir=denied\n"
	if got := RunSignalCheck(t.Context(), run, in); failedIDs(got) != "C12" || !got[0].Degrades {
		t.Fatalf("connected: %+v", got)
	}
	failing := func(ctx context.Context, d, s string) (string, error) { return "", os.ErrPermission }
	if got := RunSignalCheck(t.Context(), failing, in); failedIDs(got) != "C12" || !got[0].Degrades {
		t.Fatalf("runner failure: %+v", got)
	}
}

// Through Activate: the self-check at start finds the socket absent (C12 "not verified", degraded),
// and the re-run after the socket exists clears it. Without a Signal socket there is no C12 at all.
func TestActivateC12ThenRecheck(t *testing.T) {
	needRootForOwnership(t)
	sock := filepath.Join(shortSockDir(t), "api.sock")
	e := newActEnv(t)
	e.cfg.SignalSocket = sock
	e.out = goodOutput + "c12.sock=refused\nc12.dir=denied\n"
	iso, err := Activate(t.Context(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := iso.View(); v.Status != Degraded || !strings.Contains(v.Reason, "C12: not verified: socket absent") {
		t.Fatalf("at start: %+v", v)
	}
	listenAt(t, sock)
	iso.RecheckSignal(t.Context())
	if v := iso.View(); v.Status != Enforced {
		t.Fatalf("after the socket exists: %+v", v)
	}

	e2 := newActEnv(t)
	iso2, err := Activate(t.Context(), e2.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := iso2.View(); strings.Contains(v.Reason, "C12") || len(v.Warnings) != 0 {
		t.Fatalf("no Signal socket, yet %+v", v)
	}
}

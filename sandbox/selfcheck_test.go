//go:build unix

package sandbox

import (
	"context"
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

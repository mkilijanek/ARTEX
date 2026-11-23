//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mkilijanek/artex/sandbox"
)

// TestMain lets the test binary stand in for /app/artex: a wrapper script re-executes it with
// ARTEX_TEST_AS_BIN set, and run() then dispatches the stage named by the first argument.
func TestMain(m *testing.M) {
	if os.Getenv("ARTEX_TEST_AS_BIN") == "1" {
		if j := os.Getenv("ARTEX_TEST_LAYOUT"); j != "" {
			if err := json.Unmarshal([]byte(j), &stageLayout); err != nil {
				panic(err)
			}
		}
		os.Exit(run())
	}
	os.Exit(m.Run())
}

type chain struct {
	l       sandbox.Layout
	bin     string
	logFile string
	dir     string
}

// newChain builds a layout whose setpriv is a stand-in that records its arguments and then runs
// the program without changing the user, so the stages can be exercised end to end by tests.
func newChain(t *testing.T) *chain {
	t.Helper()
	dir := t.TempDir()
	c := &chain{dir: dir, logFile: filepath.Join(dir, "setpriv.args")}
	c.bin = filepath.Join(dir, "artex")
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setpriv := filepath.Join(dir, "setpriv")
	write(setpriv, "#!/bin/sh\nprintf '%s\\n' \"$@\" > "+c.logFile+"\nwhile [ \"$1\" != \"--no-new-privs\" ]; do shift; done\nshift\nexec \"$@\"\n")
	vars := filepath.Join(dir, "vars")
	if err := os.Mkdir(vars, 0o700); err != nil {
		t.Fatal(err)
	}
	c.l = sandbox.DefaultLayout()
	c.l.Artex, c.l.Setpriv, c.l.VarsDir = c.bin, setpriv, vars
	lj, err := json.Marshal(c.l)
	if err != nil {
		t.Fatal(err)
	}
	// the wrapper stands in for /app/artex: it re-executes this test binary as the artex binary
	write(c.bin, "#!/bin/sh\nARTEX_TEST_AS_BIN=1 ARTEX_TEST_LAYOUT='"+string(lj)+"' exec "+os.Args[0]+" \"$@\"\n")
	old := stageLayout
	stageLayout = c.l
	t.Cleanup(func() { stageLayout = old })
	return c
}

// astool runs `artex __astool args...` in a child with the given environment.
func (c *chain) astool(t *testing.T, env []string, extra []*os.File, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(c.bin, append([]string{sandbox.CmdAstool}, args...)...)
	cmd.Env = env
	cmd.ExtraFiles = extra
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("the variables file must be owned by root")
	}
}

// AC30, AC2 (environment part): what the program sees is the allow-list plus the fixed values;
// no secret and no loader variable survives, and nothing of the environment is on a command
// line.
func TestChainEnvironmentIsTheAllowListOnly(t *testing.T) {
	c := newChain(t)
	env := []string{
		"PATH=/usr/bin:/bin", "HTTP_PROXY=http://127.0.0.1:8788", "TZ=UTC",
		"ARTEX_PG_DSN=postgres://u:canary-pw@h/db", "ANTHROPIC_API_KEY=canary-key", "ARTEX_LLM_PROXY=http://u:canary-px@x",
		"LD_PRELOAD=/tmp/x.so", "NODE_OPTIONS=--require /x", "BASH_ENV=/x",
	}
	out, _, code := c.astool(t, env, nil, "--", "/bin/sh", "-c", `env; echo "ARGS:$*"`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	names := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "ARGS:") && !strings.HasPrefix(k, "ARTEX_TEST_") {
			names[k] = v
		}
	}
	for _, bad := range []string{"ARTEX_PG_DSN", "ANTHROPIC_API_KEY", "ARTEX_LLM_PROXY", "LD_PRELOAD", "NODE_OPTIONS", "BASH_ENV"} {
		if _, ok := names[bad]; ok {
			t.Errorf("%s reached the program", bad)
		}
	}
	if strings.Contains(strings.ReplaceAll(out, "ARTEX_TEST_LAYOUT", ""), "canary") {
		t.Errorf("a canary value reached the program:\n%s", out)
	}
	if names["HTTP_PROXY"] == "" || names["TZ"] != "UTC" || names["HOME"] != sandbox.ToolHome || names["SHELL"] != "/bin/bash" || names["PATH"] != sandbox.ToolPath {
		t.Errorf("env %v", names)
	}
	args, _ := os.ReadFile(c.logFile)
	if strings.Contains(string(args), "canary") || strings.Contains(string(args), "HTTP_PROXY") {
		t.Errorf("an environment value reached setpriv's arguments:\n%s", args)
	}
}

// Everything after `--` is the program's: a model-chosen command cannot add flags to the stages.
func TestChainPassesTheCommandThroughOpaquely(t *testing.T) {
	c := newChain(t)
	cmd := `printf '%s' "$1"`
	out, _, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, nil, "--", "/bin/sh", "-c", cmd, "x", "--vars /etc/shadow --cd /")
	if code != 0 || out != "--vars /etc/shadow --cd /" {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestChainDeliversVariablesAndRemovesTheFile(t *testing.T) {
	needRoot(t)
	c := newChain(t)
	name, err := sandbox.WriteVarsFile(c.l.VarsDir, map[string]string{"FOO": "canary-value", "BAR": "x y"})
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	script := `echo "FOO=$FOO"; echo "BAR=$BAR"; pwd; if test -e /proc/$$/fd/3; then echo fd3=open; else echo fd3=closed; fi`
	out, _, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, nil, "--cd", work, "--vars", name, "--", "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"FOO=canary-value", "BAR=x y", work, "fd3=closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := os.Lstat(filepath.Join(c.l.VarsDir, name)); !os.IsNotExist(err) {
		t.Error("the variables file was not unlinked")
	}
	args, _ := os.ReadFile(c.logFile)
	if strings.Contains(string(args), "canary-value") {
		t.Errorf("a value is in setpriv's arguments:\n%s", args)
	}
}

// A refused key ends the chain before the program runs; nothing is printed but the key name.
func TestChainRefusesADeniedKey(t *testing.T) {
	needRoot(t)
	c := newChain(t)
	// a planted file (not written through WriteVarsFile) carrying a denied key
	name := "v-planted"
	if err := os.WriteFile(filepath.Join(c.l.VarsDir, name), []byte(`{"LD_PRELOAD":"canary-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, nil, "--vars", name, "--", "/bin/sh", "-c", "echo program-ran")
	if code != sandbox.ExitDeniedVar {
		t.Fatalf("exit %d, want %d (out %q err %q)", code, sandbox.ExitDeniedVar, out, errOut)
	}
	if out != "" || strings.Contains(errOut, "canary-value") {
		t.Fatalf("out %q err %q", out, errOut)
	}
}

// A descriptor 3 the caller leaked is not read as variables: the second stage reads only the
// descriptor it was told about. (The Go runtime itself can hold a file at descriptor 3, which is
// why "descriptor 3 if present" cannot be the protocol.)
func TestChainIgnoresALeakedDescriptor(t *testing.T) {
	c := newChain(t)
	f, err := os.CreateTemp(t.TempDir(), "leak")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"FOO":"leaked"}`)
	f.Seek(0, 0)
	out, _, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, []*os.File{f}, "--", "/bin/sh", "-c", `echo "FOO=${FOO:-unset}"`)
	if code != 0 || strings.TrimSpace(out) != "FOO=unset" {
		t.Fatalf("exit %d out %q", code, out)
	}
}

func TestChainFailsClosedWhenSetprivIsMissing(t *testing.T) {
	c := newChain(t)
	os.Remove(c.l.Setpriv)
	out, errOut, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, nil, "--", "/bin/sh", "-c", "echo program-ran")
	if code == 0 || strings.Contains(out, "program-ran") {
		t.Fatalf("the program ran without the drop: exit %d out %q", code, out)
	}
	if !strings.Contains(errOut, "artex:") {
		t.Errorf("no message: %q", errOut)
	}
}

func TestChainProgramNotFound(t *testing.T) {
	c := newChain(t)
	_, se, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, nil, "--", "no-such-program-xyz")
	if code != sandbox.ExitNotFound {
		t.Fatalf("exit %d, want %d: %s", code, sandbox.ExitNotFound, se)
	}
}

// `__killall` must not run as anyone but the tool user: as root it would end every process.
func TestKillallRefusesRoot(t *testing.T) {
	c := newChain(t)
	out, err := exec.Command(c.bin, sandbox.CmdKillall).CombinedOutput()
	if err == nil {
		t.Fatalf("__killall ran as uid %d: %s", os.Getuid(), out)
	}
	if !strings.Contains(string(out), "artex:") {
		t.Errorf("no message: %q", out)
	}
}

// The shim is the one line of shell that stands between the SDK's bash and the chain: it must
// carry no guard that refuses to run, and its last word is the chain's start.
func TestShimStartsTheChain(t *testing.T) {
	b, err := os.ReadFile("../../docker/shims/bash")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "refusing") || strings.Contains(s, "exit 126") || strings.Contains(s, "BEGIN-GUARD") {
		t.Fatalf("the shim still carries the fail-closed guard of the image slice:\n%s", s)
	}
	last := ""
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			last = l
		}
	}
	if want := `exec ` + sandbox.ArtexBin + ` ` + sandbox.CmdAstool + ` -- /bin/bash "$@"`; last != want {
		t.Fatalf("last command %q, want %q", last, want)
	}
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Error("the shim must start with #!/bin/sh")
	}
}

// The files the root stage executes are root's in the image: COPY keeps the builder's owner
// unless told otherwise, so the Dockerfile must say so for the start script and the shim. (The
// binary is copied from the build stage, whose files are root's.)
func TestDockerfileMakesTheRootFilesRootOwned(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/app/start.sh", "/usr/local/libexec/artex/shims/bash"} {
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) > 2 && f[0] == "COPY" && f[len(f)-1] == target {
				found = true
				if !strings.Contains(line, "--chown=0:0") {
					t.Errorf("the COPY of %s keeps the builder's owner: %s", target, line)
				}
			}
		}
		if !found {
			t.Errorf("no COPY of %s in the Dockerfile", target)
		}
	}
}

// AC2 (egress proxy part): a proxy variable that carries credentials never reaches the program,
// whichever stage of the environment it came from; the credential-free forwarder address does.
func TestChainDropsProxyValuesWithCredentials(t *testing.T) {
	c := newChain(t)
	env := []string{
		"PATH=/usr/bin:/bin",
		"HTTP_PROXY=http://canaryuser:canarypw@proxy.example:3128",
		"https_proxy=socks5://canaryuser:canarypw@proxy.example:1080",
		"ALL_PROXY=canaryuser:canarypw@proxy.example:3128", // no scheme: still a credential
		"all_proxy=http://127.0.0.1:8789",
		"HTTPS_PROXY=http://127.0.0.1:8789", "HTTPS_PROXY=http://canaryuser:canarypw@proxy.example:3128", // a later credential value replaces the clean one: dropped
		"NO_PROXY=localhost,127.0.0.1",
	}
	out, _, code := c.astool(t, env, nil, "--", "/bin/sh", "-c", `env`)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	for _, k := range []string{"HTTP_PROXY", "https_proxy", "ALL_PROXY", "HTTPS_PROXY"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s=%s reached the program", k, v)
		}
	}
	if got["all_proxy"] != "http://127.0.0.1:8789" || got["NO_PROXY"] != "localhost,127.0.0.1" {
		t.Errorf("a clean value was lost: all_proxy=%q NO_PROXY=%q", got["all_proxy"], got["NO_PROXY"])
	}
	if strings.Contains(strings.ReplaceAll(out, "ARTEX_TEST_LAYOUT", ""), "canarypw") {
		t.Errorf("the credential reached the program:\n%s", out)
	}
	args, _ := os.ReadFile(c.logFile)
	if strings.Contains(string(args), "canary") {
		t.Errorf("the credential is in setpriv's arguments:\n%s", args)
	}
}

// The program starts with the tool user's process limit (soft and hard), so a fork loop of the
// tool user ends at that limit instead of at the container's.
func TestChainLimitsTheToolUsersProcesses(t *testing.T) {
	c := newChain(t)
	out, errOut, code := c.astool(t, []string{"PATH=/usr/bin:/bin"}, nil, "--", "/bin/sh", "-c", "grep '^Max processes' /proc/self/limits")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	f := strings.Fields(out)
	want := strconv.Itoa(sandbox.ToolMaxProcesses)
	if len(f) < 4 || f[2] != want || f[3] != want {
		t.Fatalf("limits %q, want soft and hard %s", out, want)
	}
}

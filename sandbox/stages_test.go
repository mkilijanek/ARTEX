package sandbox

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseStageArgs(t *testing.T) {
	a, err := ParseStageArgs([]string{"--cd", "/work", "--vars", "v-1", "--", "/bin/bash", "-c", "echo hi"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.CD != "/work" || a.Vars != "v-1" || a.Program != "/bin/bash" || !reflect.DeepEqual(a.Args, []string{"-c", "echo hi"}) {
		t.Fatalf("%+v", a)
	}
	// everything after the first `--` is opaque, whatever it looks like
	a, err = ParseStageArgs([]string{"--", "/bin/bash", "-c", "--vars /etc/shadow --cd /"}, true)
	if err != nil || a.Vars != "" || a.CD != "" || len(a.Args) != 2 || a.Args[1] != "--vars /etc/shadow --cd /" {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range [][]string{
		{}, {"--"}, {"/bin/bash"}, {"--cd"}, {"--cd", "/x"}, {"--bogus", "--", "/bin/true"}, {"--vars", "v", "--cd"},
		{"--cd", "/a", "--cd", "/b", "--", "/bin/true"}, {"--cd", "", "--", "/bin/true"},
	} {
		if _, err := ParseStageArgs(bad, true); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	// each stage takes its own variables flag
	if _, err := ParseStageArgs([]string{"--vars", "v", "--", "/bin/true"}, false); err == nil {
		t.Error("--vars accepted by the second stage")
	}
	if _, err := ParseStageArgs([]string{"--vars-fd", "7", "--", "/bin/true"}, true); err == nil {
		t.Error("--vars-fd accepted by the root stage")
	}
	a, err = ParseStageArgs([]string{"--vars-fd", "7", "--", "/bin/true"}, false)
	if err != nil || a.VarsFD != 7 {
		t.Errorf("%+v %v", a, err)
	}
	for _, bad := range []string{"0", "1", "2", "-1", "x", ""} {
		if _, err := ParseStageArgs([]string{"--vars-fd", bad, "--", "/bin/true"}, false); err == nil {
			t.Errorf("--vars-fd %q accepted", bad)
		}
	}
}

// The setpriv command line is exactly the one the plan lists: numeric ids, every capability
// set emptied, no_new_privs, then the second stage.
func TestSetprivArgv(t *testing.T) {
	l := DefaultLayout()
	got := SetprivArgv(l, StageArgs{CD: "/w", Program: "/bin/bash", Args: []string{"-c", "id"}})
	want := []string{l.Setpriv, "--reuid=30002", "--regid=30002", "--clear-groups", "--inh-caps=-all", "--ambient-caps=-all",
		"--bounding-set=-all", "--no-new-privs", l.Artex, CmdToolenv, "--cd", "/w", "--", "/bin/bash", "-c", "id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	got = SetprivArgv(l, StageArgs{Program: "/bin/true"})
	if strings.Contains(strings.Join(got, " "), "--cd") || strings.Contains(strings.Join(got, " "), "--vars-fd") {
		t.Errorf("a flag without a value: %v", got)
	}
	got = SetprivArgv(l, StageArgs{Program: "/bin/true", VarsFD: 12})
	if j := strings.Join(got, " "); !strings.Contains(j, "__toolenv --vars-fd 12 -- /bin/true") {
		t.Errorf("%v", got)
	}
}

// AC30: the root stage's output environment is the allow-list plus the fixed values, and it is
// handed over as envp, never as arguments.
func TestPlanAstoolBuildsACleanEnvironment(t *testing.T) {
	l := DefaultLayout()
	plan, err := PlanAstool(l, []string{"--", "/bin/bash", "-c", "id"}, []string{
		"HTTP_PROXY=http://127.0.0.1:8788", "ARTEX_PG_DSN=postgres://u:secret-pw@h/db", "LD_PRELOAD=/tmp/x.so", "PATH=/tmp/evil",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Path != l.Setpriv {
		t.Errorf("path %s", plan.Path)
	}
	env := envMap(plan.Env)
	if env["HTTP_PROXY"] == "" || env["PATH"] != ToolPath || env["HOME"] != ToolHome {
		t.Errorf("env %v", plan.Env)
	}
	if _, ok := env["ARTEX_PG_DSN"]; ok {
		t.Error("a secret reached the next stage")
	}
	if _, ok := env["LD_PRELOAD"]; ok {
		t.Error("a loader variable reached the next stage")
	}
	for _, a := range plan.Argv {
		if strings.Contains(a, "secret-pw") || strings.Contains(a, "HTTP_PROXY") {
			t.Errorf("an environment value is in the arguments: %q", a)
		}
	}
	if plan.VarsFile != nil || strings.Contains(strings.Join(plan.Argv, " "), "--vars-fd") {
		t.Error("a variables file without --vars")
	}
}

// Every program the chain starts gets the process limit except the kill helper: it must still
// start (an exec after the change of user fails while the tool user is at its limit) to end a
// fork loop that reached the limit.
func TestPlanAstoolLimitsProcessesExceptForTheKill(t *testing.T) {
	l := DefaultLayout()
	for _, tc := range []struct {
		args  []string
		limit bool
	}{
		{[]string{"--", "/bin/bash", "-c", "id"}, true},
		{[]string{"--", l.Artex, CmdKillall}, false},
		{[]string{"--", l.Artex, CmdToolfs}, true},
		{[]string{"--", "/bin/sh", CmdKillall}, true},
		{[]string{"--", l.Artex, CmdKillall, "x"}, true},
		{[]string{"--cd", "/tmp", "--", l.Artex, CmdKillall}, true},
	} {
		plan, err := PlanAstool(l, tc.args, nil)
		if err != nil {
			t.Fatal(err)
		}
		if plan.LimitProcesses() != tc.limit {
			t.Errorf("%v: limit %v, want %v", tc.args, plan.LimitProcesses(), tc.limit)
		}
	}
}

func TestPlanAstoolOpensAndUnlinksTheVarsFile(t *testing.T) {
	dir := t.TempDir()
	l := DefaultLayout()
	l.VarsDir = dir
	name, err := WriteVarsFile(dir, map[string]string{"FOO": "bar"})
	if err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		t.Skip("the file must be owned by root")
	}
	plan, err := PlanAstool(l, []string{"--vars", name, "--", "/bin/true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.VarsFile.Close()
	plan.SetVarsFD(11)
	if j := strings.Join(plan.Argv, " "); !strings.Contains(j, "--vars-fd 11 -- /bin/true") || strings.Contains(j, name) {
		t.Errorf("argv %v", plan.Argv)
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Error("the variables file is still there")
	}
	body, _ := io.ReadAll(plan.VarsFile)
	if !strings.Contains(string(body), "bar") {
		t.Errorf("content %q", body)
	}
}

func TestPlanAstoolRefusesUnsafeVarsFiles(t *testing.T) {
	dir := t.TempDir()
	l := DefaultLayout()
	l.VarsDir = dir
	if err := os.WriteFile(filepath.Join(dir, "loose"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(dir, "loose"), 0o644)
	for _, name := range []string{"loose", "../x", "missing", ""} {
		if _, err := PlanAstool(l, []string{"--vars", name, "--", "/bin/true"}, nil); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

// __toolenv applies the per-call variables over the clean environment and fixes SHELL; a
// denied key stops it before the program starts, with no value in the error.
func fdReader(body string) func(int) (io.ReadCloser, error) {
	return func(int) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
}

func TestPlanToolenv(t *testing.T) {
	plan, err := PlanToolenv([]string{"--cd", "/w", "--vars-fd", "9", "--", "/bin/echo", "hi"}, []string{"TZ=UTC", "HOME=" + ToolHome},
		fdReader(`{"FOO":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := envMap(plan.Env)
	if m["FOO"] != "1" || m["SHELL"] != "/bin/bash" || m["TZ"] != "UTC" {
		t.Errorf("env %v", plan.Env)
	}
	if plan.Dir != "/w" || plan.Path != "/bin/echo" || !reflect.DeepEqual(plan.Argv, []string{"/bin/echo", "hi"}) {
		t.Errorf("%+v", plan)
	}
	_, err = PlanToolenv([]string{"--vars-fd", "9", "--", "/bin/echo"}, nil, fdReader(`{"LD_PRELOAD":"secret-value"}`))
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("err %v", err)
	}
	if ExitCodeOf(err) != ExitDeniedVar {
		t.Errorf("exit code %d, want %d", ExitCodeOf(err), ExitDeniedVar)
	}
	// an unreadable descriptor is a refusal too, never a silent run without variables
	_, err = PlanToolenv([]string{"--vars-fd", "9", "--", "/bin/echo"}, nil, func(int) (io.ReadCloser, error) { return nil, os.ErrInvalid })
	if err == nil {
		t.Error("an unreadable descriptor was ignored")
	}
	// no descriptor named: nothing is opened, still SHELL
	plan, err = PlanToolenv([]string{"--", "/bin/echo"}, []string{"TZ=UTC"}, func(int) (io.ReadCloser, error) {
		t.Error("a descriptor was opened without --vars-fd")
		return nil, os.ErrInvalid
	})
	if err != nil || envMap(plan.Env)["SHELL"] != "/bin/bash" {
		t.Fatalf("%v %v", plan, err)
	}
}

func TestPlanToolenvResolvesThePathFromItsOwnEnvironment(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "mytool")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanToolenv([]string{"--", "mytool", "a"}, []string{"PATH=" + dir}, nil)
	if err != nil || plan.Path != prog || plan.Argv[0] != "mytool" {
		t.Fatalf("%+v %v", plan, err)
	}
	if _, err := PlanToolenv([]string{"--", "no-such-tool-xyz"}, []string{"PATH=" + dir}, nil); err == nil || ExitCodeOf(err) != ExitNotFound {
		t.Fatalf("err %v", err)
	}
}

// __killall signals every process of the tool user, and refuses to run as anyone else: as
// root it would end every process in the container.
func TestKillallRefusesToRunAsAnotherUser(t *testing.T) {
	if err := CheckKillallIdentity(0, 0); err == nil {
		t.Error("root was allowed")
	}
	if err := CheckKillallIdentity(ToolUID, 0); err == nil {
		t.Error("an effective uid of root was allowed")
	}
	if err := CheckKillallIdentity(1000, 1000); err == nil {
		t.Error("another user was allowed")
	}
	if err := CheckKillallIdentity(ToolUID, ToolUID); err != nil {
		t.Errorf("the tool user was refused: %v", err)
	}
}

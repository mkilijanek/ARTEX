//go:build unix

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mkilijanek/artex/sandbox"
)

// fakeRootStage stands in for `artex __astool`: it records what it was started with, reads and
// removes the variables file the way the real stage does, and runs the program in --cd. The
// interpreter in these tests is /bin/sh, so the "script" is a shell script.
func fakeRootStage(t *testing.T, l sandbox.Layout) (rec string) {
	t.Helper()
	rec = t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" > %[1]s/argv
env > %[1]s/env
shift # the stage name
cd=""; vars=""
while [ "$1" != "--" ]; do
  case "$1" in
    --cd) cd="$2"; shift 2 ;;
    --vars) vars="$2"; shift 2 ;;
    *) echo "unexpected $1" > %[1]s/bad; exit 99 ;;
  esac
done
shift
if [ -n "$vars" ]; then cp "%[2]s/$vars" %[1]s/vars.json && rm "%[2]s/$vars"; fi
[ -n "$FAKE_SLEEP" ] && sleep "$FAKE_SLEEP"
cd "$cd" && exec "$@"
`, rec, l.VarsDir)
	if err := os.MkdirAll(filepath.Dir(l.Artex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Artex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return rec
}

func readRec(t *testing.T, rec, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rec, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

// AC16: a script tool starts through the drop chain, in the workspace, with its script outside the
// workspace and the TOOL_ values in the variables file only.
func TestScriptToolStartsThroughTheDropChain(t *testing.T) {
	_, l := activateEnforcedTree(t)
	rec := fakeRootStage(t, l)
	data, work := t.TempDir(), t.TempDir()
	out, err := execPython(t.Context(), "/bin/sh", "echo cwd=$(pwd)\necho script=$0\n", map[string]any{"target": "CANARY-TARGET-VALUE", "n": float64(3)}, work, data, []string{"HTTP_PROXY=http://127.0.0.1:8788"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if bad, _ := os.ReadFile(filepath.Join(rec, "bad")); len(bad) != 0 {
		t.Fatalf("the stage was started with an argument it does not know: %s", bad)
	}
	argv := readRec(t, rec, "argv")
	if !strings.HasPrefix(argv, sandbox.CmdAstool+" --cd "+work+" --vars v-") {
		t.Errorf("argv %q", argv)
	}
	if !strings.Contains(argv, " -- /bin/sh "+filepath.Join(data, "spill", "scripts")+"/") || !strings.Contains(argv, ".py") {
		t.Errorf("the script is not under <data>/spill/scripts: %q", argv)
	}
	if strings.Contains(argv, "CANARY-TARGET-VALUE") || strings.Contains(readRec(t, rec, "env"), "CANARY-TARGET-VALUE") || strings.Contains(readRec(t, rec, "env"), "TOOL_TARGET") {
		t.Error("a TOOL_ value reached the arguments or the root stage's environment")
	}
	var vars map[string]string
	if err := json.Unmarshal([]byte(readRec(t, rec, "vars.json")), &vars); err != nil {
		t.Fatal(err)
	}
	if vars["TOOL_TARGET"] != "CANARY-TARGET-VALUE" || vars["TOOL_N"] != "3" || len(vars) != 2 {
		t.Errorf("variables file %v", vars)
	}
	if !strings.Contains(readRec(t, rec, "env"), "HTTP_PROXY=http://127.0.0.1:8788") {
		t.Error("the session's proxy variable did not reach the stage (it filters to the allow-list)")
	}
	if !strings.Contains(out, "cwd="+work) {
		t.Errorf("output %q: the script did not run in the workspace", out)
	}
	if m := regexpFind(out, `script=(\S+)`); !strings.HasPrefix(m, filepath.Join(data, "spill", "scripts")+"/") || strings.HasPrefix(m, work) {
		t.Errorf("script path %q", m)
	}
	if _, err := os.Stat(filepath.Join(work, ".tools")); err == nil {
		t.Error("a .tools directory was made in the workspace")
	}
	if ents, _ := os.ReadDir(filepath.Join(data, "spill", "scripts")); len(ents) != 0 {
		t.Errorf("script files left behind: %v", ents)
	}
	if ents, _ := os.ReadDir(l.VarsDir); len(ents) != 0 {
		t.Errorf("variables files left behind: %v", ents)
	}
}

func regexpFind(s, re string) string {
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "script="); i >= 0 {
			return strings.TrimSpace(line[i+len("script="):])
		}
	}
	return ""
}

// The script directory is the server's: a directory only the server can write, 0711, and the
// file is made exclusively with a name nobody can guess.
func TestScriptFileIsTheServersAndOutOfTheToolsReach(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: the scripts directory and the files in it are owned by root")
	}
	_, l := activateEnforcedTree(t)
	rec := fakeRootStage(t, l)
	_ = rec
	data, work := t.TempDir(), t.TempDir()
	var seen []string
	var mu sync.Mutex
	code := "ls -ld \"$0\" \"$(dirname \"$0\")\" \"$(dirname \"$(dirname \"$0\")\")\"\nstat -c '%a %u' \"$0\" \"$(dirname \"$0\")\" \"$(dirname \"$(dirname \"$0\")\")\"\necho NAME=$(basename \"$0\")\n"
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := execPython(t.Context(), "/bin/sh", code, nil, work, data, nil, 10*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen = append(seen, out)
			mu.Unlock()
		}()
	}
	wg.Wait()
	names := map[string]bool{}
	for _, out := range seen {
		lines := strings.Split(strings.TrimSpace(out), "\n")
		var stats []string
		for _, ln := range lines {
			if strings.HasPrefix(ln, "NAME=") {
				names[strings.TrimPrefix(ln, "NAME=")] = true
			}
			if len(ln) > 0 && ln[0] >= '0' && ln[0] <= '7' && !strings.Contains(ln, "/") {
				stats = append(stats, ln)
			}
		}
		if len(stats) != 3 {
			t.Fatalf("stat output %q", out)
		}
		if stats[0] != "644 0" || stats[1] != "711 0" || stats[2] != "711 0" {
			t.Errorf("file, scripts directory and spill directory: %v (want 644/711/711, owner root)", stats)
		}
	}
	if len(names) != 6 {
		t.Errorf("script names not unique: %v", names)
	}
	for n := range names {
		if !strings.HasSuffix(n, ".py") || len(n) < 20 {
			t.Errorf("name %q looks guessable", n)
		}
	}
}

// A link planted at <data>/spill or <data>/spill/scripts, or a file where the directory belongs,
// stops the run; nothing is written through it.
func TestScriptToolRefusesALinkedScriptDirectory(t *testing.T) {
	_, l := activateEnforcedTree(t)
	rec := fakeRootStage(t, l)
	data, work, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "spill"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "spill", "scripts")); err != nil {
		t.Fatal(err)
	}
	if _, err := execPython(t.Context(), "/bin/sh", "echo ran", nil, work, data, nil, 10*time.Second); err == nil {
		t.Fatal("the script was written through a linked directory")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("files appeared outside: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(rec, "argv")); err == nil {
		t.Error("the program was started")
	}
}

func TestScriptToolRefusesAParameterThatCannotBeAVariable(t *testing.T) {
	_, l := activateEnforcedTree(t)
	fakeRootStage(t, l)
	data, work := t.TempDir(), t.TempDir()
	// a value with a NUL byte is refused with an error, as exec would have refused it
	if _, err := execPython(t.Context(), "/bin/sh", "echo ran", map[string]any{"a": "x\x00y"}, work, data, nil, 10*time.Second); err == nil {
		t.Error("a value with a NUL byte was accepted")
	}
	// a name that is not a variable name is not mirrored (the JSON on stdin still has it)
	out, err := execPython(t.Context(), "/bin/sh", "cat", map[string]any{"bad name=x": "v", "good": "w"}, work, data, nil, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"bad name=x":"v"`) {
		t.Errorf("stdin lost a parameter: %q", out)
	}
}

// The run holds a lease, so the idle sweep cannot end the interpreter.
func TestScriptToolHoldsALeaseWhileItRuns(t *testing.T) {
	iso, l := activateEnforcedTree(t)
	fakeRootStage(t, l)
	t.Setenv("FAKE_SLEEP", "1")
	data, work := t.TempDir(), t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := execPython(t.Context(), "/bin/sh", "echo ran", nil, work, data, nil, 10*time.Second)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	held := 0
	for time.Now().Before(deadline) && held == 0 {
		held = iso.Leases().Held()
		time.Sleep(10 * time.Millisecond)
	}
	if held != 1 {
		t.Errorf("leases held during the run: %d", held)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if iso.Leases().Held() != 0 {
		t.Errorf("leases held after the run: %d", iso.Leases().Held())
	}
}

// Without isolation the interpreter starts directly, as before, with the values in its
// environment and the script still outside the workspace.
func TestScriptToolWithoutIsolationStartsDirectly(t *testing.T) {
	activateUnavailable(t)
	data, work := t.TempDir(), t.TempDir()
	out, err := execPython(t.Context(), "/bin/sh", "echo T=$TOOL_TARGET P=$HTTP_PROXY cwd=$(pwd) s=$0", map[string]any{"target": "example.com"}, work, data, []string{"HTTP_PROXY=http://p:1"}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"T=example.com", "P=http://p:1", "cwd=" + work, "s=" + filepath.Join(data, "spill", "scripts") + "/"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestScriptToolTimeout(t *testing.T) {
	activateUnavailable(t)
	data, work := t.TempDir(), t.TempDir()
	start := time.Now()
	out, err := execPython(t.Context(), "/bin/sh", "sleep 30", nil, work, data, nil, 300*time.Millisecond)
	if err != nil || !strings.Contains(out, "terminated on timeout") {
		t.Fatalf("%q %v", out, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the timeout did not stop the process")
	}
}

var _ = context.Background
var _ = syscall.EPERM

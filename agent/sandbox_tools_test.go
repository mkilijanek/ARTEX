//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/sandbox"
)

// toolfsStandIn is what the test binary does when it is started as the `__toolfs` helper. The mode
// (ARTEX_TEST_TOOLFS_MODE) picks a misbehaviour; the default is the real helper.
func toolfsStandIn() int {
	switch os.Getenv("ARTEX_TEST_TOOLFS_MODE") {
	case "fail":
		fmt.Fprintln(os.Stderr, "artex: boom")
		return 3
	case "garbage":
		fmt.Print("this is not json")
		return 0
	case "flood":
		fmt.Print(strings.Repeat("x", 200_000))
		return 0
	case "sleep":
		// a child in the same process group, then wait: a timeout must end both
		c := exec.Command("sleep", "60")
		if err := c.Start(); err != nil {
			return 4
		}
		_ = os.WriteFile(os.Getenv("ARTEX_TEST_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o600)
		time.Sleep(60 * time.Second)
		return 0
	case "slow":
		time.Sleep(500 * time.Millisecond)
	}
	if err := RunToolfs(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "artex: "+err.Error())
		return 2
	}
	return 0
}

// toolfsEnv is an isolation that is on, whose artex binary is a script that stands in for the drop
// chain: it records its arguments, changes into --cd and runs this test binary as the helper.
type toolfsEnv struct {
	iso  *sandbox.Isolation
	log  string // one line per helper start
	data string
	pid  string
}

func newToolfsEnv(t *testing.T, mode string) *toolfsEnv {
	t.Helper()
	t.Setenv("NORMA_RIPGREP_NO_INSTALL", "1")
	iso := activateForTest(t, goodPlatform())
	if !iso.On() {
		t.Fatal("the test isolation is off")
	}
	dir := t.TempDir()
	e := &toolfsEnv{iso: iso, log: filepath.Join(dir, "starts.log"), pid: filepath.Join(dir, "child.pid")}
	script := `#!/bin/sh
echo "$@" >> ` + e.log + `
shift
if [ "$1" = "--cd" ]; then cd "$2" || exit 126; shift 2; fi
shift
shift
ARTEX_TEST_TOOLFS=1 ARTEX_TEST_TOOLFS_MODE=` + mode + ` ARTEX_TEST_PIDFILE=` + e.pid + ` exec ` + os.Args[0] + ` "$@"
`
	if err := os.WriteFile(iso.Layout().Artex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *toolfsEnv) starts(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(e.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func wrapped(name string) actool.CoreTool {
	for _, t := range defaultTools() {
		if t.Name() == name {
			return t
		}
	}
	panic("no tool " + name)
}

func sdkTool(name string) actool.CoreTool { return newFileTool(name) }

func text(r actool.Result) string { return r.Flatten() }

// --- what the model sees does not change (AC26) ---

func TestIsolatedFileToolsDescribeThemselvesLikeTheSDKTools(t *testing.T) {
	for _, name := range fileToolNames {
		w, s := wrapped(name), sdkTool(name)
		if w.Name() != s.Name() || w.Description() != s.Description() || w.Prompt() != s.Prompt() {
			t.Errorf("%s: name, description or prompt differ", name)
		}
		if !reflect.DeepEqual(w.InputSchema(), s.InputSchema()) {
			t.Errorf("%s: the schema differs", name)
		}
		in := json.RawMessage(`{"file_path":"x"}`)
		if w.IsReadOnly(in) != s.IsReadOnly(in) || w.IsConcurrencySafe(in) != s.IsConcurrencySafe(in) {
			t.Errorf("%s: scheduling flags differ", name)
		}
		pc := permission.Context{}
		if !reflect.DeepEqual(w.CheckPermissions(t.Context(), in, pc), s.CheckPermissions(t.Context(), in, pc)) {
			t.Errorf("%s: the permission decision differs", name)
		}
	}
	if len(defaultTools()) != len(actool.DefaultTools()) {
		t.Error("defaultTools changed the number of tools")
	}
}

type call struct {
	tool  string
	input string
}

func fixture(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	w := func(rel, body string) {
		p := filepath.Join(d, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w("a.txt", "alpha\nbeta needle\ngamma\n")
	w("sub/b.go", "package b\n// needle here\nfunc F() {}\n")
	w("sub/deep/c.md", "# c\nneedle needle\n")
	w("empty.txt", "")
	w("bin.dat", "ab\x00cd")
	w("long.txt", strings.Repeat("y", 2500)+"\nshort\n")
	w("utf8.txt", "zażółć gęślą jaźń\nline two\n")
	var many strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&many, "row %04d needle\n", i)
	}
	w("many.txt", many.String())
	return d
}

var readOnlyCalls = []call{
	{"Read", `{"file_path":"a.txt"}`},
	{"Read", `{"file_path":"a.txt","offset":2,"limit":1}`},
	{"Read", `{"file_path":"empty.txt"}`},
	{"Read", `{"file_path":"bin.dat"}`},
	{"Read", `{"file_path":"long.txt"}`},
	{"Read", `{"file_path":"utf8.txt"}`},
	{"Read", `{"file_path":"missing.txt"}`},
	{"Read", `{"file_path":"sub"}`},
	{"Read", `{"file_path":"a.txt","offset":99}`},
	{"Read", `{"file_path":"many.txt"}`},
	{"Read", `not json`},
	{"Read", ``},
	{"LS", `{}`},
	{"LS", `{"path":"sub"}`},
	{"LS", `{"path":"nope"}`},
	{"Glob", `{"pattern":"**/*.go"}`},
	{"Glob", `{"pattern":"*.txt"}`},
	{"Glob", `{"pattern":"*.zip"}`},
	{"Grep", `{"pattern":"needle"}`},
	{"Grep", `{"pattern":"needle","output_mode":"count"}`},
	{"Grep", `{"pattern":"needle","output_mode":"files_with_matches"}`},
	{"Grep", `{"pattern":"NEEDLE","-i":true,"glob":"*.go"}`},
	{"Grep", `{"pattern":"("}`},
	{"Grep", `{"pattern":"zzz-no-match"}`},
	{"Edit", `{"file_path":"a.txt","old_string":"x","new_string":"x"}`},
}

var mutatingCalls = []call{
	{"Write", `{"file_path":"new/dir/f.txt","content":"hello\n"}`},
	{"Write", `{"file_path":"a.txt","content":"over\n"}`},
	{"Write", `{"file_path":"sub","content":"x"}`},
	{"Edit", `{"file_path":"a.txt","old_string":"beta","new_string":"BETA"}`},
	{"Edit", `{"file_path":"a.txt","old_string":"nothing","new_string":"x"}`},
	{"Edit", `{"file_path":"many.txt","old_string":"needle","new_string":"pin"}`},
	{"Edit", `{"file_path":"many.txt","old_string":"needle","new_string":"pin","replace_all":true}`},
	{"MultiEdit", `{"file_path":"a.txt","edits":[{"old_string":"alpha","new_string":"A"},{"old_string":"gamma","new_string":"G"}]}`},
	{"MultiEdit", `{"file_path":"a.txt","edits":[{"old_string":"alpha","new_string":"A"},{"old_string":"nope","new_string":"G"}]}`},
}

func runTool(t *testing.T, tl actool.CoreTool, c call, tc *actool.ToolContext) (string, bool, string) {
	t.Helper()
	var in json.RawMessage
	if c.input != "" {
		in = json.RawMessage(c.input)
	}
	res, err := tl.Call(t.Context(), in, tc)
	if err != nil {
		return "", false, err.Error()
	}
	return text(res), res.IsError, ""
}

// AC26: on the fixture set the wrapped tools return what the SDK tools return, errors included.
func TestFileToolResultsMatchTheSDKTools(t *testing.T) {
	e := newToolfsEnv(t, "")
	dir := fixture(t)
	tc := &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 3000}
	for _, c := range readOnlyCalls {
		wantText, wantErrFlag, wantErr := runTool(t, sdkTool(c.tool), c, tc)
		gotText, gotErrFlag, gotErr := runTool(t, wrapped(c.tool), c, tc)
		if gotText != wantText || gotErrFlag != wantErrFlag || gotErr != wantErr {
			t.Errorf("%s %s:\n in process: %q err=%v goerr=%q\n isolated:   %q err=%v goerr=%q", c.tool, c.input, wantText, wantErrFlag, wantErr, gotText, gotErrFlag, gotErr)
		}
	}
	if got := len(e.starts(t)); got != len(readOnlyCalls) {
		t.Errorf("the helper started %d times for %d calls", got, len(readOnlyCalls))
	}

	// the same changes in two copies of the workspace: the same answers and the same files
	a, b := fixture(t), fixture(t)
	for _, c := range mutatingCalls {
		wantText, wantErrFlag, wantErr := runTool(t, sdkTool(c.tool), c, &actool.ToolContext{WorkingDir: a, MaxOutputChars: 3000})
		gotText, gotErrFlag, gotErr := runTool(t, wrapped(c.tool), c, &actool.ToolContext{WorkingDir: b, MaxOutputChars: 3000})
		if gotText != wantText || gotErrFlag != wantErrFlag || gotErr != wantErr {
			t.Errorf("%s %s:\n in process: %q\n isolated:   %q", c.tool, c.input, wantText, gotText)
		}
	}
	sameTree(t, a, b)
}

func sameTree(t *testing.T, a, b string) {
	t.Helper()
	read := func(root string) map[string]string {
		m := map[string]string{}
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := os.ReadFile(p)
				rel, _ := filepath.Rel(root, p)
				m[rel] = string(b)
			}
			return nil
		})
		return m
	}
	if !reflect.DeepEqual(read(a), read(b)) {
		t.Errorf("the two workspaces differ after the same changes:\n%v\n%v", read(a), read(b))
	}
}

var spillPath = func(s string) string {
	i := strings.Index(s, "Full output saved to")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(s[i+len("Full output saved to"):])
	return strings.TrimSuffix(rest, " </persisted-output>")
}

// MaxOutputChars and truncation behave as before: an output over the budget is spilled whole to the
// session's spill directory with a head and a pointer when there is one, and cut head-and-tail when
// there is not. Read pages its own output.
func TestOutputBudgetAndSpillBehaveAsInProcess(t *testing.T) {
	newToolfsEnv(t, "")
	dir := fixture(t)
	// a directory with many entries, a tree with many matches
	lots := filepath.Join(dir, "lots")
	if err := os.MkdirAll(lots, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		os.WriteFile(filepath.Join(lots, fmt.Sprintf("entry-%04d.txt", i)), []byte("needle\n"), 0o644)
	}
	calls := []call{
		{"LS", `{"path":"lots"}`},
		{"Glob", `{"pattern":"lots/*.txt"}`},
		{"Grep", `{"pattern":"needle","head_limit":0}`},
	}
	for _, withSpill := range []bool{false, true} {
		for _, c := range calls {
			spillA, spillB := "", ""
			if withSpill {
				spillA, spillB = t.TempDir(), t.TempDir()
			}
			tcA := &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 2000, OutputDir: spillA}
			tcB := &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 2000, OutputDir: spillB}
			wantText, _, _ := runTool(t, sdkTool(c.tool), c, tcA)
			gotText, _, gotErr := runTool(t, wrapped(c.tool), c, tcB)
			if gotErr != "" {
				t.Fatalf("%s: %s", c.tool, gotErr)
			}
			if len(wantText) <= 2000 {
				t.Fatalf("%s: the fixture does not exceed the budget (%d)", c.tool, len(wantText))
			}
			if !withSpill {
				if gotText != wantText || !strings.Contains(gotText, "characters truncated]") {
					t.Errorf("%s without a spill directory:\n%q\nwant\n%q", c.tool, gotText, wantText)
				}
				continue
			}
			pa, pb := spillPath(wantText), spillPath(gotText)
			if pa == "" || pb == "" || filepath.Dir(pb) != spillB {
				t.Fatalf("%s: pointers %q and %q (spill dir %s)", c.tool, pa, pb, spillB)
			}
			norm := func(s, p string) string { return strings.ReplaceAll(s, p, "SPILLFILE") }
			if norm(gotText, pb) != norm(wantText, pa) {
				t.Errorf("%s with a spill directory: the head or the notice differs:\n%q\nwant\n%q", c.tool, gotText, wantText)
			}
			fa, _ := os.ReadFile(pa)
			fb, _ := os.ReadFile(pb)
			if len(fa) == 0 || string(fa) != string(fb) {
				t.Errorf("%s: the spilled file differs from the in-process one (%d and %d bytes)", c.tool, len(fa), len(fb))
			}
		}
	}
	// Read pages against MaxOutputChars + 1000, as in process, with no spill
	tc := &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 1500}
	c := call{"Read", `{"file_path":"many.txt"}`}
	want, _, _ := runTool(t, sdkTool("Read"), c, tc)
	got, _, _ := runTool(t, wrapped("Read"), c, tc)
	if got != want || !strings.Contains(got, "Read truncated") {
		t.Errorf("Read paging differs:\n%q\nwant\n%q", got, want)
	}
}

// The input reaches the tool byte for byte: an empty input is the tool's own error, not null.
func TestEmptyAndOddInputsAreNotNormalised(t *testing.T) {
	newToolfsEnv(t, "")
	for _, in := range []string{"", "null", "  {\"file_path\":\"a.txt\"}  ", "[]", `{"file_path":5}`} {
		tc := &actool.ToolContext{WorkingDir: fixture(t), MaxOutputChars: 1000}
		var raw json.RawMessage
		if in != "" {
			raw = json.RawMessage(in)
		}
		wantRes, wantErr := sdkTool("Read").Call(t.Context(), raw, tc)
		gotRes, gotErr := wrapped("Read").Call(t.Context(), raw, tc)
		if fmt.Sprint(wantErr) != fmt.Sprint(gotErr) || text(wantRes) != text(gotRes) || wantRes.IsError != gotRes.IsError {
			t.Errorf("input %q: in process (%q, %v) isolated (%q, %v)", in, text(wantRes), wantErr, text(gotRes), gotErr)
		}
	}
}

// --- where it runs ---

func TestHelperRunsThroughTheDropChainInTheWorkingDirectory(t *testing.T) {
	e := newToolfsEnv(t, "")
	dir := fixture(t)
	res, err := wrapped("Read").Call(t.Context(), json.RawMessage(`{"file_path":"a.txt"}`), &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 1000})
	if err != nil || !strings.Contains(text(res), "beta needle") {
		t.Fatalf("%v %q", err, text(res))
	}
	starts := e.starts(t)
	want := "__astool --cd " + dir + " -- " + e.iso.Layout().Artex + " __toolfs"
	if len(starts) != 1 || starts[0] != want {
		t.Fatalf("the helper was started as %q, want %q", starts, want)
	}
	// a relative working directory becomes absolute for both the chain and the request
	cwd, _ := os.Getwd()
	rel, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Skip("no relative path to the fixture")
	}
	if _, err := wrapped("Read").Call(t.Context(), json.RawMessage(`{"file_path":"a.txt"}`), &actool.ToolContext{WorkingDir: rel, MaxOutputChars: 1000}); err != nil {
		t.Fatal(err)
	}
	if s := e.starts(t); len(s) != 2 || s[1] != want {
		t.Fatalf("relative working directory: %q", s)
	}
}

func TestIsolatedCallHoldsALeaseWhileTheHelperRuns(t *testing.T) {
	e := newToolfsEnv(t, "slow")
	done := make(chan error, 1)
	dir := fixture(t)
	go func() {
		_, err := wrapped("LS").Call(context.Background(), json.RawMessage(`{}`), &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 1000})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for e.iso.Leases().Held() == 0 {
		select {
		case <-deadline:
			t.Fatal("no lease was held while the helper ran")
		case err := <-done:
			t.Fatalf("the call ended before a lease was seen: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.iso.Leases().Held() != 0 {
		t.Errorf("the lease was not released: %d", e.iso.Leases().Held())
	}
}

// With isolation off or unavailable the SDK tools run in process, as before, and no helper starts.
func TestWithoutIsolationTheFileToolsRunInProcess(t *testing.T) {
	for _, name := range []string{"unset", "off"} {
		e := newToolfsEnv(t, "fail") // a helper that would fail if it were used
		if name == "off" {
			sandbox.Set(nil)
			iso := activateForTest(t, &fakePlatform{env: "off"})
			if iso.On() {
				t.Fatal("isolation is on")
			}
			e.iso = iso
		} else {
			sandbox.Set(nil)
		}
		res, err := wrapped("Read").Call(t.Context(), json.RawMessage(`{"file_path":"a.txt"}`), &actool.ToolContext{WorkingDir: fixture(t)})
		if err != nil || !strings.Contains(text(res), "beta needle") {
			t.Errorf("%s: %v %q", name, err, text(res))
		}
		if len(e.starts(t)) != 0 {
			t.Errorf("%s: a helper was started", name)
		}
	}
}

// --- failure ---

func TestHelperFailureIsAToolErrorAndNeverAnInProcessFallback(t *testing.T) {
	for _, mode := range []string{"fail", "garbage"} {
		e := newToolfsEnv(t, mode)
		logs := captureLog(t)
		dir := fixture(t)
		res, err := wrapped("Read").Call(t.Context(), json.RawMessage(`{"file_path":"a.txt"}`), &actool.ToolContext{WorkingDir: dir, MaxOutputChars: 1000})
		if err == nil {
			t.Fatalf("%s: no error, result %q", mode, text(res))
		}
		if strings.Contains(err.Error()+text(res), "beta needle") {
			t.Fatalf("%s: the file was read in process: %v", mode, err)
		}
		if !strings.Contains(logs.String(), "[toolfs] Read:") {
			t.Errorf("%s: no [toolfs] log line:\n%s", mode, logs.String())
		}
		if mode == "fail" && !strings.Contains(err.Error(), "boom") {
			t.Errorf("the helper's message is lost: %v", err)
		}
		if len(e.starts(t)) != 1 {
			t.Errorf("%s: helper starts %v", mode, e.starts(t))
		}
	}
}

func TestHelperOutputOverTheCapIsAToolError(t *testing.T) {
	newToolfsEnv(t, "flood")
	old := toolfsMaxOutput
	toolfsMaxOutput = 50_000
	t.Cleanup(func() { toolfsMaxOutput = old })
	start := time.Now()
	_, err := wrapped("Grep").Call(t.Context(), json.RawMessage(`{"pattern":"x"}`), &actool.ToolContext{WorkingDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the overflow was not stopped")
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitDead(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		// a zombie still answers kill -0; read the state instead
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

// A helper that does not answer is killed with its whole process group after the timeout.
func TestHelperTimeoutKillsTheProcessGroup(t *testing.T) {
	e := newToolfsEnv(t, "sleep")
	old := toolfsTimeout
	toolfsTimeout = 400 * time.Millisecond
	t.Cleanup(func() { toolfsTimeout = old })
	logs := captureLog(t)
	start := time.Now()
	_, err := wrapped("Glob").Call(t.Context(), json.RawMessage(`{"pattern":"*"}`), &actool.ToolContext{WorkingDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("took %s", d)
	}
	b, rerr := os.ReadFile(e.pid)
	if rerr != nil {
		t.Fatalf("the helper never started its child: %v", rerr)
	}
	pid, _ := strconv.Atoi(string(b))
	waitDead(t, pid)
	if !strings.Contains(logs.String(), "[toolfs] Glob:") {
		t.Errorf("no log line:\n%s", logs.String())
	}
}

// A cancelled call (task paused or killed) ends the helper too.
func TestCancelledCallKillsTheHelper(t *testing.T) {
	e := newToolfsEnv(t, "sleep")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(e.pid); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	_, err := wrapped("LS").Call(ctx, json.RawMessage(`{}`), &actool.ToolContext{WorkingDir: t.TempDir()})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	b, _ := os.ReadFile(e.pid)
	pid, _ := strconv.Atoi(string(b))
	waitDead(t, pid)
}

// --- the helper itself ---

func runHelper(t *testing.T, req any) (toolfsResponse, error) {
	t.Helper()
	b, _ := json.Marshal(req)
	var out strings.Builder
	err := RunToolfs(strings.NewReader(string(b)), &out)
	if err != nil {
		return toolfsResponse{}, err
	}
	var resp toolfsResponse
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatalf("answer %q: %v", out.String(), err)
	}
	return resp, nil
}

func TestHelperRefusesWhatIsNotAFileTool(t *testing.T) {
	for _, name := range []string{"Bash", "WebFetch", "", "read", "Read\n", "../Read"} {
		if _, err := runHelper(t, map[string]any{"tool": name, "input": []byte("{}")}); err == nil {
			t.Errorf("%q was run", name)
		}
	}
	if _, err := runHelper(t, map[string]any{"tool": "Read", "input": []byte(`{"file_path":"x"}`), "extra": 1}); err == nil {
		t.Error("an unknown request field was accepted")
	}
	if err := RunToolfs(strings.NewReader("{not json"), io.Discard); err == nil {
		t.Error("a request that is not JSON was accepted")
	}
	old := toolfsMaxRequest
	toolfsMaxRequest = 20
	t.Cleanup(func() { toolfsMaxRequest = old })
	if err := RunToolfs(strings.NewReader(`{"tool":"Read","input":"`+strings.Repeat("a", 100)+`"}`), io.Discard); err == nil {
		t.Error("an oversized request was accepted")
	}
}

func TestHelperDoesNotReadDevicesOrHugeFiles(t *testing.T) {
	dir := t.TempDir()
	req := func(tool, path string) toolfsRequest {
		in, _ := json.Marshal(map[string]any{"file_path": path})
		return toolfsRequest{Tool: tool, Input: in, WorkingDir: dir, MaxOutputChars: 1000}
	}
	if _, err := os.Stat("/dev/zero"); err == nil {
		for _, tl := range []string{"Read", "Edit", "MultiEdit"} {
			resp, err := runHelper(t, req(tl, "/dev/zero"))
			if err != nil || !resp.IsError || !strings.Contains(strings.Join(resp.Texts, ""), "not a regular file") {
				t.Errorf("%s /dev/zero: %+v %v", tl, resp, err)
			}
		}
	}
	old := toolfsMaxFileSize
	toolfsMaxFileSize = 10
	t.Cleanup(func() { toolfsMaxFileSize = old })
	os.WriteFile(filepath.Join(dir, "big"), []byte(strings.Repeat("z", 11)), 0o644)
	os.WriteFile(filepath.Join(dir, "small"), []byte("ok"), 0o644)
	if resp, _ := runHelper(t, req("Read", "big")); !resp.IsError || !strings.Contains(strings.Join(resp.Texts, ""), "larger than") {
		t.Errorf("big: %+v", resp)
	}
	if resp, _ := runHelper(t, req("Read", "small")); resp.IsError {
		t.Errorf("small: %+v", resp)
	}
	// a path the check cannot examine is left to the tool, which says why
	if resp, _ := runHelper(t, req("Read", "missing")); !resp.IsError || !strings.Contains(strings.Join(resp.Texts, ""), "Error reading missing") {
		t.Errorf("missing: %+v", resp)
	}
}

func TestHelperDebugFieldNamesRipgrep(t *testing.T) {
	resp, err := runHelper(t, toolfsRequest{Tool: "LS", Input: []byte(`{}`), WorkingDir: t.TempDir(), MaxOutputChars: 100, Debug: true})
	if err != nil || resp.Rg == nil {
		t.Fatalf("%+v %v", resp, err)
	}
	resp, _ = runHelper(t, toolfsRequest{Tool: "LS", Input: []byte(`{}`), WorkingDir: t.TempDir(), MaxOutputChars: 100})
	if resp.Rg != nil {
		t.Error("the answer names ripgrep without the debug field")
	}
	// the server never sets the field
	b, _ := json.Marshal(toolfsRequest{Tool: "LS"})
	if strings.Contains(string(b), "debug") {
		t.Errorf("the request carries the debug field by default: %s", b)
	}
}

func TestHelperAnswerLargerThanTheCapIsAnError(t *testing.T) {
	old := toolfsMaxOutput
	toolfsMaxOutput = 10
	t.Cleanup(func() { toolfsMaxOutput = old })
	dir := fixture(t)
	resp, err := runHelper(t, toolfsRequest{Tool: "Grep", Input: []byte(`{"pattern":"needle","head_limit":0}`), WorkingDir: dir, MaxOutputChars: 100})
	if err != nil || !strings.Contains(resp.Error, "larger than") || len(resp.Texts) != 0 {
		t.Fatalf("%+v %v", resp, err)
	}
}

// --- the guard: every file tool in a guarded session runs in the helper ---

type countingTool struct {
	actool.CoreTool
	calls *int
}

func (c *countingTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	*c.calls++
	return c.CoreTool.Call(ctx, in, tc)
}

// opaqueTool hides its inner tool from the walk.
type opaqueTool struct {
	inner actool.CoreTool
	calls *int
}

func (o *opaqueTool) Name() string                       { return o.inner.Name() }
func (o *opaqueTool) Description() string                { return o.inner.Description() }
func (o *opaqueTool) Prompt() string                     { return o.inner.Prompt() }
func (o *opaqueTool) InputSchema() map[string]any        { return o.inner.InputSchema() }
func (o *opaqueTool) IsReadOnly(in json.RawMessage) bool { return o.inner.IsReadOnly(in) }
func (o *opaqueTool) IsConcurrencySafe(in json.RawMessage) bool {
	return o.inner.IsConcurrencySafe(in)
}
func (o *opaqueTool) CheckPermissions(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	return o.inner.CheckPermissions(ctx, in, pc)
}
func (o *opaqueTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	*o.calls++
	return o.inner.Call(ctx, in, tc)
}

// readThroughASession has a model ask for one Read of a.txt in a guarded session and returns the
// tool result text.
func readThroughASession(t *testing.T, tools []actool.CoreTool, dir string) string {
	t.Helper()
	p := &scriptedProvider{uses: []llm.ContentBlock{toolUse("u1", "Read", map[string]any{"file_path": "a.txt"})}}
	opts := agentcore.Options{Provider: p, Tools: tools, WorkingDir: dir, NonStreaming: true, MaxTurns: 3, PermissionMode: permission.ModeBypass}
	s, closeFn, err := newSession(t.Context(), opts, SessionInfo{Kind: "run", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	for _, err := range s.Prompt(t.Context(), "go") {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				var sb strings.Builder
				for _, c := range b.Content {
					sb.WriteString(c.Text)
				}
				return sb.String()
			}
		}
	}
	t.Fatal("no tool result")
	return ""
}

// With isolation on, the Read of a guarded session never runs in the server process, however the
// tool list was built: from defaultTools (decorators above the wrapper keep running), as the bare
// SDK tool, or behind a decorator the guard cannot see through (wrapped from the outside, logged).
func TestGuardedSessionsRunFileToolsInTheHelper(t *testing.T) {
	counted, opaque := 0, 0
	build := map[string]func() []actool.CoreTool{
		"defaultTools decorated": func() []actool.CoreTool {
			ts := defaultTools()
			for i, tl := range ts {
				ts[i] = &countingTool{CoreTool: tl, calls: &counted}
			}
			return ts
		},
		"bare SDK tools": func() []actool.CoreTool { return actool.DefaultTools() },
		"opaque decorator": func() []actool.CoreTool {
			ts := actool.DefaultTools()
			for i, tl := range ts {
				ts[i] = &opaqueTool{inner: tl, calls: &opaque}
			}
			return ts
		},
	}
	for name, mk := range build {
		e := newToolfsEnv(t, "")
		logs := captureLog(t)
		dir := fixture(t)
		got := readThroughASession(t, mk(), dir)
		if !strings.Contains(got, "beta needle") {
			t.Fatalf("%s: result %q", name, got)
		}
		if s := e.starts(t); len(s) != 1 {
			t.Errorf("%s: the helper started %d times: %q", name, len(s), s)
		}
		switch name {
		case "defaultTools decorated":
			if counted != 1 {
				t.Errorf("the decorator above the wrapper ran %d times", counted)
			}
			if strings.Contains(logs.String(), "was not built through defaultTools") {
				t.Errorf("a properly built list was wrapped again:\n%s", logs.String())
			}
		default:
			if !strings.Contains(logs.String(), "[isolation] file tool Read was not built through defaultTools") {
				t.Errorf("%s: no log line:\n%s", name, logs.String())
			}
		}
	}
	if opaque != 0 {
		t.Errorf("the opaque decorator's Call ran %d times; the outer wrapper replaces it", opaque)
	}
}

// A list built through defaultTools is kept as it is, and without isolation the guard leaves even a
// bare list alone: the file tools run in process, as before.
func TestGuardLeavesTheListAloneWhenThereIsNothingToChange(t *testing.T) {
	ts := defaultTools()
	got := isolateSessionTools(ts)
	if len(got) != len(ts) {
		t.Fatal("the list changed length")
	}
	for i := range ts {
		if got[i] != ts[i] {
			t.Errorf("%s was replaced", ts[i].Name())
		}
	}
	e := newToolfsEnv(t, "fail")
	sandbox.Set(nil)
	logs := captureLog(t)
	if res := readThroughASession(t, actool.DefaultTools(), fixture(t)); !strings.Contains(res, "beta needle") || len(e.starts(t)) != 0 {
		t.Fatalf("result %q starts %v", res, e.starts(t))
	}
	if strings.Contains(logs.String(), "[isolation] file tool") {
		t.Errorf("the guard wrapped tools without isolation:\n%s", logs.String())
	}
}

func TestInnermostToolSeesThroughDecorators(t *testing.T) {
	calls := 0
	leaf := &isolatedFileTool{CoreTool: actool.NewRead()}
	chain := &countingTool{CoreTool: &countingTool{CoreTool: leaf, calls: &calls}, calls: &calls}
	if innermostTool(chain) != actool.CoreTool(leaf) {
		t.Error("did not find the wrapper under two decorators")
	}
	bare := actool.NewRead()
	if innermostTool(bare) != bare {
		t.Error("a bare tool is its own innermost")
	}
	op := &opaqueTool{inner: leaf, calls: &calls}
	if innermostTool(op) != actool.CoreTool(op) {
		t.Error("a decorator that hides its tool was seen through")
	}
}

// The guard wraps the seven file tools by name and nothing else: the shells (Bash, Monitor, the
// background-task tools) already start through the drop chain, and the task tools take a task id,
// never a path.
func TestGuardWrapsOnlyTheFileTools(t *testing.T) {
	ran := 0
	var list []actool.CoreTool
	for _, n := range []string{"Bash", "Monitor", "TaskOutput", "TaskStop", "TaskList", "Skill", "mcp__srv__read", "ReadFile", "read"} {
		list = append(list, recordTool(n, &ran))
	}
	if got := isolateSessionTools(list); len(got) != len(list) {
		t.Fatal("the list changed")
	} else {
		for i := range list {
			if got[i] != list[i] {
				t.Errorf("%s was wrapped", list[i].Name())
			}
		}
	}
	// one file tool among them is wrapped, the rest are the same objects
	withRead := append([]actool.CoreTool{actool.NewRead()}, list...)
	got := isolateSessionTools(withRead)
	if _, ok := got[0].(*isolatedFileTool); !ok {
		t.Fatalf("Read was not wrapped: %T", got[0])
	}
	for i := range list {
		if got[i+1] != list[i] {
			t.Errorf("%s was replaced", list[i].Name())
		}
	}
	if _, ok := withRead[0].(*isolatedFileTool); ok {
		t.Error("the caller's list was modified")
	}
}

// The wrapper sits below the decorators a session's tools get (the tools table's default injection,
// usage metering, the Tool RAG call counter): a wrapped file tool still passes through each of them,
// and only the place the code runs changes.
func TestWrappedFileToolKeepsTheDecoratorsAbove(t *testing.T) {
	e := newToolfsEnv(t, "")
	dir := fixture(t)
	metered := 0
	obs := &toolObservation{Classes: map[string]string{"Read": toolClassKeep}}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"file_path": map[string]any{"type": "string", "default": "a.txt"},
	}}
	var chain actool.CoreTool = DecorateTool(wrapped("Read"), "", schema)
	chain = &countingTool{CoreTool: chain, calls: &metered}
	chain = &guardedTool{CoreTool: chain, obs: obs}
	if got := chain.InputSchema()["properties"].(map[string]any)["file_path"].(map[string]any)["default"]; got != "a.txt" {
		t.Fatalf("the schema default is %v", got)
	}

	// the model leaves file_path out: the default is injected above the wrapper and the helper reads it
	res := runSessionWith(t, []actool.CoreTool{chain}, agentcore.Options{WorkingDir: dir}, SessionInfo{Kind: "run", ID: "s1"},
		toolUse("u1", "Read", map[string]any{}))
	if len(res) != 1 || !strings.Contains(res[0], "beta needle") {
		t.Fatalf("results %q", res)
	}
	if s := e.starts(t); len(s) != 1 {
		t.Errorf("the helper started %d times: %q", len(s), s)
	}
	if metered != 1 {
		t.Errorf("the metering decorator ran %d times", metered)
	}
	if n := obs.row(RunInfo{}).Called["Read"].N; n != 1 {
		t.Errorf("the Tool RAG call counter saw %d calls", n)
	}
}

// A Grep whose output is over the budget spills in a guarded session just as it does in process, and
// the pointer's file is readable by the tool user (grep and sed through Bash) once the hook has run.
func TestGrepSpillOfAGuardedSessionIsReadable(t *testing.T) {
	newToolfsEnv(t, "")
	dir := t.TempDir()
	for i := 0; i < 1500; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%04d.txt", i)), []byte("a needle in file\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data := t.TempDir()
	spill, err := sessionSpillDir(data, "worker", "w9")
	if err != nil {
		t.Fatal(err)
	}
	type seen struct {
		path, grep, sed string
		mode            os.FileMode
	}
	var got []seen
	remove := registerPostToolUse("test-grep-spill", func(_ context.Context, _ SessionInfo, tl string, _, result []byte, _ bool) {
		var s string
		if json.Unmarshal(result, &s) != nil || tl != "Grep" {
			return
		}
		p := spillPath(s)
		if p == "" {
			return
		}
		// what the tool user does with the pointer, as this user: the file must allow it
		g, _ := exec.Command("grep", "-c", "needle", p).CombinedOutput()
		sd, _ := exec.Command("sed", "-n", "1p", p).CombinedOutput()
		got = append(got, seen{path: p, grep: strings.TrimSpace(string(g)), sed: strings.TrimSpace(string(sd)), mode: modeOf(t, p)})
	})
	defer remove()

	res := runSessionWith(t, defaultTools(), agentcore.Options{WorkingDir: dir, ToolOutputDir: spill, SessionID: "w9"}, SessionInfo{Kind: "worker", ID: "w9"},
		toolUse("u1", "Grep", map[string]any{"pattern": "needle", "output_mode": "content", "head_limit": 0}))
	if len(res) != 1 || !strings.Contains(res[0], "<persisted-output>") {
		n := len(res)
		if n > 0 {
			n = len(res[0])
		}
		t.Fatalf("no spill pointer in the result (%d results, first %d bytes)", len(res), n)
	}
	if len(got) != 1 {
		t.Fatalf("the hook saw %d spill pointers", len(got))
	}
	if filepath.Dir(got[0].path) != spill {
		t.Errorf("the spill file %s is not in the session's spill directory %s", got[0].path, spill)
	}
	if got[0].mode != 0o644 {
		t.Errorf("the spill file is %o", got[0].mode)
	}
	if n, _ := strconv.Atoi(got[0].grep); n < 1500 || !strings.Contains(got[0].sed, "needle") {
		t.Errorf("grep counted %q, sed printed %q", got[0].grep, got[0].sed)
	}
}

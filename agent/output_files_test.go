//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/sandbox"
)

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// --- the spill directory ---

func TestSessionSpillDirIsTheServersAndOutOfTheToolUsersReach(t *testing.T) {
	data := t.TempDir()
	old := syscall.Umask(0o077) // a strict umask must not make the directories unreadable
	defer syscall.Umask(old)
	dir, err := sessionSpillDir(data, "worker", "exp1-worker-i2")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(data, "spill", "worker", "exp1-worker-i2") {
		t.Fatalf("dir %s", dir)
	}
	for p, want := range map[string]os.FileMode{
		filepath.Join(data, "spill"): 0o711, filepath.Join(data, "spill", "worker"): 0o755, dir: 0o755,
	} {
		if got := modeOf(t, p); got != want {
			t.Errorf("%s: mode %o, want %o", p, got, want)
		}
		// nothing the tool user could use to plant a link: no write bit for group or others
		if modeOf(t, p)&0o022 != 0 {
			t.Errorf("%s can be written by others", p)
		}
	}
	// a second call changes nothing; a loosened directory is tightened again
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if again, err := sessionSpillDir(data, "worker", "exp1-worker-i2"); err != nil || again != dir {
		t.Fatalf("%v %s", err, again)
	}
	if got := modeOf(t, dir); got != 0o755 {
		t.Errorf("a loosened directory was left at %o", got)
	}
	if os.Geteuid() == 0 {
		// owned by root even when it was created by someone else
		if err := os.Chown(dir, 30002, 30002); err != nil {
			t.Fatal(err)
		}
		if _, err := sessionSpillDir(data, "worker", "exp1-worker-i2"); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Lstat(dir)
		if uid, _ := ownerUID(fi); uid != 0 {
			t.Errorf("owner %d", uid)
		}
	}
}

// A link where a directory belongs, planted by a tool, is refused: the server writes nothing
// through it.
func TestSessionSpillDirRefusesLinksAndBadNames(t *testing.T) {
	for _, plant := range []string{"spill", "spill/chat", "spill/chat/c1"} {
		data := t.TempDir()
		elsewhere := t.TempDir()
		before := modeOf(t, elsewhere)
		if err := os.MkdirAll(filepath.Join(data, filepath.Dir(plant)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(data, plant)); err != nil {
			t.Fatal(err)
		}
		if _, err := sessionSpillDir(data, "chat", "c1"); err == nil {
			t.Errorf("a link at %s was followed", plant)
		}
		if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
			t.Errorf("a link at %s: the server created %v in the link's target", plant, ents)
		}
		if got := modeOf(t, elsewhere); got != before {
			t.Errorf("a link at %s: the target's mode was changed from %o to %o", plant, before, got)
		}
	}
	// a link that stays inside the data directory (os.Root follows those): the directory it points
	// at, here a workspace, must not be changed or filled
	for _, plant := range []string{"spill", "spill/chat", "spill/chat/c1"} {
		data := t.TempDir()
		ws := filepath.Join(data, "tasks", "1")
		if err := os.MkdirAll(ws, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(data, filepath.Dir(plant)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(data, "tasks", "1"), filepath.Join(data, plant)); err != nil {
			t.Fatal(err)
		}
		if _, err := sessionSpillDir(data, "chat", "c1"); err == nil {
			t.Errorf("an inner link at %s was followed", plant)
		}
		if ents, _ := os.ReadDir(ws); len(ents) != 0 {
			t.Errorf("an inner link at %s: the server created %v in the workspace", plant, ents)
		}
		if got := modeOf(t, ws); got != 0o700 {
			t.Errorf("an inner link at %s: the workspace's mode became %o", plant, got)
		}
	}
	// a file in the way
	data := t.TempDir()
	os.WriteFile(filepath.Join(data, "spill"), []byte("x"), 0o644)
	if _, err := sessionSpillDir(data, "chat", "c1"); err == nil {
		t.Error("a file in the way was accepted")
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "-x", "a\x00b", strings.Repeat("a", 200)} {
		if _, err := sessionSpillDir(t.TempDir(), "chat", bad); err == nil {
			t.Errorf("id %q was accepted", bad)
		}
		if _, err := sessionSpillDir(t.TempDir(), bad, "c1"); err == nil {
			t.Errorf("kind %q was accepted", bad)
		}
	}
	if _, err := sessionSpillDir(filepath.Join(t.TempDir(), "missing"), "chat", "c1"); err == nil {
		t.Error("a missing data directory was accepted")
	}
}

func TestCheckSpillDir(t *testing.T) {
	data := t.TempDir()
	info := SessionInfo{Kind: "worker", ID: "w1"}
	good, err := sessionSpillDir(data, "worker", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSpillDir(good, info); err != nil {
		t.Fatalf("a spill directory made by sessionSpillDir: %v", err)
	}
	if err := checkSpillDir("", info); err != nil {
		t.Fatalf("no spill directory: %v", err)
	}
	other, _ := sessionSpillDir(data, "worker", "w2")
	for name, dir := range map[string]string{
		"another session": other,
		"the workspace":   filepath.Join(t.TempDir(), "cmd-output"),
		"relative":        "spill/worker/w1",
		"missing":         filepath.Join(data, "spill", "worker", "w1", "x"),
	} {
		if err := checkSpillDir(dir, info); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := checkSpillDir(good, SessionInfo{Kind: "chat", ID: "w1"}); err == nil {
		t.Error("another kind was accepted")
	}
	// writable by others, at any of the three levels
	for _, d := range []string{good, filepath.Dir(good), filepath.Dir(filepath.Dir(good))} {
		fi, _ := os.Lstat(d)
		os.Chmod(d, 0o777)
		if err := checkSpillDir(good, info); err == nil {
			t.Errorf("%s writable by others was accepted", d)
		}
		os.Chmod(d, fi.Mode().Perm())
	}
	if os.Geteuid() == 0 {
		os.Chown(good, sandbox.ToolUID, sandbox.ToolGID)
		if err := checkSpillDir(good, info); err == nil {
			t.Error("a directory owned by the tool user was accepted")
		}
		os.Chown(good, 0, 0)
	}
	// a link in place of the directory
	link := filepath.Join(data, "spill", "worker", "w3")
	os.Symlink(good, link)
	if err := checkSpillDir(link, SessionInfo{Kind: "worker", ID: "w3"}); err == nil {
		t.Error("a link was accepted")
	}
}

// With isolation on, a session whose spill directory is not its own, or can be written by others, is
// not started, and takes no lease.
func TestGuardRefusesAnUnsafeSpillDirectory(t *testing.T) {
	iso := activateForTest(t, goodPlatform())
	data := t.TempDir()
	info := SessionInfo{Kind: "worker", ID: "w1"}
	good, _ := sessionSpillDir(data, "worker", "w1")
	opts := func(dir string) agentcore.Options {
		return agentcore.Options{Provider: &scriptedProvider{}, Tools: []actool.CoreTool{}, ToolOutputDir: dir}
	}
	if _, closeFn, err := newSession(t.Context(), opts(good), info); err != nil {
		t.Fatal(err)
	} else {
		closeFn()
	}
	os.Chmod(good, 0o777)
	for name, dir := range map[string]string{"writable": good, "workspace": filepath.Join(data, "tasks", "1", "cmd-output")} {
		if _, closeFn, err := newSession(t.Context(), opts(dir), info); err == nil || closeFn != nil {
			t.Errorf("%s: the session was started (%v)", name, err)
		}
	}
	if iso.Leases().Held() != 0 {
		t.Errorf("a refused session holds %d leases", iso.Leases().Held())
	}
}

// --- the hook ---

// hookRoots builds the roots of a session for direct calls of the hook.
func hookInfo(spill, noa, id string) SessionInfo {
	r := &outputRoots{spill: spill, noa: noa}
	r.setSessionID(id)
	return SessionInfo{Kind: "worker", ID: id, NoaDir: noa, roots: r}
}

func jsonText(s string) []byte {
	b, _ := json.Marshal(s) // like the harness: '<' becomes <
	return b
}

func write0600(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func spillNotice(p string) string {
	return "head\n\n... <persisted-output>[Output too large: full 90000 bytes / 2000 lines.Full output saved to  " + p + " </persisted-output>"
}

// AC11: the spill file named in a result becomes readable; another session's, a link, a directory
// and a path outside are not touched.
func TestHookMakesThisSessionsSpillFileReadable(t *testing.T) {
	data := t.TempDir()
	mine, _ := sessionSpillDir(data, "worker", "w1")
	theirs, _ := sessionSpillDir(data, "worker", "w2")
	secret := write0600(t, filepath.Join(t.TempDir(), "secret"))
	info := hookInfo(mine, "", "w1")
	logs := captureLog(t)

	f := write0600(t, filepath.Join(mine, "output-1-1.txt"))
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(f)), false)
	if got := modeOf(t, f); got != 0o644 {
		t.Fatalf("the spill file is %o", got)
	}

	other := write0600(t, filepath.Join(theirs, "output-1-2.txt"))
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(other)), false)
	if got := modeOf(t, other); got != 0o600 {
		t.Errorf("another session's spill file was changed to %o", got)
	}
	if !strings.Contains(logs.String(), "[output-files] session worker/w1: ignored a path outside") {
		t.Errorf("no log line:\n%s", logs.String())
	}

	// a link inside the directory, to a file the tool user must not read, is not followed
	link := filepath.Join(mine, "output-1-3.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(link)), false)
	if got := modeOf(t, secret); got != 0o600 {
		t.Errorf("a link's target was changed to %o", got)
	}
	// a path that goes out through ..
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(filepath.Join(mine, "..", "..", "..", filepath.Base(filepath.Dir(secret)), "secret"))), false)
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(secret)), false)
	if got := modeOf(t, secret); got != 0o600 {
		t.Errorf("a path outside the directory was changed to %o", got)
	}
	// a directory is not changed, and neither is the directory itself
	sub := filepath.Join(mine, "sub")
	os.Mkdir(sub, 0o700)
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(sub)), false)
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice(mine)), false)
	if modeOf(t, sub) != 0o700 || modeOf(t, mine) != 0o755 {
		t.Errorf("a directory was changed: %o %o", modeOf(t, sub), modeOf(t, mine))
	}
	// relative paths are not resolved against anything
	readableOutputFiles(t.Context(), info, "Bash", nil, jsonText(spillNotice("output-1-1.txt")), false)
	// the log line appears once per session
	if n := strings.Count(logs.String(), "[output-files]"); n != 1 {
		t.Errorf("%d log lines, want 1:\n%s", n, logs.String())
	}
}

// The harness hands the hook the result as a JSON string, in which '<' is written <; a hook that
// searched the raw bytes would find no pointer.
func TestHookReadsTheResultAsJSON(t *testing.T) {
	data := t.TempDir()
	mine, _ := sessionSpillDir(data, "worker", "w1")
	f := write0600(t, filepath.Join(mine, "output-9-9.txt"))
	enc := jsonText(spillNotice(f))
	if bs := string(rune(92)); !strings.Contains(string(enc), bs+"u003c") {
		t.Fatalf("the fixture is not escaped: %s", enc)
	}
	readableOutputFiles(t.Context(), hookInfo(mine, "", "w1"), "Bash", nil, enc, false)
	if got := modeOf(t, f); got != 0o644 {
		t.Fatalf("mode %o", got)
	}
}

func TestHookWithoutRootsOrMarkersChangesNothing(t *testing.T) {
	data := t.TempDir()
	mine, _ := sessionSpillDir(data, "worker", "w1")
	f := write0600(t, filepath.Join(mine, "output-1-1.txt"))
	readableOutputFiles(t.Context(), SessionInfo{Kind: "x", ID: "y"}, "Bash", nil, jsonText(spillNotice(f)), false)
	readableOutputFiles(t.Context(), hookInfo(mine, "", "w1"), "Bash", nil, jsonText("plain output "+f), false)
	readableOutputFiles(t.Context(), hookInfo(mine, "", "w1"), "Bash", nil, []byte(`not a json string`), false)
	if got := modeOf(t, f); got != 0o600 {
		t.Errorf("mode %o", got)
	}
}

// AC42: a noa archive file named in a result is readable; a file outside the session's noa directory
// is not touched.
func TestHookMakesNoaArchivesReadable(t *testing.T) {
	data := t.TempDir()
	noa := noaSessionDir(data, "exp1-worker-i2")
	info := hookInfo("", noa, "exp1-worker-i2")
	arch := write0600(t, filepath.Join(noa, "tier1", "b1_m1-m9.md"))
	other := write0600(t, filepath.Join(noaSessionDir(data, "exp9-worker-i1"), "tier1", "b1.md"))
	res := "compressed\n  b1 tier 1 range m1-m9  " + arch + "\n<noa-archive block=\"b1\" path=\"" + other + "\"/>"
	readableOutputFiles(t.Context(), info, "Compress", nil, jsonText(res), false)
	if got := modeOf(t, arch); got != 0o644 {
		t.Errorf("the archive is %o", got)
	}
	if got := modeOf(t, other); got != 0o600 {
		t.Errorf("another session's archive was changed to %o", got)
	}
	// a tag with the path inside quotes
	arch2 := write0600(t, filepath.Join(noa, "tier2", "b2.md"))
	readableOutputFiles(t.Context(), info, "Compress", nil, jsonText(`<noa-archive block="b2" path="`+arch2+`"/>`), false)
	if got := modeOf(t, arch2); got != 0o644 {
		t.Errorf("the archive named in a tag is %o", got)
	}
}

// runSessionWith runs a guarded session where the model makes the given tool calls in turn and
// returns the tool result texts. A test hook, registered after the real one, sees each result.
func runSessionWith(t *testing.T, tools []actool.CoreTool, opts agentcore.Options, info SessionInfo, uses ...llm.ContentBlock) []string {
	t.Helper()
	p := &scriptedProvider{uses: uses}
	opts.Provider, opts.Tools = p, tools
	opts.NonStreaming, opts.MaxTurns, opts.PermissionMode = true, 3, permission.ModeBypass
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
	var out []string
	for _, m := range s.Messages() {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				var sb strings.Builder
				for _, c := range b.Content {
					sb.WriteString(c.Text)
				}
				out = append(out, sb.String())
			}
		}
	}
	return out
}

// A tool whose output is over the budget, in a real session: the harness spills it, and by the time
// the next step starts the file is readable. Control: the SDK alone leaves it 0600.
func TestSpillFileOfARealSessionIsReadable(t *testing.T) {
	data := t.TempDir()
	spill, err := sessionSpillDir(data, "worker", "w1")
	if err != nil {
		t.Fatal(err)
	}
	big := actool.Build(actool.Spec{Name: "Big", Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Text(strings.Repeat("0123456789\n", 6000)), nil
		}})

	// control: what the SDK makes of an over-budget output
	ctl := t.TempDir()
	actool.Capture(&actool.ToolContext{MaxOutputChars: 100, OutputDir: ctl}, strings.Repeat("x", 1000))
	ents, _ := os.ReadDir(ctl)
	if len(ents) != 1 || modeOf(t, filepath.Join(ctl, ents[0].Name())) != 0o600 {
		t.Fatalf("control: %v", ents)
	}

	// the session removes its spill directory when it ends, so the file is examined while it runs
	var seen []os.FileMode
	var size int
	var mu sync.Mutex
	remove := registerPostToolUse("test-observer", func(_ context.Context, s SessionInfo, tl string, _, result []byte, _ bool) {
		var text string
		json.Unmarshal(result, &text)
		if p := spillPath(text); p != "" {
			mu.Lock()
			seen = append(seen, modeOf(t, p))
			b, _ := os.ReadFile(p)
			size = len(b)
			mu.Unlock()
		}
	})
	defer remove()

	res := runSessionWith(t, []actool.CoreTool{big}, agentcore.Options{ToolOutputDir: spill, SessionID: "w1"}, SessionInfo{Kind: "worker", ID: "w1"},
		toolUse("u1", "Big", map[string]any{}))
	if len(res) != 1 || !strings.Contains(res[0], "<persisted-output>") {
		t.Fatalf("results %q", res)
	}
	p := spillPath(res[0])
	if filepath.Dir(p) != spill {
		t.Fatalf("the file %s is not in the session's spill directory %s", p, spill)
	}
	if len(seen) != 1 || seen[0] != 0o644 {
		t.Errorf("the registry hooks that ran after the readability hook saw %v", seen)
	}
	if size != 66000 {
		t.Errorf("the spilled file holds %d bytes", size)
	}
}

// AC12: the output file of a background Bash command and of Monitor is readable once the tool has
// returned.
func TestBackgroundOutputFilesAreReadable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var mu sync.Mutex
	modes := map[string]os.FileMode{}
	remove := registerPostToolUse("test-bg", func(_ context.Context, s SessionInfo, tl string, _, result []byte, _ bool) {
		var text string
		json.Unmarshal(result, &text)
		if i := strings.Index(text, "Output file: "); i >= 0 {
			p := strings.SplitN(text[i+len("Output file: "):], "\n", 2)[0]
			mu.Lock()
			modes[tl+" "+p] = modeOf(t, p)
			mu.Unlock()
		}
	})
	defer remove()
	data := t.TempDir()
	spill, _ := sessionSpillDir(data, "chat", "c1")
	work := t.TempDir()
	res := runSessionWith(t, []actool.CoreTool{actool.NewBash()}, agentcore.Options{ToolOutputDir: spill, SessionID: "c1", WorkingDir: work},
		SessionInfo{Kind: "chat", ID: "c1"},
		toolUse("u1", "Bash", map[string]any{"command": "echo bg; sleep 1", "run_in_background": true}),
		toolUse("u2", "Monitor", map[string]any{"command": "echo mon; sleep 1"}))
	if len(res) != 2 {
		t.Fatalf("results %q", res)
	}
	if len(modes) != 2 {
		t.Fatalf("saw %v from %q", modes, res)
	}
	for k, m := range modes {
		if m != 0o644 {
			t.Errorf("%s: mode %o", k, m)
		}
		if !strings.Contains(k, filepath.Join(tmp, "norma", "c1", "tasks")) {
			t.Errorf("%s: not under the session's task directory", k)
		}
	}
}

// A result that names a task output of another session is left alone.
func TestHookLeavesAnotherSessionsTaskOutputAlone(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	mine := write0600(t, filepath.Join(tmp, "norma", "c1", "tasks", "task_1.output"))
	theirs := write0600(t, filepath.Join(tmp, "norma", "c2", "tasks", "task_1.output"))
	info := hookInfo("", "", "c1")
	for _, p := range []string{mine, theirs} {
		readableOutputFiles(t.Context(), info, "Bash", nil, jsonText("Running in background as task_1. Output file: "+p+"\nUse TaskOutput"), false)
	}
	if modeOf(t, mine) != 0o644 || modeOf(t, theirs) != 0o600 {
		t.Errorf("modes %o %o", modeOf(t, mine), modeOf(t, theirs))
	}
}

// The hook is on every session, including one built without hooks of its own, and through the real
// guard.
func TestOutputFilesHookIsRegistered(t *testing.T) {
	postToolUse.RLock()
	defer postToolUse.RUnlock()
	for _, h := range postToolUse.hooks {
		if h.name == "output-files" {
			return
		}
	}
	t.Fatal("the output-files hook is not registered")
}

var _ = sandbox.ToolUID

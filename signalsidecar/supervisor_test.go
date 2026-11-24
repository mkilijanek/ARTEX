package signalsidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testNumber = "+15555550101"
	testGroup  = "dGhpcy1pcy1hLXRlc3QtZ3JvdXAtaWQtMzItYnl0ZXM="
)

// fakeBin is the compiled stand-in for signal-cli, built once per test.
func fakeBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakecli")
	out, err := exec.Command("go", "build", "-o", bin, "./testdata/fakecli").CombinedOutput()
	if err != nil {
		t.Fatalf("building the fake CLI: %v\n%s", err, out)
	}
	return bin
}

// rig is one scripted child: scenario file, data directory and record files.
type rig struct {
	t        *testing.T
	bin      string
	dir      string
	data     string
	scenario string
	calls    string
	starts   string
	dump     string
	logs     *logSink
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newRig(t *testing.T, sc map[string]any) *rig {
	t.Helper()
	r := &rig{t: t, bin: fakeBin(t), dir: t.TempDir(), logs: &logSink{}}
	r.data = filepath.Join(r.dir, "data")
	if err := os.Mkdir(r.data, 0o700); err != nil {
		t.Fatal(err)
	}
	r.scenario = filepath.Join(r.dir, "scenario.json")
	r.calls = filepath.Join(r.dir, "calls")
	r.starts = filepath.Join(r.dir, "starts")
	r.dump = filepath.Join(r.dir, "dump.json")
	r.set(sc)
	return r
}

// set replaces the scenario; a child reads it when it starts.
func (r *rig) set(sc map[string]any) {
	r.t.Helper()
	m := map[string]any{"calls_file": r.calls, "starts_file": r.starts, "dump_file": r.dump}
	for k, v := range sc {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(r.scenario, raw, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) seedAccounts(n ...string) {
	r.t.Helper()
	var acc []Account
	for _, x := range n {
		acc = append(acc, Account{Number: x, ACI: "11111111-2222-4333-8444-555555555555"})
	}
	raw, _ := json.Marshal(acc)
	if err := os.WriteFile(filepath.Join(r.data, "accounts.json"), raw, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) config() Config {
	return Config{
		Command:      r.bin,
		Args:         []string{"-c", r.data, "--scenario", r.scenario},
		Env:          ChildEnv("/nonexistent", "/usr/bin", "C", "/nonexistent"),
		CallTimeout:  2 * time.Second,
		StartTimeout: 5 * time.Second,
		MinBackoff:   50 * time.Millisecond,
		MaxBackoff:   200 * time.Millisecond,
		StopGrace:    3 * time.Second,
		TermGrace:    3 * time.Second,
		Logf:         r.logs.logf,
	}
}

// run starts a supervisor and returns it with a stop function that ends Run
// and waits for it.
func (r *rig) run(cfg Config) (*Supervisor, func()) {
	r.t.Helper()
	s := NewSupervisor(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					r.t.Errorf("Run: %v", err)
				}
			case <-time.After(15 * time.Second):
				r.t.Error("Run did not return")
			}
		})
	}
	r.t.Cleanup(stop)
	return s, stop
}

func (r *rig) waitUp(s *Supervisor) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.WaitUp(ctx); err != nil {
		r.t.Fatalf("child not up: %v (%+v)\n%s", err, s.Status(), r.logs.all())
	}
}

func (r *rig) recordedMethods() []string {
	raw, _ := os.ReadFile(r.calls)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l == "" {
			continue
		}
		var c struct{ Method string }
		_ = json.Unmarshal([]byte(l), &c)
		out = append(out, c.Method)
	}
	return out
}

func (r *rig) startTimes() (pids []int, at []time.Time) {
	raw, _ := os.ReadFile(r.starts)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		n, _ := strconv.ParseInt(f[1], 10, 64)
		pids = append(pids, p)
		at = append(at, time.Unix(0, n))
	}
	return
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestJavaCommandIsTheFixedCommandLine(t *testing.T) {
	cmd, args := JavaCommand("/opt/java/bin/java", "/opt/signal-cli/native", "/opt/signal-cli/lib", "/data")
	want := []string{
		"--enable-native-access=ALL-UNNAMED",
		"-Djava.library.path=/opt/signal-cli/native",
		"-Dorg.sqlite.lib.path=/opt/signal-cli/native",
		"-Dorg.sqlite.lib.name=libsqlitejdbc.so",
		"-cp", "/opt/signal-cli/lib/*",
		"org.asamk.signal.Main",
		"-c", "/data",
		"--scrub-log", "jsonRpc",
		"--receive-mode", "on-start",
		"--ignore-attachments", "--ignore-stories", "--ignore-avatars", "--ignore-stickers",
	}
	if cmd != "/opt/java/bin/java" || strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("command %q args %q", cmd, args)
	}
}

func TestChildEnvHoldsFourVariablesOnly(t *testing.T) {
	env := ChildEnv("/h", "/p", "C.UTF-8", "/t")
	if strings.Join(env, ",") != "HOME=/h,PATH=/p,LANG=C.UTF-8,TMPDIR=/t" {
		t.Fatalf("%q", env)
	}
}

func TestChildGetsOnlyTheGivenEnvironmentAndTheFixedArguments(t *testing.T) {
	t.Setenv("ARTEX_SIGNAL_PG_DSN", "postgres://user:SECRET-DSN@db/x")
	t.Setenv("SOME_OTHER_VARIABLE", "inherited")
	r := newRig(t, nil)
	cfg := r.config()
	s, _ := r.run(cfg)
	r.waitUp(s)

	raw, err := os.ReadFile(r.dump)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Args []string `json:"args"`
		Env  []string `json:"env"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.Env, ",") != strings.Join(cfg.Env, ",") {
		t.Fatalf("child environment %q, want %q", d.Env, cfg.Env)
	}
	if strings.Join(d.Args, "\x00") != strings.Join(cfg.Args, "\x00") {
		t.Fatalf("child args %q", d.Args)
	}
}

func TestNilEnvMeansAnEmptyEnvironmentNotTheParentOne(t *testing.T) {
	t.Setenv("ARTEX_SIGNAL_PG_DSN", "postgres://user:SECRET-DSN@db/x")
	r := newRig(t, nil)
	cfg := r.config()
	cfg.Env = nil
	s, _ := r.run(cfg)
	r.waitUp(s)
	raw, _ := os.ReadFile(r.dump)
	if strings.Contains(string(raw), "SECRET-DSN") || strings.Contains(string(raw), "PATH=") {
		t.Fatalf("parent environment leaked: %s", raw)
	}
}

func TestRunReportsVersionAndAnswersCalls(t *testing.T) {
	r := newRig(t, nil)
	s, _ := r.run(r.config())
	r.waitUp(s)
	if st := s.Status(); !st.Up || st.Version != "0.14.10" || st.Starts != 1 {
		t.Fatalf("status %+v", st)
	}
	accts, err := s.ListAccounts(ctx5(t))
	if err != nil || len(accts) != 0 {
		t.Fatalf("accounts %v %v", accts, err)
	}
}

func TestLinkFlowWithAFakeThatCompletes(t *testing.T) {
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "after", "after_ms": 100, "number": testNumber}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	uri, err := s.StartLink(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "sgnl://linkdevice?uuid=") || !strings.Contains(uri, "&pub_key=") || !strings.Contains(uri, "&capabilities=a,b") {
		t.Fatalf("uri %q", uri)
	}
	acc, err := s.FinishLink(ctx5(t), uri, "ARTEX")
	if err != nil || acc.Number != testNumber {
		t.Fatalf("finishLink %+v %v", acc, err)
	}
	accts, _ := s.ListAccounts(ctx5(t))
	if len(accts) != 1 || accts[0].Number != testNumber {
		t.Fatalf("accounts %v", accts)
	}
	// A link URI is good for one finishLink.
	_, err = s.FinishLink(ctx5(t), uri, "ARTEX")
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -1 || !strings.Contains(re.Message, "Unknown device link uri") {
		t.Fatalf("reused uri: %v", err)
	}
}

func TestFinishLinkTimeoutIsAUserError(t *testing.T) {
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "timeout", "after_ms": 50}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	uri, _ := s.StartLink(ctx5(t))
	_, err := s.FinishLink(ctx5(t), uri, "ARTEX")
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -1 || !strings.Contains(re.Message, "timed out") {
		t.Fatalf("got %v", err)
	}
}

func TestNoCallIsMadeWhileFinishLinkIsPending(t *testing.T) {
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "never"}})
	r.seedAccounts(testNumber)
	s, _ := r.run(r.config())
	r.waitUp(s)
	uri, err := s.StartLink(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { _, err := s.FinishLink(fctx, uri, "ARTEX"); res <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for !contains(r.recordedMethods(), "finishLink") {
		if time.Now().After(deadline) {
			t.Fatal("finishLink never reached the child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := len(r.recordedMethods())
	if _, err := s.Version(ctx5(t)); !errors.Is(err, ErrLinkPending) {
		t.Errorf("version: %v", err)
	}
	if _, err := s.ListGroups(ctx5(t), testNumber); !errors.Is(err, ErrLinkPending) {
		t.Errorf("listGroups: %v", err)
	}
	if _, err := s.Send(ctx5(t), testNumber, "g", "m"); !errors.Is(err, ErrLinkPending) {
		t.Errorf("send: %v", err)
	}
	if _, err := s.StartLink(ctx5(t)); !errors.Is(err, ErrLinkPending) {
		t.Errorf("startLink: %v", err)
	}
	if _, err := s.FinishLink(ctx5(t), uri, "x"); !errors.Is(err, ErrLinkPending) {
		t.Errorf("second finishLink: %v", err)
	}
	if after := len(r.recordedMethods()); after != before {
		t.Errorf("%d calls reached the child while the link was pending", after-before)
	}
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("finishLink after cancel: %v", err)
	}
	if _, err := s.Version(ctx5(t)); err != nil {
		t.Fatalf("version after the link ended: %v", err)
	}
}

func contains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

func TestClientAnswersOtherRequestsWhileFinishLinkIsPending(t *testing.T) {
	// The wire-level behaviour of the real program, used by the client
	// directly: the supervisor itself holds back calls.
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "never"}, "answer_delay_ms": 100})
	cfg := r.config()
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = []string{}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Wait() })
	c := NewClient(stdout, stdin, ClientOptions{})
	defer c.Close()
	uri, err := c.StartLink(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	fctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.FinishLink(fctx, uri, "ARTEX")
	time.Sleep(200 * time.Millisecond)
	v, err := c.Version(ctx5(t))
	if err != nil || v != "0.14.10" {
		t.Fatalf("version while pending: %q %v", v, err)
	}
}

func TestAnswersAreMatchedWhenTheyArriveOutOfOrder(t *testing.T) {
	r := newRig(t, map[string]any{"delay_by_method_ms": map[string]int{"listAccounts": 300}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if a, err := s.ListAccounts(ctx5(t)); err != nil || a == nil {
			t.Errorf("listAccounts %v %v", a, err)
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		if v, err := s.Version(ctx5(t)); err != nil || v != "0.14.10" {
			t.Errorf("version %q %v", v, err)
		}
	}()
	wg.Wait()
}

func TestSendResultsAndErrors(t *testing.T) {
	cases := []struct {
		mode    string
		code    int
		results []string // types expected from the result or from error data
	}{
		{"ok", 0, []string{"SUCCESS"}},
		{"partial", 0, []string{"SUCCESS", "NETWORK_FAILURE"}},
		{"allfail", -3, []string{"NETWORK_FAILURE"}},
		{"ratelimit", -5, []string{"RATE_LIMIT_FAILURE"}},
		{"ioerror", -3, nil},
		{"unexpected", -32603, nil},
		{"untrusted", -4, nil},
		{"unknown_group", -1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			r := newRig(t, map[string]any{"send": map[string]any{"mode": tc.mode}})
			r.seedAccounts(testNumber)
			s, _ := r.run(r.config())
			r.waitUp(s)
			res, err := s.Send(ctx5(t), testNumber, testGroup, "hello")
			var types []string
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				for _, x := range res.Results {
					types = append(types, x.Type)
				}
			} else {
				var re *RPCError
				if !errors.As(err, &re) || re.Code != tc.code {
					t.Fatalf("want code %d, got %v", tc.code, err)
				}
				if got, ok := SendFailureResults(err); ok {
					for _, x := range got {
						types = append(types, x.Type)
					}
				}
			}
			if strings.Join(types, ",") != strings.Join(tc.results, ",") {
				t.Fatalf("types %v, want %v", types, tc.results)
			}
		})
	}
}

func TestSendNamesTheAccountAndTheFakeInsistsOnIt(t *testing.T) {
	r := newRig(t, nil)
	r.seedAccounts(testNumber)
	s, _ := r.run(r.config())
	r.waitUp(s)
	_, err := s.Send(ctx5(t), "+15555550108", testGroup, "hi")
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -32602 {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := s.ListGroups(ctx5(t), testNumber); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(r.calls)
	if !strings.Contains(string(raw), `"account":"`+testNumber+`"`) {
		t.Fatalf("calls: %s", raw)
	}
}

func TestChildCrashIsRestartedWithBackoff(t *testing.T) {
	r := newRig(t, map[string]any{"crash_on": "listAccounts"})
	cfg := r.config()
	cfg.MinBackoff = 300 * time.Millisecond
	cfg.MaxBackoff = 2 * time.Second
	s, _ := r.run(cfg)
	r.waitUp(s)
	_, err := s.ListAccounts(ctx5(t))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("crash: %v", err)
	}
	r.set(nil) // the next child does not crash
	ctx := ctx5(t)
	// The old child is gone; wait for the second one.
	deadline := time.Now().Add(10 * time.Second)
	for s.Status().Starts < 2 || !s.Status().Up {
		if time.Now().After(deadline) {
			t.Fatalf("no restart: %+v\n%s", s.Status(), r.logs.all())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.ListAccounts(ctx); err != nil {
		t.Fatalf("after restart: %v", err)
	}
	_, at := r.startTimes()
	if len(at) != 2 || at[1].Sub(at[0]) < 300*time.Millisecond {
		t.Fatalf("starts %v are closer than the minimum backoff", at)
	}
}

func TestCrashLoopStartsAtMostOncePerBackoff(t *testing.T) {
	r := newRig(t, map[string]any{"exit_after_start_ms": 5})
	cfg := r.config()
	cfg.MinBackoff = 250 * time.Millisecond
	cfg.MaxBackoff = 250 * time.Millisecond
	cfg.StartTimeout = 300 * time.Millisecond
	_, stop := r.run(cfg)
	time.Sleep(1600 * time.Millisecond)
	stop()
	_, at := r.startTimes()
	if len(at) < 2 || len(at) > 7 {
		t.Fatalf("%d starts in 1.6 s", len(at))
	}
	for i := 1; i < len(at); i++ {
		if at[i].Sub(at[i-1]) < 240*time.Millisecond {
			t.Fatalf("starts %d and %d are %v apart", i-1, i, at[i].Sub(at[i-1]))
		}
	}
}

func TestThreeTimeoutsInARowRestartTheChild(t *testing.T) {
	r := newRig(t, map[string]any{"hang_on": "listGroups"})
	r.seedAccounts(testNumber)
	cfg := r.config()
	cfg.CallTimeout = 100 * time.Millisecond
	s, _ := r.run(cfg)
	r.waitUp(s)
	for i := 0; i < 2; i++ {
		if _, err := s.ListGroups(ctx5(t), testNumber); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if st := s.Status(); st.Starts != 1 {
		t.Fatalf("restarted after two timeouts: %+v", st)
	}
	if _, err := s.ListGroups(ctx5(t), testNumber); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	r.set(map[string]any{}) // the new child answers
	deadline := time.Now().Add(10 * time.Second)
	for s.Status().Starts < 2 || !s.Status().Up {
		if time.Now().After(deadline) {
			t.Fatalf("no restart after three timeouts: %+v", s.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.ListGroups(ctx5(t), testNumber); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestAnAnswerBetweenTimeoutsResetsTheCount(t *testing.T) {
	r := newRig(t, map[string]any{"hang_on": "listGroups"})
	r.seedAccounts(testNumber)
	cfg := r.config()
	cfg.CallTimeout = 100 * time.Millisecond
	s, _ := r.run(cfg)
	r.waitUp(s)
	for round := 0; round < 3; round++ {
		for i := 0; i < 2; i++ {
			if _, err := s.ListGroups(ctx5(t), testNumber); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		}
		if _, err := s.Version(ctx5(t)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if st := s.Status(); st.Starts != 1 {
		t.Fatalf("restarted: %+v", st)
	}
}

func TestACallerContextEndingIsNotATimeoutOfTheChild(t *testing.T) {
	r := newRig(t, map[string]any{"hang_on": "listGroups"})
	r.seedAccounts(testNumber)
	s, _ := r.run(r.config())
	r.waitUp(s)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := s.ListGroups(ctx, testNumber)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if st := s.Status(); st.Starts != 1 {
		t.Fatalf("restarted: %+v", st)
	}
}

func TestFinishLinkTimeoutsNeverRestartTheChild(t *testing.T) {
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "never"}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	for i := 0; i < 4; i++ {
		uri, err := s.StartLink(ctx5(t))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		_, err = s.FinishLink(ctx, uri, "x")
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if st := s.Status(); st.Starts != 1 {
		t.Fatalf("restarted: %+v", st)
	}
}

func TestRestartAbortsAPendingLinkAndKeepsAnAccountCompletedAtThatMoment(t *testing.T) {
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "on_stop", "number": testNumber}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	uri, _ := s.StartLink(ctx5(t))
	res := make(chan error, 1)
	go func() { _, err := s.FinishLink(context.Background(), uri, "x"); res <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for !contains(r.recordedMethods(), "finishLink") {
		if time.Now().After(deadline) {
			t.Fatal("finishLink not sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Restart(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if err := <-res; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending finishLink: %v", err)
	}
	accts, err := s.ListAccounts(ctx5(t))
	if err != nil || len(accts) != 1 || accts[0].Number != testNumber {
		t.Fatalf("accounts after the restart: %v %v", accts, err)
	}
}

func TestRestartWithoutACompletedLinkLeavesNoAccount(t *testing.T) {
	r := newRig(t, map[string]any{"finish": map[string]any{"mode": "never"}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	uri, _ := s.StartLink(ctx5(t))
	go s.FinishLink(context.Background(), uri, "x")
	time.Sleep(200 * time.Millisecond)
	if err := s.Restart(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	accts, err := s.ListAccounts(ctx5(t))
	if err != nil || len(accts) != 0 {
		t.Fatalf("%v %v", accts, err)
	}
	if st := s.Status(); st.Starts != 2 {
		t.Fatalf("%+v", st)
	}
}

func TestPauseStopsTheChildAndResumeStartsIt(t *testing.T) {
	r := newRig(t, nil)
	r.seedAccounts(testNumber, "+15555550104")
	s, _ := r.run(r.config())
	r.waitUp(s)
	pids, _ := r.startTimes()
	if err := s.Pause(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pids[0], 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child still alive after Pause: %v", err)
	}
	if st := s.Status(); st.Up || !st.Paused {
		t.Fatalf("%+v", st)
	}
	if _, err := s.ListAccounts(ctx5(t)); !errors.Is(err, ErrChildDown) {
		t.Fatalf("call while paused: %v", err)
	}
	// The hook for unlink: the data directory is emptied while no child runs.
	if err := os.Remove(filepath.Join(r.data, "accounts.json")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // it must stay down
	if st := s.Status(); st.Up || st.Starts != 1 {
		t.Fatalf("restarted while paused: %+v", st)
	}
	if err := s.Resume(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitUp(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	accts, err := s.ListAccounts(ctx5(t))
	if err != nil || len(accts) != 0 {
		t.Fatalf("%v %v", accts, err)
	}
	for _, m := range r.recordedMethods() {
		if !allowedMethods[m] {
			t.Errorf("child received %q", m)
		}
	}
}

func TestResumeWhileRunningChangesNothing(t *testing.T) {
	r := newRig(t, nil)
	s, _ := r.run(r.config())
	r.waitUp(s)
	if err := s.Resume(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if st := s.Status(); !st.Up || st.Starts != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestRunEndsTheChildAndDoesNotRestartIt(t *testing.T) {
	r := newRig(t, nil)
	s, stop := r.run(r.config())
	r.waitUp(s)
	pids, _ := r.startTimes()
	stop()
	if err := syscall.Kill(pids[0], 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child alive after Run returned: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if st := s.Status(); st.Up || st.Starts != 1 {
		t.Fatalf("%+v", st)
	}
	if _, err := s.Version(ctx5(t)); !errors.Is(err, ErrChildDown) {
		t.Fatalf("call after stop: %v", err)
	}
	if err := s.Restart(ctx5(t)); !errors.Is(err, ErrStopped) {
		t.Fatalf("restart after stop: %v", err)
	}
	if err := s.Pause(ctx5(t)); !errors.Is(err, ErrStopped) {
		t.Fatalf("pause after stop: %v", err)
	}
}

func TestRunStopsWhileTheChildIsStillStarting(t *testing.T) {
	r := newRig(t, map[string]any{"start_delay_ms": 3000})
	cfg := r.config()
	cfg.StartTimeout = 10 * time.Second
	s, stop := r.run(cfg)
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	stop()
	if time.Since(start) > 8*time.Second {
		t.Fatal("stop waited for the start timeout")
	}
	if s.Status().Up {
		t.Fatal("up")
	}
}

func TestChildThatNeverAnswersVersionIsRetried(t *testing.T) {
	r := newRig(t, map[string]any{"start_delay_ms": 3000})
	cfg := r.config()
	cfg.StartTimeout = 200 * time.Millisecond
	cfg.StopGrace = 100 * time.Millisecond
	s, _ := r.run(cfg)
	deadline := time.Now().Add(10 * time.Second)
	for s.Status().LastError == "" || s.Status().Starts < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("%+v", s.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(s.Status().LastError, "version") {
		t.Fatalf("last error %q", s.Status().LastError)
	}
	r.set(nil)
	r.waitUp(s)
}

func TestChildOutputIsScrubbedBeforeItIsLogged(t *testing.T) {
	uri := "sgnl://linkdevice?uuid=SECRETUUID&pub_key=SECRETKEY&capabilities=a,b"
	r := newRig(t, map[string]any{"stderr_lines": []string{
		"WARN account +15555550101 problem",
		"link " + uri,
		"group group." + testGroup + " gone",
		strings.Repeat("y", 10000),
	}})
	s, _ := r.run(r.config())
	r.waitUp(s)
	time.Sleep(300 * time.Millisecond)
	logs := r.logs.all()
	for _, bad := range []string{"15555550101", "SECRETUUID", "SECRETKEY", testGroup, "pub_key"} {
		if strings.Contains(logs, bad) {
			t.Errorf("log holds %q:\n%s", bad, logs)
		}
	}
	if !strings.Contains(logs, "…0101") || !strings.Contains(logs, "long line skipped") {
		t.Errorf("expected masked lines in:\n%s", logs)
	}
}

func TestOversizeAndNotificationLinesDoNotDisturbCallsOrLeakIntoLogs(t *testing.T) {
	r := newRig(t, map[string]any{
		"oversize":        map[string]any{"method": "listAccounts", "bytes": 5 << 20},
		"notifications":   true,
		"notify_sentinel": "SENTINEL-MESSAGE-BODY",
	})
	s, _ := r.run(r.config())
	r.waitUp(s)
	if _, err := s.ListAccounts(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Version(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	logs := r.logs.all()
	if strings.Contains(logs, "SENTINEL-MESSAGE-BODY") || strings.Contains(logs, "15555550106") {
		t.Fatalf("incoming message content in the log:\n%s", logs)
	}
	if !strings.Contains(logs, "skipped an oversize line") {
		t.Fatalf("skipped line not noted:\n%s", logs)
	}
}

func TestStartFailureIsReportedAndRetried(t *testing.T) {
	r := newRig(t, nil)
	cfg := r.config()
	cfg.Command = filepath.Join(r.dir, "does-not-exist")
	s, _ := r.run(cfg)
	deadline := time.Now().Add(5 * time.Second)
	for s.Status().LastError == "" || s.Status().Starts != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%+v", s.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(r.logs.all(), "did not start") {
		t.Fatalf("log:\n%s", r.logs.all())
	}
}

func TestSendToNumbersReachesTheChildWithTheAccount(t *testing.T) {
	r := newRig(t, nil)
	r.seedAccounts(testNumber)
	s, _ := r.run(r.config())
	r.waitUp(s)
	res, err := s.SendNumbers(ctx5(t), testNumber, []string{"+15555550102"}, "hello")
	if err != nil || len(res.Results) != 1 || res.Results[0].Type != "SUCCESS" {
		t.Fatalf("send: %+v %v", res, err)
	}
	raw, _ := os.ReadFile(r.calls)
	if !strings.Contains(string(raw), `"recipient":["+15555550102"]`) || !strings.Contains(string(raw), `"account":"`+testNumber+`"`) {
		t.Fatalf("calls: %s", raw)
	}
	if _, err := s.SendNumbers(ctx5(t), "+15555550108", []string{"+15555550102"}, "hi"); err == nil {
		t.Fatal("an account the child does not know must fail")
	}
}

// The stand-in takes its scenario from a file in its data directory when the
// command line carries no scenario flag (the smoke image has none).
func TestFakeCLIReadsTheScenarioFromItsDataDirectory(t *testing.T) {
	r := newRig(t, nil)
	raw := `{"version":"9.8.7","finish":{"mode":"after","after_ms":50,"number":"` + testNumber + `"}}`
	if err := os.WriteFile(filepath.Join(r.data, "fake-scenario.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := r.config()
	cfg.Args = []string{"-c", r.data}
	s, _ := r.run(cfg)
	r.waitUp(s)
	if v := s.Status().Version; v != "9.8.7" {
		t.Fatalf("version %q: the scenario file in the data directory was not used", v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uri, err := s.StartLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := s.FinishLink(ctx, uri, "ARTEX")
	if err != nil || acct.Number != testNumber {
		t.Fatalf("finishLink: %+v, %v", acct, err)
	}
}

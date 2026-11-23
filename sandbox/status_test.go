package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProbe is a platform that satisfies P1-P5 until a field says otherwise.
type fakeProbe struct {
	l         *Layout // paths the probe answers for; the default layout when nil
	euid      int
	capEff    string // hex, as /proc/self/status prints it
	setpriv   *fakeFile
	static    bool
	staticErr error
	shimDir   *fakeFile
	shim      *fakeFile
	env       string
}

type fakeFile struct {
	mode fs.FileMode
	uid  uint32
}

func goodProbe() *fakeProbe {
	return &fakeProbe{
		euid:    0,
		capEff:  "00000000a80425fb", // CAP_SETGID, CAP_SETUID and CAP_SETPCAP are in Docker's default set
		setpriv: &fakeFile{mode: 0o755, uid: 0},
		static:  true,
		shimDir: &fakeFile{mode: fs.ModeDir | 0o755, uid: 0},
		shim:    &fakeFile{mode: 0o755, uid: 0},
	}
}

func (p *fakeProbe) EUID() int { return p.euid }
func (p *fakeProbe) StatusFile() ([]byte, error) {
	return []byte("Name:\tartex\nCapInh:\t0000000000000000\nCapPrm:\t" + p.capEff + "\nCapEff:\t" + p.capEff + "\nCapBnd:\t" + p.capEff + "\n"), nil
}
func (p *fakeProbe) layout() Layout {
	if p.l != nil {
		return *p.l
	}
	return DefaultLayout()
}
func (p *fakeProbe) Stat(path string) (fs.FileMode, uint32, error) {
	var f *fakeFile
	l := p.layout()
	switch path {
	case l.Setpriv:
		f = p.setpriv
	case l.ShimDir:
		f = p.shimDir
	case l.ShimDir + "/bash":
		f = p.shim
	case l.Artex:
		f = &fakeFile{mode: 0o755}
	}
	if f == nil {
		return 0, 0, fs.ErrNotExist
	}
	return f.mode, f.uid, nil
}
func (p *fakeProbe) IsStatic(string) (bool, error) { return p.static, p.staticErr }
func (p *fakeProbe) Getenv(string) string          { return p.env }

func TestPreconditionsHoldOnTheShippedPlatform(t *testing.T) {
	pre := CheckPreconditions(DefaultLayout(), goodProbe())
	if len(pre.Failing()) != 0 {
		t.Fatalf("failing: %v", pre.Failing())
	}
}

// Each precondition has its own way to fail, and each is reported under its own name.
func TestPreconditionsEachFailsOnItsOwn(t *testing.T) {
	for _, tc := range []struct {
		id  string
		mut func(*fakeProbe)
	}{
		{"P1", func(p *fakeProbe) { p.euid = 1000 }},
		{"P2", func(p *fakeProbe) { p.capEff = "00000000a80425fb" }}, // replaced below
		{"P3", func(p *fakeProbe) { p.setpriv = nil }},
		{"P3", func(p *fakeProbe) { p.setpriv.uid = 1000 }},
		{"P3", func(p *fakeProbe) { p.setpriv.mode = 0o644 }},
		{"P3", func(p *fakeProbe) { p.setpriv.mode = fs.ModeSymlink | 0o755 }},
		{"P4", func(p *fakeProbe) { p.static = false }},
		{"P4", func(p *fakeProbe) {
			p.l = &Layout{Artex: "/nonexistent/artex", ShimDir: DefaultLayout().ShimDir, Setpriv: DefaultLayout().Setpriv}
		}},
		{"P4", func(p *fakeProbe) { p.staticErr = fs.ErrNotExist }},
		{"P4", func(p *fakeProbe) { p.shimDir = nil }},
		{"P4", func(p *fakeProbe) { p.shimDir.uid = 1000 }},
		{"P4", func(p *fakeProbe) { p.shim = nil }},
		{"P4", func(p *fakeProbe) { p.shim.uid = 1000 }},
		{"P5", func(p *fakeProbe) { p.env = "off" }},
		{"P5", func(p *fakeProbe) { p.env = " OFF " }},
	} {
		p := goodProbe()
		if tc.id == "P2" {
			// every one of the three capabilities, removed one at a time
			for _, bit := range []uint{6, 7, 8} { // CAP_SETGID, CAP_SETUID, CAP_SETPCAP
				q := goodProbe()
				q.capEff = fmt.Sprintf("%016x", uint64(0xa80425fb)&^(1<<bit))
				if got := CheckPreconditions(DefaultLayout(), q).Failing(); len(got) != 1 || got[0] != "P2" {
					t.Errorf("capability bit %d removed: failing %v, want [P2]", bit, got)
				}
			}
			q := goodProbe()
			q.capEff = "0000000000000000"
			if got := CheckPreconditions(DefaultLayout(), q).Failing(); len(got) != 1 || got[0] != "P2" {
				t.Errorf("no capabilities: failing %v, want [P2]", got)
			}
			continue
		}
		tc.mut(p)
		got := CheckPreconditions(DefaultLayout(), p).Failing()
		if len(got) != 1 || got[0] != tc.id {
			t.Errorf("%s case: failing %v, want [%s]", tc.id, got, tc.id)
		}
	}
}

// The override is read from the environment only; an empty value means unset, and an
// unrecognised value never switches isolation off.
func TestIsolationOverrideValues(t *testing.T) {
	for v, wantOff := range map[string]bool{"": false, "  ": false, "off": true, "OFF": true, "on": false, "1": false, "no": false, "false": false} {
		p := goodProbe()
		p.env = v
		off := !CheckPreconditions(DefaultLayout(), p).P5
		if off != wantOff {
			t.Errorf("ARTEX_TOOL_ISOLATION=%q: off=%v, want %v", v, off, wantOff)
		}
	}
}

// AC22(g): `unavailable` appears exactly when P1-P4 fail, `disabled` exactly when the
// operator turned it off, and no combination of C1-C12 failures ever changes that. A
// failure a tool can cause (every C check) leaves isolation on.
func TestStatusFollowsPreconditionsOnly(t *testing.T) {
	checkIDs := []string{"C1", "C2", "C3", "C4", "C5", "C6", "C7", "C8", "C9", "C10", "C11", "C12"}
	degrading := map[string]bool{"C1": true, "C2": true, "C3": true, "C4": true, "C5": true, "C6": true, "C10": true, "C11": true, "C12": true}
	for p := 0; p < 32; p++ {
		pre := Preconditions{P1: p&1 != 0, P2: p&2 != 0, P3: p&4 != 0, P4: p&8 != 0, P5: p&16 != 0}
		pHold := pre.P1 && pre.P2 && pre.P3 && pre.P4
		for c := 0; c < 1<<len(checkIDs); c++ {
			var checks []CheckResult
			anyDegrading := false
			for i, id := range checkIDs {
				if c&(1<<i) != 0 {
					checks = append(checks, CheckResult{ID: id, Detail: "failed", Degrades: degrading[id]})
					anyDegrading = anyDegrading || degrading[id]
				}
			}
			st, _ := ComputeStatus(pre, checks)
			var want Status
			switch {
			case !pre.P5:
				want = Disabled
			case !pHold:
				want = Unavailable
			case anyDegrading:
				want = Degraded
			default:
				want = Enforced
			}
			if st != want {
				t.Fatalf("P=%v checks=%v: status %s, want %s", pre, checks, st, want)
			}
			if st == Unavailable && pHold {
				t.Fatalf("unavailable although P1-P4 hold (checks %v)", checks)
			}
			if got := st.IsolationOn(); got != (st == Enforced || st == Degraded) {
				t.Fatalf("%s: IsolationOn=%v", st, got)
			}
		}
	}
}

func TestComputeStatusReasonsAndWarnings(t *testing.T) {
	pre := Preconditions{P1: true, P2: true, P3: true, P4: true, P5: true}
	st, view := ComputeStatus(pre, []CheckResult{
		{ID: "C3", Detail: "the config file is readable", Degrades: true},
		{ID: "C7", Detail: "database TCP is reachable"},
	})
	if st != Degraded {
		t.Fatalf("status %s", st)
	}
	if !strings.Contains(view.Reason, "C3") || strings.Contains(view.Reason, "C7") {
		t.Errorf("reason %q should name the degrading check only", view.Reason)
	}
	if len(view.Warnings) != 2 {
		t.Errorf("warnings %v, want both checks", view.Warnings)
	}
	// warnings alone keep the status enforced
	st, view = ComputeStatus(pre, []CheckResult{{ID: "C9", Detail: "metadata reachable"}})
	if st != Enforced || len(view.Warnings) != 1 || view.Reason != "" {
		t.Errorf("status %s view %+v", st, view)
	}
	// the reason of an unavailable status names the failing precondition
	st, view = ComputeStatus(Preconditions{P1: true, P2: false, P3: true, P4: true, P5: true, Reasons: map[string]string{"P2": "CAP_SETUID is missing"}}, nil)
	if st != Unavailable || !strings.Contains(view.Reason, "CAP_SETUID") {
		t.Errorf("status %s view %+v", st, view)
	}
	if view.Warnings == nil {
		t.Error("Warnings must be an empty list, not nil, so the API never returns null")
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }

// ---- C12 on the published status (plan AC10) ----

// c12Iso is an enforced isolation with a Signal socket configured, built without Activate so
// that it needs no root: the check workspace falls back to a directory the test owns.
func c12Iso(t *testing.T, socket string, run ScriptRunner) *Isolation {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file-in-the-way")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	iso := &Isolation{
		on: true, logf: func(string, ...any) {},
		pre:    Preconditions{P1: true, P2: true, P3: true, P4: true, P5: true},
		c12in:  SelfCheckInput{SignalSocket: socket, WorkDirParent: blocker, FallbackDir: dir},
		runner: run,
	}
	iso.mu.Lock()
	iso.recompute()
	iso.mu.Unlock()
	return iso
}

// shortSockDir is a temp directory with a path short enough for a unix socket.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "c12")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func listenAt(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
}

// Re-run: "not verified" (the socket was not there at start) clears once the socket exists and the
// tool user is kept out, the published status follows, and a reachable socket degrades again.
func TestRecheckSignalClearsNotVerifiedOnceTheSocketExists(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	answer := "c12.sock=refused\nc12.dir=denied\n"
	runs := 0
	iso := c12Iso(t, sock, func(ctx context.Context, d, s string) (string, error) { runs++; return answer, nil })

	iso.RecheckSignal(t.Context()) // the bridge has not created the socket yet
	v := iso.View()
	if v.Status != Degraded || !strings.Contains(v.Reason, "C12: not verified: socket absent") {
		t.Fatalf("absent: %+v", v)
	}
	if runs != 0 {
		t.Fatalf("the tool shell ran %d times for an absent socket", runs)
	}

	listenAt(t, sock)
	iso.RecheckSignal(t.Context())
	if v := iso.View(); v.Status != Enforced || v.Reason != "" || len(v.Warnings) != 0 || runs != 1 {
		t.Fatalf("socket present and closed to tools: %+v, %d runs", v, runs)
	}

	answer = "c12.sock=connected\nc12.dir=denied\n"
	iso.RecheckSignal(t.Context())
	v = iso.View()
	if v.Status != Degraded || !strings.Contains(v.Reason, "tool user can connect to "+sock) {
		t.Fatalf("connected: %+v", v)
	}
}

// A re-run changes only C12: other failed checks stay, and with no Signal socket, or the drop chain
// off, it does nothing.
func TestRecheckSignalTouchesC12Only(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "absent.sock")
	iso := c12Iso(t, sock, func(context.Context, string, string) (string, error) { return "", nil })
	iso.setCheck(CheckResult{ID: "C6", Detail: "kept", Degrades: true})
	iso.RecheckSignal(t.Context())
	if got := failedIDs(iso.failed); got != "C6,C12" {
		t.Fatalf("failed %q, want C6,C12", got)
	}
	iso.RecheckSignal(t.Context()) // repeat: replaced, not added twice
	if got := failedIDs(iso.failed); got != "C6,C12" {
		t.Fatalf("after a repeat: %q", got)
	}

	off := c12Iso(t, sock, nil)
	off.on = false
	for name, i := range map[string]*Isolation{
		"no socket":    c12Iso(t, "", nil),
		"chain off":    off,
		"nil receiver": nil,
	} {
		i.RecheckSignal(t.Context())
		if i != nil && len(i.failed) != 0 {
			t.Errorf("%s: %+v", name, i.failed)
		}
	}
}

// Re-runs, status reads and other checks at the same time (run with -race).
func TestRecheckSignalIsSafeConcurrently(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	listenAt(t, sock)
	iso := c12Iso(t, sock, func(context.Context, string, string) (string, error) {
		return "c12.sock=refused\nc12.dir=denied\n", nil
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() { defer wg.Done(); iso.RecheckSignal(t.Context()) }()
		go func() { defer wg.Done(); _ = iso.View(); _ = iso.Status(); _ = iso.SessionWarning() }()
		go func() {
			defer wg.Done()
			iso.setCheck(CheckResult{ID: "E1", Detail: "x", Degrades: true})
			iso.clearCheck("E1")
		}()
	}
	wg.Wait()
	if v := iso.View(); v.Status != Enforced {
		t.Fatalf("%+v", v)
	}
}

// The re-run waits for the C12 lock with its context: while another re-run holds the lock, a caller
// whose context is done (cancelled or past its deadline) gets an error back at once, and the one
// that holds the lock still finishes and publishes. Run with -race.
func TestRecheckSignalHonoursContextWhileAnotherRecheckHoldsTheLock(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	listenAt(t, sock)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	iso := c12Iso(t, sock, func(context.Context, string, string) (string, error) {
		started <- struct{}{}
		<-release
		return "c12.sock=refused\nc12.dir=denied\n", nil
	})
	first := make(chan error, 1)
	go func() { first <- iso.RecheckSignal(context.Background()) }()
	<-started // the first re-run now holds the lock

	call := func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() { done <- iso.RecheckSignal(ctx) }()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Error("RecheckSignal did not return while its context was done")
			return nil
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := call(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v, want context.Canceled", err)
	}
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := call(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: err = %v, want context.DeadlineExceeded", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Errorf("the re-run that held the lock: %v", err)
	}
	if v := iso.View(); v.Status != Enforced {
		t.Errorf("%+v", v)
	}
}

// nil means the check ran and its result was published; every other outcome is an error, and a
// tool shell that failed to run still degrades C12 as before.
func TestRecheckSignalReturnsAnErrorWhenTheCheckDidNotRun(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	listenAt(t, sock)

	// context already done and nothing else running: the lease is not acquired
	iso := c12Iso(t, sock, func(context.Context, string, string) (string, error) {
		t.Error("the tool shell must not run for a context that is done")
		return "", nil
	})
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := iso.RecheckSignal(done); err == nil {
		t.Error("a context that is done: want an error")
	}

	// runner failure: published as degraded, and reported
	boom := errors.New("no tool shell")
	failing := c12Iso(t, sock, func(context.Context, string, string) (string, error) { return "", boom })
	err := failing.RecheckSignal(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("runner failure: err = %v, want it to wrap %v", err, boom)
	}
	if v := failing.View(); v.Status != Degraded || !strings.Contains(v.Reason, "the tool shell failed to run") {
		t.Errorf("runner failure must still degrade C12: %+v", v)
	}

	// the check ran and passed
	ok := c12Iso(t, sock, func(context.Context, string, string) (string, error) {
		return "c12.sock=refused\nc12.dir=denied\n", nil
	})
	if err := ok.RecheckSignal(context.Background()); err != nil {
		t.Errorf("a check that ran: %v", err)
	}

	// nothing to check is not an error
	off := c12Iso(t, sock, nil)
	off.on = false
	for name, i := range map[string]*Isolation{"no socket": c12Iso(t, "", nil), "chain off": off, "nil receiver": nil} {
		if err := i.RecheckSignal(context.Background()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

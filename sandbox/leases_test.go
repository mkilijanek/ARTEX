package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKiller records the order of events and lets a test hold the kill.
type fakeKiller struct {
	mu     sync.Mutex
	events []string
	left   []int // what Left answers after the 1st, 2nd... kill; the last one repeats
	nkill  int
	gate   chan struct{}
	// killErrs and leftErrs are what the next kills and /proc reads return, one per call; nil
	// or exhausted: success.
	killErrs []error
	leftErrs []error
}

func (k *fakeKiller) failNext(kill, left []error) {
	k.mu.Lock()
	k.killErrs, k.leftErrs = kill, left
	k.mu.Unlock()
}

func (k *fakeKiller) note(s string) {
	k.mu.Lock()
	k.events = append(k.events, s)
	k.mu.Unlock()
}
func (k *fakeKiller) log() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.events...)
}
func (k *fakeKiller) KillAll(ctx context.Context) error {
	k.mu.Lock()
	k.nkill++
	k.mu.Unlock()
	k.note("kill-start")
	if k.gate != nil {
		<-k.gate
	}
	k.note("kill-end")
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.killErrs) > 0 {
		err := k.killErrs[0]
		k.killErrs = k.killErrs[1:]
		return err
	}
	return nil
}
func (k *fakeKiller) Left(ctx context.Context) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.leftErrs) > 0 {
		err := k.leftErrs[0]
		k.leftErrs = k.leftErrs[1:]
		if err != nil {
			return 0, err
		}
	}
	if len(k.left) == 0 || k.nkill == 0 {
		return 0, nil
	}
	i := k.nkill - 1
	if i >= len(k.left) {
		i = len(k.left) - 1
	}
	return k.left[i], nil
}
func (k *fakeKiller) kills() int {
	n := 0
	for _, e := range k.log() {
		if e == "kill-start" {
			n++
		}
	}
	return n
}

func fastOpts() LeaseOptions {
	return LeaseOptions{PollEvery: time.Millisecond, PollWait: 50 * time.Millisecond, RetryEvery: 5 * time.Millisecond}
}

func TestSweepKillsWhenNoLeaseIsHeld(t *testing.T) {
	k := &fakeKiller{}
	l := NewLeases(k, fastOpts())
	res, err := l.Sweep(t.Context(), false)
	if err != nil || res.Skipped || !res.Killed || res.Left != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
}

// The sweeper re-checks the count under the lock: a lease taken after the count dropped to
// zero but before the sweep ran means no kill.
func TestSweepSkipsWhileALeaseIsHeld(t *testing.T) {
	k := &fakeKiller{}
	l := NewLeases(k, fastOpts())
	lease, err := l.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	res, _ := l.Sweep(t.Context(), false)
	if !res.Skipped || k.kills() != 0 {
		t.Fatalf("swept with a lease held: %+v events %v", res, k.log())
	}
	lease.Release()
	lease.Release() // idempotent: a double release must not drive the count negative
	if l.Held() != 0 {
		t.Fatalf("held %d after release", l.Held())
	}
	res, _ = l.Sweep(t.Context(), false)
	if res.Skipped || k.kills() != 1 {
		t.Fatalf("not swept once idle: %+v", res)
	}
}

// AC17(d): a lease requested while the sweeper holds the lock waits until the kill is over,
// so every process started afterwards was started after the kill.
func TestAcquireWaitsForARunningSweep(t *testing.T) {
	k := &fakeKiller{gate: make(chan struct{})}
	l := NewLeases(k, fastOpts())
	done := make(chan struct{})
	go func() { l.Sweep(context.Background(), false); close(done) }()
	for len(k.log()) == 0 {
		time.Sleep(time.Millisecond)
	}
	got := make(chan *Lease, 1)
	go func() {
		ls, _ := l.Acquire(context.Background())
		k.note("acquired")
		got <- ls
	}()
	select {
	case <-got:
		t.Fatal("a lease was granted while the kill was still running")
	case <-time.After(30 * time.Millisecond):
	}
	close(k.gate)
	ls := <-got
	<-done
	ls.Release()
	ev := strings.Join(k.log(), ",")
	if !strings.Contains(ev, "kill-end") || strings.Index(ev, "kill-end") > strings.Index(ev, "acquired") {
		t.Fatalf("order %s: the lease was granted before the kill ended", ev)
	}
}

// A lease taken during the idle-sweep delay must not be killed by it: the sweep keeps the lock
// for its whole sequence, and the lease is granted only after it.
func TestSweepDelayHoldsTheLock(t *testing.T) {
	k := &fakeKiller{}
	opts := fastOpts()
	opts.Delay = 60 * time.Millisecond
	l := NewLeases(k, opts)
	go l.Sweep(context.Background(), false)
	for k.kills() == 0 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	ls, err := l.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Release()
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("the lease did not wait for the delayed sweep")
	}
}

// When the count falls to zero the sweeper in Run sweeps by itself.
func TestRunSweepsWhenTheLastLeaseIsReleased(t *testing.T) {
	k := &fakeKiller{}
	l := NewLeases(k, fastOpts())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go l.Run(ctx)
	ls, _ := l.Acquire(ctx)
	time.Sleep(10 * time.Millisecond)
	if k.kills() != 0 {
		t.Fatal("swept while a lease was held")
	}
	ls.Release()
	waitFor(t, func() bool { return k.kills() >= 1 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// C10: while processes remain after a sweep and no lease is held, the sweeper keeps killing,
// then reports the clear once none are left.
func TestLeftoversAreKilledAgainUntilGone(t *testing.T) {
	k := &fakeKiller{left: []int{3, 2, 0}}
	var mu sync.Mutex
	var cleared bool
	var counts []int
	opts := fastOpts()
	opts.OnLeftover = func(n int) { mu.Lock(); counts = append(counts, n); mu.Unlock() }
	opts.OnClear = func() { mu.Lock(); cleared = true; mu.Unlock() }
	l := NewLeases(k, opts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go l.Run(ctx)
	res, err := l.Sweep(ctx, false)
	if err != nil || res.Left == 0 {
		t.Fatalf("first sweep should leave processes: %+v %v", res, err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return cleared })
	if k.kills() < 2 {
		t.Fatalf("only %d kills for a leftover that needed more", k.kills())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(counts) == 0 || counts[0] != 3 {
		t.Errorf("leftover counts %v", counts)
	}
}

// While a lease is held, the retry loop leaves other tasks' processes alone.
func TestRetryDoesNotKillWhileALeaseIsHeld(t *testing.T) {
	k := &fakeKiller{left: []int{2}}
	l := NewLeases(k, fastOpts())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go l.Run(ctx)
	if _, err := l.Sweep(ctx, false); err != nil {
		t.Fatal(err)
	}
	before := k.kills()
	ls, _ := l.Acquire(ctx)
	time.Sleep(40 * time.Millisecond)
	if k.kills() != before {
		t.Fatalf("killed %d more times while a lease was held", k.kills()-before)
	}
	ls.Release()
}

// Graceful stop: wait for leases to drain up to the limit, then sweep whatever is left.
func TestDrainSweepsEvenWithLeasesHeld(t *testing.T) {
	k := &fakeKiller{}
	l := NewLeases(k, fastOpts())
	ls, _ := l.Acquire(t.Context())
	defer ls.Release()
	start := time.Now()
	l.Drain(t.Context(), 30*time.Millisecond)
	if time.Since(start) < 25*time.Millisecond {
		t.Error("did not wait for the leases")
	}
	if k.kills() != 1 {
		t.Fatalf("kills %d, want the forced sweep", k.kills())
	}
}

func TestDrainReturnsEarlyWhenIdle(t *testing.T) {
	k := &fakeKiller{}
	l := NewLeases(k, fastOpts())
	ls, _ := l.Acquire(t.Context())
	go func() { time.Sleep(10 * time.Millisecond); ls.Release() }()
	start := time.Now()
	l.Drain(t.Context(), 2*time.Second)
	if time.Since(start) > time.Second {
		t.Error("waited the full limit although the leases were released")
	}
	if k.kills() < 1 {
		t.Error("no sweep after the drain")
	}
}

// A lease requested while a sweep runs gives up when its context ends, instead of waiting for
// the sweep however long it takes.
func TestAcquireHonoursTheContextWhileASweepRuns(t *testing.T) {
	k := &fakeKiller{gate: make(chan struct{})}
	l := NewLeases(k, fastOpts())
	done := make(chan struct{})
	go func() { l.Sweep(context.Background(), false); close(done) }()
	defer func() { close(k.gate); <-done }()
	for len(k.log()) == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	got := make(chan error, 1)
	go func() {
		ls, err := l.Acquire(ctx)
		if ls != nil {
			ls.Release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err %v, want the context's deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire kept waiting for the sweep after its context ended")
	}
	if l.Held() != 0 {
		t.Fatalf("held %d after a refused lease", l.Held())
	}
}

func TestAcquireRefusesACancelledContext(t *testing.T) {
	l := NewLeases(&fakeKiller{}, fastOpts())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("a lease was granted to a cancelled context")
	}
}

// The count of tool processes ignores zombies (they cannot act) and other users.
func TestCountToolProcessesIgnoresZombiesAndOtherUsers(t *testing.T) {
	proc := t.TempDir()
	add := func(pid, state string, uid string) {
		d := filepath.Join(proc, pid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "Name:\tx\nState:\t" + state + "\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\n"
		if err := os.WriteFile(filepath.Join(d, "status"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	add("10", "S (sleeping)", "30002")
	add("11", "R (running)", "30002")
	add("12", "Z (zombie)", "30002")
	add("13", "S (sleeping)", "0")
	add("14", "T (stopped)", "30002")
	add("15", "X (dead)", "30002")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil { // not a pid
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proc, "16"), 0o755); err != nil { // vanished: no status file
		t.Fatal(err)
	}
	n, err := CountToolProcesses(proc, 30002)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v, want 3 (S, R and T)", n, err)
	}
}

func TestCountToolProcessesFailsOnAnUnreadableProcRoot(t *testing.T) {
	if _, err := CountToolProcesses(filepath.Join(t.TempDir(), "missing"), 30002); err == nil {
		t.Fatal("a missing /proc was reported as zero processes")
	}
}

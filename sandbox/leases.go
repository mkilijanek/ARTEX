package sandbox

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// Killer ends every process of the tool user and counts what is left. The real one runs
// `__killall` through the drop chain and reads /proc.
type Killer interface {
	KillAll(ctx context.Context) error
	// Left counts the tool user's processes that are not zombies.
	Left(ctx context.Context) (int, error)
}

// LeaseOptions tunes the sweeper. The zero value uses the production timings.
type LeaseOptions struct {
	PollEvery  time.Duration // between two /proc reads after a kill (default 50 ms)
	PollWait   time.Duration // how long to wait for the kill to take effect (default 2 s)
	RetryEvery time.Duration // between two kills while leftovers remain (default 5 s)
	// Delay holds the sweep after the kill, with the lock held. Only the private harness sets
	// it, to open the window for the race test.
	Delay time.Duration
	// OnLeftover is told how many processes survived a sweep; OnClear when a later sweep finds
	// none. OnFailure is told when a sweep could not kill or could not count (the sweeper then
	// keeps trying, as with leftovers, and OnClear follows the first sweep that finds none).
	OnLeftover func(n int)
	OnClear    func()
	OnFailure  func(err error)
}

func (o LeaseOptions) withDefaults() LeaseOptions {
	if o.PollEvery <= 0 {
		o.PollEvery = 50 * time.Millisecond
	}
	if o.PollWait <= 0 {
		o.PollWait = 2 * time.Second
	}
	if o.RetryEvery <= 0 {
		o.RetryEvery = 5 * time.Second
	}
	return o
}

// Leases counts the holders that may have tool processes alive. Every code path that can
// start a tool-user process takes a lease first and releases it when those processes are
// meant to be gone. When the count falls to zero the sweeper kills whatever is left of the
// tool user (processes of finished sessions that escaped their group). It holds the lock for
// the whole sequence, and Acquire takes the same lock, so every process started after a
// sweep was started after its kill.
type Leases struct {
	killer Killer
	opt    LeaseOptions

	sweep chan struct{} // a one-slot lock, held for a whole sweep; Acquire passes through it
	mu    sync.Mutex    // guards the fields below
	n     int
	idle  chan struct{}

	leftover  bool
	busySince time.Time
	warnedAt  time.Time
	failedAt  time.Time // last logged sweep failure
}

// NewLeases returns a lease counter that sweeps with k.
func NewLeases(k Killer, opt LeaseOptions) *Leases {
	return &Leases{killer: k, opt: opt.withDefaults(), idle: make(chan struct{}, 1), sweep: make(chan struct{}, 1)}
}

// Lease is one hold. Release is idempotent.
type Lease struct {
	l    *Leases
	once sync.Once
}

// Acquire takes a lease. It waits while a sweep runs, and gives up with ctx's error when ctx ends
// first. A nil *Leases hands out a lease that counts nothing, so tests that do not set the
// isolation up keep working.
func (l *Leases) Acquire(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil {
		return &Lease{}, nil
	}
	select {
	case l.sweep <- struct{}{}:
	case <-ctx.Done():
		log.Printf("[isolation] a lease gave up waiting for the cleanup of tool processes: %v", ctx.Err())
		return nil, ctx.Err()
	}
	l.mu.Lock()
	if l.n == 0 {
		l.busySince = time.Now()
	}
	l.n++
	l.mu.Unlock()
	<-l.sweep
	return &Lease{l: l}, nil
}

// Release gives the lease back. When it was the last one, the sweeper is woken.
func (ls *Lease) Release() {
	ls.once.Do(func() {
		if ls.l == nil {
			return
		}
		ls.l.mu.Lock()
		ls.l.n--
		zero := ls.l.n == 0
		ls.l.mu.Unlock()
		if zero {
			select {
			case ls.l.idle <- struct{}{}:
			default:
			}
		}
	})
}

// markLeftover makes the sweeper keep killing, as after a sweep that left processes behind.
func (l *Leases) markLeftover() {
	l.mu.Lock()
	l.leftover = true
	l.mu.Unlock()
}

// sweepFailed records a sweep that could not kill or count: the sweeper keeps trying on every
// tick, and OnFailure is told.
func (l *Leases) sweepFailed(err error) {
	l.markLeftover()
	if l.opt.OnFailure != nil {
		l.opt.OnFailure(err)
	}
}

// Held is the number of leases outstanding.
func (l *Leases) Held() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// SweepResult describes one sweep.
type SweepResult struct {
	Skipped bool // a lease was held, nothing was killed
	Killed  bool
	Left    int // processes that survived the kill
}

// Sweep kills the tool user's processes. Unless force is set it does so only when no lease is
// held, re-checked under the lock. Left is the count of non-zombie processes after up to
// PollWait.
func (l *Leases) Sweep(ctx context.Context, force bool) (SweepResult, error) {
	l.sweep <- struct{}{}
	defer func() { <-l.sweep }()
	if !force && l.Held() != 0 {
		return SweepResult{Skipped: true}, nil
	}
	if err := l.killer.KillAll(ctx); err != nil {
		return SweepResult{}, err
	}
	if l.opt.Delay > 0 {
		select {
		case <-time.After(l.opt.Delay):
		case <-ctx.Done():
		}
	}
	deadline := time.Now().Add(l.opt.PollWait)
	for {
		left, err := l.killer.Left(ctx)
		if err != nil {
			return SweepResult{Killed: true}, err
		}
		if left == 0 || !time.Now().Before(deadline) {
			l.noteLeft(left)
			return SweepResult{Killed: true, Left: left}, nil
		}
		select {
		case <-time.After(l.opt.PollEvery):
		case <-ctx.Done():
			return SweepResult{Killed: true, Left: left}, ctx.Err()
		}
	}
}

func (l *Leases) noteLeft(left int) {
	l.mu.Lock()
	was := l.leftover
	l.leftover = left > 0
	l.mu.Unlock()
	switch {
	case left > 0 && l.opt.OnLeftover != nil:
		l.opt.OnLeftover(left)
	case left == 0 && was && l.opt.OnClear != nil:
		l.opt.OnClear()
	}
}

// Run is the sweeper loop. It sweeps whenever the count falls to zero, and keeps killing every
// RetryEvery while leftovers remain and no lease is held. It returns when ctx ends.
func (l *Leases) Run(ctx context.Context) {
	tick := time.NewTicker(l.opt.RetryEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.idle:
		case <-tick.C:
			l.mu.Lock()
			retry := l.leftover
			l.mu.Unlock()
			if !retry {
				l.warnLong()
				continue
			}
		}
		if _, err := l.Sweep(ctx, false); err != nil && !errors.Is(err, context.Canceled) {
			l.mu.Lock()
			due := time.Since(l.failedAt) >= time.Minute
			if due {
				l.failedAt = time.Now()
			}
			l.mu.Unlock()
			if due {
				log.Printf("[isolation] idle sweep failed: %v; trying again every few seconds", err)
			}
			l.sweepFailed(err)
		}
	}
}

// warnLong reports leases held for more than an hour, at most once an hour.
func (l *Leases) warnLong() {
	l.mu.Lock()
	n, since, warned := l.n, l.busySince, l.warnedAt
	l.mu.Unlock()
	if n == 0 || time.Since(since) < time.Hour || time.Since(warned) < time.Hour {
		return
	}
	l.mu.Lock()
	l.warnedAt = time.Now()
	l.mu.Unlock()
	log.Printf("[isolation] idle sweep skipped: %d leases held for > 1 h", n)
}

// Drain is the graceful stop: wait up to wait for the leases to be released, then sweep
// unconditionally.
func (l *Leases) Drain(ctx context.Context, wait time.Duration) (SweepResult, error) {
	deadline := time.Now().Add(wait)
	for l.Held() > 0 && time.Now().Before(deadline) {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			return SweepResult{}, ctx.Err()
		}
	}
	return l.Sweep(ctx, true)
}

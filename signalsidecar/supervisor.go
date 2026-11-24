package signalsidecar

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrChildDown is returned while no signal-cli process is running.
	ErrChildDown = errors.New("signal-cli is not running")
	// ErrLinkPending is returned for every call except the pending finishLink
	// itself while a link waits for the phone.
	ErrLinkPending = errors.New("a device link is pending")
	// ErrStopped is returned by control calls after Run has returned.
	ErrStopped = errors.New("supervisor stopped")
)

// Config is the fixed way to run the child. Nothing in it is read from the
// process environment: Env is the complete environment of the child.
type Config struct {
	Command string
	Args    []string
	// Env is the whole environment of the child (KEY=value). Nil or empty
	// means an empty environment, never the environment of this process.
	Env []string

	// CallTimeout bounds each call except finishLink (default 20 s).
	CallTimeout time.Duration
	// StartTimeout bounds the version check after a start (default 20 s).
	StartTimeout time.Duration
	// MinBackoff is the least time between two starts (default 2 s); it
	// doubles after each failed run up to MaxBackoff (default 30 s).
	MinBackoff, MaxBackoff time.Duration
	// StableAfter is how long a child must have run for the doubling to reset
	// (default 30 s).
	StableAfter time.Duration
	// StopGrace is the wait for exit after the input stream is closed, and
	// TermGrace the wait after SIGTERM, before the child is killed (5 s each).
	StopGrace, TermGrace time.Duration
	// TimeoutLimit is the number of timeouts in a row that restart the child
	// (default 3).
	TimeoutLimit int
	// MaxLine is the longest line kept from the child (default 1 MiB).
	MaxLine int
	// Logf receives single log lines; they never hold link URIs, message text
	// or full numbers. Default: the standard logger.
	Logf func(format string, args ...any)
}

func (c *Config) setDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.CallTimeout, 20*time.Second)
	def(&c.StartTimeout, 20*time.Second)
	def(&c.MinBackoff, 2*time.Second)
	def(&c.MaxBackoff, 30*time.Second)
	def(&c.StableAfter, 30*time.Second)
	def(&c.StopGrace, 5*time.Second)
	def(&c.TermGrace, 5*time.Second)
	if c.MaxBackoff < c.MinBackoff {
		c.MaxBackoff = c.MinBackoff
	}
	if c.TimeoutLimit <= 0 {
		c.TimeoutLimit = 3
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	if c.Env == nil {
		c.Env = []string{}
	}
}

// JavaCommand returns the fixed command line of signal-cli: the JVM started
// directly (no shell), the native libraries loaded from a fixed directory.
func JavaCommand(java, nativeDir, libDir, dataDir string) (command string, args []string) {
	return java, []string{
		"--enable-native-access=ALL-UNNAMED",
		"-Djava.library.path=" + nativeDir,
		"-Dorg.sqlite.lib.path=" + nativeDir,
		"-Dorg.sqlite.lib.name=libsqlitejdbc.so",
		"-cp", libDir + "/*",
		"org.asamk.signal.Main",
		"-c", dataDir,
		"--scrub-log",
		"jsonRpc",
		"--receive-mode", "on-start",
		"--ignore-attachments",
		"--ignore-stories",
		"--ignore-avatars",
		"--ignore-stickers",
	}
}

// ChildEnv is the whole environment of the child: four variables and nothing
// else, in particular no database DSN.
func ChildEnv(home, path, lang, tmpdir string) []string {
	return []string{"HOME=" + home, "PATH=" + path, "LANG=" + lang, "TMPDIR=" + tmpdir}
}

// Status is a snapshot of the supervisor.
type Status struct {
	Up      bool
	Paused  bool
	Version string
	// Starts counts the child processes started so far.
	Starts uint64
	// LastError is the scrubbed reason of the last failed start or exit.
	LastError string
}

type child struct {
	cmd      *exec.Cmd
	stdin    *os.File
	outR     *os.File
	errR     *os.File
	client   *Client
	version  string
	exited   chan struct{}
	exitErr  error
	kill     chan struct{} // asks Run to restart this child
	timeouts int           // guarded by Supervisor.mu
}

type ctlOp int

const (
	opRestart ctlOp = iota
	opPause
	opResume
)

type ctlReq struct {
	op   ctlOp
	done chan struct{}
}

// Supervisor runs signal-cli as a child, restarts it when it dies, and is the
// only way to make calls to it. It makes no unlink or removal call of any
// kind: unlinking is Pause, deleting the data directory, Resume.
type Supervisor struct {
	cfg Config

	ctl     chan ctlReq
	stopped chan struct{}

	mu          sync.Mutex
	running     bool
	child       *child
	linkPending bool
	paused      bool
	version     string
	starts      uint64
	gen         uint64 // counts children that became ready
	lastErr     string
	changed     chan struct{}
}

// NewSupervisor returns a supervisor; Run starts the child.
func NewSupervisor(cfg Config) *Supervisor {
	cfg.setDefaults()
	return &Supervisor{
		cfg:     cfg,
		ctl:     make(chan ctlReq),
		stopped: make(chan struct{}),
		changed: make(chan struct{}),
	}
}

func (s *Supervisor) logf(format string, args ...any) { s.cfg.Logf(format, args...) }

// changedLocked wakes everything that waits for a state change.
func (s *Supervisor) changedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Status returns a snapshot.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Up: s.child != nil, Paused: s.paused, Version: s.version, Starts: s.starts, LastError: s.lastErr}
}

// WaitUp returns when a child is running and has answered its version check.
func (s *Supervisor) WaitUp(ctx context.Context) error { return s.waitGen(ctx, 0) }

func (s *Supervisor) waitGen(ctx context.Context, minGen uint64) error {
	for {
		s.mu.Lock()
		ok := s.child != nil && s.gen >= minGen
		ch := s.changed
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopped:
			return ErrStopped
		}
	}
}

// Restart stops the child (a pending finishLink ends with ErrClosed) and
// returns once a new child is up.
func (s *Supervisor) Restart(ctx context.Context) error {
	s.mu.Lock()
	g := s.gen
	s.mu.Unlock()
	if err := s.control(ctx, opRestart); err != nil {
		return err
	}
	return s.waitGen(ctx, g+1)
}

// Pause stops the child and keeps it stopped until Resume. It returns when the
// process is gone, so the data directory can be changed.
func (s *Supervisor) Pause(ctx context.Context) error { return s.control(ctx, opPause) }

// Resume starts the child again after Pause. Use WaitUp to wait for it.
func (s *Supervisor) Resume(ctx context.Context) error { return s.control(ctx, opResume) }

func (s *Supervisor) control(ctx context.Context, op ctlOp) error {
	req := ctlReq{op: op, done: make(chan struct{})}
	select {
	case s.ctl <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopped:
		return ErrStopped
	}
	select {
	case <-req.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopped:
		return ErrStopped
	}
}

// Version asks the running child for its version.
func (s *Supervisor) Version(ctx context.Context) (v string, err error) {
	err = s.do(ctx, func(ctx context.Context, c *Client) (e error) { v, e = c.Version(ctx); return })
	return
}

// ListAccounts lists the accounts of the data directory.
func (s *Supervisor) ListAccounts(ctx context.Context) (a []Account, err error) {
	err = s.do(ctx, func(ctx context.Context, c *Client) (e error) { a, e = c.ListAccounts(ctx); return })
	return
}

// StartLink begins a device link.
func (s *Supervisor) StartLink(ctx context.Context) (uri string, err error) {
	err = s.do(ctx, func(ctx context.Context, c *Client) (e error) { uri, e = c.StartLink(ctx); return })
	return
}

// ListGroups lists the groups of the account.
func (s *Supervisor) ListGroups(ctx context.Context, account string) (g []Group, err error) {
	err = s.do(ctx, func(ctx context.Context, c *Client) (e error) { g, e = c.ListGroups(ctx, account); return })
	return
}

// Send sends a text message to one group.
func (s *Supervisor) Send(ctx context.Context, account, groupID, message string) (r SendResult, err error) {
	err = s.do(ctx, func(ctx context.Context, c *Client) (e error) { r, e = c.Send(ctx, account, groupID, message); return })
	return
}

// SendNumbers sends a text message to phone numbers.
func (s *Supervisor) SendNumbers(ctx context.Context, account string, numbers []string, message string) (r SendResult, err error) {
	err = s.do(ctx, func(ctx context.Context, c *Client) (e error) {
		r, e = c.SendNumbers(ctx, account, numbers, message)
		return
	})
	return
}

// FinishLink waits for the phone. Until it returns, every other call is
// refused with ErrLinkPending. Its own timeout is the context the caller
// passes and never counts towards restarting the child. Only one link can be
// pending.
func (s *Supervisor) FinishLink(ctx context.Context, uri, deviceName string) (Account, error) {
	s.mu.Lock()
	ch := s.child
	if ch == nil {
		s.mu.Unlock()
		return Account{}, ErrChildDown
	}
	if s.linkPending {
		s.mu.Unlock()
		return Account{}, ErrLinkPending
	}
	s.linkPending = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.linkPending = false
		s.mu.Unlock()
	}()
	return ch.client.FinishLink(ctx, uri, deviceName)
}

// do runs one call with the call timeout and applies the rule that several
// timeouts in a row restart the child.
func (s *Supervisor) do(ctx context.Context, fn func(context.Context, *Client) error) error {
	s.mu.Lock()
	ch, pending := s.child, s.linkPending
	s.mu.Unlock()
	if ch == nil {
		return ErrChildDown
	}
	if pending {
		return ErrLinkPending
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	err := fn(cctx, ch.client)

	var re *RPCError
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case err == nil || errors.As(err, &re):
		ch.timeouts = 0 // the child answered
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		ch.timeouts++
		if ch.timeouts >= s.cfg.TimeoutLimit {
			ch.timeouts = 0
			s.logf("[signal] %d calls in a row timed out; restarting signal-cli", s.cfg.TimeoutLimit)
			select {
			case ch.kill <- struct{}{}:
			default:
			}
		}
	}
	return err
}

// Run starts the child and keeps it running until ctx ends. It returns after
// the child has been stopped; nothing is restarted afterwards.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("supervisor already running")
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.child = nil
		s.changedLocked()
		s.mu.Unlock()
		close(s.stopped)
	}()

	var lastStart time.Time
	fails := 0
	paused := false
	for {
		// A request that arrives while no child runs.
		apply := func(r ctlReq) {
			switch r.op {
			case opPause:
				paused = true
			case opResume:
				paused = false
			}
			s.mu.Lock()
			s.paused = paused
			s.changedLocked()
			s.mu.Unlock()
			close(r.done)
		}
		if paused {
			select {
			case <-ctx.Done():
				return nil
			case r := <-s.ctl:
				apply(r)
			}
			continue
		}
		if wait := s.backoff(lastStart, fails); wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case r := <-s.ctl:
				t.Stop()
				apply(r)
				continue
			case <-t.C:
			}
		}
		lastStart = time.Now()
		ch, err := s.start(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fails++
			s.setErr(err)
			s.logf("[signal] signal-cli did not start: %s", Scrub(err.Error()))
			continue
		}
		s.mu.Lock()
		s.child, s.version, s.lastErr = ch, ch.version, ""
		s.gen++
		s.changedLocked()
		s.mu.Unlock()
		s.logf("[signal] signal-cli %s is up", Scrub(ch.version))

		var req *ctlReq
	supervise:
		for {
			select {
			case <-ch.exited:
				s.setErr(fmt.Errorf("signal-cli exited: %v", ch.exitErr))
				s.logf("[signal] signal-cli exited: %s", Scrub(fmt.Sprint(ch.exitErr)))
				if time.Since(lastStart) >= s.cfg.StableAfter {
					fails = 0
				} else {
					fails++
				}
				break supervise
			case <-ch.kill:
				fails++
				break supervise
			case r := <-s.ctl:
				if r.op == opResume { // already running
					apply(r)
					continue
				}
				req = &r
				fails = 0
				break supervise
			case <-ctx.Done():
				break supervise
			}
		}
		s.mu.Lock()
		s.child = nil
		s.changedLocked()
		s.mu.Unlock()
		s.teardown(ch)
		if req != nil {
			apply(*req)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (s *Supervisor) setErr(err error) {
	s.mu.Lock()
	s.lastErr = Scrub(err.Error())
	s.changedLocked()
	s.mu.Unlock()
}

// backoff is the time to wait before the next start.
func (s *Supervisor) backoff(lastStart time.Time, fails int) time.Duration {
	if lastStart.IsZero() {
		return 0
	}
	d := s.cfg.MinBackoff
	for i := 0; i < fails && d < s.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > s.cfg.MaxBackoff {
		d = s.cfg.MaxBackoff
	}
	return time.Until(lastStart.Add(d))
}

// start spawns the child and waits for its version answer.
func (s *Supervisor) start(ctx context.Context) (*child, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("pipe: %w", err)
	}
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...)
	cmd.Env = s.cfg.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	startErr := cmd.Start()
	inR.Close()
	outW.Close()
	errW.Close()
	if startErr != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, fmt.Errorf("start: %w", startErr)
	}
	s.mu.Lock()
	s.starts++
	s.mu.Unlock()

	ch := &child{
		cmd: cmd, stdin: inW, outR: outR, errR: errR,
		exited: make(chan struct{}), kill: make(chan struct{}, 1),
	}
	go func() {
		ch.exitErr = cmd.Wait()
		close(ch.exited)
	}()
	go s.logStderr(errR)
	ch.client = NewClient(outR, inW, ClientOptions{MaxLine: s.cfg.MaxLine, Logf: s.cfg.Logf})

	vctx, cancel := context.WithTimeout(ctx, s.cfg.StartTimeout)
	defer cancel()
	v, err := ch.client.Version(vctx)
	if err != nil {
		s.teardown(ch)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("version check: %w", err)
	}
	ch.version = v
	return ch, nil
}

// logStderr passes the child's error output through the scrubber, line by
// line, cut to a fixed length.
func (s *Supervisor) logStderr(r *os.File) {
	br := bufio.NewReaderSize(r, 16<<10)
	var buf []byte
	for {
		line, over, err := readBoundedLine(br, buf[:0], 4<<10)
		if err != nil {
			return
		}
		buf = line
		if over {
			s.logf("[signal] child: (long line skipped)")
			continue
		}
		if text := strings.TrimSpace(Scrub(string(line))); text != "" {
			s.logf("[signal] child: %s", text)
		}
	}
}

// teardown stops the child: end of input first, then SIGTERM, then kill.
func (s *Supervisor) teardown(ch *child) {
	ch.stdin.Close()
	if !waitClosed(ch.exited, s.cfg.StopGrace) {
		_ = ch.cmd.Process.Signal(syscall.SIGTERM)
		if !waitClosed(ch.exited, s.cfg.TermGrace) {
			_ = ch.cmd.Process.Kill()
			<-ch.exited
		}
	}
	ch.client.Close()
	ch.outR.Close()
	ch.errR.Close()
}

func waitClosed(c <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c:
		return true
	case <-t.C:
		return false
	}
}

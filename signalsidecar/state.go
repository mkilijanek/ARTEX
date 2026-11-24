package signalsidecar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// States the sidecar reports.
const (
	StateUnknown  = "unknown"
	StateUnlinked = "unlinked"
	StateLinking  = "linking"
	StateLinked   = "linked"
	StateError    = "error"
)

// remoteLocalOnly is what the sidecar reports for an unlink: it makes no call
// to Signal's servers, so the device stays registered until the operator
// removes it on the phone.
const remoteLocalOnly = "local_only"

const (
	maxGroups     = 200
	maxGroupsJSON = 120 << 10 // the database refuses more than 128 KiB
)

var numberRe = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// MachineConfig tunes the state machine; the zero value gives the production
// intervals.
type MachineConfig struct {
	// DataDir is the data directory of signal-cli; unlinking empties it.
	DataDir string
	// SidecarVersion is reported in the heartbeat.
	SidecarVersion string
	// PollEvery is how often the view is read (default 2 s).
	PollEvery time.Duration
	// HeartbeatEvery is the longest time between two heartbeats (default 10 s).
	HeartbeatEvery time.Duration
	// StaleAfter is the age of the last successful read after which the
	// configuration counts as stale and sends are refused (default 60 s).
	StaleAfter time.Duration
	// GroupsEvery is the interval of the periodic group refresh (default 10 min).
	GroupsEvery time.Duration
	// GroupsRetry is the wait after a failed group refresh (default 30 s).
	GroupsRetry time.Duration
	// Logf receives single log lines without secrets. Default: discard.
	Logf func(format string, args ...any)

	// linkTTLCap, set by tests, shortens the link time to live below the
	// 30 s the database allows.
	linkTTLCap time.Duration
}

func (c *MachineConfig) setDefaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.PollEvery, 2*time.Second)
	def(&c.HeartbeatEvery, 10*time.Second)
	def(&c.StaleAfter, 60*time.Second)
	def(&c.GroupsEvery, 10*time.Minute)
	def(&c.GroupsRetry, 30*time.Second)
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

type linkResult struct {
	acct Account
	err  error
}

// linkOp is the link that is waiting for the phone. Its address is held only
// by the goroutine that waits for it.
type linkOp struct {
	gen    int64
	cancel context.CancelFunc
	res    chan linkResult
}

// Machine runs the state machine: it follows the commands in the database,
// drives signal-cli through the supervisor and reports through the heartbeat.
// All decisions are taken in Run; the exported read methods are safe to call
// from other goroutines.
type Machine struct {
	cfg MachineConfig
	st  *Store
	sup *Supervisor

	mu        sync.Mutex // guards the fields the readers use
	state     string
	number    string
	aci       string
	errText   string // the reason of state error
	childDown bool
	view      ConfigView
	viewAt    time.Time

	// Owned by the goroutine that runs Run (AwaitAuth runs before it).
	runCtx     context.Context
	handled    int64
	note       string // one-shot error text for the next heartbeat
	remote     string
	dirty      bool
	lastBeat   time.Time
	link       *linkOp
	seenStarts uint64
	needGroups bool
	groupsAt   time.Time
	groupsNext time.Time // no group refresh before this time
	dbDown     bool
	warned     map[string]time.Time
}

// NewMachine returns a machine; AwaitAuth and Run start it.
func NewMachine(cfg MachineConfig, st *Store, sup *Supervisor) *Machine {
	cfg.setDefaults()
	return &Machine{cfg: cfg, st: st, sup: sup, state: StateUnknown, warned: map[string]time.Time{}}
}

func (m *Machine) logf(format string, args ...any) { m.cfg.Logf(format, args...) }

// warn logs a line at most once a minute per key.
func (m *Machine) warn(key, format string, args ...any) {
	if t, ok := m.warned[key]; ok && time.Since(t) < time.Minute {
		return
	}
	m.warned[key] = time.Now()
	m.logf(format, args...)
}

// setState records the state; a change makes the next heartbeat due at once.
func (m *Machine) setState(state, number, aci, errText string) {
	m.mu.Lock()
	changed := m.state != state || m.number != number || m.errText != errText
	m.state, m.number, m.aci, m.errText = state, number, aci, errText
	m.mu.Unlock()
	if changed {
		m.dirty = true
	}
}

func (m *Machine) current() (state, number, aci, errText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state, m.number, m.aci, m.errText
}

func (m *Machine) setView(v ConfigView) {
	m.mu.Lock()
	m.view, m.viewAt = v, time.Now()
	m.mu.Unlock()
}

func (m *Machine) lastView() ConfigView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.view
}

func (m *Machine) dbFailed(err error) {
	if !m.dbDown {
		m.dbDown = true
		m.logf("[signal] the database is not reachable: %s", Scrub(err.Error()))
	}
}

func (m *Machine) dbOK() {
	if m.dbDown {
		m.dbDown = false
		m.dirty = true
		m.logf("[signal] the database is reachable again")
	}
}

// AwaitAuth waits until the database says that it checks passwords. Until then
// it heartbeats an error and returns nothing: signal-cli must not be started.
func (m *Machine) AwaitAuth(ctx context.Context) error {
	t := time.NewTicker(m.cfg.PollEvery)
	defer t.Stop()
	for {
		v, err := m.st.Config(ctx)
		switch {
		case err != nil:
			m.dbFailed(err)
		case v.DBAuthOK:
			m.dbOK()
			m.setView(v)
			m.setState(StateUnknown, "", "", "")
			return nil
		default:
			m.dbOK()
			m.setView(v)
			m.setState(StateError, "", "", "the server has not confirmed password authentication")
			m.beat(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Run follows the database until ctx ends. The supervisor runs on its own.
func (m *Machine) Run(ctx context.Context) error {
	m.runCtx = ctx
	t := time.NewTicker(m.cfg.PollEvery)
	defer t.Stop()
	m.step(ctx)
	for {
		var lc <-chan linkResult
		if m.link != nil {
			lc = m.link.res
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			m.step(ctx)
		case r := <-lc:
			m.onLinkDone(ctx, r)
		}
	}
}

// step is one pass over the database state.
func (m *Machine) step(ctx context.Context) {
	v, err := m.st.Config(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.dbFailed(err)
		}
		return
	}
	m.dbOK()
	m.setView(v)

	if m.link != nil {
		m.watchLink(ctx, v)
		m.beatIfDue(ctx)
		return
	}

	stat := m.sup.Status()
	if !stat.Up {
		m.childLost(stat)
		// An unlink does not need a running child, and it is the way out of a
		// data directory that signal-cli cannot start with.
		if v.Desired == "unlink" && v.Gen > max(m.handled, v.HandledGen) {
			m.unlink(ctx, v.Gen)
		}
		m.beatIfDue(ctx)
		return
	}
	// A child that started since the last look, or a sidecar that just started:
	// the state comes from listAccounts and from nothing else, and a link is
	// never resumed.
	resumed := m.isChildDown() || stat.Starts != m.seenStarts
	if resumed && !m.observe(ctx) {
		m.beatIfDue(ctx)
		return
	}

	if v.Gen > max(m.handled, v.HandledGen) {
		m.command(ctx, v, resumed)
	}
	m.groupsStep(ctx, v)
	m.beatIfDue(ctx)
}

func (m *Machine) isChildDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.childDown
}

func (m *Machine) childLost(stat Status) {
	m.mu.Lock()
	first := !m.childDown
	m.childDown = true
	m.mu.Unlock()
	if first {
		m.logf("[signal] signal-cli is not running")
	}
	text := "signal-cli is not running"
	if stat.LastError != "" {
		text += ": " + stat.LastError
	}
	m.setState(StateError, "", "", text)
}

// observe sets the state from listAccounts: none means unlinked, one means
// linked (the operator confirms it separately), more means error. It returns
// false when the accounts could not be listed.
func (m *Machine) observe(ctx context.Context) bool {
	accts, err := m.sup.ListAccounts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.setState(StateError, "", "", "cannot list the accounts: "+Scrub(err.Error()))
		}
		return false
	}
	m.mu.Lock()
	m.childDown = false
	prev := m.state
	m.mu.Unlock()
	m.seenStarts = m.sup.Status().Starts
	switch {
	case len(accts) == 0:
		m.setState(StateUnlinked, "", "", "")
	case len(accts) > 1:
		m.setState(StateError, "", "", "more than one account in the data directory; unlink to start again")
	case !numberRe.MatchString(accts[0].Number):
		m.setState(StateError, "", "", "the account in the data directory has no usable number; unlink to start again")
	default:
		m.setState(StateLinked, accts[0].Number, accts[0].ACI, "")
		if prev != StateLinked {
			m.needGroups = true
		}
	}
	return true
}

// command runs the pending command of the view.
func (m *Machine) command(ctx context.Context, v ConfigView, resumed bool) {
	switch v.Desired {
	case "unlink":
		m.unlink(ctx, v.Gen)
	case "link":
		state, _, _, _ := m.current()
		switch {
		case resumed:
			m.finish(ctx, finishOpts{gen: v.Gen, note: "link interrupted"})
		case state != StateUnlinked:
			m.finish(ctx, finishOpts{gen: v.Gen, note: "a link was requested while an account is present"})
		default:
			m.startLink(ctx, v)
		}
	default: // idle: a command that ended without the sidecar, or a cancel
		m.finish(ctx, finishOpts{gen: v.Gen, note: "link cancelled"})
	}
}

type finishOpts struct {
	gen int64
	// note is the error text reported when the end state is unlinked.
	note string
	// failure forces the state error with this text.
	failure string
	remote  string
}

// finish ends a command: the state is read from listAccounts, handled_gen
// advances and the heartbeat goes out. It is the one place where a command
// ends, so a command always advances handled_gen.
func (m *Machine) finish(ctx context.Context, o finishOpts) {
	m.observe(ctx)
	if o.failure != "" {
		m.setState(StateError, "", "", o.failure)
	}
	state, _, _, _ := m.current()
	m.handled = max(m.handled, o.gen)
	m.note = ""
	if state == StateUnlinked {
		m.note = Scrub(o.note)
		m.remote = o.remote
	}
	m.dirty = true
	m.beat(ctx)
}

// restartChild restarts signal-cli, which aborts a pending finishLink.
func (m *Machine) restartChild(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := m.sup.Restart(rctx); err != nil && ctx.Err() == nil {
		m.logf("[signal] restarting signal-cli failed: %s", Scrub(err.Error()))
	}
}

// startLink starts a link: startLink, then the address into the database, then
// finishLink in the background.
func (m *Machine) startLink(ctx context.Context, v ConfigView) {
	uri, err := m.sup.StartLink(ctx)
	if err != nil {
		m.finish(ctx, finishOpts{gen: v.Gen, note: "could not start the link: " + Scrub(err.Error())})
		return
	}
	ok, err := m.st.SetLink(ctx, v.Gen, uri)
	if err != nil || !ok {
		note := "the link request is no longer current"
		if err != nil {
			note = "the link address could not be stored"
			m.dbFailed(err)
		}
		m.restartChild(ctx)
		m.finish(ctx, finishOpts{gen: v.Gen, note: note})
		return
	}
	ttl := time.Duration(v.LinkTTLSeconds) * time.Second
	if m.cfg.linkTTLCap > 0 && ttl > m.cfg.linkTTLCap {
		ttl = m.cfg.linkTTLCap
	}
	lctx, cancel := context.WithTimeout(m.runCtx, ttl)
	op := &linkOp{gen: v.Gen, cancel: cancel, res: make(chan linkResult, 1)}
	m.link = op
	m.setState(StateLinking, "", "", "")
	go func(name string) {
		acct, err := m.sup.FinishLink(lctx, uri, name)
		op.res <- linkResult{acct: acct, err: err}
	}(v.DeviceName)
	m.beat(ctx)
}

// watchLink looks at the commands while a link waits for the phone.
func (m *Machine) watchLink(ctx context.Context, v ConfigView) {
	op := m.link
	switch {
	case v.Gen > op.gen:
		m.abortLink(ctx)
	case v.Desired != "link" && v.Gen == op.gen:
		m.cancelLink(ctx)
	}
}

// stopLink ends the wait for the phone and restarts the child, which is the
// only way to abort the link in signal-cli. An account that the phone
// completed at that moment is kept by signal-cli and found by listAccounts.
func (m *Machine) stopLink(ctx context.Context) {
	op := m.link
	m.link = nil
	op.cancel()
	m.restartChild(ctx)
	select {
	case <-op.res:
	case <-time.After(10 * time.Second):
	}
}

func (m *Machine) cancelLink(ctx context.Context) {
	gen := m.link.gen
	m.stopLink(ctx)
	m.finish(ctx, finishOpts{gen: gen, note: "link cancelled"})
}

// abortLink drops a link that a newer command overtakes; the newer command
// advances handled_gen.
func (m *Machine) abortLink(ctx context.Context) {
	m.stopLink(ctx)
	m.observe(ctx)
}

// onLinkDone handles the end of finishLink.
func (m *Machine) onLinkDone(ctx context.Context, r linkResult) {
	op := m.link
	m.link = nil
	op.cancel()
	if r.err == nil && numberRe.MatchString(r.acct.Number) {
		m.setState(StateLinked, r.acct.Number, r.acct.ACI, "")
		m.needGroups = true
		m.handled = max(m.handled, op.gen)
		m.note = ""
		m.seenStarts = m.sup.Status().Starts
		m.dirty = true
		m.beat(ctx)
		return
	}
	var note string
	switch {
	case r.err == nil:
		note = "the link ended without a usable number"
	case isLinkExpiry(r.err):
		note = "link expired"
	case errors.Is(r.err, ErrClosed), errors.Is(r.err, ErrChildDown):
		note = "link interrupted"
	default:
		note = "link failed: " + Scrub(r.err.Error())
	}
	m.restartChild(ctx)
	m.finish(ctx, finishOpts{gen: op.gen, note: note})
}

// isLinkExpiry reports whether finishLink gave up waiting: the time to live
// passed, or signal-cli answered that the request timed out or is unknown.
func isLinkExpiry(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var re *RPCError
	if errors.As(err, &re) && re.Code == -1 {
		msg := strings.ToLower(re.Message)
		return strings.Contains(msg, "link request timed out") || strings.Contains(msg, "unknown device link uri")
	}
	return false
}

// unlink wipes the data directory with the child stopped. It makes no call to
// Signal: the device stays registered there until it is removed on the phone.
func (m *Machine) unlink(ctx context.Context, gen int64) {
	if m.link != nil {
		m.abortLink(ctx)
	}
	failure := ""
	if err := m.sup.Pause(ctx); err != nil {
		failure = "could not stop signal-cli: " + Scrub(err.Error())
	} else {
		if err := wipeDir(m.cfg.DataDir); err != nil {
			failure = "could not delete the account data: " + Scrub(err.Error())
		}
		if err := m.sup.Resume(ctx); err != nil && failure == "" {
			failure = "could not start signal-cli: " + Scrub(err.Error())
		}
	}
	if failure == "" {
		uctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := m.sup.WaitUp(uctx); err != nil {
			failure = "signal-cli did not come up after the unlink: " + Scrub(err.Error())
		}
		cancel()
	}
	m.finish(ctx, finishOpts{gen: gen, failure: failure, remote: remoteLocalOnly})
}

// wipeDir deletes everything inside dir, not dir itself (it may be a mount
// point). An empty path, the root and anything that is not a directory are
// refused.
func wipeDir(dir string) error {
	if dir == "" {
		return errors.New("no data directory is configured")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve the data directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve the data directory: %w", err)
	}
	if resolved == "" || filepath.Dir(resolved) == resolved {
		return errors.New("refusing to empty the root directory")
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("the data directory is not a directory")
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(resolved, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// groupsStep refreshes the group list after a link, on request and every
// GroupsEvery.
func (m *Machine) groupsStep(ctx context.Context, v ConfigView) {
	state, number, _, _ := m.current()
	if state != StateLinked || time.Now().Before(m.groupsNext) {
		return
	}
	if !(m.needGroups || v.GroupsRefreshGen > v.GroupsGen || time.Since(m.groupsAt) >= m.cfg.GroupsEvery) {
		return
	}
	retry := func(format string, args ...any) {
		m.groupsNext = time.Now().Add(m.cfg.GroupsRetry)
		m.warn("groups", format, args...)
	}
	groups, err := m.sup.ListGroups(ctx, number)
	if err != nil {
		var re *RPCError
		if errors.As(err, &re) && re.Code == -32602 {
			m.setState(StateError, "", "", "the account is no longer linked; unlink and link again")
		}
		retry("[signal] listing the groups failed: %s", Scrub(err.Error()))
		return
	}
	ok, err := m.st.SetGroups(ctx, v.GroupsRefreshGen, groupEntries(groups))
	switch {
	case err != nil:
		retry("[signal] storing the groups failed: %s", Scrub(err.Error()))
	case !ok:
		retry("[signal] the database did not take the group list")
	default:
		m.needGroups = false
		m.groupsAt = time.Now()
	}
}

// groupEntries turns listGroups into the stored form: member, non-blocked
// groups with a 32-byte id, in ARTEX's id form, names cleaned and cut.
func groupEntries(groups []Group) []GroupEntry {
	out := make([]GroupEntry, 0, len(groups))
	for _, g := range groups {
		if !g.IsMember || g.IsBlocked {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(g.ID)
		if err != nil || len(raw) != 32 {
			continue
		}
		out = append(out, GroupEntry{
			ID:   "group." + base64.StdEncoding.EncodeToString([]byte(g.ID)),
			Name: CleanGroupName(g.Name),
		})
		if len(out) == maxGroups {
			break
		}
	}
	for len(out) > 0 {
		raw, err := json.Marshal(out)
		if err != nil || len(raw) <= maxGroupsJSON {
			break
		}
		out = out[:len(out)-1]
	}
	return out
}

func (m *Machine) beatIfDue(ctx context.Context) {
	if m.dirty || time.Since(m.lastBeat) >= m.cfg.HeartbeatEvery {
		m.beat(ctx)
	}
}

// beat sends the heartbeat. A failure is logged and retried at the next pass;
// the local state is the truth until then.
func (m *Machine) beat(ctx context.Context) {
	state, number, aci, errText := m.current()
	if state == StateUnknown {
		return
	}
	text := m.note
	if state == StateError {
		text = errText
	}
	v := m.lastView()
	hb := Heartbeat{
		State:          state,
		HandledGen:     max(m.handled, v.HandledGen),
		Number:         number,
		ACI:            aci,
		Error:          Scrub(text),
		RemoteUnlink:   m.remote,
		SidecarVersion: m.cfg.SidecarVersion,
		CLIVersion:     m.sup.Status().Version,
	}
	if err := m.st.Heartbeat(ctx, hb); err != nil {
		m.dirty = true
		if ctx.Err() != nil {
			return
		}
		var de *DBError
		if errors.As(err, &de) && de.SQLState == "" {
			m.dbFailed(err)
		} else {
			m.warn("heartbeat", "[signal] heartbeat refused: %s", err.Error())
		}
		return
	}
	m.dbOK()
	m.dirty = false
	m.lastBeat = time.Now()
	m.note, m.remote = "", ""
}

// ready reports the confirmed account the sidecar may send from, or the reason
// it may not.
func (m *Machine) ready() (account string, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.childDown || !m.sup.Status().Up:
		return "", "child_down"
	case time.Since(m.viewAt) > m.cfg.StaleAfter:
		return "", "config_stale"
	case m.state != StateLinked:
		return "", "not_linked"
	case m.view.ConfirmedAccount == "" || m.view.ConfirmedAccount != m.number:
		return "", "not_confirmed"
	}
	return m.number, ""
}

package signalsidecar

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// mrig is a database, a scripted signal-cli and the means to run one sidecar
// (supervisor and state machine) against them, more than once.
type mrig struct {
	*dbRig
	r  *rig
	cc *pgx.ConnConfig // what the store connects with
}

func newMRig(t *testing.T, label string, sc map[string]any) *mrig {
	t.Helper()
	d := newDBRig(t, label)
	if sharedFakeBin == "" {
		t.Fatal("the fake CLI was not built")
	}
	r := &rig{t: t, bin: sharedFakeBin, dir: t.TempDir(), logs: &logSink{}}
	r.data = filepath.Join(r.dir, "data")
	if err := os.Mkdir(r.data, 0o700); err != nil {
		t.Fatal(err)
	}
	r.scenario = filepath.Join(r.dir, "scenario.json")
	r.calls = filepath.Join(r.dir, "calls")
	r.starts = filepath.Join(r.dir, "starts")
	r.dump = filepath.Join(r.dir, "dump.json")
	r.set(sc)
	return &mrig{dbRig: d, r: r, cc: d.cc}
}

// sidecar is one running sidecar.
type sidecar struct {
	sup  *Supervisor
	m    *Machine
	stop func()
	done chan struct{}
}

// exited reports whether the state machine has returned.
func (s *sidecar) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func fastIntervals(c *MachineConfig) {
	c.PollEvery = 100 * time.Millisecond
	c.HeartbeatEvery = 400 * time.Millisecond
	c.GroupsRetry = 200 * time.Millisecond
}

func defaultIntervals(c *MachineConfig) {
	c.PollEvery = 0
	c.HeartbeatEvery = 0
}

func (mr *mrig) start(mc func(*MachineConfig), sc func(*Config)) *sidecar {
	mr.t.Helper()
	scfg := mr.r.config()
	if sc != nil {
		sc(&scfg)
	}
	sup, stopSup := mr.r.run(scfg)
	cfg := MachineConfig{DataDir: mr.r.data, SidecarVersion: "test-version", Logf: mr.r.logs.logf}
	fastIntervals(&cfg)
	if mc != nil {
		mc(&cfg)
	}
	store := NewStore(mr.cc, mr.r.logs.logf)
	m := NewMachine(cfg, store, sup)
	ctx, cancel := context.WithCancel(context.Background())
	s := &sidecar{sup: sup, m: m, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		if err := m.AwaitAuth(ctx); err == nil {
			_ = m.Run(ctx)
		}
	}()
	var once sync.Once
	s.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-s.done:
			case <-time.After(20 * time.Second):
				mr.t.Error("the state machine did not stop")
			}
			stopSup()
			store.Close()
		})
	}
	mr.t.Cleanup(s.stop)
	return s
}

func (mr *mrig) callCount(method string) int {
	n := 0
	for _, m := range mr.r.recordedMethods() {
		if m == method {
			n++
		}
	}
	return n
}

func (mr *mrig) waitState(state string) statusRow {
	mr.t.Helper()
	return mr.waitStatus("state "+state, 15*time.Second, func(s statusRow) bool { return s.State == state })
}

func (mr *mrig) logsHold(secrets ...string) {
	mr.t.Helper()
	logs := mr.r.logs.all()
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			mr.t.Errorf("the log holds %q:\n%s", s, logs)
		}
	}
}

// link runs a link to the end and leaves the row reconciled.
func (mr *mrig) link() int64 {
	mr.t.Helper()
	mr.waitState("unlinked")
	gen := mr.requestLink()
	mr.waitStatus("the finished link", 15*time.Second, func(s statusRow) bool { return s.State == "linked" && s.Handled == gen })
	mr.reconcile()
	return gen
}

func (mr *mrig) dataEntries() []string {
	mr.t.Helper()
	entries, err := os.ReadDir(mr.r.data)
	if err != nil {
		mr.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

var linkURIRe = regexp.MustCompile(`^sgnl://linkdevice\?[A-Za-z0-9%&=_.~+/,-]{1,400}$`)

func completes(afterMS int) map[string]any {
	return map[string]any{"finish": map[string]any{"mode": "after", "after_ms": afterMS, "number": testNumber}}
}

func TestSidecarLinksAnAccount(t *testing.T) {
	mr := newMRig(t, "sclink", completes(3000))
	mr.start(defaultIntervals, nil)
	mr.waitState("unlinked")

	gen := mr.requestLink()
	st := mr.waitStatus("the link address", 5*time.Second, func(s statusRow) bool { return s.State == "linking" })
	if !st.URI.Valid || !linkURIRe.MatchString(st.URI.String) {
		t.Fatalf("link address %q", st.URI.String)
	}
	ttl := st.ExpiresAt.Time.Sub(st.Now)
	if !st.ExpiresAt.Valid || ttl < 110*time.Second || ttl > 121*time.Second {
		t.Fatalf("expiry in %v, want about the 120 s time to live", ttl)
	}

	st = mr.waitStatus("the finished link", 15*time.Second, func(s statusRow) bool { return s.State == "linked" })
	if st.Number != testNumber || st.URI.Valid || st.ExpiresAt.Valid || st.Handled != gen || st.Err != "" {
		t.Fatalf("after the link: %v", st)
	}
	if st.SidecarVersion != "test-version" || st.CLIVersion != "0.14.10" {
		t.Fatalf("versions %q %q", st.SidecarVersion, st.CLIVersion)
	}
	raw, _ := os.ReadFile(mr.r.calls)
	if !strings.Contains(string(raw), `"deviceName":"ARTEX"`) {
		t.Fatalf("finishLink did not carry the device name:\n%s", raw)
	}
	mr.logsHold("pub_key", testNumber)
}

func TestSidecarGivesAnExpiredLinkUp(t *testing.T) {
	mr := newMRig(t, "scexpire", map[string]any{"finish": map[string]any{"mode": "never"}})
	mr.start(func(c *MachineConfig) { fastIntervals(c); c.linkTTLCap = 1500 * time.Millisecond }, nil)
	mr.waitState("unlinked")
	gen := mr.requestLink()
	mr.waitState("linking")
	st := mr.waitState("unlinked")
	if st.Err != "link expired" || st.Handled != gen || st.URI.Valid {
		t.Fatalf("after the expiry: %v", st)
	}
	if n := mr.callCount("startLink"); n != 1 {
		t.Fatalf("startLink calls: %d", n)
	}
	// The child was restarted: the pending link of signal-cli is gone with it.
	if pids, _ := mr.r.startTimes(); len(pids) != 2 {
		t.Fatalf("child starts: %v", pids)
	}
	// A new link is accepted.
	mr.reconcile()
	mr.requestLink()
	mr.waitState("linking")
}

func TestSidecarCancelsALink(t *testing.T) {
	mr := newMRig(t, "sccancel", map[string]any{"finish": map[string]any{"mode": "never"}})
	mr.start(nil, nil)
	mr.waitState("unlinked")
	gen := mr.requestLink()
	mr.waitState("linking")
	mr.requestCancel()
	st := mr.waitState("unlinked")
	if st.Err != "link cancelled" || st.Handled != gen || st.URI.Valid {
		t.Fatalf("after the cancel: %v", st)
	}
	if pids, _ := mr.r.startTimes(); len(pids) != 2 {
		t.Fatalf("child starts: %v", pids)
	}
}

func TestSidecarKeepsAnAccountThatCompletesAtTheMomentOfACancel(t *testing.T) {
	mr := newMRig(t, "sccancel6b", map[string]any{"finish": map[string]any{"mode": "on_stop", "number": testNumber}})
	sc := mr.start(nil, nil)
	mr.waitState("unlinked")
	gen := mr.requestLink()
	mr.waitState("linking")
	mr.requestCancel()
	st := mr.waitStatus("the account", 15*time.Second, func(s statusRow) bool { return s.State == "linked" })
	if st.Number != testNumber || st.Handled != gen || st.URI.Valid || st.Err != "" {
		t.Fatalf("after the cancel: %v", st)
	}
	// Linked but not confirmed: nothing may be sent from it.
	mr.waitStatus("a view", 5*time.Second, func(statusRow) bool { _, why := sc.m.ready(); return why == "not_confirmed" })
}

func TestSidecarKeepsAnAccountThatCompletesAtTheMomentOfAnExpiry(t *testing.T) {
	mr := newMRig(t, "scexpire6b", map[string]any{"finish": map[string]any{"mode": "on_stop", "number": testNumber}})
	mr.start(func(c *MachineConfig) { fastIntervals(c); c.linkTTLCap = 1500 * time.Millisecond }, nil)
	mr.waitState("unlinked")
	gen := mr.requestLink()
	st := mr.waitStatus("the account", 15*time.Second, func(s statusRow) bool { return s.State == "linked" })
	if st.Number != testNumber || st.Handled != gen || st.URI.Valid || st.Err != "" {
		t.Fatalf("after the expiry: %v", st)
	}
}

func TestSidecarUnlinksALinkedAccount(t *testing.T) {
	mr := newMRig(t, "scunlink", completes(100))
	mr.start(nil, nil)
	mr.link()
	// Files of the account, also in a subdirectory.
	if err := os.MkdirAll(filepath.Join(mr.r.data, "data", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mr.r.data, "data", "x", "key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	gen := mr.requestUnlink()
	st := mr.waitStatus("the unlink", 15*time.Second, func(s statusRow) bool { return s.Handled == gen })
	if st.State != "unlinked" || st.Remote != "local_only" || st.Number != "" || st.Groups != "[]" {
		t.Fatalf("after the unlink: %v", st)
	}
	if names := mr.dataEntries(); len(names) != 0 {
		t.Fatalf("the data directory still holds %v", names)
	}
	for _, m := range mr.r.recordedMethods() {
		if !contains([]string{"version", "listAccounts", "startLink", "finishLink", "listGroups"}, m) {
			t.Errorf("unexpected call %q to signal-cli", m)
		}
	}
	// And a new link works afterwards.
	mr.reconcile()
	mr.requestLink()
	mr.waitState("linking")
}

func TestSidecarUnlinksFromTheErrorState(t *testing.T) {
	mr := newMRig(t, "scunlinkerr", nil)
	mr.r.seedAccounts(testNumber, "+15555550202")
	mr.start(nil, nil)
	st := mr.waitState("error")
	if !strings.Contains(st.Err, "more than one account") {
		t.Fatalf("error text %q", st.Err)
	}
	gen := mr.requestUnlink()
	st = mr.waitStatus("the unlink", 15*time.Second, func(s statusRow) bool { return s.Handled == gen })
	if st.State != "unlinked" || st.Remote != "local_only" {
		t.Fatalf("after the unlink: %v", st)
	}
	if names := mr.dataEntries(); len(names) != 0 {
		t.Fatalf("the data directory still holds %v", names)
	}
}

func TestSidecarStoresTheGroupsCleaned(t *testing.T) {
	g2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	g3 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
	g4 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	sc := completes(100)
	sc["groups"] = []map[string]any{
		{"id": testGroup, "name": "Ops\u202e team\u200b-SENTINEL-GROUP-NAME", "isMember": true, "isBlocked": false},
		{"id": g2, "name": strings.Repeat("é", 200), "isMember": true, "isBlocked": false},
		{"id": g3, "name": "blocked", "isMember": true, "isBlocked": true},
		{"id": g4, "name": "left", "isMember": false, "isBlocked": false},
		{"id": "c2hvcnQ=", "name": "short id", "isMember": true, "isBlocked": false},
	}
	mr := newMRig(t, "scgroups", sc)
	mr.start(nil, nil)
	mr.link()
	st := mr.waitStatus("the groups", 10*time.Second, func(s statusRow) bool { return s.GroupsAt.Valid })
	if st.GroupsGen != 0 {
		t.Fatalf("groups_gen %d before any request", st.GroupsGen)
	}
	var got []GroupEntry
	if err := json.Unmarshal([]byte(st.Groups), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("stored groups %+v, want the two member groups with a 32-byte id", got)
	}
	if got[0].ID != testOuterGroup || got[0].Name != "Ops team-SENTINEL-GROUP-NAME" {
		t.Fatalf("first group %+v", got[0])
	}
	if got[1].Name != strings.Repeat("é", 128) {
		t.Fatalf("second group name has %d characters", len([]rune(got[1].Name)))
	}

	first := st.GroupsAt.Time
	mr.requestGroups()
	st = mr.waitStatus("the refresh", 10*time.Second, func(s statusRow) bool { return s.GroupsGen == 1 })
	if !st.GroupsAt.Time.After(first) {
		t.Fatalf("groups_at did not advance: %v then %v", first, st.GroupsAt.Time)
	}
	mr.logsHold("SENTINEL-GROUP-NAME", testGroup, testNumber)
}

func TestSidecarReportsAnAccountThatCannotListGroups(t *testing.T) {
	sc := completes(100)
	sc["list_groups_error"] = true
	mr := newMRig(t, "scgroupserr", sc)
	mr.start(nil, nil)
	mr.link()
	// The failure is logged and retried; the account stays linked.
	deadline := time.Now().Add(2 * time.Second)
	for mr.callCount("listGroups") < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := mr.callCount("listGroups"); n < 2 {
		t.Fatalf("listGroups was tried %d times", n)
	}
	if st := mr.status(); st.State != "linked" || st.GroupsAt.Valid {
		t.Fatalf("status %v", st)
	}
}

// The restart rule: after a restart of the sidecar the state comes from
// listAccounts; a pending link is never resumed.
func TestRestartedSidecarAcknowledgesAPendingLinkWithoutResumingIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
		want   string
	}{{"pending", false, "link interrupted"}, {"cancelled meanwhile", true, "link cancelled"}} {
		t.Run(tc.name, func(t *testing.T) {
			mr := newMRig(t, "screstartlink"+strconv.FormatBool(tc.cancel), map[string]any{"finish": map[string]any{"mode": "never"}})
			first := mr.start(nil, nil)
			mr.waitState("unlinked")
			gen := mr.requestLink()
			mr.waitState("linking")
			first.stop()
			if tc.cancel {
				mr.requestCancel()
			}

			mr.start(nil, nil)
			st := mr.waitStatus("the acknowledgement", 15*time.Second, func(s statusRow) bool { return s.Handled == gen })
			if st.State != "unlinked" || st.Err != tc.want || st.URI.Valid {
				t.Fatalf("after the restart: %v", st)
			}
			if n := mr.callCount("startLink"); n != 1 {
				t.Fatalf("startLink was called %d times: the link was resumed", n)
			}
		})
	}
}

func TestRestartedSidecarKeepsALinkedAccountAndItsConfirmation(t *testing.T) {
	mr := newMRig(t, "screstartlinked", completes(100))
	first := mr.start(nil, nil)
	gen := mr.link()
	mr.confirm(testNumber)
	before := mr.status().HeartbeatAt.Time
	first.stop()

	second := mr.start(nil, nil)
	mr.waitStatus("a new heartbeat", 10*time.Second, func(s statusRow) bool { return s.HeartbeatAt.Time.After(before) })
	st := mr.status()
	if st.State != "linked" || st.Number != testNumber || st.Handled != gen {
		t.Fatalf("after the restart: %v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		account, why := second.m.ready()
		if why == "" && account == testNumber {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready after the restart: %q", why)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if n := mr.callCount("startLink"); n != 1 {
		t.Fatalf("startLink calls: %d", n)
	}
}

func TestRestartedSidecarReportsErrorForSeveralAccounts(t *testing.T) {
	mr := newMRig(t, "screstarterr", nil)
	mr.r.seedAccounts(testNumber, "+15555550202")
	mr.start(nil, nil)
	st := mr.waitState("error")
	if !strings.Contains(st.Err, "more than one account") {
		t.Fatalf("error text %q", st.Err)
	}
	mr.logsHold(testNumber, "+15555550202")
}

func TestRestartedSidecarRepeatsAPendingUnlink(t *testing.T) {
	mr := newMRig(t, "screstartunlink", completes(100))
	first := mr.start(nil, nil)
	mr.link()
	first.stop()
	gen := mr.requestUnlink() // asked while the sidecar was down
	if len(mr.dataEntries()) == 0 {
		t.Fatal("no account files to wipe")
	}

	mr.start(nil, nil)
	st := mr.waitStatus("the unlink", 15*time.Second, func(s statusRow) bool { return s.Handled == gen })
	if st.State != "unlinked" || st.Remote != "local_only" {
		t.Fatalf("after the restart: %v", st)
	}
	if names := mr.dataEntries(); len(names) != 0 {
		t.Fatalf("the data directory still holds %v", names)
	}
}

func killChild(t *testing.T, mr *mrig) {
	t.Helper()
	pids, _ := mr.r.startTimes()
	if len(pids) == 0 {
		t.Fatal("no child was started")
	}
	if err := syscall.Kill(pids[len(pids)-1], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

func TestChildRestartDuringALinkEndsItAsInterrupted(t *testing.T) {
	mr := newMRig(t, "sckilllink", map[string]any{"finish": map[string]any{"mode": "never"}})
	mr.start(nil, nil)
	mr.waitState("unlinked")
	gen := mr.requestLink()
	mr.waitState("linking")
	killChild(t, mr)
	st := mr.waitStatus("the end of the link", 15*time.Second, func(s statusRow) bool { return s.Handled == gen })
	if st.State != "unlinked" || st.Err != "link interrupted" || st.URI.Valid {
		t.Fatalf("after the kill: %v", st)
	}
	if n := mr.callCount("startLink"); n != 1 {
		t.Fatalf("startLink calls: %d", n)
	}
}

func TestKilledChildIsReportedAndSendsAreRefusedUntilItIsBack(t *testing.T) {
	mr := newMRig(t, "sckillchild", completes(100))
	sc := mr.start(nil, func(c *Config) { c.MinBackoff, c.MaxBackoff = time.Second, time.Second })
	mr.link()
	mr.confirm(testNumber)
	mr.waitStatus("ready", 10*time.Second, func(statusRow) bool { _, why := sc.m.ready(); return why == "" })

	killChild(t, mr)
	// A child that is gone is a transient condition: the state is unknown, not
	// error, and the status row is not given the error text.
	mr.waitStatus("the child to be down", 10*time.Second, func(statusRow) bool {
		state, _, _, text := sc.m.current()
		return state == StateUnknown && strings.Contains(text, "signal-cli is not running")
	})
	if _, why := sc.m.ready(); why != "child_down" {
		t.Fatalf("while the child is down: %q", why)
	}
	if st := mr.status(); st.State == "error" || st.Err != "" {
		t.Fatalf("the status row shows %v for a restarting child", st)
	}
	st := mr.waitState("linked")
	if st.Number != testNumber {
		t.Fatalf("after the restart: %v", st)
	}
	mr.waitStatus("ready again", 10*time.Second, func(statusRow) bool { _, why := sc.m.ready(); return why == "" })
	if n := mr.callCount("startLink"); n != 1 {
		t.Fatalf("startLink calls: %d", n)
	}
}

func TestReadyRefusesUntilTheAccountIsLinkedAndConfirmed(t *testing.T) {
	mr := newMRig(t, "scready", completes(100))
	sc := mr.start(nil, nil)
	mr.waitState("unlinked")
	waitWhy := func(want Refusal) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			account, why := sc.m.ready()
			if why == want {
				if (want == "") != (account == testNumber) {
					t.Fatalf("account %q with reason %q", account, why)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("reason %q, want %q", why, want)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	waitWhy("not_linked")
	mr.link()
	waitWhy("not_confirmed")
	mr.confirm("+15555550999")
	waitWhy("not_confirmed")
	mr.confirm(testNumber)
	waitWhy("")
	mr.requestUnlink()
	waitWhy("not_linked")
}

// tcpProxy is a switch between the store and the database.
type tcpProxy struct {
	t      *testing.T
	target string
	addr   string
	mu     sync.Mutex
	ln     net.Listener
	conns  []net.Conn
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	p := &tcpProxy{t: t, target: target}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.Cut)
	return p
}

func (p *tcpProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); up.Close() }()
			go func() { _, _ = io.Copy(c, up); c.Close() }()
		}
	}()
}

// Cut stops accepting and drops every connection.
func (p *tcpProxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// Restore listens again on the same address.
func (p *tcpProxy) Restore() {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		p.t.Fatal(err)
	}
	p.serve(ln)
}

func TestSidecarSurvivesTheLossOfTheDatabase(t *testing.T) {
	mr := newMRig(t, "scdbloss", completes(100))
	proxy := newTCPProxy(t, net.JoinHostPort(mr.dbRig.cc.Host, strconv.Itoa(int(mr.dbRig.cc.Port))))
	cc := mr.dbRig.cc.Copy()
	host, port, _ := net.SplitHostPort(proxy.addr)
	p, _ := strconv.Atoi(port)
	cc.Host, cc.Port = host, uint16(p)
	mr.cc = cc

	sc := mr.start(func(c *MachineConfig) { fastIntervals(c); c.StaleAfter = 1500 * time.Millisecond }, nil)
	mr.link()
	mr.confirm(testNumber)
	waitWhy := func(want Refusal, d time.Duration) {
		t.Helper()
		deadline := time.Now().Add(d)
		for {
			if _, why := sc.m.ready(); why == want {
				return
			}
			if time.Now().After(deadline) {
				_, why := sc.m.ready()
				t.Fatalf("reason %q after %v, want %q", why, d, want)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	waitWhy("", 10*time.Second)
	before := mr.status().HeartbeatAt.Time

	proxy.Cut()
	waitWhy("config_stale", 10*time.Second)
	time.Sleep(500 * time.Millisecond)
	if sc.exited() {
		t.Fatal("the state machine ended with the database")
	}
	if _, why := sc.m.ready(); why != "config_stale" {
		t.Fatalf("while the database is away: %q", why)
	}

	proxy.Restore()
	waitWhy("", 15*time.Second)
	mr.waitStatus("a heartbeat after the outage", 15*time.Second, func(s statusRow) bool { return s.HeartbeatAt.Time.After(before) })
	if !strings.Contains(mr.r.logs.all(), "the database is reachable again") {
		t.Fatalf("no log line about the return:\n%s", mr.r.logs.all())
	}
}

func TestAwaitAuthHoldsBackUntilThePasswordCheckIsConfirmed(t *testing.T) {
	d := newDBRig(t, "scauth")
	d.exec(`UPDATE public.signal_sidecar_config SET db_auth_state = 'unchecked'`)
	store := d.newStore()
	// A supervisor that is never run: signal-cli must not be touched.
	sup := NewSupervisor(Config{Command: "/nonexistent/signal-cli"})
	m := NewMachine(MachineConfig{SidecarVersion: "test-version", PollEvery: 50 * time.Millisecond}, store, sup)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.AwaitAuth(ctx) }()

	// A transient condition: no error is reported, the state stays unknown.
	time.Sleep(500 * time.Millisecond)
	if st := d.status(); st.State != "unknown" || st.Err != "" {
		t.Fatalf("status %v, want unknown without an error", st)
	}
	if state, _, _, text := m.current(); state != StateUnknown || !strings.Contains(text, "has not confirmed password authentication") {
		t.Fatalf("local state %q %q", state, text)
	}
	select {
	case err := <-done:
		t.Fatalf("AwaitAuth returned early: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if sup.Status().Starts != 0 {
		t.Fatal("signal-cli was started")
	}
	d.exec(`UPDATE public.signal_sidecar_config SET db_auth_state = 'ok'`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AwaitAuth did not return after the database confirmed the authentication")
	}
}

func TestGroupEntriesKeepMemberGroupsWithA32ByteIdOnly(t *testing.T) {
	idOf := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	var in []Group
	for i := 0; i < 250; i++ {
		in = append(in, Group{ID: idOf(byte(i)), Name: "g", IsMember: true})
	}
	got, dropped := groupEntries(append([]Group{
		{ID: "not base64!", Name: "a", IsMember: true},
		{ID: idOf(1), Name: "blocked", IsMember: true, IsBlocked: true},
		{ID: idOf(2), Name: "left"},
	}, in...))
	if len(got) != maxGroups {
		t.Fatalf("%d groups, want the cap of %d", len(got), maxGroups)
	}
	// One invalid id and the 50 groups over the cap; blocked and left groups
	// are not counted.
	if dropped != 51 {
		t.Fatalf("%d groups reported as dropped, want 51", dropped)
	}
	for _, g := range got {
		if !strings.HasPrefix(g.ID, "group.") || g.Name != "g" {
			t.Fatalf("entry %+v", g)
		}
	}
	// A list that is too large for the database is cut from the end.
	big := make([]Group, 0, 200)
	for i := 0; i < 200; i++ {
		big = append(big, Group{ID: idOf(byte(i)), Name: strings.Repeat("😀", 128), IsMember: true})
	}
	out, cut := groupEntries(big)
	raw, _ := json.Marshal(out)
	if len(out) == 0 || len(raw) > maxGroupsJSON {
		t.Fatalf("%d groups, %d bytes", len(out), len(raw))
	}
	if cut != len(big)-len(out) {
		t.Fatalf("%d reported as dropped, want %d", cut, len(big)-len(out))
	}
}

func TestIsLinkExpiry(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"deadline":        {context.DeadlineExceeded, true},
		"timed out":       {&RPCError{Code: -1, Message: "Link request timed out, please try again."}, true},
		"unknown uri":     {&RPCError{Code: -1, Message: "Unknown device link uri."}, true},
		"other user err":  {&RPCError{Code: -1, Message: "Something else"}, false},
		"io error":        {&RPCError{Code: -3, Message: "Link request timed out"}, false},
		"closed":          {ErrClosed, false},
		"cancelled":       {context.Canceled, false},
		"wrapped timeout": {errors.Join(errors.New("x"), context.DeadlineExceeded), true},
	}
	for name, c := range cases {
		if got := isLinkExpiry(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestWipeDirEmptiesTheDirectoryAndNothingElse(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dataDir, "a", "b"), 0o700))
	must(os.WriteFile(filepath.Join(dataDir, "a", "b", "f"), []byte("x"), 0o600))
	must(os.WriteFile(filepath.Join(dataDir, "top"), []byte("x"), 0o600))
	must(os.Symlink(outside, filepath.Join(dataDir, "link")))
	// The configured path is itself a symbolic link to the directory.
	via := filepath.Join(t.TempDir(), "data")
	must(os.Symlink(dataDir, via))

	must(wipeDir(via))
	if entries, _ := os.ReadDir(dataDir); len(entries) != 0 {
		t.Fatalf("still holds %d entries", len(entries))
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatalf("a file behind a symbolic link was deleted: %v", err)
	}
}

func TestWipeDirRefusesWhatIsNotAPlainDataDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"empty": "", "root": "/", "root with dots": "/../..", "a file": file,
		"missing": filepath.Join(t.TempDir(), "missing"),
	} {
		if err := wipeDir(path); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the file is gone: %v", err)
	}
}

// sentCalls returns the params of every send the fake CLI received.
func (mr *mrig) sentCalls() []map[string]any {
	mr.t.Helper()
	raw, _ := os.ReadFile(mr.r.calls)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var c struct {
			Method string
			Params map[string]any
		}
		if json.Unmarshal([]byte(line), &c) == nil && c.Method == "send" {
			out = append(out, c.Params)
		}
	}
	return out
}

func TestSendThroughTheSocketApiNeedsAConfirmedAccount(t *testing.T) {
	mr := newMRig(t, "scsend", completes(100))
	sc := mr.start(nil, nil)
	mr.link()
	srv := httptest.NewServer(NewAPI(sc.m, mr.r.logs.logf))
	defer srv.Close()
	post := func(recipients ...string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"number": testNumber, "recipients": recipients,
			"message": "hello-SENTINEL-TEXT", "text_mode": "normal"})
		resp, err := http.Post(srv.URL+"/v2/send", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	waitReady := func(want Refusal) {
		t.Helper()
		mr.waitStatus("the readiness", 10*time.Second, func(statusRow) bool { _, why := sc.m.ready(); return why == want })
	}

	waitReady(RefuseNotConfirmed)
	if status, body := post(testOuterGroup); status != 400 {
		t.Fatalf("unconfirmed: %d %s", status, body)
	}
	if n := len(mr.sentCalls()); n != 0 {
		t.Fatalf("%d sends reached signal-cli", n)
	}

	mr.confirm(testNumber)
	waitReady(RefuseNone)
	if status, body := post(testOuterGroup); status != 201 {
		t.Fatalf("group: %d %s", status, body)
	}
	if status, body := post("+15555550202", "+15555550303"); status != 201 {
		t.Fatalf("numbers: %d %s", status, body)
	}
	sent := mr.sentCalls()
	if len(sent) != 2 {
		t.Fatalf("sends: %v", sent)
	}
	if sent[0]["account"] != testNumber || sent[0]["message"] != "hello-SENTINEL-TEXT" ||
		fmt.Sprint(sent[0]["groupIds"]) != "["+testGroup+"]" || sent[0]["recipient"] != nil {
		t.Fatalf("group send: %v", sent[0])
	}
	if fmt.Sprint(sent[1]["recipient"]) != "[+15555550202 +15555550303]" || sent[1]["groupIds"] != nil {
		t.Fatalf("numbers send: %v", sent[1])
	}

	// A group id that does not decode twice to 32 bytes never reaches signal-cli.
	if status, _ := post("group." + base64.StdEncoding.EncodeToString([]byte("short"))); status != 400 {
		t.Fatalf("bad group id: %d", status)
	}
	if n := len(mr.sentCalls()); n != 2 {
		t.Fatalf("%d sends reached signal-cli", n)
	}

	resp, err := http.Get(srv.URL + "/v1/about")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var about About
	if err := json.NewDecoder(resp.Body).Decode(&about); err != nil {
		t.Fatal(err)
	}
	if about != (About{Service: "artex-signal", Version: "test-version", SignalCLI: "0.14.10", State: "linked"}) {
		t.Fatalf("about: %+v", about)
	}
	mr.logsHold("SENTINEL-TEXT", "+15555550202")
}

func TestInnerGroupID(t *testing.T) {
	if got, err := innerGroupID(testOuterGroup); err != nil || got != testGroup {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "group.", "group.!!!", "group." + base64.StdEncoding.EncodeToString([]byte("short")),
		"group." + base64.StdEncoding.EncodeToString([]byte("not base64 inside!!"))} {
		if _, err := innerGroupID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// An unlink does not need signal-cli to answer: it must run, and be
// acknowledged, when listing the accounts hangs.
func TestSidecarUnlinksWhileTheChildDoesNotAnswerListAccounts(t *testing.T) {
	sc := map[string]any{"hang_on": "listAccounts"}
	mr := newMRig(t, "scunlinkhang", sc)
	mr.r.seedAccounts(testNumber)
	gen := mr.requestUnlink()
	mr.start(nil, func(c *Config) {
		c.CallTimeout = 300 * time.Millisecond
		c.TimeoutLimit = 1000
	})
	st := mr.waitStatus("the unlink", 20*time.Second, func(s statusRow) bool { return s.Handled == gen })
	if names := mr.dataEntries(); len(names) != 0 {
		t.Fatalf("the data directory still holds %v (status %v)", names, st)
	}
	if st.State != "error" || !strings.Contains(st.Err, "could not be read") {
		t.Fatalf("a command whose end state is unknown must be reported as failed: %v", st)
	}
}

// A child that runs but no longer answers is reported with its last error and
// refused for sends, until it answers again.
func TestSidecarReportsAChildThatStopsAnswering(t *testing.T) {
	sc := completes(100)
	sc["hang_version_after_ms"] = 1500
	mr := newMRig(t, "scprobe", sc)
	s := mr.start(func(c *MachineConfig) { c.ProbeEvery = 100 * time.Millisecond }, func(c *Config) {
		c.CallTimeout = 300 * time.Millisecond
		c.TimeoutLimit = 3
	})
	mr.link()
	mr.confirm(testNumber)
	mr.waitStatus("ready", 10*time.Second, func(statusRow) bool { _, why := s.m.ready(); return why == "" })
	st := mr.waitStatus("the probe failure", 20*time.Second, func(st statusRow) bool {
		return strings.Contains(st.Err, "signal-cli does not answer")
	})
	if st.State != "linked" {
		t.Fatalf("the account is still linked: %v", st)
	}
	if _, why := s.m.ready(); why != "child_down" && why != "" {
		t.Fatalf("while the child is silent: %q", why)
	}
}

func TestAboutReportsHowLongTheChildHasBeenDown(t *testing.T) {
	m := NewMachine(MachineConfig{}, nil, NewSupervisor(Config{}))
	if got := m.About(context.Background()).ChildDownSeconds; got != 0 {
		t.Fatalf("child_down_seconds %d for a child that was never down", got)
	}
	m.mu.Lock()
	m.childDown, m.downSince = true, time.Now().Add(-130*time.Second)
	m.mu.Unlock()
	if got := m.About(context.Background()).ChildDownSeconds; got < 130 {
		t.Fatalf("child_down_seconds %d, want at least 130", got)
	}
}

// A long operation must not leave the status row without heartbeats.
func TestKeepAliveHeartbeatsWhileALongOperationRuns(t *testing.T) {
	r := newDBRig(t, "sckeepalive")
	m := NewMachine(MachineConfig{HeartbeatEvery: 50 * time.Millisecond}, r.newStore(), NewSupervisor(Config{}))
	m.setState(StateUnlinked, "", "", "")
	stop := m.keepAlive(context.Background())
	defer stop()
	seen := map[time.Time]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("%d heartbeats in 5 s while the operation ran", len(seen))
		}
		if st := r.status(); st.HeartbeatAt.Valid {
			seen[st.HeartbeatAt.Time] = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
}

func TestSidecarCountsDroppedGroupsAndReportsAFailedRefresh(t *testing.T) {
	sc := completes(100)
	sc["groups"] = []map[string]any{
		{"id": testGroup, "name": "ok", "isMember": true},
		{"id": "c2hvcnQ=", "name": "short id", "isMember": true},
	}
	mr := newMRig(t, "scgroupsdrop", sc)
	mr.start(nil, nil)
	mr.link()
	mr.waitStatus("the groups", 10*time.Second, func(s statusRow) bool { return s.GroupsAt.Valid })
	if logs := mr.r.logs.all(); !strings.Contains(logs, "1 groups were not stored") {
		t.Fatalf("no count of dropped groups in the log:\n%s", logs)
	}

	fail := completes(100)
	fail["list_groups_error"] = true
	mr2 := newMRig(t, "scgroupsfail", fail)
	mr2.start(nil, nil)
	mr2.link()
	st := mr2.waitStatus("the failed refresh", 10*time.Second, func(s statusRow) bool {
		return strings.Contains(s.Err, "group list could not be read")
	})
	if st.State != "linked" {
		t.Fatalf("status %v", st)
	}
}

// A refused link address is logged with its generation, and a refused
// heartbeat (generation ahead of the database) rewinds the handled generation.
func TestRefusedLinkAddressAndRefusedHeartbeatAreHandled(t *testing.T) {
	mr := newMRig(t, "screfused", completes(100))
	sup, stop := mr.r.run(mr.r.config())
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.WaitUp(ctx); err != nil {
		t.Fatal(err)
	}
	store := mr.newStore()
	m := NewMachine(MachineConfig{SidecarVersion: "v", Logf: mr.r.logs.logf}, store, sup)
	real := mr.requestLink()
	ahead := real + 5
	m.startLink(ctx, ConfigView{Desired: "link", Gen: ahead, DeviceName: "ARTEX", LinkTTLSeconds: 5})
	want := fmt.Sprintf("refused the link address of generation %d", ahead)
	if logs := mr.r.logs.all(); !strings.Contains(logs, want) {
		t.Fatalf("log lacks %q:\n%s", want, logs)
	}
	if strings.Contains(mr.r.logs.all(), "database is not reachable") {
		t.Fatal("a refused link address was reported as an unreachable database")
	}
	if m.handled != 0 {
		t.Fatalf("handled %d after the database refused the generation, want it rewound to 0", m.handled)
	}
}

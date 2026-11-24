package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The Signal service may create its socket after this server started. The watcher re-runs C12 on its
// own, so the status becomes enforced without any Signal send.
func TestWatchC12ClearsNotVerifiedOnceTheSocketAppears(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	var runs atomic.Int32
	iso := c12Iso(t, sock, func(ctx context.Context, d, s string) (string, error) {
		runs.Add(1)
		return "c12.sock=refused\nc12.dir=denied\n", nil
	})
	iso.c12wait = 5 * time.Millisecond
	iso.RecheckSignal(t.Context()) // what the start-up self-check publishes: the socket is absent
	if v := iso.View(); v.Status != Degraded {
		t.Fatalf("at start: %+v", v)
	}

	done := make(chan struct{})
	go func() { iso.watchC12(t.Context()); close(done) }()
	time.Sleep(30 * time.Millisecond)
	if v := iso.View(); v.Status != Degraded {
		t.Fatalf("socket still absent: %+v", v)
	}
	listenAt(t, sock)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher kept running after the socket appeared")
	}
	if v := iso.View(); v.Status != Enforced || v.Reason != "" {
		t.Fatalf("after the socket appeared: %+v", v)
	}
	if runs.Load() != 1 {
		t.Fatalf("the tool shell ran %d times, want 1", runs.Load())
	}
}

// A real result ends the watch: a reachable socket is reported once, not retried.
func TestWatchC12StopsOnAnyOtherResult(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	listenAt(t, sock)
	var runs atomic.Int32
	iso := c12Iso(t, sock, func(ctx context.Context, d, s string) (string, error) {
		runs.Add(1)
		return "c12.sock=connected\nc12.dir=denied\n", nil
	})
	iso.c12wait = 5 * time.Millisecond
	iso.setCheck(CheckResult{ID: "C12", Detail: c12Precheck(filepath.Join(shortSockDir(t), "none.sock")), Degrades: true})
	done := make(chan struct{})
	go func() { iso.watchC12(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher kept running")
	}
	if v := iso.View(); v.Status != Degraded || runs.Load() != 1 {
		t.Fatalf("%+v after %d runs", v, runs.Load())
	}
}

// The watcher ends with its context.
func TestWatchC12EndsWithItsContext(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	iso := c12Iso(t, sock, func(ctx context.Context, d, s string) (string, error) { return "", nil })
	iso.c12wait = 5 * time.Millisecond
	iso.RecheckSignal(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { iso.watchC12(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher outlived its context")
	}
}

// The watcher follows the detail text that c12Precheck gives for an absent socket.
func TestWatchC12FollowsThePrecheckDetail(t *testing.T) {
	absent := c12Precheck(filepath.Join(shortSockDir(t), "none.sock"))
	if absent != "not verified: socket absent" || !strings.HasPrefix(absent, c12NotVerified) {
		t.Fatalf("precheck detail %q, watcher prefix %q", absent, c12NotVerified)
	}
}

// Another "not verified" result (the path could not be read for a moment) is retried too.
func TestWatchC12RetriesAnyNotVerifiedResult(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "api.sock")
	listenAt(t, sock)
	var runs atomic.Int32
	iso := c12Iso(t, sock, func(ctx context.Context, d, s string) (string, error) {
		runs.Add(1)
		return "c12.sock=refused\nc12.dir=denied\n", nil
	})
	iso.c12wait = 5 * time.Millisecond
	iso.setCheck(CheckResult{ID: "C12", Detail: "not verified: lstat failed", Degrades: true})
	done := make(chan struct{})
	go func() { iso.watchC12(t.Context()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher kept running")
	}
	if v := iso.View(); v.Status != Enforced || runs.Load() != 1 {
		t.Fatalf("%+v after %d runs", v, runs.Load())
	}
}

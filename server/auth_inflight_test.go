package server

import (
	"fmt"
	"testing"
	"time"
)

func admitted(th *loginThrottle, key string, n int) int {
	got := 0
	for i := 0; i < n; i++ {
		if _, ok := th.acquire(key); ok {
			got++
		}
	}
	return got
}

func TestAcquireAdmitsAsManyChecksAsThereAreFreeFailuresLeft(t *testing.T) {
	clk := useThrottle(t)
	th := authThrottle
	if n := admitted(th, "a", 20); n != throttleFreeFailures {
		t.Fatalf("fresh key admitted %d of 20 unsettled checks, want %d", n, throttleFreeFailures)
	}
	if left, ok := th.acquire("a"); ok || left <= 0 {
		t.Fatalf("saturated key: ok=%v left=%s want a refusal with a wait", ok, left)
	}
	// Two settle as failures: 2 failures recorded, 3 still in flight, 3 free left: none more.
	th.fail("a")
	th.fail("a")
	if n := admitted(th, "a", 5); n != 0 {
		t.Fatalf("admitted %d more with 3 in flight and 2 failures", n)
	}
	th.release("a") // 2 in flight, 2 failures: 3 free, so one more fits
	if n := admitted(th, "a", 5); n != 1 {
		t.Fatalf("admitted %d, want 1", n)
	}
	// Lock the key, let the lockout lapse: exactly one attempt is allowed, as in the sequential rule.
	b := "b"
	for i := 0; i < throttleFreeFailures; i++ {
		th.acquire(b)
		th.fail(b)
	}
	if _, ok := th.acquire(b); ok {
		t.Fatal("a locked key admitted a check")
	}
	clk.advance(1100 * time.Millisecond)
	if n := admitted(th, b, 20); n != 1 {
		t.Fatalf("after the lockout %d of 20 parallel checks were admitted, want 1", n)
	}
}

func TestReleaseReturnsTheReservationWithoutCountingAFailure(t *testing.T) {
	useThrottle(t)
	th := authThrottle
	for i := 0; i < 50; i++ {
		if _, ok := th.acquire("a"); !ok {
			t.Fatalf("acquire %d refused although each one was released", i)
		}
		th.release("a")
	}
	th.mu.Lock()
	n := len(th.m)
	th.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries left", n)
	}
	th.release("never-seen") // must not create or panic
}

func TestResetKeepsTheEntryWhileOtherChecksAreInFlight(t *testing.T) {
	useThrottle(t)
	th := authThrottle
	th.acquire("a")
	th.acquire("a")
	th.fail("a") // 1 failure, 1 in flight
	th.reset("a")
	th.mu.Lock()
	_, ok := th.m["a"]
	th.mu.Unlock()
	if ok {
		t.Fatal("a reset with nothing else in flight left the entry")
	}
	th.acquire("b")
	th.acquire("b")
	th.reset("b") // one settles as a success, one still in flight
	th.mu.Lock()
	el, ok := th.m["b"]
	in := 0
	if ok {
		in = el.Value.(*throttleEntry).inflight
	}
	th.mu.Unlock()
	if !ok || in != 1 {
		t.Fatalf("entry with a check in flight was dropped by a reset (ok=%v inflight=%d)", ok, in)
	}
	th.fail("b")
	th.release("b")
}

func TestAnEntryWithACheckInFlightIsNotEvicted(t *testing.T) {
	useThrottle(t)
	th := authThrottle
	if _, ok := th.acquire("held"); !ok {
		t.Fatal("acquire refused")
	}
	for i := 0; i < throttleMaxSources+500; i++ {
		th.fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	th.mu.Lock()
	_, ok := th.m["held"]
	n := len(th.m)
	th.mu.Unlock()
	if !ok {
		t.Fatal("an entry with a check in flight was evicted")
	}
	if n > throttleMaxSources+1 {
		t.Fatalf("entries=%d, bound %d", n, throttleMaxSources)
	}
}

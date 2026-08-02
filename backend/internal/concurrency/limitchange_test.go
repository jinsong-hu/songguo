package concurrency

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Raising max_concurrency must reach the requests that are ALREADY queued, not
// only the ones that arrive afterwards.
//
// This is the regression guard for a priority inversion: `limit` is a parameter
// of Acquire, so a waiter's retry loop used to re-check the value its own call
// captured — which cannot change. Nothing else woke it either, because releases
// only fire when an in-flight request finishes. With a long-lived WebSocket
// holding the only slot, raising the limit left the whole queue stuck for the
// life of that session while every later request sailed past. Measured before
// the fix: 5/5 fresh admitted, 0/5 already-queued.
func TestRaisingLimitAdmitsAlreadyQueuedWaiters(t *testing.T) {
	g := New()

	// Saturated at the old limit of 1, by a holder that never leaves — so the
	// only thing that can admit anybody is the raise itself.
	held, err := g.Acquire(context.Background(), "cred", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held()

	const queued = 5
	var admitted atomic.Int64
	for i := 0; i < queued; i++ {
		go func() {
			if _, err := g.Acquire(context.Background(), "cred", 1); err == nil {
				admitted.Add(1)
			}
		}()
	}
	waitUntil(t, func() bool { return g.Stats()["cred"].Waiting == queued }, "the queue to form at limit 1")

	// The operator raises max_concurrency to 10. One fresh request carries the
	// new value in, which is how a config change reaches the gate at all.
	if _, err := g.Acquire(context.Background(), "cred", 10); err != nil {
		t.Fatalf("acquire at raised limit: %v", err)
	}

	// 10 slots, 1 held by `held` and 1 by the fresh request: the 5 waiters fit.
	waitUntil(t, func() bool { return admitted.Load() == queued },
		"the already-queued requests to be admitted after the raise")
}

// The raise must not admit more than the new headroom allows.
func TestRaisingLimitDoesNotOvershoot(t *testing.T) {
	g := New()

	held, err := g.Acquire(context.Background(), "cred", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held()

	var admitted atomic.Int64
	for i := 0; i < 8; i++ {
		go func() {
			if _, err := g.Acquire(context.Background(), "cred", 1); err == nil {
				admitted.Add(1)
			}
		}()
	}
	waitUntil(t, func() bool { return g.Stats()["cred"].Waiting == 8 }, "8 waiters to queue")

	// 1 -> 3 creates room for exactly two more.
	rel, err := g.Acquire(context.Background(), "cred", 3)
	if err != nil {
		t.Fatalf("acquire at raised limit: %v", err)
	}
	defer rel()

	waitUntil(t, func() bool { return g.Stats()["cred"].InFlight == 3 }, "in-flight to reach the new limit")
	time.Sleep(150 * time.Millisecond) // let any overshoot show up

	if n := g.Stats()["cred"].InFlight; n != 3 {
		t.Errorf("in_flight = %d, want exactly 3 (the raise admitted past the new limit)", n)
	}
	if n := admitted.Load(); n != 1 {
		t.Errorf("admitted %d queued waiters, want 1 (limit 3 = holder + fresh + one waiter)", n)
	}
}

// Lowering the limit must not admit anyone, and must not disturb the queue.
func TestLoweringLimitAdmitsNobody(t *testing.T) {
	g := New()

	a, err := g.Acquire(context.Background(), "cred", 2)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer a()
	b, err := g.Acquire(context.Background(), "cred", 2)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer b()

	var admitted atomic.Int64
	go func() {
		if _, err := g.Acquire(context.Background(), "cred", 1); err == nil {
			admitted.Add(1)
		}
	}()
	waitUntil(t, func() bool { return g.Stats()["cred"].Waiting == 1 }, "a waiter to queue")

	time.Sleep(150 * time.Millisecond)
	if n := admitted.Load(); n != 0 {
		t.Errorf("admitted %d after the limit was LOWERED, want 0", n)
	}
	if n := g.Stats()["cred"].InFlight; n != 2 {
		t.Errorf("in_flight = %d, want 2 (existing requests drain, they are not evicted)", n)
	}
}

// A credential that goes fully idle must not leave its remembered limit behind,
// or the map grows for every provider ever configured.
func TestIdleCredentialForgetsItsLimit(t *testing.T) {
	g := New()
	rel, err := g.Acquire(context.Background(), "cred", 4)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()

	g.mu.Lock()
	_, remembered := g.limits["cred"]
	g.mu.Unlock()
	if remembered {
		t.Error("limit still remembered for a credential with nothing in flight and nobody queued")
	}
}

// waitUntil is waitFor with a description, so a timeout says what it was waiting
// for rather than just that it happened.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

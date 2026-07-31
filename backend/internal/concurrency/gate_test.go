package concurrency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnlimitedWhenLimitUnset(t *testing.T) {
	g := New()
	for i := 0; i < 100; i++ {
		release, err := g.Acquire(context.Background(), "cred", 0)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer release()
	}
	if n := g.Stats()["cred"].InFlight; n != 0 {
		t.Fatalf("in_flight = %d, want 0: an unlimited credential is not tracked", n)
	}
}

func TestAdmitsUpToLimit(t *testing.T) {
	g := New()
	var releases []func()
	for i := 0; i < 3; i++ {
		release, err := g.Acquire(context.Background(), "cred", 3)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if n := g.Stats()["cred"].InFlight; n != 3 {
		t.Fatalf("in_flight = %d, want 3", n)
	}
	for _, r := range releases {
		r()
	}
	if n := g.Stats()["cred"].InFlight; n != 0 {
		t.Fatalf("in_flight = %d, want 0 after release", n)
	}
}

// The point of the whole package: a request to a full provider WAITS rather
// than being sent elsewhere, so the session keeps its warm prompt cache.
func TestBlocksWhenFullThenProceedsOnRelease(t *testing.T) {
	g := New()
	first, err := g.Acquire(context.Background(), "cred", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	admitted := make(chan struct{})
	go func() {
		release, err := g.Acquire(context.Background(), "cred", 1)
		if err != nil {
			t.Errorf("queued acquire: %v", err)
			return
		}
		defer release()
		close(admitted)
	}()

	// The second caller must still be queued.
	select {
	case <-admitted:
		t.Fatal("second caller was admitted while the credential was full")
	case <-time.After(50 * time.Millisecond):
	}
	if st := g.Stats()["cred"]; st.InFlight != 1 || st.Waiting != 1 {
		t.Fatalf("stats = %+v, want in_flight 1 waiting 1", st)
	}

	first()

	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("second caller was not admitted after the slot freed")
	}
}

// The wait is bounded only by the caller's own context — songguo invents no
// timeout of its own.
func TestCancelWhileQueuedFreesTheSlot(t *testing.T) {
	g := New()
	held, err := g.Acquire(context.Background(), "cred", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer held()

	ctx, cancel := context.WithCancel(context.Background())
	failed := make(chan error, 1)
	go func() {
		_, err := g.Acquire(ctx, "cred", 1)
		failed <- err
	}()

	// Let it enqueue, then give up.
	waitFor(t, func() bool { return g.Stats()["cred"].Waiting == 1 })
	cancel()

	select {
	case err := <-failed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	waitFor(t, func() bool { return g.Stats()["cred"].Waiting == 0 })
}

// A cancellation racing a release must not strand the queue: whichever way the
// race falls, the free slot has to reach the waiter behind it.
//
// Which way it falls is genuinely up to the scheduler — the giving-up caller may
// be admitted before it notices, may leave while still queued, or may leave
// holding a wake-up a release already spent on it. This covers the race in the
// large; TestAbandonPassesOnASpentWakeUp pins the last of those three, which is
// far too narrow to reach by timing.
func TestCancelDoesNotStrandTheQueue(t *testing.T) {
	for i := 0; i < 50; i++ {
		g := New()
		held, err := g.Acquire(context.Background(), "cred", 1)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		gaveUp := make(chan struct{})
		go func() {
			// The queue is FIFO, so this is both the waiter that gives up and the
			// one the release wakes first. When the cancel loses the race it is
			// admitted for real and has to hand the slot back — dropping it would
			// strand the slot inside the test and blame the gate for a leak the
			// test itself created.
			release, err := g.Acquire(ctx, "cred", 1)
			if err == nil {
				release()
			}
			close(gaveUp)
		}()
		waitFor(t, func() bool { return g.Stats()["cred"].Waiting == 1 })

		admitted := make(chan struct{})
		go func() {
			release, err := g.Acquire(context.Background(), "cred", 1)
			if err != nil {
				t.Errorf("second waiter: %v", err)
				return
			}
			defer release()
			close(admitted)
		}()
		waitFor(t, func() bool { return g.Stats()["cred"].Waiting == 2 })

		// Cancel the first waiter and free the slot at nearly the same moment.
		go cancel()
		held()

		select {
		case <-admitted:
		case <-time.After(2 * time.Second):
			t.Fatalf("queue stalled on attempt %d: a free slot went unclaimed", i)
		}
		<-gaveUp
	}
}

// The narrowest case of that race, pinned deterministically: a release has
// already dequeued a waiter and closed its channel when that waiter gives up, so
// the wake-up is spent on a caller that will never use it. abandon has to pass it
// on, or the next waiter sleeps forever beside a slot nobody holds.
//
// The window is a few instructions wide — between the giving-up caller
// committing to its ctx.Done() case and it taking the mutex — so reaching it by
// timing is luck. Driving the queue into that exact state is not, which is why
// this test reaches into the gate's internals rather than racing goroutines.
func TestAbandonPassesOnASpentWakeUp(t *testing.T) {
	g := New()

	held, err := g.Acquire(context.Background(), "cred", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Stand in for a first waiter that is about to give up. Enqueuing its channel
	// directly, with no goroutine behind it, is what makes the interleaving exact.
	spent := make(chan struct{})
	g.mu.Lock()
	g.waiters["cred"] = append(g.waiters["cred"], spent)
	g.mu.Unlock()

	admitted := make(chan struct{})
	go func() {
		release, err := g.Acquire(context.Background(), "cred", 1)
		if err != nil {
			t.Errorf("second waiter: %v", err)
			return
		}
		defer release()
		close(admitted)
	}()
	waitFor(t, func() bool { return g.Stats()["cred"].Waiting == 2 })

	// Free the slot. Being first in the FIFO, the stand-in absorbs the wake-up —
	// so the real waiter is still parked with the credential now idle.
	held()
	select {
	case <-spent:
	default:
		t.Fatal("release did not signal the longest-waiting caller")
	}

	// Now that caller gives up, holding a wake-up it never used.
	g.abandon("cred", spent)

	select {
	case <-admitted:
	case <-time.After(2 * time.Second):
		t.Fatal("a spent wake-up was swallowed: the queue stalled with a free slot")
	}
}

// Limits are per credential, which is what a provider actually meters — the
// (origin, adapter) split gives one credential several routing vendors.
func TestLimitsAreIndependentPerCredential(t *testing.T) {
	g := New()
	a, err := g.Acquire(context.Background(), "cred-a", 1)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer a()

	done := make(chan struct{})
	go func() {
		release, err := g.Acquire(context.Background(), "cred-b", 1)
		if err != nil {
			t.Errorf("acquire b: %v", err)
			return
		}
		defer release()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a full credential blocked an unrelated one")
	}
}

// The limit is read at acquire time, so a config reload that raises it takes
// effect on the next admission without rebuilding anything.
func TestLimitChangeTakesEffectImmediately(t *testing.T) {
	g := New()
	first, err := g.Acquire(context.Background(), "cred", 1)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer first()

	// Same credential, higher limit: admitted at once despite one in flight.
	second, err := g.Acquire(context.Background(), "cred", 2)
	if err != nil {
		t.Fatalf("acquire with raised limit: %v", err)
	}
	defer second()

	if n := g.Stats()["cred"].InFlight; n != 2 {
		t.Fatalf("in_flight = %d, want 2", n)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	g := New()
	release, err := g.Acquire(context.Background(), "cred", 2)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release()
	release()
	if n := g.Stats()["cred"].InFlight; n != 0 {
		t.Fatalf("in_flight = %d, want 0 (no double-free)", n)
	}
}

// The invariant that matters under load: never more than `limit` at once.
func TestNeverExceedsLimitUnderLoad(t *testing.T) {
	g := New()
	const limit = 4
	var live, peak atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.Acquire(context.Background(), "cred", limit)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer release()

			n := live.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			live.Add(-1)
		}()
	}
	wg.Wait()

	if p := peak.Load(); p > limit {
		t.Fatalf("peak concurrency = %d, want <= %d", p, limit)
	}
	if n := g.Stats()["cred"].InFlight; n != 0 {
		t.Fatalf("in_flight = %d, want 0 once drained", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached within 2s")
}

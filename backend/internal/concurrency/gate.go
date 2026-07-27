// Package concurrency bounds how many requests songguo has in flight to one
// provider at a time.
//
// This is deliberately NOT a routing input. The router decides WHERE a request
// goes — health, session stickiness, priority, weight — and this gate decides
// WHEN it goes. Keeping them separate is what makes the limit safe to enforce:
// if capacity steered routing instead, a busy provider would push sessions onto
// a different vendor and throw away the prompt cache that stickiness exists to
// protect. On a large agent context that cache is worth roughly ten times the
// cost of the wait.
//
// So a request to a full provider WAITS for a slot rather than going elsewhere.
// The queue is FIFO per credential and the wait is bounded only by the caller's
// own context: songguo invents no timeout here, exactly as it invents no
// retries and no refusals. A client that is not willing to wait cancels, and
// cancelling frees the queue slot immediately.
//
// The limit is keyed by CREDENTIAL, not vendor. One provider is split into
// several routing vendors by (origin, adapter), but those vendors share one API
// key against one account quota — the thing the provider is actually metering.
//
// Known cost: a queued request holds its buffered body in RAM while it waits,
// so a long queue multiplies the memory tradeoff the proxy already accepts for
// buffering bodies at all. Set limits to match what the provider genuinely
// allows and the queue stays short.
package concurrency

import (
	"context"
	"sync"
)

// Gate admits requests per credential, blocking while a credential is at its
// limit. The zero value is not usable; call New.
type Gate struct {
	mu       sync.Mutex
	inFlight map[string]int
	waiters  map[string][]chan struct{} // FIFO queue per credential
}

// New constructs an empty Gate.
func New() *Gate {
	return &Gate{
		inFlight: make(map[string]int),
		waiters:  make(map[string][]chan struct{}),
	}
}

// State is one credential's live occupancy.
type State struct {
	InFlight int `json:"in_flight"`
	Waiting  int `json:"waiting"`
}

// Acquire blocks until credentialID has a free slot, then returns a release
// func the caller must invoke when the request is fully done — including the
// whole streamed response body, since a stream that is still being relayed is
// still occupying the provider.
//
// A limit <= 0 means unlimited and returns immediately. The limit is passed per
// call rather than registered up front so that a config reload takes effect on
// the next acquisition without having to rebuild anything.
//
// It returns ctx.Err() only if the caller gives up while queued; in that case
// no slot was taken and release must not be called.
func (g *Gate) Acquire(ctx context.Context, credentialID string, limit int) (func(), error) {
	if limit <= 0 || credentialID == "" {
		return func() {}, nil
	}

	for {
		g.mu.Lock()
		if g.inFlight[credentialID] < limit {
			g.inFlight[credentialID]++
			g.mu.Unlock()
			return g.releaser(credentialID), nil
		}
		// Full: join the back of the queue and wait to be woken by a release.
		ch := make(chan struct{})
		g.waiters[credentialID] = append(g.waiters[credentialID], ch)
		g.mu.Unlock()

		select {
		case <-ch:
			// Woken, but not handed a slot — loop and re-check. Re-checking
			// rather than transferring ownership is what lets the limit change
			// underneath us, and keeps release cheap.
		case <-ctx.Done():
			g.abandon(credentialID, ch)
			return nil, ctx.Err()
		}
	}
}

// Stats reports live occupancy per credential, for the admin API.
func (g *Gate) Stats() map[string]State {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make(map[string]State, len(g.inFlight))
	for cred, n := range g.inFlight {
		out[cred] = State{InFlight: n, Waiting: len(g.waiters[cred])}
	}
	// A credential can have waiters recorded under a limit that has since
	// dropped to zero in-flight; surface those too.
	for cred, q := range g.waiters {
		if _, seen := out[cred]; !seen && len(q) > 0 {
			out[cred] = State{Waiting: len(q)}
		}
	}
	return out
}

// releaser returns an idempotent release func, so a caller that defers it and
// also calls it explicitly cannot double-free a slot.
func (g *Gate) releaser(credentialID string) func() {
	var once sync.Once
	return func() { once.Do(func() { g.release(credentialID) }) }
}

func (g *Gate) release(credentialID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if n := g.inFlight[credentialID]; n > 1 {
		g.inFlight[credentialID] = n - 1
	} else {
		delete(g.inFlight, credentialID)
	}
	g.wakeOneLocked(credentialID)
}

// abandon removes ch from the queue after the caller gave up. If ch is already
// gone a release signalled it concurrently, and that wake-up would be lost — so
// pass it on to the next waiter instead of dropping it.
func (g *Gate) abandon(credentialID string, ch chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()

	q := g.waiters[credentialID]
	for i, c := range q {
		if c == ch {
			g.waiters[credentialID] = append(q[:i:i], q[i+1:]...)
			if len(g.waiters[credentialID]) == 0 {
				delete(g.waiters, credentialID)
			}
			return
		}
	}
	g.wakeOneLocked(credentialID)
}

// wakeOneLocked signals the longest-waiting caller. Caller must hold g.mu.
func (g *Gate) wakeOneLocked(credentialID string) {
	q := g.waiters[credentialID]
	if len(q) == 0 {
		return
	}
	next := q[0]
	g.waiters[credentialID] = q[1:]
	if len(g.waiters[credentialID]) == 0 {
		delete(g.waiters, credentialID)
	}
	close(next)
}

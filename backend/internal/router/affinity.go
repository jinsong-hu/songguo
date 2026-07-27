package router

import (
	"sort"
	"sync"
	"time"
)

// affinity remembers which vendor served each session, so a conversation stays
// on one provider and keeps its prompt cache warm. On a 200k-token agent
// context the difference between a cache hit and a cold start is roughly a
// factor of ten in input cost, which makes this the most expensive routing
// decision songguo makes.
//
// It is deliberately a CACHE, not a record: entries expire, get evicted under
// pressure, and vanish on restart. Losing one costs a single cold prompt, never
// a wrong answer, so the map is allowed to forget whenever forgetting is
// cheaper than remembering.
//
// Bounded three ways, because an unbounded map keyed by session id is a slow
// memory leak: an idle TTL, an amortized sweep, and a hard cap.
type affinity struct {
	now func() time.Time
	ttl time.Duration
	max int

	mu        sync.Mutex
	entries   map[string]affinityEntry
	nextSweep time.Time
}

type affinityEntry struct {
	vendor string
	seen   time.Time
}

// sweepInterval is how often expired entries are collected. The sweep is
// amortized onto ordinary get/set calls rather than run by a background
// goroutine: the router has no lifecycle to hang one off, and a map this small
// does not justify inventing one.
const sweepInterval = 5 * time.Minute

func newAffinity(now func() time.Time, ttl time.Duration, max int) *affinity {
	return &affinity{
		now:       now,
		ttl:       ttl,
		max:       max,
		entries:   make(map[string]affinityEntry),
		nextSweep: now().Add(sweepInterval),
	}
}

// get returns the vendor pinned to key, or "" when there is none or it has
// gone stale. A miss is not an error — it just means normal ordering decides.
func (a *affinity) get(key string) string {
	if key == "" {
		return ""
	}
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(now)

	e, ok := a.entries[key]
	if !ok || now.Sub(e.seen) >= a.ttl {
		return ""
	}
	return e.vendor
}

// set pins key to vendor and refreshes its idle timer.
func (a *affinity) set(key, vendor string) {
	if key == "" || vendor == "" {
		return
	}
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(now)

	a.entries[key] = affinityEntry{vendor: vendor, seen: now}
	a.evictLocked()
}

// sweepLocked drops expired entries, at most once per sweepInterval.
func (a *affinity) sweepLocked(now time.Time) {
	if now.Before(a.nextSweep) {
		return
	}
	a.nextSweep = now.Add(sweepInterval)
	for k, e := range a.entries {
		if now.Sub(e.seen) >= a.ttl {
			delete(a.entries, k)
		}
	}
}

// evictLocked enforces the hard cap by dropping the least recently seen
// entries. It runs only when the map is genuinely over the cap, which for any
// realistic session volume is never — it exists so that a pathological client
// minting unique session ids cannot exhaust memory.
func (a *affinity) evictLocked() {
	if len(a.entries) <= a.max {
		return
	}
	type aged struct {
		key  string
		seen time.Time
	}
	all := make([]aged, 0, len(a.entries))
	for k, e := range a.entries {
		all = append(all, aged{key: k, seen: e.seen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seen.Before(all[j].seen) })
	for i := 0; i < len(all)-a.max; i++ {
		delete(a.entries, all[i].key)
	}
}

// countFor reports how many live sessions are pinned to vendorName, for the
// routing state view.
func (a *affinity) countFor(vendorName string) int {
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	n := 0
	for _, e := range a.entries {
		if e.vendor == vendorName && now.Sub(e.seen) < a.ttl {
			n++
		}
	}
	return n
}

// reset drops every pin. Called on config reload, where a vendor may have just
// been renamed or removed underneath the pins.
func (a *affinity) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = make(map[string]affinityEntry)
	a.nextSweep = a.now().Add(sweepInterval)
}

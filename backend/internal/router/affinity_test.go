package router

import (
	"fmt"
	"testing"
	"time"
)

// twoVendorSamePriorityYAML is two interchangeable vendors in one tier — the
// shape where the weighted draw, and therefore stickiness, actually matters.
const twoVendorSamePriorityYAML = `
vendors:
  - name: alpha
    origin: https://alpha.example
    served_models: [m]
    priority: 1
    credential: {id: a1, api_key: k}
  - name: beta
    origin: https://beta.example
    served_models: [m]
    priority: 1
    credential: {id: b1, api_key: k}
`

func sessionSel(session string) Selector {
	return Selector{Scope: ScopeModel, Model: "m", Session: session}
}

func leadFor(t *testing.T, r *Router, sel Selector) string {
	t.Helper()
	got, err := r.Select(sel)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	return got[0].Vendor.Name
}

// The whole point of the feature: a session keeps its provider across turns so
// the prompt cache stays warm, even though the underlying draw is random.
func TestSessionSticksToOneVendor(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	sel := sessionSel("session-1")
	first := leadFor(t, r, sel)
	r.Pin(sel, first)

	for i := 0; i < 50; i++ {
		got := leadFor(t, r, sel)
		if got != first {
			t.Fatalf("turn %d went to %q, want %q: the session must not migrate", i, got, first)
		}
		r.Pin(sel, got)
		clk.Advance(2 * time.Second)
	}
}

// Stickiness must not collapse into "everyone uses one vendor" — distinct
// sessions are still spread by the weighted draw.
func TestDistinctSessionsSpreadAcrossVendors(t *testing.T) {
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Logger: quietLogger()})

	seen := map[string]int{}
	for i := 0; i < 400; i++ {
		sel := sessionSel(fmt.Sprintf("session-%d", i))
		v := leadFor(t, r, sel)
		r.Pin(sel, v)
		seen[v]++
	}
	if seen["alpha"] == 0 || seen["beta"] == 0 {
		t.Fatalf("sessions = %v, want both vendors used", seen)
	}
}

// A pin is only honored while its vendor works. This is what lets stickiness
// sort ahead of health without ever stranding a session on a broken provider.
func TestStickyYieldsWhenPinnedVendorFails(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	sel := sessionSel("session-1")
	pinned := leadFor(t, r, sel)
	r.Pin(sel, pinned)

	r.Report(pinned, "", SignalFail)

	got := leadFor(t, r, sel)
	if got == pinned {
		t.Fatalf("lead = %q, want the session moved off its demoted vendor", pinned)
	}

	// And the session re-pins to wherever it actually landed, so it does not
	// flap back the moment the original recovers.
	r.Pin(sel, got)
	clk.Advance(time.Hour) // original vendor's cooldown lapses
	if again := leadFor(t, r, sel); again != got {
		t.Fatalf("lead = %q, want %q: a recovered vendor must not yank a live session back", again, got)
	}
}

// A pinned session outranks priority: migrating it back to a recovered primary
// would burn a warm cache to satisfy a preference among interchangeable vendors.
func TestStickyOutranksPriority(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML) // primary p1, backup p2
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	sel := sessionSel("session-1")
	r.Pin(sel, "backup")

	if got := leadFor(t, r, sel); got != "backup" {
		t.Fatalf("lead = %q, want backup: an existing pin beats priority", got)
	}
	// A session with no pin still prefers the primary, so the fleet drains back
	// to priority order as old sessions end.
	if got := leadFor(t, r, sessionSel("fresh-session")); got != "primary" {
		t.Fatalf("lead = %q, want primary for an unpinned session", got)
	}
}

// Health outranks stickiness, so a pin is only ever consulted among vendors of
// EQUAL health. That is what makes the guarantee structural rather than a
// property of how stickiness is computed.
func TestHealthOutranksSticky(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	sel := sessionSel("session-1")
	r.Pin(sel, "alpha")
	r.Report("alpha", "", SignalFail) // pinned vendor starts cooling

	if got := leadFor(t, r, sel); got != "beta" {
		t.Fatalf("lead = %q, want beta: health must outrank a pin", got)
	}
}

// The payoff of ordering health above a PURE sticky rank: when every candidate
// is equally degraded, the pin still breaks the tie, so a session forced onto a
// cooling vendor lands on the one already holding its warm cache instead of
// being reassigned at random.
func TestStickyStillBreaksTiesAmongDegradedVendors(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	sel := sessionSel("session-1")
	r.Pin(sel, "alpha")

	// Both vendors are cooling — nothing healthy is left to prefer.
	r.Report("alpha", "", SignalFail)
	r.Report("beta", "", SignalFail)

	for i := 0; i < 20; i++ {
		if got := leadFor(t, r, sel); got != "alpha" {
			t.Fatalf("lead = %q on call %d, want alpha: among equally degraded "+
				"vendors the pinned one keeps the session", got, i)
		}
	}
}

// No session header means no affinity. We never require a header to get routed.
func TestNoSessionMeansNoAffinity(t *testing.T) {
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Logger: quietLogger()})

	sel := Selector{Scope: ScopeModel, Model: "m"} // no Session
	r.Pin(sel, "alpha")                            // must be a no-op

	seen := map[string]int{}
	for i := 0; i < 400; i++ {
		seen[leadFor(t, r, sel)]++
	}
	if seen["alpha"] == 0 || seen["beta"] == 0 {
		t.Fatalf("leads = %v, want an unpinned selector to keep spreading", seen)
	}
}

// One session addressing two models pins each independently — a single pin
// could not satisfy both when the models are served by different vendors.
func TestAffinityIsScopedPerModel(t *testing.T) {
	a := Selector{Scope: ScopeModel, Model: "m1", Session: "s"}
	b := Selector{Scope: ScopeModel, Model: "m2", Session: "s"}
	if a.affinityKey() == b.affinityKey() {
		t.Fatalf("affinity keys collide across models: %q", a.affinityKey())
	}
	if (Selector{Scope: ScopeModel, Model: "m1"}).affinityKey() != "" {
		t.Fatal("a sessionless selector must produce no affinity key")
	}
}

// --- affinity map hygiene ---------------------------------------------------

func TestAffinityExpiresAfterTTL(t *testing.T) {
	clk := newFakeClock()
	a := newAffinity(clk.Now, 30*time.Minute, 100)

	a.set("s", "alpha")
	if got := a.get("s"); got != "alpha" {
		t.Fatalf("get = %q, want alpha", got)
	}

	clk.Advance(31 * time.Minute)
	if got := a.get("s"); got != "" {
		t.Fatalf("get = %q, want empty after the idle TTL", got)
	}
}

func TestAffinityTTLIsIdleNotAbsolute(t *testing.T) {
	clk := newFakeClock()
	a := newAffinity(clk.Now, 30*time.Minute, 100)

	// An active session refreshes its pin on every turn and never expires.
	for i := 0; i < 20; i++ {
		a.set("s", "alpha")
		clk.Advance(20 * time.Minute)
	}
	if got := a.get("s"); got != "alpha" {
		t.Fatalf("get = %q, want alpha: an active session must keep its pin", got)
	}
}

// The map must stay bounded even against a client minting unique session ids —
// this is the leak the per-user rate limiter's map already has.
func TestAffinityIsBounded(t *testing.T) {
	clk := newFakeClock()
	const max = 100
	a := newAffinity(clk.Now, time.Hour, max)

	for i := 0; i < max*5; i++ {
		a.set(fmt.Sprintf("session-%d", i), "alpha")
		clk.Advance(time.Millisecond)
	}

	a.mu.Lock()
	n := len(a.entries)
	a.mu.Unlock()
	if n > max {
		t.Fatalf("entries = %d, want <= %d", n, max)
	}
	// Eviction is least-recently-seen, so the newest pin must have survived.
	if got := a.get(fmt.Sprintf("session-%d", max*5-1)); got != "alpha" {
		t.Fatalf("newest pin = %q, want alpha to survive eviction", got)
	}
}

// A config reload clears health but KEEPS session pins. Dropping them would
// cost a cold prompt for every active session on every operator edit, to solve
// a problem that resolves itself.
func TestResetHealthKeepsPins(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	sel := sessionSel("session-1")
	r.Pin(sel, "backup")
	r.Report("primary", "", SignalFail) // demote the p1 vendor
	r.ResetHealth()

	// Health was cleared...
	if len(r.Inspect()) != 0 {
		t.Fatalf("inspect = %+v, want health cleared", r.Inspect())
	}
	// ...but the session did not lose its warm provider.
	if got := leadFor(t, r, sel); got != "backup" {
		t.Fatalf("lead = %q, want backup: a reload must not drop session pins", got)
	}
}

// A pin naming a vendor the reload removed is inert rather than harmful: it
// matches nothing, so ordinary ordering decides and the next dispatch
// overwrites it.
func TestStalePinIsInert(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	sel := sessionSel("session-1")
	r.Pin(sel, "vendor-that-no-longer-exists")

	if got := leadFor(t, r, sel); got != "primary" {
		t.Fatalf("lead = %q, want primary: a stale pin must not distort ordering", got)
	}
}

package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"os"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock, so cooldown expiry is testable
// without sleeping. Safe for concurrent use because the router calls Now under
// its own lock from multiple goroutines.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// quietLogger discards demotion/restoration logs so test output stays readable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// twoVendorSnap is a p1 primary and a p2 backup, both serving model "m" — the
// canonical multi-provider shape this feature exists for.
const twoVendorYAML = `
vendors:
  - name: primary
    origin: https://primary.example
    served_models: [m]
    priority: 1
    credential: {id: p1, api_key: k}
  - name: backup
    origin: https://backup.example
    served_models: [m]
    priority: 2
    credential: {id: b1, api_key: k}
`

// splitProviderYAML mirrors what configsvc produces for ONE provider that
// declares endpoints on two different (origin, adapter) pairs: two routing
// vendors, independent hosts, but a single shared credential. The third vendor
// belongs to a different provider and must never be touched by a
// credential-scoped signal.
const splitProviderYAML = `
vendors:
  - name: acme
    origin: https://api.acme.example
    served_models: [m]
    priority: 1
    credential: {id: acme, api_key: k}
  - name: acme-anthropic
    origin: https://anthropic.acme.example
    served_models: [m]
    priority: 1
    credential: {id: acme, api_key: k}
  - name: other
    origin: https://other.example
    served_models: [m]
    priority: 2
    credential: {id: other, api_key: k2}
`

func leadName(t *testing.T, r *Router) string {
	t.Helper()
	got, err := r.Candidates("m")
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	return got[0].Vendor.Name
}

// --- Classify ---------------------------------------------------------------

func TestClassify(t *testing.T) {
	// Transport failures carry no status code — the vendor never answered — so
	// these are built as the real error values net/http would surface, wrapped
	// the way *url.Error wraps them in practice.
	connRefused := &url.Error{Op: "Post", URL: "https://v.example/v1/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp",
			Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}
	nxdomain := &url.Error{Op: "Post", URL: "https://nope.example/v1",
		Err: &net.OpError{Op: "dial", Net: "tcp",
			Err: &net.DNSError{Err: "no such host", Name: "nope.example", IsNotFound: true}}}
	dnsTempFail := &url.Error{Op: "Post", URL: "https://v.example/v1",
		Err: &net.DNSError{Err: "server misbehaving", Name: "v.example", IsTemporary: true}}
	badCert := &url.Error{Op: "Post", URL: "https://v.example/v1",
		Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}
	unknownCA := &url.Error{Op: "Post", URL: "https://v.example/v1",
		Err: x509.UnknownAuthorityError{}}
	timeout := &url.Error{Op: "Post", URL: "https://v.example/v1",
		Err: context.DeadlineExceeded}
	connReset := &url.Error{Op: "Post", URL: "https://v.example/v1",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	eof := &url.Error{Op: "Post", URL: "https://v.example/v1", Err: io.ErrUnexpectedEOF}

	tests := []struct {
		name       string
		status     int
		err        error
		clientGone bool
		want       Signal
	}{
		{"200 ok", 200, nil, false, SignalOK},
		{"201 ok", 201, nil, false, SignalOK},
		{"302 ok", 302, nil, false, SignalOK},

		// Conclusive: an identical retry would fail identically, so one is
		// enough. All of these are properties of the endpoint, not the request.
		{"connection refused", 0, connRefused, false, SignalFailHard},
		{"dns nxdomain", 0, nxdomain, false, SignalFailHard},
		{"tls cert unverified", 0, badCert, false, SignalFailHard},
		{"tls unknown authority", 0, unknownCA, false, SignalFailHard},

		// Conclusive AND credential-scoped: a revoked key is dead on every host
		// that presents it, not just the one that observed the rejection.
		{"401 revoked credential", 401, nil, false, SignalFailCredential},

		// Ambiguous: real failures, but as plausibly about this request or the
		// network as about the vendor. Need corroboration.
		{"dial/header timeout", 0, timeout, false, SignalFail},
		{"connection reset", 0, connReset, false, SignalFail},
		{"unexpected eof", 0, eof, false, SignalFail},
		{"dns temporary failure", 0, dnsTempFail, false, SignalFail},
		{"500", 500, nil, false, SignalFail},
		{"502", 502, nil, false, SignalFail},
		{"503", 503, nil, false, SignalFail},
		{"403 rejected (overloaded meaning)", 403, nil, false, SignalFail},

		// Narrower than any other failure: the vendor is serving, this model's
		// quota is spent. Scoped to (vendor, model) so one hot model cannot walk
		// the vendor's whole catalogue out of rotation.
		{"429 model quota", 429, nil, false, SignalFailModel},

		// Probably the caller, possibly this relay missing an endpoint its
		// siblings serve. Too weak to count per request; resolved by counting
		// distinct sessions instead.
		{"400 bad body", 400, nil, false, SignalFailClient},
		{"404 bad path", 404, nil, false, SignalFailClient},
		{"408 timeout", 408, nil, false, SignalFailClient},
		{"422 unprocessable", 422, nil, false, SignalFailClient},
		{"451 unavailable for legal reasons", 451, nil, false, SignalFailClient},

		// A client that hangs up must never be blamed on the vendor — not even
		// when the abort surfaces as a hard-looking transport error. This is
		// now the ONLY route to neutral: everything the vendor actually said
		// carries some weight.
		{"client aborted", 0, context.Canceled, true, SignalNeutral},
		{"client gone during conn refused", 0, connRefused, true, SignalNeutral},
		{"context canceled alone", 0, context.Canceled, false, SignalNeutral},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.err, tc.clientGone); got != tc.want {
				t.Errorf("Classify(%d, %v, %v) = %v, want %v",
					tc.status, tc.err, tc.clientGone, got, tc.want)
			}
		})
	}
}

// TestHardFailureDemotesImmediately: conclusive evidence should not wait for
// corroboration. A vendor whose port is closed is known-dead on the first
// attempt, so spending two more real client requests to re-prove it is pure
// waste.
func TestHardFailureDemotesImmediately(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	r.Report("primary", "", SignalFailHard)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup: one conclusive failure must demote", got)
	}

	states := r.Inspect()
	if !states[0].Cooling || states[0].Demotions != 1 {
		t.Fatalf("state = %+v, want cooling with 1 demotion", states[0])
	}
}

// TestHardAndSoftFailuresShareTheStreak: a hard failure short-circuits the
// count, but a success still clears it — the two kinds are strengths of the
// same evidence, not separate state machines.
func TestHardAndSoftFailuresShareTheStreak(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	// A soft failure alone is not enough...
	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary after one soft failure", got)
	}
	// ...but a hard one arriving next is conclusive on its own.
	r.Report("primary", "", SignalFailHard)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup", got)
	}

	// A success clears everything, hard demotion included.
	r.Report("primary", "", SignalOK)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: a success clears a hard demotion too", got)
	}
	if states := r.Inspect(); states[0].Cooling || states[0].ConsecFails != 0 {
		t.Fatalf("state = %+v, want live with a clean streak", states[0])
	}
}

// TestCredentialFailureDemotesEverySibling: a revoked key is dead on every host
// presenting it. Making each sibling vendor rediscover that with its own failed
// client request would be re-proving a fact we already hold.
func TestCredentialFailureDemotesEverySibling(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, splitProviderYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	// One 401 observed on one of the provider's two vendors.
	r.Report("acme", "acme", SignalFailCredential)

	cooling := coolingSet(r)
	if !cooling["acme"] || !cooling["acme-anthropic"] {
		t.Errorf("cooling = %v, want both vendors of credential acme", cooling)
	}
	// The other provider shares nothing and must be untouched.
	if cooling["other"] {
		t.Errorf("cooling = %v, want vendor 'other' (credential other) left live", cooling)
	}
	if got := leadName(t, r); got != "other" {
		t.Fatalf("lead = %q, want other: both acme vendors are cooling", got)
	}
}

// TestEndpointFailureSpareseSiblings: the counterpart. A host-scoped failure
// says nothing about the provider's OTHER host, even on the same key.
func TestEndpointFailureSparesSiblings(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, splitProviderYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	// Connection refused on one origin — conclusive, but only for that endpoint.
	r.Report("acme", "acme", SignalFailHard)

	cooling := coolingSet(r)
	if !cooling["acme"] {
		t.Errorf("cooling = %v, want acme demoted", cooling)
	}
	if cooling["acme-anthropic"] {
		t.Errorf("cooling = %v, want acme-anthropic live: a dead host says nothing "+
			"about its sibling host on the same key", cooling)
	}
	if got := leadName(t, r); got != "acme-anthropic" {
		t.Fatalf("lead = %q, want acme-anthropic (still p1 and live)", got)
	}
}

// TestCredentialFailureNeverExcludes: the permutation invariant holds even when
// a credential failure demotes several vendors at once.
func TestCredentialFailureNeverExcludes(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, splitProviderYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	r.Report("acme", "acme", SignalFailCredential)
	r.Report("other", "other", SignalFailCredential)

	got, err := r.Candidates("m")
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want all 3 even with every credential dead", len(got))
	}
}

// coolingSet reports which vendors are currently demoted.
func coolingSet(r *Router) map[string]bool {
	out := map[string]bool{}
	for _, s := range r.Inspect() {
		out[s.Vendor] = s.Cooling
	}
	return out
}

// --- Dead state -------------------------------------------------------------

// A vendor that keeps failing across demotions stops being "cooling" and
// becomes "dead": ranked below even a cooling vendor, and — unlike a cooldown —
// never restored by the clock alone.
func TestRepeatedDemotionsMarkDead(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 1, DeadAfter: 3, Logger: quietLogger(),
	})

	for i := 0; i < 3; i++ {
		r.Report("primary", "", SignalFail)
		clk.Advance(time.Hour) // let each cooldown lapse
	}

	st := stateOf(t, r, "primary")
	if !st.Dead {
		t.Fatalf("state = %+v, want dead after 3 consecutive demotions", st)
	}
	// And no amount of waiting brings it back.
	clk.Advance(24 * time.Hour)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup: dead must not expire on a timer", got)
	}
}

// Dead still never excludes. If EVERY vendor is dead — a partition on our side
// rather than a real outage — the least-bad candidate still gets the request,
// which is what lets the situation heal itself instead of paging someone.
func TestAllDeadStillDispatchesAndCanRecover(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 1, DeadAfter: 2, Logger: quietLogger(),
	})

	for i := 0; i < 2; i++ {
		r.Report("primary", "", SignalFail)
		r.Report("backup", "", SignalFail)
		clk.Advance(time.Hour)
	}
	if !stateOf(t, r, "primary").Dead || !stateOf(t, r, "backup").Dead {
		t.Fatal("want both vendors dead")
	}

	got, err := r.Candidates("m")
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 even with everything dead", len(got))
	}
	// Among dead vendors, priority still decides — so the request goes somewhere
	// sensible rather than nowhere.
	if got[0].Vendor.Name != "primary" {
		t.Fatalf("lead = %q, want primary (priority breaks the tie)", got[0].Vendor.Name)
	}

	// One success is proof, and proof beats accumulated suspicion.
	r.Report("primary", "", SignalOK)
	if st := stateOf(t, r, "primary"); st.Dead || st.Demotions != 0 {
		t.Fatalf("state = %+v, want fully cleared by a success", st)
	}
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary restored", got)
	}
}

// A success anywhere in the streak resets the demotion count, so an
// intermittently-working vendor never accumulates its way to dead.
func TestIntermittentVendorNeverDies(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 1, DeadAfter: 3, Logger: quietLogger(),
	})

	for i := 0; i < 10; i++ {
		r.Report("primary", "", SignalFail)
		clk.Advance(time.Hour)
		r.Report("primary", "", SignalOK)
	}
	if st := stateOf(t, r, "primary"); st.Dead {
		t.Fatalf("state = %+v, want alive: successes must break the streak", st)
	}
}

// Cooldowns double so a dead vendor stops costing a failed request every 30s.
func TestCooldownBacksOff(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 1, DeadAfter: 99,
		Cooldown: time.Minute, CooldownMax: 8 * time.Minute, Logger: quietLogger(),
	})

	want := []time.Duration{1, 2, 4, 8, 8, 8}
	for i, mins := range want {
		r.Report("primary", "", SignalFail)
		st := stateOf(t, r, "primary")
		got := st.CoolingUntil.Sub(clk.Now())
		if got != mins*time.Minute {
			t.Errorf("demotion %d cooldown = %v, want %v", i+1, got, mins*time.Minute)
		}
		clk.Advance(time.Hour)
	}
}

func stateOf(t *testing.T, r *Router, vendor string) VendorState {
	t.Helper()
	for _, s := range r.Inspect() {
		if s.Vendor == vendor {
			return s
		}
	}
	t.Fatalf("no routing state for vendor %q", vendor)
	return VendorState{}
}

// --- Demotion state machine -------------------------------------------------

func TestReportDemotesAfterThreshold(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	// Two failures are not enough — a lone blip must not flap the rotation.
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("after 2 failures lead = %q, want primary (below threshold)", got)
	}

	// The third trips it.
	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("after 3 failures lead = %q, want backup (primary demoted)", got)
	}
}

// TestDemotionBeatsPriority encodes the load-bearing design decision: health
// outranks priority. If this test is ever "fixed" to expect primary, the whole
// feature is gone — a p1 primary would never yield to its p2 backup.
func TestDemotionBeatsPriority(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	r.Report("primary", "", SignalFail)

	got, err := r.Candidates("m")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Vendor.Name != "backup" {
		t.Fatalf("lead = %q, want backup: a cooling priority-1 vendor must rank below a live priority-2 one", got[0].Vendor.Name)
	}
	if got[1].Vendor.Name != "primary" {
		t.Fatalf("second = %q, want primary: demoted, not excluded", got[1].Vendor.Name)
	}
}

// TestDemotedNeverExcluded is the package invariant: Select returns a
// permutation. Even with every vendor cooling, nothing is dropped and nothing
// errors — otherwise health state could manufacture a gateway refusal.
func TestDemotedNeverExcluded(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	r.Report("primary", "", SignalFail)
	r.Report("backup", "", SignalFail)

	got, err := r.Candidates("m")
	if err != nil {
		t.Fatalf("all vendors cooling must NOT produce an error, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: cooling demotes but never excludes", len(got))
	}
	// With health tied, the original priority order resumes.
	if got[0].Vendor.Name != "primary" || got[1].Vendor.Name != "backup" {
		t.Fatalf("order = %q,%q; want primary,backup (health tied -> priority decides)",
			got[0].Vendor.Name, got[1].Vendor.Name)
	}
}

func TestCooldownExpiryRestores(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 1, Cooldown: 30 * time.Second, Logger: quietLogger(),
	})

	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup while cooling", got)
	}

	clk.Advance(29 * time.Second)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup: still inside the cooldown", got)
	}

	// At expiry the vendor is simply live again — that is the half-open probe,
	// and it costs no extra bookkeeping.
	clk.Advance(2 * time.Second)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: cooldown lapsed", got)
	}
}

// TestProbationAfterCooldown pins the cost of passive detection at ONE
// client-visible failure per cooldown window, not failThreshold of them.
//
// After a cooldown lapses the vendor is selectable again, but its failure
// streak is still standing — so a single fresh failure re-demotes it. Were the
// streak reset on demotion, a permanently dead primary would burn 3 real client
// requests every 30 seconds, forever.
func TestProbationAfterCooldown(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 3, Cooldown: 30 * time.Second, Logger: quietLogger(),
	})

	// First demotion costs the full threshold.
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup after 3 failures", got)
	}

	// Cooldown lapses: primary leads again on priority — that IS the probe.
	clk.Advance(31 * time.Second)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary once the cooldown lapsed", got)
	}

	// It fails that probe. One failure must be enough to re-demote.
	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup: a vendor on probation re-demotes on a single failure", got)
	}
	if states := r.Inspect(); states[0].Demotions != 2 {
		t.Fatalf("demotions = %d, want 2", states[0].Demotions)
	}

	// A success ends probation: the streak clears, so it takes a full threshold
	// again. Two failures must not be enough.
	r.Report("primary", "", SignalOK)
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: a success reset the streak, 2 < 3", got)
	}
}

func TestSuccessClearsCooldown(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup", got)
	}

	r.Report("primary", "", SignalOK)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: a success restores immediately", got)
	}
}

func TestSuccessResetsConsecutiveFailures(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 3, Logger: quietLogger()})

	// Failures must be CONSECUTIVE: an interleaved success resets the count, so
	// an intermittently-flaky-but-mostly-fine vendor is never demoted.
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalOK)
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)

	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: the success reset the streak", got)
	}
}

func TestNeutralNeitherDemotesNorRestores(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 2, Logger: quietLogger()})

	// A stream of caller errors must never demote.
	for i := 0; i < 10; i++ {
		r.Report("primary", "", SignalNeutral)
	}
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: neutral outcomes must not demote", got)
	}

	// Nor should a neutral outcome rescue a vendor that is already cooling.
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalFail)
	r.Report("primary", "", SignalNeutral)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup: neutral must not clear a cooldown", got)
	}
}

func TestResetHealthClearsState(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	r.Report("primary", "", SignalFail)
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q, want backup", got)
	}

	r.ResetHealth()
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary after ResetHealth", got)
	}
	if states := r.Inspect(); len(states) != 0 {
		t.Fatalf("Inspect() = %d entries, want 0 after ResetHealth", len(states))
	}
}

func TestReportUnknownVendorIsSafe(t *testing.T) {
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Logger: quietLogger()})

	// An empty name is ignored outright; an unknown one is recorded harmlessly
	// and cleaned up by the next reload.
	r.Report("", "", SignalFail)
	r.Report("ghost", "", SignalFail)

	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary", got)
	}
}

// --- Inspect ----------------------------------------------------------------

func TestInspectReportsCoolingState(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{
		Now: clk.Now, FailThreshold: 2, Cooldown: 30 * time.Second, Logger: quietLogger(),
	})

	// One failure: tracked, but not yet demoted.
	r.Report("primary", "", SignalFail)
	states := r.Inspect()
	if len(states) != 1 {
		t.Fatalf("Inspect() = %d entries, want 1 (only reported vendors appear)", len(states))
	}
	if states[0].Vendor != "primary" || states[0].Cooling || states[0].ConsecFails != 1 {
		t.Fatalf("state = %+v, want primary not cooling with 1 consec fail", states[0])
	}

	// Second failure demotes.
	r.Report("primary", "", SignalFail)
	states = r.Inspect()
	if !states[0].Cooling {
		t.Fatalf("state = %+v, want cooling", states[0])
	}
	if states[0].Demotions != 1 {
		t.Fatalf("demotions = %d, want 1", states[0].Demotions)
	}
	if want := clk.Now().Add(30 * time.Second); !states[0].CoolingUntil.Equal(want) {
		t.Fatalf("cooling_until = %v, want %v", states[0].CoolingUntil, want)
	}

	// Past expiry it reads live again, without needing a sweep.
	clk.Advance(31 * time.Second)
	if states = r.Inspect(); states[0].Cooling {
		t.Fatalf("state = %+v, want not cooling after expiry", states[0])
	}
}

// --- Invariant --------------------------------------------------------------

// TestSelectIsPermutation is the behavioral-transparency guard. Whatever health
// state the router is in, Select must return the same vendors it was given —
// only reordered. If this ever fails, health state has become able to empty a
// candidate list, which would let the router manufacture a refusal.
func TestSelectIsPermutation(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: a
    origin: https://a.example
    served_models: [m]
    priority: 1
    weight: 3
    credential: {id: a1, api_key: k}
  - name: b
    origin: https://b.example
    served_models: [m]
    priority: 1
    weight: 1
    credential: {id: b1, api_key: k}
  - name: c
    origin: https://c.example
    served_models: [m]
    priority: 2
    credential: {id: c1, api_key: k}
  - name: d
    origin: https://d.example
    served_models: [m]
    priority: 5
    credential: {id: d1, api_key: k}
`)
	clk := newFakeClock()
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	want := []string{"a", "b", "c", "d"}
	names := []string{"a", "b", "c", "d", "ghost"}
	signals := []Signal{SignalOK, SignalFail, SignalNeutral}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		// Drive the health state into an arbitrary configuration.
		r.Report(names[rng.Intn(len(names))], "", signals[rng.Intn(len(signals))])
		clk.Advance(time.Duration(rng.Intn(20)) * time.Second)

		got, err := r.Candidates("m")
		if err != nil {
			t.Fatalf("iteration %d: Candidates errored: %v", i, err)
		}
		gotNames := make([]string, len(got))
		for j, tg := range got {
			gotNames[j] = tg.Vendor.Name
		}
		sorted := append([]string(nil), gotNames...)
		sort.Strings(sorted)
		if len(sorted) != len(want) {
			t.Fatalf("iteration %d: got %d candidates %v, want %d", i, len(sorted), gotNames, len(want))
		}
		for j := range want {
			if sorted[j] != want[j] {
				t.Fatalf("iteration %d: candidate set = %v, want a permutation of %v", i, gotNames, want)
			}
		}
	}
}

// TestConcurrentReportAndSelect exercises the router's single mutex from many
// goroutines. Run under -race.
func TestConcurrentReportAndSelect(t *testing.T) {
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{FailThreshold: 2, Logger: quietLogger()})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			for j := 0; j < 500; j++ {
				if _, err := r.Candidates("m"); err != nil {
					t.Errorf("candidates: %v", err)
					return
				}
				switch rng.Intn(3) {
				case 0:
					r.Report("primary", "", SignalFail)
				case 1:
					r.Report("primary", "", SignalOK)
				default:
					r.Report("backup", "", SignalFail)
				}
				_ = r.Inspect()
			}
		}(i)
	}
	wg.Wait()
}

// --- Model-scoped rate limits ------------------------------------------------

const twoVendorBothModelsYAML = `
vendors:
  - name: primary
    origin: https://primary.example
    served_models: [m, other]
    priority: 1
    credential: {id: p1, api_key: k}
  - name: backup
    origin: https://backup.example
    served_models: [m, other]
    priority: 2
    credential: {id: b1, api_key: k}
`

func leadForModel(t *testing.T, r *Router, model string) string {
	t.Helper()
	got, err := r.Candidates(model)
	if err != nil {
		t.Fatalf("candidates(%s): %v", model, err)
	}
	return got[0].Vendor.Name
}

// A 429 on one model must not cost the vendor its other models. This is the
// whole reason the signal is scoped rather than vendor-wide.
func TestModelRateLimitSparesOtherModels(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorBothModelsYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	r.ReportModel("primary", "p1", "m", SignalFailModel)

	if got := leadForModel(t, r, "m"); got != "backup" {
		t.Errorf("lead for m = %q, want backup: the rate-limited model moves", got)
	}
	if got := leadForModel(t, r, "other"); got != "primary" {
		t.Errorf("lead for other = %q, want primary: an unrelated model must not move", got)
	}
}

// It is a cooldown, not a demotion: vendor health is untouched, so a 429 can
// never accumulate toward the dead state.
func TestModelRateLimitLeavesVendorHealthAlone(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorBothModelsYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, DeadAfter: 2, Logger: quietLogger()})

	for i := 0; i < 10; i++ {
		r.ReportModel("primary", "p1", "m", SignalFailModel)
	}
	for _, st := range r.Inspect() {
		if st.Vendor == "primary" && (st.Cooling || st.Dead || st.ConsecFails > 0) {
			t.Fatalf("state = %+v, want untouched vendor health", st)
		}
	}
}

func TestModelRateLimitExpires(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorBothModelsYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, ModelCooldown: time.Minute, Logger: quietLogger()})

	r.ReportModel("primary", "p1", "m", SignalFailModel)
	if got := leadForModel(t, r, "m"); got != "backup" {
		t.Fatalf("lead = %q, want backup", got)
	}

	clk.Advance(61 * time.Second)
	if got := leadForModel(t, r, "m"); got != "primary" {
		t.Fatalf("lead = %q, want primary once the quota window lapses", got)
	}
}

// A model-less request (WebSocket upgrade, GET /v1/models) has nothing to scope
// a 429 to. Widening it into a vendor-wide demotion would punish a healthy
// vendor for a quota that may not even apply, so it is dropped instead.
func TestModelRateLimitWithoutModelIsDropped(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorBothModelsYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, FailThreshold: 1, Logger: quietLogger()})

	r.ReportModel("primary", "p1", "", SignalFailModel)

	if got := leadForModel(t, r, "m"); got != "primary" {
		t.Fatalf("lead = %q, want primary: an unscoped 429 must not demote", got)
	}
	if len(r.Inspect()) != 0 {
		t.Fatalf("inspect = %+v, want no health entries", r.Inspect())
	}
}

// --- Client errors (4xx), counted per distinct session ----------------------

// clientFail reports a 4xx from one session against one vendor.
func clientFail(r *Router, vendor, session string) {
	r.ReportAttempt(Attempt{Vendor: vendor, Credential: "p1", Model: "m", Session: session}, SignalFailClient)
}

// clientFailFrom reports a 4xx from each of n distinct sessions.
func clientFailFrom(r *Router, vendor string, n int) {
	for i := range n {
		clientFail(r, vendor, fmt.Sprintf("sess-%d", i))
	}
}

// clientFailCount reports how many distinct sessions are recorded against
// vendor. Read straight off the health map because the count is deliberately
// NOT exported: it is an implementation detail of how a 4xx demotion is
// reached, and the reachable outcome is the ordinary cooling/dead state.
func clientFailCount(r *Router, vendor string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health[vendor]
	if h == nil {
		return 0
	}
	return len(h.clientFails)
}

// THE property the whole distinct-session design exists for. One client with a
// malformed body retries in a loop; every request 400s; the vendor is fine.
// Counting requests would demote it (and then every other vendor the same
// client touched, taking the fleet down). Counting sessions cannot.
func TestOneSessionCannotDemoteHoweverManyTimesItFails(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	for range 500 {
		clientFail(r, "primary", "one-broken-client")
	}

	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: one session must never demote a vendor", got)
	}
	if st, n := stateOf(t, r, "primary"), clientFailCount(r, "primary"); st.Demotions != 0 || n != 1 {
		t.Fatalf("state = %+v, sessions = %d, want 0 demotions and 1 session", st, n)
	}
}

// The other half: enough independent callers hitting the same wall IS the
// vendor's problem, and it must move the next request.
func TestDistinctSessionsDemoteOnClientErrors(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	// One short of the default threshold: still leading.
	clientFailFrom(r, "primary", defaultClientFailThreshold-1)
	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q after %d sessions, want primary: below threshold",
			got, defaultClientFailThreshold-1)
	}

	// The threshold'th distinct session tips it.
	clientFail(r, "primary", "the-tenth")
	if got := leadName(t, r); got != "backup" {
		t.Fatalf("lead = %q after %d sessions, want backup",
			got, defaultClientFailThreshold)
	}
	if st := stateOf(t, r, "primary"); !st.Cooling || st.Demotions != 1 {
		t.Fatalf("state = %+v, want cooling with 1 demotion", st)
	}
}

// A 4xx with no session cannot be attributed, so it is dropped rather than
// widened — the same choice ReportModel makes for a model-less 429. Clients
// that send no session header therefore cannot demote anyone on 4xx.
func TestClientFailWithoutSessionIsDropped(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, ClientFailThreshold: 1, Logger: quietLogger()})

	for range 50 {
		r.ReportAttempt(Attempt{Vendor: "primary", Credential: "p1", Model: "m"}, SignalFailClient)
	}
	// The legacy entry points carry no session either.
	r.ReportModel("primary", "p1", "m", SignalFailClient)
	r.Report("primary", "p1", SignalFailClient)

	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: an unattributable 4xx must not demote", got)
	}
	if len(r.Inspect()) != 0 {
		t.Fatalf("inspect = %+v, want no health entries", r.Inspect())
	}
}

// This is what keeps a busy, healthy vendor from ever accumulating a demotion
// out of scattered client mistakes: reaching the threshold requires distinct
// sessions to fail with NO success anywhere among them.
func TestSuccessClearsTheSessionSet(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	for i := range defaultClientFailThreshold * 3 {
		clientFail(r, "primary", fmt.Sprintf("sess-%d", i))
		r.ReportModel("primary", "p1", "m", SignalOK) // ordinary traffic succeeding
	}

	if got := leadName(t, r); got != "primary" {
		t.Fatalf("lead = %q, want primary: interleaved successes must reset the set", got)
	}
	if st, n := stateOf(t, r, "primary"), clientFailCount(r, "primary"); n != 0 || st.Demotions != 0 {
		t.Fatalf("state = %+v, sessions = %d, want an empty set and no demotions", st, n)
	}
}

// The ladder is driven by NEW sessions, so escalation to dead costs
// clientFailThreshold + deadAfter - 1 distinct callers — and a session already
// in the set can never push it further.
func TestClientErrorsReachDeadOnNewSessionsOnly(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	// The Nth distinct session past the threshold is the Nth demotion, so dead
	// arrives on this many distinct callers and not one fewer.
	deadAt := defaultClientFailThreshold + defaultDeadAfter - 1

	clientFailFrom(r, "primary", deadAt-1)
	if st := stateOf(t, r, "primary"); st.Dead || st.Demotions != defaultDeadAfter-1 {
		t.Fatalf("state = %+v, want %d demotions and not dead", st, defaultDeadAfter-1)
	}
	// Re-failing from sessions already counted must not advance the ladder.
	for range 100 {
		clientFailFrom(r, "primary", deadAt-1)
	}
	if st := stateOf(t, r, "primary"); st.Dead || st.Demotions != defaultDeadAfter-1 {
		t.Fatalf("state = %+v, want unchanged: repeats are not new evidence", st)
	}

	clientFail(r, "primary", "one-more-new-session")
	st := stateOf(t, r, "primary")
	if !st.Dead || st.Demotions != defaultDeadAfter {
		t.Fatalf("state = %+v, want dead after %d demotions", st, defaultDeadAfter)
	}
	// Dead still never excludes, and a success still revives.
	r.ReportModel("primary", "p1", "m", SignalOK)
	if st, n := stateOf(t, r, "primary"), clientFailCount(r, "primary"); st.Dead || n != 0 {
		t.Fatalf("state = %+v, sessions = %d, want revived with an empty set", st, n)
	}
}

// The session set is bounded: past the cap a vendor is dead several times over
// and another client-supplied session name buys no information, so it must not
// be stored. Without this the map grows with whatever a client puts in a header.
func TestSessionSetIsBounded(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	clientFailFrom(r, "primary", 10_000)

	want := defaultClientFailThreshold + defaultDeadAfter
	if n := clientFailCount(r, "primary"); n != want {
		t.Fatalf("tracked %d sessions, want the cap of %d", n, want)
	}
}

// The two failure classes are separate axes: a 4xx must not fill the streak
// that 5xx uses (3 would demote far too fast), and a 5xx must not fill the
// session set. Only their demotions share a ladder.
func TestClientErrorsAndVendorFailuresAreSeparateAxes(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	clientFailFrom(r, "primary", defaultClientFailThreshold-1)
	if st := stateOf(t, r, "primary"); st.ConsecFails != 0 {
		t.Fatalf("state = %+v, want consec_fails 0: a 4xx is not a 5xx strike", st)
	}

	r.ReportModel("backup", "b1", "m", SignalFail)
	if n := clientFailCount(r, "backup"); n != 0 {
		t.Fatalf("sessions = %d, want an empty session set: a 5xx is not a 4xx", n)
	}
}

// Never excludes, exactly like vendor health: if every vendor is rate-limited
// for a model, the request still goes somewhere.
func TestAllModelRateLimitedStillDispatches(t *testing.T) {
	clk := newFakeClock()
	snap := buildSnapshot(t, twoVendorBothModelsYAML)
	r := New(staticSnap(snap), Options{Now: clk.Now, Logger: quietLogger()})

	r.ReportModel("primary", "p1", "m", SignalFailModel)
	r.ReportModel("backup", "b1", "m", SignalFailModel)

	got, err := r.Candidates("m")
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	if got[0].Vendor.Name != "primary" {
		t.Fatalf("lead = %q, want primary (priority breaks the tie)", got[0].Vendor.Name)
	}
}

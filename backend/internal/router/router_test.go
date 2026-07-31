package router

import (
	"errors"
	"sync"
	"testing"

	"github.com/songguo/songguo/internal/config"
)

// buildSnapshot parses a YAML config into a Snapshot, failing the test on error.
func buildSnapshot(t *testing.T, yaml string) *config.Snapshot {
	t.Helper()
	snap, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return snap
}

func staticSnap(s *config.Snapshot) func() *config.Snapshot {
	return func() *config.Snapshot { return s }
}

func TestCandidatesNoVendor(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: a
    origin: https://a.example
    served_models: [gpt-4o]
    credential: {id: a1, api_key: k}
`)
	r := New(staticSnap(snap))
	if _, err := r.Candidates("nonexistent"); !errors.Is(err, ErrNoVendor) {
		t.Fatalf("want ErrNoVendor, got %v", err)
	}
}

func TestCandidatesNilSnapshot(t *testing.T) {
	r := New(func() *config.Snapshot { return nil })
	if _, err := r.Candidates("x"); !errors.Is(err, ErrNoVendor) {
		t.Fatalf("want ErrNoVendor on nil snapshot, got %v", err)
	}
}

func TestCandidatesSingleVendor(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: a
    origin: https://a.example
    served_models: [gpt-4o]
    credential: {id: a1, api_key: k}
`)
	r := New(staticSnap(snap))
	got, err := r.Candidates("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Vendor.Name != "a" || got[0].Credential.ID != "a1" {
		t.Fatalf("got %+v", got)
	}
}

func TestCredentialIDDefaultsToVendorName(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: a
    origin: https://a.example
    served_models: [gpt-4o]
    credential: {api_key: k}
`)
	r := New(staticSnap(snap))
	got, err := r.Candidates("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Credential.ID != "a" {
		t.Fatalf("credential id = %q, want vendor name fallback", got[0].Credential.ID)
	}
}

func TestPriorityOrdering(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: low
    origin: https://low.example
    served_models: [m]
    priority: 2
    credential: {id: l1, api_key: k}
  - name: high
    origin: https://high.example
    served_models: [m]
    priority: 1
    credential: {id: h1, api_key: k}
`)
	r := New(staticSnap(snap))
	got, err := r.Candidates("m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Vendor.Name != "high" || got[1].Vendor.Name != "low" {
		t.Fatalf("priority order wrong: %v / %v", got[0].Vendor.Name, got[1].Vendor.Name)
	}
}

// Selection within a priority tier is a weighted random draw, not a rotation,
// so the split is correct in expectation rather than exact. Over this many
// samples the law of large numbers makes it tight; a short burst would not be.
func TestWeightedDistribution(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: heavy
    origin: https://heavy.example
    served_models: [m]
    priority: 1
    weight: 3
    credential: {id: h1, api_key: k}
  - name: light
    origin: https://light.example
    served_models: [m]
    priority: 1
    weight: 1
    credential: {id: l1, api_key: k}
`)
	r := New(staticSnap(snap))

	const n = 4000
	lead := map[string]int{}
	for i := 0; i < n; i++ {
		got, err := r.Candidates("m")
		if err != nil {
			t.Fatal(err)
		}
		lead[got[0].Vendor.Name]++
	}
	// Expect roughly 3:1. Tolerance is generous: this asserts the draw is
	// weighted, not that it is precise.
	ratio := float64(lead["heavy"]) / float64(lead["light"])
	if ratio < 2.4 || ratio > 3.6 {
		t.Fatalf("weighted ratio heavy/light = %.2f (heavy=%d light=%d), want ~3", ratio, lead["heavy"], lead["light"])
	}
}

// An equal-weight pair must actually alternate over time. Guards against a draw
// that is weighted but degenerate — e.g. always returning declaration order.
func TestEqualWeightsSplitEvenly(t *testing.T) {
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	r := New(staticSnap(snap), Options{Logger: quietLogger()})

	const n = 4000
	lead := map[string]int{}
	for i := 0; i < n; i++ {
		got, err := r.Candidates("m")
		if err != nil {
			t.Fatal(err)
		}
		lead[got[0].Vendor.Name]++
	}
	ratio := float64(lead["alpha"]) / float64(lead["beta"])
	if ratio < 0.85 || ratio > 1.18 {
		t.Fatalf("equal-weight ratio = %.2f (alpha=%d beta=%d), want ~1",
			ratio, lead["alpha"], lead["beta"])
	}
}

// parkedAndWeightedYAML is a parked vendor (weight 0) declared FIRST, so the
// declaration-order tiebreak would favor it if the draw ever let it through.
const parkedAndWeightedYAML = `
vendors:
  - name: parked
    origin: https://parked.example
    served_models: [m]
    priority: 1
    weight: 0
    credential: {id: p1, api_key: k}
  - name: live
    origin: https://live.example
    served_models: [m]
    priority: 1
    weight: 1
    credential: {id: l1, api_key: k}
`

// Weight 0 parks a vendor: no share of its tier, so a weighted sibling leads
// every single time — not merely most of the time, since the draw is +Inf rather
// than a very large finite number.
func TestZeroWeightTakesNoShareOfItsTier(t *testing.T) {
	snap := buildSnapshot(t, parkedAndWeightedYAML)
	r := New(staticSnap(snap), Options{Logger: quietLogger()})

	for i := 0; i < 500; i++ {
		got, err := r.Candidates("m")
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Vendor.Name != "live" {
			t.Fatalf("lead = %q on call %d, want live: weight 0 takes no share", got[0].Vendor.Name, i)
		}
		// Parking DEMOTES; it must never shorten the candidate list (see the
		// package invariant).
		if len(got) != 2 {
			t.Fatalf("candidates = %d, want 2: parking must not exclude", len(got))
		}
	}
}

// Parking is a share of the tier, not a veto — with nobody to lose the draw to,
// a parked vendor still serves. Priority is still the strict tier it always was,
// so parking the only vendor in the winning tier changes nothing; disabling it,
// or giving another vendor that tier, is what takes it out.
func TestZeroWeightStillServesWithNoWeightedRival(t *testing.T) {
	t.Run("alone", func(t *testing.T) {
		snap := buildSnapshot(t, `
vendors:
  - name: parked
    origin: https://parked.example
    served_models: [m]
    weight: 0
    credential: {id: p1, api_key: k}
`)
		r := New(staticSnap(snap), Options{Logger: quietLogger()})
		if got := leadName(t, r); got != "parked" {
			t.Fatalf("lead = %q, want parked: a parked vendor with no rival still serves", got)
		}
	})

	t.Run("better tier", func(t *testing.T) {
		snap := buildSnapshot(t, `
vendors:
  - name: parked
    origin: https://parked.example
    served_models: [m]
    priority: 1
    weight: 0
    credential: {id: p1, api_key: k}
  - name: backup
    origin: https://backup.example
    served_models: [m]
    priority: 2
    weight: 5
    credential: {id: b1, api_key: k}
`)
		r := New(staticSnap(snap), Options{Logger: quietLogger()})
		if got := leadName(t, r); got != "parked" {
			t.Fatalf("lead = %q, want parked: priority is a strict tier above weight", got)
		}
	})
}

// A weight change never recalculates pins. Stickiness sorts above the draw, so a
// session already on a vendor keeps it — and its warm prompt cache — after the
// vendor is parked, while new sessions go to the weighted one. Parking stops NEW
// sessions; an operator who needs traffic to stop now disables the provider.
func TestParkingKeepsExistingSessionPins(t *testing.T) {
	snap := buildSnapshot(t, parkedAndWeightedYAML)
	r := New(staticSnap(snap), Options{Logger: quietLogger()})

	pinned := sessionSel("session-1")
	r.Pin(pinned, "parked")
	for i := 0; i < 50; i++ {
		if got := leadFor(t, r, pinned); got != "parked" {
			t.Fatalf("lead = %q on call %d, want the pinned parked vendor", got, i)
		}
	}
	if got := leadFor(t, r, sessionSel("session-2")); got != "live" {
		t.Fatalf("new session lead = %q, want live: parking stops new sessions", got)
	}
}

// An injected Rand makes the draw reproducible, which is what lets the sticky
// and health tests assert an exact leader within one priority tier.
func TestInjectedRandMakesSelectionDeterministic(t *testing.T) {
	snap := buildSnapshot(t, twoVendorSamePriorityYAML)
	// Always draw 0 -> every vendor gets the same draw value, so the sort falls
	// through to declaration order.
	r := New(staticSnap(snap), Options{
		Rand:   func() float64 { return 0 },
		Logger: quietLogger(),
	})
	for i := 0; i < 20; i++ {
		if got := leadName(t, r); got != "alpha" {
			t.Fatalf("lead = %q on call %d, want alpha every time", got, i)
		}
	}
}

func TestCandidatesForProvider(t *testing.T) {
	// Two vendors derived from the same provider (the (origin, adapter) split):
	// both carry the provider id c1 as their credential id, so a pin by c1
	// resolves to both regardless of model.
	snap := buildSnapshot(t, `
vendors:
  - name: bailian
    origin: https://dashscope.aliyuncs.com/compatible-mode/v1
    served_models: [qwen-plus]
    credential: {id: c1, api_key: k1}
  - name: bailian-anthropic
    origin: https://dashscope.aliyuncs.com
    served_models: [qwen-plus]
    credential: {id: c1, api_key: k1}
`)
	r := New(staticSnap(snap))
	got, err := r.CandidatesForProvider("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	for _, tg := range got {
		if tg.Credential.ID != "c1" {
			t.Fatalf("target %q has credential %q, want c1", tg.Vendor.Name, tg.Credential.ID)
		}
	}
}

func TestProviderPinHonorsDisabledModelRoute(t *testing.T) {
	snap, err := config.Build(config.Config{Vendors: []config.Vendor{
		{
			Name:         "pool",
			Origin:       "https://pool.example",
			ServedModels: []string{"m"},
			ModelRoutes: map[string]config.ModelRoute{
				"m": {Enabled: false, Priority: 0, Weight: 1},
			},
			Credential: config.Credential{ID: "p1", APIKey: "k"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := New(staticSnap(snap))

	if _, err := r.Select(Selector{
		Scope: ScopeProvider, ProviderID: "p1", Model: "m",
	}); !errors.Is(err, ErrNoVendor) {
		t.Fatalf("disabled model pin error = %v, want ErrNoVendor", err)
	}
	// Explicit pins to undeclared models retain their historical behavior.
	if got, err := r.Select(Selector{
		Scope: ScopeProvider, ProviderID: "p1", Model: "other",
	}); err != nil || len(got) != 1 {
		t.Fatalf("undeclared model pin = %+v, %v", got, err)
	}
}

func TestCandidatesForProviderMissing(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: bailian
    origin: https://dashscope.aliyuncs.com/compatible-mode/v1
    served_models: [qwen-plus]
    credential: {id: c1, api_key: k1}
`)
	r := New(staticSnap(snap))
	if _, err := r.CandidatesForProvider("nope"); !errors.Is(err, ErrNoVendor) {
		t.Fatalf("want ErrNoVendor for missing provider, got %v", err)
	}
}

func TestCandidatesForProviderNilSnapshot(t *testing.T) {
	r := New(func() *config.Snapshot { return nil })
	if _, err := r.CandidatesForProvider("x"); !errors.Is(err, ErrNoVendor) {
		t.Fatalf("want ErrNoVendor on nil snapshot, got %v", err)
	}
}

func TestAllCandidates(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: a
    origin: https://a.example
    served_models: [m]
    priority: 1
    credential: {id: a1, api_key: k}
  - name: b
    origin: https://b.example
    served_models: [n]
    priority: 0
    credential: {id: b1, api_key: k}
`)
	r := New(staticSnap(snap))
	got, err := r.AllCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2", len(got))
	}
	// Priority ascending: b (0) before a (1).
	if got[0].Vendor.Name != "b" || got[1].Vendor.Name != "a" {
		t.Fatalf("order = %s,%s, want b,a (priority order)", got[0].Vendor.Name, got[1].Vendor.Name)
	}
}

func TestAllCandidatesNilSnapshot(t *testing.T) {
	r := New(func() *config.Snapshot { return nil })
	if _, err := r.AllCandidates(); !errors.Is(err, ErrNoVendor) {
		t.Fatalf("want ErrNoVendor on nil snapshot, got %v", err)
	}
}

func TestConcurrencySmoke(t *testing.T) {
	snap := buildSnapshot(t, `
vendors:
  - name: a
    origin: https://a.example
    served_models: [m]
    priority: 1
    weight: 2
    credential: {id: a1, api_key: k}
  - name: b
    origin: https://b.example
    served_models: [m]
    priority: 1
    weight: 1
    credential: {id: b1, api_key: k}
`)
	r := New(staticSnap(snap))

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				got, err := r.Candidates("m")
				if err != nil {
					t.Errorf("Candidates: %v", err)
					return
				}
				if len(got) == 0 {
					t.Errorf("no candidates")
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

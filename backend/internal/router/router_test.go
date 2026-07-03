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

func TestWeightedRoundRobinDistribution(t *testing.T) {
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
	// Expect roughly 3:1. Allow generous tolerance.
	ratio := float64(lead["heavy"]) / float64(lead["light"])
	if ratio < 2.4 || ratio > 3.6 {
		t.Fatalf("weighted ratio heavy/light = %.2f (heavy=%d light=%d), want ~3", ratio, lead["heavy"], lead["light"])
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

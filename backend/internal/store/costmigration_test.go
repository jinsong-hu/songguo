package store

import (
	"testing"

	"github.com/songguo/songguo/internal/catalog"
)

// TestBackfillModelCosts covers the one migration in this change that touches
// live operator data: provider_models rows written before the cost column
// existed carry (input, output, cached_input, unit), and their rates must
// survive verbatim onto the new axes.
//
// The per_1k/per_token cases matter most. Those scales existed precisely so an
// operator could type a rate in the basis their vendor published, and folding
// them onto the per-1M token axes is the only step here that changes a number
// rather than moving it.
func TestBackfillModelCosts(t *testing.T) {
	s := openTestStore(t)

	pvd, err := s.CreateProvider(NewProvider{
		Name: "legacy", Enabled: true, APIKey: "sk-a",
		Models: []ProviderModel{
			{Model: "m-1m"}, {Model: "m-1k"}, {Model: "m-tok"},
			{Model: "m-char"}, {Model: "m-sec"}, {Model: "m-img"}, {Model: "m-call"},
			{Model: "m-bogus"},
		},
		Endpoints: []ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://x.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Rewind those rows to the pre-cost shape.
	legacy := []struct {
		model             string
		in, out, cachedIn float64
		unit              string
	}{
		{"m-1m", 2.5, 10, 0.25, "per_1m_tokens"},
		{"m-1k", 0.0025, 0.01, 0.00025, "per_1k_tokens"},
		{"m-tok", 0.0000025, 0.00001, 0.00000025, "per_token"},
		{"m-char", 4.17e-05, 0, 0, "per_char"},
		{"m-sec", 3.27e-05, 0, 0, "per_second"},
		{"m-img", 0.04, 0, 0, "per_image"},
		{"m-call", 1.5, 0, 0, "per_call"},
		{"m-bogus", 7, 8, 0, "per_banana"},
	}
	for _, l := range legacy {
		if _, err := s.db.Exec(
			`UPDATE provider_models SET cost = '', input = ?, output = ?, cached_input = ?, unit = ?
			 WHERE provider_id = ? AND model = ?`,
			l.in, l.out, l.cachedIn, l.unit, pvd.ID, l.model); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.backfillModelCosts(); err != nil {
		t.Fatalf("backfillModelCosts: %v", err)
	}

	got, err := s.GetProvider(pvd.ID)
	if err != nil {
		t.Fatal(err)
	}
	costs := make(map[string]catalog.Cost, len(got.Models))
	for _, m := range got.Models {
		costs[m.Model] = m.Cost
	}

	// The three token bases all land on the same per-1M axes: an operator who
	// typed 0.0025 per 1k and one who typed 2.5 per 1M were always billing the
	// same rate, and must still agree afterwards.
	tokens := catalog.Cost{Input: 2.5, Output: 10, CacheRead: 0.25}
	for _, m := range []string{"m-1m", "m-1k", "m-tok"} {
		if !approxCost(costs[m], tokens) {
			t.Errorf("%s = %+v, want %+v", m, costs[m], tokens)
		}
	}

	want := map[string]catalog.Cost{
		"m-char": {Character: 4.17e-05},
		"m-sec":  {Second: 3.27e-05},
		"m-img":  {Image: 0.04},
		"m-call": {Call: 1.5},
		// A unit the old cost engine could not price metered $0; it must stay
		// unpriced rather than acquire a rate it never had.
		"m-bogus": {},
	}
	for model, w := range want {
		if !approxCost(costs[model], w) {
			t.Errorf("%s = %+v, want %+v", model, costs[model], w)
		}
	}
}

// TestBackfillModelCostsIsIdempotent: the migration runs on every Open, so a
// second pass must not touch a row it already converted — in particular it must
// not re-read the stale legacy columns over a cost an operator has since edited.
func TestBackfillModelCostsIsIdempotent(t *testing.T) {
	s := openTestStore(t)

	pvd, err := s.CreateProvider(NewProvider{
		Name: "legacy", Enabled: true, APIKey: "sk-a",
		Models:    []ProviderModel{{Model: "m1"}},
		Endpoints: []ProviderEndpoint{{Wire: "openai/chat", Endpoint: "https://x.example.com/v1/chat/completions", Adapter: "openai-compatible"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`UPDATE provider_models SET cost = '', input = 1, output = 2, unit = 'per_1m_tokens'
		 WHERE provider_id = ?`, pvd.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillModelCosts(); err != nil {
		t.Fatal(err)
	}

	// An operator edits the rate; the legacy columns still say 1/2.
	if _, err := s.UpdateProvider(pvd.ID, ProviderUpdate{
		Models: []ProviderModel{{Model: "m1", Cost: catalog.Cost{Input: 99, Output: 98}, PriceOverride: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillModelCosts(); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetProvider(pvd.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Models[0].Cost; c.Input != 99 || c.Output != 98 {
		t.Errorf("cost = %+v, want the operator's 99/98 (the backfill re-ran over an edited row)", c)
	}
}

func approxCost(a, b catalog.Cost) bool {
	const eps = 1e-9
	d := func(x, y float64) bool {
		if x > y {
			return x-y < eps
		}
		return y-x < eps
	}
	return d(a.Input, b.Input) && d(a.Output, b.Output) && d(a.CacheRead, b.CacheRead) &&
		d(a.CacheWrite, b.CacheWrite) && d(a.Character, b.Character) && d(a.Second, b.Second) &&
		d(a.Image, b.Image) && d(a.Call, b.Call)
}

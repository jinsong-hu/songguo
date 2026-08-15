package pricing

import (
	"math"
	"testing"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/wire"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestCost(t *testing.T) {
	tests := []struct {
		name string
		cost catalog.Cost
		norm wire.Normalized
		want float64
	}{
		{
			name: "token rates are per 1M",
			cost: catalog.Cost{Input: 3, Output: 15},
			norm: wire.Normalized{InputTokens: 1_000_000, OutputTokens: 2_000_000},
			want: 3*1 + 15*2,
		},
		{
			// Disjoint: 400k fresh input + 600k cache reads.
			name: "cached input billed at cache_read",
			cost: catalog.Cost{Input: 0.28, Output: 0.42, CacheRead: 0.028},
			norm: wire.Normalized{InputTokens: 400_000, CachedInputTokens: 600_000},
			want: 0.4*0.28 + 0.6*0.028,
		},
		{
			// 600k fresh + 400k cache reads, no cached rate → both at input rate.
			// "No cached rate" means "no discount", never "free".
			name: "cache reads without a cache_read rate fall back to input",
			cost: catalog.Cost{Input: 3, Output: 15},
			norm: wire.Normalized{InputTokens: 600_000, CachedInputTokens: 400_000},
			want: 3.0,
		},
		{
			// Cache writes bill at their own published rate now that models.dev
			// supplies one: 1M fresh at 2 + 500k writes at 2.5 + 400k reads at 1.
			name: "cache creation billed at cache_write",
			cost: catalog.Cost{Input: 2, CacheRead: 1, CacheWrite: 2.5},
			norm: wire.Normalized{InputTokens: 1_000_000, CacheCreationTokens: 500_000, CachedInputTokens: 400_000},
			want: 1*2 + 0.5*2.5 + 0.4*1,
		},
		{
			name: "cache writes without a cache_write rate fall back to input",
			cost: catalog.Cost{Input: 2},
			norm: wire.Normalized{CacheCreationTokens: 500_000},
			want: 0.5 * 2,
		},
		{
			// Thinking tokens are a subset of output and never priced separately.
			name: "thinking tokens do not change cost",
			cost: catalog.Cost{Input: 3, Output: 15},
			norm: wire.Normalized{InputTokens: 1_000_000, OutputTokens: 2_000_000, ThinkingTokens: 500_000},
			want: 3*1 + 15*2,
		},
		{
			name: "call axis defaults to one call",
			cost: catalog.Cost{Call: 0.01},
			norm: wire.Normalized{},
			want: 0.01,
		},
		{
			name: "call axis with an explicit count",
			cost: catalog.Cost{Call: 0.01},
			norm: wire.Normalized{Calls: 3},
			want: 0.03,
		},
		{
			name: "image axis",
			cost: catalog.Cost{Image: 0.04},
			norm: wire.Normalized{Images: 2},
			want: 0.08,
		},
		{
			name: "second axis",
			cost: catalog.Cost{Second: 0.0001},
			norm: wire.Normalized{Seconds: 90},
			want: 0.009,
		},
		{
			name: "character axis",
			cost: catalog.Cost{Character: 0.00002},
			norm: wire.Normalized{Chars: 500},
			want: 0.01,
		},
		{
			name: "a cost declaring nothing meters zero",
			cost: catalog.Cost{},
			norm: wire.Normalized{InputTokens: 1_000_000},
			want: 0,
		},
		{
			name: "zero usage zero cost",
			cost: catalog.Cost{Input: 3, Output: 15},
			norm: wire.Normalized{},
			want: 0,
		},
		// An axis a model does not declare contributes nothing, and usage on a
		// quantity it does not price contributes nothing. That is what makes
		// configsvc.fallbackPrice safe without matching the metered quantity: a
		// borrowed token rate against a speech call is inert, not wrong.
		{
			name: "token rate against speech usage is zero",
			cost: catalog.Cost{Input: 10, Output: 50},
			norm: wire.Normalized{Seconds: 120, Chars: 4_000},
			want: 0,
		},
		{
			name: "speech rate against token usage is zero",
			cost: catalog.Cost{Second: 0.01},
			norm: wire.Normalized{InputTokens: 1_000_000, OutputTokens: 500_000},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cost(tt.cost, tt.norm)
			if !approx(got, tt.want) {
				t.Errorf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCostAxesAreAdditive is the capability the single-`unit` price could not
// express: a model billed on two quantities at once. Under the old shape one of
// these axes was unstateable and silently metered $0.
func TestCostAxesAreAdditive(t *testing.T) {
	c := catalog.Cost{Input: 3, Output: 15, Second: 0.001, Character: 0.00002}
	n := wire.Normalized{
		InputTokens: 1_000_000, OutputTokens: 2_000_000,
		Seconds: 60, Chars: 1_000,
	}
	want := 3*1 + 15*2 + 0.001*60 + 0.00002*1000
	if got := Cost(c, n); !approx(got, want) {
		t.Errorf("Cost() = %v, want %v (every declared axis contributes)", got, want)
	}
}

// TestCostDeepSeekRealWorld prices a realistic DeepSeek call: cache hits at
// ~1/10 of the miss rate must dominate the bill when most input is cached.
func TestCostDeepSeekRealWorld(t *testing.T) {
	cost := catalog.Cost{Input: 0.14, Output: 0.28, CacheRead: 0.0028}
	// Disjoint: 10k fresh input + 90k cache reads.
	norm := wire.Normalized{InputTokens: 10_000, CachedInputTokens: 90_000, OutputTokens: 5_000}
	got := Cost(cost, norm)
	want := (10_000*0.14 + 90_000*0.0028 + 5_000*0.28) / 1e6
	if !approx(got, want) {
		t.Errorf("Cost() = %v, want %v", got, want)
	}
	// Sanity: ignoring the cache discount would overcharge ~8x on input.
	full := Cost(catalog.Cost{Input: 0.14, Output: 0.28}, norm)
	if full <= got {
		t.Errorf("expected discount: full %v should exceed discounted %v", full, got)
	}
}

// TestCostBillingInvariance proves the disjoint token model bills exactly what
// the pre-change folded model did. The old model stored InputTokens as the
// folded total (fresh + cache_read + cache_create) and priced
// (InputTokens-cached)*Input + cached*cachedRate + Output*Output. The new model
// stores the three input parts disjointly; this asserts the two agree for a
// range of representative vendor shapes, so redefining input_tokens to
// fresh-only changed no invoice.
//
// It also pins the boundary of the cache_write change: with no cache_write
// published, writes still bill at the input rate exactly as they always did, so
// the new axis alters nothing for a cost that does not declare it.
func TestCostBillingInvariance(t *testing.T) {
	// oldFolded replicates the pre-change tokenCost from the folded total.
	oldFolded := func(c catalog.Cost, foldedInput, cached, output float64) float64 {
		read := cached
		if read > foldedInput {
			read = foldedInput
		}
		rate := c.CacheRead
		if rate <= 0 {
			rate = c.Input
		}
		return ((foldedInput-read)*c.Input + read*rate + output*c.Output) / 1e6
	}

	cost := catalog.Cost{Input: 0.28, Output: 0.42, CacheRead: 0.028}
	cases := []struct{ fresh, cacheRead, cacheCreate, output float64 }{
		{1000, 0, 0, 500},              // no cache
		{10, 74263, 969, 285},          // the Anthropic real-world example
		{400_000, 600_000, 0, 100_000}, // OpenAI-style: fresh + reads, no writes
		{0, 0, 5000, 0},                // pure cache creation
	}
	for _, c := range cases {
		disjoint := wire.Normalized{
			InputTokens:         c.fresh,
			CachedInputTokens:   c.cacheRead,
			CacheCreationTokens: c.cacheCreate,
			OutputTokens:        c.output,
		}
		got := Cost(cost, disjoint)
		want := oldFolded(cost, c.fresh+c.cacheRead+c.cacheCreate, c.cacheRead, c.output)
		if !approx(got, want) {
			t.Errorf("shape %+v: Cost() = %v, folded model = %v", c, got, want)
		}
	}
}

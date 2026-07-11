package pricing

import (
	"math"
	"testing"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/wire"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestCost(t *testing.T) {
	tests := []struct {
		name  string
		price config.Price
		norm  wire.Normalized
		want  float64
	}{
		{
			name:  "per_1m_tokens",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			norm:  wire.Normalized{InputTokens: 1_000_000, OutputTokens: 2_000_000},
			want:  3*1 + 15*2,
		},
		{
			name:  "per_1k_tokens",
			price: config.Price{Input: 0.5, Output: 1.5, Unit: "per_1k_tokens"},
			norm:  wire.Normalized{InputTokens: 1000, OutputTokens: 2000},
			want:  0.5*1 + 1.5*2,
		},
		{
			name:  "per_token",
			price: config.Price{Input: 0.001, Output: 0.002, Unit: "per_token"},
			norm:  wire.Normalized{InputTokens: 10, OutputTokens: 5},
			want:  0.001*10 + 0.002*5,
		},
		{
			// Disjoint: 400k fresh input + 600k cache reads.
			name:  "cached input billed at cached rate",
			price: config.Price{Input: 0.28, Output: 0.42, CachedInput: 0.028, Unit: "per_1m_tokens"},
			norm:  wire.Normalized{InputTokens: 400_000, CachedInputTokens: 600_000, OutputTokens: 0},
			want:  0.4*0.28 + 0.6*0.028,
		},
		{
			// 600k fresh + 400k cache reads, no cached rate → both at input rate.
			name:  "cached tokens without cached rate fall back to input rate",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			norm:  wire.Normalized{InputTokens: 600_000, CachedInputTokens: 400_000},
			want:  3.0,
		},
		{
			// Cache creation bills at the full input rate; cache reads at the cached
			// rate: (1M + 500k)*2 + 400k*1, per 1M.
			name:  "cache creation at input rate, reads at cached rate",
			price: config.Price{Input: 2, Output: 0, CachedInput: 1, Unit: "per_1m_tokens"},
			norm:  wire.Normalized{InputTokens: 1_000_000, CacheCreationTokens: 500_000, CachedInputTokens: 400_000},
			want:  1.5*2 + 0.4*1,
		},
		{
			// Thinking tokens are a subset of output and never priced separately.
			name:  "thinking tokens do not change cost",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			norm:  wire.Normalized{InputTokens: 1_000_000, OutputTokens: 2_000_000, ThinkingTokens: 500_000},
			want:  3*1 + 15*2,
		},
		{
			name:  "per_call defaults to one call",
			price: config.Price{Input: 0.01, Unit: "per_call"},
			norm:  wire.Normalized{},
			want:  0.01,
		},
		{
			name:  "per_call explicit count",
			price: config.Price{Input: 0.01, Unit: "per_call"},
			norm:  wire.Normalized{Calls: 3},
			want:  0.03,
		},
		{
			name:  "per_image",
			price: config.Price{Input: 0.04, Unit: "per_image"},
			norm:  wire.Normalized{Images: 2},
			want:  0.08,
		},
		{
			name:  "per_second",
			price: config.Price{Input: 0.0001, Unit: "per_second"},
			norm:  wire.Normalized{Seconds: 90},
			want:  0.009,
		},
		{
			name:  "per_char",
			price: config.Price{Input: 0.00002, Unit: "per_char"},
			norm:  wire.Normalized{Chars: 500},
			want:  0.01,
		},
		{
			name:  "unknown unit yields zero",
			price: config.Price{Input: 3, Output: 15, Unit: "per_banana"},
			norm:  wire.Normalized{InputTokens: 1_000_000},
			want:  0,
		},
		{
			name:  "empty unit yields zero",
			price: config.Price{Input: 3, Output: 15},
			norm:  wire.Normalized{InputTokens: 1_000_000},
			want:  0,
		},
		{
			name:  "zero usage zero cost",
			price: config.Price{Input: 3, Output: 15, Unit: "per_1m_tokens"},
			norm:  wire.Normalized{},
			want:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Cost(tt.price, tt.norm)
			if !approx(got, tt.want) {
				t.Errorf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCostDeepSeekRealWorld prices a realistic DeepSeek call: cache hits at
// ~1/10 of the miss rate must dominate the bill when most input is cached.
func TestCostDeepSeekRealWorld(t *testing.T) {
	price := config.Price{Input: 0.14, Output: 0.28, CachedInput: 0.0028, Unit: "per_1m_tokens"}
	// Disjoint: 10k fresh input + 90k cache reads.
	norm := wire.Normalized{InputTokens: 10_000, CachedInputTokens: 90_000, OutputTokens: 5_000}
	got := Cost(price, norm)
	want := (10_000*0.14 + 90_000*0.0028 + 5_000*0.28) / 1e6
	if !approx(got, want) {
		t.Errorf("Cost() = %v, want %v", got, want)
	}
	// Sanity: ignoring the cache discount would overcharge ~8x on input.
	full := Cost(config.Price{Input: 0.14, Output: 0.28, Unit: "per_1m_tokens"}, norm)
	if full <= got {
		t.Errorf("expected discount: full %v should exceed discounted %v", full, got)
	}
}

// TestCostBillingInvariance proves the disjoint token model bills exactly what the
// pre-change folded model did. The old model stored InputTokens as the folded
// total (fresh + cache_read + cache_create) and priced
// (InputTokens-cached)*Input + cached*cachedRate + Output*Output. The new model
// stores the three input parts disjointly; this asserts the two agree for a range
// of representative vendor shapes, so redefining input_tokens to fresh-only changed
// no invoice.
func TestCostBillingInvariance(t *testing.T) {
	// oldFolded replicates the pre-change tokenCost from the folded total.
	oldFolded := func(p config.Price, foldedInput, cached, output float64) float64 {
		c := cached
		if c > foldedInput {
			c = foldedInput
		}
		rate := p.CachedInput
		if rate <= 0 {
			rate = p.Input
		}
		return ((foldedInput-c)*p.Input + c*rate + output*p.Output) / 1e6
	}

	price := config.Price{Input: 0.28, Output: 0.42, CachedInput: 0.028, Unit: "per_1m_tokens"}
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
		got := Cost(price, disjoint)
		want := oldFolded(price, c.fresh+c.cacheRead+c.cacheCreate, c.cacheRead, c.output)
		if !approx(got, want) {
			t.Errorf("invariance broken for %+v: new=%v old=%v", c, got, want)
		}
	}
}

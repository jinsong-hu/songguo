package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/wire"
)

func approx(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// anyTime is the instant for costs that declare no schedule, which is all of
// them below except the peak tests. Cost still requires one — a clock the caller
// cannot forget to pass is the point of making it a parameter — but for a cost
// with no time-dependent rate the value cannot change the answer.
var anyTime = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// deepseekFlash is DeepSeek's published Flash rate, kept in one place so every
// test below reads against the same rule: base is off-peak, and 高峰时段 —
// Beijing Mon-Fri 09:00-12:00 and 14:00-18:00 — is double on every axis.
// See https://api-docs.deepseek.com/quick_start/pricing/
func deepseekFlash() catalog.Cost {
	peak := []catalog.Window{
		{Type: catalog.WindowWeekly, Offset: "+08:00", Days: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "09:00", End: "12:00"},
		{Type: catalog.WindowWeekly, Offset: "+08:00", Days: []string{"mon", "tue", "wed", "thu", "fri"}, Start: "14:00", End: "18:00"},
	}
	return catalog.Cost{
		Input: 0.15, Output: 0.6, CacheRead: 0.003,
		Schedules: []catalog.CostSchedule{
			{Input: 0.3, Output: 1.2, CacheRead: 0.006, When: peak},
		},
	}
}

// beijing builds an instant from Beijing wall-clock, because that is how DeepSeek
// states the rule. Writing these as UTC would mean shifting every expectation by
// eight hours in your head to check it against the vendor's page.
//
// September 2026: the 7th is a Monday, so 7-11 are Mon-Fri and 12-13 the weekend.
func beijing(day, hour, minute, sec int) time.Time {
	return time.Date(2026, 9, day, hour, minute, sec, 0, time.FixedZone("CST", 8*3600))
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
			got := Cost(tt.cost, tt.norm, anyTime)
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
	if got := Cost(c, n, anyTime); !approx(got, want) {
		t.Errorf("Cost() = %v, want %v (every declared axis contributes)", got, want)
	}
}

// TestCostDeepSeekRealWorld prices a realistic DeepSeek call: cache hits at
// ~1/10 of the miss rate must dominate the bill when most input is cached.
func TestCostDeepSeekRealWorld(t *testing.T) {
	cost := catalog.Cost{Input: 0.14, Output: 0.28, CacheRead: 0.0028}
	// Disjoint: 10k fresh input + 90k cache reads.
	norm := wire.Normalized{InputTokens: 10_000, CachedInputTokens: 90_000, OutputTokens: 5_000}
	got := Cost(cost, norm, anyTime)
	want := (10_000*0.14 + 90_000*0.0028 + 5_000*0.28) / 1e6
	if !approx(got, want) {
		t.Errorf("Cost() = %v, want %v", got, want)
	}
	// Sanity: ignoring the cache discount would overcharge ~8x on input.
	full := Cost(catalog.Cost{Input: 0.14, Output: 0.28}, norm, anyTime)
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
		got := Cost(cost, disjoint, anyTime)
		want := oldFolded(cost, c.fresh+c.cacheRead+c.cacheCreate, c.cacheRead, c.output)
		if !approx(got, want) {
			t.Errorf("shape %+v: Cost() = %v, folded model = %v", c, got, want)
		}
	}
}

// TestCostContextTiers covers the bracket logic. A vendor prices the WHOLE
// request at the bracket its prompt falls into, so crossing a threshold is a
// cliff, not a marginal rate on the excess.
func TestCostContextTiers(t *testing.T) {
	// gpt-5.6-luna's real shape: 0.2/1.2 base, doubling above 272k.
	luna := catalog.Cost{
		Input: 0.2, Output: 1.2, CacheRead: 0.02,
		Tiers: []catalog.CostTier{{
			Input: 0.4, Output: 1.8, CacheRead: 0.04,
			Tier: catalog.TierBound{Type: catalog.TierContext, Size: 272_000},
		}},
	}

	tests := []struct {
		name string
		norm wire.Normalized
		want float64
	}{
		{
			name: "below the threshold bills the base rate",
			norm: wire.Normalized{InputTokens: 100_000, OutputTokens: 1_000},
			want: 0.1*0.2 + 0.001*1.2,
		},
		{
			// Exactly at the size is NOT over it: the bracket is "context over N".
			name: "exactly at the threshold is still the base rate",
			norm: wire.Normalized{InputTokens: 272_000, OutputTokens: 1_000},
			want: 0.272*0.2 + 0.001*1.2,
		},
		{
			// The whole prompt reprices, not just the 1 token past the line.
			name: "one token over reprices the entire request",
			norm: wire.Normalized{InputTokens: 272_001, OutputTokens: 1_000},
			want: 0.272001*0.4 + 0.001*1.8,
		},
		{
			// The three input fields are disjoint and together are the prompt, so
			// a cache-heavy request crosses on their sum — not on fresh input
			// alone, which is the reading that would under-bill agent traffic.
			name: "cached and cache-write tokens count toward the threshold",
			norm: wire.Normalized{InputTokens: 10_000, CachedInputTokens: 250_000, CacheCreationTokens: 20_000, OutputTokens: 1_000},
			want: 0.01*0.4 + 0.25*0.04 + 0.02*0.4 + 0.001*1.8,
		},
		{
			// Output is not part of the prompt and cannot push a request over.
			name: "output tokens do not cross the threshold",
			norm: wire.Normalized{InputTokens: 1_000, OutputTokens: 500_000},
			want: 0.001*0.2 + 0.5*1.2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cost(luna, tt.norm, anyTime); !approx(got, tt.want) {
				t.Errorf("Cost() = %v, want %v", got, tt.want)
			}
		})
	}
}

// With two brackets only the highest crossed one applies; they do not compound.
func TestCostPicksHighestCrossedTier(t *testing.T) {
	c := catalog.Cost{
		Input: 0.04, Output: 0.13,
		Tiers: []catalog.CostTier{
			{Input: 0.1, Output: 0.37, Tier: catalog.TierBound{Type: catalog.TierContext, Size: 32_000}},
			{Input: 0.19, Output: 0.74, Tier: catalog.TierBound{Type: catalog.TierContext, Size: 256_000}},
		},
	}
	cases := []struct {
		prompt float64
		in     float64
	}{
		{10_000, 0.04},  // below both
		{100_000, 0.1},  // above the first only
		{300_000, 0.19}, // above both — the higher wins, not the sum
	}
	for _, tc := range cases {
		n := wire.Normalized{InputTokens: tc.prompt}
		want := tc.prompt / 1e6 * tc.in
		if got := Cost(c, n, anyTime); !approx(got, want) {
			t.Errorf("prompt %v: Cost() = %v, want %v (rate %v)", tc.prompt, got, want, tc.in)
		}
	}
}

// A tier states only what changes; an axis it omits keeps the base rate rather
// than falling to zero (which would then re-derive from input and over-bill).
func TestCostTierKeepsUnstatedAxesAtBase(t *testing.T) {
	c := catalog.Cost{
		Input: 1, Output: 2, CacheRead: 0.1,
		Tiers: []catalog.CostTier{{
			Input: 2, Output: 4, // no cache_read
			Tier: catalog.TierBound{Type: catalog.TierContext, Size: 100},
		}},
	}
	n := wire.Normalized{InputTokens: 1_000_000, CachedInputTokens: 1_000_000}
	want := 1*2 + 1*0.1 // input at the tier rate, cache reads at the base rate
	if got := Cost(c, n, anyTime); !approx(got, want) {
		t.Errorf("Cost() = %v, want %v (an unstated tier axis must keep its base rate)", got, want)
	}
}

// An unrecognized tier type is ignored rather than guessed at, so a new kind of
// bracket bills at the base rate until it is implemented.
func TestCostIgnoresUnknownTierType(t *testing.T) {
	c := catalog.Cost{
		Input: 1, Output: 2,
		Tiers: []catalog.CostTier{{
			Input: 99, Output: 99,
			Tier: catalog.TierBound{Type: "phase-of-the-moon", Size: 10},
		}},
	}
	n := wire.Normalized{InputTokens: 1_000_000}
	if got := Cost(c, n, anyTime); !approx(got, 1) {
		t.Errorf("Cost() = %v, want the base 1 (an unknown tier type must not apply)", got)
	}
}

// Media axes are never tiered upstream, and a context bracket must not disturb
// them: a speech call reports no tokens, so it can never cross a threshold.
func TestCostTiersDoNotDisturbMediaAxes(t *testing.T) {
	c := catalog.Cost{
		Second: 0.001,
		Tiers: []catalog.CostTier{{
			Input: 99, Tier: catalog.TierBound{Type: catalog.TierContext, Size: 1},
		}},
	}
	n := wire.Normalized{Seconds: 60}
	if got := Cost(c, n, anyTime); !approx(got, 0.06) {
		t.Errorf("Cost() = %v, want 0.06", got)
	}
}

// TestCostPeakWindows is the whole feature in one table: the same call, priced
// against DeepSeek's published clock. Before schedules existed a single rate had
// to stand in for both halves of the week, so whichever number was in the
// catalogue was wrong by 2x for the other half.
func TestCostPeakWindows(t *testing.T) {
	c := deepseekFlash()
	// 1M fresh input + 1M output. Off-peak: 0.15 + 0.6 = 0.75. Peak: double.
	n := wire.Normalized{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	tests := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"monday mid-morning is peak", beijing(7, 10, 30, 0), 1.5},
		{"monday mid-afternoon is peak", beijing(7, 15, 0, 0), 1.5},
		{"the lunch gap between the two windows is off-peak", beijing(7, 13, 0, 0), 0.75},
		{"before the working day is off-peak", beijing(7, 8, 59, 0), 0.75},
		{"after the working day is off-peak", beijing(7, 18, 0, 0), 0.75},
		{"the small hours are off-peak", beijing(9, 3, 0, 0), 0.75},
		{"friday is still a weekday", beijing(11, 9, 0, 0), 1.5},
		{"saturday business hours are off-peak", beijing(12, 10, 30, 0), 0.75},
		{"sunday business hours are off-peak", beijing(13, 15, 0, 0), 0.75},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cost(c, n, tt.at); !approx(got, tt.want) {
				t.Errorf("Cost() at %s = %v, want %v", tt.at.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// A window is half-open, so the two DeepSeek publishes can be written as the
// vendor writes them (09:00-12:00, 14:00-18:00) without 12:00 belonging to both
// or to neither. The seconds matter: 11:59:59 is still inside the morning peak.
func TestCostPeakBoundaryIsHalfOpen(t *testing.T) {
	c := deepseekFlash()
	n := wire.Normalized{InputTokens: 1_000_000}

	tests := []struct {
		name string
		at   time.Time
		want float64
	}{
		{"the opening instant is inside", beijing(7, 9, 0, 0), 0.3},
		{"one second before close is inside", beijing(7, 11, 59, 59), 0.3},
		{"the closing instant is outside", beijing(7, 12, 0, 0), 0.15},
		{"one second before open is outside", beijing(7, 8, 59, 59), 0.15},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cost(c, n, tt.at); !approx(got, tt.want) {
				t.Errorf("Cost() at %s = %v, want %v", tt.at.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// The window carries its own offset, so the answer must not depend on the
// caller's zone — a gateway in UTC and one in Shanghai bill the same request
// identically. This is the property that lets the catalogue be written in the
// vendor's wall-clock instead of translated into UTC by hand.
func TestCostPeakIsZoneIndependent(t *testing.T) {
	c := deepseekFlash()
	n := wire.Normalized{InputTokens: 1_000_000}

	// One instant: Monday 10:30 Beijing == 02:30 UTC == Sunday 21:30 in New York.
	at := beijing(7, 10, 30, 0)
	for _, loc := range []*time.Location{time.UTC, time.FixedZone("CST", 8*3600), time.FixedZone("EDT", -4*3600)} {
		if got := Cost(c, n, at.In(loc)); !approx(got, 0.3) {
			t.Errorf("Cost() rendered in %s = %v, want the peak 0.3", loc, got)
		}
	}
}

// A schedule states only the axes that change, exactly as a tier does; an axis it
// omits keeps the base rate rather than dropping to free.
func TestCostScheduleKeepsUnstatedAxesAtBase(t *testing.T) {
	c := catalog.Cost{
		Input: 1, Output: 4,
		Schedules: []catalog.CostSchedule{{
			Output: 8, // input unstated
			When: []catalog.Window{{
				Type: catalog.WindowWeekly, Offset: "+08:00",
				Days: []string{"mon"}, Start: "09:00", End: "12:00",
			}},
		}},
	}
	n := wire.Normalized{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	if got := Cost(c, n, beijing(7, 10, 0, 0)); !approx(got, 1+8) {
		t.Errorf("Cost() = %v, want 9 — an unstated schedule axis must keep its base rate", got)
	}
}

// An unknown window type must not match. It cannot reach here from the embedded
// catalogue (Window.Validate rejects it at load), so this pins the behaviour of
// the matcher itself: refuse, rather than fall through to some default weekly
// reading of fields it does not understand.
func TestCostIgnoresUnknownWindowType(t *testing.T) {
	c := catalog.Cost{
		Input: 1,
		Schedules: []catalog.CostSchedule{{
			Input: 99,
			When: []catalog.Window{{
				Type: "lunar", Offset: "+08:00",
				Days: []string{"mon"}, Start: "00:00", End: "23:59",
			}},
		}},
	}
	n := wire.Normalized{InputTokens: 1_000_000}
	if got := Cost(c, n, beijing(7, 10, 0, 0)); !approx(got, 1) {
		t.Errorf("Cost() = %v, want the base 1 (an unknown window type must not apply)", got)
	}
}

// Media axes are priced per single unit and no vendor schedules them, so a
// schedule must leave them alone — the same guarantee tiers already carry.
func TestCostSchedulesDoNotDisturbMediaAxes(t *testing.T) {
	c := catalog.Cost{
		Second: 0.001,
		Schedules: []catalog.CostSchedule{{
			Input: 99,
			When: []catalog.Window{{
				Type: catalog.WindowWeekly, Offset: "+08:00",
				Days: []string{"mon"}, Start: "09:00", End: "12:00",
			}},
		}},
	}
	n := wire.Normalized{Seconds: 60}
	if got := Cost(c, n, beijing(7, 10, 0, 0)); !approx(got, 0.06) {
		t.Errorf("Cost() = %v, want 0.06 — a peak window must not touch a per-second rate", got)
	}
}

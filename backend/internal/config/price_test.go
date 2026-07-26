package config

import "testing"

// Units that price the same physical quantity share a family; everything else
// stands alone, because substituting across families computes $0 rather than a
// wrong-but-nonzero number.
func TestPriceUnitFamily(t *testing.T) {
	tests := map[string]string{
		"per_1m_tokens": "tokens",
		"per_1k_tokens": "tokens",
		"per_token":     "tokens",
		"per_call":      "per_call",
		"per_image":     "per_image",
		"per_second":    "per_second",
		"per_char":      "per_char",
		"per_1m_token":  "", // typo: no family, never a fallback candidate
		"":              "",
	}
	for unit, want := range tests {
		if got := PriceUnitFamily(unit); got != want {
			t.Errorf("PriceUnitFamily(%q) = %q, want %q", unit, got, want)
		}
	}
}

// Token rates normalize onto a per-1M basis so the three token units are
// directly comparable; single-rate units score on their only rate.
func TestPriceRank(t *testing.T) {
	tests := []struct {
		name string
		p    Price
		want float64
	}{
		{"per 1m takes the dominant side", Price{Input: 5, Output: 25, Unit: "per_1m_tokens"}, 25},
		{"input can dominate", Price{Input: 30, Output: 2, Unit: "per_1m_tokens"}, 30},
		{"per 1k scales up", Price{Input: 0.001, Output: 0.005, Unit: "per_1k_tokens"}, 5},
		{"per token scales up", Price{Input: 0.000001, Output: 0.000025, Unit: "per_token"}, 25},
		{"single rate scores input", Price{Input: 1, Unit: "per_call"}, 1},
		{"unknown unit cannot win", Price{Input: 999, Output: 999, Unit: "per_1m_token"}, 0},
	}
	for _, tc := range tests {
		if got := PriceRank(tc.p); got != tc.want {
			t.Errorf("%s: PriceRank = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A rate typed against the wrong unit normalizes to an absurd number; it is
// flagged so it can be excluded from fallback candidacy.
func TestPriceRateImplausible(t *testing.T) {
	if PriceRateImplausible(Price{Input: 10, Output: 50, Unit: "per_1m_tokens"}) {
		t.Error("a normal frontier rate must not be flagged implausible")
	}
	if !PriceRateImplausible(Price{Input: 3, Output: 3, Unit: "per_token"}) {
		t.Error("3.0 per_token is $3M/1M and must be flagged")
	}
	// Non-token families have no normalization hazard and are never flagged.
	if PriceRateImplausible(Price{Input: 5000, Unit: "per_call"}) {
		t.Error("single-rate units must not be range-checked as token rates")
	}
}

func TestIsFallbackPrice(t *testing.T) {
	if !IsFallbackPrice(Price{Source: PriceSourceFallbackPrefix + "claude-opus-4-8"}) {
		t.Error("a fallback:<model> source must be reported as a fallback")
	}
	for _, src := range []string{PriceSourceCatalog, PriceSourceOverride, PriceSourceStored, PriceSourceUnpriced, ""} {
		if IsFallbackPrice(Price{Source: src}) {
			t.Errorf("source %q must not be reported as a fallback", src)
		}
	}
}

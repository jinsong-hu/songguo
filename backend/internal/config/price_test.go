package config

import (
	"testing"

	"github.com/songguo/songguo/internal/catalog"
)

// Tokens() is what took over the old unit-family question: it decides which
// costs may lend a fallback rate (configsvc.fallbackPrice) and which are
// range-checked for a mistyped basis. A media-only cost is neither.
func TestCostTokens(t *testing.T) {
	tests := []struct {
		name string
		cost catalog.Cost
		want bool
	}{
		{"input only", catalog.Cost{Input: 1}, true},
		{"output only", catalog.Cost{Output: 1}, true},
		{"cache read only", catalog.Cost{CacheRead: 1}, true},
		{"cache write only", catalog.Cost{CacheWrite: 1}, true},
		{"per second", catalog.Cost{Second: 0.01}, false},
		{"per character", catalog.Cost{Character: 0.01}, false},
		{"per call", catalog.Cost{Call: 1}, false},
		{"empty", catalog.Cost{}, false},
		{"mixed counts as token", catalog.Cost{Input: 1, Second: 0.01}, true},
	}
	for _, tc := range tests {
		if got := tc.cost.Tokens(); got != tc.want {
			t.Errorf("%s: Tokens() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Zero is the "declares no rate at all" test, and it is deliberately NOT the
// same question as "is free": a published all-zero cost is indistinguishable
// from an absent one by value, which is why provenance, not the number, gates
// the fallback (see configsvc.vendorsFromProvider).
func TestCostZero(t *testing.T) {
	if !(catalog.Cost{}).Zero() {
		t.Error("an empty cost must report Zero")
	}
	if (catalog.Cost{Second: 0.01}).Zero() {
		t.Error("a media-only cost declares a rate and must not report Zero")
	}
}

// PriceRank scores the dominant token side, so two costs can be ordered by
// expensiveness; a media-only cost scores on its largest axis.
func TestPriceRank(t *testing.T) {
	tests := []struct {
		name string
		p    Price
		want float64
	}{
		{"takes the dominant side", Price{Cost: catalog.Cost{Input: 5, Output: 25}}, 25},
		{"input can dominate", Price{Cost: catalog.Cost{Input: 30, Output: 2}}, 30},
		{"media cost scores its axis", Price{Cost: catalog.Cost{Call: 1}}, 1},
		{"a cost with no rate cannot win", Price{Cost: catalog.Cost{}}, 0},
	}
	for _, tc := range tests {
		if got := PriceRank(tc.p); got != tc.want {
			t.Errorf("%s: PriceRank = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A rate typed against the wrong basis is an absurd number per 1M; it is flagged
// so it can be excluded from fallback candidacy.
func TestPriceRateImplausible(t *testing.T) {
	if PriceRateImplausible(Price{Cost: catalog.Cost{Input: 10, Output: 50}}) {
		t.Error("a normal frontier rate must not be flagged implausible")
	}
	if !PriceRateImplausible(Price{Cost: catalog.Cost{Input: 3000000, Output: 3000000}}) {
		t.Error("3.0 meant per-token is $3M/1M and must be flagged")
	}
	// Media axes have no second basis to confuse them with, so they are never
	// range-checked: $50 per call is unusual, not absurd.
	if PriceRateImplausible(Price{Cost: catalog.Cost{Call: 5000}}) {
		t.Error("media axes must not be range-checked as token rates")
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

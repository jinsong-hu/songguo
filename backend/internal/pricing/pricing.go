// Package pricing computes true cost from vendor price tables and normalized
// usage.
package pricing

import (
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/wire"
)

// Cost computes the USD cost for a single call from a vendor price entry and
// the canonical usage extracted by the call's wire. It is deliberately
// defensive: an unknown or empty Unit yields zero, and it never panics.
//
// INVARIANT: n must be the vendor's OFFICIAL usage — the counts the wire
// extractor read out of the vendor's response (usage object / SSE usage /
// decoded WS frames). Never pass a locally counted or estimated token total
// here. Billing bills what the vendor reported, with no local reconciliation;
// unknown usage arrives as a zero Normalized and correctly meters $0 rather than
// a guess. Local token counts (internal/compose) are for insights granularity
// and trends, and are kept out of this function on purpose.
//
// Cached input tokens (a subset of InputTokens) are billed at the CachedInput
// rate when one is configured; a non-positive CachedInput means no discount
// and the full Input rate applies.
func Cost(p config.Price, n wire.Normalized) float64 {
	switch p.Unit {
	case "per_1m_tokens":
		return tokenCost(p, n) / 1e6
	case "per_1k_tokens":
		return tokenCost(p, n) / 1e3
	case "per_token":
		return tokenCost(p, n)
	case "per_call":
		calls := n.Calls
		if calls <= 0 {
			calls = 1
		}
		return p.Input * calls
	case "per_image":
		return p.Input * n.Images
	case "per_second":
		return p.Input * n.Seconds
	case "per_char":
		return p.Input * n.Chars
	default:
		return 0
	}
}

// tokenCost prices token usage at the unit-less scale (per single token).
func tokenCost(p config.Price, n wire.Normalized) float64 {
	cached := n.CachedInputTokens
	if cached > n.InputTokens {
		cached = n.InputTokens
	}
	cachedRate := p.CachedInput
	if cachedRate <= 0 {
		cachedRate = p.Input
	}
	return (n.InputTokens-cached)*p.Input + cached*cachedRate + n.OutputTokens*p.Output
}

// Package pricing computes true cost from vendor price tables and normalized
// usage.
package pricing

import (
	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/wire"
)

// Cost computes the USD cost for a single call from a model's cost table and the
// canonical usage extracted by the call's wire. It is deliberately defensive: an
// empty cost yields zero, and it never panics.
//
// INVARIANT: n must be the vendor's OFFICIAL usage — the counts the wire
// extractor read out of the vendor's response (usage object / SSE usage /
// decoded WS frames). Never pass a locally counted or estimated token total
// here. Billing bills what the vendor reported, with no local reconciliation;
// unknown usage arrives as a zero Normalized and correctly meters $0 rather than
// a guess. Local token counts (internal/compose) are for insights granularity
// and trends, and are kept out of this function on purpose.
//
// # The axes are additive
//
// Cost sums every axis the model declares rather than selecting one. The old
// shape dispatched on a single `unit` string, which meant a model billed on two
// quantities — an audio model charging for tokens and for seconds — could only
// state one of them and silently metered $0 for the other. Summing removes that
// whole class of gap: an axis a model does not declare contributes nothing,
// because its rate is zero.
//
// Token axes are per 1M tokens (models.dev's basis); the media axes are per
// single unit (the basis vendors publish). See catalog.Cost.
//
// # Context tiers
//
// Token rates are resolved against the request's prompt size first: vendors
// charge more once a prompt crosses a threshold, and the raised rate applies to
// the whole request rather than only the tokens past it. Taking the base rate
// regardless under-bills long agent contexts by the tier multiple — 2x on the
// gpt-5.6 family above 272k — which is the same class of error as a stale price,
// pointed the other way. See catalog.Cost.At.
//
// The three input-side token fields are disjoint — fresh + cache-read +
// cache-write is the total input — so no clamping is needed. Fresh input bills
// at Input. Cache reads bill at CacheRead, falling back to Input when no
// discount is published, since "no cached rate" means "no discount", not "free".
// Cache writes bill at CacheWrite, falling back to Input the same way.
// ThinkingTokens are a subset of OutputTokens and are not priced separately.
func Cost(c catalog.Cost, n wire.Normalized) float64 {
	// Tier first: a vendor prices the whole request at the bracket its PROMPT
	// falls into, so the rates below may already be the raised ones.
	c = c.At(promptTokens(n))
	total := tokenCost(c, n) / 1e6
	total += c.Character * n.Chars
	total += c.Second * n.Seconds
	total += c.Image * n.Images
	if c.Call != 0 {
		// A per-call wire that reported no explicit count still made one call.
		calls := n.Calls
		if calls <= 0 {
			calls = 1
		}
		total += c.Call * calls
	}
	return total
}

// promptTokens is the request's input size, which is what a context tier is
// measured against. The three input fields are disjoint and sum to the total
// prompt; output is excluded because a vendor brackets on how much you sent,
// not on how much it wrote back.
func promptTokens(n wire.Normalized) float64 {
	return n.InputTokens + n.CachedInputTokens + n.CacheCreationTokens
}

// tokenCost prices token usage at the per-1M scale, before the caller divides.
func tokenCost(c catalog.Cost, n wire.Normalized) float64 {
	readRate := c.CacheRead
	if readRate <= 0 {
		readRate = c.Input
	}
	writeRate := c.CacheWrite
	if writeRate <= 0 {
		writeRate = c.Input
	}
	return n.InputTokens*c.Input +
		n.CacheCreationTokens*writeRate +
		n.CachedInputTokens*readRate +
		n.OutputTokens*c.Output
}

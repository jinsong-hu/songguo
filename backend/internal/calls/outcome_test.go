package calls

import "testing"

// allSlugs is every exact slug the gateway writes into Entry.Err. Keep it in
// step with the Err* constants — TestOutcomeOfIsTotal is the guard that makes
// deriving the outcome (rather than persisting it) safe.
var allSlugs = []string{
	ErrBudgetExceeded,
	ErrRateLimited,
	ErrClientGone,
	ErrModelNotAllowed,
	ErrVendorNotAllowed,
	ErrNoUpstream,
	ErrRequestBuildFailed,
	ErrUpstreamError,
}

var allPrefixes = []string{
	ErrPrefixUnmatched,
	ErrPrefixTransport,
	ErrPrefixStream,
}

func TestOutcomeOfIsTotal(t *testing.T) {
	// Adding a slug in the proxy without teaching OutcomeOf about it fails here,
	// rather than silently rendering "unknown" on somebody's dashboard.
	for _, slug := range allSlugs {
		if got := OutcomeOf(502, slug); got == OutcomeUnknown {
			t.Errorf("OutcomeOf(502, %q) = unknown; every written slug must classify", slug)
		}
	}
	for _, prefix := range allPrefixes {
		if got := OutcomeOf(502, prefix+"some detail"); got == OutcomeUnknown {
			t.Errorf("OutcomeOf(502, %q...) = unknown; every written prefix must classify", prefix)
		}
	}
}

func TestOutcomeOfDegradesUnrecognizedSlugVisibly(t *testing.T) {
	if got := OutcomeOf(502, "invented_later"); got != OutcomeUnknown {
		t.Errorf("OutcomeOf(502, invented_later) = %q, want unknown", got)
	}
	// Crucially it must not be filed as the provider's fault.
	if BlameFor(OutcomeUnknown) != BlameNone {
		t.Errorf("an unrecognized outcome must blame nobody, got %q", BlameFor(OutcomeUnknown))
	}
}

func TestOutcomeOfSeparatesGatewayFromProvider(t *testing.T) {
	// The point of the whole exercise: the same integer, two different events.
	tests := []struct {
		name   string
		status int
		err    string
		want   Outcome
	}{
		{"provider throttled us", 429, "", OutcomeVendorError},
		{"we throttled the caller", 429, ErrRateLimited, OutcomeDeniedRate},
		{"provider refused", 403, "", OutcomeVendorError},
		{"we refused on scope", 403, ErrModelNotAllowed, OutcomeDeniedScope},
		{"provider 404", 404, "", OutcomeVendorError},
		{"no wire matched", 404, ErrPrefixUnmatched + "POST /v1/foo", OutcomeUnmatched},
		{"provider 502", 502, "", OutcomeVendorError},
		{"never reached provider", 502, ErrPrefixTransport + "connection refused", OutcomeTransportError},
		{"no vendor serves it", 502, ErrNoUpstream, OutcomeNoRoute},
		{"could not build request", 502, ErrRequestBuildFailed, OutcomeBuildFailed},
		{"clean answer", 200, "", OutcomeOK},
		{"stream died mid-body", 200, ErrPrefixStream + "unexpected EOF", OutcomeTruncated},
		{"caller hung up", 499, ErrClientGone, OutcomeClientGone},
		{"still running", StatusPending, "", OutcomeInFlight},
		// Legacy encoding: status 0 with no slug meant "no upstream response".
		// It must stay a failure — reading it as ok silently turned every old
		// transport failure into a success in the ratios.
		{"legacy transport failure", 0, "", OutcomeTransportError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OutcomeOf(tt.status, tt.err); got != tt.want {
				t.Errorf("OutcomeOf(%d, %q) = %q, want %q", tt.status, tt.err, got, tt.want)
			}
		})
	}
}

func TestBlameKeepsProviderRateHonest(t *testing.T) {
	// Nothing songguo or the caller did may count against a provider. This is the
	// definition the aggregates use, so a success rate cannot disagree with the
	// pill rendered beside it.
	notProvider := []Outcome{
		OutcomeDeniedBudget, OutcomeDeniedRate, OutcomeDeniedScope,
		OutcomeNoRoute, OutcomeBuildFailed, OutcomeUnmatched,
		OutcomeClientGone, OutcomeInFlight, OutcomeAbandoned,
		OutcomeUpstreamFailed, OutcomeUnknown,
	}
	for _, o := range notProvider {
		if IsProviderFailure(o) {
			t.Errorf("%q must not count as a provider failure", o)
		}
	}
	for _, o := range []Outcome{OutcomeVendorError, OutcomeTransportError, OutcomeTruncated} {
		if !IsProviderFailure(o) {
			t.Errorf("%q must count as a provider failure", o)
		}
	}
	if IsProviderFailure(OutcomeOK) {
		t.Error("a served call is not a failure")
	}
}

func TestLegacyUpstreamErrorIsNotGuessedAt(t *testing.T) {
	// It meant a transport failure OR a request-build failure and we cannot now
	// tell which. Picking one would invent a fact.
	got := OutcomeOf(502, ErrUpstreamError)
	if got != OutcomeUpstreamFailed {
		t.Fatalf("OutcomeOf(502, upstream_error) = %q, want upstream_failed", got)
	}
	if got == OutcomeTransportError || got == OutcomeBuildFailed {
		t.Error("legacy upstream_error must not be resolved into either specific failure")
	}
	if BlameFor(got) != BlameNone {
		t.Errorf("an unresolvable outcome must blame nobody, got %q", BlameFor(got))
	}
}

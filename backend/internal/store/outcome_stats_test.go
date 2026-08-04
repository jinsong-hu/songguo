package store

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// seedOutcomeCalls writes one row per distinct outcome plus a control success.
func seedOutcomeCalls(t *testing.T, s *Store) {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	rows := []calls.Entry{
		{Status: http.StatusOK, LatencyMS: 100},                                          // served
		{Status: http.StatusInternalServerError, LatencyMS: 200},                         // provider's 5xx
		{Status: http.StatusTooManyRequests, Err: calls.ErrRateLimited, LatencyMS: 5},    // OUR rate limit
		{Status: statusClientClosed, Err: calls.ErrClientGone, LatencyMS: 10},            // caller left
		{Status: http.StatusPaymentRequired, Err: calls.ErrBudgetExceeded, LatencyMS: 3}, // our denial
	}
	for i, e := range rows {
		e.TS = base.Add(time.Duration(i) * time.Minute)
		e.UserID, e.Model, e.Vendor = "u", "m", "v"
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
	// A call still in flight: created at phase 1, never finalized. latency_ms is
	// 0 on this row, which is exactly what used to poison the percentiles.
	if err := s.CreateCall(calls.Entry{
		ID: "pending-1", TS: base.Add(10 * time.Minute),
		UserID: "u", Model: "m", Vendor: "v",
	}); err != nil {
		t.Fatalf("CreateCall: %v", err)
	}
}

const statusClientClosed = 499

func TestOverviewStatsGradesOnlyWhatItHasAnOpinionOn(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	st, err := s.OverviewStats(Scope{}, nil, nil)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	// Requests stays a census of the 5 finalized rows — it renders as the
	// window's call count and has to reconcile with the facet counts beside it.
	// Only the pending row is missing, because it has not finished.
	if st.Requests != 5 {
		t.Errorf("Requests = %d, want 5 (every finalized call, refusals included)", st.Requests)
	}
	// Three of those five say nothing about whether requests get served: two
	// refusals songguo issued under configured limits, and one caller who left.
	if st.Rated != 2 {
		t.Errorf("Rated = %d, want 2 (the 200 and the 500; not our 429/402, not the 499)", st.Rated)
	}
	if st.Denied != 2 {
		t.Errorf("Denied = %d, want 2 (our rate limit + our budget refusal)", st.Denied)
	}
	// Only the provider's 500. Counting our own refusals here is what put a
	// healthy model at 28% while every provider behind it was fine.
	if st.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the 500 alone)", st.Errors)
	}
	// The three excluded rows leave BOTH sides. Landing in the denominator only
	// would silently reclassify each of them as a success.
	if st.Requests-st.Rated != 3 {
		t.Errorf("ungraded = %d, want 3", st.Requests-st.Rated)
	}
}

func TestOverviewStatsKeepsZeroLatencyRowsOutOfThePercentiles(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	st, err := s.OverviewStats(Scope{}, nil, nil)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	// Graded latencies: [100, 200]. denyCapture never sets latency_ms, so in
	// production a refusal is a 0 that sorts first and drags every percentile
	// down a rank — the same way a pending row's 0 used to. (The fixture gives
	// them 3ms and 5ms so this test fails loudly if they creep back in, rather
	// than agreeing with the bug by coincidence.)
	if st.P50 != 100 {
		t.Errorf("P50 = %d, want 100 (only graded calls have a service latency)", st.P50)
	}
	if st.P95 != 200 {
		t.Errorf("P95 = %d, want 200", st.P95)
	}
}

func TestClientCancellationIsNotAProviderFailure(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	vs, err := s.VendorStats(nil, nil)
	if err != nil {
		t.Fatalf("VendorStats: %v", err)
	}
	v := vs["v"]
	// Only the 500 is the provider's doing. Our 429, our 402 and the caller's
	// 499 are not, and must never mark a vendor down.
	if v.Errors != 1 {
		t.Errorf("vendor errors = %d, want 1 (only the provider's own 500)", v.Errors)
	}
	if v.Requests != 5 {
		t.Errorf("vendor requests = %d, want 5 (census: pending excluded)", v.Requests)
	}
	// The rate is Errors/Rated. Over Requests it would read 1/5 = 20% instead of
	// 1/2 = 50%, understating a provider that failed half the calls it was given
	// because three others never reached it.
	if v.Rated != 2 {
		t.Errorf("vendor rated = %d, want 2", v.Rated)
	}
	if v.Denied != 2 {
		t.Errorf("vendor denied = %d, want 2", v.Denied)
	}
	// Averaged over the graded set for the same reason the percentiles are.
	if v.AvgLatency != 150 {
		t.Errorf("vendor avg latency = %v, want 150 (mean of 100 and 200)", v.AvgLatency)
	}
}

func TestRefusalsAreCountedButNotGraded(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Every call for this model was refused for budget. Nothing was learned about
	// the model, so it has no success rate — and reporting 100% (which is what a
	// denominator of Requests, or of zero, produces) is the failure mode this
	// whole split exists to prevent.
	for i := 0; i < 3; i++ {
		e := calls.Entry{
			Status: http.StatusPaymentRequired, Err: calls.ErrBudgetExceeded,
			TS: base.Add(time.Duration(i) * time.Minute), UserID: "u", Model: "broke", Vendor: "v",
		}
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	until := base.Add(time.Hour)
	models, buckets, err := s.SuccessByModelSeries(Scope{}, BreakdownByModel, base, until, time.Hour)
	if err != nil {
		t.Fatalf("SuccessByModelSeries: %v", err)
	}
	// Ranking is by the census count, so a wholly-refused key still gets its own
	// row instead of vanishing into "Other" — the case where the panel most needs
	// to say what happened.
	if len(models) != 1 || models[0] != "broke" {
		t.Fatalf("models = %v, want [broke]", models)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	b := buckets[0]
	if b.Requests["broke"] != 3 {
		t.Errorf("requests = %d, want 3 (the refusals are still counted)", b.Requests["broke"])
	}
	if b.Rated["broke"] != 0 {
		t.Errorf("rated = %d, want 0 (nothing here can be graded)", b.Rated["broke"])
	}
	if b.Denied["broke"] != 3 {
		t.Errorf("denied = %d, want 3 (so the panel can say why the rate is empty)", b.Denied["broke"])
	}
	if b.Errors["broke"] != 0 {
		t.Errorf("errors = %d, want 0 (a refusal is not the service failing)", b.Errors["broke"])
	}

	// The same calls remain fully visible in the census that ranks error codes.
	// A rate that ignores them and a list that shows them is the intended shape;
	// a list that ALSO dropped them would hide 946 refusals from the operator.
	rows, err := s.TopErrorCodes(Scope{}, "", "", nil, nil, 8)
	if err != nil {
		t.Fatalf("TopErrorCodes: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome != string(calls.OutcomeDeniedBudget) || rows[0].Count != 3 {
		t.Errorf("error codes = %+v, want one denied_budget row of 3", rows)
	}
}

func TestVendorLastStatusNeverReportsTheSentinel(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	vs, err := s.VendorStats(nil, nil)
	if err != nil {
		t.Fatalf("VendorStats: %v", err)
	}
	// The newest row for "v" is the pending one. Reporting its status would put
	// a raw -1 on the Providers page.
	if got := vs["v"].LastStatus; got == calls.StatusPending {
		t.Errorf("LastStatus = %d; the pending sentinel must never surface", got)
	}
	if got := vs["v"].LastStatus; got != http.StatusPaymentRequired {
		t.Errorf("LastStatus = %d, want 402 (the newest FINISHED call)", got)
	}
}

func TestTopErrorCodesSeparatesOurRateLimitFromTheProviders(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Two 429s that mean opposite things: one the provider throttling us, one
	// songguo throttling the caller. Grouped by status alone they merge.
	rows := []calls.Entry{
		{Status: http.StatusTooManyRequests, Err: ""},
		{Status: http.StatusTooManyRequests, Err: ""},
		{Status: http.StatusTooManyRequests, Err: calls.ErrRateLimited},
	}
	for i, e := range rows {
		e.TS = base.Add(time.Duration(i) * time.Minute)
		e.UserID, e.Model, e.Vendor = "u", "m", "v"
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	got, err := s.TopErrorCodes(Scope{}, "", "", nil, nil, 8)
	if err != nil {
		t.Fatalf("TopErrorCodes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d (%+v), want 2 — the provider's 429 and ours are different failures", len(got), got)
	}
	byOutcome := map[string]int{}
	for _, r := range got {
		if r.Status != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429 preserved alongside the outcome", r.Status)
		}
		byOutcome[r.Outcome] = r.Count
	}
	if byOutcome[string(calls.OutcomeVendorError)] != 2 {
		t.Errorf("provider 429s = %d, want 2", byOutcome[string(calls.OutcomeVendorError)])
	}
	if byOutcome[string(calls.OutcomeDeniedRate)] != 1 {
		t.Errorf("songguo 429s = %d, want 1", byOutcome[string(calls.OutcomeDeniedRate)])
	}
}

func TestErrorClassCountsSeparatesGatewayFromProvider(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	c, err := s.ErrorClassCounts(nil, nil)
	if err != nil {
		t.Fatalf("ErrorClassCounts: %v", err)
	}
	if c.ServerError != 1 {
		t.Errorf("ServerError = %d, want 1", c.ServerError)
	}
	// Our own 429 must not be filed under the provider's rate-limit bucket.
	if c.RateLimited != 0 {
		t.Errorf("RateLimited = %d, want 0 (the only 429 was songguo's own)", c.RateLimited)
	}
	// Our 429 and our 402 are both gateway refusals.
	if c.Gateway != 2 {
		t.Errorf("Gateway = %d, want 2 (our rate limit + our budget denial)", c.Gateway)
	}
	if c.ClientError != 0 {
		t.Errorf("ClientError = %d, want 0 (the 499 is the caller leaving, not an error)", c.ClientError)
	}
}

func TestQualifyRewritesEveryPredicate(t *testing.T) {
	// qualify is a substring rewrite, safe only because no slug in the predicates
	// contains "status" or "err " with its trailing space. Both ways of breaking
	// that are silent — the one caller's outer scope has a single table, so an
	// unqualified column still resolves and the query still runs. This is the
	// only thing standing between a new slug and a wrong session error count.
	for _, pred := range []string{
		sqlHasVerdict, sqlNotCallerAbort, sqlPolicyDenied,
		sqlNotServed, sqlRated, sqlRatedFailure, sqlProviderFailed,
	} {
		got := qualify(pred, "c")
		// Every column reference must have picked up the alias...
		if strings.Contains(got, "c.c.") {
			t.Errorf("qualify(%q) double-prefixed: %q", pred, got)
		}
		for _, bare := range []string{" status ", "(status ", " err ", "(err "} {
			if strings.Contains(" "+got, bare) {
				t.Errorf("qualify(%q) left a bare column %q: %q", pred, bare, got)
			}
		}
		// ...and no string literal may have been rewritten along with them.
		for _, lit := range []string{
			"'client_gone'", "'budget_exceeded'", "'rate_limited'",
			"'transport_error:%'", "'stream_error:%'",
		} {
			if strings.Contains(pred, lit) && !strings.Contains(got, lit) {
				t.Errorf("qualify(%q) corrupted the literal %s: %q", pred, lit, got)
			}
		}
	}
	if got := qualify(sqlRated, ""); got != sqlRated {
		t.Errorf("qualify with no alias must be identity, got %q", got)
	}
}

func TestErrorClassCountsCountsRealTransportFailures(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	rows := []calls.Entry{
		// The current encoding, and the legacy one, must both land in Transport.
		{Status: http.StatusBadGateway, Err: calls.ErrPrefixTransport + "connection refused"},
		{Status: 0, Err: ""},
		{Status: http.StatusOK, Err: calls.ErrPrefixStream + "unexpected EOF"},
	}
	for i, e := range rows {
		e.TS = base.Add(time.Duration(i) * time.Minute)
		e.UserID, e.Model, e.Vendor = "u", "m", "v"
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	c, err := s.ErrorClassCounts(nil, nil)
	if err != nil {
		t.Fatalf("ErrorClassCounts: %v", err)
	}
	// This bucket was permanently zero: it keyed on status = 0, which nothing has
	// written since transport failures started being recorded as 502.
	if c.Transport != 3 {
		t.Errorf("Transport = %d, want 3 (502+slug, legacy 0, and the cut stream)", c.Transport)
	}
	if c.ServerError != 0 {
		t.Errorf("ServerError = %d, want 0 (a transport failure is not the provider's 5xx)", c.ServerError)
	}
}

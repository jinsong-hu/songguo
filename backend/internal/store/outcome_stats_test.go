package store

import (
	"net/http"
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

func TestOverviewStatsGivesPendingNoVerdict(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	st, err := s.OverviewStats(Scope{}, nil, nil)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	// 5 finalized rows; the pending one is in neither numerator nor denominator.
	// Counting it as a success (the old behavior — -1 is not >= 400) let
	// abandoned calls quietly raise the success rate.
	if st.Requests != 5 {
		t.Errorf("Requests = %d, want 5 (pending excluded from the denominator)", st.Requests)
	}
	// 500 and the budget denial failed; the 200 served, our 429 is a denial that
	// also failed, and the client cancellation is nobody's failure.
	if st.Errors != 3 {
		t.Errorf("Errors = %d, want 3 (500 + our 429 + budget denial; not the 499)", st.Errors)
	}
}

func TestOverviewStatsExcludesPendingFromLatency(t *testing.T) {
	s := openTestStore(t)
	seedOutcomeCalls(t, s)

	st, err := s.OverviewStats(Scope{}, nil, nil)
	if err != nil {
		t.Fatalf("OverviewStats: %v", err)
	}
	// Sorted finalized latencies: [3, 5, 10, 100, 200]. A pending row's 0 would
	// sort first and drag every percentile down a rank.
	if st.P50 != 10 {
		t.Errorf("P50 = %d, want 10 (a pending row's latency_ms=0 must not count)", st.P50)
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
		t.Errorf("vendor requests = %d, want 5 (pending excluded)", v.Requests)
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

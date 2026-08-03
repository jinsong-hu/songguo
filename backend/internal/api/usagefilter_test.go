package api

import (
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

// filterFixture seeds a two-model, two-provider, two-session ledger:
//
//	opus  / anyrouter  $1  sess1  200  claude-code
//	opus  / packy      $2  sess2  200  codex-openai
//	haiku / anyrouter  $4  sess2  500  claude-code   <- the only error
//	haiku / packy      $8  (none) 200  (no client)
//
// Every (model, provider) pair appears exactly once and costs a distinct power
// of two, so any subset's total spend names the exact rows that survived a
// filter — no assertion has to trust a count alone.
//
// The last row deliberately carries no client: an unrecognized User-Agent stores
// an empty client_name and is offered by no facet, which is what makes the client
// dimension the one where selecting every option is narrower than selecting none.
func filterFixture(t *testing.T) (*store.Store, time.Time) {
	t.Helper()
	s := newTestStore(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	entries := []calls.Entry{
		{TS: now.Add(-1 * time.Hour), SessionID: "sess1", Model: "claude-opus-5", Modality: calls.ModalityChat, Vendor: "anyrouter", Status: 200, Cost: 1.0, LatencyMS: 100, InputTokens: 100, OutputTokens: 10, ClientName: "claude-code"},
		{TS: now.Add(-2 * time.Hour), SessionID: "sess2", Model: "claude-opus-5", Modality: calls.ModalityChat, Vendor: "packy", Status: 200, Cost: 2.0, LatencyMS: 100, InputTokens: 100, OutputTokens: 10, ClientName: "codex-openai"},
		{TS: now.Add(-3 * time.Hour), SessionID: "sess2", Model: "claude-haiku-4-5", Modality: calls.ModalityChat, Vendor: "anyrouter", Status: 500, Cost: 4.0, LatencyMS: 100, InputTokens: 100, OutputTokens: 10, ClientName: "claude-code"},
		{TS: now.Add(-4 * time.Hour), Model: "claude-haiku-4-5", Modality: calls.ModalityChat, Vendor: "packy", Status: 200, Cost: 8.0, LatencyMS: 100, InputTokens: 100, OutputTokens: 10},
	}
	for _, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
	}
	return s, now
}

// TestOverviewModelVendorFilter checks that ?models=/?vendors= narrow the
// overview aggregate, that repeating a param ORs within a dimension, and that
// the two dimensions AND with each other.
func TestOverviewModelVendorFilter(t *testing.T) {
	s, now := filterFixture(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	cases := []struct {
		name  string
		query string
		spend float64
		reqs  int
	}{
		{"unfiltered", "", 15.0, 4},
		{"one model", "?models=claude-opus-5", 3.0, 2},
		{"one provider", "?vendors=anyrouter", 5.0, 2},
		{"repeated param ORs within a dimension", "?models=claude-opus-5&models=claude-haiku-4-5", 15.0, 4},
		{"the two dimensions AND", "?models=claude-opus-5&vendors=packy", 2.0, 1},
		{"no overlap yields an empty aggregate", "?models=claude-opus-5&vendors=nobody", 0.0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ov overviewView
			decodeBody(t, do(h, "GET", "/api/overview"+tc.query, "secret", nil), &ov)
			if !approxF(ov.TotalSpend, tc.spend) {
				t.Errorf("total_spend = %v, want %v", ov.TotalSpend, tc.spend)
			}
			if ov.Requests != tc.reqs {
				t.Errorf("requests = %d, want %d", ov.Requests, tc.reqs)
			}
		})
	}
}

// TestOverviewClientFilter is TestOverviewModelVendorFilter's sibling for the
// client dimension, plus the case that pins the design decision: the client list
// is not exhaustive, so selecting every offered client is narrower than
// selecting none. If someone ever adds a synthesized "Other" facet, the union
// case below is what fails.
func TestOverviewClientFilter(t *testing.T) {
	s, now := filterFixture(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	cases := []struct {
		name  string
		query string
		spend float64
		reqs  int
	}{
		{"unfiltered", "", 15.0, 4},
		{"claude code", "?clients=claude-code", 5.0, 2},
		{"codex", "?clients=codex-openai", 2.0, 1},
		// $8 row carried no recognized client, so it survives no client filter.
		{"every offered client is still narrower than none", "?clients=claude-code&clients=codex-openai", 7.0, 3},
		{"client ANDs with model", "?clients=claude-code&models=claude-opus-5", 1.0, 1},
		{"client ANDs with provider", "?clients=claude-code&vendors=packy", 0.0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ov overviewView
			decodeBody(t, do(h, "GET", "/api/overview"+tc.query, "secret", nil), &ov)
			if !approxF(ov.TotalSpend, tc.spend) {
				t.Errorf("total_spend = %v, want %v", ov.TotalSpend, tc.spend)
			}
			if ov.Requests != tc.reqs {
				t.Errorf("requests = %d, want %d", ov.Requests, tc.reqs)
			}
		})
	}
}

// TestUsageFilterAppliesToEveryOverviewPanel is the coverage guard: each
// endpoint the Overview page reads must honour the filter. A new panel that
// forgets to thread statsScope through shows up here as an unnarrowed count.
func TestUsageFilterAppliesToEveryOverviewPanel(t *testing.T) {
	s, now := filterFixture(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})
	const q = "?models=claude-opus-5"

	// tokens-by-model: only the filtered model may appear as a series.
	var tok tokensByModelView
	decodeBody(t, do(h, "GET", "/api/usage/tokens-by-model"+q, "secret", nil), &tok)
	for _, m := range tok.Models {
		if m != "claude-opus-5" {
			t.Errorf("tokens-by-model series = %v, want only claude-opus-5", tok.Models)
		}
	}

	// success-by-model: same, via its own store query.
	var suc successByModelView
	decodeBody(t, do(h, "GET", "/api/usage/success-by-model"+q, "secret", nil), &suc)
	for _, m := range suc.Models {
		if m != "claude-opus-5" {
			t.Errorf("success-by-model series = %v, want only claude-opus-5", suc.Models)
		}
	}

	// cache-by-model: same.
	var cache cacheByModelView
	decodeBody(t, do(h, "GET", "/api/usage/cache-by-model"+q, "secret", nil), &cache)
	for _, m := range cache.Models {
		if m != "claude-opus-5" {
			t.Errorf("cache-by-model series = %v, want only claude-opus-5", cache.Models)
		}
	}

	// error-codes: the only error row is haiku's 500, so filtering to opus
	// must empty the panel rather than leave the 500 behind.
	var ec errorCodesView
	decodeBody(t, do(h, "GET", "/api/usage/error-codes"+q, "secret", nil), &ec)
	if len(ec.Rows) != 0 {
		t.Errorf("error-codes rows = %v, want none (the 500 belongs to haiku)", ec.Rows)
	}
	var ecHaiku errorCodesView
	decodeBody(t, do(h, "GET", "/api/usage/error-codes?models=claude-haiku-4-5", "secret", nil), &ecHaiku)
	if len(ecHaiku.Rows) != 1 || ecHaiku.Rows[0].Status != 500 {
		t.Errorf("error-codes for haiku = %v, want one 500 row", ecHaiku.Rows)
	}

	// feed: 2 opus calls, in 2 different sessions, so 2 grouped rows.
	var feed feedView
	decodeBody(t, do(h, "GET", "/api/feed"+q, "secret", nil), &feed)
	if feed.Total != 2 {
		t.Errorf("feed total = %d, want 2", feed.Total)
	}

	// The client dimension travels the same two paths — statsScope for the
	// aggregates, CallFilter for the feed — so check one of each. The 500 belongs
	// to a claude-code call, so codex must see no error rows and one feed row.
	var ecCodex errorCodesView
	decodeBody(t, do(h, "GET", "/api/usage/error-codes?clients=codex-openai", "secret", nil), &ecCodex)
	if len(ecCodex.Rows) != 0 {
		t.Errorf("error-codes for codex = %v, want none (the 500 was a claude-code call)", ecCodex.Rows)
	}
	var ecClaude errorCodesView
	decodeBody(t, do(h, "GET", "/api/usage/error-codes?clients=claude-code", "secret", nil), &ecClaude)
	if len(ecClaude.Rows) != 1 || ecClaude.Rows[0].Status != 500 {
		t.Errorf("error-codes for claude-code = %v, want one 500 row", ecClaude.Rows)
	}
	var feedCodex feedView
	decodeBody(t, do(h, "GET", "/api/feed?clients=codex-openai", "secret", nil), &feedCodex)
	if feedCodex.Total != 1 {
		t.Errorf("feed total for codex = %d, want 1", feedCodex.Total)
	}
}

// TestSessionStatsFilterSelectsTouchingSessions pins the one place the filter
// changes meaning instead of just narrowing: a session is selected when it
// *touched* the model, and is then counted whole. sess2 ran both models, so
// filtering to either one must still return it — with all of its turns.
func TestSessionStatsFilterSelectsTouchingSessions(t *testing.T) {
	s, now := filterFixture(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	var all, opus, haiku sessionStatsView
	decodeBody(t, do(h, "GET", "/api/sessions/overview", "secret", nil), &all)
	decodeBody(t, do(h, "GET", "/api/sessions/overview?models=claude-opus-5", "secret", nil), &opus)
	decodeBody(t, do(h, "GET", "/api/sessions/overview?models=claude-haiku-4-5", "secret", nil), &haiku)

	if all.Sessions != 2 {
		t.Fatalf("unfiltered sessions = %d, want 2", all.Sessions)
	}
	// opus ran in sess1 and sess2 → both.
	if opus.Sessions != 2 {
		t.Errorf("sessions with models=opus = %d, want 2 (sess1 and sess2 both used it)", opus.Sessions)
	}
	// haiku ran only in sess2 (its other call carries no session id) → one.
	if haiku.Sessions != 1 {
		t.Errorf("sessions with models=haiku = %d, want 1 (only sess2)", haiku.Sessions)
	}
	// sess2 ran two models but is still reported as one whole session: its two
	// turns survive a filter that matches only one of them.
	if haiku.AvgTurns != 2 {
		t.Errorf("avg_turns for the haiku-filtered view = %v, want 2 (sess2 counted whole)", haiku.AvgTurns)
	}
}

// TestSessionStatsClientFilter is the same "touched, then counted whole" rule on
// the client dimension. It is the assertion that catches a forgotten Clients
// field in SessionStats' subquery Scope: without it the filter would be dropped
// silently and every query would return both sessions.
func TestSessionStatsClientFilter(t *testing.T) {
	s, now := filterFixture(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	var codex sessionStatsView
	decodeBody(t, do(h, "GET", "/api/sessions/overview?clients=codex-openai", "secret", nil), &codex)

	// Only sess2 has a codex turn.
	if codex.Sessions != 1 {
		t.Errorf("sessions with clients=codex = %d, want 1 (only sess2)", codex.Sessions)
	}
	// And it is reported whole — both of sess2's turns, though only one was codex.
	if codex.AvgTurns != 2 {
		t.Errorf("avg_turns for the codex-filtered view = %v, want 2 (sess2 counted whole)", codex.AvgTurns)
	}
}

// TestUsageFacets checks the option lists: observed values ranked by request
// count, and — the part that is easy to get wrong — no cross-filtering, so
// selecting a model must not shrink the providers list.
func TestUsageFacets(t *testing.T) {
	s, now := filterFixture(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})

	var f facetsView
	decodeBody(t, do(h, "GET", "/api/usage/facets", "secret", nil), &f)
	if len(f.Models) != 2 || len(f.Vendors) != 2 {
		t.Fatalf("facets = %d models / %d vendors, want 2 and 2", len(f.Models), len(f.Vendors))
	}
	for _, m := range f.Models {
		if m.Requests != 2 {
			t.Errorf("facet %q requests = %d, want 2", m.Key, m.Requests)
		}
	}

	// Clients: ranked by count, and — the load-bearing one — no row for the call
	// whose User-Agent named no recognized client. That '' row is deliberately
	// unselectable rather than offered as "Other"; see store.Facets before
	// "fixing" this.
	if len(f.Clients) != 2 {
		t.Fatalf("client facets = %+v, want exactly 2 (claude-code, codex-openai) and no row for the unrecognized-client call", f.Clients)
	}
	if f.Clients[0].Key != "claude-code" || f.Clients[0].Requests != 2 {
		t.Errorf("top client facet = %+v, want claude-code with 2 requests", f.Clients[0])
	}
	if f.Clients[1].Key != "codex-openai" || f.Clients[1].Requests != 1 {
		t.Errorf("second client facet = %+v, want codex-openai with 1 request", f.Clients[1])
	}

	// Passing a selection must not narrow either list.
	var pinned facetsView
	decodeBody(t, do(h, "GET", "/api/usage/facets?models=claude-opus-5&vendors=packy&clients=claude-code", "secret", nil), &pinned)
	if len(pinned.Models) != 2 || len(pinned.Vendors) != 2 || len(pinned.Clients) != 2 {
		t.Errorf("facets under a selection = %d models / %d vendors / %d clients, want the full 2, 2 and 2 (no cross-filtering)",
			len(pinned.Models), len(pinned.Vendors), len(pinned.Clients))
	}
}

// TestUsageFacetsScopedToUserKey confirms the option lists respect the consumer
// scope: a user key must not be offered a model only somebody else ran.
func TestUsageFacetsScopedToUserKey(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	alice, aliceKey, err := s.CreateUser(store.NewUser{Name: "alice"})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, _, err := s.CreateUser(store.NewUser{Name: "bob"})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	for _, e := range []calls.Entry{
		{TS: now.Add(-time.Hour), UserID: alice.ID, Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 200, Cost: 1, ClientName: "claude-code"},
		{TS: now.Add(-time.Hour), UserID: bob.ID, Model: "deepseek-chat", Modality: calls.ModalityChat, Vendor: "deepseek", Status: 200, Cost: 1, ClientName: "codex-openai"},
	} {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
	}

	h := testHandler(t, Deps{Store: s, AdminKey: "secret", Now: func() time.Time { return now }})
	var f facetsView
	decodeBody(t, do(h, "GET", "/api/usage/facets", aliceKey, nil), &f)
	for _, m := range f.Models {
		if m.Key == "deepseek-chat" {
			t.Errorf("alice's facets leaked bob's model: %+v", f.Models)
		}
	}
	for _, v := range f.Vendors {
		if v.Key == "deepseek" {
			t.Errorf("alice's facets leaked bob's provider: %+v", f.Vendors)
		}
	}
	for _, c := range f.Clients {
		if c.Key == "codex-openai" {
			t.Errorf("alice's facets leaked bob's client: %+v", f.Clients)
		}
	}
}

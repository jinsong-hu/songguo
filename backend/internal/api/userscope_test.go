package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

// TestOverviewScopedToUserKey verifies that the shared analytics endpoints
// (/api/overview, /api/usage/tokens-by-model, /api/feed) restrict their results
// to the calling consumer key's own traffic, while the admin key still sees the
// union across all users. It also confirms a user cannot widen the feed back to
// another user's rows via a spoofed ?user_id query param.
func TestOverviewScopedToUserKey(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	alice, aliceKey, err := s.CreateUser(store.NewUser{Name: "alice"})
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, bobKey, err := s.CreateUser(store.NewUser{Name: "bob"})
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	// Alice: two successful gpt-4o calls totalling $3.00, one of them session-
	// bearing (sessA). Bob: one $10.00 call in its own session (sessB). The
	// session-bearing calls fold into the per-user sessions rollup.
	entries := []calls.Entry{
		{TS: now.Add(-1 * time.Hour), UserID: alice.ID, Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 200, Cost: 1.0, LatencyMS: 100, InputTokens: 100, OutputTokens: 20},
		{TS: now.Add(-2 * time.Hour), UserID: alice.ID, SessionID: "sessA", Model: "gpt-4o", Modality: calls.ModalityChat, Vendor: "openai", Status: 200, Cost: 2.0, LatencyMS: 120, InputTokens: 200, OutputTokens: 40},
		{TS: now.Add(-3 * time.Hour), UserID: bob.ID, SessionID: "sessB", Model: "deepseek-chat", Modality: calls.ModalityChat, Vendor: "deepseek", Status: 200, Cost: 10.0, LatencyMS: 300, InputTokens: 500, OutputTokens: 90},
	}
	for _, e := range entries {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
	}

	h := testHandler(t, Deps{
		Store:    s,
		AdminKey: "secret",
		Now:      func() time.Time { return now },
	})

	// --- /api/overview ---
	var adminOv, aliceOv, bobOv overviewView
	decodeBody(t, do(h, "GET", "/api/overview", "secret", nil), &adminOv)
	decodeBody(t, do(h, "GET", "/api/overview", aliceKey, nil), &aliceOv)
	decodeBody(t, do(h, "GET", "/api/overview", bobKey, nil), &bobOv)

	if got := adminOv.TotalSpend; !approxF(got, 13.0) {
		t.Errorf("admin total_spend = %v, want 13.0 (union)", got)
	}
	if got := aliceOv.TotalSpend; !approxF(got, 3.0) {
		t.Errorf("alice total_spend = %v, want 3.0 (own only)", got)
	}
	if got := bobOv.TotalSpend; !approxF(got, 10.0) {
		t.Errorf("bob total_spend = %v, want 10.0 (own only)", got)
	}
	if aliceOv.Requests != 2 {
		t.Errorf("alice requests = %d, want 2", aliceOv.Requests)
	}
	// Fleet-only fields are suppressed in the scoped view.
	if aliceOv.ActiveCallers != 0 || aliceOv.UsersActive != 0 || aliceOv.RunwayDays != nil {
		t.Errorf("alice fleet fields leaked: callers=%d users=%d runway=%v",
			aliceOv.ActiveCallers, aliceOv.UsersActive, aliceOv.RunwayDays)
	}
	// Admin keeps the fleet rollups.
	if adminOv.ActiveCallers != 2 {
		t.Errorf("admin active_callers = %d, want 2", adminOv.ActiveCallers)
	}

	// --- /api/usage/tokens-by-model: alice only ever used gpt-4o ---
	var aliceTok tokensByModelView
	decodeBody(t, do(h, "GET", "/api/usage/tokens-by-model", aliceKey, nil), &aliceTok)
	for _, m := range aliceTok.Models {
		if m == "deepseek-chat" {
			t.Errorf("alice tokens-by-model leaked bob's model deepseek-chat: %v", aliceTok.Models)
		}
	}

	// --- /api/feed: alice sees her 2 rows, and a spoofed ?user_id can't widen it ---
	var aliceFeed, spoofFeed feedView
	decodeBody(t, do(h, "GET", "/api/feed", aliceKey, nil), &aliceFeed)
	if aliceFeed.Total != 2 {
		t.Errorf("alice feed total = %d, want 2", aliceFeed.Total)
	}
	decodeBody(t, do(h, "GET", "/api/feed?user_id="+bob.ID, aliceKey, nil), &spoofFeed)
	if spoofFeed.Total != 2 {
		t.Errorf("alice feed with spoofed user_id=bob total = %d, want 2 (scope enforced server-side)", spoofFeed.Total)
	}

	// --- /api/sessions/overview: scoped to the caller's own sessions ---
	var adminSess, aliceSess, bobSess sessionStatsView
	decodeBody(t, do(h, "GET", "/api/sessions/overview", "secret", nil), &adminSess)
	decodeBody(t, do(h, "GET", "/api/sessions/overview", aliceKey, nil), &aliceSess)
	decodeBody(t, do(h, "GET", "/api/sessions/overview", bobKey, nil), &bobSess)
	if adminSess.Sessions != 2 {
		t.Errorf("admin sessions = %d, want 2 (union)", adminSess.Sessions)
	}
	if aliceSess.Sessions != 1 {
		t.Errorf("alice sessions = %d, want 1 (own only)", aliceSess.Sessions)
	}
	if bobSess.Sessions != 1 {
		t.Errorf("bob sessions = %d, want 1 (own only)", bobSess.Sessions)
	}

	// The per-session detail routes remain admin-only.
	if rec := do(h, "GET", "/api/sessions/sessB", aliceKey, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("user key on /api/sessions/{id} = %d, want 401", rec.Code)
	}
}

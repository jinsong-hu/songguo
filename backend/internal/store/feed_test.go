package store

import (
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// TestAppendCallRoundTripsAttribution checks the coding-agent attribution
// columns persist and read back through AppendCall/QueryCalls/GetCall.
func TestAppendCallRoundTripsAttribution(t *testing.T) {
	s := openTestStore(t)

	id, err := s.AppendCall(calls.Entry{
		TS: time.Now(), Model: "m", Vendor: "v", Status: 200,
		SessionID: "sess-1", AgentID: "agent-a", ParentAgentID: "agent-root",
	})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}

	got, err := s.GetCall(id)
	if err != nil {
		t.Fatalf("GetCall: %v", err)
	}
	if got.SessionID != "sess-1" || got.AgentID != "agent-a" || got.ParentAgentID != "agent-root" {
		t.Errorf("attribution round-trip = %q/%q/%q, want sess-1/agent-a/agent-root",
			got.SessionID, got.AgentID, got.ParentAgentID)
	}
}

func TestGetCallNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetCall("999"); err != ErrNotFound {
		t.Errorf("GetCall(missing) err = %v, want ErrNotFound", err)
	}
}

// TestFeedGroupsSessions checks the feed collapses a session's calls into one
// row while leaving session-less calls as their own request rows, newest first.
func TestFeedGroupsSessions(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Three calls in session "sess" (oldest three), then one standalone request.
	appends := []calls.Entry{
		{TS: base.Add(0 * time.Minute), Model: "m1", Vendor: "v", Status: 200, Cost: 1, InputTokens: 10, OutputTokens: 5, LatencyMS: 1000, SessionID: "sess"},
		{TS: base.Add(1 * time.Minute), Model: "m2", Vendor: "v", Status: 500, Cost: 2, InputTokens: 20, OutputTokens: 7, LatencyMS: 2000, SessionID: "sess"},
		{TS: base.Add(2 * time.Minute), Model: "m1", Vendor: "v", Status: 200, Cost: 1, InputTokens: 5, OutputTokens: 3, LatencyMS: 3000, SessionID: "sess"},
		{TS: base.Add(3 * time.Minute), Model: "m3", Vendor: "w", Status: 200, Cost: 4, InputTokens: 30, OutputTokens: 9, LatencyMS: 4000},
	}
	for i, e := range appends {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, "Recent activity titles", "sess"); err != nil {
		t.Fatalf("set session title: %v", err)
	}

	rows, total, err := s.Feed(CallFilter{})
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if total != 2 {
		t.Fatalf("total groups = %d, want 2", total)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	// Newest activity first: the standalone request (t+3m) leads the session
	// (last activity t+2m).
	req := rows[0]
	if req.Kind != "request" {
		t.Errorf("rows[0].Kind = %q, want request", req.Kind)
	}
	if req.RequestID == "" || req.Model != "m3" || req.Calls != 1 {
		t.Errorf("request row = %+v, want single m3 call with an id", req)
	}
	if req.DurationMS != 4000 {
		t.Errorf("request duration = %d, want 4000", req.DurationMS)
	}

	sess := rows[1]
	if sess.Kind != "session" || sess.SessionID != "sess" {
		t.Fatalf("rows[1] = %+v, want session sess", sess)
	}
	if sess.Title != "Recent activity titles" {
		t.Errorf("session title = %q, want Recent activity titles", sess.Title)
	}
	if sess.Calls != 3 {
		t.Errorf("session calls = %d, want 3", sess.Calls)
	}
	if sess.Cost != 4 || sess.InputTokens != 35 || sess.OutputTokens != 15 {
		t.Errorf("session rollup cost/in/out = %v/%v/%v, want 4/35/15", sess.Cost, sess.InputTokens, sess.OutputTokens)
	}
	if sess.ErrorCount != 1 {
		t.Errorf("session error_count = %d, want 1 (the 500)", sess.ErrorCount)
	}
	if len(sess.Models) != 2 {
		t.Errorf("session models = %v, want 2 distinct", sess.Models)
	}
	if sess.MajorModel != "m1" {
		t.Errorf("session major model = %q, want m1", sess.MajorModel)
	}
	if sess.DurationMS != 123000 {
		t.Errorf("session duration = %d, want 123000", sess.DurationMS)
	}
}

// TestFeedSort checks each whitelisted sort key ranks the feed by the right
// metric and that "failures" scopes to errored rows only.
func TestFeedSort(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Three standalone requests, each dominant on a different metric.
	//   A: newest, cheap, small, fast, ok
	//   B: middle, most tokens + most cost, slow latency, ok
	//   C: oldest, longest wall-clock (big latency tail), errored (500)
	appends := []calls.Entry{
		{TS: base.Add(20 * time.Minute), Model: "mA", Vendor: "v", Status: 200, Cost: 1, InputTokens: 10, OutputTokens: 5, LatencyMS: 500},
		{TS: base.Add(10 * time.Minute), Model: "mB", Vendor: "v", Status: 200, Cost: 9, InputTokens: 900, OutputTokens: 100, LatencyMS: 8000},
		{TS: base.Add(0 * time.Minute), Model: "mC", Vendor: "v", Status: 500, Cost: 2, InputTokens: 50, OutputTokens: 20, LatencyMS: 60000},
	}
	for i, e := range appends {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	// leadModel returns the model of the first feed row for a given sort.
	leadModel := func(sort string) (string, int) {
		rows, total, err := s.Feed(CallFilter{FeedSort: sort})
		if err != nil {
			t.Fatalf("Feed(%q): %v", sort, err)
		}
		if len(rows) == 0 {
			t.Fatalf("Feed(%q): no rows", sort)
		}
		return rows[0].Model, total
	}

	cases := []struct {
		sort string
		want string
	}{
		{"", "mA"},         // default: recent-first → newest is A
		{"recent", "mA"},   // A is newest
		{"tokens", "mB"},   // B has the most total tokens (1000)
		{"cost", "mB"},     // B costs the most (9)
		{"duration", "mC"}, // C has the longest wall-clock (60s latency tail)
		{"slow", "mC"},     // C has the worst single-call latency (60000)
		{"turns", "mA"},    // all single-call; tiebreak MAX(ts) DESC → A newest
	}
	for _, c := range cases {
		if got, _ := leadModel(c.sort); got != c.want {
			t.Errorf("Feed(sort=%q) lead model = %q, want %q", c.sort, got, c.want)
		}
	}

	// failures: only C errored, so exactly one row and it is C.
	rows, total, err := s.Feed(CallFilter{FeedSort: "failures"})
	if err != nil {
		t.Fatalf("Feed(failures): %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("failures total/rows = %d/%d, want 1/1", total, len(rows))
	}
	if rows[0].Model != "mC" {
		t.Errorf("failures lead model = %q, want mC", rows[0].Model)
	}
}

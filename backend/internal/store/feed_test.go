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
	if _, err := s.GetCall(999); err != ErrNotFound {
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

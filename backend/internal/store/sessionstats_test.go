package store

import (
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// TestSessionStats checks session aggregation: outcome inferred from each
// session's last call, subagent fan-out, totals, and percentiles. Non-session
// traffic is excluded.
func TestSessionStats(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	appends := []calls.Entry{
		// sess-done: 3 turns, ends on a 200 → completed. Spans 2 minutes.
		{TS: base.Add(0 * time.Minute), Model: "m", Vendor: "v", Status: 200, InputTokens: 10, OutputTokens: 5, SessionID: "sess-done"},
		{TS: base.Add(1 * time.Minute), Model: "m", Vendor: "v", Status: 500, InputTokens: 10, OutputTokens: 5, SessionID: "sess-done"},
		{TS: base.Add(2 * time.Minute), Model: "m", Vendor: "v", Status: 200, InputTokens: 10, OutputTokens: 5, SessionID: "sess-done",
			AgentID: "sub", ParentAgentID: "root"}, // spawned a subagent
		// sess-cut: 1 turn, ends on status 0 (client abort) → interrupted.
		{TS: base.Add(3 * time.Minute), Model: "m", Vendor: "v", Status: 0, InputTokens: 4, OutputTokens: 0, SessionID: "sess-cut"},
		// sess-err: last call is a 429 → errored.
		{TS: base.Add(4 * time.Minute), Model: "m", Vendor: "v", Status: 200, InputTokens: 2, OutputTokens: 1, SessionID: "sess-err"},
		{TS: base.Add(5 * time.Minute), Model: "m", Vendor: "v", Status: 429, InputTokens: 2, OutputTokens: 1, SessionID: "sess-err"},
		// standalone request (no session id) — must be ignored entirely.
		{TS: base.Add(6 * time.Minute), Model: "m", Vendor: "v", Status: 200, InputTokens: 99, OutputTokens: 99},
	}
	for i, e := range appends {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	st, err := s.SessionStats(nil, nil)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}

	if st.Sessions != 3 {
		t.Errorf("Sessions = %d, want 3", st.Sessions)
	}
	if st.Completed != 1 || st.Interrupted != 1 || st.Errored != 1 {
		t.Errorf("outcomes = completed %d / interrupted %d / errored %d, want 1/1/1",
			st.Completed, st.Interrupted, st.Errored)
	}
	if st.WithSubagents != 1 {
		t.Errorf("WithSubagents = %d, want 1", st.WithSubagents)
	}
	if st.TotalTurns != 6 {
		t.Errorf("TotalTurns = %d, want 6 (3+1+2, standalone excluded)", st.TotalTurns)
	}
	// Tokens: sess-done 45, sess-cut 4, sess-err 6 → 55; standalone (198) excluded.
	if st.TotalTokens != 55 {
		t.Errorf("TotalTokens = %v, want 55", st.TotalTokens)
	}
	// Durations (seconds): sess-done 120, sess-cut 0, sess-err 60.
	// Sorted: [0, 60, 120]. p50 nearest-rank rank=ceil(.5*3)=2 → 60; p95 → 120.
	if st.DurationP50 != 60 || st.DurationP95 != 120 {
		t.Errorf("duration p50/p95 = %d/%d, want 60/120", st.DurationP50, st.DurationP95)
	}
	// Turns sorted [1,2,3]: p50 rank=2 → 2; p95 rank=3 → 3.
	if st.TurnsP50 != 2 || st.TurnsP95 != 3 {
		t.Errorf("turns p50/p95 = %d/%d, want 2/3", st.TurnsP50, st.TurnsP95)
	}
	if st.AvgTurns != 2.0 {
		t.Errorf("AvgTurns = %v, want 2", st.AvgTurns)
	}
}

// TestSessionStatsWindow checks the [since, until) window filters calls before
// aggregating, and that an empty window yields zeroed stats.
func TestSessionStatsWindow(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	for i, e := range []calls.Entry{
		{TS: base.Add(0 * time.Minute), Model: "m", Vendor: "v", Status: 200, SessionID: "a"},
		{TS: base.Add(10 * time.Minute), Model: "m", Vendor: "v", Status: 200, SessionID: "b"},
	} {
		if _, err := s.AppendCall(e); err != nil {
			t.Fatalf("AppendCall[%d]: %v", i, err)
		}
	}

	// Window covering only the first call.
	since := base.Add(-1 * time.Minute)
	until := base.Add(5 * time.Minute)
	st, err := s.SessionStats(&since, &until)
	if err != nil {
		t.Fatalf("SessionStats: %v", err)
	}
	if st.Sessions != 1 {
		t.Errorf("windowed Sessions = %d, want 1", st.Sessions)
	}

	// Empty window → no sessions, no panic on percentiles.
	emptySince := base.Add(100 * time.Hour)
	emptyUntil := base.Add(200 * time.Hour)
	st, err = s.SessionStats(&emptySince, &emptyUntil)
	if err != nil {
		t.Fatalf("SessionStats(empty): %v", err)
	}
	if st.Sessions != 0 || st.TurnsP50 != 0 || st.AvgTurns != 0 {
		t.Errorf("empty window stats = %+v, want zeroed", st)
	}
}

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
)

func TestCompositionLookupAndSessionAggregate(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)

	id1, err := s.AppendCall(calls.Entry{TS: base, SessionID: "sess", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall 1: %v", err)
	}
	id2, err := s.AppendCall(calls.Entry{TS: base.Add(time.Minute), SessionID: "sess", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall 2: %v", err)
	}

	if _, err := s.GetComposition(id1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetComposition before save = %v, want ErrNotFound", err)
	}

	c1 := compose.Composition{
		Total:  100,
		Cached: 20,
		Sources: []compose.Source{
			{Key: "system", Tokens: 40, Cached: 20},
			{Key: "tool_results", Tokens: 60, Children: []compose.Producer{{Key: "read", Tokens: 60}}},
		},
	}
	c2 := compose.Composition{
		Total:  200,
		Cached: 50,
		Sources: []compose.Source{
			{Key: "system", Tokens: 80, Cached: 50},
			{Key: "tool_results", Tokens: 120, Children: []compose.Producer{{Key: "bash", Tokens: 70}, {Key: "read", Tokens: 50}}},
		},
	}
	if err := s.SaveComposition(id1, c1); err != nil {
		t.Fatalf("SaveComposition 1: %v", err)
	}
	if err := s.SaveComposition(id2, c2); err != nil {
		t.Fatalf("SaveComposition 2: %v", err)
	}

	got, err := s.GetComposition(id2)
	if err != nil {
		t.Fatalf("GetComposition: %v", err)
	}
	if got.Total != 200 || got.Cached != 50 || len(got.Sources) != 2 {
		t.Fatalf("composition = %+v, want second saved composition", got)
	}

	agg, err := s.AggregateSessionComposition("sess")
	if err != nil {
		t.Fatalf("AggregateSessionComposition: %v", err)
	}
	if agg.Requests != 2 || agg.AvgTotal != 150 {
		t.Fatalf("aggregate requests/avg = %d/%v, want 2/150", agg.Requests, agg.AvgTotal)
	}
	if len(agg.Sources) != 2 {
		t.Fatalf("aggregate sources len = %d, want 2", len(agg.Sources))
	}
	if agg.Sources[0].Key != "tool_results" || agg.Sources[0].Tokens != 180 {
		t.Fatalf("top source = %+v, want tool_results 180", agg.Sources[0])
	}
	if len(agg.Sources[0].Children) != 2 || agg.Sources[0].Children[0].Key != "read" || agg.Sources[0].Children[0].Tokens != 110 {
		t.Fatalf("children = %+v, want read 110 first", agg.Sources[0].Children)
	}
}

// TestSessionCompositionExcludesUtilityAndCarriesAgents checks that
// SessionComposition drops harness utility calls (keeping only main/legacy rows,
// per the accretion-metric invariant) while carrying each surviving row's agent
// ids so the handler can scope the context charts by agent.
func TestSessionCompositionExcludesUtilityAndCarriesAgents(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC)

	// One session with a main-loop turn (agent ""), a sub-agent turn (agent
	// "sub"), and a harness utility call — all decomposed.
	mainID, err := s.AppendCall(calls.Entry{TS: base, SessionID: "sess", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall main: %v", err)
	}
	subID, err := s.AppendCall(calls.Entry{TS: base.Add(time.Minute), SessionID: "sess", AgentID: "sub", ParentAgentID: "root", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall sub: %v", err)
	}
	utilID, err := s.AppendCall(calls.Entry{TS: base.Add(2 * time.Minute), SessionID: "sess", Entrypoint: calls.EntrypointUtility, Status: 200})
	if err != nil {
		t.Fatalf("AppendCall util: %v", err)
	}

	c := compose.Composition{Total: 100, Cached: 10, Sources: []compose.Source{{Key: "system", Tokens: 100, Cached: 10}}}
	for _, id := range []string{mainID, subID, utilID} {
		if err := s.SaveComposition(id, c); err != nil {
			t.Fatalf("SaveComposition %s: %v", id, err)
		}
	}

	rows, err := s.SessionComposition("sess")
	if err != nil {
		t.Fatalf("SessionComposition: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (utility excluded)", len(rows))
	}
	for _, r := range rows {
		if r.CallID == utilID {
			t.Fatalf("utility call %s leaked into SessionComposition", utilID)
		}
	}
	// Ordered by ts: main first, then sub-agent — and agent ids are carried.
	if rows[0].AgentID != "" {
		t.Errorf("rows[0] agent = %q, want main \"\"", rows[0].AgentID)
	}
	if rows[1].AgentID != "sub" || rows[1].ParentAgentID != "root" {
		t.Errorf("rows[1] agent/parent = %q/%q, want sub/root", rows[1].AgentID, rows[1].ParentAgentID)
	}

	// The whole-session aggregate is likewise utility-free (2 requests, not 3).
	agg, err := s.AggregateSessionComposition("sess")
	if err != nil {
		t.Fatalf("AggregateSessionComposition: %v", err)
	}
	if agg.Requests != 2 {
		t.Fatalf("aggregate requests = %d, want 2 (utility excluded)", agg.Requests)
	}
}

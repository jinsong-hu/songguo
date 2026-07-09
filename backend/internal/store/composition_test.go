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

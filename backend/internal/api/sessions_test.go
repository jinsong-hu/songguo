package api

import (
	"testing"

	"github.com/songguo/songguo/internal/calls"
)

// TestBuildAgentTree checks calls fold into a main-loop→subagent forest with
// subtree rollups. Root "a" runs 2 calls and spawns child "b" (1 call); "c" is a
// second root whose parent is absent from the session.
func TestBuildAgentTree(t *testing.T) {
	entries := []calls.Entry{
		{AgentID: "a", ParentAgentID: "", Cost: 1, InputTokens: 10},
		{AgentID: "b", ParentAgentID: "a", Cost: 2, InputTokens: 20},
		{AgentID: "a", ParentAgentID: "", Cost: 1, InputTokens: 10},
		{AgentID: "c", ParentAgentID: "ghost", Cost: 4, InputTokens: 40},
	}

	roots := buildAgentTree(entries)
	if len(roots) != 2 {
		t.Fatalf("roots = %d, want 2 (a, c)", len(roots))
	}

	a := roots[0]
	if a.AgentID != "a" {
		t.Fatalf("roots[0] = %q, want a (first-seen order)", a.AgentID)
	}
	// Subtree: a's own 2 calls + b's 1 call = 3; cost 1+1+2 = 4.
	if a.Calls != 3 {
		t.Errorf("a subtree calls = %d, want 3", a.Calls)
	}
	if a.Cost != 4 {
		t.Errorf("a subtree cost = %v, want 4", a.Cost)
	}
	if len(a.Children) != 1 || a.Children[0].AgentID != "b" {
		t.Fatalf("a children = %+v, want [b]", a.Children)
	}
	if a.Children[0].Calls != 1 {
		t.Errorf("b calls = %d, want 1", a.Children[0].Calls)
	}

	c := roots[1]
	if c.AgentID != "c" || c.Calls != 1 || len(c.Children) != 0 {
		t.Errorf("c node = %+v, want lone root c with 1 call", c)
	}
}

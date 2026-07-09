package api

import (
	"testing"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
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

func TestTitleFromPayload(t *testing.T) {
	p := store.Payload{
		ReqBody: []byte(`{
			"messages":[{"role":"user","content":[{"type":"text","text":"<session>\nPlease polish the session detail page.\n</session>"}]}],
			"system":[{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session. Return JSON with a single \"title\" field."}],
			"tools":[],
			"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}}
		}`),
		RespBody: []byte("event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"{\\\"title\\\": \\\"Session\"}}\n\n" +
			"event: content_block_delta\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" detail polish\\\"}\"}}\n\n"),
	}
	if got := titleFromPayload(p); got != "Session detail polish" {
		t.Fatalf("titleFromPayload = %q, want Session detail polish", got)
	}

	p.ReqBody = []byte(`{
		"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: Grafana generic_oauth 飞书 Feishu"}]}],
		"system":[{"type":"text","text":"You are an assistant for performing a web search tool use"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],
		"tool_choice":{"type":"tool","name":"web_search"}
	}`)
	if got := titleFromPayload(p); got != "" {
		t.Fatalf("tool payload title = %q, want empty", got)
	}
}

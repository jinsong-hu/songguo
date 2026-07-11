package compose

import "testing"

func TestToolTurnAnthropic(t *testing.T) {
	// Tail is a user turn of two tool_results; the assistant just before it
	// issued two tool_use blocks. Earlier history (an older tool round-trip) must
	// NOT be counted — only the tail turn.
	body := []byte(`{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "list the repo"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t0", "name": "Bash", "input": {"cmd": "ls"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t0", "content": "old output that should not be summed here"}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "reading files"},
				{"type": "tool_use", "id": "t1", "name": "Read", "input": {"path": "a.go"}},
				{"type": "tool_use", "id": "t2", "name": "Read", "input": {"path": "b.go"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "package a\nfunc A() {}"},
				{"type": "tool_result", "tool_use_id": "t2", "content": [{"type": "text", "text": "package b"}]}
			]}
		]
	}`)
	calls, tokens := ToolTurn("anthropic/messages", body)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (only the tail assistant turn)", calls)
	}
	if tokens <= 0 {
		t.Fatalf("tokens = %d, want > 0", tokens)
	}
}

func TestToolTurnOpenAIChat(t *testing.T) {
	body := []byte(`{
		"model": "gpt-x",
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "Read", "arguments": "{}"}},
				{"id": "c2", "type": "function", "function": {"name": "Bash", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "file contents here"},
			{"role": "tool", "tool_call_id": "c2", "content": "command output here"}
		]
	}`)
	calls, tokens := ToolTurn("openai/chat", body)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if tokens <= 0 {
		t.Fatalf("tokens = %d, want > 0", tokens)
	}
}

func TestToolTurnResponses(t *testing.T) {
	body := []byte(`{
		"model": "gpt-x",
		"input": [
			{"type": "message", "role": "user", "content": "go"},
			{"type": "function_call", "call_id": "c1", "name": "Read", "arguments": "{}"},
			{"type": "function_call", "call_id": "c2", "name": "Bash", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "c1", "output": "some file text"},
			{"type": "function_call_output", "call_id": "c2", "output": "some command text"}
		]
	}`)
	calls, tokens := ToolTurn("openai/responses", body)
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if tokens <= 0 {
		t.Fatalf("tokens = %d, want > 0", tokens)
	}
}

func TestToolTurnTailIsPlainText(t *testing.T) {
	// A human follow-up (no tool results at the tail) is not a completed tool
	// round-trip: 0/0, so it contributes nothing to the session sum even though
	// an earlier turn used tools.
	body := []byte(`{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t0", "name": "Bash", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t0", "content": "output"}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "done"}]},
			{"role": "user", "content": "now do something else"}
		]
	}`)
	calls, tokens := ToolTurn("anthropic/messages", body)
	if calls != 0 || tokens != 0 {
		t.Fatalf("got (%d, %d), want (0, 0) for a plain-text tail", calls, tokens)
	}
}

func TestToolTurnNoToolsAtAll(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if calls, tokens := ToolTurn("anthropic/messages", body); calls != 0 || tokens != 0 {
		t.Fatalf("got (%d, %d), want (0, 0)", calls, tokens)
	}
}

func TestToolTurnUnknownWireAndGarbage(t *testing.T) {
	if calls, tokens := ToolTurn("image/generations", []byte(`{}`)); calls != 0 || tokens != 0 {
		t.Fatalf("unknown wire: got (%d, %d), want (0, 0)", calls, tokens)
	}
	if calls, tokens := ToolTurn("anthropic/messages", []byte(`not json`)); calls != 0 || tokens != 0 {
		t.Fatalf("garbage body: got (%d, %d), want (0, 0)", calls, tokens)
	}
}

// TestToolTurnDisjointAcrossSession is the property the whole design rests on:
// replaying a session request-by-request and summing each call's ToolTurn must
// total the run's tool invocations exactly once each — never the cumulative
// history. Two tool turns here (2 calls, then 1 call) must sum to 3, not more.
func TestToolTurnDisjointAcrossSession(t *testing.T) {
	reqAfterTurn1 := []byte(`{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "Read", "input": {}},
				{"type": "tool_use", "id": "t2", "name": "Read", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "aaa"},
				{"type": "tool_result", "tool_use_id": "t2", "content": "bbb"}
			]}
		]
	}`)
	reqAfterTurn2 := []byte(`{
		"model": "claude-sonnet-5",
		"messages": [
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t1", "name": "Read", "input": {}},
				{"type": "tool_use", "id": "t2", "name": "Read", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "aaa"},
				{"type": "tool_result", "tool_use_id": "t2", "content": "bbb"}
			]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "t3", "name": "Bash", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t3", "content": "ccc"}
			]}
		]
	}`)
	c1, _ := ToolTurn("anthropic/messages", reqAfterTurn1)
	c2, _ := ToolTurn("anthropic/messages", reqAfterTurn2)
	if c1+c2 != 3 {
		t.Fatalf("session tool-call sum = %d (%d + %d), want 3 — cumulative history leaked in", c1+c2, c1, c2)
	}
}

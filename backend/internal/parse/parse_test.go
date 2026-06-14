package parse

import "testing"

func TestOpenAIChatJSON(t *testing.T) {
	req := `{"model":"gpt-4o","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"weather in SF?"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}]}`
	resp := `{"choices":[{"message":{"role":"assistant","content":"",
		"tool_calls":[{"id":"call_1","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
		"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":20,"completion_tokens":8,
		"prompt_tokens_details":{"cached_tokens":12},
		"completion_tokens_details":{"reasoning_tokens":3}}}`

	c, err := Parse(Input{Wire: "openai/chat", ReqBody: []byte(req), RespBody: []byte(resp)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.Format != "openai-chat" || c.Model != "gpt-4o" {
		t.Fatalf("format/model = %q/%q", c.Format, c.Model)
	}
	if c.System != "be brief" {
		t.Errorf("system = %q", c.System)
	}
	if len(c.Input) != 2 || c.Tools[0] != "get_weather" {
		t.Errorf("input/tools = %+v / %v", c.Input, c.Tools)
	}
	if c.FinishReason != "tool_calls" || c.ToolCallCount != 1 {
		t.Errorf("finish/count = %q/%d", c.FinishReason, c.ToolCallCount)
	}
	tc := c.Output[0].ToolCalls[0]
	if tc.Name != "get_weather" || tc.ID != "call_1" || tc.Arguments != `{"city":"SF"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if c.Tokens.Input != 20 || c.Tokens.Output != 8 || c.Tokens.CachedInput != 12 || c.Tokens.Reasoning != 3 {
		t.Errorf("tokens = %+v", c.Tokens)
	}
}

func TestOpenAIChatStreamReassembly(t *testing.T) {
	// Tool call streamed in fragments across chunks; usage on the final chunk.
	resp := `data: {"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"f","arguments":"{\"a\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}

data: [DONE]

`
	c, err := Parse(Input{Wire: "openai/chat", Stream: true, ReqBody: []byte(`{"model":"gpt-4o"}`), RespBody: []byte(resp)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	out := c.Output[0]
	if out.Text != "Hello" {
		t.Errorf("text = %q", out.Text)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "f" || out.ToolCalls[0].Arguments != `{"a":1}` {
		t.Errorf("tool calls = %+v", out.ToolCalls)
	}
	if c.FinishReason != "tool_calls" || c.Tokens.Input != 5 || c.Tokens.Output != 2 {
		t.Errorf("finish/tokens = %q / %+v", c.FinishReason, c.Tokens)
	}
}

func TestAnthropicJSON(t *testing.T) {
	req := `{"model":"claude-3","system":"sys","messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"tools":[{"name":"lookup"}]}`
	resp := `{"role":"assistant","stop_reason":"tool_use","content":[
		{"type":"text","text":"sure"},
		{"type":"tool_use","id":"tu_1","name":"lookup","input":{"q":"x"}}],
		"usage":{"input_tokens":11,"output_tokens":4,"cache_read_input_tokens":7,"cache_creation_input_tokens":2}}`

	c, err := Parse(Input{Wire: "anthropic/messages", ReqBody: []byte(req), RespBody: []byte(resp)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.Format != "anthropic-messages" || c.System != "sys" || c.Tools[0] != "lookup" {
		t.Errorf("format/system/tools = %q/%q/%v", c.Format, c.System, c.Tools)
	}
	if c.Output[0].Text != "sure" || c.ToolCallCount != 1 {
		t.Errorf("output = %+v", c.Output[0])
	}
	tc := c.Output[0].ToolCalls[0]
	if tc.Name != "lookup" || tc.Arguments != `{"q":"x"}` {
		t.Errorf("tool call = %+v", tc)
	}
	if c.FinishReason != "tool_use" || c.Tokens.Input != 11 || c.Tokens.Output != 4 ||
		c.Tokens.CachedInput != 7 || c.Tokens.CacheWrite != 2 {
		t.Errorf("finish/tokens = %q / %+v", c.FinishReason, c.Tokens)
	}
}

func TestAnthropicStreamReassembly(t *testing.T) {
	resp := `event: message_start
data: {"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":9,"cache_read_input_tokens":3}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"there"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu_2","name":"calc"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"n\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"2}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}

event: message_stop
data: {"type":"message_stop"}

`
	c, err := Parse(Input{Wire: "anthropic/messages", Stream: true, ReqBody: []byte(`{"model":"claude-3"}`), RespBody: []byte(resp)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	out := c.Output[0]
	if out.Text != "Hi there" {
		t.Errorf("text = %q", out.Text)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "calc" || out.ToolCalls[0].Arguments != `{"n":2}` {
		t.Errorf("tool calls = %+v", out.ToolCalls)
	}
	if c.FinishReason != "tool_use" || c.Tokens.Input != 9 || c.Tokens.CachedInput != 3 || c.Tokens.Output != 6 {
		t.Errorf("finish/tokens = %q / %+v", c.FinishReason, c.Tokens)
	}
}

func TestDispatchFallbackAndGeneric(t *testing.T) {
	// Unmatched wire + anthropic adapter routes to the anthropic parser.
	c, _ := Parse(Input{Adapter: "anthropic-compatible", ReqBody: []byte(`{"model":"m"}`),
		RespBody: []byte(`{"role":"assistant","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":1}}`)})
	if c.Format != "anthropic-messages" || c.Output[0].Text != "x" {
		t.Errorf("fallback dispatch = %+v", c)
	}

	// Image/unknown wire → generic, with any top-level usage captured.
	g, _ := Parse(Input{Wire: "openai/images", ReqBody: []byte(`{"model":"seedream"}`),
		RespBody: []byte(`{"data":[{}],"usage":{"input_tokens":3}}`)})
	if g.Format != "generic" || g.Model != "seedream" || g.Tokens.Input != 3 {
		t.Errorf("generic = %+v", g)
	}
}

func TestTruncatedBodyIsNonFatal(t *testing.T) {
	// A truncated JSON response must not panic; it returns an error + the
	// request-side view still parsed.
	c, err := Parse(Input{Wire: "openai/chat", ReqBody: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		RespBody: []byte(`{"choices":[{"message":{"rol`)})
	if err == nil {
		t.Errorf("expected non-fatal parse error on truncated body")
	}
	if c.Model != "gpt-4o" || len(c.Input) != 1 {
		t.Errorf("request side should still parse: %+v", c)
	}
}

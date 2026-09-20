package wire

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The reply decoders are display-only readers of a body we already stored, so
// what they owe is fidelity: the items they hand back must be the same shape
// the wire's REQUEST carries, and they must never state more than the body did.
func TestDecodeReply(t *testing.T) {
	tests := []struct {
		name     string
		wire     string
		streamed bool
		body     string
		// want is the decoded reply as a JSON array, compared structurally so
		// map key order cannot make the test flap.
		want string
	}{
		{
			name: "responses non-streamed returns the output array verbatim",
			wire: "openai/responses",
			body: `{
				"id":"resp_1","status":"completed",
				"output":[
					{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"weighing it"}]},
					{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"42"}]}
				],
				"usage":{"input_tokens":9,"output_tokens":3}
			}`,
			want: `[
				{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"weighing it"}]},
				{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"42"}]}
			]`,
		},
		{
			name:     "responses stream prefers the terminal event's whole output",
			wire:     "openai/responses",
			streamed: true,
			body: sse(
				`{"type":"response.created","response":{"id":"resp_1","output":[]}}`,
				`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
				`{"type":"response.output_text.delta","delta":"4"}`,
				`{"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"42"}]}}`,
				`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"42"}]}],"usage":{"output_tokens":3}}}`,
				`[DONE]`,
			),
			want: `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"42"}]}]`,
		},
		{
			name:     "responses stream that died early falls back to finished items",
			wire:     "openai/responses",
			streamed: true,
			body: sse(
				`{"type":"response.created","response":{"id":"resp_1","output":[]}}`,
				`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"checked"}]}}`,
				`{"type":"response.output_item.done","item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}`,
				`{"type":"response.output_text.delta","delta":" and then noth`,
			),
			want: `[
				{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"checked"}]},
				{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"partial"}]}
			]`,
		},
		{
			name: "responses body with no output has nothing to show",
			wire: "openai/responses",
			body: `{"id":"resp_1","status":"failed","error":{"message":"upstream gone"}}`,
			want: `null`,
		},
		{
			name: "anthropic non-streamed wraps content as an assistant turn",
			wire: "anthropic/messages",
			body: `{
				"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
				"content":[
					{"type":"thinking","thinking":"weighing it","signature":"sig"},
					{"type":"text","text":"42"}
				],
				"usage":{"input_tokens":9,"output_tokens":3}
			}`,
			want: `[{"role":"assistant","content":[
				{"type":"thinking","thinking":"weighing it","signature":"sig"},
				{"type":"text","text":"42"}
			]}]`,
		},
		{
			name:     "anthropic stream rebuilds interleaved thinking and tool_use blocks",
			wire:     "anthropic/messages",
			streamed: true,
			body: sse(
				`{"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[],"usage":{"input_tokens":9}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing "}}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"it"}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"/tmp/x\"}"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":14}}`,
				`{"type":"message_stop"}`,
			),
			want: `[{"role":"assistant","content":[
				{"type":"thinking","thinking":"weighing it","signature":"sig"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"/tmp/x"}}
			]}]`,
		},
		{
			name:     "anthropic stream keeps the template input when the tool arguments never finished",
			wire:     "anthropic/messages",
			streamed: true,
			body: sse(
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
			),
			want: `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}]`,
		},
		{
			name:     "anthropic stream that carried no content has nothing to show",
			wire:     "anthropic/messages",
			streamed: true,
			body:     sse(`{"type":"error","error":{"type":"overloaded_error","message":"upstream gone"}}`),
			want:     `null`,
		},
		{
			name: "a body that is not this wire's shape reads as nothing, not as an error",
			wire: "anthropic/messages",
			body: `<html>502 Bad Gateway</html>`,
			want: `null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, ok := Get(tt.wire)
			if !ok {
				t.Fatalf("wire %q is not registered", tt.wire)
			}
			if w.DecodeReply == nil {
				t.Fatalf("wire %q has no DecodeReply", tt.wire)
			}
			got := w.DecodeReply([]byte(tt.body), tt.streamed)
			assertJSONEqual(t, got, tt.want)
		})
	}
}

// The reply is shown beside the prompt, so its items must render through the
// same path as the request's own — which means arriving in the same vocabulary.
// A Responses reply is a bare output item; an Anthropic reply is a role-bearing
// message. Both are shapes the request side already carries.
func TestDecodeReplyItemsAreRequestShaped(t *testing.T) {
	responses, _ := Get("openai/responses")
	items := responses.DecodeReply([]byte(`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`), false)
	if len(items) != 1 {
		t.Fatalf("responses reply = %d items, want 1", len(items))
	}
	var item struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(items[0], &item); err != nil {
		t.Fatalf("responses item: %v", err)
	}
	if item.Type != "message" || item.Role != "assistant" {
		t.Errorf("responses item = type %q role %q; want message/assistant", item.Type, item.Role)
	}

	anthropic, _ := Get("anthropic/messages")
	items = anthropic.DecodeReply([]byte(`{"role":"assistant","content":[{"type":"text","text":"hi"}]}`), false)
	if len(items) != 1 {
		t.Fatalf("anthropic reply = %d items, want 1", len(items))
	}
	if err := json.Unmarshal(items[0], &item); err != nil {
		t.Fatalf("anthropic item: %v", err)
	}
	if item.Role != "assistant" {
		t.Errorf("anthropic item role = %q, want assistant", item.Role)
	}
}

// Wires that declare no reply shape say so by leaving the field nil, so a
// caller can tell "this protocol has no turn to show" from "this body was
// empty" without parsing anything.
func TestWiresWithoutAReplyShapeDeclareNone(t *testing.T) {
	for _, name := range []string{"anthropic/count_tokens", "openai/models"} {
		w, ok := Get(name)
		if !ok {
			t.Fatalf("wire %q is not registered", name)
		}
		if w.DecodeReply != nil {
			t.Errorf("wire %q declares DecodeReply; want none", name)
		}
	}
}

// sse joins payloads into the stored body of an event stream.
func sse(payloads ...string) string {
	out := ""
	for _, p := range payloads {
		out += "data: " + p + "\n\n"
	}
	return out
}

func assertJSONEqual(t *testing.T, got []json.RawMessage, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal decoded reply: %v", err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatalf("decode got: %v", err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("reply =\n%s\nwant\n%s", encoded, want)
	}
}

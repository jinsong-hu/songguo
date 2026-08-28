package parse

import (
	"bytes"
	"testing"
)

// The Codex/Responses shape: a mix of plain messages and the typed items the API
// layers on top. Every one must land in Input, because Count is compared against
// the array's length across turns.
const responsesTurn1 = `{"model":"gpt-5","instructions":"be brief","input":[
	{"type":"additional_tools","role":"developer","tools":[{"name":"shell"}]},
	{"role":"user","content":"one"},
	{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
	{"type":"function_call_output","call_id":"c1","output":"ok"}
]}`

// Turn 2 is turn 1 plus one more message — the shape a coding agent produces on
// every turn, and the one the cover has to recognize as a superset.
const responsesTurn2 = `{"model":"gpt-5","instructions":"be brief","input":[
	{"type":"additional_tools","role":"developer","tools":[{"name":"shell"}]},
	{"role":"user","content":"one"},
	{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
	{"type":"function_call_output","call_id":"c1","output":"ok"},
	{"role":"user","content":"two"}
]}`

func responsesFingerprint(t *testing.T, body string) Fingerprint {
	t.Helper()
	c, err := Parse(Input{Wire: "openai/responses", ReqBody: []byte(body), RespBody: []byte(`{}`)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if c.Format != "openai-responses" {
		t.Fatalf("format = %q", c.Format)
	}
	return c.Fingerprint()
}

// The regression guard for the stall: a Responses request MUST yield a non-zero
// fingerprint. While it did not, SaveMessageFingerprint's `count <= 0` guard
// dropped every write, msg_count stayed 0, and messageCover — which treats
// unknown as never-redundant — selected every request in the session.
func TestResponsesRequestIsFingerprinted(t *testing.T) {
	f := responsesFingerprint(t, responsesTurn1)
	if f.Count == 0 || len(f.Head) == 0 || len(f.Tail) == 0 {
		t.Fatalf("zero fingerprint: %+v — the session cover will read every body", f)
	}
}

// One Message per input item, in order. A parser that folded the tool result
// into the call before it would report 3 here, and Count would stop tracking the
// array length that the cover compares turn to turn.
func TestResponsesInputIsPositional(t *testing.T) {
	if f := responsesFingerprint(t, responsesTurn1); f.Count != 4 {
		t.Fatalf("count = %d, want 4 (one per input item)", f.Count)
	}
	if f := responsesFingerprint(t, responsesTurn2); f.Count != 5 {
		t.Fatalf("count = %d, want 5", f.Count)
	}
}

// The property the whole cover rests on: an appended turn keeps Head and grows
// Count, so a run of turns collapses to its last member instead of opening a new
// run on every request.
func TestResponsesGrowingTurnExtendsPreviousTurn(t *testing.T) {
	f1 := responsesFingerprint(t, responsesTurn1)
	f2 := responsesFingerprint(t, responsesTurn2)

	if !bytes.Equal(f1.Head, f2.Head) {
		t.Fatal("head moved between turns; every request would open a new run")
	}
	if f2.Count < f1.Count {
		t.Fatalf("count shrank: %d -> %d", f1.Count, f2.Count)
	}
	if bytes.Equal(f1.Tail, f2.Tail) {
		t.Fatal("tail identical across different conversations")
	}
}

// Distinct items must not hash alike: equal hashes are what license the cover to
// DROP a body, so a collision is the one way this produces a wrong conversation
// rather than a wasted read. Items differing only in a field the normalizer
// skips are exactly that risk — hence the type/id fallback.
func TestResponsesDistinctItemsDoNotCollide(t *testing.T) {
	a := responsesFingerprint(t, `{"input":[
		{"type":"reasoning","id":"rs_1","summary":[]},
		{"role":"user","content":"go"}]}`)
	b := responsesFingerprint(t, `{"input":[
		{"type":"reasoning","id":"rs_2","summary":[]},
		{"role":"user","content":"go"}]}`)

	if a.Count != 2 || b.Count != 2 {
		t.Fatalf("counts = %d, %d; want 2, 2", a.Count, b.Count)
	}
	if bytes.Equal(a.Head, b.Head) {
		t.Fatal("distinct reasoning items produced the same head hash")
	}
}

// Two same-length arrays that diverge are a fork, not a duplicate. extends()
// only consults Tail when Count is equal, so this is the case it guards.
func TestResponsesSameLengthForkDiffersInTail(t *testing.T) {
	a := responsesFingerprint(t, `{"input":[{"role":"user","content":"one"},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"}]}`)
	b := responsesFingerprint(t, `{"input":[{"role":"user","content":"one"},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"rm\"}"}]}`)

	if !bytes.Equal(a.Head, b.Head) {
		t.Fatal("head differed; these forks share a first message")
	}
	if bytes.Equal(a.Tail, b.Tail) {
		t.Fatal("fork hashed as a duplicate; the cover would drop a distinct body")
	}
}

// The item types share field NAMES without sharing field TYPES: `arguments` is
// a JSON string on a function_call and a JSON object on a tool_search_call.
// A struct with `Arguments string` unmarshals the second one into an error and
// loses the whole item — type, id and all — which is how real Codex traffic
// (reasoning + tool_search_call + tool_search_output) first went through here.
func TestResponsesPolymorphicArgumentsDoNotLoseTheItem(t *testing.T) {
	c, err := Parse(Input{Wire: "openai/responses", RespBody: []byte(`{}`), ReqBody: []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"AAAA"},
		{"type":"tool_search_call","id":"tsc_1","call_id":"call_1","status":"completed",
		 "execution":"client","arguments":{"query":"a b c"}},
		{"type":"tool_search_output","id":"tso_1","call_id":"call_1","status":"completed",
		 "execution":"client","tools":[{"type":"namespace","name":"mcp__x"}]}
	]}`)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(c.Input) != 4 {
		t.Fatalf("input = %d items, want 4", len(c.Input))
	}
	// Every typed item must keep an identity. Falling through to the raw-bytes
	// escape hatch leaves Role empty, which is the symptom that exposed the bug.
	for i, want := range []string{"user", "reasoning", "tool_search_call", "tool_search_output"} {
		if c.Input[i].Role != want {
			t.Errorf("item %d role = %q, want %q", i, c.Input[i].Role, want)
		}
	}
	for i, want := range []string{"", "rs_1", "tsc_1", "tso_1"} {
		if c.Input[i].Name != want {
			t.Errorf("item %d name = %q, want %q", i, c.Input[i].Name, want)
		}
	}
}

// A function_call's string `arguments` must still read as the arguments, not as
// a re-marshalled JSON string — the polymorphism fix must not regress it.
func TestResponsesFunctionCallKeepsStringArguments(t *testing.T) {
	c, _ := Parse(Input{Wire: "openai/responses", RespBody: []byte(`{}`), ReqBody: []byte(`{"input":[
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"cmd\":\"ls\"}"}]}`)})
	if len(c.Input) != 1 || len(c.Input[0].ToolCalls) != 1 {
		t.Fatalf("input = %+v", c.Input)
	}
	tc := c.Input[0].ToolCalls[0]
	if tc.Name != "shell" || tc.ID != "c1" || tc.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("tool call = %+v", tc)
	}
}

// Two role-less, content-less items with no id at all are the last collision
// risk: nothing the normalizer reads distinguishes them, so it keeps their
// bytes. Equal hashes would let the cover drop a body that is not a duplicate.
func TestResponsesUnidentifiableItemsFallBackToRawBytes(t *testing.T) {
	a := responsesFingerprint(t, `{"input":[{"type":"reasoning","encrypted_content":"AAAA"}]}`)
	b := responsesFingerprint(t, `{"input":[{"type":"reasoning","encrypted_content":"BBBB"}]}`)
	if bytes.Equal(a.Head, b.Head) {
		t.Fatal("id-less items with different payloads hashed alike")
	}
}

// `input` also accepts a bare string prompt in place of the array.
func TestResponsesStringInput(t *testing.T) {
	f := responsesFingerprint(t, `{"model":"gpt-5","input":"just a prompt"}`)
	if f.Count != 1 {
		t.Fatalf("count = %d, want 1", f.Count)
	}
}

// A body with no input at all stays unknown rather than claiming an empty
// conversation — unknown is what makes the cover keep the body.
func TestResponsesMissingInputStaysUnknown(t *testing.T) {
	if f := responsesFingerprint(t, `{"model":"gpt-5"}`); f.Count != 0 {
		t.Fatalf("count = %d, want 0 for a body with no input", f.Count)
	}
}

// Truncated captures must not panic and must not fabricate a shape.
func TestResponsesTruncatedBodyIsNonFatal(t *testing.T) {
	c, _ := Parse(Input{
		Wire:     "openai/responses",
		ReqBody:  []byte(`{"model":"gpt-5","input":[{"role":"user","content":"on`),
		RespBody: []byte(`{}`),
	})
	if f := c.Fingerprint(); f.Count != 0 {
		t.Fatalf("count = %d, want 0 for a truncated body", f.Count)
	}
}

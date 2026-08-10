package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

func TestSessionMessagesMergesCapturedRequests(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)

	body1 := `{
		"model":"gpt-5",
		"instructions":"System prompt",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"input":[
			{"role":"user","content":"one"},
			{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}","cache_control":{"type":"ephemeral"}}
		]
	}`
	body2 := `{
		"model":"gpt-5",
		"instructions":"System prompt",
		"tools":[{"parameters":{"type":"object"},"name":"lookup","type":"function"}],
		"input":[
			{"content":"one","role":"user"},
			{"arguments":"{}","name":"lookup","call_id":"call-1","type":"function_call","cache_control":{"ttl":"1h"}},
			{"type":"function_call_output","call_id":"call-1","output":"result"}
		]
	}`
	body3 := `{
		"model":"gpt-5",
		"instructions":"System prompt",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"input":[
			{"role":"developer","content":"Compacted summary"},
			{"role":"user","content":"three"}
		]
	}`

	appendCaptured := func(ts time.Time, sessionID, wire, req string, headers map[string]string) string {
		t.Helper()
		id, err := s.AppendCall(calls.Entry{TS: ts, SessionID: sessionID, Wire: wire, Status: 200})
		if err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
		if err := s.SavePayload(store.Payload{
			CallID:     id,
			ReqHeaders: headers,
			ReqBody:    []byte(req),
			RespBody:   []byte("RESPONSE_MUST_NOT_BE_RETURNED"),
			CreatedAt:  ts,
		}); err != nil {
			t.Fatalf("SavePayload: %v", err)
		}
		return id
	}

	appendCaptured(base, "sess", "openai/responses", body1, nil)
	secondID, err := s.AppendCall(calls.Entry{TS: base.Add(time.Minute), SessionID: "sess", Wire: "openai/responses", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall compressed: %v", err)
	}
	if err := s.SavePayload(store.Payload{
		CallID:     secondID,
		ReqHeaders: map[string]string{"Content-Encoding": "gzip"},
		ReqBody:    gzipBytes(t, []byte(body2)),
		RespBody:   []byte("RESPONSE_MUST_NOT_BE_RETURNED"),
		CreatedAt:  base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("SavePayload compressed: %v", err)
	}
	appendCaptured(base.Add(2*time.Minute), "sess", "openai/responses", body3, nil)
	appendCaptured(base.Add(3*time.Minute), "sess", "anthropic/count_tokens", `{"messages":[{"role":"user","content":"COUNT_TOKEN_MUST_NOT_BE_RETURNED"}]}`, nil)
	appendCaptured(base.Add(4*time.Minute), "other", "openai/responses", `{"input":[{"role":"user","content":"OTHER_SESSION_MUST_NOT_BE_RETURNED"}]}`, nil)
	appendCaptured(base.Add(5*time.Minute), "sess", "openai/responses", `{not json`, nil)

	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})
	rec := do(h, http.MethodGet, "/api/sessions/sess/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session messages: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "MUST_NOT_BE_RETURNED") {
		t.Fatalf("session messages leaked excluded content: %s", rec.Body.String())
	}

	var view sessionMessagesView
	decodeBody(t, rec, &view)
	if view.SessionID != "sess" || view.Model != "gpt-5" {
		t.Fatalf("identity = %q/%q, want sess/gpt-5", view.SessionID, view.Model)
	}
	if len(view.System) != 1 || string(view.System[0]) != `"System prompt"` {
		t.Errorf("system = %s, want one unique prompt", mustMarshalJSON(t, view.System))
	}
	if len(view.Tools) != 1 {
		t.Errorf("tools len = %d, want 1", len(view.Tools))
	}
	if len(view.Messages) != 5 {
		t.Fatalf("messages len = %d, want 5", len(view.Messages))
	}

	var messages []map[string]any
	for _, raw := range view.Messages {
		var message map[string]any
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("decode message %s: %v", raw, err)
		}
		messages = append(messages, message)
	}
	if messages[0]["content"] != "one" || messages[2]["type"] != "function_call_output" {
		t.Errorf("cumulative merge = %+v, want original messages plus output delta", messages[:3])
	}
	if messages[3]["content"] != "Compacted summary" || messages[4]["content"] != "three" {
		t.Errorf("compacted continuation = %+v, want summary and final user message", messages[3:])
	}
}

// Codex's responses-lite shape declares neither instructions nor a top-level
// tools array: the schemas ride in an additional_tools item and the prompt in a
// run of developer messages at the head of input. Both belong in their own
// panel, not in the conversation.
func TestSessionMessagesHoistsInlinePromptMaterial(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 8, 10, 5, 31, 0, 0, time.UTC)

	preamble := `{"type":"additional_tools","role":"developer","tools":[
			{"type":"custom","name":"exec","description":"Run JavaScript"},
			{"type":"function","name":"wait","parameters":{"type":"object"}}
		]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"PREAMBLE ONE"}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"PREAMBLE TWO"}]}`

	appendCapturedRequest(t, s, base, "lite", "openai/responses", `{
		"model":"gpt-5.6-sol",
		"input":[`+preamble+`,
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)
	appendCapturedRequest(t, s, base.Add(time.Minute), "lite", "openai/responses", `{
		"model":"gpt-5.6-sol",
		"input":[`+preamble+`,
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}
		]
	}`)

	view := sessionMessages(t, s, "lite")

	if len(view.System) != 2 {
		t.Fatalf("system = %s, want the two developer blocks once each", mustMarshalJSON(t, view.System))
	}
	if !strings.Contains(string(view.System[0]), "PREAMBLE ONE") ||
		!strings.Contains(string(view.System[1]), "PREAMBLE TWO") {
		t.Errorf("system = %s, want the preamble in order", mustMarshalJSON(t, view.System))
	}
	if got := promptToolNames(t, view.Tools); len(got) != 2 || got[0] != "exec" || got[1] != "wait" {
		t.Errorf("tools = %v, want [exec wait]", got)
	}
	if len(view.Messages) != 2 {
		t.Fatalf("messages = %s, want the user turn and the assistant reply", mustMarshalJSON(t, view.Messages))
	}
	for _, raw := range view.Messages {
		if strings.Contains(string(raw), "PREAMBLE") || strings.Contains(string(raw), "additional_tools") {
			t.Errorf("message still carries hoisted material: %s", raw)
		}
	}
}

// A developer item past the head of the array is per-turn context or a
// compaction summary, not the preamble — it stays conversation.
func TestSessionMessagesKeepsDeveloperItemsPastTheHead(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 8, 10, 5, 31, 0, 0, time.UTC)

	appendCapturedRequest(t, s, base, "mid", "openai/responses", `{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","role":"developer","content":"PREAMBLE"},
			{"type":"message","role":"user","content":"one"},
			{"type":"message","role":"developer","content":"MID-TURN CONTEXT"}
		]
	}`)

	view := sessionMessages(t, s, "mid")

	if len(view.System) != 1 || !strings.Contains(string(view.System[0]), "PREAMBLE") {
		t.Fatalf("system = %s, want the leading run only", mustMarshalJSON(t, view.System))
	}
	if len(view.Messages) != 2 {
		t.Fatalf("messages = %s, want the user turn and the mid-turn developer item", mustMarshalJSON(t, view.Messages))
	}
	if !strings.Contains(string(view.Messages[1]), "MID-TURN CONTEXT") {
		t.Errorf("messages[1] = %s, want the mid-turn developer item", view.Messages[1])
	}
}

// Chat Completions has no top-level instruction field at all: its system prompt
// is always the leading system message.
func TestSessionMessagesHoistsChatSystemMessage(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 8, 10, 5, 31, 0, 0, time.UTC)

	appendCapturedRequest(t, s, base, "chat", "openai/chat", `{
		"model":"gpt-5",
		"messages":[
			{"role":"system","content":"CHAT SYSTEM"},
			{"role":"user","content":"one"}
		]
	}`)

	view := sessionMessages(t, s, "chat")

	if len(view.System) != 1 || string(view.System[0]) != `"CHAT SYSTEM"` {
		t.Fatalf("system = %s, want the leading system message", mustMarshalJSON(t, view.System))
	}
	if len(view.Messages) != 1 || !strings.Contains(string(view.Messages[0]), "one") {
		t.Errorf("messages = %s, want the user turn alone", mustMarshalJSON(t, view.Messages))
	}
}

// The hoist is decided for the whole session, not per request: one request that
// states its instructions out of band settles it for every request, so a
// session never hands the overlap merge two shapes of the same conversation.
func TestSessionMessagesInlineHoistIsDecidedPerSession(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 8, 10, 5, 31, 0, 0, time.UTC)

	appendCapturedRequest(t, s, base, "mixed", "openai/responses", `{
		"model":"gpt-5",
		"instructions":"Top-level prompt",
		"input":[
			{"type":"message","role":"developer","content":"Compacted summary"},
			{"type":"message","role":"user","content":"one"}
		]
	}`)
	appendCapturedRequest(t, s, base.Add(time.Minute), "mixed", "openai/responses", `{
		"model":"gpt-5",
		"input":[
			{"type":"message","role":"developer","content":"Compacted summary"},
			{"type":"message","role":"user","content":"one"},
			{"type":"message","role":"assistant","content":"two"}
		]
	}`)

	view := sessionMessages(t, s, "mixed")

	if len(view.System) != 1 || string(view.System[0]) != `"Top-level prompt"` {
		t.Fatalf("system = %s, want the top-level prompt alone", mustMarshalJSON(t, view.System))
	}
	if len(view.Messages) != 3 {
		t.Fatalf("messages = %s, want the summary merged once ahead of both turns", mustMarshalJSON(t, view.Messages))
	}
	if !strings.Contains(string(view.Messages[0]), "Compacted summary") {
		t.Errorf("messages[0] = %s, want the summary kept as conversation", view.Messages[0])
	}
}

func appendCapturedRequest(t *testing.T, s *store.Store, ts time.Time, sessionID, wire, body string) {
	t.Helper()
	id, err := s.AppendCall(calls.Entry{TS: ts, SessionID: sessionID, Wire: wire, Status: 200})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	if err := s.SavePayload(store.Payload{CallID: id, ReqBody: []byte(body), CreatedAt: ts}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}
}

func sessionMessages(t *testing.T, s *store.Store, sessionID string) sessionMessagesView {
	t.Helper()
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})
	rec := do(h, http.MethodGet, "/api/sessions/"+sessionID+"/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session messages: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var view sessionMessagesView
	decodeBody(t, rec, &view)
	return view
}

func promptToolNames(t *testing.T, tools []json.RawMessage) []string {
	t.Helper()
	out := make([]string, 0, len(tools))
	for _, raw := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			t.Fatalf("decode tool %s: %v", raw, err)
		}
		out = append(out, tool.Name)
	}
	return out
}

func TestSessionMessagesEmptySessionUsesArrays(t *testing.T) {
	s := newTestStore(t)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})
	rec := do(h, http.MethodGet, "/api/sessions/missing/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session messages: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"system":[]`) ||
		!strings.Contains(rec.Body.String(), `"tools":[]`) ||
		!strings.Contains(rec.Body.String(), `"messages":[]`) {
		t.Errorf("empty response should use arrays: %s", rec.Body.String())
	}
}

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(body)
}

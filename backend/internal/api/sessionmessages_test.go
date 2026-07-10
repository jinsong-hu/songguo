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

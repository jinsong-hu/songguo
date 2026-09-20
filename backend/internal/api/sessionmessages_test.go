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
	"github.com/songguo/songguo/internal/pressure"
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

// Reading captured bodies is our own bookkeeping, on the disk the ledger is
// short of, so the Messages view stands down while the gateway sheds load.
func TestSessionMessagesStandsDownUnderPressure(t *testing.T) {
	s := newTestStore(t)
	appendCapturedRequest(t, s, time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC), "sess", "openai/responses", `{"input":"hi"}`)
	level := pressure.ShedCapture
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", PressureStats: func() pressure.Stats {
		return pressure.Stats{Level: level.String()}
	}})

	rec := do(h, http.MethodGet, "/api/sessions/sess/messages", "secret", nil)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "songguo_shedding_load") {
		t.Fatalf("under pressure: code = %d, body = %s; want 503 shedding_load", rec.Code, rec.Body.String())
	}

	level = pressure.Normal
	rec = do(h, http.MethodGet, "/api/sessions/sess/messages", "secret", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"omitted_requests":0`) {
		t.Fatalf("recovered: code = %d, body = %s; want 200 with nothing omitted", rec.Code, rec.Body.String())
	}
}

// The title fallback probes sizes and fetches only small candidates, so a
// session whose title request was never captured does not read its
// conversation looking for one. The large body below IS a title request; if the
// fallback read it, the title would come back.
func TestSessionTitleFallbackSkipsLargeBodies(t *testing.T) {
	s := newTestStore(t)
	titleReq := `{"system":[{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session."}],` +
		`"messages":[{"role":"user","content":"x"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}}}`
	titleResp := `{"content":[{"type":"text","text":"{\"title\": \"Found the title\"}"}]}`
	add := func(ts time.Time, req string) calls.Entry {
		t.Helper()
		e := calls.Entry{TS: ts, SessionID: "t", Wire: "anthropic/messages", Status: 200}
		id, err := s.AppendCall(e)
		if err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
		if err := s.SavePayload(store.Payload{CallID: id, ReqBody: []byte(req), RespBody: []byte(titleResp)}); err != nil {
			t.Fatalf("SavePayload: %v", err)
		}
		e.ID = id
		return e
	}
	base := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	large := add(base, titleReq+strings.Repeat(" ", titleCandidateMaxBytes))

	a := newAPI(Deps{Store: s})
	if got := a.sessionTitleFromEntries("t", []calls.Entry{large}); got != "" {
		t.Fatalf("title = %q from a body over titleCandidateMaxBytes, want it skipped", got)
	}
	small := add(base.Add(time.Minute), titleReq)
	if got := a.sessionTitleFromEntries("t", []calls.Entry{large, small}); got != "Found the title" {
		t.Fatalf("title = %q, want the small title request's", got)
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

// The request page reads one call's own prompt: the same view as a session,
// but never merged with the session's other requests. It also reads that call's
// own reply — the half of the turn a session view must never carry.
func TestCallMessagesReadsOneCapturedRequest(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 14, 5, 33, 0, 0, time.UTC)

	// A real non-streamed Responses reply: a reasoning item carrying its text
	// in content[] with an empty summary, then the assistant message.
	reply := []byte(`{
		"id":"resp_1","status":"completed",
		"output":[
			{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"REPLY_REASONING"}]},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"REPLY_TEXT"}]}
		]
	}`)

	capture := func(ts time.Time, req []byte, headers map[string]string) string {
		t.Helper()
		id, err := s.AppendCall(calls.Entry{TS: ts, SessionID: "sess", Wire: "openai/responses", Status: 200})
		if err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
		if err := s.SavePayload(store.Payload{
			CallID:     id,
			ReqHeaders: headers,
			ReqBody:    req,
			RespBody:   reply,
			CreatedAt:  ts,
		}); err != nil {
			t.Fatalf("SavePayload: %v", err)
		}
		return id
	}

	capture(base, []byte(`{"model":"deepseek-flash","instructions":"System prompt","input":[{"role":"user","content":"EARLIER_REQUEST_MUST_NOT_BE_RETURNED"}]}`), nil)
	id := capture(base.Add(time.Minute), gzipBytes(t, []byte(`{
		"model":"deepseek-flash",
		"instructions":"System prompt",
		"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}],
		"input":[{"role":"user","content":"hello"},{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{}"}]
	}`)), map[string]string{"Content-Encoding": "gzip"})
	junk := capture(base.Add(2*time.Minute), []byte(`{not json`), nil)
	bare, err := s.AppendCall(calls.Entry{TS: base.Add(3 * time.Minute), SessionID: "sess", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall bare: %v", err)
	}

	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})
	rec := do(h, http.MethodGet, "/api/calls/"+id+"/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call messages: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "MUST_NOT_BE_RETURNED") {
		t.Fatalf("call messages leaked other content: %s", rec.Body.String())
	}
	var view sessionMessagesView
	decodeBody(t, rec, &view)
	if view.Model != "deepseek-flash" || len(view.System) != 1 || len(view.Tools) != 1 || len(view.Messages) != 2 {
		t.Fatalf("view = model %q, %d system, %d tools, %d messages; want deepseek-flash, 1, 1, 2",
			view.Model, len(view.System), len(view.Tools), len(view.Messages))
	}
	if len(view.Reply) != 2 {
		t.Fatalf("reply = %d items, want 2 (reasoning, message): %s", len(view.Reply), rec.Body.String())
	}
	for _, want := range []string{"REPLY_REASONING", "REPLY_TEXT"} {
		if !strings.Contains(string(view.Reply[0])+string(view.Reply[1]), want) {
			t.Errorf("reply is missing %s: %s", want, rec.Body.String())
		}
	}

	// A call whose request body is unreadable still has a readable response,
	// and the two are decoded independently.
	rec = do(h, http.MethodGet, "/api/calls/"+junk+"/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unparseable capture: code = %d, want 200 with an empty prompt", rec.Code)
	}
	var junkView sessionMessagesView
	decodeBody(t, rec, &junkView)
	if len(junkView.Messages) != 0 || len(junkView.Reply) != 2 {
		t.Errorf("unparseable request: %d messages, %d reply items; want 0 and 2", len(junkView.Messages), len(junkView.Reply))
	}

	if rec := do(h, http.MethodGet, "/api/calls/"+bare+"/messages", "secret", nil); rec.Code != http.StatusNotFound {
		t.Errorf("uncaptured call: code = %d, want 404", rec.Code)
	}
}

// A session view merges REQUESTS, and every request after the first already
// carries the previous turn's reply inside its own history. Decoding responses
// there too would render each assistant turn twice, so the reply is the one
// field the two paths must never agree on — and a call with no reply shape to
// read still answers with an array rather than a null.
func TestSessionMessagesCarriesNoReply(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 14, 5, 33, 0, 0, time.UTC)

	id, err := s.AppendCall(calls.Entry{TS: base, SessionID: "sess", Wire: "anthropic/messages", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	if err := s.SavePayload(store.Payload{
		CallID:    id,
		ReqBody:   []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`),
		RespBody:  []byte(`{"role":"assistant","content":[{"type":"text","text":"REPLY_TEXT"}]}`),
		CreatedAt: base,
	}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}

	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, http.MethodGet, "/api/sessions/sess/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session messages: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reply":[]`) {
		t.Errorf("session messages should carry an empty reply array: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "REPLY_TEXT") {
		t.Errorf("session messages leaked the response body: %s", rec.Body.String())
	}

	// The same call read one at a time does surface it.
	rec = do(h, http.MethodGet, "/api/calls/"+id+"/messages", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("call messages: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "REPLY_TEXT") {
		t.Errorf("call messages should carry the reply: %s", rec.Body.String())
	}
}

// Whether the reply came from a stream is recorded fact on the call row, not
// something to guess from the bytes: the same body read under the wrong flag
// decodes to nothing.
func TestCallMessagesReadsStreamFromTheCallRow(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 14, 5, 33, 0, 0, time.UTC)
	body := "data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"STREAMED_REPLY"}}` + "\n\n" +
		"data: " + `{"type":"message_stop"}` + "\n\n"

	capture := func(stream bool) string {
		t.Helper()
		id, err := s.AppendCall(calls.Entry{TS: base, SessionID: "sess", Wire: "anthropic/messages", Status: 200, Stream: stream})
		if err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
		if err := s.SavePayload(store.Payload{
			CallID:    id,
			ReqBody:   []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`),
			RespBody:  []byte(body),
			CreatedAt: base,
		}); err != nil {
			t.Fatalf("SavePayload: %v", err)
		}
		return id
	}

	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, http.MethodGet, "/api/calls/"+capture(true)+"/messages", "secret", nil)
	if !strings.Contains(rec.Body.String(), "STREAMED_REPLY") {
		t.Errorf("streamed call should decode its SSE reply: %s", rec.Body.String())
	}
	rec = do(h, http.MethodGet, "/api/calls/"+capture(false)+"/messages", "secret", nil)
	if !strings.Contains(rec.Body.String(), `"reply":[]`) {
		t.Errorf("an SSE body read as non-streamed has nothing to show: %s", rec.Body.String())
	}
}

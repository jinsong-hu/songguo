package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/pressure"
	"github.com/songguo/songguo/internal/store"
)

// capturedCall writes one call with a captured request and response and returns
// its id.
func capturedCall(t *testing.T, s *store.Store, wire, req, resp string) string {
	t.Helper()
	ts := time.Date(2026, 9, 22, 8, 0, 0, 0, time.UTC)
	id, err := s.AppendCall(calls.Entry{TS: ts, SessionID: "sess", Wire: wire, Status: 200})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	if err := s.SavePayload(store.Payload{
		CallID: id, ReqBody: []byte(req), RespBody: []byte(resp), CreatedAt: ts,
	}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}
	return id
}

// Every route that decodes a captured body stands down while the gateway is
// shedding — not just the session-wide one. A single call's capture is not
// automatically the smaller read: being a point lookup bounds the number of
// rows, and the disk is charged for bytes.
//
// > History: the gate was added for /sessions/{id}/messages on 2026-09-15 and
// > the three single-call readers were deliberately left out. Opening one call
// > detail page then read the same multi-MB row twice, concurrently, with
// > nothing able to stand either read down.
func TestCapturedBodyReadersStandDownUnderPressure(t *testing.T) {
	s := newTestStore(t)
	callID := capturedCall(t, s, "openai/responses", `{"model":"m","input":[{"role":"user","content":"hi"}]}`, `{"output":[]}`)
	sysOneID := capturedCall(t, s, "typesafe/systemone", `{}`, `{}`)

	level := pressure.ShedCapture
	h := testHandler(t, Deps{Store: s, AdminKey: "secret", PressureStats: func() pressure.Stats {
		return pressure.Stats{Level: level.String()}
	}})

	routes := []struct {
		name string
		path string
	}{
		{"session messages", "/api/sessions/sess/messages"},
		{"call messages", "/api/calls/" + callID + "/messages"},
		{"call trace", "/api/calls/" + callID + "/trace"},
		{"call systemone", "/api/calls/" + sysOneID + "/systemone"},
	}

	for _, route := range routes {
		rec := do(h, http.MethodGet, route.path, "secret", nil)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "songguo_shedding_load") {
			t.Errorf("%s while shedding: code = %d, body = %s; want 503 songguo_shedding_load",
				route.name, rec.Code, rec.Body.String())
		}
	}

	level = pressure.Normal
	for _, route := range routes {
		rec := do(h, http.MethodGet, route.path, "secret", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s recovered: code = %d, body = %s; want 200", route.name, rec.Code, rec.Body.String())
		}
	}
}

// The admission is a semaphore of one, so a few open tabs cannot stack up
// concurrent body reads on a disk that has one of them. A caller whose context
// is already done gives up its place in the queue rather than waiting.
func TestAdmitBodyReadAdmitsOneAtATime(t *testing.T) {
	a := newAPI(Deps{Store: newTestStore(t), AdminKey: "secret"})

	release, err := a.admitBodyRead(context.Background())
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.admitBodyRead(ctx); err == nil {
		t.Fatal("second admission was granted while the first was held")
	}

	release()
	release2, err := a.admitBodyRead(context.Background())
	if err != nil {
		t.Fatalf("admission after release: %v", err)
	}
	release2()
}

// A reader who navigated away must stop the gateway paying for the read. The
// handler answers nobody rather than spending a response on a dead connection.
func TestCapturedBodyReadStopsWhenTheViewerLeaves(t *testing.T) {
	s := newTestStore(t)
	callID := capturedCall(t, s, "openai/responses", `{"model":"m","input":[{"role":"user","content":"hi"}]}`, `{"output":[]}`)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/calls/"+callID+"/messages", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if body := rec.Body.String(); strings.Contains(body, "shedding_load") || strings.Contains(body, `"messages"`) {
		t.Fatalf("a departed viewer was answered: %s", body)
	}
}

// canonicalPromptJSONReference is the implementation canonicalPromptKey
// replaced: decode into a generic tree, strip cache_control by rebuilding it,
// and marshal the result. It is kept HERE, in the test, as the independent
// statement of what the key means — the fast implementation is checked against
// it rather than against itself.
func canonicalPromptJSONReference(raw json.RawMessage, stripCacheControl bool) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "invalid:" + string(raw)
	}
	if stripCacheControl {
		value = referenceWithoutCacheControl(value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "invalid:" + string(raw)
	}
	return string(encoded)
}

func referenceWithoutCacheControl(value any) any {
	switch value := value.(type) {
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = referenceWithoutCacheControl(child)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			if key != "cache_control" {
				out[key] = referenceWithoutCacheControl(child)
			}
		}
		return out
	default:
		return value
	}
}

// canonicalPromptKey is a digest rather than canonical text, so what has to
// hold is the EQUIVALENCE: two values share a key exactly when the reference
// implementation gives them the same canonical JSON. Anything else is a merge
// that either duplicates a message or swallows one.
func TestCanonicalPromptKeyMatchesCanonicalJSONEquivalence(t *testing.T) {
	corpus := []string{
		// Key order, which is the whole reason a canonical form exists.
		`{"role":"user","content":"hi"}`,
		`{"content":"hi","role":"user"}`,
		`{"role":"user","content":"HI"}`,
		// cache_control, stripped for messages and kept for system/tools.
		`{"role":"user","content":"hi","cache_control":{"type":"ephemeral"}}`,
		`{"role":"user","content":"hi","cache_control":{"ttl":"1h"}}`,
		// Nesting, and cache_control below the top level.
		`{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`,
		`{"role":"user","content":[{"text":"a","type":"text"}]}`,
		// Runs that must not be readable as one another.
		`["a","b"]`, `["ab"]`, `["a","b","c"]`, `[]`, `[[]]`, `[{}]`, `{}`,
		`{"a":{"b":1}}`, `{"a":{},"b":{}}`, `{"ab":1}`, `{"a":"b1"}`,
		// Scalars, including number literals that must stay distinct.
		`1`, `1.0`, `10`, `"1"`, `true`, `false`, `null`, `""`, `"null"`,
		`{"n":1}`, `{"n":1.0}`, `{"n":"1"}`,
		// An escape that decodes to the same string, and so must key the same.
		`"A"`, `"\u0041"`, `"B"`,
		// Duplicate keys: unmarshalling into a map keeps the last.
		`{"a":1,"a":2}`, `{"a":2}`, `{"a":1}`,
		// Not JSON at all, and empty.
		`{not json`, `{also not json`, ``,
	}

	for _, strip := range []bool{true, false} {
		for i, a := range corpus {
			for j, b := range corpus {
				sameRef := canonicalPromptJSONReference(json.RawMessage(a), strip) ==
					canonicalPromptJSONReference(json.RawMessage(b), strip)
				sameKey := canonicalPromptKey(json.RawMessage(a), strip) ==
					canonicalPromptKey(json.RawMessage(b), strip)
				if sameRef != sameKey {
					t.Errorf("strip=%v corpus[%d]=%s vs corpus[%d]=%s: reference says equal=%v, key says equal=%v",
						strip, i, a, j, b, sameRef, sameKey)
				}
			}
		}
	}
}

// The key exists only for mergePromptItems to compare one request's messages
// against another's, so a single-request view computes none — and must produce
// exactly the same view either way. This is the guard on that "must".
func TestSinglePromptViewDoesNotDependOnKeys(t *testing.T) {
	prompt := capturedPromptBody{
		Model:        "gpt-5",
		Instructions: json.RawMessage(`"System prompt"`),
		Tools:        json.RawMessage(`[{"type":"function","name":"lookup"},{"name":"lookup","type":"function"}]`),
		Input: json.RawMessage(`[
			{"role":"developer","content":"preamble"},
			{"role":"user","content":"one","cache_control":{"type":"ephemeral"}},
			{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"}
		]`),
	}

	withoutKeys := mergePrompts([]capturedPromptBody{prompt})

	// The same prompt twice takes the keyed path: the overlap merge sees a
	// complete overlap and contributes nothing the second time, so the messages
	// must match the single-prompt view exactly.
	withKeys := mergePrompts([]capturedPromptBody{prompt, prompt})

	if got, want := len(withoutKeys.Messages), len(withKeys.Messages); got != want {
		t.Fatalf("messages: single-prompt view has %d, keyed merge has %d", got, want)
	}
	for i := range withoutKeys.Messages {
		if !bytes.Equal(withoutKeys.Messages[i], withKeys.Messages[i]) {
			t.Errorf("message %d differs:\n keyless: %s\n  keyed: %s", i, withoutKeys.Messages[i], withKeys.Messages[i])
		}
	}
	if len(withoutKeys.System) != len(withKeys.System) || len(withoutKeys.Tools) != len(withKeys.Tools) {
		t.Errorf("system/tools: keyless = %d/%d, keyed = %d/%d",
			len(withoutKeys.System), len(withoutKeys.Tools), len(withKeys.System), len(withKeys.Tools))
	}
	// The duplicated tool above proves the system/tools dedup still runs on the
	// single-prompt path, which uses keys regardless of how many prompts there are.
	if len(withoutKeys.Tools) != 1 {
		t.Errorf("tools = %d, want 1 (the duplicate is dropped by key)", len(withoutKeys.Tools))
	}
}

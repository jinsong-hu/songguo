package api

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

// seedTracedCall inserts a call plus a captured (redacted) payload and returns
// the call id.
func seedTracedCall(t *testing.T, s *store.Store) int64 {
	t.Helper()
	id, err := s.AppendCall(calls.Entry{
		TS: time.Now(), UserID: "tokA", Model: "gpt-4o", Modality: calls.ModalityChat,
		Vendor: "openai", Status: 200, Cost: 0.10, LatencyMS: 100,
	})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	// Headers are already redacted by the proxy before storage; mimic that here
	// (no Authorization key present).
	p := store.Payload{
		CallID:          id,
		ReqHeaders:      map[string]string{"Content-Type": "application/json", "X-Songguo-Tags": "{}"},
		ReqBody:         []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}`),
		ReqContentType:  "application/json",
		RespHeaders:     map[string]string{"Content-Type": "application/json"},
		RespBody:        []byte(`{"id":"chatcmpl-1","usage":{"total_tokens":7}}`),
		RespContentType: "application/json",
		CreatedAt:       time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := s.SavePayload(p); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}
	return id
}

func TestCallTraceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := seedTracedCall(t, s)
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	target := "/api/calls/" + strconv.FormatInt(id, 10) + "/trace"

	// 401 without the admin key.
	rec := do(h, "GET", target, "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: code = %d, want 401", rec.Code)
	}

	// 200 with the key; shape + content.
	rec = do(h, "GET", target, "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var tv traceView
	decodeBody(t, rec, &tv)
	if tv.CallID != id {
		t.Errorf("call_id = %d, want %d", tv.CallID, id)
	}
	if !strings.Contains(tv.Request.Body, `"content":"ping"`) {
		t.Errorf("request body = %q, want the sent request", tv.Request.Body)
	}
	if !strings.Contains(tv.Response.Body, `"total_tokens":7`) {
		t.Errorf("response body = %q, want the mock response", tv.Response.Body)
	}
	if tv.Request.ContentType != "application/json" {
		t.Errorf("request content_type = %q", tv.Request.ContentType)
	}
	if _, err := time.Parse(time.RFC3339, tv.CapturedAt); err != nil {
		t.Errorf("captured_at not RFC3339: %q", tv.CapturedAt)
	}

	// Redaction: no Authorization header in the captured request, and the raw
	// bytes never contain it.
	if _, ok := tv.Request.Headers["Authorization"]; ok {
		t.Error("captured request leaked Authorization header")
	}
	if strings.Contains(rec.Body.String(), "Authorization") {
		t.Error("trace response contains an Authorization header")
	}
}

func TestCallTraceDisplaysGzipText(t *testing.T) {
	s := newTestStore(t)
	id, err := s.AppendCall(calls.Entry{
		TS: time.Now(), UserID: "tokA", Model: "gpt-5.5", Modality: calls.ModalityChat,
		Vendor: "openai", Status: 200,
	})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("data: {\"response\":{\"usage\":{\"input_tokens\":77}}}\n\n"))
	_ = zw.Close()
	if err := s.SavePayload(store.Payload{
		CallID:          id,
		ReqHeaders:      map[string]string{"Content-Type": "application/json"},
		ReqBody:         []byte(`{"model":"gpt-5.5"}`),
		ReqContentType:  "application/json",
		RespHeaders:     map[string]string{"Content-Type": "text/event-stream", "Content-Encoding": "gzip"},
		RespBody:        gz.Bytes(),
		RespContentType: "text/event-stream",
		CreatedAt:       time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("SavePayload: %v", err)
	}
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, "GET", "/api/calls/"+strconv.FormatInt(id, 10)+"/trace", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var tv traceView
	decodeBody(t, rec, &tv)
	if tv.Response.BodyBase64 {
		t.Fatalf("response body_base64 = true, want decoded text")
	}
	if !strings.Contains(tv.Response.Body, `"input_tokens":77`) {
		t.Errorf("response body = %q, want decoded SSE", tv.Response.Body)
	}
}

func TestCallTraceNotFound(t *testing.T) {
	s := newTestStore(t)
	// A call with no payload.
	id, err := s.AppendCall(calls.Entry{UserID: "x", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, "GET", "/api/calls/"+strconv.FormatInt(id, 10)+"/trace", "secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("uncaptured call trace: code = %d, want 404", rec.Code)
	}

	// Non-numeric / unknown id -> 404.
	rec = do(h, "GET", "/api/calls/99999/trace", "secret", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id trace: code = %d, want 404", rec.Code)
	}
}

func TestCallsListHasTrace(t *testing.T) {
	s := newTestStore(t)
	traced := seedTracedCall(t, s)
	// A second call WITHOUT a payload.
	untraced, err := s.AppendCall(calls.Entry{TS: time.Now(), UserID: "tokA", Model: "m", Vendor: "openai", Status: 200})
	if err != nil {
		t.Fatalf("AppendCall: %v", err)
	}
	h := testHandler(t, Deps{Store: s, AdminKey: "secret"})

	rec := do(h, "GET", "/api/calls", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("calls: code = %d", rec.Code)
	}
	var view callsView
	decodeBody(t, rec, &view)

	byID := map[int64]entryView{}
	for _, e := range view.Entries {
		byID[e.ID] = e
	}
	if !byID[traced].HasTrace {
		t.Errorf("call %d has_trace = false, want true", traced)
	}
	if byID[untraced].HasTrace {
		t.Errorf("call %d has_trace = true, want false", untraced)
	}
}

func TestSettingsExposeCapture(t *testing.T) {
	yaml := `
settings:
  listen: ":8080"
  capture: true
vendors:
  - name: openai
    origin: https://api.openai.com
    served_models: [gpt-4o]
    credential: {id: k1, api_key: sk-x}
    prices:
      gpt-4o: { input: 1, output: 1, unit: per_1m_tokens }
`
	snap := mustSnapshot(t, yaml)
	h := testHandler(t, Deps{AdminKey: "secret", Snapshot: func() *config.Snapshot { return snap }})

	rec := do(h, "GET", "/api/settings", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: code = %d", rec.Code)
	}
	var sv settingsView
	decodeBody(t, rec, &sv)
	if !sv.Capture {
		t.Error("capture = false, want true")
	}
}

func TestSettingsCaptureDefaults(t *testing.T) {
	// The default test config has no capture block: capture off.
	h := testHandler(t, Deps{AdminKey: "secret"})
	rec := do(h, "GET", "/api/settings", "secret", nil)
	var sv settingsView
	decodeBody(t, rec, &sv)
	if sv.Capture {
		t.Error("capture default should be false")
	}
}

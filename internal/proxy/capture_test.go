package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/store"
)

// captureYAML builds a one-vendor config with the global capture switch and
// caps set explicitly.
func captureYAML(baseURL string, capture bool, maxBytes, retain int) string {
	return fmt.Sprintf(`
settings:
  listen: ":8080"
  capture: %t
  capture_max_bytes: %d
  capture_retain: %d
vendors:
  - name: vendorA
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 1
    credentials:
      - id: credA
        api_key: vendor-secret-key
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, capture, maxBytes, retain, baseURL)
}

// --- capture ON: non-streaming round-trip with redaction ---

func TestCaptureNonStreaming(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL, true, 32768, 10000)), st)

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp := env.post(t, "/v1/chat/completions", key, reqBody)
	gotBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The client still gets the upstream body byte-for-byte (capture is invisible).
	wantBody := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	if string(gotBody) != wantBody {
		t.Errorf("client body altered by capture:\n got %q\nwant %q", gotBody, wantBody)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	callID := callIDForVendor(t, st, "vendorA")

	p, err := st.GetPayload(callID)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	if string(p.ReqBody) != reqBody {
		t.Errorf("captured req body = %q, want %q", p.ReqBody, reqBody)
	}
	if string(p.RespBody) != wantBody {
		t.Errorf("captured resp body = %q, want %q", p.RespBody, wantBody)
	}
	if p.ReqTruncated || p.RespTruncated {
		t.Errorf("unexpected truncation: req=%v resp=%v", p.ReqTruncated, p.RespTruncated)
	}
	// Redaction: the consumer Authorization header must be gone.
	if _, ok := p.ReqHeaders["Authorization"]; ok {
		t.Error("captured request leaked Authorization header")
	}
	if p.ReqContentType != "application/json" {
		t.Errorf("req content type = %q", p.ReqContentType)
	}
	if !strings.Contains(strings.ToLower(p.RespContentType), "application/json") {
		t.Errorf("resp content type = %q", p.RespContentType)
	}
}

// --- capture ON: streaming tees a capped buffer; client stream unaffected ---

func TestCaptureStreamingTruncates(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	// Tiny cap so the streamed body is truncated.
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL, true, 16, 10000)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	streamed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Client still receives the full SSE stream despite capture truncation.
	if !strings.Contains(string(streamed), `data: [DONE]`) {
		t.Errorf("client stream truncated; missing [DONE]:\n%s", streamed)
	}
	if !strings.Contains(string(streamed), `"content":"he"`) {
		t.Errorf("client stream missing first chunk:\n%s", streamed)
	}

	callID := callIDForVendor(t, st, "vendorA")
	p, err := st.GetPayload(callID)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	if !p.RespTruncated {
		t.Error("streamed resp should be marked truncated under a 16-byte cap")
	}
	if len(p.RespBody) > 16 {
		t.Errorf("captured stream = %d bytes, want <= 16 (capped)", len(p.RespBody))
	}
	if len(p.RespBody) == 0 {
		t.Error("expected some captured stream bytes")
	}
}

// --- capture OFF (global): no payload stored ---

func TestCaptureOffStoresNothing(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL, false, 32768, 10000)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()

	callID := callIDForVendor(t, st, "vendorA")
	if _, err := st.GetPayload(callID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no payload when capture off, got err %v", err)
	}
}

// --- per-token override beats the global setting (off->on and on->off) ---

func TestCaptureTokenOverride(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	// Global capture OFF, but a token opts IN via override.
	on := true
	_, key := mustToken(t, st, store.NewToken{Name: "on", Capture: &on})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL, false, 32768, 10000)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()

	callID := callIDForVendor(t, st, "vendorA")
	if _, err := st.GetPayload(callID); err != nil {
		t.Errorf("token override on should capture, got err %v", err)
	}
}

// --- failover: only the served (final) attempt gets a payload ---

func TestCaptureServedAttemptOnly(t *testing.T) {
	upA := &mockUpstream{forceStatus: 500}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

	yaml := fmt.Sprintf(`
settings:
  listen: ":8080"
  capture: true
vendors:
  - name: vendorA
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 1
    credentials:
      - id: credA
        api_key: keyA
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
  - name: vendorB
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 2
    credentials:
      - id: credB
        api_key: keyB
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mockA.URL, mockB.URL)

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()

	// Two call rows: vendorA (500, attempt 1) and vendorB (200, attempt 2).
	entries, err := st.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("call rows = %d, want 2", len(entries))
	}
	var served, failed int64
	for _, e := range entries {
		switch e.Vendor {
		case "vendorB":
			served = e.ID
		case "vendorA":
			failed = e.ID
		}
	}
	// The served (vendorB) attempt has a payload.
	if _, err := st.GetPayload(served); err != nil {
		t.Errorf("served attempt should have a payload, got %v", err)
	}
	// The failed-over (vendorA) attempt does NOT.
	if _, err := st.GetPayload(failed); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("failed-over attempt should have no payload, got %v", err)
	}
}

// callIDForVendor returns the id of the single call row for a vendor.
func callIDForVendor(t *testing.T, st *store.Store, vendor string) int64 {
	t.Helper()
	entries, err := st.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	for _, e := range entries {
		if e.Vendor == vendor {
			return e.ID
		}
	}
	t.Fatalf("no call row for vendor %q", vendor)
	return 0
}

package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/store"
)

// captureYAML builds a one-vendor config; capture is configured per user.
func captureYAML(baseURL string) string {
	return fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: vendor-secret-key}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, baseURL)
}

// --- capture ON: non-streaming round-trip with redaction ---

func TestCaptureNonStreaming(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", Capture: true})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL)), st)

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

// --- capture ON: streaming buffers the full stream; client stream unaffected ---

func TestCaptureStreaming(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", Capture: true})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	streamed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Client receives the full SSE stream.
	if !strings.Contains(string(streamed), `data: [DONE]`) {
		t.Errorf("client stream missing [DONE]:\n%s", streamed)
	}
	if !strings.Contains(string(streamed), `"content":"he"`) {
		t.Errorf("client stream missing first chunk:\n%s", streamed)
	}

	callID := callIDForVendor(t, st, "vendorA")
	p, err := st.GetPayload(callID)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	// The captured stream is the full body — same bytes the client received.
	if string(p.RespBody) != string(streamed) {
		t.Errorf("captured stream != client stream:\n got %q\nwant %q", p.RespBody, streamed)
	}
}

// --- capture OFF (user): no payload stored ---

func TestCaptureOffStoresNothing(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()

	callID := callIDForVendor(t, st, "vendorA")
	if _, err := st.GetPayload(callID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no payload when capture off, got err %v", err)
	}
}

func TestCodexSessionTitleRequiresCapture(t *testing.T) {
	for _, tt := range []struct {
		name    string
		capture bool
		want    string
	}{
		{name: "captured", capture: true, want: "Add persistent session titles to the recent activity"},
		{name: "not captured", capture: false, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			up := &mockUpstream{}
			mock := httptest.NewServer(up.handler())
			defer mock.Close()

			st := openStore(t)
			_, key := mustUser(t, st, store.NewUser{Name: "t", Capture: tt.capture})
			env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL)), st)

			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"Add persistent session titles to the recent activity table today"}]}`
			req, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/chat/completions", strings.NewReader(body))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Originator", "codex-tui")
			req.Header.Set("Session-Id", "codex-title-session")

			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatalf("client.Do: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			deadline := time.Now().Add(2 * time.Second)
			for {
				got, err := st.SessionTitle("codex-title-session")
				if err == nil && (got == tt.want || !tt.capture) {
					if got != tt.want {
						t.Fatalf("SessionTitle = %q, want %q", got, tt.want)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("SessionTitle did not settle: got %q, err %v, want %q", got, err, tt.want)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// --- a transport-failed attempt records a row but captures no payload ---

// With no failover, the only way an attempt lands without a forwarded response
// is a failure before the response exists (a transport/dial error). That still
// records a call row, but there is nothing to capture — GetPayload must miss.
// (A forwarded error status, e.g. a 500, IS captured: it is the served response.)
func TestCaptureNoPayloadOnTransportFailure(t *testing.T) {
	// A server we immediately close, so its origin refuses connections — the
	// single attempt fails at dial and never produces a response to forward.
	dead := httptest.NewServer((&mockUpstream{}).handler())
	deadURL := dead.URL
	dead.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: keyA}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, deadURL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", Capture: true})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (transport failure surfaced verbatim)", resp.StatusCode)
	}

	entries, err := st.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("call rows = %d, want 1", len(entries))
	}
	if _, err := st.GetPayload(entries[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("transport-failed attempt should have no payload, got %v", err)
	}
}

// --- a gateway-denied wire_unmatched 404 records a row AND captures the request ---

// A denied call never reaches an upstream, but it is still a request the operator
// wants to inspect (the signal a wire mapping is missing). With capture on, the
// 404 saves the request payload plus the synthesized error body.
func TestCaptureDeniedUnmatched(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", Capture: true})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL)), st)

	reqBody := `{"model":"gpt-4o","messages":[]}`
	// /v1/rerank matches no enabled wire (vendorA serves only openai/chat).
	resp := env.post(t, "/v1/rerank", key, reqBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (wire_unmatched)", resp.StatusCode)
	}

	if rows := env.callRows(t); len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	callID := callIDForVendor(t, st, "vendorA")
	p, err := st.GetPayload(callID)
	if err != nil {
		t.Fatalf("denied 404 should be captured: GetPayload: %v", err)
	}
	if string(p.ReqBody) != reqBody {
		t.Errorf("captured req body = %q, want %q", p.ReqBody, reqBody)
	}
	if !strings.Contains(string(p.RespBody), "wire_unmatched") {
		t.Errorf("captured resp body should carry the 404 error, got %q", p.RespBody)
	}
	if _, ok := p.ReqHeaders["Authorization"]; ok {
		t.Error("captured denied request leaked Authorization header")
	}
}

// With capture off, a denied 404 still records its row but saves no payload.
func TestCaptureOffDeniedNoPayload(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, captureYAML(mock.URL)), st)

	resp := env.post(t, "/v1/rerank", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	callID := callIDForVendor(t, st, "vendorA")
	if _, err := st.GetPayload(callID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected no payload when capture off, got %v", err)
	}
}

// callIDForVendor returns the id of the single call row for a vendor.
func callIDForVendor(t *testing.T, st *store.Store, vendor string) string {
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
	return ""
}

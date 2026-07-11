package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/server"
	"github.com/songguo/songguo/internal/store"
)

// mockUpstream is a configurable fake vendor used by the integration tests. It
// echoes the Authorization header it received, records the request body it saw,
// and can be told to fail (500/429) or stream.
type mockUpstream struct {
	mu sync.Mutex

	forceStatus int         // if non-zero, every request returns this status
	lastAuth    string      // Authorization header observed on the last request
	lastBody    []byte      // request body observed on the last request
	lastHeaders http.Header // all headers observed on the last request
	calls       int
}

func (m *mockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.lastAuth = r.Header.Get("Authorization")
		m.lastBody = body
		m.lastHeaders = r.Header.Clone()
		m.calls++
		forced := m.forceStatus
		m.mu.Unlock()

		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))

		if forced != 0 {
			w.WriteHeader(forced)
			_, _ = io.WriteString(w, `{"error":"forced"}`)
			return
		}

		// Streaming if the request body asked for it.
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)

		switch {
		case req.Stream:
			m.serveStream(w)
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":8,"total_tokens":8}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
		}
	}
}

func (m *mockUpstream) serveStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	chunks := []string{
		`data: {"id":"c","choices":[{"delta":{"content":"he"}}]}`,
		`data: {"id":"c","choices":[{"delta":{"content":"llo"}}]}`,
		`data: {"id":"c","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`,
		`data: [DONE]`,
	}
	for _, c := range chunks {
		_, _ = io.WriteString(w, c+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// snapshotFunc builds a config.Snapshot from YAML and returns a provider func.
func snapshotFunc(t *testing.T, yaml string) func() *config.Snapshot {
	t.Helper()
	snap, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	return func() *config.Snapshot { return snap }
}

// openStore opens a fresh store in a temp dir.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// testEnv bundles everything an integration test drives.
type testEnv struct {
	server *httptest.Server
	store  *store.Store
	client *http.Client
}

// post issues a POST to the proxy with the given path, token and body.
func (e *testEnv) post(t *testing.T, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

// postPinned is post with an X-Songguo-Provider pin header, constraining
// model-routed candidates to the provider whose credential id is providerID.
func (e *testEnv) postPinned(t *testing.T, path, token, providerID, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Songguo-Provider", providerID)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

// doPinned is do with an X-Songguo-Provider pin header, for model-less requests
// (e.g. async GET polls) that select their provider explicitly.
func (e *testEnv) doPinned(t *testing.T, method, path, token, providerID, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Songguo-Provider", providerID)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

func (e *testEnv) callRows(t *testing.T) []callRow {
	t.Helper()
	entries, err := e.store.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	out := make([]callRow, len(entries))
	for i, en := range entries {
		out[i] = callRow{
			Vendor:     en.Vendor,
			Status:     en.Status,
			Model:      en.Model,
			Cost:       en.Cost,
			Stream:     en.Stream,
			Usage:      en.Usage,
			Tags:       en.Tags,
			Wire:       en.Wire,
			Confidence: string(en.Confidence),
			Err:        en.Err,
		}
	}
	return out
}

type callRow struct {
	Vendor     string
	Status     int
	Model      string
	Cost       float64
	Stream     bool
	Usage      map[string]any
	Tags       map[string]string
	Wire       string
	Confidence string
	Err        string
}

func storeFilterAll() store.CallFilter { return store.CallFilter{Limit: 1000} }

// approxEqual compares two costs with a small tolerance, since costs round-trip
// through SQLite REAL and float arithmetic is not bit-exact.
func approxEqual(a, b float64) bool {
	const eps = 1e-12
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// newEnv wires a proxy handler over the given snapshot func and store, behind an
// httptest.Server, and returns a driver. The default *http.Client is used for
// the proxy's upstream calls so failover and streaming exercise real HTTP.
func newEnv(t *testing.T, snap func() *config.Snapshot, st *store.Store) *testEnv {
	t.Helper()
	h := NewHandler(Deps{
		Snapshot: snap,
		Store:    st,
		Router:   router.New(snap),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &testEnv{server: srv, store: st, client: srv.Client()}
}

func mustUser(t *testing.T, st *store.Store, nt store.NewUser) (store.User, string) {
	t.Helper()
	tok, key, err := st.CreateUser(nt)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return tok, key
}

// --- Test 1: chat happy path (transparency: body + usage + cost) ---

func TestChatHappyPath(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat, openai/completions, openai/embeddings, openai/models]
    credential: {id: credA, api_key: vendor-secret-key}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp := env.post(t, "/v1/chat/completions", key, reqBody)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	wantBody := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`
	if string(gotBody) != wantBody {
		t.Errorf("response body not byte-for-byte:\n got %q\nwant %q", gotBody, wantBody)
	}

	// Transparency: upstream saw the VENDOR key, not the Songguo token.
	if up.lastAuth != "Bearer vendor-secret-key" {
		t.Errorf("upstream Authorization = %q, want vendor key", up.lastAuth)
	}
	if up.lastAuth == "Bearer "+key {
		t.Errorf("upstream received the Songguo token; must be swapped")
	}
	// Transparency: request body forwarded UNCHANGED.
	if string(up.lastBody) != reqBody {
		t.Errorf("upstream body changed:\n got %q\nwant %q", up.lastBody, reqBody)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	r := rows[0]
	// cost = 2.50*10/1e6 + 10.00*20/1e6 = 0.000225
	wantCost := 2.50*10/1e6 + 10.00*20/1e6
	if !approxEqual(r.Cost, wantCost) {
		t.Errorf("cost = %v, want %v", r.Cost, wantCost)
	}
	if r.Vendor != "vendorA" || r.Status != 200 || r.Model != "gpt-4o" {
		t.Errorf("row = %+v", r)
	}
}

func TestStreamingGzipResponseMetersUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, "event: response.output_text.delta\n")
		_, _ = io.WriteString(zw, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(zw, "event: response.completed\n")
		_, _ = io.WriteString(zw, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":77,\"output_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":50}}}}\n\n")
		_ = zw.Close()
	}))
	defer up.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-5.5]
    priority: 1
    wires: [openai/responses]
    credential: {id: credA, api_key: vendor-secret-key}
    prices:
      gpt-5.5: { input: 1.00, output: 2.00, cached_input: 0.50, unit: per_1m_tokens }
`, up.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("content-encoding = %q, want gzip", resp.Header.Get("Content-Encoding"))
	}
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("client body was not forwarded as gzip: first bytes % x", body[:min(len(body), 4)])
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	if rows[0].Confidence != string(calls.ConfidenceMeasured) {
		t.Errorf("confidence = %q, want measured", rows[0].Confidence)
	}
	if got := rows[0].Usage["input_tokens"]; got != float64(77) {
		t.Errorf("usage input_tokens = %v, want 77", got)
	}
	if got := rows[0].Usage["output_tokens"]; got != float64(9) {
		t.Errorf("usage output_tokens = %v, want 9", got)
	}
	wantCost := 0.50*50/1e6 + 1.00*(77-50)/1e6 + 2.00*9/1e6
	if !approxEqual(rows[0].Cost, wantCost) {
		t.Errorf("cost = %v, want %v", rows[0].Cost, wantCost)
	}
}

// --- Test 1b: ingress key in X-Api-Key (drop-in swap for X-Api-Key SDKs) ---

// A client whose native SDK carries the key in X-Api-Key (Anthropic, ByteDance
// ASR/TTS) must authenticate by changing only the endpoint. The songguo key
// arrives in X-Api-Key with no Authorization header; the caller is still
// authenticated, and the client's key never leaks upstream.
func TestChatIngressXApiKey(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat, openai/completions, openai/embeddings, openai/models]
    credential: {id: credA, api_key: vendor-secret-key}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Client presents its songguo key in X-Api-Key ONLY — no Authorization.
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (X-Api-Key ingress must authenticate)", resp.StatusCode)
	}
	// Egress: openai vendor still gets the vendor key via Authorization, and the
	// client's songguo key is stripped from X-Api-Key so it never leaks upstream.
	if up.lastAuth != "Bearer vendor-secret-key" {
		t.Errorf("upstream Authorization = %q, want vendor key", up.lastAuth)
	}
	if got := up.lastHeaders.Get("X-Api-Key"); got != "" {
		t.Errorf("upstream X-Api-Key = %q, want empty (client key must not leak)", got)
	}
}

// --- Test 2: embeddings happy path ---

func TestEmbeddingsHappyPath(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: emb
    origin: %s/v1
    served_models: [text-embedding-3-small]
    priority: 1
    wires: [openai/embeddings, openai/models]
    credential: {id: credE, api_key: emb-key}
    prices:
      text-embedding-3-small: { input: 0.02, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/embeddings", key, `{"model":"text-embedding-3-small","input":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	wantCost := 0.02 * 8 / 1e6
	if !approxEqual(rows[0].Cost, wantCost) {
		t.Errorf("cost = %v, want %v", rows[0].Cost, wantCost)
	}
	if got := rows[0].Usage["prompt_tokens"]; got != float64(8) {
		t.Errorf("usage prompt_tokens = %v, want 8", got)
	}
}

// --- Test 3: invalid / missing token -> 401, no call row ---

func TestInvalidUser(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k")
	st := openStore(t)
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// Bogus token.
	resp := env.post(t, "/v1/chat/completions", "sg-does-not-exist", `{"model":"gpt-4o"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing token.
	resp2 := env.post(t, "/v1/chat/completions", "", `{"model":"gpt-4o"}`)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status (no token) = %d, want 401", resp2.StatusCode)
	}
	resp2.Body.Close()

	if rows := env.callRows(t); len(rows) != 0 {
		t.Fatalf("call rows = %d, want 0 (no upstream call on auth failure)", len(rows))
	}
	if up.calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls)
	}
}

// --- Test 4: out-of-scope model -> 403 ---

func TestOutOfScope(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", Scope: []string{"some-other-model"}})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if up.calls != 0 {
		t.Fatalf("upstream called despite scope rejection")
	}
}

// --- Test 5: budget exceeded on the second call -> 402 ---

func TestBudgetExceeded(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: k}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	// Budget tiny enough that one call's cost crosses it.
	budget := 0.0001
	_, key := mustUser(t, st, store.NewUser{Name: "t", Budget: &budget})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	// First call proceeds (coarse pre-check: spent 0 < budget).
	r1 := env.post(t, "/v1/chat/completions", key, body)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", r1.StatusCode)
	}
	r1.Body.Close()

	// Second call: spent (0.000225) >= budget (0.0001) -> 402.
	r2 := env.post(t, "/v1/chat/completions", key, body)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("second call status = %d, want 402", r2.StatusCode)
	}
}

// --- Test 6: rpm=1 -> second call 429 ---

func TestRateLimit(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", RPM: 1})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	body := `{"model":"gpt-4o","messages":[]}`
	r1 := env.post(t, "/v1/chat/completions", key, body)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", r1.StatusCode)
	}
	r1.Body.Close()

	r2 := env.post(t, "/v1/chat/completions", key, body)
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", r2.StatusCode)
	}
}

// --- Test 7: no per-call failover — A(500) is surfaced, B(healthy) untouched ---

// Even when a healthy lower-priority vendor is available, songguo forwards a
// single attempt and surfaces the top candidate's failure verbatim. It does NOT
// replay the request against the next vendor, and (today) does not auto-demote
// the failing vendor — routing is priority then weighted-RR only.
func TestNoPerCallFailover(t *testing.T) {
	upA := &mockUpstream{forceStatus: 500}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

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
  - name: vendorB
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 2
    wires: [openai/chat]
    credential: {id: credB, api_key: keyB}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mockA.URL, mockB.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (A's failure surfaced, no failover)", resp.StatusCode)
	}
	if upA.calls != 1 {
		t.Errorf("vendorA calls = %d, want 1", upA.calls)
	}
	if upB.calls != 0 {
		t.Errorf("vendorB calls = %d, want 0 (must not fail over)", upB.calls)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1 (single attempt)", len(rows))
	}
	if rows[0].Vendor != "vendorA" || rows[0].Status != 500 {
		t.Errorf("row = %+v, want vendorA status 500", rows[0])
	}
}

// --- Provider pin: X-Songguo-Provider constrains model-routed candidates ---

func TestProviderPin(t *testing.T) {
	upA := &mockUpstream{}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

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
  - name: vendorB
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credB, api_key: keyB}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mockA.URL, mockB.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// Pin to vendorB's provider (credential id credB): only B is tried, even
	// though both vendors share priority and serve the model.
	resp := env.postPinned(t, "/v1/chat/completions", key, "credB", `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if upB.calls != 1 {
		t.Errorf("vendorB calls = %d, want 1", upB.calls)
	}
	if upA.calls != 0 {
		t.Errorf("vendorA calls = %d, want 0 (pinned away)", upA.calls)
	}
}

func TestProviderPinUnknown(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// A pin that matches no serving provider is a 502, and never hits upstream.
	resp := env.postPinned(t, "/v1/chat/completions", key, "nonexistent", `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if up.calls != 0 {
		t.Errorf("upstream calls = %d, want 0 (unknown pin)", up.calls)
	}
}

// --- Test 8: all-fail single vendor -> upstream 500 passed through ---

func TestAllFailPassthrough(t *testing.T) {
	up := &mockUpstream{forceStatus: 500}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 passed through verbatim", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// Transparency: real upstream body forwarded, not a synthesized error.
	if string(body) != `{"error":"forced"}` {
		t.Errorf("body = %q, want upstream body verbatim", body)
	}
	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	if rows[0].Status != 500 {
		t.Errorf("row status = %d, want 500", rows[0].Status)
	}
}

// --- Test 9: no vendor for model -> 502 ---

func TestNoVendor(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k") // serves gpt-4o only
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"unknown-model"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if up.calls != 0 {
		t.Fatalf("upstream called for unrouteable model")
	}
}

// --- Test 10: streaming -> SSE bytes unchanged, call captures usage+cost ---

func TestStreaming(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: k}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var got bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		got.WriteString(sc.Text())
		got.WriteByte('\n')
	}
	wantContains := []string{
		`data: {"id":"c","choices":[{"delta":{"content":"he"}}]}`,
		`data: {"id":"c","choices":[{"delta":{"content":"llo"}}]}`,
		`data: [DONE]`,
	}
	for _, w := range wantContains {
		if !strings.Contains(got.String(), w) {
			t.Errorf("streamed output missing %q\n got:\n%s", w, got.String())
		}
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if !r.Stream {
		t.Errorf("call stream flag = false, want true")
	}
	if got := r.Usage["total_tokens"]; got != float64(30) {
		t.Errorf("streamed usage total_tokens = %v, want 30", got)
	}
	wantCost := 2.50*10/1e6 + 10.00*20/1e6
	if !approxEqual(r.Cost, wantCost) {
		t.Errorf("streamed cost = %v, want %v", r.Cost, wantCost)
	}
}

// singleVendorYAML builds a one-vendor config serving gpt-4o. The mock upstream
// is mounted at the host root, so we put /v1 in the origin and Mode-A's suffix
// (path minus /v1) lands the upstream call back on /chat/completions etc.
func singleVendorYAML(baseURL, vendor, credID, apiKey string) string {
	return fmt.Sprintf(`
vendors:
  - name: %s
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat, openai/completions, openai/embeddings, openai/models]
    credential: {id: %s, api_key: %s}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, vendor, baseURL, credID, apiKey)
}

// TestServerSmoke exercises the real server wiring (server.New with a mounted
// proxy handler) over a live loopback listener: /healthz must answer 200 and
// /v1/* must reach the proxy (401 without a token). This mirrors the binary's
// startup path while avoiding config.NewManager's fsnotify watcher, which is
// unrelated to proxying and may be unavailable in constrained sandboxes.
func TestServerSmoke(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	snap := snapshotFunc(t, singleVendorYAML(mock.URL, "vendorA", "credA", "k"))
	ph := NewHandler(Deps{Snapshot: snap, Store: st, Router: router.New(snap)})

	// Grab a free loopback port, then hand its address to the real server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := server.New(server.Options{Addr: addr, ProxyHandler: ph})
	go func() { _ = srv.Start() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	client := &http.Client{Timeout: 2 * time.Second}

	// Poll /healthz until the server is listening.
	var hresp *http.Response
	for i := 0; i < 100; i++ {
		hresp, err = client.Get("http://" + addr + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET /healthz never succeeded: %v", err)
	}
	defer hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", hresp.StatusCode)
	}

	// /v1 reaches the proxy: 401 without a token.
	preq, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	presp, err := client.Do(preq)
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1 unauthenticated status = %d, want 401", presp.StatusCode)
	}
}

// do issues an arbitrary-method request to the proxy with the given path, token
// and (optional) body.
func (e *testEnv) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.server.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	return resp
}

// --- Test 11: a non-/v1 vendor endpoint (e.g. Ark) is hit via its stored URL ---

func TestModelRoutedNonV1Prefix(t *testing.T) {
	// This mock ONLY serves /api/v3/chat/completions, mimicking 火山方舟/Ark whose
	// OpenAI-compatible base is …/api/v3. A /v1/chat/completions request must land
	// on /api/v3/chat/completions via the wire's stored full endpoint.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"wrong path"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: ark
    origin: %s
    served_models: [doubao-pro-32k]
    priority: 1
    endpoints:
      openai/chat: %s/api/v3/chat/completions
    credential: {id: arkKey, api_key: ark-secret}
    prices:
      doubao-pro-32k: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"doubao-pro-32k","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (request must reach /api/v3/chat/completions)", resp.StatusCode)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	wantCost := 2.50*10/1e6 + 10.00*20/1e6
	if !approxEqual(rows[0].Cost, wantCost) {
		t.Errorf("cost = %v, want %v", rows[0].Cost, wantCost)
	}
	if rows[0].Vendor != "ark" || rows[0].Status != 200 || rows[0].Model != "doubao-pro-32k" {
		t.Errorf("row = %+v", rows[0])
	}
}

// pathRecorder is a mock vendor host that records every request path it sees and
// serves a DashScope-style response with top-level usage.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
	auth  string
}

func (p *pathRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.auth = r.Header.Get("Authorization")
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// DashScope native shape: top-level usage with input/output_tokens.
		_, _ = io.WriteString(w, `{"output":{"text":"hi"},"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}`)
	}
}

// nativeYAML builds a one-vendor config for native-path tests. The native
// endpoints exercised here have no phase-1 wire, so the vendor opts into
// allow_unmatched: calls are forwarded to the vendor origin (scheme://host) with
// the inbound path verbatim, but metered zero at unknown confidence.
func nativeYAML(baseURL, vendor string) string {
	return fmt.Sprintf(`
vendors:
  - name: %s
    origin: %s
    served_models: [qwen-plus]
    priority: 1
    wires: [openai/chat]
    allow_unmatched: true
    credential: {id: %s-key, api_key: %s-secret}
    prices:
      qwen-plus: { input: 0.40, output: 1.20, unit: per_1m_tokens }
`, vendor, baseURL, vendor, vendor)
}

// --- Test 12: a native path is forwarded to origin + path (allow_unmatched) ---

func TestNativeUsage(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := nativeYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// Native generation endpoint with a model in the body: the model selects the
	// provider, no /x/ prefix and no header needed.
	body := `{"model":"qwen-plus","input":{"prompt":"hi"}}`
	resp := env.post(t, "/api/v1/services/aigc/text-generation/generation", key, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rec.mu.Lock()
	gotPaths := append([]string(nil), rec.paths...)
	gotAuth := rec.auth
	rec.mu.Unlock()

	// The native path is forwarded to the vendor origin verbatim.
	if len(gotPaths) != 1 || gotPaths[0] != "/api/v1/services/aigc/text-generation/generation" {
		t.Fatalf("upstream paths = %v, want [/api/v1/services/aigc/text-generation/generation]", gotPaths)
	}
	if gotAuth != "Bearer bailian-secret" {
		t.Errorf("upstream auth = %q, want the vendor key", gotAuth)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	// No wire matches the native path: the call is forwarded (allow_unmatched)
	// but metered zero at unknown confidence — budget integrity over guesswork.
	if rows[0].Cost != 0 {
		t.Errorf("cost = %v, want 0 (unmatched paths are not priced)", rows[0].Cost)
	}
	if rows[0].Wire != "" || rows[0].Confidence != string(calls.ConfidenceUnknown) {
		t.Errorf("wire/confidence = %q/%q, want \"\"/unknown", rows[0].Wire, rows[0].Confidence)
	}
	if rows[0].Vendor != "bailian" || rows[0].Status != 200 {
		t.Errorf("row = %+v", rows[0])
	}
}

// --- Test 13: a model-less GET resolves via the default provider, not 400 ---

func TestNativeModelLessGet(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := nativeYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// No body, no model, no header: an async task poll. With a single configured
	// provider it resolves as the default — never rejected as missing_model.
	resp := env.do(t, http.MethodGet, "/api/v1/tasks/abc", key, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (model-less GET must be forwarded)", resp.StatusCode)
	}

	rec.mu.Lock()
	gotPaths := append([]string(nil), rec.paths...)
	rec.mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != "/api/v1/tasks/abc" {
		t.Fatalf("upstream paths = %v, want [/api/v1/tasks/abc]", gotPaths)
	}

	rows := env.callRows(t)
	if len(rows) != 1 || rows[0].Status != 200 {
		t.Fatalf("call rows = %+v, want 1 with status 200", rows)
	}
}

// --- Test 13b: X-Songguo-Provider pins a model-less request to one provider ---

func TestNativeModelLessProviderPin(t *testing.T) {
	recA := &pathRecorder{}
	mockA := httptest.NewServer(recA.handler())
	defer mockA.Close()
	recB := &pathRecorder{}
	mockB := httptest.NewServer(recB.handler())
	defer mockB.Close()

	// Two providers both serving the same allow_unmatched native path. A pin must
	// send the call to exactly the named provider's origin.
	yaml := fmt.Sprintf(`
vendors:
  - name: bailian-a
    origin: %s
    served_models: [qwen-plus]
    priority: 1
    wires: [openai/chat]
    allow_unmatched: true
    credential: {id: provA, api_key: a-secret}
  - name: bailian-b
    origin: %s
    served_models: [qwen-plus]
    priority: 0
    wires: [openai/chat]
    allow_unmatched: true
    credential: {id: provB, api_key: b-secret}
`, mockA.URL, mockB.URL)
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.doPinned(t, http.MethodGet, "/api/v1/tasks/abc", key, "provA", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	recA.mu.Lock()
	aCalls := len(recA.paths)
	recA.mu.Unlock()
	recB.mu.Lock()
	bCalls := len(recB.paths)
	recB.mu.Unlock()
	if aCalls != 1 || bCalls != 0 {
		t.Fatalf("calls A=%d B=%d, want the pinned provider A only", aCalls, bCalls)
	}
}

// --- Test 13b2: an origin-only endpoint on a MATCHED wire is a transparent
// passthrough — it forwards the inbound path verbatim rather than POSTing to the
// host root. This lets one wire span several native suffixes; volc/asr-file
// (submit + query) is the canonical case. A path-bearing endpoint would instead
// rewrite to that fixed path (see TestModelRoutedNonV1Prefix). Regression: a
// bare-origin endpoint used to be used verbatim, so every suffix hit "/" and
// 404'd. ---

func TestOriginOnlyEndpointPassesThroughPath(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	// volc/asr-file is a model-less, multi-suffix wire whose endpoint is the bare
	// origin (scheme://host, no path), so submit and query each keep their path.
	yaml := fmt.Sprintf(`
vendors:
  - name: volc
    origin: %s
    adapter: volc-speech
    served_models: [doubao-seed-asr-2.0]
    priority: 1
    endpoints:
      volc/asr-file: %s
    credential: {id: volcKey, api_key: volc-secret}
`, mock.URL, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	for _, p := range []string{"/api/v3/auc/bigmodel/submit", "/api/v3/auc/bigmodel/query"} {
		resp := env.post(t, p, key, `{}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status = %d, want 200 (origin-only endpoint must forward, not 404 at root)", p, resp.StatusCode)
		}
	}

	rec.mu.Lock()
	got := append([]string(nil), rec.paths...)
	rec.mu.Unlock()
	want := []string{"/api/v3/auc/bigmodel/submit", "/api/v3/auc/bigmodel/query"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("upstream paths = %v, want %v (origin-only endpoint must keep the inbound path)", got, want)
	}
}

// --- Test 13c: a model-less POST carrying only X-Api-Resource-Id routes by
// endpoint, not by the resource id as if it were a model. Regression: the
// resource id (a billing class, e.g. volc.seedasr.auc) used to be assigned to
// res.Model and fed to Candidates(), which looked up byModel[<billing class>],
// found nothing, and returned 502 no_upstream. Routing is endpoint-first; the
// resource id is metering-only. ---

func TestNativeResourceIdRoutesByEndpoint(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := nativeYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// An ASR-style submit: no body model, a billing class in X-Api-Resource-Id.
	// volc.seedasr.auc is not a served model, so it must NOT be routed on — the
	// single provider serving the path is selected by endpoint.
	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/api/v3/auc/bigmodel/submit",
		strings.NewReader(`{"user":{"uid":"me"},"request":{"model_name":"bigmodel"}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Resource-Id", "volc.seedasr.auc")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (resource id must not be routed as a model)", resp.StatusCode)
	}

	rec.mu.Lock()
	gotPaths := append([]string(nil), rec.paths...)
	rec.mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != "/api/v3/auc/bigmodel/submit" {
		t.Fatalf("upstream paths = %v, want [/api/v3/auc/bigmodel/submit]", gotPaths)
	}
}

// --- Test 14: an unknown provider pin -> 502 no_upstream, nothing forwarded ---

func TestNativeUnknownProviderPin(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := nativeYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.doPinned(t, http.MethodGet, "/api/v1/tasks/abc", key, "nope", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for unknown provider pin", resp.StatusCode)
	}
	rec.mu.Lock()
	calls := len(rec.paths)
	rec.mu.Unlock()
	if calls != 0 {
		t.Fatalf("upstream called %d times for unknown provider", calls)
	}
}

// --- Test 15: a scoped user is denied a model-less pin to an out-of-scope vendor ---

func TestNativeScope(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := nativeYAML(mock.URL, "bailian")
	st := openStore(t)
	// User scoped to a different vendor: it may not address bailian.
	_, key := mustUser(t, st, store.NewUser{Name: "t", Scope: []string{"othervendor"}})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// Model-less pin to bailian (provider id bailian-key); vendor "bailian" is not
	// in scope, so it is rejected before any upstream call.
	resp := env.doPinned(t, http.MethodGet, "/api/v1/tasks/abc", key, "bailian-key", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (vendor not in scope)", resp.StatusCode)
	}
	rec.mu.Lock()
	calls := len(rec.paths)
	rec.mu.Unlock()
	if calls != 0 {
		t.Fatalf("upstream called despite scope rejection")
	}
}

// --- Test 16: a single provider serving the model is a single attempt ---

func TestNativeSingleAttempt(t *testing.T) {
	// The mock 500s; songguo forwards a single attempt and surfaces the error to
	// the client verbatim — never retried (there is no per-call failover, even
	// when other providers exist; see TestNoPerCallFailover).
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key1" {
			t.Errorf("Authorization = %q, want Bearer key1", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"down"}`)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: bailian
    origin: %s
    served_models: [qwen-plus]
    priority: 1
    wires: [openai/chat]
    allow_unmatched: true
    credential: {id: c1, api_key: key1}
    prices:
      qwen-plus: { input: 0.40, output: 1.20, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/api/v1/services/x/generation", key, `{"model":"qwen-plus"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (single attempt, no retry)", resp.StatusCode)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1 (exactly one attempt)", len(rows))
	}
	if rows[0].Vendor != "bailian" || rows[0].Status != 500 {
		t.Errorf("row = %+v, want bailian/500", rows[0])
	}
}

// --- Test 17: unmatched path denied (model-routed) -> 404 + recorded ---

func TestUnmatchedDenyModelRouted(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	// vendorA enables only openai/embeddings: a chat call must be denied.
	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/embeddings]
    credential: {id: credA, api_key: k}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 wire_unmatched", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "wire_unmatched") {
		t.Errorf("body = %q, want wire_unmatched error", body)
	}
	if up.calls != 0 {
		t.Fatalf("upstream called %d times for denied path", up.calls)
	}

	// The rejection is recorded so the dashboard surfaces the missing wire.
	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1 (unmatched log)", len(rows))
	}
	r := rows[0]
	if r.Status != 404 || r.Confidence != string(calls.ConfidenceUnknown) || !strings.Contains(r.Err, "unmatched:") {
		t.Errorf("unmatched row = %+v", r)
	}
}

// --- Test 18: unmatched native path denied when allow_unmatched is off ---

func TestUnmatchedDenyNative(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: bailian
    origin: %s
    served_models: [qwen-plus]
    priority: 1
    wires: [openai/chat]
    credential: {id: c1, api_key: key1}
    prices:
      qwen-plus: { input: 0.40, output: 1.20, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/api/v1/services/aigc/text-generation/generation", key, `{"model":"qwen-plus"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no allow_unmatched)", resp.StatusCode)
	}
	rec.mu.Lock()
	upCalls := len(rec.paths)
	rec.mu.Unlock()
	if upCalls != 0 {
		t.Fatalf("upstream called for denied path")
	}
	// But a wired path on the same vendor still works.
	resp2 := env.post(t, "/v1/chat/completions", key, `{"model":"qwen-plus","messages":[]}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("wired path status = %d, want 200", resp2.StatusCode)
	}
}

// anthropicUpstream mimics an Anthropic-compatible vendor: non-stream returns
// a Messages body; stream emits message_start (input usage) + message_delta
// (output usage), the shape whose input_tokens the old scanner dropped.
func anthropicUpstream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":40}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":100,"cache_read_input_tokens":40,"output_tokens":1}}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","delta":{"text":"hi"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, c+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func anthropicYAML(baseURL string) string {
	return fmt.Sprintf(`
vendors:
  - name: anthro
    origin: %s/v1
    adapter: anthropic-compatible
    served_models: [claude-x]
    priority: 1
    wires: [anthropic/messages]
    credential: {id: credAn, api_key: anthro-key}
    prices:
      claude-x: { input: 3.0, output: 15.0, cached_input: 0.3, unit: per_1m_tokens }
`, baseURL)
}

// --- Test 19: anthropic streaming merges input + output usage and prices cache reads ---

func TestAnthropicStreamingUsageMerged(t *testing.T) {
	mock := httptest.NewServer(anthropicUpstream())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, anthropicYAML(mock.URL)), st)

	resp := env.post(t, "/v1/messages", key, `{"model":"claude-x","stream":true,"messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Wire != "anthropic/messages" || r.Confidence != string(calls.ConfidenceMeasured) {
		t.Errorf("wire/confidence = %q/%q", r.Wire, r.Confidence)
	}
	// input_tokens from message_start MUST survive the merge.
	if got := r.Usage["input_tokens"]; got != float64(100) {
		t.Errorf("usage input_tokens = %v, want 100 (dropped by pre-wire scanner)", got)
	}
	if got := r.Usage["output_tokens"]; got != float64(25) {
		t.Errorf("usage output_tokens = %v, want 25", got)
	}
	// Norm folds cache reads into input: in=140 (40 cached @0.3), out=25.
	wantCost := (100*3.0 + 40*0.3 + 25*15.0) / 1e6
	if !approxEqual(r.Cost, wantCost) {
		t.Errorf("cost = %v, want %v", r.Cost, wantCost)
	}
}

// --- Test 20: cached-input pricing on a non-stream deepseek-style call ---

func TestCachedInputPricing(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10}}`)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: deepseek
    origin: %s/v1
    served_models: [deepseek-v4-flash]
    priority: 1
    wires: [openai/chat]
    quirks: { cache_tokens: deepseek }
    credential: {id: credD, api_key: ds-key}
    prices:
      deepseek-v4-flash: { input: 0.14, output: 0.28, cached_input: 0.0028, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"deepseek-v4-flash","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	// 10 miss @0.14 + 90 hit @0.0028 + 10 out @0.28, per 1M.
	wantCost := (10*0.14 + 90*0.0028 + 10*0.28) / 1e6
	if !approxEqual(rows[0].Cost, wantCost) {
		t.Errorf("cost = %v, want %v (cache-hit discount must apply)", rows[0].Cost, wantCost)
	}
}

// --- Test 21: the request body is forwarded verbatim, streamed or not ---

func TestRequestBodyForwardedVerbatim(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: k}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// Streamed request: the proxy must NOT rewrite the body (no injection).
	stream := `{"model":"gpt-4o","stream":true,"messages":[]}`
	resp := env.post(t, "/v1/chat/completions", key, stream)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if string(up.lastBody) != stream {
		t.Errorf("streamed body changed: got %q want %q", up.lastBody, stream)
	}

	// Non-streamed request must pass through byte-for-byte too.
	plain := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	resp2 := env.post(t, "/v1/chat/completions", key, plain)
	defer resp2.Body.Close()
	_, _ = io.Copy(io.Discard, resp2.Body)
	if string(up.lastBody) != plain {
		t.Errorf("non-stream body changed: got %q want %q", up.lastBody, plain)
	}
}

// A wire's full endpoint is used verbatim in Mode A: the inbound /v1/... suffix
// is NOT re-appended, so a non-standard upstream path is honored. Also exercises
// the wire allowlist being derived from the endpoints map (no explicit `wires:`).
func TestModelRoutedFullEndpoint(t *testing.T) {
	var gotPath string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: v
    origin: %s
    endpoints:
      openai/chat: %s/weird/upstream/path
    served_models: [gpt-4o]
    priority: 1
    credential: {id: c, api_key: k}
    prices:
      gpt-4o: { input: 1, output: 1, unit: per_1m_tokens }
`, mock.URL, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	if gotPath != "/weird/upstream/path" {
		t.Errorf("upstream path = %q, want /weird/upstream/path (full endpoint used as-is, suffix not appended)", gotPath)
	}
}

// TestCapturesClaudeCodeAttribution checks the three X-Claude-Code-* headers are
// sniffed (read-only) onto the recorded ledger entry, so the ledger can be
// grouped by session and agent.
func TestCapturesClaudeCodeAttribution(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: vendor-secret-key}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-42")
	req.Header.Set("X-Claude-Code-Agent-Id", "agent-a")
	req.Header.Set("X-Claude-Code-Parent-Agent-Id", "agent-root")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	entries, err := st.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.SessionID != "sess-42" || e.AgentID != "agent-a" || e.ParentAgentID != "agent-root" {
		t.Errorf("attribution = %q/%q/%q, want sess-42/agent-a/agent-root",
			e.SessionID, e.AgentID, e.ParentAgentID)
	}

	// Transparency: the attribution headers are forwarded upstream untouched.
	if up.lastHeaders.Get("X-Claude-Code-Session-Id") != "sess-42" {
		t.Errorf("upstream session header = %q, want sess-42 (forwarded verbatim)",
			up.lastHeaders.Get("X-Claude-Code-Session-Id"))
	}
}

func TestCapturesCodexSessionAttribution(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-4o]
    priority: 1
    wires: [openai/chat]
    credential: {id: credA, api_key: vendor-secret-key}
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Originator", "codex_cli_rs")
	req.Header.Set("Session-Id", "codex-session-42")
	req.Header.Set("Thread-Id", "codex-thread-99")
	req.Header.Set("X-Codex-Turn-Metadata", `{"session_id":"codex-session-42","thread_id":"codex-thread-99","turn_id":"turn-1"}`)
	req.Header.Set("X-Codex-Window-Id", "codex-session-42:0")
	req.Header.Set("User-Agent", "codex_cli_rs/0.137.0 (Debian 13.0.0; x86_64) unknown")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	entries, err := st.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].SessionID != "codex-session-42" {
		t.Errorf("SessionID = %q, want codex-session-42", entries[0].SessionID)
	}
	if entries[0].ClientName != "codex-openai" || entries[0].ClientVersion != "0.137.0" {
		t.Errorf("client = %q/%q, want codex-openai/0.137.0",
			entries[0].ClientName, entries[0].ClientVersion)
	}
	if entries[0].AgentID != "" || entries[0].ParentAgentID != "" {
		t.Errorf("Codex agent attribution = %q/%q, want empty/empty",
			entries[0].AgentID, entries[0].ParentAgentID)
	}

	if up.lastHeaders.Get("Session-Id") != "codex-session-42" {
		t.Errorf("upstream Session-Id = %q, want codex-session-42 (forwarded verbatim)",
			up.lastHeaders.Get("Session-Id"))
	}
}

func TestExtractAttributionCodexFallbacksAndGuards(t *testing.T) {
	h := http.Header{}
	h.Set("Originator", "codex-tui")
	h.Set("X-Codex-Turn-Metadata", `{"session_id":"meta-session","thread_id":"meta-thread"}`)
	if got := extractAttribution(h); got.session != "meta-session" {
		t.Fatalf("metadata session = %q, want meta-session", got.session)
	}

	h = http.Header{}
	h.Set("User-Agent", "codex-tui/0.142.5")
	h.Set("Thread-Id", "thread-only")
	if got := extractAttribution(h); got.session != "thread-only" {
		t.Fatalf("thread fallback session = %q, want thread-only", got.session)
	}

	h = http.Header{}
	h.Set("Session-Id", "generic-session")
	if got := extractAttribution(h); got.session != "" {
		t.Fatalf("generic Session-Id was captured as %q, want empty", got.session)
	}
}

func TestExtractClientInfo(t *testing.T) {
	tests := []struct {
		name          string
		ua            string
		stainlessOS   string
		wantName      string
		wantVersion   string
		wantOS        string
		wantOSVersion string
	}{
		{
			// claude-cli's UA comment carries no OS; the OS comes from the
			// Stainless SDK header (no version there).
			name:        "claude cli, stainless linux",
			ua:          "claude-cli/2.1.201 (external, sdk-ts, agent-sdk/0.3.198)",
			stainlessOS: "Linux",
			wantName:    "claude-code",
			wantVersion: "2.1.201",
			wantOS:      "Linux",
		},
		{
			// MacOS is already the Stainless spelling — passes through canonical.
			name:        "claude cli, stainless macos",
			ua:          "claude-cli/2.1.198 (external, sdk-ts, agent-sdk/0.3.198)",
			stainlessOS: "MacOS",
			wantName:    "claude-code",
			wantVersion: "2.1.198",
			wantOS:      "MacOS",
		},
		{
			// No Stainless header and no OS in the claude UA comment -> no OS.
			name:        "claude cli, no os",
			ua:          "claude-cli/2.1.198 (external, sdk-ts, agent-sdk/0.3.198)",
			wantName:    "claude-code",
			wantVersion: "2.1.198",
		},
		{
			name:        "codex tui, no comment",
			ua:          "codex-tui/0.142.5",
			wantName:    "codex-openai",
			wantVersion: "0.142.5",
		},
		{
			// codex embeds the OS browser-style in the UA comment; family + version.
			name:          "codex mac from ua",
			ua:            "codex_cli_rs/0.129.0 (Mac OS 26.5.2; arm64) otty/1.2.1",
			wantName:      "codex-openai",
			wantVersion:   "0.129.0",
			wantOS:        "MacOS",
			wantOSVersion: "26.5.2",
		},
		{
			// Unrecognized distro name is kept best-effort (not forced to "Linux").
			name:          "codex rust cli, debian from ua",
			ua:            "codex_cli_rs/0.137.0 (Debian 13.0.0; x86_64) unknown",
			wantName:      "codex-openai",
			wantVersion:   "0.137.0",
			wantOS:        "Debian",
			wantOSVersion: "13.0.0",
		},
		{
			// The explicit Stainless header wins over the UA comment (and carries
			// no version).
			name:        "stainless header overrides ua comment",
			ua:          "codex_cli_rs/0.129.0 (Mac OS 26.5.2; arm64) otty/1.2.1",
			stainlessOS: "Linux",
			wantName:    "codex-openai",
			wantVersion: "0.129.0",
			wantOS:      "Linux",
		},
		{
			// OS is captured even when the client family is unrecognized.
			name:        "unknown client, stainless windows",
			ua:          "openai-node/4.0",
			stainlessOS: "Windows",
			wantOS:      "Windows",
		},
		{
			name: "unknown",
			ua:   "openai-node/4.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calls.ParseClientInfo(tt.ua, tt.stainlessOS)
			if got.Name != tt.wantName || got.Version != tt.wantVersion {
				t.Fatalf("calls.ParseClientInfo(%q, %q) name/version = %q/%q, want %q/%q",
					tt.ua, tt.stainlessOS, got.Name, got.Version, tt.wantName, tt.wantVersion)
			}
			if got.OS != tt.wantOS || got.OSVersion != tt.wantOSVersion {
				t.Fatalf("calls.ParseClientInfo(%q, %q) os/version = %q/%q, want %q/%q",
					tt.ua, tt.stainlessOS, got.OS, got.OSVersion, tt.wantOS, tt.wantOSVersion)
			}
		})
	}
}

// --- count_tokens routes like Messages and is billed zero-cost ---

// The Anthropic token-counting endpoint shares the /messages stem but is its own
// ZeroCost wire. It must route on the body model, forward verbatim, and record a
// call at $0 — not 404 as wire_unmatched.
func TestAnthropicCountTokensZeroCost(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":7}`)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: anthro
    origin: %s
    adapter: anthropic-compatible
    served_models: [claude-x]
    priority: 1
    wires: [anthropic/messages, anthropic/count_tokens]
    credential: {id: credAn, api_key: anthro-key}
    prices:
      claude-x: { input: 3.0, output: 15.0, cached_input: 0.3, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/v1/messages/count_tokens", key,
		`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (routed, not wire_unmatched); body=%s", resp.StatusCode, body)
	}
	if string(body) != `{"input_tokens":7}` {
		t.Errorf("body not forwarded verbatim: got %q", body)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	if rows[0].Wire != "anthropic/count_tokens" {
		t.Errorf("wire = %q, want anthropic/count_tokens", rows[0].Wire)
	}
	if rows[0].Cost != 0 {
		t.Errorf("cost = %v, want 0 (zero-cost wire)", rows[0].Cost)
	}
}

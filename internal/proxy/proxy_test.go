package proxy

import (
	"bufio"
	"bytes"
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

	forceStatus int    // if non-zero, every request returns this status
	lastAuth    string // Authorization header observed on the last request
	lastBody    []byte // request body observed on the last request
	calls       int
}

func (m *mockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.lastAuth = r.Header.Get("Authorization")
		m.lastBody = body
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

func (e *testEnv) callRows(t *testing.T) []callRow {
	t.Helper()
	entries, err := e.store.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	out := make([]callRow, len(entries))
	for i, en := range entries {
		out[i] = callRow{
			Vendor:  en.Vendor,
			Status:  en.Status,
			Attempt: en.Attempt,
			Model:   en.Model,
			Cost:    en.Cost,
			Stream:  en.Stream,
			Usage:   en.Usage,
			Tags:    en.Tags,
		}
	}
	return out
}

type callRow struct {
	Vendor  string
	Status  int
	Attempt int
	Model   string
	Cost    float64
	Stream  bool
	Usage   map[string]any
	Tags    map[string]string
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

func mustToken(t *testing.T, st *store.Store, nt store.NewToken) (store.Token, string) {
	t.Helper()
	tok, key, err := st.CreateToken(nt)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
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
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 1
    credentials:
      - id: credA
        api_key: vendor-secret-key
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
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

// --- Test 2: embeddings happy path ---

func TestEmbeddingsHappyPath(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: emb
    base_url: %s/v1
    served_models: [text-embedding-3-small]
    priority: 1
    credentials:
      - id: credE
        api_key: emb-key
    prices:
      text-embedding-3-small: { input: 0.02, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
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

func TestInvalidToken(t *testing.T) {
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
	_, key := mustToken(t, st, store.NewToken{Name: "t", Scope: []string{"some-other-model"}})
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
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 1
    credentials:
      - id: credA
        api_key: k
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	// Budget tiny enough that one call's cost crosses it.
	budget := 0.0001
	_, key := mustToken(t, st, store.NewToken{Name: "t", Budget: &budget})
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
	_, key := mustToken(t, st, store.NewToken{Name: "t", RPM: 1})
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

// --- Test 7: failover A(500) -> B(200), two call rows ---

func TestFailover(t *testing.T) {
	upA := &mockUpstream{forceStatus: 500}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

	yaml := fmt.Sprintf(`
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (from B)", resp.StatusCode)
	}
	if upB.calls != 1 {
		t.Errorf("vendorB calls = %d, want 1", upB.calls)
	}
	if upA.calls != 1 {
		t.Errorf("vendorA calls = %d, want 1", upA.calls)
	}

	rows := env.callRows(t)
	if len(rows) != 2 {
		t.Fatalf("call rows = %d, want 2", len(rows))
	}
	// Rows are ts DESC; find by vendor.
	var aRow, bRow *callRow
	for i := range rows {
		switch rows[i].Vendor {
		case "vendorA":
			aRow = &rows[i]
		case "vendorB":
			bRow = &rows[i]
		}
	}
	if aRow == nil || aRow.Status != 500 || aRow.Attempt != 1 {
		t.Errorf("vendorA row = %+v, want status 500 attempt 1", aRow)
	}
	if bRow == nil || bRow.Status != 200 || bRow.Attempt != 2 {
		t.Errorf("vendorB row = %+v, want status 200 attempt 2", bRow)
	}
}

// --- Test 8: all-fail single vendor -> upstream 500 passed through ---

func TestAllFailPassthrough(t *testing.T) {
	up := &mockUpstream{forceStatus: 500}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	yaml := singleVendorYAML(mock.URL, "vendorA", "credA", "k")
	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
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
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
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
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 1
    credentials:
      - id: credA
        api_key: k
    prices:
      gpt-4o: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)
	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
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
// is mounted at the host root, so we append /v1 to base_url and Mode-A's suffix
// (path minus /v1) lands the upstream call back on /chat/completions etc.
func singleVendorYAML(baseURL, vendor, credID, apiKey string) string {
	return fmt.Sprintf(`
vendors:
  - name: %s
    base_url: %s/v1
    served_models: [gpt-4o]
    priority: 1
    credentials:
      - id: %s
        api_key: %s
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

// --- Test 11: Mode A reaches a non-/v1 vendor base prefix (e.g. Ark) ---

func TestModelRoutedNonV1Prefix(t *testing.T) {
	// This mock ONLY serves /api/v3/chat/completions, mimicking 火山方舟/Ark whose
	// OpenAI-compatible base is …/api/v3. A /v1/chat/completions request must land
	// on /api/v3/chat/completions after the base_url + suffix rewrite.
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
    base_url: %s/api/v3
    served_models: [doubao-pro-32k]
    priority: 1
    credentials:
      - id: arkKey
        api_key: ark-secret
    prices:
      doubao-pro-32k: { input: 2.50, output: 10.00, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
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

// passthroughYAML builds a one-vendor config for passthrough tests. base_url
// carries a non-trivial path prefix (DashScope's /compatible-mode/v1); Mode B
// must strip it and forward to the host origin. served_models/prices key off
// the native model name so cost is computed.
func passthroughYAML(baseURL, vendor string) string {
	return fmt.Sprintf(`
vendors:
  - name: %s
    base_url: %s/compatible-mode/v1
    served_models: [qwen-plus]
    priority: 1
    credentials:
      - id: %s-key
        api_key: %s-secret
    prices:
      qwen-plus: { input: 0.40, output: 1.20, unit: per_1m_tokens }
`, vendor, baseURL, vendor, vendor)
}

// --- Test 12: Mode B native submit forwarded to origin+rest, usage metered ---

func TestPassthroughNativeUsage(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := passthroughYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// Native generation endpoint with a model in the body.
	body := `{"model":"qwen-plus","input":{"prompt":"hi"}}`
	resp := env.post(t, "/x/bailian/api/v1/services/aigc/text-generation/generation", key, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	rec.mu.Lock()
	gotPaths := append([]string(nil), rec.paths...)
	gotAuth := rec.auth
	rec.mu.Unlock()

	// The base_url path prefix (/compatible-mode/v1) must be stripped: the native
	// path is forwarded to the host origin verbatim.
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
	// input_tokens=12, output_tokens=8 -> 0.40*12/1e6 + 1.20*8/1e6.
	wantCost := 0.40*12/1e6 + 1.20*8/1e6
	if !approxEqual(rows[0].Cost, wantCost) {
		t.Errorf("cost = %v, want %v", rows[0].Cost, wantCost)
	}
	if rows[0].Vendor != "bailian" || rows[0].Status != 200 {
		t.Errorf("row = %+v", rows[0])
	}
}

// --- Test 13: Mode B model-less GET (async task poll) is forwarded, not 400 ---

func TestPassthroughModelLessGet(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := passthroughYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	// No body, no model: an async task poll. Must NOT be rejected as missing_model.
	resp := env.do(t, http.MethodGet, "/x/bailian/api/v1/tasks/abc", key, "")
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

// --- Test 14: Mode B unknown vendor -> 404 ---

func TestPassthroughUnknownVendor(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := passthroughYAML(mock.URL, "bailian")
	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.do(t, http.MethodGet, "/x/nope/api/v1/tasks/abc", key, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown vendor", resp.StatusCode)
	}
	rec.mu.Lock()
	calls := len(rec.paths)
	rec.mu.Unlock()
	if calls != 0 {
		t.Fatalf("upstream called %d times for unknown vendor", calls)
	}
}

// --- Test 15: Mode B passthrough scope enforces vendor allowlist -> 403 ---

func TestPassthroughScope(t *testing.T) {
	rec := &pathRecorder{}
	mock := httptest.NewServer(rec.handler())
	defer mock.Close()

	yaml := passthroughYAML(mock.URL, "bailian")
	st := openStore(t)
	// Token scoped to a different vendor: it may not address bailian.
	_, key := mustToken(t, st, store.NewToken{Name: "t", Scope: []string{"othervendor"}})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/x/bailian/api/v1/services/x/generation", key, `{"model":"qwen-plus"}`)
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

// --- Test 16: Mode B fails over across the vendor's own credentials ---

func TestPassthroughCredentialRetry(t *testing.T) {
	// The mock 500s on the first credential and 200s on the second, identifying
	// the credential by the swapped Authorization header.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer key1" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"down"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: bailian
    base_url: %s/compatible-mode/v1
    served_models: [qwen-plus]
    priority: 1
    credentials:
      - id: c1
        api_key: key1
      - id: c2
        api_key: key2
    prices:
      qwen-plus: { input: 0.40, output: 1.20, unit: per_1m_tokens }
`, mock.URL)

	st := openStore(t)
	_, key := mustToken(t, st, store.NewToken{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	resp := env.post(t, "/x/bailian/api/v1/services/x/generation", key, `{"model":"qwen-plus"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (retry on 2nd credential)", resp.StatusCode)
	}

	rows := env.callRows(t)
	if len(rows) != 2 {
		t.Fatalf("call rows = %d, want 2 (one per credential attempt)", len(rows))
	}
	var got500, got200 bool
	for _, r := range rows {
		if r.Vendor != "bailian" {
			t.Errorf("row vendor = %q, want bailian (no cross-vendor failover)", r.Vendor)
		}
		switch r.Status {
		case 500:
			got500 = true
		case 200:
			got200 = true
		}
	}
	if !got500 || !got200 {
		t.Errorf("expected one 500 row and one 200 row, got %+v", rows)
	}
}

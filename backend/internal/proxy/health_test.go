package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/store"
)

// newEnvRouter is newEnv with explicit router options, for tests that drive
// cross-request health steering (a threshold of 1 demotes on the first failure,
// so a test needs two requests rather than four).
func newEnvRouter(t *testing.T, snap func() *config.Snapshot, st *store.Store, opt router.Options) *testEnv {
	t.Helper()
	h := NewHandler(Deps{
		Snapshot: snap,
		Store:    st,
		Router:   router.New(snap, opt),
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	// Drain the background forks before the store's cleanup closes the database
	// under them (t.Cleanup is LIFO, and openStore registered its close first).
	t.Cleanup(h.Close)
	return &testEnv{server: srv, store: st, client: srv.Client(), handler: h}
}

// setStatus changes the forced status mid-test, so a vendor can fail and then
// recover. Locked because the mock serves on the httptest goroutine.
func (m *mockUpstream) setStatus(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceStatus = status
}

// callCount reads the request counter under the mock's lock.
func (m *mockUpstream) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// failoverYAML is a priority-1 primary (A) and a priority-2 backup (B), both
// serving the same model — the canonical multi-provider shape.
func failoverYAML(originA, originB string) string {
	return fmt.Sprintf(`
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
`, originA, originB)
}

// TestHealthDemotionIsCrossRequest is the companion to TestNoPerCallFailover and
// the proof that cross-request steering stays inside behavioral transparency.
//
// Same two-vendor setup, opposite time axis. TestNoPerCallFailover asserts that
// ONE request never becomes two attempts. This asserts that the failure still
// changes where the NEXT request goes. Both must hold at once: the failing
// request is surfaced verbatim with vendorB untouched, and only afterwards does
// routing move.
func TestHealthDemotionIsCrossRequest(t *testing.T) {
	upA := &mockUpstream{forceStatus: 500}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnvRouter(t, snapshotFunc(t, failoverYAML(mockA.URL, mockB.URL)), st,
		router.Options{FailThreshold: 1})

	// --- Request 1: A fails. It is surfaced verbatim, with NO failover. ---
	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("request 1 status = %d, want 500 (A's failure surfaced verbatim)", resp.StatusCode)
	}
	if got := upA.callCount(); got != 1 {
		t.Fatalf("request 1: vendorA calls = %d, want 1", got)
	}
	if got := upB.callCount(); got != 0 {
		t.Fatalf("request 1: vendorB calls = %d, want 0 — the failing request must NOT be replayed", got)
	}
	if rows := env.callRows(t); len(rows) != 1 {
		t.Fatalf("request 1: ledger rows = %d, want 1 (a single attempt)", len(rows))
	}

	// --- Request 2: routing has moved. A is not retried, B serves. ---
	resp = env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200 (steered to healthy vendorB)", resp.StatusCode)
	}
	if got := upA.callCount(); got != 1 {
		t.Fatalf("request 2: vendorA calls = %d, want still 1 — A is demoted, not re-probed", got)
	}
	if got := upB.callCount(); got != 1 {
		t.Fatalf("request 2: vendorB calls = %d, want 1", got)
	}

	rows := env.callRows(t)
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2 (one per request, never one per attempt)", len(rows))
	}
	if rows[0].Vendor == rows[1].Vendor {
		t.Fatalf("both rows served by %q; want the second to have moved to vendorB", rows[0].Vendor)
	}
}

// testClock is a manually advanced clock for the router, so cooldown expiry is
// testable without sleeping. Only the router's clock is faked; the proxy keeps
// real time so ledger latencies stay meaningful.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestCooldownExpiryReturnsTraffic walks the full demote → steer → recover
// cycle. The recovery half matters as much as the demotion half: without it a
// single blip would strand traffic on the backup until the process restarted.
//
// It also shows the accepted cost of passive detection — A recovers upstream at
// step 3, but nothing routes to it until the cooldown lapses, because we do not
// probe. We only learn a vendor is healthy again by eventually sending it a
// real request.
func TestCooldownExpiryReturnsTraffic(t *testing.T) {
	upA := &mockUpstream{forceStatus: 503}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	clk := &testClock{t: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)}
	env := newEnvRouter(t, snapshotFunc(t, failoverYAML(mockA.URL, mockB.URL)), st,
		router.Options{FailThreshold: 1, Cooldown: 30 * time.Second, Now: clk.Now})

	body := `{"model":"gpt-4o","messages":[]}`

	// 1. A fails and is demoted. The client sees the real 503.
	resp := env.post(t, "/v1/chat/completions", key, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("request 1 status = %d, want 503", resp.StatusCode)
	}

	// 2. Traffic moves to B.
	env.post(t, "/v1/chat/completions", key, body).Body.Close()
	if got := upB.callCount(); got != 1 {
		t.Fatalf("vendorB calls = %d, want 1 after demotion", got)
	}

	// 3. A is healthy again upstream, but still cooling — we do not probe, so
	//    traffic stays on B.
	upA.setStatus(0)
	env.post(t, "/v1/chat/completions", key, body).Body.Close()
	if got := upA.callCount(); got != 1 {
		t.Fatalf("vendorA calls = %d, want still 1 inside the cooldown", got)
	}
	if got := upB.callCount(); got != 2 {
		t.Fatalf("vendorB calls = %d, want 2", got)
	}

	// 4. Cooldown lapses: A is simply live again (the half-open probe), leads on
	//    priority, and succeeds.
	clk.Advance(31 * time.Second)
	resp = env.post(t, "/v1/chat/completions", key, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request 4 status = %d, want 200", resp.StatusCode)
	}
	if got := upA.callCount(); got != 2 {
		t.Fatalf("vendorA calls = %d, want 2 — priority resumes after the cooldown", got)
	}
	if got := upB.callCount(); got != 2 {
		t.Fatalf("vendorB calls = %d, want still 2", got)
	}
}

// Test4xxDoesNotDemote: a caller error is not a vendor defect. Every vendor
// would reject a malformed request identically, so counting 4xx would let one
// broken client walk the entire fleet out of rotation.
func Test4xxDoesNotDemote(t *testing.T) {
	upA := &mockUpstream{forceStatus: 400}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnvRouter(t, snapshotFunc(t, failoverYAML(mockA.URL, mockB.URL)), st,
		router.Options{FailThreshold: 1})

	for i := 0; i < 3; i++ {
		resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("request %d status = %d, want 400 surfaced verbatim", i+1, resp.StatusCode)
		}
	}
	if got := upA.callCount(); got != 3 {
		t.Fatalf("vendorA calls = %d, want 3 — a 4xx must never demote", got)
	}
	if got := upB.callCount(); got != 0 {
		t.Fatalf("vendorB calls = %d, want 0", got)
	}
}

// TestGatewayDenialDoesNotDemote: songguo's own refusals (budget, scope, rate
// limit, unmatched wire) never reach the vendor, so they must never be counted
// against it. This holds structurally — Report is only called from the sites
// that actually dispatched — and this test pins that down.
func TestGatewayDenialDoesNotDemote(t *testing.T) {
	upA := &mockUpstream{}
	mockA := httptest.NewServer(upA.handler())
	defer mockA.Close()
	upB := &mockUpstream{}
	mockB := httptest.NewServer(upB.handler())
	defer mockB.Close()

	st := openStore(t)
	zero := 0.0
	_, brokeKey := mustUser(t, st, store.NewUser{Name: "broke", Budget: &zero})
	_, okKey := mustUser(t, st, store.NewUser{Name: "ok"})
	env := newEnvRouter(t, snapshotFunc(t, failoverYAML(mockA.URL, mockB.URL)), st,
		router.Options{FailThreshold: 1})

	// Three budget denials: gateway-side, never dispatched.
	for i := 0; i < 3; i++ {
		resp := env.post(t, "/v1/chat/completions", brokeKey, `{"model":"gpt-4o","messages":[]}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("denial %d status = %d, want 402", i+1, resp.StatusCode)
		}
	}
	if got := upA.callCount(); got != 0 {
		t.Fatalf("vendorA calls = %d, want 0 (denied before dispatch)", got)
	}

	// vendorA must still be the pick: nothing was ever reported against it.
	resp := env.post(t, "/v1/chat/completions", okKey, `{"model":"gpt-4o","messages":[]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := upA.callCount(); got != 1 {
		t.Fatalf("vendorA calls = %d, want 1 — gateway denials must not demote", got)
	}
	if got := upB.callCount(); got != 0 {
		t.Fatalf("vendorB calls = %d, want 0", got)
	}
}

package proxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

// vendorYAML is a one-vendor config pointed at origin.
func vendorYAML(origin string) string {
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
`, origin)
}

// closedPort returns an origin nothing is listening on, so a dial fails fast
// with a real connection-refused rather than a timeout.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + addr
}

// A transport failure must record WHY it failed. "connection refused", "no such
// host" and "certificate has expired" are different faults with different fixes;
// the flat "upstream_error" slug threw all three away even though the client was
// already being told the exact text.
func TestTransportFailureRecordsRealError(t *testing.T) {
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, vendorYAML(closedPort(t))), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	got := rows[0].Err
	if !strings.HasPrefix(got, calls.ErrPrefixTransport) {
		t.Errorf("err = %q, want the %q prefix", got, calls.ErrPrefixTransport)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("err = %q, want the real transport failure preserved", got)
	}
	if o := calls.OutcomeOf(rows[0].Status, got); o != calls.OutcomeTransportError {
		t.Errorf("outcome = %q, want transport_error", o)
	}
}

// A stream that dies mid-body was stored as a clean 200 with no error at all —
// indistinguishable from a completed answer. Both fields must now be true at
// once: the client really did receive a 200 header, and the relay really did
// break.
func TestTruncatedStreamIsNotRecordedAsClean200(t *testing.T) {
	// Announce far more body than we send, then hang up: the client sees a 200
	// header and the relay hits an unexpected EOF.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Returning early with Content-Length unmet truncates the response.
		panic(http.ErrAbortHandler)
	}))
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, vendorYAML(mock.URL)), st)

	resp := env.post(t, "/v1/chat/completions", key,
		`{"model":"gpt-4o","messages":[],"stream":true}`)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	// The status stays the vendor's. Rewriting it would be its own lie: the
	// client did receive a 200 header.
	if rows[0].Status != http.StatusOK {
		t.Errorf("status = %d, want 200 (the client really did get a 200 header)", rows[0].Status)
	}
	if rows[0].Err == "" {
		t.Fatal("err is empty: a truncated stream is being recorded as a clean success")
	}
	if !strings.HasPrefix(rows[0].Err, calls.ErrPrefixStream) {
		t.Errorf("err = %q, want the %q prefix", rows[0].Err, calls.ErrPrefixStream)
	}
	if o := calls.OutcomeOf(rows[0].Status, rows[0].Err); o != calls.OutcomeTruncated {
		t.Errorf("outcome = %q, want truncated", o)
	}
}

// A clean stream must stay clean — the truncation check above must not start
// flagging ordinary traffic.
func TestCleanStreamRecordsNoError(t *testing.T) {
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, vendorYAML(mock.URL)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	if rows[0].Err != "" {
		t.Errorf("err = %q, want empty for a clean relay", rows[0].Err)
	}
	if o := calls.OutcomeOf(rows[0].Status, rows[0].Err); o != calls.OutcomeOK {
		t.Errorf("outcome = %q, want ok", o)
	}
}

// A gateway denial must not look like the provider returning the same code.
func TestGatewayDenialIsDistinguishableFromVendorStatus(t *testing.T) {
	st := openStore(t)
	// A user scoped to another model: the request is refused before routing.
	_, key := mustUser(t, st, store.NewUser{Name: "t", Scope: []string{"other-model"}})
	up := &mockUpstream{}
	mock := httptest.NewServer(up.handler())
	defer mock.Close()
	env := newEnv(t, snapshotFunc(t, vendorYAML(mock.URL)), st)

	resp := env.post(t, "/v1/chat/completions", key, `{"model":"gpt-4o","messages":[]}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if up.calls != 0 {
		t.Errorf("upstream calls = %d, want 0", up.calls)
	}

	rows := env.callRows(t)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	o := calls.OutcomeOf(rows[0].Status, rows[0].Err)
	if o != calls.OutcomeDeniedScope {
		t.Fatalf("outcome = %q, want denied_scope", o)
	}
	// The whole point: a 403 we issued must never be charged to the provider.
	if calls.BlameFor(o) != calls.BlameGateway {
		t.Errorf("blame = %q, want gateway", calls.BlameFor(o))
	}
	if calls.IsProviderFailure(o) {
		t.Error("a songguo denial must not count against the provider")
	}
}

// A WebSocket dial that fails must leave a ledger row. Every HTTP failure path
// records one, and so does the WS unmatched-wire path — but a failed dial used
// to return silently, so a provider whose endpoint was unreachable produced no
// trace at all and the outage looked like an absence of traffic.
func TestWSDialFailureRecordsRow(t *testing.T) {
	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	// Point the vendor at a port nothing is listening on, so the dial fails.
	env := newEnv(t, snapshotFunc(t, volcSpeechVendorYAML(closedPort(t))), st)

	conn, br := dialProxyWS(t, env.server.URL, "/sauc/bigmodel_async", key, "")
	defer conn.Close()
	if code := readStatusLine(t, br); code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a failed upstream dial", code)
	}

	rows := waitForRows(t, env, 1)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1 (a failed dial must not vanish)", len(rows))
	}
	if !strings.HasPrefix(rows[0].Err, calls.ErrPrefixTransport) {
		t.Errorf("err = %q, want the %q prefix", rows[0].Err, calls.ErrPrefixTransport)
	}
	if o := calls.OutcomeOf(rows[0].Status, rows[0].Err); o != calls.OutcomeTransportError {
		t.Errorf("outcome = %q, want transport_error", o)
	}
}

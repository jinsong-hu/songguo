package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
      gpt-4o: { cost: { input: 2.5, output: 10 } }
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

// A Responses client may stop reading as soon as response.completed arrives.
// That closes the downstream request context while the proxy is waiting for
// transport EOF, but the protocol response is already complete and must remain
// a clean success.
func TestResponsesCompletedBeforeClientCloseRecordsSuccess(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":3}}}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer mock.Close()

	yaml := fmt.Sprintf(`
vendors:
  - name: vendorA
    origin: %s/v1
    served_models: [gpt-5.5]
    priority: 1
    wires: [openai/responses]
    credential: {id: credA, api_key: keyA}
    prices:
      gpt-5.5: { cost: { input: 1, output: 2 } }
`, mock.URL)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, yaml), st)

	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/v1/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}

	sc := bufio.NewScanner(resp.Body)
	sawCompleted := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"type":"response.completed"`) {
			sawCompleted = true
			break
		}
	}
	if !sawCompleted {
		resp.Body.Close()
		t.Fatal("client did not receive response.completed")
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		mock.CloseClientConnections()
		t.Fatal("caller close did not cancel the in-flight upstream request")
	}

	deadline := time.Now().Add(2 * time.Second)
	var rows []callRow
	for {
		rows = env.callRows(t)
		if len(rows) == 1 && rows[0].Status != calls.StatusPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("call did not finalize: rows = %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	if rows[0].Err != "" {
		t.Errorf("err = %q, want empty after response.completed", rows[0].Err)
	}
	if o := calls.OutcomeOf(rows[0].Status, rows[0].Err); o != calls.OutcomeOK {
		t.Errorf("outcome = %q, want ok", o)
	}
	if got := rows[0].Usage["output_tokens"]; got != float64(3) {
		t.Errorf("output_tokens = %v, want 3 from terminal usage", got)
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

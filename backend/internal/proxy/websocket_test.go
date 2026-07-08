package proxy

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// wsMagicGUID is the RFC 6455 accept-key salt.
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsAccept computes the Sec-WebSocket-Accept value for a client key.
func wsAccept(key string) string {
	h := sha1.New()
	io.WriteString(h, key+wsMagicGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsMockUpstream is a minimal WebSocket echo server. It validates the upgrade,
// echoes the received Authorization into X-Echo-Auth, responds 101 (unless told
// to refuse), then echoes raw bytes back. Frame structure is irrelevant: it is
// a pure byte echo, matching the proxy's pure byte relay.
type wsMockUpstream struct {
	refuseStatus int    // if non-zero, refuse the upgrade with this status
	script       []byte // if set, written downstream after 101 (instead of echo)
	lastAuth     string // Authorization observed on the handshake
	lastPath     string // request-target observed on the handshake
}

func (m *wsMockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastAuth = r.Header.Get("Authorization")
		m.lastPath = r.URL.RequestURI()

		if !isWebSocketUpgrade(r) {
			http.Error(w, "expected upgrade", http.StatusBadRequest)
			return
		}

		if m.refuseStatus != 0 {
			w.Header().Set("X-Echo-Auth", m.lastAuth)
			w.WriteHeader(m.refuseStatus)
			_, _ = io.WriteString(w, `{"error":"refused"}`)
			return
		}

		key := r.Header.Get("Sec-WebSocket-Key")
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + wsAccept(key) + "\r\n" +
			"X-Echo-Auth: " + m.lastAuth + "\r\n" +
			"\r\n"
		if _, err := rw.WriteString(resp); err != nil {
			return
		}
		if err := rw.Flush(); err != nil {
			return
		}

		// Scripted mode: push fixed downstream bytes, then drain the client until
		// it closes (no echo). Used to exercise downstream metering.
		if len(m.script) > 0 {
			if _, err := rw.Write(m.script); err != nil {
				return
			}
			if err := rw.Flush(); err != nil {
				return
			}
			buf := make([]byte, 4096)
			for {
				if _, err := rw.Read(buf); err != nil {
					return
				}
			}
		}

		// Echo raw bytes until the client closes.
		buf := make([]byte, 4096)
		for {
			n, err := rw.Read(buf)
			if n > 0 {
				if _, werr := rw.Write(buf[:n]); werr != nil {
					return
				}
				if ferr := rw.Flush(); ferr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
}

// wsMockServer starts the mock upstream and returns it plus the listener host.
func wsMockServer(t *testing.T, m *wsMockUpstream) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	t.Cleanup(srv.Close)
	return srv
}

// wsVendorYAML builds a one-vendor config whose origin points at the mock host
// (http scheme, so the proxy dials plain ws). It uses an OpenAI-compatible wire
// so the WebSocket relay still exercises the same credential swap and byte pipe
// while preserving the invariant that every proxied endpoint resolves to a wire.
func wsVendorYAML(baseURL, vendor, credID, apiKey string) string {
	return fmt.Sprintf(`
vendors:
  - name: %s
    origin: %s
    served_models: [realtime-model]
    priority: 1
    wires: [openai/images]
    endpoints:
      openai/images: %s/v1/images/generations
    credential: {id: %s, api_key: %s}
    prices:
      realtime-model: { input: 1.0, unit: per_call }
`, vendor, baseURL, baseURL, credID, apiKey)
}

// dialProxyWS opens a raw TCP connection to the proxy and writes a WebSocket
// upgrade request for the given path with the given token, returning the conn
// and a buffered reader positioned to read the handshake response. When provider
// is non-empty it is sent as the optional X-Songguo-Provider pin; leaving it
// empty exercises endpoint-first routing (the path alone selects the vendor).
func dialProxyWS(t *testing.T, proxyURL, path, token, provider string) (net.Conn, *bufio.Reader) {
	t.Helper()
	u, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}

	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&req, "Host: %s\r\n", u.Host)
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	if token != "" {
		fmt.Fprintf(&req, "Authorization: Bearer %s\r\n", token)
	}
	if provider != "" {
		fmt.Fprintf(&req, "X-Songguo-Provider: %s\r\n", provider)
	}
	req.WriteString("\r\n")

	if _, err := conn.Write([]byte(req.String())); err != nil {
		t.Fatalf("write upgrade: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

// readStatusLine reads and parses "HTTP/1.1 <code> ..." from the reader.
func readStatusLine(t *testing.T, br *bufio.Reader) int {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 {
		t.Fatalf("malformed status line: %q", line)
	}
	var code int
	if _, err := fmt.Sscanf(parts[1], "%d", &code); err != nil {
		t.Fatalf("parse status code from %q: %v", line, err)
	}
	return code
}

// readHeaders reads response headers up to the blank line.
func readHeaders(t *testing.T, br *bufio.Reader) http.Header {
	t.Helper()
	h := http.Header{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return h
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		h.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}
}

// waitForRows polls the store until n call rows exist or the deadline passes.
func waitForRows(t *testing.T, e *testEnv, n int) []callRow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := e.callRows(t)
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("call rows = %d, want %d", len(rows), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- WS Test 1: happy path — upgrade, credential swap, byte echo, metering ---

func TestWebSocketHappyPath(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, wsVendorYAML(mock.URL, "rt", "credR", "vendor-rt-secret")), st)

	conn, br := dialProxyWS(t, env.server.URL, "/v1/images/generations?model=realtime-model", key, "credR")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", code)
	}
	headers := readHeaders(t, br)

	// The mock echoes the Authorization it saw; it MUST be the vendor key, not
	// the Songguo token.
	if got := headers.Get("X-Echo-Auth"); got != "Bearer vendor-rt-secret" {
		t.Errorf("upstream saw Authorization %q, want vendor key", got)
	}
	if up.lastAuth == "Bearer "+key {
		t.Errorf("upstream received the Songguo token; must be swapped")
	}
	if accept := headers.Get("Sec-WebSocket-Accept"); accept != wsAccept("dGhlIHNhbXBsZSBub25jZQ==") {
		t.Errorf("Sec-WebSocket-Accept = %q, want correct accept", accept)
	}
	// The upstream must have seen the matched wire endpoint plus inbound query.
	if up.lastPath != "/v1/images/generations?model=realtime-model" {
		t.Errorf("upstream request-target = %q, want /v1/images/generations?model=realtime-model", up.lastPath)
	}

	// Now send raw bytes and expect them echoed back unchanged both directions.
	payload := []byte("hello-realtime-bytes")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("echo = %q, want %q (bytes must pass unchanged)", got, payload)
	}

	// Close the client side to end the session, then check the metered row.
	conn.Close()

	rows := waitForRows(t, env, 1)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Vendor != "rt" || r.Status != http.StatusSwitchingProtocols {
		t.Errorf("row = %+v, want vendor=rt status=101", r)
	}
	if r.Wire != "openai/images" {
		t.Errorf("row wire = %q, want openai/images", r.Wire)
	}
	if r.Usage == nil {
		t.Fatalf("usage is nil, want bytes_up/bytes_down/duration_ms")
	}
	up0, _ := r.Usage["bytes_up"].(float64)
	down0, _ := r.Usage["bytes_down"].(float64)
	if up0 <= 0 {
		t.Errorf("bytes_up = %v, want > 0", r.Usage["bytes_up"])
	}
	if down0 <= 0 {
		t.Errorf("bytes_down = %v, want > 0", r.Usage["bytes_down"])
	}
	if _, ok := r.Usage["duration_ms"]; !ok {
		t.Errorf("usage missing duration_ms: %+v", r.Usage)
	}

	// Modality must be realtime.
	entries, err := st.QueryCalls(storeFilterAll())
	if err != nil {
		t.Fatalf("QueryCalls: %v", err)
	}
	if entries[0].Modality != "realtime" {
		t.Errorf("modality = %q, want realtime", entries[0].Modality)
	}
	if !entries[0].Stream {
		t.Errorf("stream flag = false, want true")
	}
}

// --- WS Test 2: upstream refuses the upgrade (401) -> client gets 401, row ---

func TestWebSocketUpstreamRefuses(t *testing.T) {
	up := &wsMockUpstream{refuseStatus: http.StatusUnauthorized}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, volcSpeechVendorYAML(mock.URL)), st)

	conn, br := dialProxyWS(t, env.server.URL, "/api/v3/sauc/bigmodel_async?model=seed-asr", key, "credV")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusUnauthorized {
		t.Fatalf("handshake status = %d, want 401 (upstream refusal relayed)", code)
	}

	rows := waitForRows(t, env, 1)
	if len(rows) != 1 {
		t.Fatalf("call rows = %d, want 1", len(rows))
	}
	if rows[0].Status != http.StatusUnauthorized {
		t.Errorf("row status = %d, want 401", rows[0].Status)
	}
	if rows[0].Wire != "volc/asr-stream-async" {
		t.Errorf("row wire = %q, want volc/asr-stream-async", rows[0].Wire)
	}
	entries, _ := st.QueryCalls(storeFilterAll())
	if entries[0].Modality != "realtime" {
		t.Errorf("modality = %q, want realtime", entries[0].Modality)
	}
}

// --- WS Test 3: unknown provider pin -> 502 ---

func TestWebSocketUnknownProvider(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, wsVendorYAML(mock.URL, "rt", "credR", "vendor-rt-secret")), st)

	conn, br := dialProxyWS(t, env.server.URL, "/v1/images/generations", key, "nope")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for unknown provider pin", code)
	}
	if up.lastAuth != "" {
		t.Errorf("upstream was dialed for an unknown provider")
	}
}

// --- WS Test 4a: no provider pin routes by endpoint (the "just change the
// endpoint" invariant) — the path alone selects the vendor and the handshake
// reaches the upstream with the credential swapped in. ---

func TestWebSocketNoProviderRoutesByEndpoint(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	// Vendor declares the volc/asr-stream-async wire, whose suffix matches the
	// dialed path. No X-Songguo-Provider header is sent.
	env := newEnv(t, snapshotFunc(t, volcSpeechVendorYAML(mock.URL)), st)

	conn, br := dialProxyWS(t, env.server.URL, "/api/v3/sauc/bigmodel_async?model=seed-asr", key, "")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101 (endpoint-first routing, no pin)", code)
	}
	// The upstream was dialed purely off the endpoint — no pin, no model routing.
	if !strings.Contains(up.lastPath, "/sauc/bigmodel_async") {
		t.Errorf("upstream path = %q, want the dialed endpoint (upstream not reached?)", up.lastPath)
	}

	conn.Close()
	rows := waitForRows(t, env, 1)
	if rows[0].Wire != "volc/asr-stream-async" {
		t.Errorf("row wire = %q, want volc/asr-stream-async", rows[0].Wire)
	}
}

// --- WS Test 4b: no pin and a path that matches no enabled wire -> 404
// wire_unmatched, never a blind pipe to an arbitrary vendor origin. ---

func TestWebSocketNoProviderUnmatchedPath(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t"})
	env := newEnv(t, snapshotFunc(t, volcSpeechVendorYAML(mock.URL)), st)

	// This path matches no suffix of the vendor's one enabled wire.
	conn, br := dialProxyWS(t, env.server.URL, "/v1/realtime", key, "")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unmatched WS path without a pin", code)
	}
	if up.lastPath != "" {
		t.Errorf("upstream was dialed for an unmatched path")
	}
}

// --- WS Test 5a: missing/invalid token -> 401 before any dial ---

func TestWebSocketAuthRequired(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	env := newEnv(t, snapshotFunc(t, wsVendorYAML(mock.URL, "rt", "credR", "vendor-rt-secret")), st)

	// Invalid token.
	conn, br := dialProxyWS(t, env.server.URL, "/v1/images/generations", "sg-bogus", "credR")
	if code := readStatusLine(t, br); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for invalid token", code)
	}
	conn.Close()

	// Missing token.
	conn2, br2 := dialProxyWS(t, env.server.URL, "/v1/images/generations", "", "credR")
	if code := readStatusLine(t, br2); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for missing token", code)
	}
	conn2.Close()

	if up.lastAuth != "" {
		t.Errorf("upstream dialed despite auth failure")
	}
	if rows := env.callRows(t); len(rows) != 0 {
		t.Errorf("call rows = %d, want 0 on auth failure", len(rows))
	}
}

// --- WS Test 5b: token scoped to another vendor -> 403 ---

func TestWebSocketScopeRejected(t *testing.T) {
	up := &wsMockUpstream{}
	mock := wsMockServer(t, up)

	st := openStore(t)
	_, key := mustUser(t, st, store.NewUser{Name: "t", Scope: []string{"othervendor"}})
	env := newEnv(t, snapshotFunc(t, wsVendorYAML(mock.URL, "rt", "credR", "vendor-rt-secret")), st)

	conn, br := dialProxyWS(t, env.server.URL, "/v1/images/generations", key, "credR")
	defer conn.Close()

	if code := readStatusLine(t, br); code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (vendor not in scope)", code)
	}
	if up.lastAuth != "" {
		t.Errorf("upstream dialed despite scope rejection")
	}
}

// TestWSUpstreamTarget covers the per-attempt WS path rewrite: a matched wire
// with a path-bearing endpoint dials that endpoint (host + plan path), while an
// origin-only endpoint forwards the inbound path verbatim.
func TestWSUpstreamTarget(t *testing.T) {
	wireName := "volc/asr-stream-async"
	planEP := "https://openspeech.bytedance.com/api/v3/plan/sauc/bigmodel_async"
	mk := func(ep string) router.Target {
		return router.Target{Vendor: config.Vendor{
			Origin:    "https://openspeech.bytedance.com",
			Endpoints: map[string]string{wireName: ep},
		}}
	}
	matched := resolvedWire{wire: wire.Wire{Name: wireName}, matched: true}

	cases := []struct {
		name           string
		target         router.Target
		rw             resolvedWire
		wantHost, want string
	}{
		{
			name:     "path-bearing endpoint rewrites to the plan path",
			target:   mk(planEP),
			rw:       matched,
			wantHost: "openspeech.bytedance.com:443",
			want:     "/api/v3/plan/sauc/bigmodel_async",
		},
		{
			name:     "origin-only endpoint keeps the inbound path",
			target:   mk("https://openspeech.bytedance.com"),
			rw:       matched,
			wantHost: "openspeech.bytedance.com:443",
			want:     "/api/v3/sauc/bigmodel_async",
		},
		{
			name:     "unmatched wire keeps the inbound path",
			target:   mk(planEP),
			rw:       resolvedWire{matched: false},
			wantHost: "openspeech.bytedance.com:443",
			want:     "/api/v3/sauc/bigmodel_async",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, useTLS, reqTarget, err := wsUpstreamTarget(c.target, c.rw, "/api/v3/sauc/bigmodel_async", "")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !useTLS {
				t.Errorf("useTLS = false, want true for https")
			}
			if host != c.wantHost {
				t.Errorf("host = %q, want %q", host, c.wantHost)
			}
			if reqTarget != c.want {
				t.Errorf("requestTarget = %q, want %q", reqTarget, c.want)
			}
		})
	}
}

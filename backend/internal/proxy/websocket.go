package proxy

// WebSocket passthrough. Realtime AI APIs (OpenAI Realtime, DashScope /
// Volcengine streaming ASR/TTS) speak WebSocket, which the HTTP proxy path
// cannot carry — it strips the Upgrade header and buffers the body. This file
// adds a transparent relay: after replaying the client's handshake to the
// upstream (swapping only Authorization for the vendor credential) and getting
// a 101, the handler hijacks the client conn and pipes raw bytes both
// directions, untouched, for the life of the session. WebSocket frames are
// NEVER parsed — we relay the TCP stream and meter only bytes + duration.
//
// All policy (scope, budget, RPM) and credential failover happen BEFORE any
// hijack, so a rejection or an upstream non-101 is a normal HTTP response.

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/pricing"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// wsHandshakeTimeout bounds the upstream dial + handshake. The established pipe
// gets NO deadline: realtime sessions are long-lived and idle for stretches.
const wsHandshakeTimeout = 10 * time.Second

// wsHandshakeHeaders are the headers that carry the WebSocket handshake itself.
// They must survive verbatim when we replay the client's request upstream, even
// though some (Upgrade, Connection) are otherwise hop-by-hop.
var wsHandshakeHeaders = map[string]struct{}{
	"Upgrade":                  {},
	"Connection":               {},
	"Sec-Websocket-Key":        {},
	"Sec-Websocket-Version":    {},
	"Sec-Websocket-Protocol":   {},
	"Sec-Websocket-Extensions": {},
}

// isWebSocketUpgrade reports whether r is a WebSocket upgrade request: the
// Upgrade header is the token "websocket" (case-insensitive) and the Connection
// header lists "upgrade" (case-insensitive, possibly among other tokens).
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return headerContainsToken(r.Header.Get("Connection"), "upgrade")
}

// headerContainsToken reports whether a comma-separated header value contains
// the given token, case-insensitively.
func headerContainsToken(value, token string) bool {
	for _, part := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// wsCompressionNegotiated reports whether the upstream 101 accepted a
// permessage-deflate extension. When it did, frame payloads are DEFLATE-
// compressed and the volc binary protocol can't be read off the raw stream, so
// metering must stand down rather than mis-decode.
func wsCompressionNegotiated(h http.Header) bool {
	for _, v := range h.Values("Sec-WebSocket-Extensions") {
		if strings.Contains(strings.ToLower(v), "permessage-deflate") {
			return true
		}
	}
	return false
}

// handleWebSocket performs a transparent WebSocket passthrough. The request
// carries no body, so it cannot be model-routed: the caller pins the provider
// with the X-Songguo-Provider header (by provider id), and the inbound path is
// relayed verbatim to the chosen vendor's origin. The provider can yield several
// (origin, adapter) vendors; we prefer the one whose enabled wires match the
// path, then try each as a credential/origin attempt until one returns 101.
func (h *handler) handleWebSocket(w http.ResponseWriter, r *http.Request, user store.User) {
	// 1. Resolve the provider pin and its candidate vendors.
	pin := r.Header.Get("X-Songguo-Provider")
	if pin == "" {
		writeError(w, http.StatusBadRequest, "missing_provider",
			"websocket upgrades must set X-Songguo-Provider (cannot be model-routed)")
		return
	}

	targets, err := h.router.CandidatesForProvider(pin)
	if err != nil || len(targets) == 0 {
		writeError(w, http.StatusBadGateway, "no_upstream", "no credentials for provider")
		return
	}
	// Prefer the vendor(s) whose enabled wires match this path; fall back to all
	// of the provider's vendors when none declares the (realtime) wire. Keep the
	// wire map so each attempt can rewrite to its vendor's configured endpoint.
	matchedTargets, wireMap, _ := resolveWires(targets, r.Method, r.URL.Path)
	if len(matchedTargets) > 0 {
		targets = matchedTargets
	}

	// 2. Policy, all BEFORE any hijack so rejections are normal HTTP responses.
	// Scope: a scoped token restricts which vendors it may address.
	if len(user.Scope) > 0 {
		targets = filterScopedVendors(targets, user.Scope)
		if len(targets) == 0 {
			writeError(w, http.StatusForbidden, "vendor_not_allowed", "vendor not allowed for this user")
			return
		}
	}
	if user.Budget != nil {
		spent, err := h.store.SpendByUser(user.ID, nil)
		if err != nil {
			h.logger.Error("budget lookup failed", "err", err)
		} else if spent >= *user.Budget {
			writeError(w, http.StatusPaymentRequired, "budget_exceeded", "budget exceeded")
			return
		}
	}
	if !h.limiter.allow(user.ID, user.RPM) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
		return
	}

	// Billing model: ByteDance openspeech names the billed class in the
	// X-Api-Resource-Id header (mirroring the HTTP path, proxy.go); fall back to
	// the query model. This is what we meter/price as, never used for routing.
	billingModel := r.Header.Get("X-Api-Resource-Id")
	if billingModel == "" {
		billingModel = r.URL.Query().Get("model")
	}

	// 3. Try each candidate until one yields 101. Each target carries its own
	// origin (the provider's per-wire host), so the dial target is per-attempt.
	// We hold dial/handshake state for the winning attempt; failed attempts
	// surface their last HTTP response.
	var (
		upConn    net.Conn
		upReader  *bufio.Reader
		upResp    *http.Response
		chosen    router.Target
		attempt   int
		handshake time.Duration
	)
	for i, t := range targets {
		last := i == len(targets)-1

		host, useTLS, requestTarget, terr := wsUpstreamTarget(t, wireMap[t.Vendor.Name], r.URL.Path, r.URL.RawQuery)
		if terr != nil {
			h.logger.Error("vendor origin invalid", "err", terr, "vendor", t.Vendor.Name)
			h.router.Report(t.Vendor.Name, t.Credential.ID, 0, terr)
			if last {
				writeError(w, http.StatusBadGateway, "upstream_error", "vendor origin invalid")
				return
			}
			continue
		}

		start := h.now()
		conn, reader, resp, derr := h.dialWSUpstream(host, useTLS, requestTarget, r, t)
		if derr != nil {
			h.router.Report(t.Vendor.Name, t.Credential.ID, 0, derr)
			if last {
				h.logger.Error("websocket dial failed", "err", derr, "vendor", t.Vendor.Name)
				writeError(w, http.StatusBadGateway, "upstream_error", derr.Error())
				return
			}
			continue
		}

		if resp.StatusCode == http.StatusSwitchingProtocols {
			upConn, upReader, upResp = conn, reader, resp
			chosen, attempt = t, i+1
			handshake = h.now().Sub(start)
			h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
			break
		}

		// Non-101: this credential failed the upgrade. Remember the response so we
		// can relay the last one, then try the next credential.
		h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
		if last {
			upResp = resp
			chosen, attempt = t, i+1
			handshake = h.now().Sub(start)
			_ = conn.Close()
			break
		}
		drainAndClose(resp.Body)
		_ = conn.Close()
	}

	// 4a. No 101 from any credential: relay the last upstream response verbatim
	// over the normal ResponseWriter (we have NOT hijacked yet) and record it.
	if upConn == nil {
		h.relayFailedHandshake(w, upResp, user.ID, billingModel, chosen.Vendor.Name, chosen, attempt, handshake)
		return
	}

	// 4b. Got 101: hijack the client conn and pipe raw bytes both directions.
	h.pipeWebSocket(w, r, upConn, upReader, upResp, user.ID, billingModel, chosen.Vendor.Name, wireMap[chosen.Vendor.Name], chosen, attempt, handshake)
}

// dialWSUpstream dials the upstream (TLS for wss), writes the replayed
// handshake request with the credential swapped in, and reads the upstream's
// HTTP response. On success it returns the live conn, its buffered reader (which
// may already hold post-handshake bytes), and the parsed response. The caller
// owns closing conn. A non-nil error means the conn was never usable.
func (h *handler) dialWSUpstream(host string, useTLS bool, requestTarget string, r *http.Request, t router.Target) (net.Conn, *bufio.Reader, *http.Response, error) {
	hostname := hostOnly(host)
	dialer := &net.Dialer{Timeout: wsHandshakeTimeout}

	var (
		conn net.Conn
		err  error
	)
	if useTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", host, &tls.Config{ServerName: hostname})
	} else {
		conn, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("dial upstream %q: %w", host, err)
	}

	// Bound dial+handshake; cleared once we have the response so the pipe is
	// deadline-free.
	_ = conn.SetDeadline(h.now().Add(wsHandshakeTimeout))

	reqBytes := buildWSHandshake(host, requestTarget, r, t.Vendor.Adapter, t.Credential.APIKey)
	if _, err := conn.Write(reqBytes); err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("write handshake: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, r)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("read upstream handshake response: %w", err)
	}

	// Clear the deadline: an established realtime session is long-lived.
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, resp, nil
}

// buildWSHandshake assembles the raw HTTP/1.1 upgrade request sent upstream. It
// copies the client's headers EXCEPT the credential header(s) and hop-by-hop
// headers we must control, but KEEPS the WebSocket handshake headers verbatim,
// then injects the chosen credential per the vendor's adapter convention. This
// is a transparent relay of a real client's handshake; browser-specific test
// traffic uses the dedicated /api/test driver (see wstest.go), not this path.
func buildWSHandshake(host, requestTarget string, r *http.Request, adapter, apiKey string) []byte {
	// Header names the adapter owns; we strip any client-sent value and write
	// our own so the upstream only ever sees the vendor credential.
	credHeader := "Authorization"
	if adapter == config.AdapterVolcSpeech {
		credHeader = "X-Api-Key"
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", requestTarget)
	fmt.Fprintf(&b, "Host: %s\r\n", hostHeader(host))

	for key, vals := range r.Header {
		canon := http.CanonicalHeaderKey(key)
		if canon == "Authorization" || canon == http.CanonicalHeaderKey(credHeader) || canon == "Host" {
			continue
		}
		_, isHandshake := wsHandshakeHeaders[canon]
		if _, hop := hopByHopHeaders[canon]; hop && !isHandshake {
			continue
		}
		if canon == "Content-Length" {
			continue
		}
		for _, v := range vals {
			fmt.Fprintf(&b, "%s: %s\r\n", canon, v)
		}
	}

	if adapter == config.AdapterVolcSpeech {
		fmt.Fprintf(&b, "X-Api-Key: %s\r\n", apiKey)
	} else {
		fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", apiKey)
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// relayFailedHandshake forwards a non-101 upstream handshake response to the
// client verbatim (status + headers + body) and records a realtime call row
// with the upstream status and zero bytes.
func (h *handler) relayFailedHandshake(w http.ResponseWriter, resp *http.Response,
	userID, model, vendorName string, t router.Target, attempt int, handshake time.Duration) {
	status := http.StatusBadGateway
	if resp != nil {
		defer resp.Body.Close()
		copyHeaders(w.Header(), resp.Header)
		status = resp.StatusCode
		// Buffer the (small) error body so we can both forward it to the client
		// and log the upstream's reason for refusing the upgrade.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, defaultMaxBodyBytes))
		h.logger.Warn("websocket upstream refused upgrade",
			"vendor", vendorName, "model", model, "credential", t.Credential.ID,
			"status", status, "attempt", attempt, "handshake_ms", handshake.Milliseconds(),
			"body", errorSnippet(body))
		w.WriteHeader(status)
		_, _ = w.Write(body)
	} else {
		h.logger.Warn("websocket upstream refused upgrade (no response)",
			"vendor", vendorName, "model", model, "credential", t.Credential.ID,
			"attempt", attempt, "handshake_ms", handshake.Milliseconds())
		writeError(w, status, "upstream_error", "upstream refused websocket upgrade")
	}

	h.append(calls.Entry{
		TS:           h.now(),
		UserID:       userID,
		Model:        model,
		Modality:     calls.ModalityRealtime,
		Vendor:       vendorName,
		CredentialID: t.Credential.ID,
		Attempt:      attempt,
		Status:       status,
		Usage:        map[string]any{"bytes_up": int64(0), "bytes_down": int64(0), "duration_ms": int64(0)},
		Cost:         0,
		LatencyMS:    handshake.Milliseconds(),
		Stream:       true,
	})
}

// pipeWebSocket hijacks the client conn, completes its handshake by writing the
// upstream's 101 response back, then bidirectionally relays raw bytes until
// either side closes. It meters bytes each way and the session duration, and
// records a single realtime call row at close. For eligible Volcengine speech
// wires it also tees the downstream bytes into an async wsMeter to recover real
// usage and price the session; the relay itself is never blocked by metering.
func (h *handler) pipeWebSocket(w http.ResponseWriter, r *http.Request,
	upConn net.Conn, upReader *bufio.Reader, upResp *http.Response,
	userID, billingModel, vendorName string, rw resolvedWire, t router.Target, attempt int, handshake time.Duration) {
	defer upConn.Close()
	defer upResp.Body.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "connection does not support hijacking")
		return
	}
	clientConn, clientRW, err := hj.Hijack()
	if err != nil {
		h.logger.Error("hijack failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "hijack failed")
		return
	}
	defer clientConn.Close()

	// Complete the client's handshake: write the upstream's 101 status line and
	// handshake headers back over the hijacked conn.
	if err := writeWSResponse(clientRW.Writer, upResp); err != nil {
		h.logger.Error("write client handshake failed", "err", err)
		return
	}

	sessionStart := h.now()
	var bytesUp, bytesDown atomic.Int64

	// Eligible Volcengine speech sessions get an async usage meter: the matched
	// non-zero-cost wire's extractor, fed off the relay's hot path. WS-layer
	// permessage-deflate would garble the volc frames, so skip metering when it
	// was negotiated rather than mis-decode.
	var meter *wsMeter
	if rw.matched && !rw.wire.ZeroCost && rw.wire.Extract != nil &&
		t.Vendor.Adapter == config.AdapterVolcSpeech && !wsCompressionNegotiated(upResp.Header) {
		meter = newWSMeter(rw.wire.Extract, wire.Quirks(t.Vendor.Quirks))
	}

	// One goroutine per direction. Reading from the BUFFERED readers (not the raw
	// conns) is essential: ReadResponse and the hijack may have already pulled
	// post-handshake bytes into those buffers, which would otherwise be lost.
	var wg sync.WaitGroup
	wg.Add(2)

	// client -> upstream
	go func() {
		defer wg.Done()
		n, _ := io.Copy(upConn, clientRW.Reader)
		bytesUp.Add(n)
		// Unblock the other direction by closing both conns.
		_ = upConn.Close()
		_ = clientConn.Close()
	}()

	// upstream -> client (tee'd into the meter when present). Teeing the READER
	// keeps the existing unbuffered relay fast path (bufio.ReadFrom delegates to
	// the conn), while the sink — which never blocks or errors — sees every byte.
	go func() {
		defer wg.Done()
		var src io.Reader = upReader
		if meter != nil {
			src = io.TeeReader(upReader, wsMeterSink{meter})
		}
		n, _ := io.Copy(clientRW.Writer, src)
		bytesDown.Add(n)
		_ = clientRW.Writer.Flush()
		_ = clientConn.Close()
		_ = upConn.Close()
	}()

	// Tear down if the request context is cancelled (server shutdown / client
	// gone); closing the conns unblocks both copies.
	done := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			_ = upConn.Close()
			_ = clientConn.Close()
		case <-done:
		}
	}()

	wg.Wait()
	close(done)

	duration := h.now().Sub(sessionStart)
	bu, bd := bytesUp.Load(), bytesDown.Load()

	// Drain the meter (no-op when nil): decode ran concurrently during the
	// session, so this only waits on the buffered tail. Price the recovered usage
	// against the billing model's table entry; unknown/absent usage stays $0.
	usage := map[string]any{
		"bytes_up":    bu,
		"bytes_down":  bd,
		"duration_ms": duration.Milliseconds(),
	}
	cost := 0.0
	var confidence calls.Confidence
	if meter != nil {
		ext := meter.finish()
		confidence = ext.Confidence
		if len(ext.Raw) > 0 {
			usage["usage"] = ext.Raw
		}
		if snap := h.snapshot(); snap != nil {
			if price, ok := snap.PriceFor(vendorName, billingModel); ok {
				cost = pricing.Cost(price, ext.Norm)
			}
		}
	}

	// The access log only sees the 101 handshake; this is the only record of how
	// the realtime session actually went. A session that relayed no upstream
	// bytes is almost always an application-level rejection the raw byte pipe
	// can't decode (bad config, post-upgrade auth failure), so flag it loudly.
	args := []any{
		"vendor", vendorName, "model", billingModel, "credential", t.Credential.ID,
		"attempt", attempt, "handshake_ms", handshake.Milliseconds(),
		"bytes_up", bu, "bytes_down", bd, "duration_ms", duration.Milliseconds(),
		"cost", cost,
	}
	if bd == 0 {
		h.logger.Warn("websocket session closed with no upstream data", args...)
	} else {
		h.logger.Info("websocket session closed", args...)
	}

	h.append(calls.Entry{
		TS:           h.now(),
		UserID:       userID,
		Model:        billingModel,
		Modality:     calls.ModalityRealtime,
		Vendor:       vendorName,
		CredentialID: t.Credential.ID,
		Confidence:   confidence,
		Attempt:      attempt,
		Status:       http.StatusSwitchingProtocols,
		Usage:        usage,
		Cost:         cost,
		LatencyMS:    handshake.Milliseconds(),
		Stream:       true,
	})
}

// writeWSResponse writes a 101 (or other) handshake response — status line plus
// headers — back to the client over the hijacked writer, so the client library
// sees a complete, verbatim upstream handshake.
func writeWSResponse(w *bufio.Writer, resp *http.Response) error {
	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode)); err != nil {
		return fmt.Errorf("write status line: %w", err)
	}
	for key, vals := range resp.Header {
		for _, v := range vals {
			if _, err := fmt.Fprintf(w, "%s: %s\r\n", key, v); err != nil {
				return fmt.Errorf("write header: %w", err)
			}
		}
	}
	if _, err := w.WriteString("\r\n"); err != nil {
		return fmt.Errorf("write header terminator: %w", err)
	}
	return w.Flush()
}

// wsUpstreamTarget computes the dial host and request line for one WebSocket
// attempt. Like the HTTP path, a matched wire whose endpoint carries a path is a
// rewrite: the upstream URL is the configured endpoint (host + path), so a vendor
// that serves a wire under a non-public base — e.g. a plan account's
// .../api/v3/plan/sauc/... vs the public .../api/v3/sauc/... — is reached
// correctly instead of being dialed at the verbatim inbound path (which the
// upstream rejects, often as a 401). An origin-only endpoint, or an unmatched
// wire, forwards the inbound path to the vendor origin unchanged.
func wsUpstreamTarget(t router.Target, rw resolvedWire, inboundPath, rawQuery string) (host string, useTLS bool, requestTarget string, err error) {
	endpoint := t.Vendor.Origin
	path := inboundPath
	if rw.matched {
		if ep, ok := t.Vendor.Endpoints[rw.wire.Name]; ok && endpointHasPath(ep) {
			endpoint = ep
			if u, perr := url.Parse(ep); perr == nil && u.Path != "" {
				path = u.Path
			}
		}
	}
	host, useTLS, err = wsTargetOf(endpoint)
	return host, useTLS, joinQuery(path, rawQuery), err
}

// wsTargetOf maps a vendor origin to a WebSocket dial target. The scheme is
// mapped https->wss (TLS) and http->ws (plain); the returned host carries a
// port, defaulting to 443 for wss and 80 for ws when the URL omits one.
func wsTargetOf(origin string) (host string, useTLS bool, err error) {
	u, perr := url.Parse(origin)
	if perr != nil {
		return "", false, fmt.Errorf("parse origin %q: %w", origin, perr)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", false, fmt.Errorf("origin %q missing scheme or host", origin)
	}

	switch u.Scheme {
	case "https":
		useTLS = true
	case "http":
		useTLS = false
	default:
		return "", false, fmt.Errorf("origin %q has unsupported scheme %q", origin, u.Scheme)
	}

	hostname := u.Hostname()
	port := u.Port()
	if port == "" {
		if useTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(hostname, port), useTLS, nil
}

// hostOnly returns the hostname portion of a host:port, for the TLS ServerName.
func hostOnly(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return h
}

// hostHeader returns the value for the upstream Host header: host:port, but
// dropping the default port for the scheme so it matches what a browser would
// send.
func hostHeader(hostPort string) string {
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	if p == "80" || p == "443" {
		return h
	}
	return hostPort
}

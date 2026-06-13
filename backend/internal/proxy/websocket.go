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
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/store"
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

// handleWebSocket performs a transparent WebSocket passthrough for a request
// mounted at /x/<vendor>/<rest>. rest is the path remainder after "/x/".
func (h *handler) handleWebSocket(w http.ResponseWriter, r *http.Request, user store.User, rest string) {
	// 1. Resolve the vendor and the upstream WS origin from its base_url.
	vendorName, restPath, ok := strings.Cut(rest, "/")
	if !ok || vendorName == "" || restPath == "" {
		writeError(w, http.StatusNotFound, "not_found", "expected /x/<vendor>/<path>")
		return
	}
	restPath = "/" + restPath

	snap := h.snapshot()
	if snap == nil {
		writeError(w, http.StatusNotFound, "unknown_vendor", "unknown vendor")
		return
	}
	vendor, ok := snap.Vendor(vendorName)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_vendor", "unknown vendor")
		return
	}

	host, useTLS, err := wsTargetOf(vendor.BaseURL)
	if err != nil {
		h.logger.Error("vendor base_url invalid", "err", err, "vendor", vendorName)
		writeError(w, http.StatusBadGateway, "upstream_error", "vendor base_url invalid")
		return
	}

	// 2. Policy, all BEFORE any hijack so rejections are normal HTTP responses.
	// Scope: a scoped token restricts which vendors it may address.
	if len(user.Scope) > 0 && !contains(user.Scope, vendorName) {
		writeError(w, http.StatusForbidden, "vendor_not_allowed", "vendor not allowed for this user")
		return
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

	// 3. Candidate credentials for this vendor (rotated key pool).
	targets, err := h.router.CandidatesForVendor(vendorName)
	if err != nil || len(targets) == 0 {
		writeError(w, http.StatusBadGateway, "no_upstream", "no credentials for vendor")
		return
	}

	requestTarget := joinQuery(restPath, r.URL.RawQuery)
	model := r.URL.Query().Get("model") // best-effort; realtime model often in query

	// 4. Try each credential until one yields 101. We hold dial/handshake state
	// for the winning attempt; failed attempts surface their last HTTP response.
	var (
		upConn    net.Conn
		upReader  *bufio.Reader
		upResp    *http.Response
		chosen    router.Target
		attempt   int
		handshake time.Duration
	)
	for i, t := range targets {
		start := h.now()
		conn, reader, resp, derr := h.dialWSUpstream(host, useTLS, requestTarget, r, t)
		if derr != nil {
			h.router.Report(t.Vendor.Name, t.Credential.ID, 0, derr)
			if i == len(targets)-1 {
				h.logger.Error("websocket dial failed", "err", derr, "vendor", vendorName)
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
		if i == len(targets)-1 {
			upResp = resp
			chosen, attempt = t, i+1
			handshake = h.now().Sub(start)
			_ = conn.Close()
			break
		}
		drainAndClose(resp.Body)
		_ = conn.Close()
	}

	// 5a. No 101 from any credential: relay the last upstream response verbatim
	// over the normal ResponseWriter (we have NOT hijacked yet) and record it.
	if upConn == nil {
		h.relayFailedHandshake(w, upResp, user.ID, model, vendorName, chosen, attempt, handshake)
		return
	}

	// 5b. Got 101: hijack the client conn and pipe raw bytes both directions.
	h.pipeWebSocket(w, r, upConn, upReader, upResp, user.ID, model, vendorName, chosen, attempt, handshake)
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
// then injects the chosen credential per the vendor's adapter convention.
// Building the bytes explicitly guarantees the handshake headers survive
// untouched.
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
		w.WriteHeader(status)
		_, _ = io.Copy(w, resp.Body)
	} else {
		writeError(w, status, "upstream_error", "upstream refused websocket upgrade")
	}

	h.append(calls.Entry{
		TS:           h.now(),
		UserID:      userID,
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
// records a single realtime call row at close.
func (h *handler) pipeWebSocket(w http.ResponseWriter, r *http.Request,
	upConn net.Conn, upReader *bufio.Reader, upResp *http.Response,
	userID, model, vendorName string, t router.Target, attempt int, handshake time.Duration) {
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

	// upstream -> client
	go func() {
		defer wg.Done()
		n, _ := io.Copy(clientRW.Writer, upReader)
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
	h.append(calls.Entry{
		TS:           h.now(),
		UserID:      userID,
		Model:        model,
		Modality:     calls.ModalityRealtime,
		Vendor:       vendorName,
		CredentialID: t.Credential.ID,
		Attempt:      attempt,
		Status:       http.StatusSwitchingProtocols,
		Usage: map[string]any{
			"bytes_up":    bytesUp.Load(),
			"bytes_down":  bytesDown.Load(),
			"duration_ms": duration.Milliseconds(),
		},
		Cost:      0, // realtime pricing deferred
		LatencyMS: handshake.Milliseconds(),
		Stream:    true,
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

// wsTargetOf maps a vendor base_url to a WebSocket dial target. The scheme is
// mapped https->wss (TLS) and http->ws (plain); the returned host carries a
// port, defaulting to 443 for wss and 80 for ws when the URL omits one.
func wsTargetOf(base string) (host string, useTLS bool, err error) {
	u, perr := url.Parse(base)
	if perr != nil {
		return "", false, fmt.Errorf("parse base_url %q: %w", base, perr)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", false, fmt.Errorf("base_url %q missing scheme or host", base)
	}

	switch u.Scheme {
	case "https":
		useTLS = true
	case "http":
		useTLS = false
	default:
		return "", false, fmt.Errorf("base_url %q has unsupported scheme %q", base, u.Scheme)
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

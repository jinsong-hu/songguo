// Package proxy transparently forwards AI requests, swapping only credentials.
//
// The handler is a gate plus a meter: it authenticates the consumer token,
// enforces scope, budget and rate limits, routes the request to an upstream
// vendor (with failover), and records every attempt as a call. It NEVER
// rewrites the request or response body — the only mutation is the
// Authorization header, swapped from the consumer's Songguo token to the chosen
// upstream credential. Metering is read-only sniffing and must never block or
// alter traffic.
//
// Two routing modes share this handler, decided by the request path:
//
//   - Model-routed (/v1/...): the ergonomic default for OpenAI-compatible SDKs.
//     The model is read from the body, candidates span every vendor serving it
//     (priority/weighted-RR/failover), and the upstream URL is the vendor's
//     published base_url plus the path suffix after /v1.
//   - Passthrough (/x/<vendor>/...): the caller pins a vendor by name; no model
//     is required (so model-less async polls work), failover is across that
//     vendor's own credentials only, and the rest of the path is forwarded to
//     the vendor's host origin (base_url with its path stripped).
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/meter"
	"github.com/songguo/songguo/internal/pricing"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/store"
)

// defaultMaxBodyBytes bounds both the buffered request body and a non-streaming
// upstream response body.
const defaultMaxBodyBytes int64 = 25 << 20 // 25 MiB

// hopByHopHeaders are connection-specific headers that must not be forwarded in
// either direction. Content-Length is handled separately (recomputed by the
// transport / ResponseWriter).
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// Deps are the collaborators a Handler needs.
type Deps struct {
	Snapshot     func() *config.Snapshot
	Store        *store.Store
	Router       *router.Router
	Logger       *slog.Logger
	HTTPClient   *http.Client     // optional; default constructed if nil
	Now          func() time.Time // optional; defaults to time.Now (for tests)
	MaxBodyBytes int64            // optional; default ~25MiB
}

// handler is the concrete http.Handler returned by NewHandler.
type handler struct {
	snapshot     func() *config.Snapshot
	store        *store.Store
	router       *router.Router
	logger       *slog.Logger
	client       *http.Client
	now          func() time.Time
	maxBodyBytes int64
	limiter      *rateLimiter
}

// NewHandler builds the transparent proxy handler.
func NewHandler(d Deps) http.Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	client := d.HTTPClient
	if client == nil {
		client = defaultHTTPClient()
	}
	max := d.MaxBodyBytes
	if max <= 0 {
		max = defaultMaxBodyBytes
	}
	return &handler{
		snapshot:     d.Snapshot,
		store:        d.Store,
		router:       d.Router,
		logger:       logger,
		client:       client,
		now:          now,
		maxBodyBytes: max,
		limiter:      newRateLimiter(now),
	}
}

// defaultHTTPClient returns a client tuned for proxying, including long-lived
// streams: it sets connect/TLS/header timeouts but NO overall Client.Timeout,
// which would truncate streaming responses. Per-request cancellation is honored
// through the request context.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Auth.
	key := bearerToken(r.Header.Get("Authorization"))
	if key == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing authorization")
		return
	}
	token, err := h.store.GetTokenByKey(key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
			return
		}
		h.logger.Error("token lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "token lookup failed")
		return
	}

	// 2. Buffer the request body, bounded.
	body, tooLarge, err := readBounded(r.Body, h.maxBodyBytes)
	if tooLarge {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body too large")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}

	// 3. Resolve the route. Mode is decided by the mount path: explicit-vendor
	// passthrough under /x/, otherwise model-routed under /v1/. Resolution sets
	// the model/modality, the candidate targets (with their failover policy),
	// and the per-target upstream-URL builder.
	rt, ok := h.resolve(w, r, token, body)
	if !ok {
		return
	}

	// 4. Budget (coarse pre-check).
	if token.Budget != nil {
		spent, err := h.store.SpendByToken(token.ID, nil)
		if err != nil {
			h.logger.Error("budget lookup failed", "err", err)
		} else if spent >= *token.Budget {
			writeError(w, http.StatusPaymentRequired, "budget_exceeded", "budget exceeded")
			return
		}
	}

	// 5. Rate limit.
	if !h.limiter.allow(token.ID, token.RPM) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
		return
	}

	tags := extractTags(r.Header.Get("X-Songguo-Tags"), body)

	// 6. Forward with failover. The loop is identical for both modes; only the
	// candidate set and the upstream URL (built by rt.upstreamURL) differ.
	for i, t := range rt.targets {
		attempt := i + 1
		last := i == len(rt.targets)-1

		upReq, err := h.buildUpstreamRequest(r, t, rt.upstreamURL(t), body)
		if err != nil {
			h.logger.Error("build upstream request failed", "err", err, "vendor", t.Vendor.Name)
			h.recordFailure(token.ID, rt.model, rt.modality, t, attempt, 0, err, 0, tags)
			h.router.Report(t.Vendor.Name, t.Credential.ID, 0, err)
			if last {
				writeError(w, http.StatusBadGateway, "upstream_error", "failed to build upstream request")
				return
			}
			continue
		}

		start := h.now()
		resp, err := h.client.Do(upReq)
		latency := h.now().Sub(start).Milliseconds()

		// Transport error: failover-eligible.
		if err != nil {
			h.recordFailure(token.ID, rt.model, rt.modality, t, attempt, 0, err, latency, tags)
			h.router.Report(t.Vendor.Name, t.Credential.ID, 0, err)
			if last {
				// Transparency: surface the real transport failure verbatim
				// (we have no upstream response to forward).
				writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
				return
			}
			continue
		}

		failover := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if failover && !last {
			h.recordFailure(token.ID, rt.model, rt.modality, t, attempt, resp.StatusCode,
				fmt.Errorf("upstream status %d", resp.StatusCode), latency, tags)
			h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
			drainAndClose(resp.Body)
			continue
		}

		// This is the chosen response (either a non-failover status, or the last
		// target even if it failed). Report health, then forward verbatim.
		h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
		h.forward(w, r, resp, token.ID, rt.model, rt.modality, t, attempt, latency, tags)
		return
	}
}

// route is the resolved plan for a request: the targets to try (in order, with
// their failover policy already encoded as a set of own credentials or a
// cross-vendor list), the model/modality to record, and a per-target builder
// for the upstream URL.
type route struct {
	model       string
	modality    calls.Modality
	targets     []router.Target
	upstreamURL func(router.Target) string
}

// resolve dispatches on the request path to build the route, enforcing
// per-mode scope and writing any error response itself. It returns ok=false
// when it has already responded.
func (h *handler) resolve(w http.ResponseWriter, r *http.Request, token store.Token, body []byte) (route, bool) {
	if rest, isX := strings.CutPrefix(r.URL.Path, "/x/"); isX {
		return h.resolvePassthrough(w, r, token, body, rest)
	}
	return h.resolveModelRouted(w, r, token, body)
}

// resolveModelRouted handles Mode A: model-routed traffic mounted at /v1/. The
// upstream path is the request path with the /v1 mount prefix stripped,
// appended to the vendor's published (version-inclusive) base_url.
func (h *handler) resolveModelRouted(w http.ResponseWriter, r *http.Request, token store.Token, body []byte) (route, bool) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1")

	res := meter.Classify(r.Method, r.URL.Path, body)
	if res.Model == "" {
		writeError(w, http.StatusBadRequest, "missing_model", "missing model")
		return route{}, false
	}

	// Scope: the model must be allowed when the token is scoped.
	if len(token.Scope) > 0 && !contains(token.Scope, res.Model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "model not allowed for this token")
		return route{}, false
	}

	targets, err := h.router.Candidates(res.Model)
	if err != nil {
		if errors.Is(err, router.ErrNoVendor) {
			writeError(w, http.StatusBadGateway, "no_upstream", "no upstream for model")
			return route{}, false
		}
		h.logger.Error("routing failed", "err", err)
		writeError(w, http.StatusBadGateway, "no_upstream", "routing failed")
		return route{}, false
	}

	return route{
		model:    res.Model,
		modality: res.Modality,
		targets:  targets,
		upstreamURL: func(t router.Target) string {
			return joinQuery(strings.TrimRight(t.Vendor.BaseURL, "/")+suffix, r.URL.RawQuery)
		},
	}, true
}

// resolvePassthrough handles Mode B: explicit-vendor passthrough mounted at
// /x/<vendor>/<rest>. No model is required (this is what lets model-less calls
// like async GET /api/v1/tasks/{id} polls through). The rest of the path is
// appended to the vendor's host origin (base_url's scheme://host, path
// stripped), so native/async/rerank endpoints are reachable. Failover is across
// the named vendor's own credentials only.
func (h *handler) resolvePassthrough(w http.ResponseWriter, r *http.Request, token store.Token, body []byte, rest string) (route, bool) {
	vendorName, rest, ok := strings.Cut(rest, "/")
	if !ok || vendorName == "" || rest == "" {
		writeError(w, http.StatusNotFound, "not_found", "expected /x/<vendor>/<path>")
		return route{}, false
	}
	rest = "/" + rest

	snap := h.snapshot()
	if snap == nil {
		writeError(w, http.StatusNotFound, "unknown_vendor", "unknown vendor")
		return route{}, false
	}
	vendor, ok := snap.Vendor(vendorName)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_vendor", "unknown vendor")
		return route{}, false
	}

	// Scope: in passthrough mode a scoped token restricts which VENDORS it may
	// address, not which models (model is often absent here).
	if len(token.Scope) > 0 && !contains(token.Scope, vendorName) {
		writeError(w, http.StatusForbidden, "vendor_not_allowed", "vendor not allowed for this token")
		return route{}, false
	}

	origin, err := originOf(vendor.BaseURL)
	if err != nil {
		h.logger.Error("vendor base_url invalid", "err", err, "vendor", vendorName)
		writeError(w, http.StatusBadGateway, "upstream_error", "vendor base_url invalid")
		return route{}, false
	}

	targets, err := h.router.CandidatesForVendor(vendorName)
	if err != nil {
		writeError(w, http.StatusBadGateway, "no_upstream", "no credentials for vendor")
		return route{}, false
	}

	// Best-effort metering only; model may be empty for model-less calls.
	res := meter.Classify(r.Method, rest, body)

	return route{
		model:    res.Model,
		modality: res.Modality,
		targets:  targets,
		upstreamURL: func(router.Target) string {
			return joinQuery(origin+rest, r.URL.RawQuery)
		},
	}, true
}

// buildUpstreamRequest constructs the upstream request: the given URL, the
// original method, a fresh body reader over the buffered bytes, all original
// headers minus hop-by-hop and Content-Length, and the only mutation —
// Authorization set to the chosen credential.
func (h *handler) buildUpstreamRequest(r *http.Request, t router.Target, upURL string, body []byte) (*http.Request, error) {
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytesReader(body))
	if err != nil {
		return nil, fmt.Errorf("new upstream request: %w", err)
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.ContentLength = int64(len(body))
	upReq.Header.Set("Authorization", "Bearer "+t.Credential.APIKey)
	return upReq, nil
}

// joinQuery appends a raw query string to a URL if non-empty.
func joinQuery(u, rawQuery string) string {
	if rawQuery == "" {
		return u
	}
	return u + "?" + rawQuery
}

// originOf returns the scheme://host of a base URL, stripping any path so
// passthrough can target native endpoints that live outside the vendor's
// OpenAI-compatible path prefix.
func originOf(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse base_url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base_url %q missing scheme or host", base)
	}
	return u.Scheme + "://" + u.Host, nil
}

// forward copies the chosen upstream response to the client verbatim and sniffs
// usage as it passes. Streaming responses are streamed chunk-by-chunk and
// flushed; non-streaming responses are buffered (bounded) and written whole.
func (h *handler) forward(w http.ResponseWriter, r *http.Request, resp *http.Response,
	tokenID, model string, modality calls.Modality, t router.Target, attempt int,
	latency int64, tags map[string]string) {
	defer resp.Body.Close()

	stream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var usage map[string]any
	if stream {
		usage = h.streamBody(r.Context(), w, resp.Body)
	} else {
		usage = h.copyBody(w, resp.Body)
	}

	cost := 0.0
	if snap := h.snapshot(); snap != nil {
		if price, ok := snap.PriceFor(t.Vendor.Name, model); ok {
			cost = pricing.Cost(price, usage)
		}
	}

	h.append(calls.Entry{
		TS:           h.now(),
		TokenID:      tokenID,
		Model:        model,
		Modality:     modality,
		Vendor:       t.Vendor.Name,
		CredentialID: t.Credential.ID,
		Attempt:      attempt,
		Status:       resp.StatusCode,
		Usage:        usage,
		Cost:         cost,
		LatencyMS:    latency,
		Stream:       stream,
		Tags:         tags,
	})
}

// streamBody tees the SSE stream to both the client and a usage scanner,
// flushing after each chunk so nothing is buffered. It returns the sniffed
// usage (possibly nil).
func (h *handler) streamBody(ctx context.Context, w http.ResponseWriter, src io.Reader) map[string]any {
	scanner := meter.NewStreamUsageScanner()
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				break
			}
			_, _ = scanner.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	return scanner.Usage()
}

// copyBody reads the full (bounded) non-streaming body, writes it to the client
// unchanged, and extracts usage from it.
func (h *handler) copyBody(w http.ResponseWriter, src io.Reader) map[string]any {
	body, _, err := readBounded(src, h.maxBodyBytes)
	if err != nil {
		h.logger.Error("read upstream body failed", "err", err)
	}
	if len(body) > 0 {
		if _, werr := w.Write(body); werr != nil {
			h.logger.Error("write client body failed", "err", werr)
		}
	}
	return meter.ExtractUsage(body)
}

// recordFailure appends a call row for a failed (failover-eligible) attempt.
func (h *handler) recordFailure(tokenID, model string, modality calls.Modality,
	t router.Target, attempt, status int, err error, latency int64, tags map[string]string) {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	h.append(calls.Entry{
		TS:           h.now(),
		TokenID:      tokenID,
		Model:        model,
		Modality:     modality,
		Vendor:       t.Vendor.Name,
		CredentialID: t.Credential.ID,
		Attempt:      attempt,
		Status:       status,
		Err:          detail,
		Cost:         0,
		LatencyMS:    latency,
		Tags:         tags,
	})
}

// append writes a call entry, logging (never surfacing) any failure.
func (h *handler) append(e calls.Entry) {
	if _, err := h.store.AppendCall(e); err != nil {
		h.logger.Error("call append failed", "err", err, "vendor", e.Vendor, "model", e.Model)
	}
}

// --- helpers ---

// bearerToken extracts the key from an Authorization header value, accepting
// either "Bearer <key>" (case-insensitive scheme) or a raw "<key>".
func bearerToken(header string) string {
	h := strings.TrimSpace(header)
	if h == "" {
		return ""
	}
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

// readBounded reads up to max bytes from r. If the source has more than max
// bytes it returns tooLarge=true.
func readBounded(r io.Reader, max int64) (body []byte, tooLarge bool, err error) {
	if r == nil {
		return nil, false, nil
	}
	// Read one extra byte to detect overflow.
	limited := io.LimitReader(r, max+1)
	body, err = io.ReadAll(limited)
	if err != nil {
		return nil, false, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > max {
		return nil, true, nil
	}
	return body, false, nil
}

// bytesReader returns a fresh reader over b suitable for an http.Request body.
// A nil/empty body yields http.NoBody so no Content-Length confusion arises.
func bytesReader(b []byte) io.Reader {
	if len(b) == 0 {
		return http.NoBody
	}
	return strings.NewReader(string(b))
}

// copyHeaders copies all of src into dst except hop-by-hop headers and
// Content-Length (which the transport / writer recomputes).
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if _, hop := hopByHopHeaders[http.CanonicalHeaderKey(k)]; hop {
			continue
		}
		if http.CanonicalHeaderKey(k) == "Content-Length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// drainAndClose discards and closes a response body so the connection can be
// reused.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// extractTags builds the call tags from, in order of precedence, the
// X-Songguo-Tags header (a JSON string map) then a top-level "metadata" object
// of string->string in the request body. Any parse error is ignored.
func extractTags(headerVal string, body []byte) map[string]string {
	out := map[string]string{}

	if len(body) > 0 {
		var env struct {
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(body, &env); err == nil {
			for k, v := range env.Metadata {
				out[k] = v
			}
		}
	}

	if headerVal != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(headerVal), &m); err == nil {
			for k, v := range m {
				out[k] = v
			}
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// errorBody is the JSON error envelope returned for gateway-originated errors.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// writeError writes a gateway error in the OpenAI-compatible shape.
func writeError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{
		Message: message,
		Type:    "songguo_" + reason,
	}})
}

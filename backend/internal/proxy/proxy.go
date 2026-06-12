// Package proxy transparently forwards AI requests, swapping only credentials.
//
// The handler is a gate plus a meter: it authenticates the consumer user,
// enforces scope, budget and rate limits, routes the request to an upstream
// vendor (with failover), and records every attempt as a call. It NEVER
// rewrites the request or response body — the only mutations are the
// credential headers and, when a service explicitly opts in via the
// inject_stream_usage quirk, setting stream_options.include_usage so streamed
// calls stay meterable. Metering is read-only sniffing and must never block
// or alter traffic.
//
// Every request must resolve to a wire (see internal/wire): the service's
// enabled wire whose path pattern matches. The wire owns usage extraction and
// the call's modality. Paths matching no enabled wire are denied — every
// forwarded call must have a pricing rule — unless the service sets
// allow_unmatched, which forwards the bytes metered-zero at unknown
// confidence.
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
	"github.com/songguo/songguo/internal/wire"
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
	user, err := h.store.GetUserByKey(key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid user key")
			return
		}
		h.logger.Error("user lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "user lookup failed")
		return
	}

	// 1b. WebSocket upgrade detection. A WS handshake must be relayed as a raw
	// byte pipe (see handleWebSocket); it cannot be model-routed (the model
	// lives only in the body, and there is no body to buffer here), so only the
	// explicit-vendor /x/ mount supports it. We branch BEFORE buffering the body
	// so an upgrade is never read as an HTTP body.
	if isWebSocketUpgrade(r) {
		if rest, isX := strings.CutPrefix(r.URL.Path, "/x/"); isX {
			h.handleWebSocket(w, r, user, rest)
			return
		}
		writeError(w, http.StatusUpgradeRequired, "upgrade_required",
			"websocket upgrades must use /x/<vendor>/... (cannot be model-routed)")
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

	// Decide capture once: a per-user override (if set) wins over the global
	// snapshot setting. The settings are also resolved here so the cap/retain
	// used for this request are stable even if config hot-reloads mid-flight.
	capCfg := h.captureFor(user)

	// 3. Resolve the route. Mode is decided by the mount path: explicit-vendor
	// passthrough under /x/, otherwise model-routed under /v1/. Resolution sets
	// the model/modality, the candidate targets (with their failover policy),
	// and the per-target upstream-URL builder.
	rt, ok := h.resolve(w, r, user, body)
	if !ok {
		return
	}

	// 4. Budget (coarse pre-check).
	if user.Budget != nil {
		spent, err := h.store.SpendByUser(user.ID, nil)
		if err != nil {
			h.logger.Error("budget lookup failed", "err", err)
		} else if spent >= *user.Budget {
			writeError(w, http.StatusPaymentRequired, "budget_exceeded", "budget exceeded")
			return
		}
	}

	// 5. Rate limit.
	if !h.limiter.allow(user.ID, user.RPM) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
		return
	}

	tags := extractTags(r.Header.Get("X-Songguo-Tags"), body)

	// 6. Forward with failover. The loop is identical for both modes; only the
	// candidate set and the upstream URL (built by rt.upstreamURL) differ.
	for i, t := range rt.targets {
		attempt := i + 1
		last := i == len(rt.targets)-1
		rw := rt.wires[t.Vendor.Name]
		modality := rt.modalityFor(t.Vendor.Name)

		// The one sanctioned body mutation, per-service opt-in: ask the
		// upstream to report usage on streamed calls so they stay meterable.
		upBody := body
		if rw.matched && rw.quirks[quirkInjectStreamUsage] == "true" {
			upBody = injectStreamUsage(body)
		}

		upReq, err := h.buildUpstreamRequest(r, t, rt.upstreamURL(t), upBody)
		if err != nil {
			h.logger.Error("build upstream request failed", "err", err, "vendor", t.Vendor.Name)
			h.recordFailure(user.ID, rt.model, modality, t, attempt, 0, err, 0, tags)
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
			h.recordFailure(user.ID, rt.model, modality, t, attempt, 0, err, latency, tags)
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
			h.recordFailure(user.ID, rt.model, modality, t, attempt, resp.StatusCode,
				fmt.Errorf("upstream status %d", resp.StatusCode), latency, tags)
			h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
			drainAndClose(resp.Body)
			continue
		}

		// This is the chosen response (either a non-failover status, or the last
		// target even if it failed). Report health, then forward verbatim.
		h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
		h.forward(w, r, resp, user.ID, rt.model, modality, rw, t, attempt, latency, tags, capCfg, body)
		return
	}
}

// route is the resolved plan for a request: the targets to try (in order, with
// their failover policy already encoded as a set of own credentials or a
// cross-vendor list), the model/modality to record, a per-target builder for
// the upstream URL, and the per-vendor resolved wire that owns metering.
type route struct {
	model       string
	modality    calls.Modality
	targets     []router.Target
	upstreamURL func(router.Target) string
	wires       map[string]resolvedWire // keyed by vendor name
}

// resolvedWire is the metering plan for one candidate vendor: the matched wire
// (or matched=false for an allow_unmatched passthrough) plus the vendor's
// quirk flags.
type resolvedWire struct {
	wire    wire.Wire
	matched bool
	quirks  wire.Quirks
}

// modalityFor returns the modality to record for a vendor: the matched wire's
// modality, falling back to the route-level classification.
func (rt route) modalityFor(vendorName string) calls.Modality {
	if rw, ok := rt.wires[vendorName]; ok && rw.matched && rw.wire.Modality != "" {
		return rw.wire.Modality
	}
	return rt.modality
}

// resolveWires matches the upstream path against each candidate vendor's
// enabled wires, dropping vendors that match none (unless they allow
// unmatched passthrough). It returns the surviving targets, their metering
// plans, and the names of vendors that denied the path.
func resolveWires(targets []router.Target, method, path string) (kept []router.Target, wires map[string]resolvedWire, denied []string) {
	wires = make(map[string]resolvedWire, len(targets))
	for _, t := range targets {
		if _, seen := wires[t.Vendor.Name]; seen {
			kept = append(kept, t)
			continue
		}
		w, ok := wire.Resolve(t.Vendor.Wires, method, path)
		switch {
		case ok:
			wires[t.Vendor.Name] = resolvedWire{wire: w, matched: true, quirks: wire.Quirks(t.Vendor.Quirks)}
			kept = append(kept, t)
		case t.Vendor.AllowUnmatched:
			wires[t.Vendor.Name] = resolvedWire{quirks: wire.Quirks(t.Vendor.Quirks)}
			kept = append(kept, t)
		default:
			denied = append(denied, t.Vendor.Name)
		}
	}
	return kept, wires, denied
}

// denyUnmatched rejects a request whose path matched no enabled wire on any
// candidate, and records the rejection as a call row so it surfaces on the
// dashboard (the signal that a wire mapping is missing).
func (h *handler) denyUnmatched(w http.ResponseWriter, r *http.Request, userID, model, path string, vendors []string) {
	detail := fmt.Sprintf("no enabled wire matches %s %s on service %s; add a wire mapping or enable allow_unmatched",
		r.Method, path, strings.Join(vendors, ", "))
	h.append(calls.Entry{
		TS:         h.now(),
		UserID:    userID,
		Model:      model,
		Vendor:     strings.Join(vendors, ","),
		Status:     http.StatusNotFound,
		Err:        "unmatched: " + r.Method + " " + path,
		Confidence: calls.ConfidenceUnknown,
	})
	writeError(w, http.StatusNotFound, "wire_unmatched", detail)
}

// resolve dispatches on the request path to build the route, enforcing
// per-mode scope and writing any error response itself. It returns ok=false
// when it has already responded.
func (h *handler) resolve(w http.ResponseWriter, r *http.Request, user store.User, body []byte) (route, bool) {
	if rest, isX := strings.CutPrefix(r.URL.Path, "/x/"); isX {
		return h.resolvePassthrough(w, r, user, body, rest)
	}
	return h.resolveModelRouted(w, r, user, body)
}

// resolveModelRouted handles Mode A: model-routed traffic mounted at /v1/. The
// upstream path is the request path with the /v1 mount prefix stripped,
// appended to the vendor's published (version-inclusive) base_url.
func (h *handler) resolveModelRouted(w http.ResponseWriter, r *http.Request, user store.User, body []byte) (route, bool) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1")

	res := meter.Classify(r.Method, r.URL.Path, body)
	if res.Model == "" {
		writeError(w, http.StatusBadRequest, "missing_model", "missing model")
		return route{}, false
	}

	// Scope: the model must be allowed when the user is scoped.
	if len(user.Scope) > 0 && !contains(user.Scope, res.Model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "model not allowed for this user")
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

	kept, wires, denied := resolveWires(targets, r.Method, suffix)
	if len(kept) == 0 {
		h.denyUnmatched(w, r, user.ID, res.Model, suffix, denied)
		return route{}, false
	}

	return route{
		model:    res.Model,
		modality: res.Modality,
		targets:  kept,
		wires:    wires,
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
func (h *handler) resolvePassthrough(w http.ResponseWriter, r *http.Request, user store.User, body []byte, rest string) (route, bool) {
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

	// Scope: in passthrough mode a scoped user restricts which VENDORS it may
	// address, not which models (model is often absent here).
	if len(user.Scope) > 0 && !contains(user.Scope, vendorName) {
		writeError(w, http.StatusForbidden, "vendor_not_allowed", "vendor not allowed for this user")
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

	kept, wires, denied := resolveWires(targets, r.Method, rest)
	if len(kept) == 0 {
		h.denyUnmatched(w, r, user.ID, res.Model, rest, denied)
		return route{}, false
	}

	return route{
		model:    res.Model,
		modality: res.Modality,
		targets:  kept,
		wires:    wires,
		upstreamURL: func(router.Target) string {
			return joinQuery(origin+rest, r.URL.RawQuery)
		},
	}, true
}

// buildUpstreamRequest constructs the upstream request: the given URL, the
// original method, a fresh body reader over the buffered bytes, all original
// headers minus hop-by-hop and Content-Length, and the only mutation —
// the credential, applied per the vendor's adapter convention.
func (h *handler) buildUpstreamRequest(r *http.Request, t router.Target, upURL string, body []byte) (*http.Request, error) {
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upURL, bytesReader(body))
	if err != nil {
		return nil, fmt.Errorf("new upstream request: %w", err)
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.ContentLength = int64(len(body))
	applyUpstreamAuth(upReq, t.Vendor.Adapter, t.Credential.APIKey)
	return upReq, nil
}

// applyUpstreamAuth swaps in the upstream credential using the header style the
// vendor's adapter expects. This is the proxy's only request mutation; the body
// is never touched. An unknown/empty adapter defaults to OpenAI-style bearer.
func applyUpstreamAuth(req *http.Request, adapter, key string) {
	switch adapter {
	case config.AdapterAnthropic:
		// Anthropic authenticates with x-api-key and requires an API version
		// header; strip any inherited bearer so only the upstream key is sent.
		req.Header.Del("Authorization")
		req.Header.Set("X-Api-Key", key)
		if req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// quirkInjectStreamUsage is the per-service opt-in flag (value "true") that
// lets the proxy set stream_options.include_usage on streamed requests. Some
// vendors (DeepSeek, Ark, DashScope) omit usage from SSE streams unless the
// client asked for it, which would leave those calls metered zero. This is the
// proxy's only body mutation and is off by default.
const quirkInjectStreamUsage = "inject_stream_usage"

// injectStreamUsage returns the body with stream_options.include_usage set to
// true when (and only when) the body is a JSON object requesting a stream and
// the option is not already present. Any other shape is returned unchanged.
func injectStreamUsage(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber() // preserve number representations across the re-marshal
	if err := dec.Decode(&obj); err != nil {
		return body
	}
	if stream, _ := obj["stream"].(bool); !stream {
		return body
	}
	opts, _ := obj["stream_options"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	} else if _, set := opts["include_usage"]; set {
		return body
	}
	opts["include_usage"] = true
	obj["stream_options"] = opts
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
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

// captureConfig is the resolved, per-request capture decision plus the caps to
// apply when it is enabled. It is computed once so a mid-request config reload
// cannot change the behaviour for an in-flight call.
type captureConfig struct {
	on       bool
	maxBytes int
	retain   int
}

// captureFor resolves whether to capture this request: a non-nil per-token
// override wins, otherwise the global snapshot setting decides. The caps come
// from the snapshot regardless.
func (h *handler) captureFor(user store.User) captureConfig {
	var settings config.Settings
	if snap := h.snapshot(); snap != nil {
		settings = snap.Settings()
	}
	on := settings.Capture
	if user.Capture != nil {
		on = *user.Capture
	}
	return captureConfig{on: on, maxBytes: settings.CaptureMaxBytes, retain: settings.CaptureRetain}
}

// forward copies the chosen upstream response to the client verbatim and sniffs
// usage as it passes, using the resolved wire's extractor. Streaming responses
// are streamed chunk-by-chunk and flushed; non-streaming responses are buffered
// (bounded) and written whole. When capture is on, it also tees a capped copy
// of the response body and persists the redacted request/response payload after
// the call row is written.
func (h *handler) forward(w http.ResponseWriter, r *http.Request, resp *http.Response,
	userID, model string, modality calls.Modality, rw resolvedWire, t router.Target, attempt int,
	latency int64, tags map[string]string, capCfg captureConfig, reqBody []byte) {
	defer resp.Body.Close()

	stream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var (
		ext           wire.Extraction
		respBody      []byte
		respTruncated bool
	)
	if stream {
		var scanner wire.StreamScanner
		if rw.matched && rw.wire.NewScanner != nil {
			scanner = rw.wire.NewScanner(rw.quirks)
		}
		respBody, respTruncated = h.streamBody(r.Context(), w, resp.Body, capCfg, scanner)
		if scanner != nil {
			ext = scanner.Result()
		} else {
			ext = wire.Extraction{Confidence: calls.ConfidenceUnknown}
		}
	} else {
		var full []byte
		full, respBody, respTruncated = h.copyBody(w, resp.Body, capCfg)
		if rw.matched {
			ext = rw.wire.Extract(full, rw.quirks)
		} else {
			ext = wire.Extraction{Confidence: calls.ConfidenceUnknown}
		}
	}

	cost := 0.0
	if rw.matched && !rw.wire.ZeroCost {
		if snap := h.snapshot(); snap != nil {
			if price, ok := snap.PriceFor(t.Vendor.Name, model); ok {
				cost = pricing.Cost(price, ext.Norm)
			}
		}
	}

	wireName := ""
	if rw.matched {
		wireName = rw.wire.Name
	}

	id, err := h.store.AppendCall(calls.Entry{
		TS:           h.now(),
		UserID:      userID,
		Model:        model,
		Modality:     modality,
		Vendor:       t.Vendor.Name,
		CredentialID: t.Credential.ID,
		Wire:         wireName,
		Confidence:   ext.Confidence,
		Attempt:      attempt,
		Status:       resp.StatusCode,
		Usage:        ext.Raw,
		Cost:         cost,
		LatencyMS:    latency,
		Stream:       stream,
		Tags:         tags,
	})
	if err != nil {
		h.logger.Error("call append failed", "err", err, "vendor", t.Vendor.Name, "model", model)
		return
	}

	if capCfg.on {
		h.savePayload(id, r, reqBody, resp, respBody, respTruncated, capCfg)
	}
}

// savePayload builds and persists the redacted request/response payload for the
// served attempt. Any failure is logged only — never surfaced to the client.
func (h *handler) savePayload(callID int64, r *http.Request, reqBody []byte,
	resp *http.Response, respBody []byte, respTruncated bool, capCfg captureConfig) {
	storedReq, reqTruncated := capBytes(reqBody, capCfg.maxBytes)
	p := store.Payload{
		CallID:          callID,
		ReqHeaders:      redactHeaders(r.Header),
		ReqBody:         storedReq,
		ReqContentType:  r.Header.Get("Content-Type"),
		ReqTruncated:    reqTruncated,
		RespHeaders:     redactHeaders(resp.Header),
		RespBody:        respBody,
		RespContentType: resp.Header.Get("Content-Type"),
		RespTruncated:   respTruncated,
		CreatedAt:       h.now(),
	}
	if err := h.store.SavePayload(p, capCfg.retain); err != nil {
		h.logger.Error("save payload failed", "err", err, "call_id", callID)
	}
}

// streamBody tees the SSE stream to the client, the wire's usage scanner (when
// given), and (when capture is on) a capped buffer, flushing after each chunk
// so nothing is buffered for the client. It returns the captured body and
// whether it was truncated.
func (h *handler) streamBody(ctx context.Context, w http.ResponseWriter, src io.Reader, capCfg captureConfig, scanner wire.StreamScanner) ([]byte, bool) {
	flusher, _ := w.(http.Flusher)

	var capture *cappedBuffer
	if capCfg.on {
		capture = newCappedBuffer(capCfg.maxBytes)
	}

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
			if scanner != nil {
				_, _ = scanner.Write(chunk)
			}
			if capture != nil {
				capture.Write(chunk)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	if capture != nil {
		return capture.Bytes(), capture.Truncated()
	}
	return nil, false
}

// copyBody reads the full (bounded) non-streaming body and writes it to the
// client unchanged. It returns the full body (for usage extraction) plus, when
// capture is on, a capped copy and whether it was truncated.
func (h *handler) copyBody(w http.ResponseWriter, src io.Reader, capCfg captureConfig) (full, captured []byte, truncated bool) {
	body, _, err := readBounded(src, h.maxBodyBytes)
	if err != nil {
		h.logger.Error("read upstream body failed", "err", err)
	}
	if len(body) > 0 {
		if _, werr := w.Write(body); werr != nil {
			h.logger.Error("write client body failed", "err", werr)
		}
	}
	if capCfg.on {
		captured, truncated = capBytes(body, capCfg.maxBytes)
		return body, captured, truncated
	}
	return body, nil, false
}

// recordFailure appends a call row for a failed (failover-eligible) attempt.
func (h *handler) recordFailure(userID, model string, modality calls.Modality,
	t router.Target, attempt, status int, err error, latency int64, tags map[string]string) {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	h.append(calls.Entry{
		TS:           h.now(),
		UserID:      userID,
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

// redactedHeaders are request/response headers stripped before a payload is
// stored, so captured traces never persist consumer or upstream secrets.
var redactedHeaders = map[string]struct{}{
	"Authorization": {},
	"Api-Key":       {},
	"X-Api-Key":     {},
	"Cookie":        {},
}

// redactHeaders flattens an http.Header into a string map (first value per
// header), dropping sensitive headers entirely.
func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if _, drop := redactedHeaders[http.CanonicalHeaderKey(k)]; drop {
			continue
		}
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// capBytes returns a copy of b truncated to max bytes, plus whether truncation
// occurred. A non-positive max returns the bytes unchanged (the caller's caps
// are already normalized to positive defaults).
func capBytes(b []byte, max int) (out []byte, truncated bool) {
	if max <= 0 || len(b) <= max {
		cp := make([]byte, len(b))
		copy(cp, b)
		return cp, false
	}
	cp := make([]byte, max)
	copy(cp, b[:max])
	return cp, true
}

// cappedBuffer accumulates written bytes up to a fixed cap, recording whether
// any bytes were dropped. It is an io.Writer-like sink used to tee a streaming
// response into a bounded capture buffer without ever blocking the stream.
type cappedBuffer struct {
	buf       []byte
	max       int
	truncated bool
}

// newCappedBuffer returns a buffer that stores at most max bytes.
func newCappedBuffer(max int) *cappedBuffer {
	return &cappedBuffer{max: max}
}

// Write appends as much of p as fits under the cap; the rest is discarded and
// the truncated flag is set. It never returns an error.
func (c *cappedBuffer) Write(p []byte) {
	if c.max <= 0 {
		return
	}
	remaining := c.max - len(c.buf)
	if remaining <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return
	}
	if len(p) > remaining {
		c.buf = append(c.buf, p[:remaining]...)
		c.truncated = true
		return
	}
	c.buf = append(c.buf, p...)
}

// Bytes returns the accumulated bytes (may be nil if nothing was written).
func (c *cappedBuffer) Bytes() []byte { return c.buf }

// Truncated reports whether any bytes were dropped due to the cap.
func (c *cappedBuffer) Truncated() bool { return c.truncated }

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

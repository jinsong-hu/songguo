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
// There is one resolution rule, with no addressing "modes": match the wire by
// path suffix, then select the provider by the first available selector —
//
//   - the X-Songguo-Provider header (an explicit pin by provider id, stripped
//     before forwarding), else
//   - the body's model string (every vendor serving it; priority/weighted-RR/
//     failover), else
//   - the default: every vendor serving the matched path, priority-ordered.
//
// For a vendor with a stored endpoint for the matched wire, the upstream URL is
// that full endpoint ({model} substituted, query merged); otherwise (an
// allow_unmatched path, or a wire without a stored endpoint) the inbound path is
// forwarded verbatim to the vendor's origin. Paths are always native: there is
// no /x/<vendor>/ mount.
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
	"github.com/songguo/songguo/internal/parse"
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
	parse        *parsePipeline
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
		parse:        newParsePipeline(d.Store, logger, 0, 0),
	}
}

// defaultHTTPClient returns a client tuned for proxying, including long-lived
// streams: it sets short connect/TLS timeouts but a generous (1h) header
// timeout for slow upstreams, and NO overall Client.Timeout, which would
// truncate streaming responses. Per-request cancellation is honored through
// the request context.
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
			ResponseHeaderTimeout: 1 * time.Hour,
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
	// lives only in the body, and there is no body to buffer here), so the caller
	// pins the provider with X-Songguo-Provider. We branch BEFORE buffering the
	// body so an upgrade is never read as an HTTP body.
	if isWebSocketUpgrade(r) {
		h.handleWebSocket(w, r, user)
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

	// Decide capture once from the global setting, read here so it is stable
	// even if config hot-reloads mid-flight.
	capture := h.captureOn()

	// 3. Resolve the route: match the wire by path suffix and select the
	// provider (X-Songguo-Provider header, else body model, else default).
	// Resolution sets the model/modality, the candidate targets (with their
	// failover policy), and the per-target upstream-URL builder.
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
			h.logger.Warn("upstream request failed",
				"vendor", t.Vendor.Name, "model", rt.model, "credential", t.Credential.ID,
				"url", upReq.URL.String(), "attempt", attempt, "latency_ms", latency,
				"failover", !last, "err", err)
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
			h.logger.Warn("upstream returned failover status; trying next target",
				"vendor", t.Vendor.Name, "model", rt.model, "credential", t.Credential.ID,
				"status", resp.StatusCode, "attempt", attempt, "latency_ms", latency,
				"body", errorSnippet(peekBody(resp)))
			h.recordFailure(user.ID, rt.model, modality, t, attempt, resp.StatusCode,
				fmt.Errorf("upstream status %d", resp.StatusCode), latency, tags)
			h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
			drainAndClose(resp.Body)
			continue
		}

		// This is the chosen response (either a non-failover status, or the last
		// target even if it failed). Report health, then forward verbatim.
		h.router.Report(t.Vendor.Name, t.Credential.ID, resp.StatusCode, nil)
		h.forward(w, r, resp, user.ID, rt.model, modality, rw, t, attempt, latency, tags, capture, body)
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
		UserID:     userID,
		Model:      model,
		Vendor:     strings.Join(vendors, ","),
		Status:     http.StatusNotFound,
		Err:        "unmatched: " + r.Method + " " + path,
		Confidence: calls.ConfidenceUnknown,
	})
	writeError(w, http.StatusNotFound, "wire_unmatched", detail)
}

// resolve builds the route with a single rule: match the wire by path suffix,
// then select the provider by the first available selector — the
// X-Songguo-Provider header (an explicit pin by provider id), else the body's
// model string, else the default (every vendor serving the matched path,
// priority-ordered). It enforces scope and writes any error response itself,
// returning ok=false when it has already responded.
func (h *handler) resolve(w http.ResponseWriter, r *http.Request, user store.User, body []byte) (route, bool) {
	res := meter.Classify(r.Method, r.URL.Path, body)

	// Two distinct identities, deliberately kept apart:
	//   routingModel — the body's model, the ONLY thing we route on. Empty for
	//     model-less wires (TTS/ASR), which route by endpoint alone.
	//   billingModel — what we meter/price as. Falls back to X-Api-Resource-Id
	//     (ByteDance openspeech names the billed class in a header) so PriceFor
	//     can match the table.
	// The resource id must never reach routing: it is a billing class, not a
	// model id, so routing on it would look up byModel[<billing class>], which
	// can never match — the bug this split fixes. Routing is endpoint-first;
	// the model only refines among providers that share an endpoint.
	routingModel := res.Model
	billingModel := res.Model
	if billingModel == "" {
		billingModel = r.Header.Get("X-Api-Resource-Id")
	}

	// Scope (model-bearing case): reject early if the requested model is not in
	// a scoped user's allowlist, before any routing work.
	if routingModel != "" && len(user.Scope) > 0 && !contains(user.Scope, routingModel) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "model not allowed for this user")
		return route{}, false
	}

	// Select the candidate set, endpoint-first. A provider pin wins; else a body
	// model narrows across the vendors serving it; else the default is every
	// vendor, and resolveWires (below) narrows to those serving the requested
	// path — i.e. the endpoint. A single provider on an endpoint is selected
	// without the model ever being consulted.
	pin := r.Header.Get("X-Songguo-Provider")
	var (
		targets []router.Target
		err     error
	)
	switch {
	case pin != "":
		targets, err = h.router.CandidatesForProvider(pin)
	case routingModel != "":
		targets, err = h.router.Candidates(routingModel)
	default:
		targets, err = h.router.AllCandidates()
	}
	if err != nil {
		if errors.Is(err, router.ErrNoVendor) {
			writeError(w, http.StatusBadGateway, "no_upstream", "no upstream serves this request")
			return route{}, false
		}
		h.logger.Error("routing failed", "err", err)
		writeError(w, http.StatusBadGateway, "no_upstream", "routing failed")
		return route{}, false
	}

	kept, wires, denied := resolveWires(targets, r.Method, r.URL.Path)
	if len(kept) == 0 {
		h.denyUnmatched(w, r, user.ID, billingModel, r.URL.Path, denied)
		return route{}, false
	}

	// Scope (model-less case): a scoped user is restricted to its allowed
	// providers/vendors when there is no model to check.
	if routingModel == "" && len(user.Scope) > 0 {
		kept = filterScopedVendors(kept, user.Scope)
		if len(kept) == 0 {
			writeError(w, http.StatusForbidden, "vendor_not_allowed", "vendor not allowed for this user")
			return route{}, false
		}
	}

	model := billingModel
	return route{
		model:    model,
		modality: res.Modality,
		targets:  kept,
		wires:    wires,
		upstreamURL: func(t router.Target) string {
			if rw, ok := wires[t.Vendor.Name]; ok && rw.matched {
				// A path-bearing endpoint is the fixed upstream URL — a rewrite
				// (e.g. /v1/chat/completions -> /api/plan/v3/chat/completions).
				// An origin-only endpoint (scheme://host, no path) is a transparent
				// passthrough: keep the inbound path. That lets one wire cover several
				// native suffixes (e.g. volc/asr-file submit+query) and stops a
				// path-less endpoint from silently POSTing to the host root.
				if ep, ok := t.Vendor.Endpoints[rw.wire.Name]; ok && endpointHasPath(ep) {
					return buildUpstreamURL(ep, model, r.URL.RawQuery)
				}
			}
			// allow_unmatched, or a matched wire whose endpoint is origin-only:
			// forward the inbound path to the vendor origin — but a child path
			// under a known collection endpoint inherits that endpoint's rewritten
			// base (e.g. the video task-status GET .../tasks/{id} under the
			// ark/video submit endpoint .../api/plan/v3/.../tasks), so a vendor
			// that rewrites the path prefix doesn't drop it and 404 on the child.
			return passthroughURL(t.Vendor, r.URL.Path, r.URL.RawQuery)
		},
	}, true
}

// filterScopedVendors keeps only the targets whose vendor name is in the scope
// allowlist, used to constrain a model-less request from a scoped user.
func filterScopedVendors(targets []router.Target, scope []string) []router.Target {
	var out []router.Target
	for _, t := range targets {
		if contains(scope, t.Vendor.Name) {
			out = append(out, t)
		}
	}
	return out
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
	// X-Songguo-Provider is a gateway-internal routing hint (provider pin); it
	// has no meaning to the upstream vendor, so don't leak it.
	upReq.Header.Del("X-Songguo-Provider")
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
	case config.AdapterVolcSpeech:
		// ByteDance openspeech APIs authenticate with X-Api-Key alone; the
		// client supplies the other X-Api-* headers (resource id, request id).
		req.Header.Del("Authorization")
		req.Header.Set("X-Api-Key", key)
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

// buildUpstreamURL turns a wire's full endpoint into the concrete upstream URL:
// it substitutes a {model} placeholder with the request's model and merges the
// endpoint's own query (e.g. Azure's ?api-version=…) with the inbound query.
func buildUpstreamURL(endpoint, model, inboundQuery string) string {
	u := strings.ReplaceAll(endpoint, "{model}", url.PathEscape(model))
	return mergeQuery(u, inboundQuery)
}

// passthroughURL builds the upstream URL for a request that no wire fully
// matched (an allow_unmatched passthrough). It forwards to the vendor origin,
// except when the inbound path is a child of one of the vendor's collection
// endpoints — then it inherits that endpoint's rewritten base plus the child
// tail. This is what lets the video task-status GET (.../tasks/{id}) reach the
// same .../api/plan/v3/... base its submit (.../tasks) was rewritten to, instead
// of being forwarded to the bare origin with the prefix dropped.
func passthroughURL(v config.Vendor, inboundPath, rawQuery string) string {
	if ep, tail, ok := stemEndpoint(v, inboundPath); ok {
		base, epQuery, hasQuery := strings.Cut(ep, "?")
		u := strings.TrimRight(base, "/") + tail
		if hasQuery {
			u += "?" + epQuery
		}
		return mergeQuery(u, rawQuery)
	}
	return joinQuery(strings.TrimRight(v.Origin, "/")+inboundPath, rawQuery)
}

// stemEndpoint finds the vendor's path-bearing wire endpoint that is the parent
// "collection" of inboundPath, returning that endpoint and the child tail. It
// matches the LONGEST wire suffix that appears in inboundPath immediately before
// a "/<tail>" boundary, mirroring wire.Resolve's longest-suffix rule. A bare
// match (the suffix at the very end, no child tail) is left to the normal
// matched-wire path and not handled here.
func stemEndpoint(v config.Vendor, inboundPath string) (endpoint, tail string, ok bool) {
	lower := strings.ToLower(inboundPath)
	bestLen := -1
	for _, name := range v.Wires {
		ep, has := v.Endpoints[name]
		if !has || !endpointHasPath(ep) {
			continue
		}
		w, exists := wire.Get(name)
		if !exists {
			continue
		}
		for _, suf := range w.Suffixes {
			idx := strings.Index(lower, strings.ToLower(suf)+"/")
			if idx < 0 || len(suf) <= bestLen {
				continue
			}
			endpoint, tail, ok, bestLen = ep, inboundPath[idx+len(suf):], true, len(suf)
		}
	}
	return endpoint, tail, ok
}

// endpointHasPath reports whether a configured endpoint carries a path beyond
// the bare origin. An origin-only endpoint (scheme://host or scheme://host/)
// signals a transparent passthrough — the inbound request path is forwarded
// unchanged — while a path-bearing endpoint is the fixed upstream URL to
// rewrite to. A malformed endpoint is treated as explicit (config validation
// surfaces it elsewhere).
func endpointHasPath(endpoint string) bool {
	u, err := url.Parse(strings.ReplaceAll(endpoint, "{model}", "m"))
	if err != nil {
		return true
	}
	return strings.Trim(u.Path, "/") != ""
}

// mergeQuery appends inboundQuery to a URL that may already carry its own query
// string. On key conflict the endpoint's configured params win over inbound ones
// (the operator's intent, e.g. a pinned api-version). When the URL has no query,
// it behaves like joinQuery.
func mergeQuery(u, inboundQuery string) string {
	if inboundQuery == "" {
		return u
	}
	base, epQuery, hasQ := strings.Cut(u, "?")
	if !hasQ {
		return u + "?" + inboundQuery
	}
	merged, _ := url.ParseQuery(inboundQuery)
	ep, _ := url.ParseQuery(epQuery)
	for k, vs := range ep {
		merged[k] = vs
	}
	return base + "?" + merged.Encode()
}

// captureOn resolves whether to capture this request from the global snapshot
// setting. It is read once per request so a mid-request config reload cannot
// change the behaviour for an in-flight call.
func (h *handler) captureOn() bool {
	if snap := h.snapshot(); snap != nil {
		return snap.Settings().Capture
	}
	return false
}

// forward copies the chosen upstream response to the client verbatim and sniffs
// usage as it passes, using the resolved wire's extractor. Streaming responses
// are streamed chunk-by-chunk and flushed; non-streaming responses are buffered
// (bounded) and written whole. When capture is on, it also tees a copy of the
// response body and persists the redacted request/response payload after the
// call row is written.
func (h *handler) forward(w http.ResponseWriter, r *http.Request, resp *http.Response,
	userID, model string, modality calls.Modality, rw resolvedWire, t router.Target, attempt int,
	latency int64, tags map[string]string, capture bool, reqBody []byte) {
	defer resp.Body.Close()

	stream := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	var (
		ext           wire.Extraction
		respBody      []byte
		parseRespBody []byte // fullest response bytes available, for async parse
	)
	if stream {
		var scanner wire.StreamScanner
		if rw.matched && rw.wire.NewScanner != nil {
			scanner = rw.wire.NewScanner(rw.quirks)
		}
		respBody = h.streamBody(r.Context(), w, resp.Body, capture, scanner)
		parseRespBody = respBody
		if scanner != nil {
			ext = scanner.Result()
		} else {
			ext = wire.Extraction{Confidence: calls.ConfidenceUnknown}
		}
	} else {
		full := h.copyBody(w, resp.Body)
		respBody = full
		parseRespBody = full
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

	// An error status on the chosen (forwarded) response is the single most
	// useful debugging signal: the vendor rejected the call. Log it with the
	// vendor's own error body so the cause (bad key, unknown model, quota, …)
	// is visible without opening the captured payload.
	if resp.StatusCode >= 400 {
		h.logger.Warn("upstream error response",
			"vendor", t.Vendor.Name, "model", model, "credential", t.Credential.ID,
			"wire", wireName, "status", resp.StatusCode, "attempt", attempt,
			"latency_ms", latency, "stream", stream, "body", errorSnippet(parseRespBody))
	}

	id, err := h.store.AppendCall(calls.Entry{
		TS:           h.now(),
		UserID:       userID,
		Model:        model,
		Modality:     modality,
		Vendor:       t.Vendor.Name,
		CredentialID: t.Credential.ID,
		Wire:         wireName,
		Confidence:   ext.Confidence,
		Attempt:      attempt,
		Status:       resp.StatusCode,
		Usage:        ext.Raw,
		InputTokens:  ext.Norm.InputTokens,
		OutputTokens: ext.Norm.OutputTokens,
		CachedTokens: ext.Norm.CachedInputTokens,
		Cost:         cost,
		LatencyMS:    latency,
		Stream:       stream,
		Tags:         tags,
	})
	if err != nil {
		h.logger.Error("call append failed", "err", err, "vendor", t.Vendor.Name, "model", model)
		return
	}

	if capture {
		h.savePayload(id, r, reqBody, resp, respBody)
		// Hand the captured bytes to the async parse pipeline. This is the
		// "full parse" — off the hot path; the call is already metered above.
		h.parse.submit(parseJob{
			callID: id,
			at:     h.now(),
			in: parse.Input{
				Wire:            wireName,
				Adapter:         t.Vendor.Adapter,
				Modality:        string(modality),
				Stream:          stream,
				ReqContentType:  r.Header.Get("Content-Type"),
				RespContentType: resp.Header.Get("Content-Type"),
				ReqBody:         reqBody,
				RespBody:        parseRespBody,
			},
		})
	}
}

// savePayload builds and persists the redacted request/response payload for the
// served attempt. Any failure is logged only — never surfaced to the client.
func (h *handler) savePayload(callID int64, r *http.Request, reqBody []byte,
	resp *http.Response, respBody []byte) {
	p := store.Payload{
		CallID:          callID,
		ReqHeaders:      redactHeaders(r.Header),
		ReqBody:         reqBody,
		ReqContentType:  r.Header.Get("Content-Type"),
		RespHeaders:     redactHeaders(resp.Header),
		RespBody:        respBody,
		RespContentType: resp.Header.Get("Content-Type"),
		CreatedAt:       h.now(),
	}
	if err := h.store.SavePayload(p); err != nil {
		h.logger.Error("save payload failed", "err", err, "call_id", callID)
	}
}

// streamBody tees the SSE stream to the client, the wire's usage scanner (when
// given), and (when capture is on) an in-memory buffer, flushing after each
// chunk so nothing is buffered for the client. It returns the captured body, or
// nil when capture is off.
func (h *handler) streamBody(ctx context.Context, w http.ResponseWriter, src io.Reader, capture bool, scanner wire.StreamScanner) []byte {
	flusher, _ := w.(http.Flusher)

	var captured []byte
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
			if capture {
				captured = append(captured, chunk...)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	return captured
}

// copyBody reads the full (bounded) non-streaming body and writes it to the
// client unchanged, returning the body for usage extraction and capture.
func (h *handler) copyBody(w http.ResponseWriter, src io.Reader) []byte {
	body, _, err := readBounded(src, h.maxBodyBytes)
	if err != nil {
		h.logger.Error("read upstream body failed", "err", err)
	}
	if len(body) > 0 {
		if _, werr := w.Write(body); werr != nil {
			h.logger.Error("write client body failed", "err", werr)
		}
	}
	return body
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
		UserID:       userID,
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

// errorSnippet renders an upstream error body as a single bounded log field:
// whitespace is collapsed so the message stays on one line, and the result is
// truncated to keep noisy HTML/JSON error pages from flooding the log.
func errorSnippet(b []byte) string {
	const max = 512
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// peekBody reads a bounded prefix of an upstream response body for logging. It
// is used only on the failover path, where the body is discarded (drained and
// closed) rather than forwarded, so consuming a prefix here is safe.
func peekBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return b
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

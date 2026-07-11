// Package proxy transparently forwards AI requests, swapping only credentials.
//
// The handler is a gate plus a meter: it authenticates the consumer user,
// enforces scope, budget and rate limits, routes the request to a single
// upstream vendor, and records the attempt as a call. It NEVER
// rewrites the request or response body — the credential headers are the only
// mutation; request and response bytes are forwarded verbatim. Metering is
// read-only sniffing and must never block or alter traffic.
//
// It forwards exactly one attempt: there is no per-call retry or failover. The
// vendor's response — success or failure (429, 5xx, transport error) — is
// surfaced to the client verbatim; a client that wants to retry retries itself.
// Choosing among multiple candidates for a model is a routing decision (priority
// then weighted round-robin); there is no automatic health demotion today, so a
// failing vendor stays selected until an operator changes config.
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
//     health-ordered), else
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

	"github.com/google/uuid"

	"github.com/songguo/songguo/internal/bodycodec"
	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/meter"
	"github.com/songguo/songguo/internal/parse"
	"github.com/songguo/songguo/internal/pricing"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/sessiontitle"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

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
	Snapshot   func() *config.Snapshot
	Store      *store.Store
	Router     *router.Router
	Logger     *slog.Logger
	HTTPClient *http.Client     // optional; default constructed if nil
	Now        func() time.Time // optional; defaults to time.Now (for tests)
}

// handler is the concrete http.Handler returned by NewHandler.
type handler struct {
	snapshot func() *config.Snapshot
	store    *store.Store
	router   *router.Router
	logger   *slog.Logger
	client   *http.Client
	now      func() time.Time
	limiter  *rateLimiter
	parse    *parsePipeline
	insight  *insightsFork
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
	return &handler{
		snapshot: d.Snapshot,
		store:    d.Store,
		router:   d.Router,
		logger:   logger,
		client:   client,
		now:      now,
		limiter:  newRateLimiter(now),
		parse:    newParsePipeline(d.Store, logger, 0, 0),
		insight:  newInsightsFork(d.Store, logger, 0, 0),
	}
}

// insights hands a finalized call to the async insights fork (fire and forget).
// It never blocks or fails the forward; see docs/arch-insights.md.
func (h *handler) insights(e calls.Entry, title string) {
	h.insight.submit(e, title)
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
	// 1. Auth. The client presents its songguo key in whichever header its
	// native SDK uses — Authorization: Bearer (OpenAI-style) or X-Api-Key
	// (Anthropic, ByteDance ASR/TTS) — so the endpoint swap needs no other change.
	key := clientKey(r)
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
	// byte pipe (see handleWebSocket); it has no body to buffer, so it routes
	// endpoint-first (by path) like every other request — no model, no required
	// header, just the endpoint. We branch BEFORE buffering the body so an upgrade
	// is never read as an HTTP body.
	if isWebSocketUpgrade(r) {
		h.handleWebSocket(w, r, user)
		return
	}

	// 2. Buffer the request body. No size ceiling: songguo is key-gated and
	// single-tenant, so a caller's payload is trusted; the buffer grows to the
	// actual body size and is forwarded verbatim.
	body, err := readBody(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}

	// Decide capture once from the authenticated user's setting so it is stable
	// for this in-flight request.
	capture := user.Capture

	// Mint the call id (UUID) up front and open the ledger row (phase 1,
	// create-at-start). Everything downstream — denials during routing, budget/
	// rate rejections, the forwarded attempt — finalizes THIS row (phase 2) by
	// id, so an incomplete call stays visible as pending. See docs/arch-gateway.md.
	callID := uuid.NewString()
	h.createCall(callID, user.ID, r)

	// 3. Resolve the route: match the wire by path suffix and select the
	// provider (X-Songguo-Provider header, else body model, else default).
	// Resolution sets the model/modality, the ordered candidate targets, and the
	// per-target upstream-URL builder. Forwarding uses only the first target.
	rt, ok := h.resolve(w, r, user, capture, body, callID)
	if !ok {
		return
	}

	// Tags/attribution were resolved once (in resolve) and carried on the route so
	// a denied call's ledger row and captured trace share the same attribution a
	// forwarded one would.
	tags := rt.tags
	attr := rt.attr
	client := rt.client
	t := rt.targets[0]
	modality := rt.modalityFor(t.Vendor.Name)

	// 4. Budget (coarse pre-check). A denial is recorded and captured like any
	// other gateway-originated outcome (see denyCapture).
	if user.Budget != nil {
		spent, err := h.store.SpendByUser(user.ID, nil)
		if err != nil {
			h.logger.Error("budget lookup failed", "err", err)
		} else if spent >= *user.Budget {
			h.denyCapture(w, r, body, capture, calls.Entry{
				ID:     callID,
				UserID: user.ID, Model: rt.model, Modality: modality,
				Vendor: t.Vendor.Name, CredentialID: t.Credential.ID,
				Tags: tags, SessionID: attr.session, AgentID: attr.agent, ParentAgentID: attr.parentAgent,
				ClientName: client.Name, ClientVersion: client.Version,
			}, http.StatusPaymentRequired, "budget_exceeded", "budget exceeded")
			return
		}
	}

	// 5. Rate limit.
	if !h.limiter.allow(user.ID, user.RPM) {
		h.denyCapture(w, r, body, capture, calls.Entry{
			ID:     callID,
			UserID: user.ID, Model: rt.model, Modality: modality,
			Vendor: t.Vendor.Name, CredentialID: t.Credential.ID,
			Tags: tags, SessionID: attr.session, AgentID: attr.agent, ParentAgentID: attr.parentAgent,
			ClientName: client.Name, ClientVersion: client.Version,
		}, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
		return
	}

	// 6. Forward exactly one attempt — no per-call retry or failover. songguo is
	// a transparent gateway: it forwards the request to the selected upstream and
	// surfaces whatever the vendor returns — success OR failure (429, 5xx, a
	// transport error) — verbatim. A client that wants to retry retries itself.
	//
	// Choosing among multiple candidates is still a real decision, but a
	// cross-request (server-side) one, not a per-call one: rt.targets is ordered
	// by priority -> weighted round-robin, so targets[0] is the pick. There is no
	// health demotion today — a failing vendor is NOT auto-brought-down, so it
	// stays selected until an operator changes config (see router package).
	rw := rt.wires[t.Vendor.Name]

	upReq, err := h.buildUpstreamRequest(r, t, rt.upstreamURL(t), body)
	if err != nil {
		h.logger.Error("build upstream request failed", "err", err, "vendor", t.Vendor.Name)
		// An upstream build/transport failure records a row but captures no payload:
		// there is no served response, and pairing a request with a synthesized error
		// is deliberately not treated as a capture (see denyCapture — that is reserved
		// for gateway-side denials). Pass capture=false regardless of the flag.
		h.denyCapture(w, r, body, false, calls.Entry{
			ID:     callID,
			UserID: user.ID, Model: rt.model, Modality: modality,
			Vendor: t.Vendor.Name, CredentialID: t.Credential.ID,
			Tags: tags, SessionID: attr.session, AgentID: attr.agent, ParentAgentID: attr.parentAgent,
			ClientName: client.Name, ClientVersion: client.Version,
		}, http.StatusBadGateway, "upstream_error", "failed to build upstream request")
		return
	}

	start := h.now()
	resp, err := h.client.Do(upReq)
	latency := h.now().Sub(start).Milliseconds()

	// Transport error: we have no upstream response to forward, so surface the
	// real failure verbatim.
	if err != nil {
		h.logger.Warn("upstream request failed",
			"vendor", t.Vendor.Name, "model", rt.model, "credential", t.Credential.ID,
			"url", upReq.URL.String(), "latency_ms", latency, "err", err)
		h.denyCapture(w, r, body, false, calls.Entry{
			ID:     callID,
			UserID: user.ID, Model: rt.model, Modality: modality,
			Vendor: t.Vendor.Name, CredentialID: t.Credential.ID, LatencyMS: latency,
			Tags: tags, SessionID: attr.session, AgentID: attr.agent, ParentAgentID: attr.parentAgent,
			ClientName: client.Name, ClientVersion: client.Version,
		}, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}

	// Forward the vendor's response verbatim — including a 429/5xx. The client
	// sees the real outcome and decides whether to retry.
	h.forward(w, r, resp, callID, user.ID, rt.model, modality, rw, t, latency, tags, attr, client, capture, body)
}

// route is the resolved plan for a request: the candidate targets in selection
// order (priority -> weighted-RR; only the first is forwarded to, the rest are
// context for a future server-side ejection decision), the model/modality to
// record, a per-target builder for the upstream URL, and the per-vendor resolved
// wire that owns metering.
type route struct {
	model       string
	modality    calls.Modality
	targets     []router.Target
	upstreamURL func(router.Target) string
	wires       map[string]resolvedWire // keyed by vendor name
	tags        map[string]string       // call tags (header + body metadata)
	attr        attribution             // agent-session attribution ids
	client      calls.ClientInfo        // normalized caller client from User-Agent
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

// createCall opens the ledger row for a request at its start (phase 1,
// create-at-start). It records the minted id, start time, and user; the rest of
// the identity (model/vendor/session) is filled by the finalizing path, which
// knows the resolved values. A failure here is logged, not surfaced: the forward
// still proceeds, and the missing row just means this call won't appear in the
// ledger. See docs/arch-gateway.md.
func (h *handler) createCall(callID, userID string, r *http.Request) {
	attr := extractAttribution(r.Header)
	client := calls.ParseClientInfo(r.UserAgent())
	if err := h.store.CreateCall(calls.Entry{
		ID:            callID,
		TS:            h.now(),
		UserID:        userID,
		SessionID:     attr.session,
		AgentID:       attr.agent,
		ParentAgentID: attr.parentAgent,
		ClientName:    client.Name,
		ClientVersion: client.Version,
	}); err != nil {
		h.logger.Error("call create failed", "err", err, "call_id", callID)
	}
}

// denyCapture finalizes a gateway-originated rejection (phase 2) on the row
// opened by createCall and, when capture is passed true, saves the request
// payload plus the synthesized error body — so a denied call is as inspectable
// as a forwarded one. Gateway-side denials (unmatched 404, scope 403, budget
// 402, rate 429) pass the global capture flag; upstream build/transport failures
// (502) reuse this to record the outcome but pass capture=false, since there is
// no served response to pair with. It owns writing the error response; the caller
// fills the Entry's id and known identity fields and status/reason/message.
func (h *handler) denyCapture(w http.ResponseWriter, r *http.Request, body []byte, capture bool,
	e calls.Entry, status int, reason, message string) {
	e.TSEnd = h.now()
	e.Status = status
	if e.ClientName == "" {
		client := calls.ParseClientInfo(r.UserAgent())
		e.ClientName = client.Name
		e.ClientVersion = client.Version
	}
	if e.Err == "" {
		e.Err = reason
	}
	if e.Confidence == "" {
		e.Confidence = calls.ConfidenceUnknown
	}
	err := h.store.FinalizeCall(e)
	if err != nil {
		h.logger.Error("call finalize failed", "err", err, "vendor", e.Vendor, "model", e.Model)
	}

	// Build the exact bytes the client will receive, so the captured response is
	// byte-identical to what was served.
	respBytes, _ := json.Marshal(errorBody{Error: errorDetail{Message: message, Type: "songguo_" + reason}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBytes)

	if capture && err == nil {
		if perr := h.store.SavePayload(store.Payload{
			CallID:          e.ID,
			ReqHeaders:      redactHeaders(r.Header),
			ReqBody:         body,
			ReqContentType:  r.Header.Get("Content-Type"),
			RespHeaders:     map[string]string{"Content-Type": "application/json"},
			RespBody:        respBytes,
			RespContentType: "application/json",
			CreatedAt:       h.now(),
		}); perr != nil {
			h.logger.Error("save payload failed", "err", perr, "call_id", e.ID)
		}
	}

	title := ""
	if capture && err == nil && e.SessionID != "" {
		reqBody := bodyForMeter(body, r.Header.Get("Content-Encoding"), h.logger)
		title = capturedSessionTitle(r.Header, e.Wire, reqBody, respBytes)
	}

	// Hand the finalized denial to insights (async fork — fire and forget).
	h.insights(e, title)
}

// resolve builds the route with a single rule: match the wire by path suffix,
// then select the provider by the first available selector — the
// X-Songguo-Provider header (an explicit pin by provider id), else the body's
// model string, else the default (every vendor serving the matched path,
// priority-ordered). It enforces scope and writes any error response itself,
// returning ok=false when it has already responded. Denials are recorded and
// captured via denyCapture, so they conform to the authenticated user's capture
// setting.
func (h *handler) resolve(w http.ResponseWriter, r *http.Request, user store.User, capture bool, body []byte, callID string) (route, bool) {
	res := meter.Classify(r.Method, r.URL.Path, body)
	tags := extractTags(r.Header.Get("X-Songguo-Tags"), body)
	attr := extractAttribution(r.Header)
	client := calls.ParseClientInfo(r.UserAgent())

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

	denyEntry := func(vendor string) calls.Entry {
		return calls.Entry{
			ID:     callID,
			UserID: user.ID, Model: billingModel, Modality: res.Modality, Vendor: vendor,
			Tags: tags, SessionID: attr.session, AgentID: attr.agent, ParentAgentID: attr.parentAgent,
			ClientName: client.Name, ClientVersion: client.Version,
		}
	}

	// Scope (model-bearing case): reject early if the requested model is not in
	// a scoped user's allowlist, before any routing work.
	if routingModel != "" && len(user.Scope) > 0 && !contains(user.Scope, routingModel) {
		h.denyCapture(w, r, body, capture, denyEntry(""), http.StatusForbidden, "model_not_allowed", "model not allowed for this user")
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
			h.denyCapture(w, r, body, capture, denyEntry(""), http.StatusBadGateway, "no_upstream", "no upstream serves this request")
			return route{}, false
		}
		h.logger.Error("routing failed", "err", err)
		h.denyCapture(w, r, body, capture, denyEntry(""), http.StatusBadGateway, "no_upstream", "routing failed")
		return route{}, false
	}

	kept, wires, denied := resolveWires(targets, r.Method, r.URL.Path)
	if len(kept) == 0 {
		detail := fmt.Sprintf("no enabled wire matches %s %s on service %s; add a wire mapping or enable allow_unmatched",
			r.Method, r.URL.Path, strings.Join(denied, ", "))
		e := denyEntry(strings.Join(denied, ","))
		e.Err = "unmatched: " + r.Method + " " + r.URL.Path
		h.denyCapture(w, r, body, capture, e, http.StatusNotFound, "wire_unmatched", detail)
		return route{}, false
	}

	// Scope (model-less case): a scoped user is restricted to its allowed
	// providers/vendors when there is no model to check.
	if routingModel == "" && len(user.Scope) > 0 {
		kept = filterScopedVendors(kept, user.Scope)
		if len(kept) == 0 {
			h.denyCapture(w, r, body, capture, denyEntry(""), http.StatusForbidden, "vendor_not_allowed", "vendor not allowed for this user")
			return route{}, false
		}
	}

	model := billingModel
	return route{
		model:    model,
		modality: res.Modality,
		targets:  kept,
		wires:    wires,
		tags:     tags,
		attr:     attr,
		client:   client,
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
//
// The client authenticated to songguo with its own key in whichever header its
// native SDK uses (Authorization: Bearer or X-Api-Key; see clientKey). Both
// credential headers are stripped first so the client's songguo key never leaks
// upstream, regardless of which one it arrived in; only the exact X-Api-Key
// credential is removed, so volc-speech's other X-Api-* headers (resource id,
// request id) still pass through verbatim.
func applyUpstreamAuth(req *http.Request, adapter, key string) {
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	switch adapter {
	case config.AdapterAnthropic:
		// Anthropic authenticates with x-api-key and requires an API version
		// header.
		req.Header.Set("X-Api-Key", key)
		if req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
	case config.AdapterVolcSpeech:
		// ByteDance openspeech APIs authenticate with X-Api-Key alone.
		req.Header.Set("X-Api-Key", key)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
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

// forward copies the chosen upstream response to the client verbatim and sniffs
// usage as it passes, using the resolved wire's extractor. Streaming responses
// are streamed chunk-by-chunk and flushed; non-streaming responses are buffered
// (bounded) and written whole. When capture is on, it also tees a copy of the
// response body and persists the redacted request/response payload after the
// call row is written.
func (h *handler) forward(w http.ResponseWriter, r *http.Request, resp *http.Response,
	callID, userID, model string, modality calls.Modality, rw resolvedWire, t router.Target,
	latency int64, tags map[string]string, attr attribution, client calls.ClientInfo, capture bool, reqBody []byte) {
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
		respBody, parseRespBody = h.streamBody(r.Context(), w, resp.Body, capture, scanner, resp.Header.Get("Content-Encoding"))
		if scanner != nil {
			ext = scanner.Result()
		} else {
			ext = wire.Extraction{Confidence: calls.ConfidenceUnknown}
		}
	} else {
		full := h.copyBody(w, resp.Body)
		respBody = full
		parseRespBody = bodyForMeter(full, resp.Header.Get("Content-Encoding"), h.logger)
		if rw.matched {
			ext = rw.wire.Extract(parseRespBody, rw.quirks)
		} else {
			ext = wire.Extraction{Confidence: calls.ConfidenceUnknown}
		}
	}

	// Cost is priced from the vendor's OFFICIAL usage only. ext.Norm is the
	// canonical view of the `usage` object the vendor returned in the response
	// body (see the wire extractors). We never reconcile it against a locally
	// counted token total: the number we bill is the number the vendor reported,
	// verbatim. If the vendor under-reports, over-reports, or omits usage (then
	// Norm is zero), cost reflects that faithfully and meters accordingly —
	// $0 on unknown usage, never a local guess.
	//
	// Local token counting DOES exist (internal/compose) but is deliberately kept
	// out of billing: it feeds context-composition insights, where granularity and
	// stable proportions/trends matter more than matching the vendor's exact total.
	// Billing wants the authoritative number; insights want the granular one. They
	// are separate on purpose and must not be cross-reconciled.
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
			"wire", wireName, "status", resp.StatusCode,
			"latency_ms", latency, "stream", stream, "body", errorSnippet(parseRespBody))
	}

	entry := calls.Entry{
		ID:            callID,
		TSEnd:         h.now(),
		UserID:        userID,
		Model:         model,
		Modality:      modality,
		Vendor:        t.Vendor.Name,
		CredentialID:  t.Credential.ID,
		Wire:          wireName,
		Confidence:    ext.Confidence,
		Status:        resp.StatusCode,
		Usage:         ext.Raw,
		InputTokens:   ext.Norm.InputTokens,
		OutputTokens:  ext.Norm.OutputTokens,
		CachedTokens:  ext.Norm.CachedInputTokens,
		Cost:          cost,
		LatencyMS:     latency,
		Stream:        stream,
		Tags:          tags,
		SessionID:     attr.session,
		AgentID:       attr.agent,
		ParentAgentID: attr.parentAgent,
		// Client identity is authoritatively persisted at createCall (phase 1);
		// carried here too so the insights fork sees a complete entry.
		ClientName:    client.Name,
		ClientVersion: client.Version,
	}
	// Phase 2, update-at-end: finalize the row opened at request-start.
	if err := h.store.FinalizeCall(entry); err != nil {
		h.logger.Error("call finalize failed", "err", err, "vendor", t.Vendor.Name, "model", model)
		return
	}
	id := callID
	title := ""
	if capture && entry.SessionID != "" {
		reqForTitle := bodyForMeter(reqBody, r.Header.Get("Content-Encoding"), h.logger)
		title = capturedSessionTitle(r.Header, wireName, reqForTitle, parseRespBody)
	}
	// Hand the finalized call to insights (async fork — fire and forget; never
	// blocks or fails the forward). See docs/arch-insights.md.
	h.insights(entry, title)

	// Context-window composition: read-only sniff of the already-buffered request
	// body to decompose the input context across sources (system, tool schemas,
	// tool results, ...). Tokens are counted LOCALLY (o200k_base) per block for a
	// stable, vendor-independent breakdown — deliberately decoupled from official
	// usage (which stays authoritative for billing). The official cache-read total
	// is the one number passed through, so the cache cross-cut stays real. This
	// never counts against billing and never touches the bytes (same category as
	// reading `model` or metering `usage`). It runs after the client response is
	// already sent, so it adds no client latency, and is NOT gated by capture.
	// Any failure is logged and never surfaced to the client.
	if modality == calls.ModalityChat && rw.matched && ext.Norm.InputTokens > 0 {
		if comp, ok := compose.Compose(rw.wire.Name, reqBody,
			int64(ext.Norm.CachedInputTokens)); ok {
			if err := h.store.SaveComposition(id, comp); err != nil {
				h.logger.Error("save composition failed", "err", err, "call_id", id)
			}
		}
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
func (h *handler) savePayload(callID string, r *http.Request, reqBody []byte,
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
func (h *handler) streamBody(ctx context.Context, w http.ResponseWriter, src io.Reader, capture bool, scanner wire.StreamScanner, contentEncoding string) ([]byte, []byte) {
	flusher, _ := w.(http.Flusher)
	scanWrite, scanFinish := meteringScannerWriter(scanner, contentEncoding, h.logger)
	defer scanFinish()

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
			scanWrite(chunk)
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
	if !capture {
		return nil, nil
	}
	return captured, bodyForMeter(captured, contentEncoding, h.logger)
}

func meteringScannerWriter(scanner wire.StreamScanner, contentEncoding string, logger *slog.Logger) (func([]byte), func()) {
	if scanner == nil {
		return func([]byte) {}, func() {}
	}
	if contentEncoding == "" {
		return func(chunk []byte) { _, _ = scanner.Write(chunk) }, func() {}
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pr.Close()

		zr, ok, err := bodycodec.NewReader(pr, contentEncoding)
		if !ok {
			buf := make([]byte, 32*1024)
			for {
				n, err := pr.Read(buf)
				if n > 0 {
					_, _ = scanner.Write(buf[:n])
				}
				if err != nil {
					return
				}
			}
		}
		if err != nil {
			if logger != nil {
				logger.Warn("encoded stream metering disabled", "encoding", contentEncoding, "err", err)
			}
			return
		}
		defer zr.Close()

		buf := make([]byte, 32*1024)
		for {
			n, err := zr.Read(buf)
			if n > 0 {
				_, _ = scanner.Write(buf[:n])
			}
			if err != nil {
				if err != io.EOF && logger != nil {
					logger.Warn("encoded stream metering read failed", "encoding", contentEncoding, "err", err)
				}
				return
			}
		}
	}()

	return func(chunk []byte) {
			_, _ = pw.Write(chunk)
		}, func() {
			_ = pw.Close()
			<-done
		}
}

func bodyForMeter(body []byte, contentEncoding string, logger *slog.Logger) []byte {
	decoded, ok, err := bodycodec.Decode(body, contentEncoding)
	if !ok {
		return body
	}
	if err != nil {
		if logger != nil {
			logger.Warn("encoded body decode failed", "encoding", contentEncoding, "err", err)
		}
		return body
	}
	return decoded
}

// copyBody reads the full non-streaming body and writes it to the client
// unchanged, returning the body for usage extraction and capture.
func (h *handler) copyBody(w http.ResponseWriter, src io.Reader) []byte {
	body, err := readBody(src)
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

// append writes a single-shot finalized call entry, for paths that have no
// two-phase lifecycle to split — the WebSocket relay, whose raw byte pipe has no
// clean create-at-start/update-at-end boundary, meters once at the end. It mints
// the id if the caller left it empty, opens and immediately finalizes the row,
// hands the result to insights, and logs (never surfaces) any failure.
func (h *handler) append(e calls.Entry) {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.TS.IsZero() {
		e.TS = h.now()
	}
	if e.TSEnd.IsZero() {
		e.TSEnd = h.now()
	}
	if err := h.store.CreateCall(e); err != nil {
		h.logger.Error("call create failed", "err", err, "vendor", e.Vendor, "model", e.Model)
		return
	}
	if err := h.store.FinalizeCall(e); err != nil {
		h.logger.Error("call finalize failed", "err", err, "vendor", e.Vendor, "model", e.Model)
		return
	}
	h.insights(e, "")
}

func capturedSessionTitle(headers http.Header, wireName string, reqBody, respBody []byte) string {
	if isCodexRequest(headers) {
		return sessiontitle.FromCodex(reqBody)
	}
	if wireName == "anthropic/messages" {
		return sessiontitle.FromClaude(reqBody, respBody)
	}
	return ""
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

// clientKey extracts the caller's songguo key from the request, accepting it in
// whichever header the client's native SDK carries the credential: Authorization
// (Bearer or raw) or, for X-Api-Key-style wires (Anthropic, ByteDance ASR/TTS),
// the X-Api-Key header. Authorization wins when both are present so existing
// OpenAI-style callers are unaffected. This is the ingress half of songguo's
// "change only the endpoint" promise; the egress half lives in applyUpstreamAuth.
func clientKey(r *http.Request) string {
	if k := bearerToken(r.Header.Get("Authorization")); k != "" {
		return k
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
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

// readBody reads r fully into memory. There is no size ceiling: songguo is a
// key-gated single-tenant gateway, so payloads are trusted and forwarded
// verbatim. The buffer grows to the actual body size.
func readBody(r io.Reader) (body []byte, err error) {
	if r == nil {
		return nil, nil
	}
	body, err = io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
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

// contains reports whether s contains v.
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// attribution carries request-attribution ids sniffed from known coding-agent
// headers (read-only; the bytes are forwarded untouched). All fields are "" for
// non-agent traffic.
type attribution struct {
	session     string
	agent       string
	parentAgent string
}

// extractAttribution reads coding-agent attribution headers. Claude Code
// exposes session + agent tree headers; Codex exposes a stable Session-Id (and
// also repeats it in X-Codex-Turn-Metadata). SessionID lets the ledger aggregate
// a run's calls. AgentID + ParentAgentID reconstruct the main-loop→subagent tree
// when the client provides that information.
func extractAttribution(h http.Header) attribution {
	attr := attribution{
		session:     strings.TrimSpace(h.Get("X-Claude-Code-Session-Id")),
		agent:       strings.TrimSpace(h.Get("X-Claude-Code-Agent-Id")),
		parentAgent: strings.TrimSpace(h.Get("X-Claude-Code-Parent-Agent-Id")),
	}
	if attr.session != "" || !isCodexRequest(h) {
		return attr
	}

	if sid := strings.TrimSpace(h.Get("Session-Id")); sid != "" {
		attr.session = sid
		return attr
	}
	if sid := codexMetadataSessionID(h.Get("X-Codex-Turn-Metadata")); sid != "" {
		attr.session = sid
		return attr
	}
	// Last resort for older/future Codex clients that carry only a thread id.
	attr.session = strings.TrimSpace(h.Get("Thread-Id"))
	return attr
}

func isCodexRequest(h http.Header) bool {
	originator := strings.ToLower(strings.TrimSpace(h.Get("Originator")))
	ua := strings.ToLower(h.Get("User-Agent"))
	return strings.HasPrefix(originator, "codex") ||
		strings.Contains(ua, "codex") ||
		h.Get("X-Codex-Turn-Metadata") != "" ||
		h.Get("X-Codex-Window-Id") != ""
}

func codexMetadataSessionID(raw string) string {
	if raw == "" {
		return ""
	}
	var meta struct {
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return ""
	}
	if sid := strings.TrimSpace(meta.SessionID); sid != "" {
		return sid
	}
	return strings.TrimSpace(meta.ThreadID)
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

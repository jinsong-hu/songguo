package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sort"
	"syscall"
	"time"
)

// Signal is what one dispatched attempt's outcome says about a vendor's health.
//
// Failures come in two strengths, because the evidence genuinely differs. A
// connection refused is proof: nothing is listening, and the next request will
// fail the same way. A timeout is a hint: the vendor may be overloaded, or the
// caller may simply have sent a 200k-token prompt. Treating those alike means
// either demoting on flimsy evidence or ignoring conclusive evidence, so we
// don't.
type Signal int

const (
	// SignalOK: the vendor served the request.
	SignalOK Signal = iota
	// SignalFail: the vendor failed, but ambiguously — it may be transient or
	// load-dependent. Counts as one strike; failThreshold of them demote.
	SignalFail
	// SignalFailHard: the vendor is definitively not serving, and repeating the
	// request would fail identically. Conclusive on its own: one is enough to
	// demote, because waiting for corroboration just burns more client requests
	// to learn something already known.
	SignalFailHard
	// SignalFailCredential: conclusive like SignalFailHard, but the fault is the
	// CREDENTIAL's rather than the endpoint's, so it demotes every vendor
	// sharing that credential at once.
	//
	// This is the one signal whose blast radius is wider than the vendor that
	// observed it. A provider is split into several vendors by (origin,
	// adapter), and those vendors have genuinely independent hosts — but they
	// share one API key against one account. A revoked key is dead on all of
	// them simultaneously, and making each sibling rediscover that with its own
	// failed client request would be re-proving a known fact.
	SignalFailCredential
	// SignalFailModel: the vendor is fine, but it will not serve THIS model
	// right now — a 429 against one model's quota. Narrower than every other
	// failure: it demotes the (vendor, model) pair for a fixed cooldown and
	// leaves the vendor's own health untouched, so the rest of its catalogue
	// keeps serving.
	//
	// It needs none of the machinery the other signals have. There is no
	// corroboration streak, because a 429 is already a definite statement
	// rather than an inference; and there is no dead state, because a rate
	// limit is transient by construction.
	SignalFailModel
	// SignalFailClient: the vendor answered and rejected the request with a 4xx
	// that is not one of the cases above — 400, 404, 408, 422, and the rest.
	//
	// The status code says "the caller's fault", and usually it is. But songguo
	// balances across RESELLERS, not across replicas of one service, and that
	// makes the status code a liar often enough to matter: a relay that has not
	// implemented an endpoint, rejects a parameter its siblings accept, or
	// carries a stale model alias returns exactly these codes while a healthy
	// sibling returns 200. Discarding them outright — which is what every
	// homogeneous-backend proxy does, and what songguo did until now — makes
	// that class of breakage permanently invisible to routing.
	//
	// The reason it cannot simply be counted like SignalFail is blast radius. A
	// single client with a malformed body produces identical 4xx against every
	// vendor it touches, so an ordinary streak would let one broken caller walk
	// the entire fleet to dead in about a dozen requests. So this signal is
	// counted along the axis that actually separates the two cases: DISTINCT
	// SESSIONS. One session failing a hundred times is one client's bug and
	// demotes nothing; ten different sessions failing once each is the vendor's
	// bug and demotes it. See applyReport.
	SignalFailClient
	// SignalNeutral: the CALLER went away — hung up mid-stream, cancelled the
	// context. Not an outcome the vendor produced at all, so it neither demotes
	// nor restores.
	SignalNeutral
)

// String renders a signal for logs.
func (s Signal) String() string {
	switch s {
	case SignalOK:
		return "ok"
	case SignalFail:
		return "fail"
	case SignalFailHard:
		return "fail_hard"
	case SignalFailCredential:
		return "fail_credential"
	case SignalFailModel:
		return "fail_model"
	case SignalFailClient:
		return "fail_client"
	case SignalNeutral:
		return "neutral"
	}
	return "unknown"
}

// Classify maps the outcome of one dispatched attempt to a health signal.
// clientGone reports whether the CLIENT aborted; a cancelled request must never
// be blamed on the vendor.
//
// Detection is PASSIVE — we read the outcomes of requests clients actually
// made. Active probing is deliberately not done: it would invent traffic to a
// credential the operator pays for, for requests nobody asked for, which is the
// direct sibling of inventing attempts and inventing bytes.
//
// The accepted cost: one client-visible failure per cooldown window. The first
// request after a vendor dies still goes to it and still fails, because we do
// not retry. That is the price of one-attempt transparency.
// A transport failure has NO status code — the vendor never answered — so
// status is ignored whenever err is non-nil. Callers pass 0 for clarity.
func Classify(status int, err error, clientGone bool) Signal {
	if err != nil {
		// A client that hangs up mid-stream (Claude Code does exactly this on
		// interrupt) also cancels our upstream request. That is not a vendor
		// fault. The 2026-07 implementation treated every non-nil error as a
		// failure, so an interrupt-happy client would have walked the whole
		// fleet down; do not reproduce that.
		if clientGone || errors.Is(err, context.Canceled) {
			return SignalNeutral
		}
		if conclusiveTransportFailure(err) {
			return SignalFailHard
		}
		// Everything else that produced no response: dial and response-header
		// timeouts, connection resets, unexpected EOF. Real failures, but not
		// conclusive — a timeout can just as easily mean a huge prompt against
		// a busy-but-healthy vendor, so it needs corroboration.
		return SignalFail
	}

	switch {
	case status == http.StatusUnauthorized:
		// 401 is deterministic AND credential-scoped: the key is wrong, it will
		// be wrong for every subsequent request, and it is wrong on every host
		// that presents it. The caller never sees our upstream key, so this can
		// only be our key or our account — the rotated-key outage, the most
		// common real one.
		return SignalFailCredential

	case status == http.StatusTooManyRequests:
		// A 429 is about one model's quota far more often than the whole
		// vendor's: providers meter per model tier, so a hot gpt-4o says
		// nothing about text-embedding-3 on the same key. Scoping it to the
		// model keeps one busy model from walking a vendor's entire catalogue
		// out of rotation. A vendor that is genuinely rate-limited across the
		// board simply accumulates one cooldown per model, converging on the
		// same behavior without the collateral damage.
		return SignalFailModel

	case status >= 500,
		// 403 is a vendor-side rejection, but the meaning is overloaded: it can
		// be a disabled key OR "this model is not on your plan", which would be
		// per-model rather than vendor-wide. Too ambiguous to be conclusive.
		status == http.StatusForbidden:
		return SignalFail

	case status >= 400:
		// Probably the caller's body or path. Possibly this relay missing an
		// endpoint its siblings serve. The status code cannot tell the two
		// apart, so the signal is deliberately weak and is resolved by
		// correlation instead — see SignalFailClient.
		return SignalFailClient
	}

	return SignalOK
}

// conclusiveTransportFailure reports whether err proves the vendor is not
// serving, as opposed to merely being slow, busy, or flaky.
//
// The test is "would an identical retry fail identically?". Nothing listening,
// a host that does not exist, and a certificate that does not verify are all
// properties of the endpoint and answer yes. Timeouts and resets answer no —
// they are as likely to be about this particular request, or the network in
// between, as about the vendor.
func conclusiveTransportFailure(err error) bool {
	// Nothing is listening on the port.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}

	// The hostname does not resolve. Only NXDOMAIN counts: a temporary resolver
	// failure or DNS timeout proves nothing about the vendor.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}

	// TLS does not verify. Deterministic per host: an expired, self-signed, or
	// wrong-hostname certificate fails identically on every retry.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}

	return false
}

// vendorHealth is one vendor's cross-request health state.
type vendorHealth struct {
	consecFail int       // consecutive failures since the last success
	openUntil  time.Time // zero while live; otherwise the end of the cooldown
	demotions  int       // consecutive demotions since the last success
	dead       bool      // demoted deadAfter times running; see healthDead

	// clientFails is the set of distinct agent sessions that have seen a
	// SignalFailClient from this vendor since its last success. Its SIZE is the
	// evidence, not its contents: a set answers "how many independent callers
	// hit this" where a counter can only answer "how many requests failed", and
	// only the former distinguishes a broken vendor from a broken client.
	//
	// A set rather than a counter is the whole point, so re-failing from an
	// already-recorded session adds nothing — which is what makes one client
	// retrying in a loop unable to move this vendor at all.
	//
	// Bounded at clientFailMax entries. Cleared by any success, like every
	// other failure state.
	clientFails map[string]struct{}
}

// Report records the outcome of the single attempt that was dispatched to
// vendorName. It is a CROSS-REQUEST signal: it never affects the in-flight
// call, only the ordering of subsequent ones. Safe for concurrent use.
//
// Only the two sites that actually dispatched call this, so gateway-side
// denials (budget, scope, rate limit, unmatched wire) can never reach it —
// those are our refusals, not the vendor's.
func (r *Router) Report(vendorName, credentialID string, sig Signal) {
	r.ReportAttempt(Attempt{Vendor: vendorName, Credential: credentialID}, sig)
}

// ReportModel is Report with the model the request named, which lets a 429 be
// scoped to the (vendor, model) pair it actually concerns. A model-less request
// (a WebSocket upgrade, GET /v1/models) passes "" and simply cannot produce a
// model-scoped demotion.
func (r *Router) ReportModel(vendorName, credentialID, model string, sig Signal) {
	r.ReportAttempt(Attempt{Vendor: vendorName, Credential: credentialID, Model: model}, sig)
}

// Attempt identifies the single dispatched attempt an outcome belongs to. Each
// field is a scope some signal needs: a 429 demotes (Vendor, Model), a 401
// demotes every vendor on Credential, and a 4xx is counted per distinct
// Session. Fields the caller cannot supply are "", and the signals that need
// them degrade to doing nothing rather than widening their blast radius.
type Attempt struct {
	Vendor     string
	Credential string
	Model      string // billing model; "" for model-less wires
	Session    string // caller's agent session; "" when the client sent none
}

// ReportAttempt is the full form of Report: it records the outcome of one
// dispatched attempt with every scope the signal might need.
func (r *Router) ReportAttempt(a Attempt, sig Signal) {
	vendorName, credentialID, model := a.Vendor, a.Credential, a.Model
	if vendorName == "" || sig == SignalNeutral {
		return
	}
	now := r.now()

	// A client-error failure is counted per distinct session, so a request that
	// carries no session identity cannot contribute to it. Dropping it is the
	// same choice ReportModel makes for a model-less 429: when the scope that
	// makes a weak signal safe is missing, the signal is discarded rather than
	// widened into something stronger than its evidence supports.
	//
	// Consequence worth knowing: traffic from clients that send no session
	// header (curl, most WebSocket upgrades) can never demote a vendor on 4xx.
	if sig == SignalFailClient {
		if a.Session == "" {
			return
		}
		r.logTransition(vendorName, sig, r.applyClientFail(vendorName, a.Session, now))
		return
	}

	// A model-scoped failure never touches vendor health: the vendor is
	// serving, it just will not serve this model right now.
	if sig == SignalFailModel {
		if model == "" {
			// Nothing to scope it to. Rather than widen a per-model rate limit
			// into a vendor-wide demotion, drop it — the alternative punishes
			// a healthy vendor for a quota that may not even apply.
			return
		}
		r.mu.Lock()
		until := now.Add(r.modelCooldown)
		r.modelCooling[modelKey(vendorName, model)] = until
		r.mu.Unlock()
		r.logger.Info("model demoted on vendor",
			"vendor", vendorName, "model", model, "until", until)
		return
	}

	// A credential-scoped failure lands on every vendor presenting that key, not
	// just the one that happened to observe it. Resolved from the snapshot
	// before taking the lock, since it reads config rather than health state.
	names := []string{vendorName}
	if sig == SignalFailCredential && credentialID != "" {
		names = r.vendorsSharingCredential(credentialID, vendorName)
	}

	for _, name := range names {
		r.logTransition(name, sig, r.applyReport(name, sig, now))
	}
}

// healthTransition is what one report changed about a vendor's state, if
// anything. All false is the common case: a success on a live vendor, or a
// failure that has not yet reached the threshold.
type healthTransition struct {
	demoted  bool
	restored bool
	died     bool
	until    time.Time
}

// vendorsSharingCredential returns every configured vendor name presenting
// credentialID, with observed guaranteed to be included even if the config has
// since changed underneath us.
func (r *Router) vendorsSharingCredential(credentialID, observed string) []string {
	snap := r.snapshot()
	if snap == nil {
		return []string{observed}
	}
	names := []string{observed}
	for _, v := range snap.Vendors() {
		if v.Credential.ID == credentialID && v.Name != observed {
			names = append(names, v.Name)
		}
	}
	return names
}

// applyReport folds one signal into one vendor's health and reports which
// transition, if any, it caused.
func (r *Router) applyReport(vendorName string, sig Signal, now time.Time) (tr healthTransition) {
	r.mu.Lock()
	defer r.mu.Unlock()

	h := r.health[vendorName]
	if h == nil {
		h = &vendorHealth{}
		r.health[vendorName] = h
	}
	switch sig {
	case SignalOK:
		// A success is the one thing that clears every kind of demotion,
		// including dead — proof beats any amount of accumulated suspicion.
		// Log a restoration only if the vendor was actually still down; one
		// whose cooldown already lapsed has recovered silently.
		tr.restored = h.dead || (!h.openUntil.IsZero() && now.Before(h.openUntil))
		h.consecFail = 0
		h.demotions = 0
		h.openUntil = time.Time{}
		h.dead = false
		// The session set is evidence of the same kind, so it clears with the
		// rest. This is what keeps a vendor that serves ordinary traffic fine
		// from ever accumulating a demotion out of scattered 4xx: every success
		// in between wipes the slate, so reaching the threshold requires ten
		// distinct sessions to fail with NO success anywhere among them.
		clear(h.clientFails)
	case SignalFail, SignalFailHard, SignalFailCredential:
		h.consecFail++
		// A conclusive failure short-circuits the streak: there is nothing to
		// corroborate, so make it count for the full threshold rather than
		// spending two more real client requests re-proving it.
		if sig != SignalFail && h.consecFail < r.failThreshold {
			h.consecFail = r.failThreshold
		}
		if h.consecFail >= r.failThreshold {
			tr = r.demoteLocked(h, now)
		}
		// consecFail is deliberately NOT reset here. It stays at or above the
		// threshold, which puts the vendor on PROBATION once its cooldown
		// lapses: the very next failure re-demotes it immediately instead of
		// costing another full threshold's worth of client-visible failures.
		//
		// Without this, a permanently dead vendor bills the caller
		// failThreshold failures every cooldown window forever (3 per 30s),
		// rather than the one failure the passive-detection tradeoff is
		// supposed to cost. Only a success clears the streak.
	}
	return tr
}

// demoteLocked opens the next cooldown on h and marks it dead once the
// demotions have piled up. Every failure path funnels through here, so all of
// them back off on the same doubling ladder and reach dead by the same rule —
// the signals differ in what it takes to GET here, never in what happens after.
// Caller must hold r.mu.
func (r *Router) demoteLocked(h *vendorHealth, now time.Time) (tr healthTransition) {
	h.demotions++
	h.openUntil = now.Add(r.nextCooldown(h.demotions))
	tr.demoted, tr.until = true, h.openUntil
	// Enough consecutive demotions, with no success anywhere in between, stops
	// being a blip and starts being a broken provider.
	if h.demotions >= r.deadAfter && !h.dead {
		h.dead, tr.died = true, true
	}
	return tr
}

// applyClientFail folds one 4xx into the vendor's distinct-session set and
// demotes once enough independent callers have hit it.
//
// The ladder is driven by NEW sessions only: the clientFailThreshold'th distinct
// session opens the first cooldown, the next one the second, and so on, so it
// takes clientFailThreshold + deadAfter distinct sessions to mark a vendor dead.
// A session already in the set contributes nothing however many times it
// retries — that is the property that makes counting 4xx safe at all, and it is
// why this cannot be folded into the consecFail streak.
func (r *Router) applyClientFail(vendorName, session string, now time.Time) (tr healthTransition) {
	r.mu.Lock()
	defer r.mu.Unlock()

	h := r.health[vendorName]
	if h == nil {
		h = &vendorHealth{}
		r.health[vendorName] = h
	}
	if h.clientFails == nil {
		h.clientFails = make(map[string]struct{})
	}
	// Past the cap the vendor is already dead several times over and another
	// session name buys no information, so stop growing rather than keep an
	// unbounded set keyed by something a client controls.
	if _, seen := h.clientFails[session]; seen || len(h.clientFails) >= r.clientFailMax {
		return tr
	}
	h.clientFails[session] = struct{}{}
	if len(h.clientFails) < r.clientFailThreshold {
		return tr
	}
	return r.demoteLocked(h, now)
}

// logTransition reports a health change. Called outside the lock. These
// transitions are rare and high-value, so they are Info: an operator wants to
// see a provider drop out of rotation.
func (r *Router) logTransition(vendorName string, sig Signal, tr healthTransition) {
	switch {
	case tr.died:
		// Warn, not Info: this one wants an operator's attention. A dead vendor
		// will not come back on a timer, so it sits out of rotation until it
		// succeeds again or someone disables it.
		r.logger.Warn("vendor marked dead; it will not be retried while any healthy vendor exists",
			"vendor", vendorName, "signal", sig.String(),
			"consecutive_demotions", r.deadAfter,
			"hint", "check the provider, then disable it or let a success clear it")
	case tr.demoted:
		r.logger.Info("vendor demoted",
			"vendor", vendorName, "signal", sig.String(),
			"until", tr.until, "for", tr.until.Sub(r.now()))
	case tr.restored:
		r.logger.Info("vendor restored", "vendor", vendorName)
	}
}

// The three health tiers, in preference order. None of them excludes a vendor —
// they only decide how far down the candidate list it sorts.
const (
	// healthLive: no outstanding failure streak, or a cooldown that has lapsed.
	healthLive = iota
	// healthCooling: recently demoted. Temporary and time-based — it lapses on
	// its own, and the vendor is simply live again.
	healthCooling
	// healthDead: demoted repeatedly with no success in between. Ranked below
	// even a cooling vendor, and NOT restored by the passage of time.
	//
	// The only things that clear it are a success and an operator. That sounds
	// like it could strand a vendor forever, and in the normal case it
	// deliberately does: while any working vendor exists, a dead one is never
	// chosen, so it never gets the success that would revive it, so it stays
	// down until someone looks. That is the intent — a vendor that has failed
	// this consistently needs a human, not another retry.
	//
	// The corner case is what makes it safe. Because health never excludes, a
	// dead vendor still sorts into the list, and if EVERYTHING is dead — a
	// network partition on our side rather than a real vendor outage — the
	// least-bad candidate still gets the request, and the first success clears
	// it. So a self-inflicted outage heals itself, while a genuinely dead
	// provider waits for an operator.
	healthDead
)

// modelKey namespaces a per-(vendor, model) cooldown.
func modelKey(vendorName, model string) string {
	return vendorName + "\x00" + model
}

// modelCooling reports whether this vendor is currently rate-limited for this
// specific model. Caller must hold r.mu.
func (r *Router) modelIsCooling(vendorName, model string, now time.Time) bool {
	if model == "" {
		return false
	}
	until, ok := r.modelCooling[modelKey(vendorName, model)]
	return ok && now.Before(until)
}

// healthRank places a vendor in one of the three tiers above.
//
// A demoted vendor is never excluded: it stays in the candidate list and leads
// again as soon as nothing healthier outranks it. That keeps the package
// invariant (Select returns a permutation) true, and it gives the half-open
// probe for free — at cooldown expiry the vendor is simply live again, and a
// single fresh failure re-demotes it.
//
// Caller must hold r.mu.
func (r *Router) healthRank(vendorName string, now time.Time) int {
	h := r.health[vendorName]
	if h == nil {
		return healthLive
	}
	if h.dead {
		return healthDead
	}
	if !h.openUntil.IsZero() && now.Before(h.openUntil) {
		return healthCooling
	}
	return healthLive
}

// nextCooldown returns how long the nth consecutive demotion should last,
// doubling from the base up to the ceiling.
//
// A fixed cooldown never learns: a vendor that has been dead for an hour still
// costs a failed client request every 30 seconds, forever. Doubling takes that
// from ~120 wasted requests an hour to ~4, while still rechecking often enough
// that a recovered vendor is noticed the same afternoon.
func (r *Router) nextCooldown(demotions int) time.Duration {
	d := r.cooldown
	for i := 1; i < demotions && d < r.cooldownMax; i++ {
		d *= 2
	}
	if d > r.cooldownMax {
		d = r.cooldownMax
	}
	return d
}

// ResetHealth clears cross-request health state (vendor demotions and per-model
// rate-limit cooldowns). configsvc calls it after a config reload: an operator
// edit is an explicit statement about a provider, so a vendor gets a clean slate
// rather than inheriting a cooldown earned under different config. It also drops
// entries for vendors the reload removed, so the map cannot grow with config
// churn.
//
// It does NOT clear session affinity — see the note below.
func (r *Router) ResetHealth() {
	r.mu.Lock()
	clear(r.health)
	clear(r.modelCooling)
	r.mu.Unlock()

	// Session pins are deliberately KEPT. A stale pin cannot do harm: if its
	// vendor was renamed or removed it simply matches no candidate, stickiness
	// contributes nothing, and the next dispatch overwrites it with wherever the
	// session actually landed. Clearing them would instead cost a cold prompt
	// for every active session on every config edit — real money, to solve a
	// problem that resolves itself.
}

// VendorState is a read-only view of one vendor's live routing state, for the
// admin API. It reflects the router's in-memory decision inputs, which is a
// different thing from the ledger-derived stats on the same view: this is what
// routing will do NEXT, not what traffic did historically.
type VendorState struct {
	Vendor       string    `json:"vendor"`
	Cooling      bool      `json:"cooling"`
	CoolingUntil time.Time `json:"cooling_until,omitempty"`
	// Dead reports that the vendor failed consistently enough to be presumed
	// broken. Unlike Cooling this does not lapse on a timer: it clears on a
	// success or when an operator disables the provider. Surface it prominently
	// — it is the state that wants a human.
	Dead        bool `json:"dead"`
	ConsecFails int  `json:"consec_fails"`
	Demotions   int  `json:"demotions"`
	// Sessions is how many live agent sessions are currently pinned here.
	Sessions int `json:"sessions"`
}

// Inspect returns the live routing state of every vendor the router has an
// opinion about, sorted by name. Vendors that have never been reported on are
// absent — they are live by default.
func (r *Router) Inspect() []VendorState {
	now := r.now()

	r.mu.Lock()
	out := make([]VendorState, 0, len(r.health))
	for name, h := range r.health {
		st := VendorState{
			Vendor:      name,
			Dead:        h.dead,
			ConsecFails: h.consecFail,
			Demotions:   h.demotions,
		}
		if !h.openUntil.IsZero() && now.Before(h.openUntil) {
			st.Cooling = true
			st.CoolingUntil = h.openUntil
		}
		out = append(out, st)
	}
	r.mu.Unlock()

	// Session counts come from the affinity map, which has its own lock — read
	// them after releasing r.mu so the two are never held at once.
	for i := range out {
		out[i].Sessions = r.affinity.countFor(out[i].Vendor)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	return out
}

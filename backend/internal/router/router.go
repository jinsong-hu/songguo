// Package router selects a vendor for a requested model.
//
// For each request the router produces an ordered list of candidates
// ([]Target). The proxy forwards to the FIRST candidate only — there is no
// per-call failover. Choosing among several candidates is still a real
// decision, but a cross-request one: the router observes how each vendor
// behaved on previous requests and steers the NEXT request, never retrying the
// current one.
//
// The ordering is one lexicographic sort key per vendor:
//
//	(health, sticky, priority, draw, decl)
//
//	health   0 live, 1 cooling, 2 dead        — see health.go
//	sticky   0 if this session is pinned here — see affinity.go
//	priority v.Priority, lower preferred      — config
//	draw     weighted random, lower wins      — Weight
//	decl     declaration order                — deterministic tiebreak
//
// Read it as a cascade of questions. Is this vendor working? Does this session
// already have a provider? Which tier is preferred? Within a tier, whose turn is
// it? Each question only gets asked when the previous one ties.
//
// The health component also absorbs per-model rate limits: a 429 demotes the
// (vendor, model) pair rather than the vendor, so one hot model cannot walk a
// vendor's whole catalogue out of rotation. See SignalFailModel.
//
// Health first is what makes stickiness safe. Because a pin is only consulted
// among vendors of equal health, it can never hold a session on a broken
// provider — the guarantee is structural, not a property of how stickiness
// happens to be computed. It also means a pin still counts when every candidate
// is degraded: forced onto a cooling vendor, we pick the one already holding
// this session's warm cache rather than flipping a coin.
//
// Priority is a STRICT tier, not a weight: while any priority-1 vendor is live,
// priority-2 receives nothing. That keeps two knobs with one job each — same
// priority + different weights is load balancing, different priority is
// failover. To split 90/10, give both vendors the same priority and weights 9
// and 1.
//
// Health outranks priority deliberately. `priority` means "prefer this one when
// the candidates are otherwise interchangeable", and a vendor that is failing is
// not interchangeable. Ranking health below priority would mean a priority-1
// primary never yields to its priority-2 backup — which is the entire point of
// the feature.
//
// Stickiness outranks priority but yields to health. It beats priority because
// a pin only exists because some earlier request's full ordering — priority
// included — chose that vendor, and migrating a live session back to a recovered
// primary burns a warm cache to satisfy a preference among vendors we already
// called interchangeable. It yields to health because no amount of cache
// locality is worth sending a request to a vendor that is failing.
//
// INVARIANT — the router orders; it never denies and never filters. Select
// returns a permutation of its input: same length, same vendors, different
// order. Health DEMOTES, it never excludes. So no candidate list can be emptied
// by health state, and no gateway-invented refusal can originate here. That is
// what keeps cross-request steering inside songguo's behavioral transparency
// guarantee: we never invent an attempt, we only choose which single attempt to
// make (see CLAUDE.md).
//
// It reads the live config Snapshot on every call so hot-reloads are honored.
// Mutable state is per-vendor health and per-session affinity, both
// mutex-guarded; every exported method is safe for concurrent use.
package router

import (
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/songguo/songguo/internal/config"
)

// ErrNoVendor is returned by Select when no configured vendor matches the
// selector. It reports a CONFIG gap (nothing serves this model / provider id),
// never a runtime one — health demotes but never excludes, so it can never be
// the reason a candidate list came back empty.
var ErrNoVendor = errors.New("router: no vendor serves model")

// Default routing knobs. They are process-wide rather than per-provider: an
// operator has no way to know a good cooldown for one vendor but not another,
// and per-provider tuning is a footgun with no evidence anyone wants it.
const (
	// defaultFailThreshold is how many CONSECUTIVE failures demote a vendor.
	// The implementation removed in 2026-07 tripped on a single failure, which
	// let one 429 from a busy-but-healthy vendor flap the whole rotation.
	defaultFailThreshold = 3
	// defaultCooldown is the FIRST demotion's cooldown. Repeat demotions back
	// off from here — see nextCooldown.
	defaultCooldown = 30 * time.Second
	// defaultCooldownMax caps the backoff. Long enough that a dead vendor costs
	// ~4 failed requests an hour instead of 120, short enough that a recovered
	// one is noticed the same afternoon.
	defaultCooldownMax = 15 * time.Minute
	// defaultDeadAfter is how many consecutive demotions mark a vendor DEAD:
	// ranked below merely-cooling vendors and, unlike a cooldown, never
	// restored by the passage of time. See health.go.
	defaultDeadAfter = 5
	// defaultModelCooldown is how long one model stays demoted on one vendor
	// after a 429. Longer than a demotion cooldown because a quota window is
	// typically a minute, and unlike vendor health there is no streak to work
	// through — a single 429 is already a definite statement.
	defaultModelCooldown = time.Minute
	// defaultStickyTTL is how long a session keeps its provider after its last
	// request. Anthropic's prompt cache is 5m (1h extended), so a session idle
	// this long has lost the cache the pin exists to protect.
	defaultStickyTTL = 30 * time.Minute
	// defaultStickyMax bounds the affinity map so it cannot grow without limit.
	defaultStickyMax = 50_000
)

// Target is a single attempt: a vendor paired with its credential.
type Target struct {
	Vendor     config.Vendor
	Credential config.Credential
}

// Scope is how a Selector narrows the candidate set. It is explicit rather than
// inferred from which fields happen to be set, so that an empty Model under
// ScopeModel stays "nothing serves the empty model" instead of silently
// widening to every vendor.
type Scope int

const (
	// ScopeAll selects every configured vendor: the default for a model-less
	// request with no provider pin. Wire matching downstream narrows it to the
	// vendors that actually serve the requested path.
	ScopeAll Scope = iota
	// ScopeModel selects the vendors that serve Selector.Model.
	ScopeModel
	// ScopeProvider selects every vendor derived from Selector.ProviderID,
	// regardless of model.
	ScopeProvider
)

// Selector describes what a request needs from the router. The zero value is a
// ScopeAll selector with no affinity, matching AllCandidates.
type Selector struct {
	Scope      Scope
	Model      string // body model; used under ScopeModel
	ProviderID string // X-Songguo-Provider pin; used under ScopeProvider
	// Session is the caller's agent session, used to keep a conversation on one
	// provider so its prompt cache stays warm. Empty disables affinity, which is
	// the honest default for clients that do not identify their session — we
	// never REQUIRE a header to get routed (see CLAUDE.md).
	Session string
}

// affinityKey is the key a session pins under. It is scoped by the selector as
// well as the session, because one agent session can address several models and
// each needs its own provider: model A may be served by vendors 1 and 2 while
// model B is served by 3 and 4, and a single pin could not satisfy both. The
// caller still sees plain "this session sticks to one provider".
func (s Selector) affinityKey() string {
	if s.Session == "" {
		return ""
	}
	return s.Session + "\x00" + s.scopeKey()
}

func (s Selector) scopeKey() string {
	switch s.Scope {
	case ScopeModel:
		return s.Model
	case ScopeProvider:
		return s.ProviderID
	default:
		return ""
	}
}

// Options carries the routing knobs. The zero value is valid and yields the
// defaults; tests override Now to drive cooldown expiry without sleeping, and
// Rand to make the weighted draw deterministic.
type Options struct {
	Now           func() time.Time // injectable clock; defaults to time.Now
	Rand          func() float64   // injectable [0,1) source; defaults to rand.Float64
	FailThreshold int              // consecutive failures that demote; default 3
	Cooldown      time.Duration    // first demotion's cooldown; default 30s
	CooldownMax   time.Duration    // backoff ceiling; default 15m
	DeadAfter     int              // consecutive demotions that mark dead; default 5
	ModelCooldown time.Duration    // per-(vendor, model) 429 cooldown; default 60s
	StickyTTL     time.Duration    // session affinity idle timeout; default 30m
	StickyMax     int              // affinity map hard cap; default 50k
	Logger        *slog.Logger     // demotion/restoration log; defaults to slog.Default
}

// Router selects ordered upstream attempts for a request.
type Router struct {
	snapshot func() *config.Snapshot
	now      func() time.Time
	rand     func() float64
	logger   *slog.Logger

	failThreshold int
	cooldown      time.Duration
	cooldownMax   time.Duration
	deadAfter     int
	modelCooldown time.Duration

	affinity *affinity

	mu     sync.Mutex
	health map[string]*vendorHealth // vendor name -> cross-request health
	// modelCooling holds per-(vendor, model) rate-limit cooldowns, kept apart
	// from vendor health because a 429 on one model says nothing about the
	// vendor's other models. Expiry is by timestamp, so stale entries are inert;
	// ResetHealth clears them on config reload.
	modelCooling map[string]time.Time
}

// New constructs a Router that reads config through snapshot on every call.
// Options are optional and only the first is used; New(snap) yields defaults.
func New(snapshot func() *config.Snapshot, opts ...Options) *Router {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Rand == nil {
		// math/rand/v2's top-level source is goroutine-safe and seeded from the
		// runtime, so there is nothing to initialize.
		opt.Rand = rand.Float64
	}
	if opt.FailThreshold <= 0 {
		opt.FailThreshold = defaultFailThreshold
	}
	if opt.Cooldown <= 0 {
		opt.Cooldown = defaultCooldown
	}
	if opt.CooldownMax <= 0 {
		opt.CooldownMax = defaultCooldownMax
	}
	if opt.CooldownMax < opt.Cooldown {
		opt.CooldownMax = opt.Cooldown
	}
	if opt.DeadAfter <= 0 {
		opt.DeadAfter = defaultDeadAfter
	}
	if opt.ModelCooldown <= 0 {
		opt.ModelCooldown = defaultModelCooldown
	}
	if opt.StickyTTL <= 0 {
		opt.StickyTTL = defaultStickyTTL
	}
	if opt.StickyMax <= 0 {
		opt.StickyMax = defaultStickyMax
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	return &Router{
		snapshot:      snapshot,
		now:           opt.Now,
		rand:          opt.Rand,
		logger:        opt.Logger,
		failThreshold: opt.FailThreshold,
		cooldown:      opt.Cooldown,
		cooldownMax:   opt.CooldownMax,
		deadAfter:     opt.DeadAfter,
		modelCooldown: opt.ModelCooldown,
		affinity:      newAffinity(opt.Now, opt.StickyTTL, opt.StickyMax),
		health:        make(map[string]*vendorHealth),
		modelCooling:  make(map[string]time.Time),
	}
}

// Select returns the ordered candidates for sel, best first. The result is a
// permutation of the vendors matching sel — the router reorders, it never drops
// (see the package invariant). Returns ErrNoVendor when nothing matches.
func (r *Router) Select(sel Selector) ([]Target, error) {
	snap := r.snapshot()
	if snap == nil {
		return nil, ErrNoVendor
	}

	var vendors []config.Vendor
	switch sel.Scope {
	case ScopeModel:
		vendors = snap.VendorsForModel(sel.Model)
	case ScopeProvider:
		for _, v := range snap.Vendors() {
			if v.Credential.ID == sel.ProviderID {
				vendors = append(vendors, v)
			}
		}
	default:
		vendors = snap.Vendors()
	}
	if len(vendors) == 0 {
		return nil, ErrNoVendor
	}
	return r.order(sel, vendors), nil
}

// Candidates returns the ordered attempts for model. Returns ErrNoVendor if
// nothing serves the model.
func (r *Router) Candidates(model string) ([]Target, error) {
	return r.Select(Selector{Scope: ScopeModel, Model: model})
}

// CandidatesForProvider returns the ordered attempts for every vendor derived
// from the given provider id (its credential id), regardless of model. It backs
// requests that pin a provider with X-Songguo-Provider but carry no model to
// route on — model-less wires and WebSocket upgrades. The (origin, adapter)
// split means one provider can yield several vendors; wire matching downstream
// narrows them to the one serving the requested path. Returns ErrNoVendor if no
// vendor carries that provider id.
func (r *Router) CandidatesForProvider(providerID string) ([]Target, error) {
	return r.Select(Selector{Scope: ScopeProvider, ProviderID: providerID})
}

// AllCandidates returns the ordered attempts across every configured vendor. It
// is the default selection for a model-less request with no provider pin: wire
// matching downstream narrows these to the vendors that serve the requested
// path, and this ordering picks the default among them. Returns ErrNoVendor
// when no vendor is configured at all.
func (r *Router) AllCandidates() ([]Target, error) {
	return r.Select(Selector{Scope: ScopeAll})
}

// Pin records that sel's session was actually served by vendorName, so the
// session's next request prefers the same provider and reuses its prompt cache.
//
// The proxy calls this with the vendor it DISPATCHED to, which is not
// necessarily Select's first candidate — wire matching and user scope narrow
// the list afterwards. Pinning the real choice keeps affinity honest and means
// every breakage case (vendor demoted, removed from config, TTL lapsed) is
// handled by one rule instead of four: whatever we actually used becomes the
// new pin.
func (r *Router) Pin(sel Selector, vendorName string) {
	key := sel.affinityKey()
	if key == "" || vendorName == "" {
		return
	}
	r.affinity.set(key, vendorName)
}

// rankedVendor pairs a vendor with its sort key components.
type rankedVendor struct {
	vendor   config.Vendor
	sticky   int
	health   int
	priority int
	draw     float64
	decl     int
}

// order sorts vendors by the package's lexicographic sort key and flattens them
// to Targets.
func (r *Router) order(sel Selector, vendors []config.Vendor) []Target {
	pinned := r.affinity.get(sel.affinityKey())

	// One draw per CREDENTIAL, not per vendor. A provider is split into several
	// routing vendors by (origin, adapter), and drawing for each of them would
	// give that provider several chances to win — two equal-weight providers
	// would split 2:1 rather than 1:1 purely because one declares two protocols.
	// Sharing the draw makes `weight` mean "this provider's share of traffic",
	// which is what the config UI says it means.
	//
	// Vendors of one credential therefore tie on draw, and the declaration-order
	// tiebreak settles them. That is harmless: wire matching downstream keeps
	// only the vendor that serves the requested path.
	//
	// Drawn under the lock so that an injected Rand need not be goroutine-safe.
	r.mu.Lock()
	now := r.now()

	draws := make(map[string]float64, len(vendors))
	for _, v := range vendors {
		if _, seen := draws[v.Credential.ID]; !seen {
			draws[v.Credential.ID] = weightedDraw(r.rand(), v.Weight)
		}
	}

	ranks := make([]rankedVendor, len(vendors))
	for i, v := range vendors {
		hr := r.healthRank(v.Name, now)
		// A model-scoped rate limit demotes this vendor for THIS request without
		// touching its health for any other model. Folding it into the health
		// rank keeps the sort key at five components and preserves the
		// permutation invariant — it demotes, it never excludes.
		if hr == healthLive && r.modelIsCooling(v.Name, sel.Model, now) {
			hr = healthCooling
		}
		ranks[i] = rankedVendor{
			vendor: v,
			// Stickiness is a pure fact — is this the session's vendor — with no
			// health mixed in. Health outranks it in the sort, which is what
			// guarantees a pin can never hold a session on a broken provider.
			sticky:   boolRank(v.Name == pinned),
			health:   hr,
			priority: v.Priority,
			draw:     draws[v.Credential.ID],
			decl:     i,
		}
	}
	r.mu.Unlock()

	sort.SliceStable(ranks, func(a, b int) bool {
		x, y := ranks[a], ranks[b]
		if x.health != y.health {
			return x.health < y.health
		}
		if x.sticky != y.sticky {
			return x.sticky < y.sticky
		}
		if x.priority != y.priority {
			return x.priority < y.priority
		}
		if x.draw != y.draw {
			return x.draw < y.draw
		}
		return x.decl < y.decl
	})

	targets := make([]Target, len(ranks))
	for i, rv := range ranks {
		targets[i] = Target{Vendor: rv.vendor, Credential: rv.vendor.Credential}
	}
	return targets
}

// weightedDraw converts a uniform sample into a sort value where a vendor with
// twice the weight wins twice as often. Lower wins, matching every other
// component of the sort key.
//
// This replaces the rotation cursor the router used to keep. A cursor spreads
// traffic exactly — 3:1 weights give precisely three of every four requests —
// but it needs shared mutable state per (scope, priority), which grew without
// bound and had to be locked on every request. Sampling is stateless and
// correct in expectation, which is all a load split needs; the cost is that a
// short burst can deviate from the ratio.
//
// The transform is the standard exponential race: -ln(U)/w. Its minimum over a
// set of vendors lands on vendor i with probability w_i / sum(w), which is the
// definition of weighted selection.
func weightedDraw(u float64, weight int) float64 {
	if weight < 1 {
		weight = 1
	}
	// rand.Float64 returns [0,1); shift to (0,1] so the log is always finite.
	u = 1 - u
	return -math.Log(u) / float64(weight)
}

func boolRank(b bool) int {
	if b {
		return 0
	}
	return 1
}

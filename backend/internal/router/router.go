// Package router selects a vendor for a requested model.
//
// For each model the router produces an ordered list of candidates ([]Target)
// honoring, in order: priority groups (lower first) then weighted round-robin
// within a group. The proxy forwards to the first candidate only — there is no
// per-call failover, and (today) no health-based demotion: a failing vendor is
// NOT automatically brought down, so it stays selected until an operator changes
// config. Each vendor contributes exactly one candidate with its single
// credential; spreading load across multiple keys is done by configuring
// multiple services that serve the same model.
//
// Automatically bringing a persistently-failing vendor down is deliberately not
// implemented yet. When it is, it belongs here as a cross-request health
// decision (steer the NEXT request), never as a per-call retry.
//
// It reads the live config Snapshot on every call so hot-reloads are honored,
// and keeps only small per-(model,priority) rotation counters. The counters are
// mutex-guarded; Candidates is safe for concurrent use.
package router

import (
	"errors"
	"sort"
	"strconv"
	"sync"

	"github.com/songguo/songguo/internal/config"
)

// ErrNoVendor is returned by Candidates when no configured vendor serves the
// requested model.
var ErrNoVendor = errors.New("router: no vendor serves model")

// Target is a single attempt: a vendor paired with its credential.
type Target struct {
	Vendor     config.Vendor
	Credential config.Credential
}

// Router selects ordered upstream attempts for a model.
type Router struct {
	snapshot func() *config.Snapshot

	mu  sync.Mutex
	wrr map[string]int // "model\x00priority" -> next weighted-RR cursor
}

// New constructs a Router that reads config through snapshot on every call.
func New(snapshot func() *config.Snapshot) *Router {
	return &Router{
		snapshot: snapshot,
		wrr:      make(map[string]int),
	}
}

// Candidates returns the ordered list of attempts for model. The order is:
// priority group ascending; within a group, weighted round-robin order. Returns
// ErrNoVendor if nothing serves the model.
func (r *Router) Candidates(model string) ([]Target, error) {
	snap := r.snapshot()
	var vendors []config.Vendor
	if snap != nil {
		vendors = snap.VendorsForModel(model)
	}
	if len(vendors) == 0 {
		return nil, ErrNoVendor
	}
	return r.order(model, vendors), nil
}

// CandidatesForProvider returns the ordered attempts for every vendor derived
// from the given provider id (its credential id), regardless of model. It backs
// requests that pin a provider with X-Songguo-Provider but carry no model to
// route on — model-less wires and WebSocket upgrades. The (origin, adapter)
// split means one provider can yield several vendors; wire matching downstream
// narrows them to the one serving the requested path. Returns ErrNoVendor if no
// vendor carries that provider id.
func (r *Router) CandidatesForProvider(providerID string) ([]Target, error) {
	snap := r.snapshot()
	if snap == nil {
		return nil, ErrNoVendor
	}
	var vendors []config.Vendor
	for _, v := range snap.Vendors() {
		if v.Credential.ID == providerID {
			vendors = append(vendors, v)
		}
	}
	if len(vendors) == 0 {
		return nil, ErrNoVendor
	}
	return r.order(providerID, vendors), nil
}

// AllCandidates returns the ordered attempts across every configured vendor. It
// is the default selection for a model-less request with no provider pin: wire
// matching downstream narrows these to the vendors that serve the requested
// path, and priority/health ordering picks the default among them. Returns
// ErrNoVendor when no vendor is configured at all.
func (r *Router) AllCandidates() ([]Target, error) {
	snap := r.snapshot()
	if snap == nil {
		return nil, ErrNoVendor
	}
	vendors := snap.Vendors()
	if len(vendors) == 0 {
		return nil, ErrNoVendor
	}
	return r.order("", vendors), nil
}

// order groups vendors by priority (ascending) and orders each group by
// weighted round-robin, then flattens to Targets. key namespaces the
// round-robin cursor — the model, the provider id, or "" for the global default.
func (r *Router) order(key string, vendors []config.Vendor) []Target {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Group vendors by priority, preserving declaration order within a group.
	groups := make(map[int][]config.Vendor)
	var priorities []int
	for _, v := range vendors {
		if _, seen := groups[v.Priority]; !seen {
			priorities = append(priorities, v.Priority)
		}
		groups[v.Priority] = append(groups[v.Priority], v)
	}
	sort.Ints(priorities)

	var targets []Target
	for _, prio := range priorities {
		for _, v := range r.weightedOrder(key, prio, groups[prio]) {
			targets = append(targets, Target{Vendor: v, Credential: v.Credential})
		}
	}
	return targets
}

// weightedOrder returns the group's vendors in weighted round-robin order using
// a per-(key,priority) cursor that advances by one each call, so successive
// calls rotate the starting vendor proportional to Weight.
func (r *Router) weightedOrder(key string, prio int, group []config.Vendor) []config.Vendor {
	if len(group) == 1 {
		return group
	}

	// Build a weighted slot list: each vendor appears Weight times, interleaved
	// so equal weights round-robin and higher weights appear more often.
	total := 0
	for _, v := range group {
		w := v.Weight
		if w < 1 {
			w = 1
		}
		total += w
	}

	wk := wrrKey(key, prio)
	start := r.wrr[wk]
	r.wrr[wk] = (start + 1) % total

	// Generate the interleaved slot sequence deterministically, then rotate by
	// start and dedup to produce a vendor ordering for this call.
	slots := interleave(group)
	out := make([]config.Vendor, 0, len(group))
	seen := make(map[string]struct{}, len(group))
	for i := 0; i < len(slots); i++ {
		v := slots[(start+i)%len(slots)]
		if _, dup := seen[v.Name]; dup {
			continue
		}
		seen[v.Name] = struct{}{}
		out = append(out, v)
	}
	return out
}

// interleave expands vendors into a slot sequence where each vendor occurs
// Weight times, spread out via largest-remainder placement so that, over many
// rotations, each vendor leads proportional to its weight.
func interleave(group []config.Vendor) []config.Vendor {
	total := 0
	weights := make([]int, len(group))
	for i, v := range group {
		w := v.Weight
		if w < 1 {
			w = 1
		}
		weights[i] = w
		total += w
	}

	slots := make([]config.Vendor, 0, total)
	// Stride placement: assign each output position to the vendor whose ideal
	// cumulative share is most "owed", giving an even interleave.
	acc := make([]float64, len(group))
	for pos := 0; pos < total; pos++ {
		best := -1
		var bestVal float64
		for i := range group {
			acc[i] += float64(weights[i]) / float64(total)
			if best == -1 || acc[i] > bestVal {
				best = i
				bestVal = acc[i]
			}
		}
		acc[best] -= 1
		slots = append(slots, group[best])
	}
	return slots
}

func wrrKey(key string, prio int) string {
	return key + "\x00" + strconv.Itoa(prio)
}

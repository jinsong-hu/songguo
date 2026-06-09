// Package router selects a vendor and credential for a requested model.
//
// For each model the router produces an ordered list of attempts ([]Target)
// honoring, in order: priority groups (lower first), weighted round-robin
// within a group, health-based failover (vendors in cooldown pushed to the
// back), and per-vendor credential rotation across the key pool (号池).
//
// It reads the live config Snapshot on every call so hot-reloads are honored,
// and keeps only small per-(model,priority) and per-vendor rotation counters
// plus a vendor health map. All mutable state is mutex-guarded; Candidates and
// Report are safe for concurrent use.
package router

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/songguo/songguo/internal/config"
)

// ErrNoVendor is returned by Candidates when no configured vendor serves the
// requested model.
var ErrNoVendor = errors.New("router: no vendor serves model")

// cooldown is how long a vendor stays demoted after a failure.
const cooldown = 30 * time.Second

// Target is a single attempt: a vendor paired with one of its credentials.
type Target struct {
	Vendor     config.Vendor
	Credential config.Credential
}

// Router selects ordered upstream attempts for a model.
type Router struct {
	snapshot func() *config.Snapshot
	now      func() time.Time // injectable clock for tests

	mu       sync.Mutex
	wrr      map[string]int       // "model\x00priority" -> next weighted-RR cursor
	credRR   map[string]int       // vendor name -> next credential offset
	coolDown map[string]time.Time // vendor name -> cooldown expiry
}

// New constructs a Router that reads config through snapshot on every call.
func New(snapshot func() *config.Snapshot) *Router {
	return &Router{
		snapshot: snapshot,
		now:      time.Now,
		wrr:      make(map[string]int),
		credRR:   make(map[string]int),
		coolDown: make(map[string]time.Time),
	}
}

// Candidates returns the ordered list of attempts for model. The order is:
// priority group ascending; within a group, healthy vendors before cooling-down
// vendors, each in weighted round-robin order; per vendor, its credentials in
// rotated order. Returns ErrNoVendor if nothing serves the model.
func (r *Router) Candidates(model string) ([]Target, error) {
	snap := r.snapshot()
	var vendors []config.Vendor
	if snap != nil {
		vendors = snap.VendorsForModel(model)
	}
	if len(vendors) == 0 {
		return nil, ErrNoVendor
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()

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
		ordered := r.orderGroup(model, prio, groups[prio], now)
		for _, v := range ordered {
			targets = append(targets, r.credTargets(v)...)
		}
	}
	return targets, nil
}

// orderGroup orders one priority group: weighted round-robin first, then a
// stable partition placing cooling-down vendors after healthy ones.
func (r *Router) orderGroup(model string, prio int, group []config.Vendor, now time.Time) []config.Vendor {
	ordered := r.weightedOrder(model, prio, group)

	healthy := make([]config.Vendor, 0, len(ordered))
	cooling := make([]config.Vendor, 0, len(ordered))
	for _, v := range ordered {
		if until, ok := r.coolDown[v.Name]; ok && now.Before(until) {
			cooling = append(cooling, v)
		} else {
			healthy = append(healthy, v)
		}
	}
	return append(healthy, cooling...)
}

// weightedOrder returns the group's vendors in weighted round-robin order using
// a per-(model,priority) cursor that advances by one each call, so successive
// calls rotate the starting vendor proportional to Weight.
func (r *Router) weightedOrder(model string, prio int, group []config.Vendor) []config.Vendor {
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

	key := wrrKey(model, prio)
	start := r.wrr[key]
	r.wrr[key] = (start + 1) % total

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

// credTargets emits a vendor's credentials in rotated order, advancing the
// per-vendor offset by one so repeated calls and same-vendor failover spread
// across the key pool.
func (r *Router) credTargets(v config.Vendor) []Target {
	creds := v.Credentials
	if len(creds) == 0 {
		return nil
	}
	start := r.credRR[v.Name]
	r.credRR[v.Name] = (start + 1) % len(creds)

	out := make([]Target, 0, len(creds))
	for i := 0; i < len(creds); i++ {
		out = append(out, Target{Vendor: v, Credential: creds[(start+i)%len(creds)]})
	}
	return out
}

// Report records the outcome of an attempt. A transport error or a status of
// 429 or >=500 puts the vendor into a cooldown; any other success clears it.
func (r *Router) Report(vendorName, credentialID string, status int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil || status == 429 || status >= 500 {
		r.coolDown[vendorName] = r.now().Add(cooldown)
		return
	}
	delete(r.coolDown, vendorName)
}

func wrrKey(model string, prio int) string {
	return model + "\x00" + strconv.Itoa(prio)
}

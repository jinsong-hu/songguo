// Package configsvc builds the live routing Snapshot from SQLite-backed provider
// rows, replacing the file-based config.Manager as the source of truth.
//
// It holds an atomic *config.Snapshot rebuilt on demand (Reload) after any
// dashboard write. The router, proxy, and admin API consume it through the same
// Current func() *config.Snapshot signature the file manager exposed, so the
// rest of the gateway is unchanged — only the snapshot's source moved from a
// YAML file to the database.
//
// Robustness: a single incomplete provider (no credentials, no models, or
// disabled) is skipped rather than allowed to fail the whole snapshot build, so
// a half-configured provider can never take routing down.
package configsvc

import (
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// Manager owns the live snapshot derived from the store.
type Manager struct {
	store   *store.Store
	logger  *slog.Logger
	current atomic.Pointer[config.Snapshot]
}

// NewManager builds the initial snapshot from the store and returns a ready
// Manager. A build error at startup is non-fatal: it logs and starts empty so
// the gateway still serves (an admin can then fix the offending provider).
func NewManager(st *store.Store, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{store: st, logger: logger}
	if err := m.Reload(); err != nil {
		logger.Error("initial config build failed; starting empty", "err", err)
		m.current.Store(emptySnapshot())
	}
	return m, nil
}

// Current returns the live snapshot. Never nil after construction.
func (m *Manager) Current() *config.Snapshot {
	return m.current.Load()
}

// Reload rebuilds the snapshot from the store and swaps it in atomically. On
// failure it keeps the previous snapshot (if any) and returns the error.
func (m *Manager) Reload() error {
	snap, err := m.build()
	if err != nil {
		return err
	}
	m.current.Store(snap)
	m.logger.Info("config reloaded from store", "vendors", len(snap.Vendors()))
	return nil
}

// build assembles a config.Config from the store and validates it into a
// Snapshot. Incomplete/disabled providers are skipped with a warning.
func (m *Manager) build() (*config.Snapshot, error) {
	providers, err := m.store.ListProviders()
	if err != nil {
		return nil, fmt.Errorf("configsvc: list providers: %w", err)
	}
	cat, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("configsvc: load catalog: %w", err)
	}
	cfg := config.Config{}
	for _, pvd := range providers {
		if !pvd.Enabled {
			continue
		}
		if pvd.APIKey == "" || len(pvd.Models) == 0 {
			m.logger.Warn("skipping incomplete provider (no API key or models)",
				"provider", pvd.Name, "has_key", pvd.APIKey != "", "models", len(pvd.Models))
			continue
		}
		cfg.Vendors = append(cfg.Vendors, vendorsFromProvider(pvd, cat, m.logger)...)
	}

	return config.Build(cfg)
}

// vendorsFromProvider projects a stored provider into one or more config.Vendors
// for routing: its endpoints are grouped by (origin, adapter), and each group
// becomes a vendor carrying that group's wire→endpoint map. Wire names not present
// in the registry are dropped with a warning so a typo can never silently match.
// The shared API key, models/prices, and quirks are replicated onto every group.
// The first group keeps the provider's name (so vendor names and stats stay
// stable for single-host providers); additional groups get an "-<adapter>"
// suffix. Every group carries the provider id as its credential id, so an
// X-Songguo-Provider pin resolves across the split.
func vendorsFromProvider(pvd store.Provider, cat catalog.Catalog, logger *slog.Logger) []config.Vendor {
	// Pass 1: resolve each model's published price (catalog, or the stored row).
	models := make([]string, 0, len(pvd.Models))
	prices := make(map[string]config.Price, len(pvd.Models))
	modelRoutes := make(map[string]config.ModelRoute, len(pvd.Models))
	for _, m := range pvd.Models {
		models = append(models, m.Model)
		prices[m.Model] = effectivePrice(pvd.CatalogID, m, cat)
		priority := pvd.Priority
		if m.PriorityOverride != nil {
			priority = *m.PriorityOverride
		}
		weight := pvd.Weight
		if m.WeightOverride != nil {
			weight = *m.WeightOverride
		}
		enabled := true
		if m.RoutingConfigured {
			enabled = m.RoutingEnabled
		}
		modelRoutes[m.Model] = config.ModelRoute{
			Enabled:  enabled,
			Priority: priority,
			Weight:   weight,
		}
	}

	// Pass 2: a model with no published rate would meter every call as $0, which
	// is indistinguishable from "free" in the ledger and under-bills real usage.
	// Give it the most expensive rate this same provider charges in the same unit
	// family instead, so an unpriced model errs toward over-billing and surfaces
	// as an outlier rather than vanishing. See fallbackPrice for the rules.
	//
	// The gate is provenance, NOT the number: only PriceSourceUnpriced — nobody
	// ever stated a rate — is eligible. A zero that someone published is a real
	// price meaning free (the catalog lists genuinely free tiers, and an operator
	// override of zero is a deliberate "don't bill this"), and re-pricing it at
	// the provider's ceiling would invent a charge for a model that costs nothing.
	for _, m := range pvd.Models {
		p := prices[m.Model]
		if p.Source != config.PriceSourceUnpriced {
			continue
		}
		fb, from, ok := fallbackPrice(prices, m.Model, p.Unit)
		if !ok {
			continue
		}
		fb.Source = config.PriceSourceFallbackPrefix + from
		prices[m.Model] = fb
	}

	// Price-completeness warnings (non-fatal): a provider must still route when
	// part of its price table is missing or suspect. Warn, don't block.
	for _, m := range pvd.Models {
		p := prices[m.Model]
		switch {
		case !config.KnownPriceUnits[p.Unit]:
			logger.Warn("price unit not recognized; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit)
		case config.PriceMetersZero(p) && p.Source == config.PriceSourceOverride:
			logger.Warn("price is explicitly overridden to zero; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit)
		case config.PriceMetersZero(p) && p.Source == config.PriceSourceCatalog:
			logger.Warn("catalog publishes this model as free; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit)
		case config.PriceMetersZero(p) && p.Source == config.PriceSourceUnpriced:
			logger.Warn("price is zero and no same-unit price exists to fall back to; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit)
		case config.PriceMetersZero(p):
			logger.Warn("price meters as $0 for its unit; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit)
		case config.IsFallbackPrice(p):
			logger.Warn("model has no published price; metering it at this provider's most expensive same-unit rate",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit,
				"borrowed_from", strings.TrimPrefix(p.Source, config.PriceSourceFallbackPrefix),
				"input", p.Input, "output", p.Output)
		case config.PriceRateImplausible(p):
			logger.Warn("price rate is implausibly high for its unit; check the unit — it meters as written but cannot be borrowed as a fallback",
				"provider", pvd.Name, "model", m.Model, "unit", p.Unit,
				"input", p.Input, "output", p.Output)
		}
	}

	// Group endpoints by (origin, adapter): a group shares a credential, auth
	// scheme, and host. Each wire's full URL is kept verbatim for model-routed
	// forwarding; the shared origin serves WebSocket/unmatched paths.
	type groupKey struct{ origin, adapter string }
	order := make([]groupKey, 0, len(pvd.Endpoints))
	groups := make(map[groupKey][]store.ProviderEndpoint)
	for _, ep := range pvd.Endpoints {
		if _, ok := wire.Get(ep.Wire); !ok {
			logger.Warn("dropping unknown wire from provider endpoints", "provider", pvd.Name, "wire", ep.Wire)
			continue
		}
		k := groupKey{originOf(ep.Endpoint), ep.Adapter}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], ep)
	}

	// Stable, intuitive primary: openai-compatible groups rank first, then by
	// origin. The primary group keeps the provider's plain name (so vendor names
	// and stats stay stable); others get a unique suffix.
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := adapterRank(order[i].adapter), adapterRank(order[j].adapter)
		if ri != rj {
			return ri < rj
		}
		return order[i].origin < order[j].origin
	})

	vendors := make([]config.Vendor, 0, len(order))
	usedNames := make(map[string]struct{}, len(order))
	for i, k := range order {
		name := pvd.Name
		if i > 0 {
			name = pvd.Name + "-" + adapterSlug(k.adapter)
			for n := 2; ; n++ {
				if _, clash := usedNames[name]; !clash {
					break
				}
				name = fmt.Sprintf("%s-%s-%d", pvd.Name, adapterSlug(k.adapter), n)
			}
		}
		usedNames[name] = struct{}{}
		eps := groups[k]
		endpoints := make(map[string]string, len(eps))
		wires := make([]string, 0, len(eps))
		for _, ep := range eps {
			endpoints[ep.Wire] = ep.Endpoint
			wires = append(wires, ep.Wire)
		}
		vendors = append(vendors, config.Vendor{
			Name:           name,
			Origin:         k.origin,
			Adapter:        k.adapter,
			ServedModels:   models,
			Priority:       pvd.Priority,
			Weight:         pvd.Weight,
			ModelRoutes:    modelRoutes,
			Credential:     config.Credential{ID: pvd.ID, APIKey: pvd.APIKey},
			Prices:         prices,
			Wires:          wires,
			Endpoints:      endpoints,
			AllowUnmatched: pvd.AllowUnmatched,
			// Replicated onto every vendor of this provider ON PURPOSE: the limit
			// is per credential, and these vendors share one. See config.Vendor.
			MaxConcurrency: pvd.MaxConcurrency,
			Quirks:         pvd.Quirks,
		})
	}
	return vendors
}

func effectivePrice(catalogID string, m store.ProviderModel, cat catalog.Catalog) config.Price {
	if !m.PriceOverride {
		if p, ok := catalogModelPrice(cat, catalogID, m.Model); ok {
			return p
		}
		if p, ok := catalogAnyModelPrice(cat, m.Model); ok {
			return p
		}
	}
	unit := m.Unit
	if unit == "" {
		unit = "per_1m_tokens"
	}
	source := config.PriceSourceStored
	switch {
	case m.PriceOverride:
		source = config.PriceSourceOverride
	case m.Input <= 0 && m.Output <= 0:
		// No catalog entry and no operator-entered rate: nothing priced this
		// model. Pass 2 in vendorsFromProvider may replace it with a fallback.
		source = config.PriceSourceUnpriced
	}
	return config.Price{Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: unit, Source: source}
}

// fallbackPrice picks the price an unpriced model should borrow: the most
// expensive rate the same provider charges in the same unit family. It returns
// the winning price and the model it came from.
//
// Three rules make the substitution safe:
//
//   - Same unit family only (config.PriceUnitFamily). Units across families
//     price disjoint quantities — wire.Normalized keeps Seconds/Chars/Images
//     apart from token counts and pricing.Cost dispatches on Unit — so lending
//     a token rate to a per_second model would not over-bill, it would compute
//     $0 while claiming a rate. A speech model borrows a speech rate or none.
//   - A whole real price is copied, never a per-field maximum. A synthetic max
//     inverts the cache axis: CachedInput == 0 means "charge full Input"
//     (pricing.tokenCost), so max(CachedInput) picks the largest discount and
//     can bill cache-heavy traffic cheaper than any real model.
//   - Same provider only. Borrowing across providers would make one provider's
//     bill a function of unrelated config — adding an Anthropic provider would
//     silently re-price unknown DeepSeek models at ~57x — so a provider with no
//     usable same-family rate keeps its $0 and is warned about instead.
//
// Candidates must be plausibly scaled (config.PriceRateImplausible) so a single
// mistyped unit cannot become the ceiling for every unpriced model. Ties break
// on model name, keeping the choice stable across reloads.
func fallbackPrice(prices map[string]config.Price, forModel, unit string) (config.Price, string, bool) {
	family := config.PriceUnitFamily(unit)
	if family == "" {
		return config.Price{}, "", false
	}
	var best config.Price
	var bestModel string
	var bestRank float64
	for model, p := range prices {
		if model == forModel ||
			config.PriceUnitFamily(p.Unit) != family ||
			config.PriceMetersZero(p) ||
			config.IsFallbackPrice(p) ||
			config.PriceRateImplausible(p) {
			continue
		}
		rank := config.PriceRank(p)
		if bestModel == "" || rank > bestRank || (rank == bestRank && model < bestModel) {
			best, bestModel, bestRank = p, model, rank
		}
	}
	if bestModel == "" {
		return config.Price{}, "", false
	}
	return best, bestModel, true
}

func catalogModelPrice(cat catalog.Catalog, catalogID, model string) (config.Price, bool) {
	if catalogID == "" {
		return config.Price{}, false
	}
	for _, v := range cat.Vendors {
		if v.ID != catalogID {
			continue
		}
		m, ok := v.Models[model]
		if !ok {
			return config.Price{}, false
		}
		return config.Price{Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: m.Unit, Source: config.PriceSourceCatalog}, true
	}
	return config.Price{}, false
}

func catalogAnyModelPrice(cat catalog.Catalog, model string) (config.Price, bool) {
	for _, v := range cat.Vendors {
		m, ok := v.Models[model]
		if !ok {
			continue
		}
		return config.Price{Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: m.Unit, Source: config.PriceSourceCatalog}, true
	}
	return config.Price{}, false
}

// originOf returns the scheme://host of a (possibly {model}-templated) endpoint
// URL, dropping path and query. Empty on a parse failure — config validation
// then rejects the malformed endpoint, so a bad value can't silently route.
func originOf(raw string) string {
	u, err := url.Parse(strings.ReplaceAll(raw, "{model}", "MODEL"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// adapterRank orders endpoint groups so the primary (name-keeping) group is
// deterministic and intuitive: the OpenAI-compatible surface comes first.
func adapterRank(adapter string) int {
	switch adapter {
	case config.AdapterOpenAI:
		return 0
	case config.AdapterAnthropic:
		return 1
	case config.AdapterVolcSpeech:
		return 2
	default:
		return 3
	}
}

// adapterSlug shortens an adapter name into a vendor-name suffix used to
// disambiguate a provider's secondary endpoint groups (e.g. "deepseek-anthropic").
func adapterSlug(adapter string) string {
	switch adapter {
	case config.AdapterAnthropic:
		return "anthropic"
	case config.AdapterVolcSpeech:
		return "speech"
	default:
		return "openai"
	}
}

// emptySnapshot returns a valid empty snapshot for the degraded-startup path.
func emptySnapshot() *config.Snapshot {
	snap, _ := config.Build(config.Config{})
	return snap
}

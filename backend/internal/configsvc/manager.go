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
	proxies, err := m.store.ListProxies()
	if err != nil {
		return nil, fmt.Errorf("configsvc: list proxies: %w", err)
	}
	proxiesByID := make(map[string]config.Proxy, len(proxies))
	for _, p := range proxies {
		proxiesByID[p.ID] = config.Proxy{
			ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port,
			Username: p.Username, Password: p.Password,
		}
	}
	cat, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("configsvc: load catalog: %w", err)
	}
	// The hand-written half on its own. Load merges it into cat, which is what
	// most lookups want, but the merge is exactly what destroys the "was this
	// pinned?" signal — see priceSources.pinned.
	pinned, err := catalog.Manual()
	if err != nil {
		return nil, fmt.Errorf("configsvc: load pinned catalog: %w", err)
	}
	// A missing or unreadable feed is not fatal: the embedded catalog is the
	// floor, and metering must never depend on a refresh having happened.
	feed, err := m.store.ListFeedPrices()
	if err != nil {
		m.logger.Warn("cannot read refreshed prices; falling back to the embedded catalog", "err", err)
		feed = nil
	}
	prices := priceSources{feed: feed, catalog: cat, pinned: pinned}
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
		var outboundProxy *config.Proxy
		if pvd.ProxyID != "" {
			p, ok := proxiesByID[pvd.ProxyID]
			if !ok {
				return nil, fmt.Errorf("configsvc: provider %q references missing proxy %q", pvd.Name, pvd.ProxyID)
			}
			outboundProxy = &p
		}
		cfg.Vendors = append(cfg.Vendors, vendorsFromProvider(pvd, outboundProxy, prices, m.logger)...)
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
func vendorsFromProvider(pvd store.Provider, outboundProxy *config.Proxy, prices priceSources, logger *slog.Logger) []config.Vendor {
	// Pass 1: resolve each model's published price (catalog, or the stored row).
	models := make([]string, 0, len(pvd.Models))
	priceTable := make(map[string]config.Price, len(pvd.Models))
	modelRoutes := make(map[string]config.ModelRoute, len(pvd.Models))
	for _, m := range pvd.Models {
		models = append(models, m.Model)
		priceTable[m.Model] = effectivePrice(pvd.CatalogID, m, prices)
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
	// Give it the most expensive rate this same provider charges instead, so an
	// unpriced model errs toward over-billing and surfaces as an outlier rather
	// than vanishing. See fallbackPrice for the rules.
	//
	// The gate is provenance, NOT the number: only PriceSourceUnpriced — nobody
	// ever stated a rate — is eligible. A zero that someone published is a real
	// price meaning free (the catalog lists genuinely free tiers, and an operator
	// override of zero is a deliberate "don't bill this"), and re-pricing it at
	// the provider's ceiling would invent a charge for a model that costs nothing.
	// With axes that distinction is only visible in the provenance: an empty
	// catalog.Cost and a published all-zero cost are the same value.
	for _, m := range pvd.Models {
		p := priceTable[m.Model]
		if p.Source != config.PriceSourceUnpriced {
			continue
		}
		fb, from, ok := fallbackPrice(priceTable, m.Model)
		if !ok {
			continue
		}
		fb.Source = config.PriceSourceFallbackPrefix + from
		priceTable[m.Model] = fb
	}

	// Price-completeness warnings (non-fatal): a provider must still route when
	// part of its price table is missing or suspect. Warn, don't block.
	for _, m := range pvd.Models {
		p := priceTable[m.Model]
		switch {
		case config.PriceMetersZero(p) && p.Source == config.PriceSourceOverride:
			logger.Warn("price is explicitly overridden to zero; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model)
		case config.PriceMetersZero(p) && p.Source == config.PriceSourceCatalog:
			logger.Warn("catalog publishes this model as free; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model)
		case config.PriceMetersZero(p) && p.Source == config.PriceSourceUnpriced:
			logger.Warn("model has no published price and this provider has no rate to fall back to; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model)
		case config.PriceMetersZero(p):
			logger.Warn("price declares no rate on any axis; calls for this model will meter as $0",
				"provider", pvd.Name, "model", m.Model)
		case config.IsFallbackPrice(p):
			logger.Warn("model has no published price; metering it at this provider's most expensive rate",
				"provider", pvd.Name, "model", m.Model,
				"borrowed_from", strings.TrimPrefix(p.Source, config.PriceSourceFallbackPrefix),
				"input", p.Cost.Input, "output", p.Cost.Output)
		case config.PriceRateImplausible(p):
			logger.Warn("token rate is implausibly high; check the basis — it meters as written but cannot be borrowed as a fallback",
				"provider", pvd.Name, "model", m.Model,
				"input", p.Cost.Input, "output", p.Cost.Output)
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
			Proxy:          outboundProxy,
			Prices:         priceTable,
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

// priceSources is where a published rate can come from. Kept as separate places
// rather than one merged map, because the feed is current, the catalog is a seed
// and a pin is a deliberate override — and a reader needs to be told which a
// number came from.
type priceSources struct {
	// feed is the last successful price refresh, keyed [provider][model]. Empty
	// until one lands, which is the normal state on a fresh or offline install.
	feed map[string]map[string]store.FeedPrice
	// catalog is the embedded catalog, generated and hand-written already merged:
	// the floor, always present.
	catalog catalog.Catalog
	// pinned is the hand-written half ALONE (catalog.Manual), which is the only
	// way to tell a rate someone typed on purpose from one a sync produced. The
	// merged catalog cannot answer that question: merge() folds both into one
	// map, so by the time you are looking at it every model appears equally
	// authored. See effectivePrice for why the distinction has to survive.
	pinned catalog.Catalog
}

// effectivePrice resolves one model's rate from, in order:
//
//  1. the operator's own row, when they marked it price_override;
//  2. a rate hand-pinned in catalog.json for THIS provider's catalog id;
//  3. the price feed for THIS provider's catalog id, which is why a stale rate
//     self-corrects;
//  4. the embedded catalog for THIS provider's catalog id — the offline floor
//     and the first-boot seed;
//  5. the feed under ANY provider, then the catalog under any provider, for a
//     row that names no catalog id;
//  6. the operator's row as-is, else unpriced (pass 2 may lend it a fallback).
//
// Step 5 is why most operators get a current rate at all. A provider added from
// the "Custom" tile carries no catalog id, and until the feed grew an
// any-provider step those rows could reach only the EMBEDDED catalog — so the
// daily refresh, which is the entire point of internal/pricefeed, did nothing
// for them. On the deployment this was found in, that was 11 of 13 providers,
// billing a build-time snapshot while the correct number sat unread in the
// feed_prices table.
//
// # A pin is stated, not merely left over — so it is checked FIRST
//
// modelsdev.Generate skips every model catalog.json prices, so a pin works by
// REMOVING the model from its own provider's feed. That is elegant while the
// feed is in step with the catalogue and quietly wrong the moment it is not: a
// feed_prices row written BEFORE the pin existed is still sitting in the store
// on the deploy that introduces the pin, and a lookup that consults the feed
// first will find it. The pin then does nothing, at the old rate, with the new
// number visible in catalog.json looking authoritative.
//
// That is not a startup race that settles on its own, either. The refresh that
// would clear the row only walks the models it just generated, so a model that
// LEFT the set is invisible to it (pricefeed counts removals for exactly this
// reason). And if the fetch fails — no network, models.dev down — the stale rows
// survive untouched and the pin never applies at all.
//
// So the pin is not inferred from an absence; it is read from the hand-written
// file directly. src.pinned is catalog.Manual, which is the only source that can
// distinguish "someone typed this" from "a sync produced this".
//
// # Exact beats a guess
//
// Both any-provider steps are GUESSES: they match on a model id alone, with
// nothing to say which vendor's list the answer should come from. So they run
// after every lookup that had a catalog id to narrow with — an exact match on
// the provider the operator actually named is an answer, and no guess should
// outrank it.
//
// This used to be the other way round, and pinning was again what broke: with
// the model gone from its own provider's feed, the any-provider step went
// looking for the id across every other list. models.dev's "azure" list carries
// deepseek-v4-flash at 0.19/0.51 and deepseek-v4-pro at Azure's markup;
// azure-openai sorts last (see rehosts) but last still wins when every maker
// ahead of it misses. Both defects had the same shape — pinning a rate made it
// MORE likely to bill at somebody else's number.
//
// Nothing here changes for an unpinned model: it is not in src.pinned, so step 3
// still hits first and the feed still beats the embedded catalog. Nor does a pin
// lose anything by winning early — Generate never emits a pinned model, so the
// feed can hold nothing fresher for it than the row the pin replaced.
// TestCatalogPinBeatsRehostFeed and TestPinBeatsAStaleFeedRow are the guards.
func effectivePrice(catalogID string, m store.ProviderModel, src priceSources) config.Price {
	if !m.PriceOverride {
		if p, ok := catalogModelPrice(src.pinned, catalogID, m.Model); ok {
			return p
		}
		if p, ok := feedPrice(src.feed, catalogID, m.Model); ok {
			return p
		}
		if p, ok := catalogModelPrice(src.catalog, catalogID, m.Model); ok {
			return p
		}
		if p, ok := feedAnyModelPrice(src.feed, m.Model); ok {
			return p
		}
		if p, ok := catalogAnyModelPrice(src.catalog, m.Model); ok {
			return p
		}
	}
	source := config.PriceSourceStored
	switch {
	case m.PriceOverride:
		source = config.PriceSourceOverride
	case m.Cost.Zero():
		// No catalog entry and no operator-entered rate: nothing priced this
		// model. Pass 2 in vendorsFromProvider may replace it with a fallback.
		source = config.PriceSourceUnpriced
	}
	return config.Price{Cost: m.Cost, Source: source}
}

// fallbackPrice picks the price an unpriced model should borrow: the most
// expensive rate the same provider charges. It returns the winning price and the
// model it came from.
//
// Three rules make the substitution safe:
//
//   - A whole real cost is copied, never a per-axis maximum. A synthetic max
//     inverts the cache axis: CacheRead == 0 means "charge full Input"
//     (pricing.tokenCost), so max(CacheRead) picks the largest discount and can
//     bill cache-heavy traffic cheaper than any real model.
//   - Same provider only. Borrowing across providers would make one provider's
//     bill a function of unrelated config — adding an Anthropic provider would
//     silently re-price unknown DeepSeek models at ~57x — so a provider with no
//     usable rate keeps its $0 and is warned about instead.
//   - Candidates must be plausibly scaled (config.PriceRateImplausible), so a
//     single mistyped rate cannot become the ceiling for every unpriced model.
//
// Candidates are restricted to TOKEN-priced models, and that restriction
// replaces the old "same unit family" rule. The reason is that the axis an
// unpriced model should be priced on is no longer knowable: a row with no rate
// declares no axes, and the retired `unit` column was the only thing that ever
// said "this one bills per second". Lending a media rate on a guess would state
// a rate for a quantity we have no evidence about.
//
// Token-only is the right guess because unpriced implies absent from the
// catalog (effectivePrice consults it first), and a model that is new enough to
// be missing is a chat model in practice — media wires have small, fixed model
// sets that are hand-maintained in catalog.json. The capability traded away is
// real but narrow: an unpriced speech model no longer borrows its provider's
// priciest per-second rate, and instead keeps $0 and is warned about, which is
// the branch the old rule already took whenever no same-family sibling existed.
//
// Ties break on model name, keeping the choice stable across reloads.
func fallbackPrice(prices map[string]config.Price, forModel string) (config.Price, string, bool) {
	var best config.Price
	var bestModel string
	var bestRank float64
	for model, p := range prices {
		if model == forModel ||
			!p.Cost.Tokens() ||
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

// rehosts are catalog providers that resell models another vendor makes, at
// their own markup. They are ranked LAST when a model id has to be matched
// without a catalog id to narrow it (see anyProviderOrder).
//
// Membership is about the price list, not the company: azure-openai is here
// because models.dev's "azure" list carries 87 models — Anthropic's, DeepSeek's,
// xAI's, Moonshot's — at Azure's rates, and dashscope because Alibaba re-hosts
// Zhipu's GLM line alongside its own Qwen models. Both are still the RIGHT
// source for a provider that names them, which is why they are demoted rather
// than excluded.
var rehosts = map[string]bool{"azure-openai": true, "dashscope": true}

// anyProviderOrder is the order providers are consulted when a model id must be
// resolved without a catalog id: makers first, re-hosts last, alphabetical
// within each group so a reload never changes the answer.
//
// The ordering is a correctness fix, not a tidiness one. Plain alphabetical
// order put azure-openai ahead of deepseek and xai, and once the generator began
// taking each provider's whole published list that became actively wrong:
//
//	deepseek-v4-pro    azure-openai 1.74/3.48   vs deepseek 0.435/0.87  (4x over)
//	deepseek-v4-flash  azure-openai 0.19/0.51   vs deepseek 0.14/0.28
//	grok-4.6           azure-openai NO COST     vs xai      2/6         (meters $0)
//
// The last row is why the two any-provider lookups also skip a candidate whose
// cost meters zero: a provider can list a model it publishes no rate for, and
// letting that win would convert a real rate into free — the same silent zero
// this change exists to remove.
//
// That skip is deliberately NOT applied when a provider names its catalog id.
// There a published zero is a real price meaning free — the catalog carries
// genuinely free tiers — and the rest of this file already depends on telling
// that apart from "nobody stated a rate" (see the provenance gate in
// vendorsFromProvider's pass 2). The difference is that an exact match is an
// answer and an any-provider match is a guess, and only a guess should decline
// to believe a zero.
func anyProviderOrder(ids []string) []string {
	sort.SliceStable(ids, func(i, j int) bool {
		if rehosts[ids[i]] != rehosts[ids[j]] {
			return !rehosts[ids[i]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

// feedPrice reads the last successful refresh for a provider that names its
// catalog id. Model ids are matched canonically (catalog.LookupIDs), so an
// operator who typed claude-fable-5.1 still resolves the feed's
// claude-fable-5-1.
func feedPrice(feed map[string]map[string]store.FeedPrice, catalogID, model string) (config.Price, bool) {
	if catalogID == "" || feed == nil {
		return config.Price{}, false
	}
	return feedPriceFrom(feed[catalogID], model)
}

// feedAnyModelPrice matches a model id under any provider in the feed — the
// feed's counterpart to catalogAnyModelPrice, for the provider rows that carry
// no catalog id. Without it those rows never see a refreshed rate at all.
func feedAnyModelPrice(feed map[string]map[string]store.FeedPrice, model string) (config.Price, bool) {
	for _, id := range anyProviderOrder(config.SortedKeys(feed)) {
		if p, ok := feedPriceFrom(feed[id], model); ok && !config.PriceMetersZero(p) {
			return p, true
		}
	}
	return config.Price{}, false
}

func feedPriceFrom(models map[string]store.FeedPrice, model string) (config.Price, bool) {
	index := make(map[string]store.FeedPrice, len(models))
	for k, v := range models {
		index[catalog.CanonicalID(k)] = v
	}
	for _, id := range catalog.LookupIDs(model) {
		if fp, ok := index[id]; ok {
			return config.Price{Cost: fp.Cost, Source: config.PriceSourceFeed}, true
		}
	}
	return config.Price{}, false
}

func catalogModelPrice(cat catalog.Catalog, catalogID, model string) (config.Price, bool) {
	if catalogID == "" {
		return config.Price{}, false
	}
	p, ok := cat[catalogID]
	if !ok {
		return config.Price{}, false
	}
	return catalogPriceFrom(p.Models, model)
}

// catalogAnyModelPrice matches a model id under any provider, for a provider row
// that carries no catalog_id. See anyProviderOrder for why the order matters.
func catalogAnyModelPrice(cat catalog.Catalog, model string) (config.Price, bool) {
	for _, id := range anyProviderOrder(config.SortedKeys(cat)) {
		if p, ok := catalogPriceFrom(cat[id].Models, model); ok && !config.PriceMetersZero(p) {
			return p, true
		}
	}
	return config.Price{}, false
}

func catalogPriceFrom(models map[string]catalog.Model, model string) (config.Price, bool) {
	index := make(map[string]catalog.Model, len(models))
	for k, v := range models {
		index[catalog.CanonicalID(k)] = v
	}
	for _, id := range catalog.LookupIDs(model) {
		if m, ok := index[id]; ok {
			return config.Price{Cost: m.Cost, Source: config.PriceSourceCatalog}, true
		}
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

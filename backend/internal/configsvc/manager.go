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
	as, err := m.store.GetAppSettings()
	if err != nil {
		return nil, fmt.Errorf("configsvc: get settings: %w", err)
	}

	cfg := config.Config{
		Settings: config.Settings{
			Capture: as.Capture,
		},
	}
	for _, pvd := range providers {
		if !pvd.Enabled {
			continue
		}
		if pvd.APIKey == "" || len(pvd.Models) == 0 {
			m.logger.Warn("skipping incomplete provider (no API key or models)",
				"provider", pvd.Name, "has_key", pvd.APIKey != "", "models", len(pvd.Models))
			continue
		}
		cfg.Vendors = append(cfg.Vendors, vendorsFromProvider(pvd, m.logger)...)
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
func vendorsFromProvider(pvd store.Provider, logger *slog.Logger) []config.Vendor {
	models := make([]string, 0, len(pvd.Models))
	prices := make(map[string]config.Price, len(pvd.Models))
	for _, m := range pvd.Models {
		models = append(models, m.Model)
		unit := m.Unit
		if unit == "" {
			unit = "per_1m_tokens"
		}
		prices[m.Model] = config.Price{Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: unit}
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
			Credential:     config.Credential{ID: pvd.ID, APIKey: pvd.APIKey},
			Prices:         prices,
			Wires:          wires,
			Endpoints:      endpoints,
			AllowUnmatched: pvd.AllowUnmatched,
			Quirks:         pvd.Quirks,
		})
	}
	return vendors
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

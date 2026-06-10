// Package configsvc builds the live routing Snapshot from SQLite-backed service
// rows, replacing the file-based config.Manager as the source of truth.
//
// It holds an atomic *config.Snapshot rebuilt on demand (Reload) after any
// dashboard write. The router, proxy, and admin API consume it through the same
// Current func() *config.Snapshot signature the file manager exposed, so the
// rest of the gateway is unchanged — only the snapshot's source moved from a
// YAML file to the database.
//
// Robustness: a single incomplete service (no credentials, no models, or
// disabled) is skipped rather than allowed to fail the whole snapshot build, so
// a half-configured service can never take routing down.
package configsvc

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// DefaultWires is the wire allowlist granted to a service when none is given
// explicitly, keyed by the service's adapter (auth scheme). Catalog presets
// override this with precise per-service lists.
func DefaultWires(adapter string) []string {
	if adapter == config.AdapterAnthropic {
		return []string{"anthropic/messages", "anthropic/models"}
	}
	return []string{"openai/chat", "openai/completions", "openai/embeddings", "openai/models"}
}

// Manager owns the live snapshot derived from the store.
type Manager struct {
	store   *store.Store
	logger  *slog.Logger
	current atomic.Pointer[config.Snapshot]
}

// NewManager builds the initial snapshot from the store and returns a ready
// Manager. A build error at startup is non-fatal: it logs and starts empty so
// the gateway still serves (an admin can then fix the offending service).
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
// Snapshot. Incomplete/disabled services are skipped with a warning.
func (m *Manager) build() (*config.Snapshot, error) {
	services, err := m.store.ListServices()
	if err != nil {
		return nil, fmt.Errorf("configsvc: list services: %w", err)
	}
	as, err := m.store.GetAppSettings()
	if err != nil {
		return nil, fmt.Errorf("configsvc: get settings: %w", err)
	}

	cfg := config.Config{
		Settings: config.Settings{
			Capture:         as.Capture,
			CaptureMaxBytes: as.CaptureMaxBytes,
			CaptureRetain:   as.CaptureRetain,
		},
	}
	for _, svc := range services {
		if !svc.Enabled {
			continue
		}
		if svc.APIKey == "" || len(svc.Models) == 0 {
			m.logger.Warn("skipping incomplete service (no API key or models)",
				"service", svc.Name, "has_key", svc.APIKey != "", "models", len(svc.Models))
			continue
		}
		cfg.Vendors = append(cfg.Vendors, vendorFromService(svc, m.logger))
	}

	return config.Build(cfg)
}

// vendorFromService projects a stored service into a config.Vendor for routing.
// Wire names not present in the registry are dropped with a warning so a typo
// in the allowlist can never silently match traffic.
func vendorFromService(svc store.Service, logger *slog.Logger) config.Vendor {
	models := make([]string, 0, len(svc.Models))
	prices := make(map[string]config.Price, len(svc.Models))
	for _, m := range svc.Models {
		models = append(models, m.Model)
		unit := m.Unit
		if unit == "" {
			unit = "per_1m_tokens"
		}
		prices[m.Model] = config.Price{Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: unit}
	}

	wires := make([]string, 0, len(svc.Wires))
	for _, w := range svc.Wires {
		if _, ok := wire.Get(w); !ok {
			logger.Warn("dropping unknown wire from service allowlist", "service", svc.Name, "wire", w)
			continue
		}
		wires = append(wires, w)
	}

	return config.Vendor{
		Name:           svc.Name,
		BaseURL:        svc.BaseURL,
		Adapter:        svc.Adapter,
		ServedModels:   models,
		Priority:       svc.Priority,
		Weight:         svc.Weight,
		Credential:     config.Credential{ID: svc.ID, APIKey: svc.APIKey},
		Prices:         prices,
		Wires:          wires,
		AllowUnmatched: svc.AllowUnmatched,
		Quirks:         svc.Quirks,
	}
}

// emptySnapshot returns a valid empty snapshot for the degraded-startup path.
func emptySnapshot() *config.Snapshot {
	snap, _ := config.Build(config.Config{})
	return snap
}

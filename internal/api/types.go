package api

import (
	"time"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/ledger"
	"github.com/songguo/songguo/internal/store"
)

// tokenView is the JSON representation of a token, including computed lifetime
// spend and active state. It never exposes the key hash or plaintext key.
type tokenView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	Budget    *float64 `json:"budget"`
	Scope     []string `json:"scope"`
	RPM       int      `json:"rpm"`
	CreatedAt string   `json:"created_at"`
	RevokedAt *string  `json:"revoked_at"`
	Spent     float64  `json:"spent"`
	Active    bool     `json:"active"`
	// Key carries the plaintext key, set ONLY on creation. Omitted otherwise.
	Key string `json:"key,omitempty"`
}

// newTokenView converts a store.Token plus its lifetime spend into a view.
func newTokenView(t store.Token, spent float64) tokenView {
	scope := t.Scope
	if scope == nil {
		scope = []string{}
	}
	v := tokenView{
		ID:        t.ID,
		Name:      t.Name,
		KeyPrefix: t.KeyPrefix,
		Budget:    t.Budget,
		Scope:     scope,
		RPM:       t.RPM,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
		Spent:     spent,
		Active:    t.RevokedAt == nil,
	}
	if t.RevokedAt != nil {
		s := t.RevokedAt.UTC().Format(time.RFC3339)
		v.RevokedAt = &s
	}
	return v
}

// entryView is the JSON representation of a ledger entry.
type entryView struct {
	ID           int64             `json:"id"`
	TS           string            `json:"ts"`
	TokenID      string            `json:"token_id"`
	Model        string            `json:"model"`
	Modality     string            `json:"modality"`
	Vendor       string            `json:"vendor"`
	CredentialID string            `json:"credential_id"`
	Attempt      int               `json:"attempt"`
	Status       int               `json:"status"`
	Err          string            `json:"err"`
	Usage        map[string]any    `json:"usage"`
	Cost         float64           `json:"cost"`
	LatencyMS    int64             `json:"latency_ms"`
	Stream       bool              `json:"stream"`
	Tags         map[string]string `json:"tags"`
}

// newEntryView converts a ledger.Entry into its JSON view.
func newEntryView(e ledger.Entry) entryView {
	usage := e.Usage
	if usage == nil {
		usage = map[string]any{}
	}
	tags := e.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	return entryView{
		ID:           e.ID,
		TS:           e.TS.UTC().Format(time.RFC3339),
		TokenID:      e.TokenID,
		Model:        e.Model,
		Modality:     string(e.Modality),
		Vendor:       e.Vendor,
		CredentialID: e.CredentialID,
		Attempt:      e.Attempt,
		Status:       e.Status,
		Err:          e.Err,
		Usage:        usage,
		Cost:         e.Cost,
		LatencyMS:    e.LatencyMS,
		Stream:       e.Stream,
		Tags:         tags,
	}
}

// rangeView reports the resolved [since, until) window as unix seconds.
type rangeView struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
}

// latencyView holds latency percentiles in milliseconds.
type latencyView struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
}

// overviewView is the GET /api/overview response.
type overviewView struct {
	Range           rangeView          `json:"range"`
	TotalSpend      float64            `json:"total_spend"`
	SpendByModality map[string]float64 `json:"spend_by_modality"`
	Requests        int                `json:"requests"`
	Errors          int                `json:"errors"`
	ErrorRate       float64            `json:"error_rate"`
	LatencyMS       latencyView        `json:"latency_ms"`
	VendorsActive   int                `json:"vendors_active"`
	TokensActive    int                `json:"tokens_active"`
	DailyBurn       float64            `json:"daily_burn"`
	RunwayDays      *float64           `json:"runway_days"`
}

// ledgerView is the GET /api/ledger response.
type ledgerView struct {
	Entries []entryView `json:"entries"`
	Total   int         `json:"total"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
}

// credentialView is a credential with its key masked. The raw key is NEVER
// included.
type credentialView struct {
	ID        string `json:"id"`
	MaskedKey string `json:"masked_key"`
}

// priceView is a single model price.
type priceView struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
}

// vendorStatsView is the per-vendor health/usage summary.
type vendorStatsView struct {
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	LastStatus   int     `json:"last_status"`
	Healthy      bool    `json:"healthy"`
}

// vendorView is the JSON representation of a vendor (without secrets).
type vendorView struct {
	Name         string               `json:"name"`
	BaseURL      string               `json:"base_url"`
	ServedModels []string             `json:"served_models"`
	Priority     int                  `json:"priority"`
	Weight       int                  `json:"weight"`
	Credentials  []credentialView     `json:"credentials"`
	Prices       map[string]priceView `json:"prices"`
	Stats        vendorStatsView      `json:"stats"`
}

// newVendorView builds a vendor view from config plus computed stats. The raw
// api_key is intentionally dropped; only masked previews are emitted.
func newVendorView(v config.Vendor, stat store.VendorStat, hasStat bool) vendorView {
	models := v.ServedModels
	if models == nil {
		models = []string{}
	}

	creds := make([]credentialView, 0, len(v.Credentials))
	for _, c := range v.Credentials {
		creds = append(creds, credentialView{ID: c.ID, MaskedKey: maskKey(c.APIKey)})
	}

	prices := make(map[string]priceView, len(v.Prices))
	for model, p := range v.Prices {
		prices[model] = priceView{Input: p.Input, Output: p.Output, Unit: p.Unit}
	}

	sv := vendorStatsView{Healthy: true} // no traffic => healthy.
	if hasStat {
		sv.Requests = stat.Requests
		sv.Errors = stat.Errors
		sv.AvgLatencyMS = stat.AvgLatency
		sv.LastStatus = stat.LastStatus
		if stat.Requests > 0 {
			sv.ErrorRate = float64(stat.Errors) / float64(stat.Requests)
		}
		sv.Healthy = stat.Errors == 0
	}

	return vendorView{
		Name:         v.Name,
		BaseURL:      v.BaseURL,
		ServedModels: models,
		Priority:     v.Priority,
		Weight:       v.Weight,
		Credentials:  creds,
		Prices:       prices,
		Stats:        sv,
	}
}

// testVendorView is the POST /api/vendors/{name}/test response.
type testVendorView struct {
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// settingsView is the GET /api/settings response. It never exposes the admin
// key.
type settingsView struct {
	Listen         string `json:"listen"`
	ConfigPath     string `json:"config_path"`
	DBPath         string `json:"db_path"`
	AdminProtected bool   `json:"admin_protected"`
	Version        string `json:"version"`
	WatchMode      string `json:"watch_mode,omitempty"`
}

// pricingRow is one flattened pricing entry for GET /api/pricing.
type pricingRow struct {
	Vendor string  `json:"vendor"`
	Model  string  `json:"model"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
}

// maskKey returns a masked preview of an API key: first 3 + "…" + last 2 chars,
// or "••••" if the key is too short to mask meaningfully. It never returns the
// raw key.
func maskKey(key string) string {
	const ellipsis = "…"
	if len(key) < 6 {
		return "••••"
	}
	return key[:3] + ellipsis + key[len(key)-2:]
}

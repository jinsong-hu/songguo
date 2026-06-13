package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// --- views ---

type providerModelView struct {
	Model       string  `json:"model"`
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input"`
	Unit        string  `json:"unit"`
}

// providerEndpointView is one wire bound to a base URL + adapter (auth scheme).
type providerEndpointView struct {
	Wire    string `json:"wire"`
	BaseURL string `json:"base_url"`
	Adapter string `json:"adapter"`
}

// providerView is the JSON representation of a configured provider. The API key
// is never serialized in the clear — only a masked preview.
type providerView struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Vendor         string                 `json:"vendor"`
	Priority       int                    `json:"priority"`
	Weight         int                    `json:"weight"`
	Enabled        bool                   `json:"enabled"`
	CatalogID      string                 `json:"catalog_id"`
	Endpoints      []providerEndpointView `json:"endpoints"`
	AllowUnmatched bool                   `json:"allow_unmatched"`
	Quirks         map[string]string      `json:"quirks"`
	MaskedKey      string                 `json:"masked_key"`
	Models         []providerModelView    `json:"models"`
	CreatedAt      string                 `json:"created_at"`
	UpdatedAt      string                 `json:"updated_at"`
	Stats          vendorStatsView        `json:"stats"`
}

func newProviderView(pvd store.Provider, stat store.VendorStat, hasStat bool) providerView {
	masked := ""
	if pvd.APIKey != "" {
		masked = maskKey(pvd.APIKey)
	}
	models := make([]providerModelView, 0, len(pvd.Models))
	for _, m := range pvd.Models {
		models = append(models, providerModelView{Model: m.Model, Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: m.Unit})
	}
	endpoints := make([]providerEndpointView, 0, len(pvd.Endpoints))
	for _, ep := range pvd.Endpoints {
		endpoints = append(endpoints, providerEndpointView{Wire: ep.Wire, BaseURL: ep.BaseURL, Adapter: ep.Adapter})
	}

	sv := vendorStatsView{Healthy: true}
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

	return providerView{
		ID:             pvd.ID,
		Name:           pvd.Name,
		Vendor:         pvd.Vendor,
		Priority:       pvd.Priority,
		Weight:         pvd.Weight,
		Enabled:        pvd.Enabled,
		CatalogID:      pvd.CatalogID,
		Endpoints:      endpoints,
		AllowUnmatched: pvd.AllowUnmatched,
		Quirks:         pvd.Quirks,
		MaskedKey:      masked,
		Models:         models,
		CreatedAt:      pvd.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      pvd.UpdatedAt.UTC().Format(time.RFC3339),
		Stats:          sv,
	}
}

// --- request bodies ---

type providerModelReq struct {
	Model       string  `json:"model"`
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input"`
	Unit        string  `json:"unit"`
}

type providerEndpointReq struct {
	Wire    string `json:"wire"`
	BaseURL string `json:"base_url"`
	Adapter string `json:"adapter"`
}

type createProviderReq struct {
	Name           string                `json:"name"`
	Vendor         string                `json:"vendor"`
	Priority       int                   `json:"priority"`
	Weight         int                   `json:"weight"`
	Enabled        *bool                 `json:"enabled"`
	CatalogID      string                `json:"catalog_id"`
	AllowUnmatched bool                  `json:"allow_unmatched"`
	Quirks         map[string]string     `json:"quirks"`
	APIKey         string                `json:"api_key"`
	Models         []providerModelReq    `json:"models"`
	Endpoints      []providerEndpointReq `json:"endpoints"`
}

type patchProviderReq struct {
	Name           *string           `json:"name"`
	Vendor         *string           `json:"vendor"`
	Priority       *int              `json:"priority"`
	Weight         *int              `json:"weight"`
	Enabled        *bool             `json:"enabled"`
	AllowUnmatched *bool             `json:"allow_unmatched"`
	// APIKey replaces the provider's key when present and non-empty.
	APIKey    *string                `json:"api_key"`
	Quirks    *map[string]string     `json:"quirks"`
	Models    *[]providerModelReq    `json:"models"`
	Endpoints *[]providerEndpointReq `json:"endpoints"`
}

// --- handlers ---

// handleListProviders returns all configured providers (keys masked) with stats.
func (a *api) handleListProviders(w http.ResponseWriter, r *http.Request) {
	pvds, err := a.store.ListProviders()
	if err != nil {
		a.serverError(w, "list providers", err)
		return
	}
	stats, err := a.store.VendorStats(nil, nil)
	if err != nil {
		a.serverError(w, "vendor stats", err)
		return
	}
	views := make([]providerView, 0, len(pvds))
	for _, pvd := range pvds {
		st, ok := stats[pvd.Name]
		views = append(views, newProviderView(pvd, st, ok))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleGetProvider returns one provider.
func (a *api) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	pvd, err := a.store.GetProvider(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		a.serverError(w, "get provider", err)
		return
	}
	writeJSON(w, http.StatusOK, newProviderView(pvd, store.VendorStat{}, false))
}

// handleCreateProvider creates a provider from a JSON body and reloads the config.
func (a *api) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req createProviderReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	endpoints, msg := toStoreEndpoints(req.Endpoints)
	if msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	pvd, err := a.store.CreateProvider(store.NewProvider{
		Name:           strings.TrimSpace(req.Name),
		Vendor:         req.Vendor,
		Priority:       req.Priority,
		Weight:         req.Weight,
		Enabled:        enabled,
		CatalogID:      req.CatalogID,
		AllowUnmatched: req.AllowUnmatched,
		Quirks:         req.Quirks,
		APIKey:         strings.TrimSpace(req.APIKey),
		Models:         toStoreModels(req.Models),
		Endpoints:      endpoints,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "conflict", "a provider with that name already exists")
			return
		}
		a.serverError(w, "create provider", err)
		return
	}
	a.reloadAfterWrite()
	writeJSON(w, http.StatusCreated, newProviderView(pvd, store.VendorStat{}, false))
}

// handlePatchProvider applies a subset of fields and reloads the config.
func (a *api) handlePatchProvider(w http.ResponseWriter, r *http.Request) {
	var req patchProviderReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.APIKey != nil {
		trimmed := strings.TrimSpace(*req.APIKey)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "api_key cannot be empty")
			return
		}
		req.APIKey = &trimmed
	}
	upd := store.ProviderUpdate{
		Name:           req.Name,
		Vendor:         req.Vendor,
		Priority:       req.Priority,
		Weight:         req.Weight,
		Enabled:        req.Enabled,
		AllowUnmatched: req.AllowUnmatched,
		APIKey:         req.APIKey,
		Quirks:         req.Quirks,
	}
	if req.Models != nil {
		upd.Models = toStoreModels(*req.Models)
		if upd.Models == nil {
			upd.Models = []store.ProviderModel{} // explicit clear
		}
	}
	if req.Endpoints != nil {
		eps, msg := toStoreEndpoints(*req.Endpoints)
		if msg != "" {
			writeError(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
		upd.Endpoints = eps
		if upd.Endpoints == nil {
			upd.Endpoints = []store.ProviderEndpoint{} // explicit clear
		}
	}

	pvd, err := a.store.UpdateProvider(r.PathValue("id"), upd)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "conflict", "a provider with that name already exists")
			return
		}
		a.serverError(w, "update provider", err)
		return
	}
	a.reloadAfterWrite()
	writeJSON(w, http.StatusOK, newProviderView(pvd, store.VendorStat{}, false))
}

// handleDeleteProvider removes a provider and reloads the config.
func (a *api) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteProvider(r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		a.serverError(w, "delete provider", err)
		return
	}
	a.reloadAfterWrite()
	w.WriteHeader(http.StatusNoContent)
}

// handleTestProvider probes a configured provider's host origin for reachability,
// authenticating with its API key.
func (a *api) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	pvd, err := a.store.GetProvider(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "provider not found")
			return
		}
		a.serverError(w, "get provider", err)
		return
	}

	if len(pvd.Endpoints) == 0 {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: "provider has no endpoints"})
		return
	}
	ep := pvd.Endpoints[0]
	origin, err := originOf(ep.BaseURL)
	if err != nil {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: err.Error()})
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: err.Error()})
		return
	}
	if pvd.APIKey != "" {
		applyTestAuth(req, ep.Adapter, pvd.APIKey)
	}

	start := a.now()
	resp, err := a.client.Do(req)
	latency := a.now().Sub(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, LatencyMS: latency, Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	drain(resp.Body)

	writeJSON(w, http.StatusOK, testVendorView{Reachable: true, Status: resp.StatusCode, LatencyMS: latency})
}

// handleCatalog returns the embedded preset directory.
func (a *api) handleCatalog(w http.ResponseWriter, r *http.Request) {
	c, err := catalog.Load()
	if err != nil {
		a.serverError(w, "load catalog", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleWires returns all registered wire names, for the provider form's
// allowlist picker.
func (a *api) handleWires(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, wire.Names())
}

type patchSettingsReq struct {
	Capture         *bool `json:"capture"`
	CaptureMaxBytes *int  `json:"capture_max_bytes"`
	CaptureRetain   *int  `json:"capture_retain"`
}

// handlePatchSettings updates the gateway settings singleton and reloads.
func (a *api) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req patchSettingsReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	cur, err := a.store.GetAppSettings()
	if err != nil {
		a.serverError(w, "get settings", err)
		return
	}
	if req.Capture != nil {
		cur.Capture = *req.Capture
	}
	if req.CaptureMaxBytes != nil && *req.CaptureMaxBytes > 0 {
		cur.CaptureMaxBytes = *req.CaptureMaxBytes
	}
	if req.CaptureRetain != nil && *req.CaptureRetain > 0 {
		cur.CaptureRetain = *req.CaptureRetain
	}
	if err := a.store.UpdateAppSettings(cur); err != nil {
		a.serverError(w, "update settings", err)
		return
	}
	a.reloadAfterWrite()
	a.handleSettings(w, r)
}

// --- helpers ---

// reloadAfterWrite rebuilds the live snapshot after a config change, logging
// (never surfacing) a build failure — the write already succeeded.
func (a *api) reloadAfterWrite() {
	if err := a.reload(); err != nil {
		a.logger.Error("config reload after write failed", "err", err)
	}
}

// toStoreModels converts request models into store models, dropping empties.
func toStoreModels(in []providerModelReq) []store.ProviderModel {
	if in == nil {
		return nil
	}
	out := make([]store.ProviderModel, 0, len(in))
	for _, m := range in {
		if strings.TrimSpace(m.Model) == "" {
			continue
		}
		unit := m.Unit
		if unit == "" {
			unit = "per_1m_tokens"
		}
		out = append(out, store.ProviderModel{Model: m.Model, Input: m.Input, Output: m.Output, CachedInput: m.CachedInput, Unit: unit})
	}
	return out
}

// toStoreEndpoints converts request endpoints into store endpoints, validating
// each base URL. It returns ("", problem) on the first invalid base URL.
func toStoreEndpoints(in []providerEndpointReq) ([]store.ProviderEndpoint, string) {
	if in == nil {
		return nil, ""
	}
	out := make([]store.ProviderEndpoint, 0, len(in))
	for _, ep := range in {
		if strings.TrimSpace(ep.Wire) == "" {
			continue
		}
		if msg := validateBaseURL(ep.BaseURL); msg != "" {
			return nil, msg
		}
		adapter := ep.Adapter
		if adapter == "" {
			adapter = "openai-compatible"
		}
		out = append(out, store.ProviderEndpoint{Wire: ep.Wire, BaseURL: strings.TrimSpace(ep.BaseURL), Adapter: adapter})
	}
	return out, ""
}

// validateBaseURL returns "" when base is a valid absolute http(s) URL with a
// host, else a human-readable problem message.
func validateBaseURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return "base_url is required"
	}
	u, err := url.Parse(base)
	if err != nil {
		return "base_url is not a valid URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "base_url must be an absolute http or https URL"
	}
	if u.Host == "" {
		return "base_url must include a host"
	}
	return ""
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure
// (e.g. a duplicate provider name).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// applyTestAuth sets the credential header for a connectivity probe using the
// adapter's convention, mirroring the proxy's auth handling.
func applyTestAuth(req *http.Request, adapter, key string) {
	if adapter == "anthropic-compatible" {
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		return
	}
	if adapter == "volc-speech" {
		req.Header.Set("X-Api-Key", key)
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

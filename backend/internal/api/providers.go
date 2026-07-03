package api

import (
	"context"
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

// providerEndpointView is one wire bound to its full upstream URL + adapter (auth scheme).
type providerEndpointView struct {
	Wire     string `json:"wire"`
	Endpoint string `json:"endpoint"`
	Adapter  string `json:"adapter"`
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
		endpoints = append(endpoints, providerEndpointView{Wire: ep.Wire, Endpoint: ep.Endpoint, Adapter: ep.Adapter})
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
	Input       float64 `json:"input,omitempty"`
	Output      float64 `json:"output,omitempty"`
	CachedInput float64 `json:"cached_input,omitempty"`
	Unit        string  `json:"unit,omitempty"`
}

type providerEndpointReq struct {
	Wire     string `json:"wire"`
	Endpoint string `json:"endpoint"`
	Adapter  string `json:"adapter,omitempty"`
}

type createProviderReq struct {
	Name           string                `json:"name"`
	Vendor         string                `json:"vendor,omitempty"`
	Priority       int                   `json:"priority,omitempty"`
	Weight         int                   `json:"weight,omitempty"`
	Enabled        *bool                 `json:"enabled,omitempty"`
	CatalogID      string                `json:"catalog_id,omitempty"`
	AllowUnmatched bool                  `json:"allow_unmatched,omitempty"`
	Quirks         map[string]string     `json:"quirks,omitempty"`
	APIKey         string                `json:"api_key,omitempty"`
	Models         []providerModelReq    `json:"models,omitempty"`
	Endpoints      []providerEndpointReq `json:"endpoints,omitempty"`
}

type patchProviderReq struct {
	Name           *string `json:"name,omitempty"`
	Vendor         *string `json:"vendor,omitempty"`
	Priority       *int    `json:"priority,omitempty"`
	Weight         *int    `json:"weight,omitempty"`
	Enabled        *bool   `json:"enabled,omitempty"`
	AllowUnmatched *bool   `json:"allow_unmatched,omitempty"`
	// APIKey replaces the provider's key when present and non-empty.
	APIKey    *string                `json:"api_key,omitempty"`
	Quirks    *map[string]string     `json:"quirks,omitempty"`
	Models    *[]providerModelReq    `json:"models,omitempty"`
	Endpoints *[]providerEndpointReq `json:"endpoints,omitempty"`
}

// --- handlers ---

// handleListProviders returns all configured providers (keys masked) with stats.
// A consumer key gets a sanitized view: identity and wire/model shape only, with
// upstream endpoint URLs, key preview, pricing, quirks and stats stripped.
func (a *api) handleListProviders(w http.ResponseWriter, r *http.Request) {
	views, err := a.providersData()
	if err != nil {
		a.writeDataErr(w, "list providers", err)
		return
	}
	if roleFrom(r) == roleUser {
		views = sanitizeProvidersForUser(views)
	}
	writeJSON(w, http.StatusOK, views)
}

// sanitizeProvidersForUser strips operator-only fields from provider views before
// exposing them to a consumer key. It keeps the identity plus the per-wire
// (wire/adapter) and model-name shape the playground needs to pin a provider,
// but drops the upstream endpoint URLs, the masked key, per-provider pricing,
// quirks, and aggregate call stats. The JSON shape is unchanged so the SPA reads
// it with the same Provider type.
func sanitizeProvidersForUser(in []providerView) []providerView {
	out := make([]providerView, 0, len(in))
	for _, p := range in {
		p.MaskedKey = ""
		p.Quirks = nil
		p.Stats = vendorStatsView{Healthy: true}
		eps := make([]providerEndpointView, 0, len(p.Endpoints))
		for _, ep := range p.Endpoints {
			eps = append(eps, providerEndpointView{Wire: ep.Wire, Adapter: ep.Adapter})
		}
		p.Endpoints = eps
		models := make([]providerModelView, 0, len(p.Models))
		for _, m := range p.Models {
			models = append(models, providerModelView{Model: m.Model, Unit: m.Unit})
		}
		p.Models = models
		out = append(out, p)
	}
	return out
}

// providersData returns all configured providers (keys masked) with per-vendor
// stats.
func (a *api) providersData() ([]providerView, error) {
	pvds, err := a.store.ListProviders()
	if err != nil {
		return nil, err
	}
	stats, err := a.store.VendorStats(nil, nil)
	if err != nil {
		return nil, err
	}
	views := make([]providerView, 0, len(pvds))
	for _, pvd := range pvds {
		st, ok := stats[pvd.Name]
		views = append(views, newProviderView(pvd, st, ok))
	}
	return views, nil
}

// handleGetProvider returns one provider.
func (a *api) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	view, err := a.getProviderData(r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "get provider", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// getProviderData returns one provider (key masked). An unknown id is a
// *apiError (404).
func (a *api) getProviderData(id string) (providerView, error) {
	pvd, err := a.store.GetProvider(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return providerView{}, notFoundErr("provider not found")
		}
		return providerView{}, err
	}
	return newProviderView(pvd, store.VendorStat{}, false), nil
}

// handleCreateProvider creates a provider from a JSON body and reloads the config.
func (a *api) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req createProviderReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	view, err := a.createProviderData(req)
	if err != nil {
		a.writeDataErr(w, "create provider", err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// createProviderData creates a provider and reloads the live config. A missing
// name or an invalid endpoint is a *apiError (400); a duplicate name is a
// *apiError (409).
func (a *api) createProviderData(req createProviderReq) (providerView, error) {
	if strings.TrimSpace(req.Name) == "" {
		return providerView{}, badRequestErr("name is required")
	}
	endpoints, msg := toStoreEndpoints(req.Endpoints)
	if msg != "" {
		return providerView{}, badRequestErr(msg)
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
			return providerView{}, conflictErr("a provider with that name already exists")
		}
		return providerView{}, err
	}
	a.reloadAfterWrite()
	return newProviderView(pvd, store.VendorStat{}, false), nil
}

// handlePatchProvider applies a subset of fields and reloads the config.
func (a *api) handlePatchProvider(w http.ResponseWriter, r *http.Request) {
	var req patchProviderReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	view, err := a.updateProviderData(r.PathValue("id"), req)
	if err != nil {
		a.writeDataErr(w, "update provider", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// updateProviderData applies a subset of fields and reloads the config. An empty
// api_key or invalid endpoint is a *apiError (400); an unknown id is 404; a
// duplicate name is 409.
func (a *api) updateProviderData(id string, req patchProviderReq) (providerView, error) {
	if req.APIKey != nil {
		trimmed := strings.TrimSpace(*req.APIKey)
		if trimmed == "" {
			return providerView{}, badRequestErr("api_key cannot be empty")
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
			return providerView{}, badRequestErr(msg)
		}
		upd.Endpoints = eps
		if upd.Endpoints == nil {
			upd.Endpoints = []store.ProviderEndpoint{} // explicit clear
		}
	}

	pvd, err := a.store.UpdateProvider(id, upd)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return providerView{}, notFoundErr("provider not found")
		}
		if isUniqueViolation(err) {
			return providerView{}, conflictErr("a provider with that name already exists")
		}
		return providerView{}, err
	}
	a.reloadAfterWrite()
	return newProviderView(pvd, store.VendorStat{}, false), nil
}

// handleDeleteProvider removes a provider and reloads the config.
func (a *api) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := a.deleteProviderData(r.PathValue("id")); err != nil {
		a.writeDataErr(w, "delete provider", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteProviderData removes a provider and reloads the config. An unknown id is
// a *apiError (404).
func (a *api) deleteProviderData(id string) error {
	if err := a.store.DeleteProvider(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundErr("provider not found")
		}
		return err
	}
	a.reloadAfterWrite()
	return nil
}

// handleTestProvider probes a configured provider's host origin for reachability,
// authenticating with its API key.
func (a *api) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	view, err := a.testProviderData(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "get provider", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// testProviderData probes the host origin of a provider's first endpoint,
// authenticating with its API key. Reachability failures are reported in the
// view (not as errors); only an unknown id returns a *apiError (404).
func (a *api) testProviderData(ctx context.Context, id string) (testVendorView, error) {
	pvd, err := a.store.GetProvider(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return testVendorView{}, notFoundErr("provider not found")
		}
		return testVendorView{}, err
	}

	if len(pvd.Endpoints) == 0 {
		return testVendorView{Reachable: false, Error: "provider has no endpoints"}, nil
	}
	ep := pvd.Endpoints[0]
	origin, err := originOf(ep.Endpoint)
	if err != nil {
		return testVendorView{Reachable: false, Error: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		return testVendorView{Reachable: false, Error: err.Error()}, nil
	}
	if pvd.APIKey != "" {
		applyTestAuth(req, ep.Adapter, pvd.APIKey)
	}

	start := a.now()
	resp, err := a.client.Do(req)
	latency := a.now().Sub(start).Milliseconds()
	if err != nil {
		return testVendorView{Reachable: false, LatencyMS: latency, Error: err.Error()}, nil
	}
	defer resp.Body.Close()
	drain(resp.Body)

	return testVendorView{Reachable: true, Status: resp.StatusCode, LatencyMS: latency}, nil
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
	Capture *bool `json:"capture,omitempty"`
}

// handlePatchSettings updates the gateway settings singleton and reloads.
func (a *api) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req patchSettingsReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	view, err := a.updateSettingsData(req)
	if err != nil {
		a.writeDataErr(w, "update settings", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// updateSettingsData applies the capture setting to the singleton, reloads the
// live config, and returns the resulting (non-secret) settings view.
func (a *api) updateSettingsData(req patchSettingsReq) (settingsView, error) {
	cur, err := a.store.GetAppSettings()
	if err != nil {
		return settingsView{}, err
	}
	if req.Capture != nil {
		cur.Capture = *req.Capture
	}
	if err := a.store.UpdateAppSettings(cur); err != nil {
		return settingsView{}, err
	}
	a.reloadAfterWrite()
	return a.settingsData(), nil
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
// each endpoint URL. It returns ("", problem) on the first invalid endpoint.
func toStoreEndpoints(in []providerEndpointReq) ([]store.ProviderEndpoint, string) {
	if in == nil {
		return nil, ""
	}
	out := make([]store.ProviderEndpoint, 0, len(in))
	for _, ep := range in {
		if strings.TrimSpace(ep.Wire) == "" {
			continue
		}
		if msg := validateEndpoint(ep.Endpoint); msg != "" {
			return nil, msg
		}
		adapter := ep.Adapter
		if adapter == "" {
			adapter = "openai-compatible"
		}
		out = append(out, store.ProviderEndpoint{Wire: ep.Wire, Endpoint: strings.TrimSpace(ep.Endpoint), Adapter: adapter})
	}
	return out, ""
}

// validateEndpoint returns "" when ep is a valid absolute http(s) URL with a
// host, else a human-readable problem message. A {model} placeholder is allowed
// (substituted at request time) and replaced with a probe before parsing.
func validateEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return "endpoint is required"
	}
	u, err := url.Parse(strings.ReplaceAll(ep, "{model}", "MODEL"))
	if err != nil {
		return "endpoint is not a valid URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "endpoint must be an absolute http or https URL"
	}
	if u.Host == "" {
		return "endpoint must include a host"
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

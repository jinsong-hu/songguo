package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/store"
)

// --- views ---

type serviceModelView struct {
	Model  string  `json:"model"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
}

type serviceCredentialView struct {
	ID        string `json:"id"`
	MaskedKey string `json:"masked_key"`
	CreatedAt string `json:"created_at"`
}

// serviceView is the JSON representation of a configured service. API keys are
// never serialized in the clear — only masked previews.
type serviceView struct {
	ID          string                  `json:"id"`
	Name        string                  `json:"name"`
	Vendor      string                  `json:"vendor"`
	Adapter     string                  `json:"adapter"`
	BaseURL     string                  `json:"base_url"`
	Priority    int                     `json:"priority"`
	Weight      int                     `json:"weight"`
	Enabled     bool                    `json:"enabled"`
	CatalogID   string                  `json:"catalog_id"`
	Credentials []serviceCredentialView `json:"credentials"`
	Models      []serviceModelView      `json:"models"`
	CreatedAt   string                  `json:"created_at"`
	UpdatedAt   string                  `json:"updated_at"`
	Stats       vendorStatsView         `json:"stats"`
}

func newServiceView(svc store.Service, stat store.VendorStat, hasStat bool) serviceView {
	creds := make([]serviceCredentialView, 0, len(svc.Credentials))
	for _, c := range svc.Credentials {
		creds = append(creds, serviceCredentialView{
			ID:        c.ID,
			MaskedKey: maskKey(c.APIKey),
			CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	models := make([]serviceModelView, 0, len(svc.Models))
	for _, m := range svc.Models {
		models = append(models, serviceModelView{Model: m.Model, Input: m.Input, Output: m.Output, Unit: m.Unit})
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

	return serviceView{
		ID:          svc.ID,
		Name:        svc.Name,
		Vendor:      svc.Vendor,
		Adapter:     svc.Adapter,
		BaseURL:     svc.BaseURL,
		Priority:    svc.Priority,
		Weight:      svc.Weight,
		Enabled:     svc.Enabled,
		CatalogID:   svc.CatalogID,
		Credentials: creds,
		Models:      models,
		CreatedAt:   svc.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   svc.UpdatedAt.UTC().Format(time.RFC3339),
		Stats:       sv,
	}
}

// --- request bodies ---

type serviceModelReq struct {
	Model  string  `json:"model"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
}

type createServiceReq struct {
	Name      string            `json:"name"`
	Vendor    string            `json:"vendor"`
	Adapter   string            `json:"adapter"`
	BaseURL   string            `json:"base_url"`
	Priority  int               `json:"priority"`
	Weight    int               `json:"weight"`
	Enabled   *bool             `json:"enabled"`
	CatalogID string            `json:"catalog_id"`
	APIKeys   []string          `json:"api_keys"`
	Models    []serviceModelReq `json:"models"`
}

type patchServiceReq struct {
	Name     *string            `json:"name"`
	Vendor   *string            `json:"vendor"`
	Adapter  *string            `json:"adapter"`
	BaseURL  *string            `json:"base_url"`
	Priority *int               `json:"priority"`
	Weight   *int               `json:"weight"`
	Enabled  *bool              `json:"enabled"`
	Models   *[]serviceModelReq `json:"models"`
}

// --- handlers ---

// handleListServices returns all configured services (keys masked) with stats.
func (a *api) handleListServices(w http.ResponseWriter, r *http.Request) {
	svcs, err := a.store.ListServices()
	if err != nil {
		a.serverError(w, "list services", err)
		return
	}
	stats, err := a.store.VendorStats(nil, nil)
	if err != nil {
		a.serverError(w, "vendor stats", err)
		return
	}
	views := make([]serviceView, 0, len(svcs))
	for _, svc := range svcs {
		st, ok := stats[svc.Name]
		views = append(views, newServiceView(svc, st, ok))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleGetService returns one service.
func (a *api) handleGetService(w http.ResponseWriter, r *http.Request) {
	svc, err := a.store.GetService(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "service not found")
			return
		}
		a.serverError(w, "get service", err)
		return
	}
	writeJSON(w, http.StatusOK, newServiceView(svc, store.VendorStat{}, false))
}

// handleCreateService creates a service from a JSON body and reloads the config.
func (a *api) handleCreateService(w http.ResponseWriter, r *http.Request) {
	var req createServiceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	if msg := validateBaseURL(req.BaseURL); msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	keys := make([]string, 0, len(req.APIKeys))
	for _, k := range req.APIKeys {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}

	svc, err := a.store.CreateService(store.NewService{
		Name:      strings.TrimSpace(req.Name),
		Vendor:    req.Vendor,
		Adapter:   req.Adapter,
		BaseURL:   strings.TrimSpace(req.BaseURL),
		Priority:  req.Priority,
		Weight:    req.Weight,
		Enabled:   enabled,
		CatalogID: req.CatalogID,
		APIKeys:   keys,
		Models:    toStoreModels(req.Models),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "conflict", "a service with that name already exists")
			return
		}
		a.serverError(w, "create service", err)
		return
	}
	a.reloadAfterWrite()
	writeJSON(w, http.StatusCreated, newServiceView(svc, store.VendorStat{}, false))
}

// handlePatchService applies a subset of fields and reloads the config.
func (a *api) handlePatchService(w http.ResponseWriter, r *http.Request) {
	var req patchServiceReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.BaseURL != nil {
		if msg := validateBaseURL(*req.BaseURL); msg != "" {
			writeError(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
	}
	upd := store.ServiceUpdate{
		Name:     req.Name,
		Vendor:   req.Vendor,
		Adapter:  req.Adapter,
		BaseURL:  req.BaseURL,
		Priority: req.Priority,
		Weight:   req.Weight,
		Enabled:  req.Enabled,
	}
	if req.Models != nil {
		upd.Models = toStoreModels(*req.Models)
		if upd.Models == nil {
			upd.Models = []store.ServiceModel{} // explicit clear
		}
	}

	svc, err := a.store.UpdateService(r.PathValue("id"), upd)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "service not found")
			return
		}
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "conflict", "a service with that name already exists")
			return
		}
		a.serverError(w, "update service", err)
		return
	}
	a.reloadAfterWrite()
	writeJSON(w, http.StatusOK, newServiceView(svc, store.VendorStat{}, false))
}

// handleDeleteService removes a service and reloads the config.
func (a *api) handleDeleteService(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteService(r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "service not found")
			return
		}
		a.serverError(w, "delete service", err)
		return
	}
	a.reloadAfterWrite()
	w.WriteHeader(http.StatusNoContent)
}

type addCredentialReq struct {
	APIKey string `json:"api_key"`
}

// handleAddCredential appends a key to a service's pool and reloads the config.
func (a *api) handleAddCredential(w http.ResponseWriter, r *http.Request) {
	var req addCredentialReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "api_key is required")
		return
	}
	cred, err := a.store.AddCredential(r.PathValue("id"), req.APIKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "service not found")
			return
		}
		a.serverError(w, "add credential", err)
		return
	}
	a.reloadAfterWrite()
	writeJSON(w, http.StatusCreated, serviceCredentialView{
		ID:        cred.ID,
		MaskedKey: maskKey(cred.APIKey),
		CreatedAt: cred.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// handleDeleteCredential removes one key from a service's pool.
func (a *api) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteCredential(r.PathValue("id"), r.PathValue("cid")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "credential not found")
			return
		}
		a.serverError(w, "delete credential", err)
		return
	}
	a.reloadAfterWrite()
	w.WriteHeader(http.StatusNoContent)
}

// handleTestService probes a configured service's host origin for reachability,
// authenticating with its first credential.
func (a *api) handleTestService(w http.ResponseWriter, r *http.Request) {
	svc, err := a.store.GetService(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "service not found")
			return
		}
		a.serverError(w, "get service", err)
		return
	}

	origin, err := originOf(svc.BaseURL)
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
	if len(svc.Credentials) > 0 && svc.Credentials[0].APIKey != "" {
		applyTestAuth(req, svc.Adapter, svc.Credentials[0].APIKey)
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
func toStoreModels(in []serviceModelReq) []store.ServiceModel {
	if in == nil {
		return nil
	}
	out := make([]store.ServiceModel, 0, len(in))
	for _, m := range in {
		if strings.TrimSpace(m.Model) == "" {
			continue
		}
		unit := m.Unit
		if unit == "" {
			unit = "per_1m_tokens"
		}
		out = append(out, store.ServiceModel{Model: m.Model, Input: m.Input, Output: m.Output, Unit: unit})
	}
	return out
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
// (e.g. a duplicate service name).
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
	req.Header.Set("Authorization", "Bearer "+key)
}

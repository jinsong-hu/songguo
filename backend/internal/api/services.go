package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/songguo/songguo/internal/store"
	"github.com/songguo/songguo/internal/wire"
)

// --- auto-derived services (model-centric view) ---

type serviceProviderView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Enabled          bool   `json:"enabled"`
	ProviderEnabled  bool   `json:"provider_enabled"`
	Routable         bool   `json:"routable"`
	Priority         int    `json:"priority"`
	Weight           int    `json:"weight"`
	DefaultPriority  int    `json:"default_priority"`
	DefaultWeight    int    `json:"default_weight"`
	PriorityOverride *int   `json:"priority_override"`
	WeightOverride   *int   `json:"weight_override"`
}

type serviceStatsView struct {
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

type serviceView struct {
	Model     string                `json:"model"`
	Providers []serviceProviderView `json:"providers"`
	Stats     serviceStatsView      `json:"stats"`
}

// handleListServices returns the model-centric service list. Operators see
// every configured provider/model relationship, including disabled ones so
// they can be re-enabled. Consumer keys see only relationships that can
// currently route.
func (a *api) handleListServices(w http.ResponseWriter, r *http.Request) {
	includeDisabled := roleFrom(r) != roleUser
	views, err := a.servicesData(includeDisabled)
	if err != nil {
		a.writeDataErr(w, "model stats", err)
		return
	}
	if u, ok := userFrom(r); ok && len(u.Scope) > 0 {
		views = filterServicesByScope(views, u.Scope)
	}
	writeJSON(w, http.StatusOK, views)
}

// filterServicesByScope keeps only services whose model is in the allowed set.
func filterServicesByScope(views []serviceView, scope []string) []serviceView {
	allowed := make(map[string]bool, len(scope))
	for _, m := range scope {
		allowed[m] = true
	}
	out := make([]serviceView, 0, len(views))
	for _, v := range views {
		if allowed[v.Model] {
			out = append(out, v)
		}
	}
	return out
}

// servicesData derives services from provider model declarations. The provider
// row supplies default routing; nullable model overrides replace those defaults.
// includeDisabled keeps unavailable relationships visible for operator config.
func (a *api) servicesData(includeDisabled bool) ([]serviceView, error) {
	providers, err := a.store.ListProviders()
	if err != nil {
		return nil, err
	}

	modelProviders := make(map[string][]serviceProviderView)
	for _, pvd := range providers {
		hasKnownEndpoint := false
		for _, endpoint := range pvd.Endpoints {
			if _, ok := wire.Get(endpoint.Wire); ok {
				hasKnownEndpoint = true
				break
			}
		}
		providerReady := pvd.Enabled && pvd.APIKey != "" && hasKnownEndpoint
		for _, model := range pvd.Models {
			enabled := true
			if model.RoutingConfigured {
				enabled = model.RoutingEnabled
			}
			routable := providerReady && enabled
			if !includeDisabled && !routable {
				continue
			}

			priority := pvd.Priority
			if model.PriorityOverride != nil {
				priority = *model.PriorityOverride
			}
			weight := pvd.Weight
			if model.WeightOverride != nil {
				weight = *model.WeightOverride
			}
			if weight <= 0 {
				weight = 1
			}
			modelProviders[model.Model] = append(modelProviders[model.Model], serviceProviderView{
				ID:               pvd.ID,
				Name:             pvd.Name,
				Enabled:          enabled,
				ProviderEnabled:  pvd.Enabled,
				Routable:         routable,
				Priority:         priority,
				Weight:           weight,
				DefaultPriority:  pvd.Priority,
				DefaultWeight:    pvd.Weight,
				PriorityOverride: model.PriorityOverride,
				WeightOverride:   model.WeightOverride,
			})
		}
	}

	modelStats, err := a.store.ModelStats(nil, nil)
	if err != nil {
		return nil, err
	}

	models := make([]string, 0, len(modelProviders))
	for model := range modelProviders {
		models = append(models, model)
	}
	sort.Strings(models)

	views := make([]serviceView, 0, len(models))
	for _, model := range models {
		providers := modelProviders[model]
		sort.SliceStable(providers, func(i, j int) bool {
			if providers[i].Priority != providers[j].Priority {
				return providers[i].Priority < providers[j].Priority
			}
			return providers[i].Name < providers[j].Name
		})
		stats := serviceStatsView{}
		if stat, ok := modelStats[model]; ok {
			stats.Requests = stat.Requests
			stats.Errors = stat.Errors
			stats.AvgLatencyMS = stat.AvgLatency
		}
		views = append(views, serviceView{Model: model, Providers: providers, Stats: stats})
	}
	return views, nil
}

type patchServiceProviderRoutingReq struct {
	Model           string `json:"model"`
	Enabled         *bool  `json:"enabled,omitempty"`
	Priority        *int   `json:"priority,omitempty"`
	Weight          *int   `json:"weight,omitempty"`
	InheritPriority bool   `json:"inherit_priority,omitempty"`
	InheritWeight   bool   `json:"inherit_weight,omitempty"`
}

// handlePatchServiceProviderRouting changes one provider's routing policy
// within one model service. The model is in the body so model ids containing
// slashes do not need to be embedded in the URL path.
func (a *api) handlePatchServiceProviderRouting(w http.ResponseWriter, r *http.Request) {
	var req patchServiceProviderRoutingReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := a.patchServiceProviderRoutingData(r.PathValue("id"), req); err != nil {
		a.writeDataErr(w, "update service routing", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) patchServiceProviderRoutingData(providerID string, req patchServiceProviderRoutingReq) error {
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		return badRequestErr("model is required")
	}
	if req.Priority != nil && req.InheritPriority {
		return badRequestErr("priority and inherit_priority cannot both be set")
	}
	if req.Weight != nil && req.InheritWeight {
		return badRequestErr("weight and inherit_weight cannot both be set")
	}
	if req.Priority != nil && *req.Priority < 0 {
		return badRequestErr("priority must be zero or greater")
	}
	if req.Weight != nil && *req.Weight < 1 {
		return badRequestErr("weight must be at least 1")
	}

	setPriority := req.Priority != nil || req.InheritPriority
	setWeight := req.Weight != nil || req.InheritWeight
	err := a.store.UpdateProviderModelRouting(
		providerID,
		req.Model,
		req.Enabled,
		req.Priority,
		setPriority,
		req.Weight,
		setWeight,
	)
	if errors.Is(err, store.ErrNotFound) {
		return notFoundErr("provider does not serve this model")
	}
	if err != nil {
		return err
	}
	a.reloadAfterWrite()
	return nil
}

package api

import (
	"net/http"
	"sort"
)

// --- auto-derived services (model-centric view) ---

type serviceProviderView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	Weight   int    `json:"weight"`
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

// handleListServices returns the auto-derived, model-centric service list.
// Each service corresponds to a unique model name served by one or more
// providers. No database table — derived from the live snapshot + call stats.
func (a *api) handleListServices(w http.ResponseWriter, r *http.Request) {
	views, err := a.servicesData()
	if err != nil {
		a.writeDataErr(w, "model stats", err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// servicesData derives the model-centric service list from the live snapshot
// (which providers serve each model) plus per-model call stats.
func (a *api) servicesData() ([]serviceView, error) {
	snap := a.snap()
	if snap == nil {
		return []serviceView{}, nil
	}

	vendors := snap.Vendors()

	// Group vendors by model.
	type providerInfo struct {
		id   string
		name string
		prio int
		wt   int
	}
	modelProviders := make(map[string][]providerInfo)
	for _, v := range vendors {
		for _, m := range v.ServedModels {
			modelProviders[m] = append(modelProviders[m], providerInfo{
				id:   v.Credential.ID,
				name: v.Name,
				prio: v.Priority,
				wt:   v.Weight,
			})
		}
	}

	// Aggregate call stats per model.
	modelStats, err := a.store.ModelStats(nil, nil)
	if err != nil {
		return nil, err
	}

	// Sort models for stable output.
	models := make([]string, 0, len(modelProviders))
	for m := range modelProviders {
		models = append(models, m)
	}
	sort.Strings(models)

	views := make([]serviceView, 0, len(models))
	for _, m := range models {
		pis := modelProviders[m]
		pvs := make([]serviceProviderView, 0, len(pis))
		for _, pi := range pis {
			pvs = append(pvs, serviceProviderView{
				ID: pi.id, Name: pi.name, Priority: pi.prio, Weight: pi.wt,
			})
		}
		sv := serviceStatsView{}
		if stat, ok := modelStats[m]; ok {
			sv.Requests = stat.Requests
			sv.Errors = stat.Errors
			sv.AvgLatencyMS = stat.AvgLatency
		}
		views = append(views, serviceView{Model: m, Providers: pvs, Stats: sv})
	}
	return views, nil
}

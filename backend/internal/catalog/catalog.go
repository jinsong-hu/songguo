// Package catalog serves a curated, read-only directory of known vendors and
// their services (presets) that the dashboard lists for one-click add. It is
// pure reference data — prices, model lists, and metadata for discovery — and
// is independent of what the user has actually configured. Adding a catalog
// service instantiates a configured service (in the store) from the preset,
// pre-filling everything except the user's own API key.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed catalog.json
var raw []byte

// Catalog is the root of the preset directory.
type Catalog struct {
	Vendors []Vendor `json:"vendors"`
}

// Vendor is one upstream provider's preset: its model price list (defined once,
// keyed by model id) and the endpoints it exposes. Quirks parameterize usage
// extraction (see internal/wire) and apply to the whole vendor.
type Vendor struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Homepage string            `json:"homepage,omitempty"`
	Quirks   map[string]string `json:"quirks,omitempty"`
	// Custom marks a template vendor with no preset models or pinned upstream:
	// the user supplies the base URL (substituted into the {base} placeholder in
	// each endpoint) and types their own model ids. Used for the "Custom" tile.
	Custom    bool             `json:"custom,omitempty"`
	Models    map[string]Model `json:"models"`
	Endpoints []Endpoint       `json:"endpoints"`
}

// Endpoint is one preset wire bound to its full upstream URL + adapter (auth
// scheme), 1:1 with the wire. The URL is used as-is by the proxy and may carry a
// {model} placeholder (or, on Custom vendors, a {base} placeholder the user
// fills in). Models lists the model ids (keys into Vendor.Models) this
// endpoint serves; companion wires like a model-listing endpoint carry none.
type Endpoint struct {
	Wire     string   `json:"wire"`
	Endpoint string   `json:"endpoint"`
	Adapter  string   `json:"adapter"`
	Docs     string   `json:"docs,omitempty"`
	Note     string   `json:"note,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// Model is a catalog model with its default price and descriptive metadata.
type Model struct {
	Input       float64  `json:"input"`
	Output      float64  `json:"output"`
	CachedInput float64  `json:"cached_input,omitempty"`
	Unit        string   `json:"unit"`
	Context     int      `json:"context,omitempty"`
	Modalities  []string `json:"modalities,omitempty"`
}

// Load parses the embedded catalog. It fails fast if catalog.json is malformed,
// which surfaces at startup/test time rather than to an end user.
func Load() (Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return Catalog{}, fmt.Errorf("catalog: parse: %w", err)
	}
	return c, nil
}

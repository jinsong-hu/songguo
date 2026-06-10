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

// Vendor groups a provider's services for display.
type Vendor struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Homepage string    `json:"homepage,omitempty"`
	Services []Service `json:"services"`
}

// Service is one preset: an adapter + base_url + the models it serves, plus
// descriptive metadata for the listing/playground.
type Service struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"` // chat, embedding, asr, tts, image, mcp, ...
	Adapter string  `json:"adapter"`
	BaseURL string  `json:"base_url"`
	Docs    string  `json:"docs,omitempty"`
	Note    string  `json:"note,omitempty"`
	Models  []Model `json:"models"`
}

// Model is a catalog model with its default price and descriptive metadata.
type Model struct {
	Model      string   `json:"model"`
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	Unit       string   `json:"unit"`
	Context    int      `json:"context,omitempty"`
	Modalities []string `json:"modalities,omitempty"`
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

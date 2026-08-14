// Package catalog serves the read-only directory of known providers and the
// models they serve, which the dashboard lists for one-click add. It is pure
// reference data, independent of what the operator has actually configured;
// adding a catalog provider instantiates a configured provider (in the store)
// from the preset, pre-filling everything except the API key.
//
// # Shape
//
// The types mirror models.dev's api.json exactly — a map keyed by provider id,
// each provider carrying a map of models with modalities/limit/cost. That is
// deliberate: cmd/catalogsync copies models.dev's model subtrees verbatim
// rather than translating them, so there is no field mapping to drift, no unit
// conversion, and no rounding step that could make a re-sync see a difference
// where none exists.
//
// # Two files, two owners
//
//   - models.json is generated. cmd/catalogsync owns every byte; do not hand-edit.
//   - catalog.json is hand-maintained. It holds the routing topology — endpoints,
//     adapters, quirks, the custom template — which models.dev has no notion of
//     because everything there is SDK-shaped rather than URL-shaped. It also holds
//     the models models.dev cannot supply: every Volcengine model (no first-party
//     provider exists there), Alibaba's and Zhipu's embeddings, and everything
//     billed per character, per second or per call.
//
// Load merges them, and a hand-written entry always wins — which doubles as the
// escape hatch for pinning a rate the generator would otherwise overwrite.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
)

//go:embed models.json
var generated []byte

//go:embed catalog.json
var manual []byte

// Catalog is the provider directory, keyed by provider id.
type Catalog map[string]Provider

// Provider is one upstream's preset. The first group of fields is models.dev's
// provider shape; Endpoints, Quirks and Custom are songguo's own and have no
// models.dev counterpart.
type Provider struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Doc    string           `json:"doc,omitempty"`
	Env    []string         `json:"env,omitempty"`
	NPM    string           `json:"npm,omitempty"`
	Models map[string]Model `json:"models"`

	// Endpoints binds each wire to its full upstream URL and auth adapter.
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// Quirks parameterize usage extraction (see internal/wire) for the whole
	// provider. They never change what is sent.
	Quirks map[string]string `json:"quirks,omitempty"`
	// Custom marks a template provider with no preset models or pinned upstream:
	// the operator supplies the base URL (substituted into the {base} placeholder
	// in each endpoint) and types their own model ids. Used for the "Custom" tile.
	Custom bool `json:"custom,omitempty"`
}

// Endpoint is one preset wire bound to its full upstream URL + adapter, 1:1 with
// the wire. The URL is used as-is by the proxy and may carry a {model}
// placeholder (or, on Custom providers, a {base} placeholder). Models lists the
// model ids this endpoint serves — and is also what cmd/catalogsync reads to
// decide which models to generate; companion wires like a model-listing endpoint
// carry none.
type Endpoint struct {
	Wire     string   `json:"wire"`
	Endpoint string   `json:"endpoint"`
	Adapter  string   `json:"adapter"`
	Docs     string   `json:"docs,omitempty"`
	Note     string   `json:"note,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// Model is one model's descriptive metadata and price, in models.dev's shape.
type Model struct {
	ID          string     `json:"id"`
	Name        string     `json:"name,omitempty"`
	Family      string     `json:"family,omitempty"`
	Attachment  bool       `json:"attachment,omitempty"`
	Reasoning   bool       `json:"reasoning,omitempty"`
	ToolCall    bool       `json:"tool_call,omitempty"`
	Temperature bool       `json:"temperature,omitempty"`
	ReleaseDate string     `json:"release_date,omitempty"`
	LastUpdated string     `json:"last_updated,omitempty"`
	OpenWeights bool       `json:"open_weights,omitempty"`
	Modalities  Modalities `json:"modalities,omitempty"`
	Limit       Limit      `json:"limit,omitempty"`
	Cost        Cost       `json:"cost"`
}

// Modalities is what a model accepts and produces: text, image, audio, video,
// file (models.dev's vocabulary; the dashboard labels the first four).
type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// Limit is the model's context window and per-message caps. Zero means the
// upstream does not publish one.
type Limit struct {
	Context int `json:"context,omitempty"`
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
}

// Cost is the rate for each quantity songguo meters. Every field is optional and
// they are **additive**, not alternatives: a model's cost is the sum over the
// axes it declares. That is what lets an audio model bill tokens and seconds at
// once, which the old single-`unit` price could not express — it would price one
// axis and silently compute $0 for the other.
//
// Each axis maps 1:1 onto a wire.Normalized field, so adding a metered quantity
// means adding a field here and a term in pricing.Cost, and nothing else.
//
// The two groups differ in scale, and the split is not ours to remove: the first
// four are models.dev's own fields at models.dev's basis (USD per 1M tokens), so
// generated entries stay byte-identical to upstream. The rest are songguo
// extensions for wires models.dev does not cover, at the basis the vendors
// publish (USD per single unit).
type Cost struct {
	// Per 1M tokens — models.dev's fields, copied verbatim.
	Input      float64 `json:"input,omitempty"`       // fresh input tokens
	Output     float64 `json:"output,omitempty"`      // output tokens
	CacheRead  float64 `json:"cache_read,omitempty"`  // cache-read input tokens
	CacheWrite float64 `json:"cache_write,omitempty"` // cache-write input tokens

	// Per single unit — songguo extensions. models.dev has no unit concept and
	// no field of any kind for these, across all 5,901 models it lists.
	Character float64 `json:"character,omitempty"` // per character (volc TTS)
	Second    float64 `json:"second,omitempty"`    // per second of audio (volc ASR)
	Image     float64 `json:"image,omitempty"`     // per image
	Call      float64 `json:"call,omitempty"`      // per request (volc video)
}

// Zero reports whether a cost declares no rate at all, i.e. would always meter
// $0. Distinct from a published zero on a genuinely free tier, which declares a
// rate that happens to be zero — callers that care about provenance must ask
// where the cost came from, not whether it is empty.
func (c Cost) Zero() bool { return c == Cost{} }

// Tokens reports whether the cost prices token usage, which is the axis group
// that most models use and the only one models.dev supplies.
func (c Cost) Tokens() bool {
	return c.Input != 0 || c.Output != 0 || c.CacheRead != 0 || c.CacheWrite != 0
}

// Load parses and merges the embedded catalog. It fails fast if either file is
// malformed, which surfaces at startup/test time rather than to an end user.
//
// The hand-maintained file wins: a model present in both keeps its hand-written
// cost, so pinning a rate the generator would overwrite is a matter of adding it
// to catalog.json.
func Load() (Catalog, error) {
	gen, err := parse(generated, "models.json")
	if err != nil {
		return nil, err
	}
	man, err := parse(manual, "catalog.json")
	if err != nil {
		return nil, err
	}
	return merge(gen, man), nil
}

func parse(raw []byte, name string) (Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("catalog: parse %s: %w", name, err)
	}
	return c, nil
}

// merge overlays the hand-maintained catalog onto the generated one. Providers
// are unioned; within a provider the hand-written topology and any hand-written
// model replace their generated counterparts, and generated models the manual
// file says nothing about are kept.
func merge(gen, man Catalog) Catalog {
	out := make(Catalog, len(gen)+len(man))
	for id, p := range gen {
		p.Models = maps.Clone(p.Models)
		out[id] = p
	}
	for id, m := range man {
		p, ok := out[id]
		if !ok {
			out[id] = m
			continue
		}
		// Identity and topology are the manual file's to state; the generator
		// emits neither, so an empty field there must not blank one here.
		if m.Name != "" {
			p.Name = m.Name
		}
		if m.Doc != "" {
			p.Doc = m.Doc
		}
		if len(m.Env) > 0 {
			p.Env = m.Env
		}
		if m.NPM != "" {
			p.NPM = m.NPM
		}
		p.ID = id
		p.Endpoints = m.Endpoints
		p.Quirks = m.Quirks
		p.Custom = m.Custom
		if p.Models == nil {
			p.Models = make(map[string]Model, len(m.Models))
		}
		maps.Copy(p.Models, m.Models)
		out[id] = p
	}
	return out
}

// DeclaredModels returns the model ids a provider's endpoints serve, deduped.
// It is the set cmd/catalogsync generates prices for: the catalog carries the
// models songguo actually routes, not every model an upstream happens to list.
func (p Provider) DeclaredModels() []string {
	seen := make(map[string]bool)
	var out []string
	for _, ep := range p.Endpoints {
		for _, m := range ep.Models {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

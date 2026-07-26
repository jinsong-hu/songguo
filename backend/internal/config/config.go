// Package config defines the gateway's vendor and settings types, validates
// them, and builds an immutable routing Snapshot. The SQLite-backed config
// manager assembles a Config from stored service rows and calls Build; no file
// parsing happens in this package.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Settings holds gateway-wide options.
type Settings struct{}

// Credential is a vendor's upstream API key. A vendor holds exactly one; to
// use several keys against the same platform, configure several services that
// serve the same models and let model routing spread across them.
type Credential struct {
	ID     string `yaml:"id"`
	APIKey string `yaml:"api_key"`
}

// Price is the true per-model cost used for metering and cheapest-route.
// CachedInput is the rate for cache-hit input tokens; non-positive means
// "charge the full Input rate" (no cache discount configured).
//
// Source records where the rate came from, so a synthetic price is never
// mistaken for a published one. It is metadata only — pricing.Cost ignores it —
// but it is surfaced through GET /api/pricing and the vendor view so the ledger
// and the pricing table can always be reconciled. See PriceSource* below.
type Price struct {
	Input       float64 `yaml:"input"`
	Output      float64 `yaml:"output"`
	CachedInput float64 `yaml:"cached_input"`
	Unit        string  `yaml:"unit"`   // e.g. per_1m_tokens, per_1k_tokens, per_token, per_call, per_image, per_second, per_char
	Source      string  `yaml:"source"` // provenance; see PriceSource*
}

// Price provenance values. Anything with the PriceSourceFallbackPrefix is a
// guess: the model had no published rate, so it borrowed the named model's
// price (see configsvc.fallbackPrice).
const (
	PriceSourceCatalog        = "catalog"   // matched an entry in the embedded catalog
	PriceSourceOverride       = "override"  // operator typed the rate and set price_override
	PriceSourceStored         = "stored"    // rate came from the provider row as-is
	PriceSourceUnpriced       = "unpriced"  // no rate available; meters $0
	PriceSourceFallbackPrefix = "fallback:" // + the model whose price was borrowed
)

// IsFallbackPrice reports whether p's rate was borrowed from another model
// rather than published for this one.
func IsFallbackPrice(p Price) bool {
	return strings.HasPrefix(p.Source, PriceSourceFallbackPrefix)
}

// KnownPriceUnits is the set of pricing units the cost engine understands (see
// internal/pricing.Cost). Any other unit silently meters as $0, so config
// assembly warns when a price uses one outside this set.
var KnownPriceUnits = map[string]bool{
	"per_1m_tokens": true,
	"per_1k_tokens": true,
	"per_token":     true,
	"per_call":      true,
	"per_image":     true,
	"per_second":    true,
	"per_char":      true,
}

// PriceMetersZero reports whether a price would always compute $0 cost: a
// token-rate price with both input and output non-positive, or a single-rate
// (per_call/per_image/per_second/per_char) price with a non-positive input.
func PriceMetersZero(p Price) bool {
	switch p.Unit {
	case "per_1m_tokens", "per_1k_tokens", "per_token":
		return p.Input <= 0 && p.Output <= 0
	default:
		return p.Input <= 0
	}
}

// PriceUnitFamily groups units that price the same physical thing, so two
// prices can be compared or substituted for one another. The three token units
// differ only by scale and share a family; every other unit stands alone
// because the quantities are disjoint — wire.Normalized keeps Seconds, Chars,
// Images and Calls separate from the token counts, and pricing.Cost dispatches
// on Unit alone. Substituting across families therefore does not over- or
// under-bill, it silently computes $0. An unrecognized unit has no family.
func PriceUnitFamily(unit string) string {
	switch unit {
	case "per_1m_tokens", "per_1k_tokens", "per_token":
		return "tokens"
	case "per_call", "per_image", "per_second", "per_char":
		return unit
	default:
		return ""
	}
}

// PriceRank scores a price so two prices in the same family can be ordered by
// expensiveness. Token rates are normalized to a per-1M basis so units within
// the family are comparable; the score is the dominant side (output rates
// exceed input rates for every model in the catalogue, but take the max rather
// than assume it). Single-rate units score on Input, their only rate. Prices
// outside any known family score 0 — they can never win a comparison.
func PriceRank(p Price) float64 {
	side := p.Input
	if p.Output > side {
		side = p.Output
	}
	switch p.Unit {
	case "per_1m_tokens":
		return side
	case "per_1k_tokens":
		return side * 1e3
	case "per_token":
		return side * 1e6
	case "per_call", "per_image", "per_second", "per_char":
		return p.Input
	default:
		return 0
	}
}

// MaxSaneTokenRate bounds a per-1M-normalized token rate that config assembly
// will treat as real. It exists because per_token and per_1k_tokens normalize
// by 1e6 and 1e3: a rate typed against the wrong unit (input: 3.0 as per_token
// rather than per_1m_tokens) becomes $3,000,000/1M. Such a row still meters as
// written — we do not silently rewrite an operator's number — but it is warned
// about and barred from being borrowed as another model's fallback price, so
// one typo cannot re-price a whole provider.
const MaxSaneTokenRate = 1000.0

// PriceRateImplausible reports whether a price's normalized rate exceeds
// MaxSaneTokenRate, i.e. it is almost certainly a unit mistake.
func PriceRateImplausible(p Price) bool {
	return PriceUnitFamily(p.Unit) == "tokens" && PriceRank(p) > MaxSaneTokenRate
}

// Adapter names the auth scheme a vendor expects (header style applied when
// the proxy swaps in the credential).
const (
	AdapterOpenAI     = "openai-compatible"
	AdapterAnthropic  = "anthropic-compatible"
	AdapterVolcSpeech = "volc-speech" // ByteDance openspeech: X-Api-Key, no version header
	AdapterMCP        = "mcp"
)

// Vendor is an upstream AI provider.
type Vendor struct {
	Name         string           `yaml:"name"`
	Origin       string           `yaml:"origin"`  // scheme://host, used for passthrough/WebSocket and forwarding unmatched paths
	Adapter      string           `yaml:"adapter"` // auth scheme; default openai-compatible
	ServedModels []string         `yaml:"served_models"`
	Priority     int              `yaml:"priority"` // lower = preferred; default 0
	Weight       int              `yaml:"weight"`   // weighted round-robin within a priority; normalized to >=1
	Credential   Credential       `yaml:"credential"`
	Prices       map[string]Price `yaml:"prices"`
	// Wires is the allowlist of wire names (see internal/wire) the proxy may
	// serve for this vendor; paths matching none are denied unless
	// AllowUnmatched forwards them metered-zero.
	Wires []string `yaml:"wires"`
	// Endpoints maps each enabled wire to its full upstream URL, used as-is in
	// model-routed mode (no suffix join). A value may carry a {model} placeholder
	// (substituted with the request's model) and a query string (e.g. Azure's
	// ?api-version=…), which is merged with any inbound query.
	Endpoints      map[string]string `yaml:"endpoints"`
	AllowUnmatched bool              `yaml:"allow_unmatched"`
	Quirks         map[string]string `yaml:"quirks"`
}

// Config is the root configuration assembled from stored service rows.
type Config struct {
	Settings Settings `yaml:"settings"`
	Vendors  []Vendor `yaml:"vendors"`
}

// Build normalizes, validates, and indexes an in-memory Config into an
// immutable Snapshot. It is the core used by the SQLite-backed config manager,
// which assembles a Config from stored service rows.
func Build(cfg Config) (*Snapshot, error) {
	normalize(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return newSnapshot(cfg), nil
}

// normalize applies defaults that should hold regardless of validity.
func normalize(cfg *Config) {
	for i := range cfg.Vendors {
		if cfg.Vendors[i].Weight <= 0 {
			cfg.Vendors[i].Weight = 1
		}
		if cfg.Vendors[i].Adapter == "" {
			cfg.Vendors[i].Adapter = AdapterOpenAI
		}
		// The wire allowlist is the set of wires that have an endpoint; derive it
		// when not given explicitly (the store path sets both).
		if len(cfg.Vendors[i].Wires) == 0 && len(cfg.Vendors[i].Endpoints) > 0 {
			wires := make([]string, 0, len(cfg.Vendors[i].Endpoints))
			for w := range cfg.Vendors[i].Endpoints {
				wires = append(wires, w)
			}
			sort.Strings(wires)
			cfg.Vendors[i].Wires = wires
		}
		// The credential ID identifies which key served a call in the ledger;
		// with one key per vendor it defaults to the vendor's own name.
		if cfg.Vendors[i].Credential.ID == "" {
			cfg.Vendors[i].Credential.ID = cfg.Vendors[i].Name
		}
	}
}

// validate checks every invariant and aggregates all problems so a single
// load surfaces the full list rather than the first failure.
func validate(cfg *Config) error {
	var problems []error

	seenVendor := make(map[string]struct{}, len(cfg.Vendors))

	for vi := range cfg.Vendors {
		v := &cfg.Vendors[vi]
		who := vendorLabel(v.Name, vi)

		if v.Name == "" {
			problems = append(problems, fmt.Errorf("%s: name must be non-empty", who))
		} else if _, dup := seenVendor[v.Name]; dup {
			problems = append(problems, fmt.Errorf("vendor %q: duplicate vendor name", v.Name))
		} else {
			seenVendor[v.Name] = struct{}{}
		}

		problems = append(problems, validateOrigin(who, v.Origin)...)
		for wname, ep := range v.Endpoints {
			problems = append(problems, validateEndpoint(who, wname, ep)...)
		}
		problems = append(problems, validateServedModels(who, v.ServedModels)...)
		if v.Credential.APIKey == "" {
			problems = append(problems, fmt.Errorf("%s: credential api_key must be non-empty", who))
		}
		problems = append(problems, validatePrices(who, v.Prices)...)
	}

	return errors.Join(problems...)
}

// validateOrigin checks a vendor's scheme://host origin, used for passthrough
// forwarding, WebSocket dials, and forwarding paths that match no wire.
func validateOrigin(who, origin string) []error {
	return validateURLField(who, "origin", origin, false)
}

// validateEndpoint checks one wire's full upstream URL. A {model} placeholder is
// allowed (substituted at request time); it is replaced with a probe before
// parsing so the braces can't trip the URL parser.
func validateEndpoint(who, wire, ep string) []error {
	return validateURLField(who, fmt.Sprintf("endpoint for wire %q", wire), ep, true)
}

func validateURLField(who, label, raw string, allowTemplate bool) []error {
	if raw == "" {
		return []error{fmt.Errorf("%s: %s must be non-empty", who, label)}
	}
	probe := raw
	if allowTemplate {
		probe = strings.ReplaceAll(probe, "{model}", "MODEL")
	}
	u, err := url.Parse(probe)
	if err != nil {
		return []error{fmt.Errorf("%s: %s %q is not a valid URL: %w", who, label, raw, err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return []error{fmt.Errorf("%s: %s %q must be an absolute http or https URL", who, label, raw)}
	}
	if u.Host == "" {
		return []error{fmt.Errorf("%s: %s %q must include a host", who, label, raw)}
	}
	return nil
}

func validateServedModels(who string, models []string) []error {
	if len(models) == 0 {
		return []error{fmt.Errorf("%s: served_models must be non-empty", who)}
	}
	var problems []error
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		if m == "" {
			problems = append(problems, fmt.Errorf("%s: served_models contains an empty model name", who))
			continue
		}
		if _, dup := seen[m]; dup {
			problems = append(problems, fmt.Errorf("%s: duplicate served model %q", who, m))
			continue
		}
		seen[m] = struct{}{}
	}
	return problems
}

func validatePrices(who string, prices map[string]Price) []error {
	var problems []error
	for model, p := range prices {
		if p.Unit == "" {
			problems = append(problems, fmt.Errorf("%s: price for model %q has an empty unit", who, model))
		}
	}
	return problems
}

func vendorLabel(name string, idx int) string {
	if name == "" {
		return fmt.Sprintf("vendor #%d", idx)
	}
	return "vendor " + strconv.Quote(name)
}

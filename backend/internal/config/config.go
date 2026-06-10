// Package config defines the gateway's vendor and settings types, validates
// them, and builds an immutable routing Snapshot. The SQLite-backed config
// manager assembles a Config from stored service rows and calls Build; no file
// parsing happens in this package.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Default capture tuning, applied during normalization when a value is unset
// (zero) or non-positive.
const (
	defaultCaptureMaxBytes = 32768
	defaultCaptureRetain   = 10000
)

// Settings holds gateway-wide options.
type Settings struct {
	Capture         bool   `yaml:"capture"`
	CaptureMaxBytes int    `yaml:"capture_max_bytes"`
	CaptureRetain   int    `yaml:"capture_retain"`
}

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
type Price struct {
	Input       float64 `yaml:"input"`
	Output      float64 `yaml:"output"`
	CachedInput float64 `yaml:"cached_input"`
	Unit        string  `yaml:"unit"` // e.g. per_1m_tokens, per_1k_tokens, per_token, per_call, per_image, per_second, per_char
}

// Adapter names the auth scheme a vendor expects (header style applied when
// the proxy swaps in the credential).
const (
	AdapterOpenAI    = "openai-compatible"
	AdapterAnthropic = "anthropic-compatible"
	AdapterMCP       = "mcp"
)

// Vendor is an upstream AI provider.
type Vendor struct {
	Name         string           `yaml:"name"`
	BaseURL      string           `yaml:"base_url"`
	Adapter      string           `yaml:"adapter"` // auth scheme; default openai-compatible
	ServedModels []string         `yaml:"served_models"`
	Priority     int              `yaml:"priority"` // lower = preferred; default 0
	Weight       int              `yaml:"weight"`   // weighted round-robin within a priority; normalized to >=1
	Credential   Credential       `yaml:"credential"`
	Prices       map[string]Price `yaml:"prices"`
	// Wires is the allowlist of wire names (see internal/wire) the proxy may
	// serve for this vendor; paths matching none are denied unless
	// AllowUnmatched forwards them metered-zero.
	Wires          []string          `yaml:"wires"`
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
	if cfg.Settings.CaptureMaxBytes <= 0 {
		cfg.Settings.CaptureMaxBytes = defaultCaptureMaxBytes
	}
	if cfg.Settings.CaptureRetain <= 0 {
		cfg.Settings.CaptureRetain = defaultCaptureRetain
	}

	for i := range cfg.Vendors {
		if cfg.Vendors[i].Weight <= 0 {
			cfg.Vendors[i].Weight = 1
		}
		if cfg.Vendors[i].Adapter == "" {
			cfg.Vendors[i].Adapter = AdapterOpenAI
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

	if cfg.Settings.CaptureMaxBytes < 0 {
		problems = append(problems, fmt.Errorf("settings: capture_max_bytes must be non-negative"))
	}
	if cfg.Settings.CaptureRetain < 0 {
		problems = append(problems, fmt.Errorf("settings: capture_retain must be non-negative"))
	}

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

		problems = append(problems, validateBaseURL(who, v.BaseURL)...)
		problems = append(problems, validateServedModels(who, v.ServedModels)...)
		if v.Credential.APIKey == "" {
			problems = append(problems, fmt.Errorf("%s: credential api_key must be non-empty", who))
		}
		problems = append(problems, validatePrices(who, v.Prices)...)
	}

	return errors.Join(problems...)
}

func validateBaseURL(who, base string) []error {
	if base == "" {
		return []error{fmt.Errorf("%s: base_url must be non-empty", who)}
	}
	u, err := url.Parse(base)
	if err != nil {
		return []error{fmt.Errorf("%s: base_url %q is not a valid URL: %w", who, base, err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return []error{fmt.Errorf("%s: base_url %q must be an absolute http or https URL", who, base)}
	}
	if u.Host == "" {
		return []error{fmt.Errorf("%s: base_url %q must include a host", who, base)}
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

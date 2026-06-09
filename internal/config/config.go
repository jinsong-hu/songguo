// Package config loads, validates, and watches the file-based vendor config.
//
// Vendors (upstream AI providers) are configured in a YAML file and
// hot-reloaded via fsnotify, so adding a vendor, key, model, or price takes
// effect with no server restart. Consumer tokens are NOT stored here; they
// live in SQLite.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Default capture tuning, applied during normalization when a value is unset
// (zero) or non-positive.
const (
	defaultCaptureMaxBytes = 32768
	defaultCaptureRetain   = 10000
)

// Settings holds gateway-wide options.
type Settings struct {
	Listen string `yaml:"listen"`

	// Capture toggles opt-in request/response body capture. Off by default.
	Capture bool `yaml:"capture"`
	// CaptureMaxBytes caps each stored body (per side). Defaults to 32768.
	CaptureMaxBytes int `yaml:"capture_max_bytes"`
	// CaptureRetain keeps this many most-recent payload rows. Defaults to 10000.
	CaptureRetain int `yaml:"capture_retain"`
}

// Credential is a single upstream API key within a vendor's key pool (号池).
type Credential struct {
	ID     string `yaml:"id"`
	APIKey string `yaml:"api_key"`
}

// Price is the true per-model cost used for metering and cheapest-route.
type Price struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
	Unit   string  `yaml:"unit"` // e.g. per_1m_tokens, per_1k_tokens, per_token, per_call, per_image, per_second, per_char
}

// Vendor is an upstream AI provider.
type Vendor struct {
	Name         string           `yaml:"name"`
	BaseURL      string           `yaml:"base_url"`
	ServedModels []string         `yaml:"served_models"`
	Priority     int              `yaml:"priority"` // lower = preferred; default 0
	Weight       int              `yaml:"weight"`   // weighted round-robin within a priority; normalized to >=1
	Credentials  []Credential     `yaml:"credentials"`
	Prices       map[string]Price `yaml:"prices"`
}

// Config is the root of the YAML document.
type Config struct {
	Settings Settings `yaml:"settings"`
	Vendors  []Vendor `yaml:"vendors"`
}

// Parse unmarshals YAML, normalizes defaults, validates, and builds an
// immutable Snapshot with a precomputed model->vendors index.
func Parse(data []byte) (*Snapshot, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	normalize(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return newSnapshot(cfg), nil
}

// LoadFile reads the file at path and parses it.
func LoadFile(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	snap, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	return snap, nil
}

// isNotExist reports whether err (possibly wrapped) is a "file not found"
// error, distinguishing a missing config from a malformed one.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// normalize applies defaults that should hold regardless of validity.
func normalize(cfg *Config) {
	// Capture caps fall back to sane defaults when unset or non-positive, so a
	// 0/negative value never disables a body to zero length.
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
	seenCredential := make(map[string]string) // credential id -> vendor that first declared it

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
		problems = append(problems, validateCredentials(who, v.Credentials, seenCredential)...)
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

func validateCredentials(who string, creds []Credential, seenCredential map[string]string) []error {
	if len(creds) == 0 {
		return []error{fmt.Errorf("%s: credentials must be non-empty", who)}
	}
	var problems []error
	for ci := range creds {
		c := creds[ci]
		if c.ID == "" {
			problems = append(problems, fmt.Errorf("%s: credential #%d has an empty id", who, ci))
		} else if owner, dup := seenCredential[c.ID]; dup {
			problems = append(problems, fmt.Errorf("%s: credential id %q already used by %s", who, c.ID, owner))
		} else {
			seenCredential[c.ID] = who
		}
		if c.APIKey == "" {
			problems = append(problems, fmt.Errorf("%s: credential %q has an empty api_key", who, c.ID))
		}
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

package config

import (
	"strings"
	"testing"
)

func TestBuildValid(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{
				Name:         "openai-main",
				BaseURL:      "https://api.openai.com/v1",
				ServedModels: []string{"gpt-4o", "gpt-4o-mini", "text-embedding-3-small"},
				Priority:     1,
				Weight:       1,
				Credential:   Credential{ID: "openai-key-1", APIKey: "sk-aaa"},
				Prices: map[string]Price{
					"gpt-4o":                  {Input: 2.50, Output: 10.00, Unit: "per_1m_tokens"},
					"gpt-4o-mini":            {Input: 0.15, Output: 0.60, Unit: "per_1m_tokens"},
					"text-embedding-3-small": {Input: 0.02, Unit: "per_1m_tokens"},
				},
			},
			{
				Name:         "deepseek",
				BaseURL:      "https://api.deepseek.com",
				ServedModels: []string{"deepseek-chat", "gpt-4o"},
				Priority:     2,
				Credential:   Credential{ID: "deepseek-key-1", APIKey: "sk-bbb"},
				Prices: map[string]Price{
					"deepseek-chat": {Input: 0.27, Output: 1.10, Unit: "per_1m_tokens"},
				},
			},
		},
	}

	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}

	if got := len(snap.Vendors()); got != 2 {
		t.Fatalf("len(Vendors) = %d, want 2", got)
	}

	v, ok := snap.Vendor("openai-main")
	if !ok {
		t.Fatal("Vendor(openai-main) not found")
	}
	if v.Priority != 1 {
		t.Errorf("openai-main priority = %d, want 1", v.Priority)
	}

	p, ok := snap.PriceFor("openai-main", "gpt-4o")
	if !ok {
		t.Fatal("PriceFor(openai-main, gpt-4o) not found")
	}
	if p.Input != 2.50 || p.Output != 10.00 || p.Unit != "per_1m_tokens" {
		t.Errorf("gpt-4o price = %+v, unexpected", p)
	}

	// A model with no price entry is allowed.
	if _, ok := snap.PriceFor("deepseek", "gpt-4o"); ok {
		t.Error("expected no price for deepseek/gpt-4o")
	}
}

func TestVendorsForModel_MultiVendor(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{Name: "openai-main", BaseURL: "https://api.openai.com/v1", ServedModels: []string{"gpt-4o"}, Credential: Credential{APIKey: "sk-a"}},
			{Name: "deepseek", BaseURL: "https://api.deepseek.com", ServedModels: []string{"deepseek-chat", "gpt-4o"}, Credential: Credential{APIKey: "sk-b"}},
		},
	}
	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	vs := snap.VendorsForModel("gpt-4o")
	if len(vs) != 2 {
		t.Fatalf("VendorsForModel(gpt-4o) = %d vendors, want 2", len(vs))
	}
	names := map[string]bool{vs[0].Name: true, vs[1].Name: true}
	if !names["openai-main"] || !names["deepseek"] {
		t.Errorf("VendorsForModel(gpt-4o) names = %v, want openai-main + deepseek", names)
	}

	if got := snap.VendorsForModel("deepseek-chat"); len(got) != 1 || got[0].Name != "deepseek" {
		t.Errorf("VendorsForModel(deepseek-chat) = %+v, want [deepseek]", got)
	}
	if got := snap.VendorsForModel("does-not-exist"); got != nil {
		t.Errorf("VendorsForModel(unknown) = %v, want nil", got)
	}
}

func TestSnapshotReturnsCopies(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{Name: "openai-main", BaseURL: "https://api.openai.com/v1", ServedModels: []string{"gpt-4o"}, Credential: Credential{APIKey: "sk-a"},
				Prices: map[string]Price{"gpt-4o": {Input: 2.50, Unit: "per_1m_tokens"}}},
		},
	}
	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	v, _ := snap.Vendor("openai-main")
	v.ServedModels[0] = "mutated"
	v.Prices["gpt-4o"] = Price{Input: 999}

	again, _ := snap.Vendor("openai-main")
	if again.ServedModels[0] == "mutated" {
		t.Error("mutating returned ServedModels leaked into snapshot")
	}
	if again.Prices["gpt-4o"].Input == 999 {
		t.Error("mutating returned Prices leaked into snapshot")
	}
}

func TestEmptyConfigValid(t *testing.T) {
	snap, err := Build(Config{})
	if err != nil {
		t.Fatalf("empty config should be valid, got: %v", err)
	}
	if len(snap.Vendors()) != 0 {
		t.Errorf("expected 0 vendors, got %d", len(snap.Vendors()))
	}
}

func TestWeightNormalization(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}},
			{Name: "b", BaseURL: "https://b.example.com", ServedModels: []string{"m2"}, Weight: 5, Credential: Credential{APIKey: "k"}},
			{Name: "c", BaseURL: "https://c.example.com", ServedModels: []string{"m3"}, Weight: -3, Credential: Credential{APIKey: "k"}},
		},
	}
	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := map[string]int{"a": 1, "b": 5, "c": 1}
	for name, w := range want {
		v, _ := snap.Vendor(name)
		if v.Weight != w {
			t.Errorf("vendor %s weight = %d, want %d", name, v.Weight, w)
		}
	}
}

func TestValidationFailures(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		wantSubs []string
	}{
		{
			name: "duplicate vendor name",
			cfg: Config{Vendors: []Vendor{
				{Name: "dup", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}},
				{Name: "dup", BaseURL: "https://b.example.com", ServedModels: []string{"m2"}, Credential: Credential{APIKey: "k"}},
			}},
			wantSubs: []string{"duplicate vendor name"},
		},
		{
			name:     "missing base_url",
			cfg:      Config{Vendors: []Vendor{{Name: "a", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}}}},
			wantSubs: []string{"base_url must be non-empty"},
		},
		{
			name:     "bad base_url scheme",
			cfg:      Config{Vendors: []Vendor{{Name: "a", BaseURL: "ftp://x", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}}}},
			wantSubs: []string{"absolute http or https"},
		},
		{
			name:     "relative base_url",
			cfg:      Config{Vendors: []Vendor{{Name: "a", BaseURL: "/relative/path", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}}}},
			wantSubs: []string{"absolute http or https"},
		},
		{
			name:     "empty served_models",
			cfg:      Config{Vendors: []Vendor{{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{}, Credential: Credential{APIKey: "k"}}}},
			wantSubs: []string{"served_models must be non-empty"},
		},
		{
			name:     "duplicate model within vendor",
			cfg:      Config{Vendors: []Vendor{{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1", "m1"}, Credential: Credential{APIKey: "k"}}}},
			wantSubs: []string{"duplicate served model"},
		},
		{
			name:     "missing credential",
			cfg:      Config{Vendors: []Vendor{{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}}}},
			wantSubs: []string{"credential api_key must be non-empty"},
		},
		{
			name:     "missing api_key",
			cfg:      Config{Vendors: []Vendor{{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{ID: "k1"}}}},
			wantSubs: []string{"credential api_key must be non-empty"},
		},
		{
			name: "price with empty unit",
			cfg: Config{Vendors: []Vendor{
				{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"},
					Prices: map[string]Price{"m1": {Input: 1.0, Output: 2.0}}},
			}},
			wantSubs: []string{"empty unit"},
		},
		{
			name: "aggregates multiple problems",
			cfg:  Config{Vendors: []Vendor{{Name: "", ServedModels: []string{}}}},
			wantSubs: []string{"name must be non-empty", "base_url must be non-empty", "served_models must be non-empty", "credential api_key must be non-empty"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			msg := err.Error()
			for _, sub := range tc.wantSubs {
				if !strings.Contains(msg, sub) {
					t.Errorf("error %q does not contain %q", msg, sub)
				}
			}
		})
	}
}

func TestCaptureDefaults(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}},
		},
	}
	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := snap.Settings()
	if s.CaptureMaxBytes != 32768 {
		t.Errorf("CaptureMaxBytes = %d, want 32768", s.CaptureMaxBytes)
	}
	if s.CaptureRetain != 10000 {
		t.Errorf("CaptureRetain = %d, want 10000", s.CaptureRetain)
	}
}

func TestAdapterDefault(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{Name: "a", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}},
		},
	}
	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	v, _ := snap.Vendor("a")
	if v.Adapter != AdapterOpenAI {
		t.Errorf("Adapter = %q, want %q", v.Adapter, AdapterOpenAI)
	}
}

func TestCredentialIDDefaultsToVendorName(t *testing.T) {
	cfg := Config{
		Vendors: []Vendor{
			{Name: "my-vendor", BaseURL: "https://a.example.com", ServedModels: []string{"m1"}, Credential: Credential{APIKey: "k"}},
		},
	}
	snap, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	v, _ := snap.Vendor("my-vendor")
	if v.Credential.ID != "my-vendor" {
		t.Errorf("Credential.ID = %q, want %q", v.Credential.ID, "my-vendor")
	}
}

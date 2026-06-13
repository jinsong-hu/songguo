package catalog

import (
	"testing"

	"github.com/songguo/songguo/internal/wire"
)

// TestCatalogLoads parses the embedded catalog and checks structural invariants:
// every endpoint names a registered wire and a non-empty base URL + adapter, and
// every model an endpoint references is defined in the vendor's model map.
func TestCatalogLoads(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Vendors) == 0 {
		t.Fatal("no vendors in catalog")
	}

	knownAdapter := map[string]bool{
		"openai-compatible":    true,
		"anthropic-compatible": true,
		"volc-speech":          true,
	}

	for _, v := range c.Vendors {
		if v.ID == "" || v.Name == "" {
			t.Errorf("vendor with empty id/name: %+v", v)
		}
		if len(v.Endpoints) == 0 {
			t.Errorf("vendor %q has no endpoints", v.ID)
		}
		for _, ep := range v.Endpoints {
			if _, ok := wire.Get(ep.Wire); !ok {
				t.Errorf("vendor %q endpoint references unknown wire %q", v.ID, ep.Wire)
			}
			if ep.BaseURL == "" {
				t.Errorf("vendor %q endpoint %q has empty base_url", v.ID, ep.Wire)
			}
			if !knownAdapter[ep.Adapter] {
				t.Errorf("vendor %q endpoint %q has unknown adapter %q", v.ID, ep.Wire, ep.Adapter)
			}
			for _, m := range ep.Models {
				if _, ok := v.Models[m]; !ok {
					t.Errorf("vendor %q endpoint %q references model %q not in vendor models", v.ID, ep.Wire, m)
				}
			}
		}
		for id, m := range v.Models {
			if m.Unit == "" {
				t.Errorf("vendor %q model %q has empty unit", v.ID, id)
			}
		}
	}
}

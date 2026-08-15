package catalog

import (
	"testing"

	"github.com/songguo/songguo/internal/wire"
)

// TestCatalogLoads parses and merges the embedded files and checks structural
// invariants: every endpoint names a registered wire and a non-empty URL +
// adapter, and every model an endpoint references is defined in the provider's
// model map. That last one is what keeps the generated models.json and the
// hand-written catalog.json from drifting apart — cmd/catalogsync generates
// exactly the models the endpoints declare, so a gap means the sync is stale or
// a hand edit dropped a model an endpoint still serves.
func TestCatalogLoads(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c) == 0 {
		t.Fatal("no providers in catalog")
	}

	knownAdapter := map[string]bool{
		"openai-compatible":    true,
		"anthropic-compatible": true,
		"volc-speech":          true,
	}

	for id, p := range c {
		if id == "" || p.Name == "" {
			t.Errorf("provider with empty id/name: %q %+v", id, p)
		}
		if p.ID != id {
			t.Errorf("provider %q carries id %q; key and id field must agree", id, p.ID)
		}
		if len(p.Endpoints) == 0 {
			t.Errorf("provider %q has no endpoints", id)
		}
		for _, ep := range p.Endpoints {
			if _, ok := wire.Get(ep.Wire); !ok {
				t.Errorf("provider %q endpoint references unknown wire %q", id, ep.Wire)
			}
			if ep.Endpoint == "" {
				t.Errorf("provider %q endpoint %q has empty endpoint URL", id, ep.Wire)
			}
			if !knownAdapter[ep.Adapter] {
				t.Errorf("provider %q endpoint %q has unknown adapter %q", id, ep.Wire, ep.Adapter)
			}
			for _, m := range ep.Models {
				if _, ok := p.Models[m]; !ok {
					t.Errorf("provider %q endpoint %q references model %q not in provider models", id, ep.Wire, m)
				}
			}
		}
		for mid, m := range p.Models {
			if m.ID != mid {
				t.Errorf("provider %q model %q carries id %q; key and id field must agree", id, mid, m.ID)
			}
		}
	}
}

// TestManualWinsOverGenerated pins the merge rule that makes catalog.json the
// place to pin a rate: a model present in both files keeps its hand-written
// cost, and the generated sibling is still kept.
func TestManualWinsOverGenerated(t *testing.T) {
	gen := Catalog{"acme": {ID: "acme", Name: "Generated", Models: map[string]Model{
		"m1": {ID: "m1", Cost: Cost{Input: 1, Output: 2}},
		"m2": {ID: "m2", Cost: Cost{Input: 3, Output: 4}},
	}}}
	man := Catalog{"acme": {ID: "acme", Name: "Acme", Models: map[string]Model{
		"m1": {ID: "m1", Cost: Cost{Input: 99}},
	}}}

	got := merge(gen, man)
	if n := len(got["acme"].Models); n != 2 {
		t.Fatalf("merged models = %d, want 2 (generated m2 kept alongside pinned m1)", n)
	}
	if c := got["acme"].Models["m1"].Cost; c.Input != 99 {
		t.Errorf("m1 cost = %+v, want the hand-written 99", c)
	}
	if c := got["acme"].Models["m2"].Cost; c.Input != 3 {
		t.Errorf("m2 cost = %+v, want the generated 3", c)
	}
	if got["acme"].Name != "Acme" {
		t.Errorf("name = %q, want the hand-written one", got["acme"].Name)
	}
}

// TestMergeKeepsGeneratedIdentityWhenManualIsSilent guards the other direction:
// catalog.json states topology and often says nothing about a provider's name or
// docs, and an empty field there must not blank the generated value.
func TestMergeKeepsGeneratedIdentityWhenManualIsSilent(t *testing.T) {
	gen := Catalog{"acme": {ID: "acme", Name: "Acme Inc", Doc: "https://acme.example/docs"}}
	man := Catalog{"acme": {ID: "acme", Endpoints: []Endpoint{
		{Wire: "openai/chat", Endpoint: "https://x", Adapter: "openai-compatible"},
	}}}

	got := merge(gen, man)["acme"]
	if got.Name != "Acme Inc" || got.Doc != "https://acme.example/docs" {
		t.Errorf("merged identity = %q/%q, want the generated values preserved", got.Name, got.Doc)
	}
	if len(got.Endpoints) != 1 {
		t.Errorf("merged endpoints = %d, want the hand-written 1", len(got.Endpoints))
	}
}

// TestDeclaredModels is what cmd/catalogsync reads to decide what to generate,
// so it must dedupe across endpoints that share a model and skip companion
// wires that serve none.
func TestDeclaredModels(t *testing.T) {
	p := Provider{Endpoints: []Endpoint{
		{Wire: "openai/chat", Models: []string{"a", "b"}},
		{Wire: "openai/responses", Models: []string{"b", "c"}},
		{Wire: "openai/models"},
	}}
	got := p.DeclaredModels()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("DeclaredModels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DeclaredModels() = %v, want %v", got, want)
		}
	}
}

// TestSpeechModelsPriceOnTheirOwnAxes pins the reason Cost was extended beyond
// models.dev's fields: models.dev has no per-character or per-second field of
// any kind, so these entries live only in the hand-written file and must carry a
// media rate. A token rate here would meter $0, since a speech call reports
// characters and seconds and no tokens at all.
func TestSpeechModelsPriceOnTheirOwnAxes(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := c["volcengine-speech"]
	if !ok {
		t.Fatal("volcengine-speech missing from the catalog")
	}
	if len(p.Models) == 0 {
		t.Fatal("volcengine-speech has no models")
	}
	for mid, m := range p.Models {
		if m.Cost.Tokens() {
			t.Errorf("%s prices tokens; speech wires meter characters and seconds", mid)
		}
		if m.Cost.Character == 0 && m.Cost.Second == 0 {
			t.Errorf("%s declares neither a character nor a second rate; it would meter $0", mid)
		}
	}
}

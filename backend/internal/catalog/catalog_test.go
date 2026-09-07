package catalog

import (
	"testing"

	"github.com/songguo/songguo/internal/wire"
)

// TestCatalogLoads parses and merges the embedded files and checks structural
// invariants: every endpoint names a registered wire and a non-empty URL +
// adapter, and every model an endpoint suggests is defined in the provider's
// model map.
//
// That last check changed meaning when cmd/catalogsync started generating every
// model an upstream publishes rather than only the ones the endpoints name. It
// no longer detects a stale SYNC — a model an endpoint omits is now priced
// anyway. It detects a stale catalog.json: an endpoint suggesting a model
// models.dev has retired, which is a dead entry in the dashboard's picker.
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

	canon := make(map[string]map[string]bool, len(c))
	for id, p := range c {
		set := make(map[string]bool, len(p.Models))
		for mid := range p.Models {
			set[CanonicalID(mid)] = true
		}
		canon[id] = set
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
				// Canonically, because the two files legitimately spell a
				// version differently: catalog.json carries the dotted form a
				// client sends, models.json carries models.dev's key. A literal
				// compare would report drift where there is none.
				if _, ok := canon[id][CanonicalID(m)]; !ok {
					t.Errorf("provider %q endpoint %q suggests model %q, which models.dev no longer publishes — drop it from catalog.json", id, ep.Wire, m)
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

// TestCanonicalID pins the rule that lets the two spellings of a model id meet.
func TestCanonicalID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The version dot becomes a dash, which is the whole job.
		{"claude-fable-5.1", "claude-fable-5-1"},
		{"claude-fable-5-1", "claude-fable-5-1"},
		{"gpt-5.6-sol", "gpt-5-6-sol"},
		{"GPT-5.6-Sol", "gpt-5-6-sol"},
		// Twice, because the pattern consumes the digit on either side and a
		// chain would otherwise keep its middle separator.
		{"doubao-seed-2.0.1-pro", "doubao-seed-2-0-1-pro"},
		// A dot NOT between two digits is not a version separator and stays.
		{"gpt-4o.preview", "gpt-4o.preview"},
		// Already-dashed and dotless ids are untouched.
		{"gpt-5-mini", "gpt-5-mini"},
		{"gpt-6-astra", "gpt-6-astra"},
		{"text-embedding-3-small", "text-embedding-3-small"},
	} {
		if got := CanonicalID(tc.in); got != tc.want {
			t.Errorf("CanonicalID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLookupIDsPrefersTheDatedBuild guards the ordering rule: a dated id must
// take its OWN published rate when the upstream lists it, and fall back to the
// undated model only when it does not. Vendors reprice between builds, so the
// reverse order would quietly bill a build at a sibling's rate.
func TestLookupIDsPrefersTheDatedBuild(t *testing.T) {
	got := LookupIDs("claude-haiku-4-5-20251001")
	want := []string{"claude-haiku-4-5-20251001", "claude-haiku-4-5"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("LookupIDs = %v, want %v", got, want)
	}
	// An id with no date pin yields exactly one form — no speculative second
	// lookup that could match something else.
	if got := LookupIDs("gpt-6-astra"); len(got) != 1 || got[0] != "gpt-6-astra" {
		t.Fatalf("LookupIDs(gpt-6-astra) = %v, want [gpt-6-astra]", got)
	}
	// A trailing number that is not 8 digits is a version, not a date pin.
	if got := LookupIDs("qwen3.8-max-0902"); len(got) != 1 {
		t.Fatalf("LookupIDs(qwen3.8-max-0902) = %v, want one form", got)
	}
}

// TestCanonicalIDNeverCollides is the safety net under CanonicalID. Two distinct
// models meeting under the rule would silently merge their prices — one would
// bill at the other's rate with nothing to show for it — which is strictly worse
// than the missed match the rule exists to fix.
//
// It runs over the embedded catalog, which since the generator began taking each
// mapped provider's whole published list is a full copy of what models.dev names
// under those providers. So a future upstream id that collides with one we
// already carry fails here at the next `make catalog-sync`, before it can reach
// a price.
func TestCanonicalIDNeverCollides(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checked := 0
	for id, p := range c {
		seen := make(map[string]string, len(p.Models))
		for mid := range p.Models {
			checked++
			k := CanonicalID(mid)
			if prev, dup := seen[k]; dup {
				t.Errorf("provider %q: %q and %q both canonicalize to %q — their prices would merge", id, prev, mid, k)
				continue
			}
			seen[k] = mid
		}
	}
	if checked == 0 {
		t.Fatal("no models checked — this test is vacuous")
	}
	t.Logf("checked %d model id(s) for canonical collisions", checked)
}

// TestDeclaredModels covers the dashboard's suggested-model list for a preset,
// so it must dedupe across endpoints that share a model and skip companion
// wires that serve none. Pricing no longer reads it — modelsdev.Generate prices
// every model the upstream publishes — so a gap here costs a suggestion, not a
// rate.
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

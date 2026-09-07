package modelsdev

import (
	"testing"

	"github.com/songguo/songguo/internal/catalog"
)

// TestGenerateNeverEmitsAPinnedModel is load-bearing beyond this package.
//
// configsvc.effectivePrice has no rule protecting a hand-pinned rate from the
// price feed, because it does not need one: the feed is built from Generate, and
// Generate skips every model the hand-written catalog.json defines, so a pinned
// model cannot appear in the feed to be overtaken by it. If that ever stops
// holding, a refresh would start silently overwriting rates someone typed on
// purpose — and the only thing standing between those two states is this test.
func TestGenerateNeverEmitsAPinnedModel(t *testing.T) {
	hand, err := catalog.Manual()
	if err != nil {
		t.Fatalf("catalog.Manual: %v", err)
	}

	// An upstream that offers a rate for every model songguo declares AND every
	// one catalog.json pins. The pinned half is the point: since Generate now
	// enumerates the upstream rather than our declarations, a pin is only safe
	// because Generate refuses it, so the fixture has to actually offer it.
	upstream := make(map[string]catalog.Provider)
	for id, p := range hand {
		mdID, mapped := providerFor[id]
		if !mapped {
			continue
		}
		models := make(map[string]catalog.Model)
		for _, m := range p.DeclaredModels() {
			models[m] = catalog.Model{ID: m, Cost: catalog.Cost{Input: 123, Output: 456}}
		}
		for m := range p.Models {
			models[m] = catalog.Model{ID: m, Cost: catalog.Cost{Input: 123, Output: 456}}
		}
		upstream[mdID] = catalog.Provider{ID: mdID, Name: mdID, Models: models}
	}

	generated, _ := Generate(upstream, hand)

	pinnedSeen := 0
	for pid, p := range generated {
		for mid := range p.Models {
			if _, pinned := pinnedAs(hand[pid], mid); pinned {
				t.Errorf("Generate emitted %s/%s, which catalog.json pins — a refresh would overwrite a hand-set rate", pid, mid)
				pinnedSeen++
			}
		}
	}

	// Guard the guard: if nothing is pinned under a mapped provider, the test
	// above passes vacuously and proves nothing.
	pinnable := 0
	for id, p := range hand {
		if _, mapped := providerFor[id]; !mapped {
			continue
		}
		pinnable += len(p.Models)
	}
	if pinnable == 0 {
		t.Fatal("no pinned model under a mapped provider — this test is vacuous, and effectivePrice's missing pin rule is unguarded")
	}
	t.Logf("%d pinned model(s) under mapped providers, %d leaked", pinnable, pinnedSeen)
}

// TestGeneratePricesModelsTheCatalogNeverDeclared is the behaviour this package
// was inverted for: a model nobody wrote into catalog.json still gets its
// published rate.
//
// Before the inversion, Generate iterated the ids on catalog.json's endpoints,
// so a model a vendor shipped after the last hand edit could not be priced at
// all — no matter how promptly models.dev published it. In the deployment that
// prompted this, gpt-6-astra was published at 10/50 and metered at 5/30 because
// it was not in the file, and grok-4.6 metered at zero.
func TestGeneratePricesModelsTheCatalogNeverDeclared(t *testing.T) {
	hand, err := catalog.Manual()
	if err != nil {
		t.Fatalf("catalog.Manual: %v", err)
	}
	// An upstream that publishes a model no endpoint in catalog.json names.
	const fresh = "gpt-6-astra"
	for _, p := range hand["openai"].Endpoints {
		for _, m := range p.Models {
			if m == fresh {
				t.Fatalf("catalog.json declares %s, so this test cannot prove anything; pick an id it does not", fresh)
			}
		}
	}
	upstream := map[string]catalog.Provider{"openai": {
		ID: "openai", Name: "OpenAI",
		Models: map[string]catalog.Model{
			fresh: {ID: fresh, Cost: catalog.Cost{Input: 10, Output: 50}},
		},
	}}

	generated, _ := Generate(upstream, hand)

	got, ok := generated["openai"].Models[fresh]
	if !ok {
		t.Fatalf("Generate dropped %s — an undeclared model cannot be priced, which is the bug this inversion removes", fresh)
	}
	if got.Cost.Input != 10 || got.Cost.Output != 50 {
		t.Errorf("%s priced %+v, want the published 10/50", fresh, got.Cost)
	}
}

// A pin must hold even when the two files spell the version differently —
// otherwise TestGenerateNeverEmitsAPinnedModel's guarantee would cover only the
// ids that happen to match literally.
func TestGenerateRespectsAPinAcrossSpellings(t *testing.T) {
	hand := catalog.Catalog{"zhipu": {
		ID: "zhipu", Name: "Zhipu",
		Models: map[string]catalog.Model{
			"glm-4.5": {ID: "glm-4.5", Cost: catalog.Cost{Input: 1, Output: 2}},
		},
	}}
	upstream := map[string]catalog.Provider{"zhipuai": {
		ID: "zhipuai", Name: "Zhipu",
		Models: map[string]catalog.Model{
			"glm-4-5": {ID: "glm-4-5", Cost: catalog.Cost{Input: 999, Output: 999}},
		},
	}}

	generated, skips := Generate(upstream, hand)

	if _, leaked := generated["zhipu"].Models["glm-4-5"]; leaked {
		t.Error("Generate emitted glm-4-5 alongside the pinned glm-4.5 — the same model twice, and the pin only survives by merge order")
	}
	var reported bool
	for _, s := range skips {
		if s.Provider == "zhipu" && s.Model == "glm-4.5" && s.Reason == "pinned in catalog.json" {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the pin was honoured but not reported; skips = %+v", skips)
	}
}

// A provider with no models.dev counterpart is never generated, which is what
// keeps every Volcengine rate hand-maintained.
func TestGenerateSkipsUnmappedProviders(t *testing.T) {
	hand, err := catalog.Manual()
	if err != nil {
		t.Fatalf("catalog.Manual: %v", err)
	}
	// Offer a rate for everything, under every models.dev provider we map.
	upstream := map[string]catalog.Provider{}
	for _, mdID := range providerFor {
		models := map[string]catalog.Model{}
		for _, p := range hand {
			for _, m := range p.DeclaredModels() {
				models[m] = catalog.Model{ID: m, Cost: catalog.Cost{Input: 1}}
			}
		}
		upstream[mdID] = catalog.Provider{ID: mdID, Name: mdID, Models: models}
	}

	generated, _ := Generate(upstream, hand)

	for _, id := range []string{"volcengine-ark", "volcengine-ark-plan", "volcengine-speech", "custom"} {
		if _, ok := generated[id]; ok {
			t.Errorf("Generate emitted %s, which has no models.dev counterpart", id)
		}
	}
}

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

	// An upstream that offers a rate for EVERY model songguo declares, including
	// the ones catalog.json pins. Generate must still refuse the pinned ones.
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
		upstream[mdID] = catalog.Provider{ID: mdID, Name: mdID, Models: models}
	}

	generated, _ := Generate(upstream, hand)

	pinnedSeen := 0
	for pid, p := range generated {
		for mid := range p.Models {
			if _, pinned := hand[pid].Models[mid]; pinned {
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
		for _, m := range p.DeclaredModels() {
			if _, pinned := p.Models[m]; pinned {
				pinnable++
			}
		}
	}
	if pinnable == 0 {
		t.Fatal("no pinned model under a mapped provider — this test is vacuous, and effectivePrice's missing pin rule is unguarded")
	}
	t.Logf("%d pinned model(s) under mapped providers, %d leaked", pinnable, pinnedSeen)
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

//go:build modelsdev

// This file is behind a build tag because it talks to the network. `make test`
// stays offline and hermetic; run this deliberately with `make catalog-check`.

package modelsdev

import (
	"context"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/catalog"
)

// TestEmbeddedPricesMatchUpstream is the guard the catalog did not have. It
// re-resolves every model models.dev can price and fails when the embedded rate
// disagrees.
//
// Before this existed, gpt-5.6-luna sat in the catalog at 1.00/6.00 against a
// published 0.20/1.20 — a 5x over-bill on every metered call, invisible because
// nothing ever compared the file to reality. gpt-5.6-terra, grok-4.5 and three
// qwen models were wrong the same way.
//
// It checks costs only. Context windows and modalities drift too, but they are
// descriptive: no ledger row is wrong because a context window is stale.
func TestEmbeddedPricesMatchUpstream(t *testing.T) {
	embedded, err := catalog.Load()
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	// Generate must see the hand-written half: against the merged catalog every
	// model reads as already pinned and nothing would be checked.
	hand, err := catalog.Manual()
	if err != nil {
		t.Fatalf("catalog.Manual: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	upstream, err := New().Fetch(ctx)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	generated, skips := Generate(upstream, hand)

	checked := 0
	for _, pid := range sortedKeys(generated) {
		for _, mid := range sortedKeys(generated[pid].Models) {
			want := generated[pid].Models[mid].Cost
			got := embedded[pid].Models[mid].Cost
			checked++
			if !got.Equal(want) {
				t.Errorf("%s/%s: catalog has %+v, models.dev publishes %+v — run `make catalog-sync`",
					pid, mid, got, want)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no model was checked — the provider mapping or models.dev's keys changed")
	}
	t.Logf("checked %d model(s) against models.dev; %d left hand-maintained", checked, len(skips))
}

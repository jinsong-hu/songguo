package pricefeed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type stubFetcher struct {
	providers map[string]catalog.Provider
	err       error
	calls     int
}

func (s *stubFetcher) Fetch(context.Context) (map[string]catalog.Provider, error) {
	s.calls++
	return s.providers, s.err
}

// upstream builds a models.dev-shaped snapshot for the real openai provider,
// carrying the model ids the embedded catalog declares.
func upstream(cost catalog.Cost) map[string]catalog.Provider {
	return map[string]catalog.Provider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]catalog.Model{
			"gpt-5.6-luna": {ID: "gpt-5.6-luna", Cost: cost},
		}},
	}
}

func newTestFeed(t *testing.T, st *store.Store, f Fetcher) *Feed {
	t.Helper()
	fd := New(st, quietLogger(), time.Hour, nil)
	fd.fetcher = f
	return fd
}

// A refresh stores what upstream published, and a later refresh replaces it.
func TestRefreshStoresAndUpdates(t *testing.T) {
	st := openTestStore(t)
	f := &stubFetcher{providers: upstream(catalog.Cost{Input: 0.2, Output: 1.2})}
	fd := newTestFeed(t, st, f)

	fd.refresh(context.Background())
	got, err := st.ListFeedPrices()
	if err != nil {
		t.Fatal(err)
	}
	if c := got["openai"]["gpt-5.6-luna"].Cost; c.Input != 0.2 || c.Output != 1.2 {
		t.Fatalf("stored cost = %+v, want 0.2/1.2", c)
	}

	f.providers = upstream(catalog.Cost{Input: 0.4, Output: 2.4})
	fd.refresh(context.Background())
	got, _ = st.ListFeedPrices()
	if c := got["openai"]["gpt-5.6-luna"].Cost; c.Input != 0.4 {
		t.Fatalf("stored cost = %+v, want the refreshed 0.4", c)
	}
}

// A failed fetch must leave the previous rates in place. Reverting to the
// embedded seed on a network blip would silently change what traffic is billed.
func TestRefreshFailureKeepsLastKnownGood(t *testing.T) {
	st := openTestStore(t)
	f := &stubFetcher{providers: upstream(catalog.Cost{Input: 0.2, Output: 1.2})}
	fd := newTestFeed(t, st, f)
	fd.refresh(context.Background())

	f.err = errors.New("upstream down")
	fd.refresh(context.Background())

	got, _ := st.ListFeedPrices()
	if c := got["openai"]["gpt-5.6-luna"].Cost; c.Input != 0.2 {
		t.Errorf("cost = %+v, want the last known good 0.2 after a failed refresh", c)
	}
}

// An upstream that returns nothing usable is a failure, not an instruction to
// forget every rate.
func TestRefreshEmptyUpstreamKeepsLastKnownGood(t *testing.T) {
	st := openTestStore(t)
	f := &stubFetcher{providers: upstream(catalog.Cost{Input: 0.2, Output: 1.2})}
	fd := newTestFeed(t, st, f)
	fd.refresh(context.Background())

	f.providers = map[string]catalog.Provider{}
	fd.refresh(context.Background())

	got, _ := st.ListFeedPrices()
	if c := got["openai"]["gpt-5.6-luna"].Cost; c.Input != 0.2 {
		t.Errorf("cost = %+v, want the previous rate kept when upstream quotes nothing", c)
	}
}

// A rate that normalizes to an absurd number is an upstream basis slip; it must
// not land. The rest of the refresh still proceeds.
func TestRefreshRejectsImplausibleRates(t *testing.T) {
	st := openTestStore(t)
	fd := newTestFeed(t, st, &stubFetcher{providers: upstream(catalog.Cost{Input: 3_000_000, Output: 3_000_000})})

	fd.refresh(context.Background())

	got, _ := st.ListFeedPrices()
	if _, ok := got["openai"]["gpt-5.6-luna"]; ok {
		t.Error("an implausibly scaled rate was stored")
	}
}

// The feed only ever quotes models songguo declares, through the same mapping
// cmd/catalogsync uses — it cannot introduce a provider or model of its own.
func TestRefreshIgnoresUndeclaredModels(t *testing.T) {
	st := openTestStore(t)
	fd := newTestFeed(t, st, &stubFetcher{providers: map[string]catalog.Provider{
		"openai": {ID: "openai", Name: "OpenAI", Models: map[string]catalog.Model{
			"gpt-5.6-luna":     {ID: "gpt-5.6-luna", Cost: catalog.Cost{Input: 0.2}},
			"some-new-model":   {ID: "some-new-model", Cost: catalog.Cost{Input: 9}},
			"another-unlisted": {ID: "another-unlisted", Cost: catalog.Cost{Input: 9}},
		}},
		// Not mapped to any songguo provider.
		"cerebras": {ID: "cerebras", Name: "Cerebras", Models: map[string]catalog.Model{
			"x": {ID: "x", Cost: catalog.Cost{Input: 1}},
		}},
	}})

	fd.refresh(context.Background())

	got, _ := st.ListFeedPrices()
	if _, ok := got["openai"]["some-new-model"]; ok {
		t.Error("feed quoted a model the catalog does not declare")
	}
	if _, ok := got["cerebras"]; ok {
		t.Error("feed quoted a provider songguo does not map")
	}
	if _, ok := got["openai"]["gpt-5.6-luna"]; !ok {
		t.Error("feed dropped a declared model")
	}
}

// Volcengine has no upstream counterpart, so a refresh must never touch it —
// its rates are hand-maintained and would otherwise be the ones at risk.
func TestRefreshNeverQuotesVolcengine(t *testing.T) {
	st := openTestStore(t)
	fd := newTestFeed(t, st, &stubFetcher{providers: upstream(catalog.Cost{Input: 0.2})})
	fd.refresh(context.Background())

	got, _ := st.ListFeedPrices()
	for id := range got {
		if id == "volcengine-ark" || id == "volcengine-ark-plan" || id == "volcengine-speech" {
			t.Errorf("feed quoted %s, which no upstream carries", id)
		}
	}
}

// A refresh that changes a rate reloads the config; one that changes nothing
// does not, so a daily no-op tick does not churn the live snapshot.
func TestRefreshReloadsOnlyOnChange(t *testing.T) {
	st := openTestStore(t)
	reloads := 0
	fd := New(st, quietLogger(), time.Hour, func() error { reloads++; return nil })
	f := &stubFetcher{providers: upstream(catalog.Cost{Input: 0.2, Output: 1.2})}
	fd.fetcher = f

	fd.refresh(context.Background())
	if reloads != 1 {
		t.Fatalf("reloads = %d after the first refresh, want 1", reloads)
	}
	fd.refresh(context.Background())
	if reloads != 1 {
		t.Errorf("reloads = %d after an unchanged refresh, want it to stay 1", reloads)
	}
	f.providers = upstream(catalog.Cost{Input: 0.4, Output: 1.2})
	fd.refresh(context.Background())
	if reloads != 2 {
		t.Errorf("reloads = %d after a changed refresh, want 2", reloads)
	}
}

// Run refreshes immediately rather than waiting a full interval: a gateway that
// has been down for a month must come back current.
func TestRunRefreshesOnStart(t *testing.T) {
	st := openTestStore(t)
	f := &stubFetcher{providers: upstream(catalog.Cost{Input: 0.2})}
	fd := newTestFeed(t, st, f)

	ctx, cancel := context.WithCancel(context.Background())
	go fd.Run(ctx)

	deadline := time.After(5 * time.Second)
	for {
		got, _ := st.ListFeedPrices()
		if _, ok := got["openai"]["gpt-5.6-luna"]; ok {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run did not refresh on start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	fd.Wait()
}

func TestRatioExceeds(t *testing.T) {
	tests := []struct {
		a, b float64
		want bool
	}{
		{1, 1, false},
		{1, 1.5, false},
		{1, 3, true},
		{3, 1, true},
		{0, 1, true},  // free -> priced is always worth surfacing
		{1, 0, true},  // priced -> free likewise
		{0, 0, false}, // unchanged
	}
	for _, tc := range tests {
		if got := ratioExceeds(tc.a, tc.b, bigMove); got != tc.want {
			t.Errorf("ratioExceeds(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

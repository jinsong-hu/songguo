// Package pricefeed keeps model prices current without anyone remembering to
// run anything. It refreshes from models.dev on startup and on a fixed clock,
// stores the result, and reloads the config so new calls meter at the new rate.
//
// It exists because the alternative did not work. cmd/catalogsync regenerates
// the embedded catalog, but a generator only runs when a human runs it, and the
// prices it maintains had silently drifted 5x on a frontier model before anyone
// noticed. A build-time seed plus a running refresh is the combination: the seed
// makes a fresh checkout and an air-gapped install correct on first boot, and
// the refresh makes a long-lived deployment stay correct.
//
// # Why this is safe to do automatically
//
// Cost is computed at call time and persisted to calls.cost. A refresh can only
// affect FUTURE calls; it can never rewrite a ledger row. That is what makes an
// automatic rate change different from an automatic retry — there is no past
// state to revise, and the row keeps saying what it was billed when it was
// billed.
//
// It still refuses to go quiet about it. Every rate that moves is logged, and
// the resolved price carries config.PriceSourceFeed so GET /api/pricing names
// the feed rather than presenting the number as though it were published in the
// embedded catalog.
//
// # What it will not do
//
//   - Overwrite a hand-pinned rate. catalog.json wins over the feed exactly as
//     it wins over the generated models.json — and it wins because
//     configsvc.effectivePrice reads the hand-written file FIRST, not merely
//     because Generate declines to emit the model. The difference is the deploy
//     that introduces a pin, when the store still holds the row from before it:
//     resting on the absence left that row in charge, at the rate the pin exists
//     to replace.
//   - Overwrite an operator's price_override. That is checked first and never
//     consults the feed at all.
//   - Invent a rate for a model the feed does not carry, which is every
//     Volcengine model and everything billed per character, second or call.
//   - Accept an implausibly scaled rate, so an upstream unit slip cannot land.
//   - Zero anything out on failure. An unreachable feed keeps the last stored
//     refresh; only a successful fetch replaces it.
package pricefeed

import (
	"context"
	"log/slog"
	"time"

	"github.com/songguo/songguo/internal/catalog"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/modelsdev"
	"github.com/songguo/songguo/internal/store"
)

// DefaultInterval is how often prices are re-read. Published rates move on the
// order of weeks, so this is about bounding staleness, not tracking a market.
const DefaultInterval = 24 * time.Hour

// bigMove is the factor beyond which a rate change is logged at WARN rather than
// INFO. It is a reporting threshold and deliberately NOT a limit: the 5x
// correction this package exists to catch would have been suppressed by any rule
// that refused large moves, which is precisely the bug we started with. Surface
// it loudly; never swallow it.
const bigMove = 2.0

// Fetcher reads upstream prices. Swapped in tests.
type Fetcher interface {
	Fetch(ctx context.Context) (map[string]catalog.Provider, error)
}

// Feed refreshes stored prices on a clock.
type Feed struct {
	store    *store.Store
	fetcher  Fetcher
	logger   *slog.Logger
	interval time.Duration
	// reload rebuilds the live config snapshot so a new rate takes effect
	// without a restart. Nil is allowed (tests); a refresh then just persists.
	reload func() error
	now    func() time.Time
	done   chan struct{}
}

// New builds a Feed. A non-positive interval defaults to DefaultInterval; pass
// 0 to Run's caller instead if the feed should not run at all.
func New(st *store.Store, logger *slog.Logger, interval time.Duration, reload func() error) *Feed {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Feed{
		store: st, fetcher: modelsdev.New(), logger: logger,
		interval: interval, reload: reload,
		now: time.Now, done: make(chan struct{}),
	}
}

// Run refreshes once immediately, then on every tick, until ctx is cancelled.
// Refreshing on start is the point: a gateway that has been down for a month
// comes back with current rates rather than waiting a full interval.
func (f *Feed) Run(ctx context.Context) {
	defer close(f.done)
	f.refresh(ctx)
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.refresh(ctx)
		}
	}
}

// Wait blocks until Run has returned, so a caller that cancels the context can
// be sure no write is still in flight before it closes the store. It blocks
// forever if Run was never started; pair the two.
func (f *Feed) Wait() { <-f.done }

// refresh performs one cycle. Every failure path leaves the stored prices alone
// and logs — a price feed must never be able to take a gateway's metering down.
func (f *Feed) refresh(ctx context.Context) {
	hand, err := catalog.Manual()
	if err != nil {
		f.logger.Error("price feed: cannot read the catalog; keeping stored prices", "err", err)
		return
	}

	upstream, err := f.fetcher.Fetch(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a failure
		}
		f.logger.Warn("price feed: refresh failed; keeping the last known good prices", "err", err)
		return
	}

	// Generate applies the same provider mapping and model selection as
	// cmd/catalogsync, so the feed can never quote a model songguo does not
	// declare, nor a provider with no first-party upstream.
	generated, _ := modelsdev.Generate(upstream, hand)

	previous, err := f.store.ListFeedPrices()
	if err != nil {
		f.logger.Warn("price feed: cannot read stored prices; refresh skipped", "err", err)
		return
	}

	at := f.now()
	var prices []store.FeedPrice
	moved, rejected := 0, 0
	for _, pid := range sortedKeys(generated) {
		for _, mid := range sortedKeys(generated[pid].Models) {
			cost := generated[pid].Models[mid].Cost
			if implausible(cost) {
				// An upstream basis slip would otherwise land as a real rate.
				f.logger.Warn("price feed: rejecting an implausibly scaled rate",
					"provider", pid, "model", mid, "input", cost.Input, "output", cost.Output)
				rejected++
				continue
			}
			if before, ok := previous[pid][mid]; !ok || !before.Cost.Equal(cost) {
				f.report(pid, mid, before.Cost, cost, ok)
				moved++
			}
			prices = append(prices, store.FeedPrice{ProviderID: pid, Model: mid, Cost: cost, FetchedAt: at})
		}
	}
	// A model that LEFT the set is a change too, and the loop above cannot see
	// it — that loop only walks what was just generated. The case that matters is
	// a rate becoming hand-pinned: Generate then skips the model, ReplaceFeedPrices
	// drops its row below, and configsvc has to rebuild for the pin to reach
	// traffic. Counting only rewrites left moved at zero on exactly the deploy
	// that introduces a pin, so the reload was skipped and the gateway kept
	// billing the superseded rate until some unrelated config write happened.
	dropped := countDropped(previous, generated)
	moved += dropped

	if len(prices) == 0 {
		f.logger.Warn("price feed: upstream quoted nothing; keeping the last known good prices",
			"rejected", rejected)
		return
	}
	if err := f.store.ReplaceFeedPrices(prices, at); err != nil {
		f.logger.Error("price feed: cannot store prices; keeping the previous set", "err", err)
		return
	}

	f.logger.Info("price feed refreshed",
		"models", len(prices), "changed", moved, "dropped", dropped, "rejected", rejected)
	if moved == 0 || f.reload == nil {
		return
	}
	// Rates only reach traffic through a config rebuild; without this the new
	// numbers would sit in the store until the next unrelated config write.
	if err := f.reload(); err != nil {
		f.logger.Error("price feed: stored new prices but the config reload failed", "err", err)
	}
}

// countDropped reports how many stored rates the new snapshot no longer carries.
//
// It counts rather than logs each one: the ordinary reason a model disappears is
// that an upstream retired it, which is not news, and the reason that IS news —
// a rate becoming hand-pinned — is already visible in the diff of catalog.json
// that caused it. What matters here is only that the number is non-zero, so the
// config rebuild happens.
func countDropped(previous map[string]map[string]store.FeedPrice, generated catalog.Catalog) int {
	n := 0
	for pid, models := range previous {
		for mid := range models {
			if _, ok := generated[pid].Models[mid]; !ok {
				n++
			}
		}
	}
	return n
}

// report logs one rate change. A large move is worth an operator's attention —
// it is either a real repricing or a mistake upstream — so it is escalated,
// never hidden.
func (f *Feed) report(provider, model string, before, after catalog.Cost, had bool) {
	if !had {
		f.logger.Info("price feed: new model priced",
			"provider", provider, "model", model, "input", after.Input, "output", after.Output)
		return
	}
	args := []any{
		"provider", provider, "model", model,
		"input", before.Input, "new_input", after.Input,
		"output", before.Output, "new_output", after.Output,
	}
	if ratioExceeds(before.Input, after.Input, bigMove) || ratioExceeds(before.Output, after.Output, bigMove) {
		f.logger.Warn("price feed: large rate change", args...)
		return
	}
	f.logger.Info("price feed: rate changed", args...)
}

// ratioExceeds reports whether a and b differ by more than factor, in either
// direction. A move away from zero always counts: going from free to priced is
// exactly the kind of change worth surfacing.
func ratioExceeds(a, b, factor float64) bool {
	if a == b {
		return false
	}
	if a <= 0 || b <= 0 {
		return true
	}
	if a > b {
		a, b = b, a
	}
	return b/a > factor
}

// implausible reuses the config-side bound so the feed and an operator's typed
// rate are held to the same standard.
func implausible(c catalog.Cost) bool {
	return config.PriceRateImplausible(config.Price{Cost: c})
}

func sortedKeys[V any](m map[string]V) []string {
	return config.SortedKeys(m)
}

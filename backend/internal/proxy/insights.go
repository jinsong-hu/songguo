package proxy

import (
	"log/slog"
	"sync"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

// The insights fork runs derived-data updates OFF the request hot path (see
// docs/arch-insights.md). The gateway finalizes a call, then hands the record
// here and moves on — this layer never blocks or fails a forward. Today it
// maintains the materialized `sessions` write-through cache; more derived views
// can hang off the same fork.
//
// It is best-effort by construction: a saturated queue drops the update (the
// call's own ledger row is already durable), and any store error is logged and
// swallowed. A dropped update is a permanently slightly-stale session rollup —
// the accepted price of never recomputing (see arch-insights.md), not an
// incident.

type insightsFork struct {
	entries chan sessionInsight
	store   *store.Store
	logger  *slog.Logger
	wg      sync.WaitGroup
}

type sessionInsight struct {
	entry calls.Entry
	title string
}

const (
	defaultInsightsWorkers = 1
	defaultInsightsQueue   = 512
)

// newInsightsFork starts the worker pool. A single worker serializes session
// upserts, which keeps the per-session accumulation race-free without row locks;
// the queue is generous so a burst is buffered rather than dropped.
func newInsightsFork(st *store.Store, logger *slog.Logger, workers, queue int) *insightsFork {
	if logger == nil {
		logger = slog.Default()
	}
	if workers <= 0 {
		workers = defaultInsightsWorkers
	}
	if queue <= 0 {
		queue = defaultInsightsQueue
	}
	f := &insightsFork{entries: make(chan sessionInsight, queue), store: st, logger: logger}
	f.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go f.worker()
	}
	return f
}

func (f *insightsFork) worker() {
	defer f.wg.Done()
	for insight := range f.entries {
		f.process(insight)
	}
}

// submit hands a finalized call to the fork without ever blocking the caller. A
// full queue means insights is backed up; the update is dropped (and logged)
// rather than slowing the gateway.
func (f *insightsFork) submit(e calls.Entry, title string) {
	if f == nil {
		return
	}
	select {
	case f.entries <- sessionInsight{entry: e, title: title}:
	default:
		f.logger.Warn("insights queue full; dropping session update", "call_id", e.ID, "session_id", e.SessionID)
	}
}

func (f *insightsFork) process(insight sessionInsight) {
	e := insight.entry
	// Session-less traffic has no rollup; UpsertSessionCall no-ops on an empty
	// session id, but skip the call entirely to avoid the round trip.
	if e.SessionID == "" {
		return
	}
	if err := f.store.UpsertSessionCall(e, insight.title); err != nil {
		f.logger.Error("session upsert failed", "err", err, "call_id", e.ID, "session_id", e.SessionID)
	}
}

// Close stops accepting updates and drains in-flight ones. Invoked on shutdown;
// tests use it as a drain barrier.
func (f *insightsFork) Close() {
	if f == nil {
		return
	}
	close(f.entries)
	f.wg.Wait()
}

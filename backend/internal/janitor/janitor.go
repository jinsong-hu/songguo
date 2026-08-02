// Package janitor prunes derived and captured data on a fixed clock. It is the
// server-side housekeeping half of the retention policy in docs/arch.md, and
// runs entirely off the gateway hot path — a slow or failing prune never affects
// forwarding.
package janitor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/songguo/songguo/internal/store"
)

// Windows holds the three retention horizons. Zero means "never prune this
// tier", so a misconfigured/zeroed window fails safe (keeps data) rather than
// deleting everything.
type Windows struct {
	Raw      time.Duration // captured bodies (raw table)
	Calls    time.Duration // call-level stats (calls table; cascades to children)
	Sessions time.Duration // materialized session rollups (sessions table)
}

// DefaultWindows is the policy from docs/arch.md: raw 7d, calls 90d, sessions 90d.
var DefaultWindows = Windows{
	Raw:      7 * 24 * time.Hour,
	Calls:    90 * 24 * time.Hour,
	Sessions: 90 * 24 * time.Hour,
}

// Janitor periodically prunes the store.
type Janitor struct {
	store    *store.Store
	logger   *slog.Logger
	windows  Windows
	interval time.Duration
	now      func() time.Time
	done     chan struct{}
}

// New builds a Janitor. A non-positive interval defaults to hourly.
func New(st *store.Store, logger *slog.Logger, w Windows, interval time.Duration) *Janitor {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &Janitor{
		store: st, logger: logger, windows: w, interval: interval,
		now: time.Now, done: make(chan struct{}),
	}
}

// Run prunes once immediately, then on each interval tick until ctx is done. It
// blocks, so callers run it in a goroutine. Each sweep logs what it removed;
// errors are logged and the loop continues (a transient DB error must not stop
// future sweeps).
func (j *Janitor) Run(ctx context.Context) {
	defer close(j.done)
	j.sweep(ctx)
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.sweep(ctx)
		}
	}
}

// Wait blocks until Run has returned, so a caller that cancels the context can
// be sure no DELETE is still in flight before it closes the store. Closing the
// database underneath a running sweep is the race this exists to remove.
//
// It blocks forever if Run was never started; pair the two.
func (j *Janitor) Wait() { <-j.done }

// sweep runs all three prunes once. Order matters only for logging: raw first
// (shortest window), then calls (whose cascade also drops any straggler raw),
// then sessions.
//
// ctx cancellation stops a prune part-way. That is not an error worth
// reporting — the rows that survive are simply pruned by the next sweep — so
// cancellation is separated from a genuine DB failure here.
func (j *Janitor) sweep(ctx context.Context) {
	now := j.now()
	report := func(what string, window time.Duration, n int64, err error) {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			if n > 0 {
				j.logger.Info("prune interrupted by shutdown", "what", what, "rows", n)
			}
		case err != nil:
			j.logger.Error("prune failed", "what", what, "err", err)
		case n > 0:
			j.logger.Info("pruned", "what", what, "rows", n, "older_than", window.String())
		}
	}
	if j.windows.Raw > 0 {
		n, err := j.store.PruneRaw(ctx, now.Add(-j.windows.Raw))
		report("raw bodies", j.windows.Raw, n, err)
	}
	if j.windows.Calls > 0 {
		n, err := j.store.PruneCalls(ctx, now.Add(-j.windows.Calls))
		report("calls", j.windows.Calls, n, err)
	}
	if j.windows.Sessions > 0 {
		n, err := j.store.PruneSessions(ctx, now.Add(-j.windows.Sessions))
		report("sessions", j.windows.Sessions, n, err)
	}
}

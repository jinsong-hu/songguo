// Package janitor prunes derived and captured data on a fixed clock. It is the
// server-side housekeeping half of the retention policy in docs/arch.md, and
// runs entirely off the gateway hot path — a slow or failing prune never affects
// forwarding.
package janitor

import (
	"context"
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
}

// New builds a Janitor. A non-positive interval defaults to hourly.
func New(st *store.Store, logger *slog.Logger, w Windows, interval time.Duration) *Janitor {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &Janitor{store: st, logger: logger, windows: w, interval: interval, now: time.Now}
}

// Run prunes once immediately, then on each interval tick until ctx is done. It
// blocks, so callers run it in a goroutine. Each sweep logs what it removed;
// errors are logged and the loop continues (a transient DB error must not stop
// future sweeps).
func (j *Janitor) Run(ctx context.Context) {
	j.sweep()
	t := time.NewTicker(j.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.sweep()
		}
	}
}

// sweep runs all three prunes once. Order matters only for logging: raw first
// (shortest window), then calls (whose cascade also drops any straggler raw),
// then sessions.
func (j *Janitor) sweep() {
	now := j.now()
	if j.windows.Raw > 0 {
		if n, err := j.store.PruneRaw(now.Add(-j.windows.Raw)); err != nil {
			j.logger.Error("prune raw failed", "err", err)
		} else if n > 0 {
			j.logger.Info("pruned raw bodies", "rows", n, "older_than", j.windows.Raw.String())
		}
	}
	if j.windows.Calls > 0 {
		if n, err := j.store.PruneCalls(now.Add(-j.windows.Calls)); err != nil {
			j.logger.Error("prune calls failed", "err", err)
		} else if n > 0 {
			j.logger.Info("pruned calls", "rows", n, "older_than", j.windows.Calls.String())
		}
	}
	if j.windows.Sessions > 0 {
		if n, err := j.store.PruneSessions(now.Add(-j.windows.Sessions)); err != nil {
			j.logger.Error("prune sessions failed", "err", err)
		} else if n > 0 {
			j.logger.Info("pruned sessions", "rows", n, "older_than", j.windows.Sessions.String())
		}
	}
}

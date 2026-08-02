// Package spend tracks how much each user has spent, as a running total rather
// than as a query over the call log.
//
// Budget enforcement needs one number per user, not a row per call. That
// distinction matters for three separate reasons:
//
//   - It is O(users), not O(calls). A few dozen floats, which do not grow with
//     traffic and never need to queue behind anything. The gateway can charge a
//     call the instant it finishes without touching the database — the point of
//     the ledger fork next door.
//   - SUM(cost) over the call log gets slower every day, and it ran on the
//     request path for every budgeted user plus nine more times across the
//     admin API. A running total is one indexed row.
//   - Most importantly, SUM(cost) was quietly WRONG. Retention prunes calls
//     older than 90 days, so a user's lifetime spend silently decreased over
//     time and their budget refilled on its own. A running total is immune,
//     because it does not live in the rows being deleted.
//
// # Why nothing here derives from the call log
//
// The obvious design — seed a user's total from SUM(cost) on first use — has a
// double-counting race: a charge already added in memory may or may not have
// reached the calls table yet, so the seed either misses it or counts it twice
// and there is no way to tell which. So the call log is consulted exactly once,
// by a one-time backfill when the user_spend table is created (see
// store.migrate). After that, user_spend is the only source: an unknown user
// starts at zero, and every charge is an increment.
//
// # Consistency and durability
//
// The in-memory total is authoritative while the process runs; user_spend is
// where it survives a restart. Flush is driven by the ledger writer, so spend
// reaches disk on the same cadence as the call rows it came from.
//
// A crash loses at most the increments since the last flush. That is a real
// weakening versus deriving the number from committed rows, and it is the price
// of keeping the request path off the database. The exposure is seconds.
package spend

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Store is the persistence this package needs.
type Store interface {
	// LoadSpend returns a user's stored running total, and whether a row
	// existed at all. A missing row means a user with no recorded spend, which
	// is zero — not an error.
	LoadSpend(userID string) (float64, bool, error)
	// SaveSpend writes a user's running total.
	SaveSpend(userID string, total float64) error
}

// Tracker holds running totals in memory, backed by Store.
//
// total is what has been persisted (plus whatever Flush has folded in); pending
// is the increments not yet written. They are kept apart so that a charge is
// never lost to a concurrent flush, and so Add never has to consult the
// database.
type Tracker struct {
	mu      sync.Mutex
	total   map[string]float64
	pending map[string]float64
	store   Store
	logger  *slog.Logger
	done    chan struct{}
}

// New builds an empty Tracker. Totals load lazily, per user, on first use —
// there is no startup scan, so a large user table does not delay boot.
func New(st Store, logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{
		total:   make(map[string]float64),
		pending: make(map[string]float64),
		store:   st,
		logger:  logger,
		done:    make(chan struct{}),
	}
}

// Add charges a call to a user. Called on the request goroutine the moment a
// call is finalized, deliberately BEFORE the ledger row is queued: the cost
// must not sit behind a backlog, or a budget check moments later would read a
// stale total and let the user through.
//
// It only takes a mutex — no database, no allocation, no blocking.
func (t *Tracker) Add(userID string, delta float64) {
	if t == nil || userID == "" || delta == 0 {
		return
	}
	t.mu.Lock()
	t.pending[userID] += delta
	t.mu.Unlock()
}

// Get returns a user's spend: what has been persisted plus what is still
// pending. Loading a user costs one indexed row read, once per process.
func (t *Tracker) Get(userID string) (float64, error) {
	if t == nil || userID == "" {
		return 0, nil
	}
	t.mu.Lock()
	base, loaded := t.total[userID]
	pending := t.pending[userID]
	t.mu.Unlock()
	if loaded {
		return base + pending, nil
	}

	// Load outside the lock: holding the mutex across a database read would
	// stall every other user's budget check behind one cold row. A concurrent
	// load of the same user is harmless — both read the same value.
	stored, err := t.load(userID)
	if err != nil {
		return 0, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if base, loaded := t.total[userID]; loaded {
		return base + t.pending[userID], nil // someone loaded it meanwhile
	}
	t.total[userID] = stored
	return stored + t.pending[userID], nil
}

// load reads the persisted total. A user with no row has simply never spent
// anything; the call log is deliberately not consulted (see the package doc).
func (t *Tracker) load(userID string) (float64, error) {
	total, ok, err := t.store.LoadSpend(userID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	return total, nil
}

// Flush persists every total with pending increments. Driven by the ledger
// writer, so it runs on a background goroutine and its database work is off the
// request path.
//
// A failed write leaves the increment pending rather than dropping it, so the
// next flush retries. Increments that arrive mid-flush stay pending too: the
// amount written is subtracted, never zeroed.
func (t *Tracker) Flush() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	ids := make([]string, 0, len(t.pending))
	for id, d := range t.pending {
		if d != 0 {
			ids = append(ids, id)
		}
	}
	t.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}

	var firstErr error
	for _, id := range ids {
		t.mu.Lock()
		base, loaded := t.total[id]
		t.mu.Unlock()
		if !loaded {
			stored, err := t.load(id)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue // stays pending; retried next flush
			}
			t.mu.Lock()
			if _, raced := t.total[id]; !raced {
				t.total[id] = stored
			}
			base = t.total[id]
			t.mu.Unlock()
		}

		t.mu.Lock()
		delta := t.pending[id]
		t.mu.Unlock()

		if err := t.store.SaveSpend(id, base+delta); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue // stays pending
		}

		t.mu.Lock()
		t.total[id] = base + delta
		// Subtract exactly what was written; anything Add contributed while the
		// write was in flight remains pending for the next round.
		if t.pending[id] -= delta; t.pending[id] == 0 {
			delete(t.pending, id)
		}
		t.mu.Unlock()
	}
	return firstErr
}

// DefaultFlushInterval bounds how much spend a crash can lose. Short, because
// the work is proportional to the number of users charged since the last tick —
// usually zero rows — not to traffic.
const DefaultFlushInterval = 2 * time.Second

// Run flushes on a ticker until ctx is done, then flushes once more so a clean
// shutdown persists everything. It blocks; callers run it in a goroutine and
// pair it with Wait, exactly like the retention janitor.
func (t *Tracker) Run(ctx context.Context, every time.Duration) {
	defer close(t.done)
	if every <= 0 {
		every = DefaultFlushInterval
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := t.Flush(); err != nil {
				t.logger.Error("final spend flush failed", "err", err)
			}
			return
		case <-tick.C:
			if err := t.Flush(); err != nil {
				t.logger.Error("spend flush failed", "err", err)
			}
		}
	}
}

// Wait blocks until Run has returned, so a caller that cancels the context can
// be sure the final flush has landed before it closes the store.
//
// It blocks forever if Run was never started; pair the two.
func (t *Tracker) Wait() { <-t.done }

// Forget drops a user's cached total so the next Get re-reads it. Used when a
// user is deleted, and as the repair path if a total is ever edited underneath
// the process.
func (t *Tracker) Forget(userID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.total, userID)
	delete(t.pending, userID)
}

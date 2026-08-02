package store

import (
	"context"
	"fmt"
	"time"
)

// Retention prunes derived and captured data on a fixed clock (see
// docs/arch.md). It is analysis-side housekeeping — never on the gateway hot
// path — and each prune deletes rows older than a cutoff on a timestamp column.
// There is no VACUUM: freed pages are reused by new inserts, so the database
// plateaus rather than shrinks (a periodic full vacuum would lock the DB and
// churn disk for a steady-state gateway).
//
// Every prune runs in BATCHES rather than as one statement. SQLite has a single
// write lock for the whole database, and a DELETE holds it for the duration of
// its implicit transaction — so an unbounded prune blocks every ledger write
// for as long as it takes, which on a first sweep against an old database or
// after someone shortens a retention window is a long time. Batching bounds the
// hold to one chunk and lets live traffic interleave. The cost is that a prune
// is no longer atomic; a cancelled sweep leaves some rows behind and the next
// sweep finishes the job, which for retention is a non-issue.

// pruneBatch is how many rows one DELETE removes before releasing the write
// lock. Large enough that pruning stays cheap per statement, small enough that
// a proxied request never waits long behind it.
const pruneBatch = 2000

// pruneOlderThan deletes rows of table whose tsCol predates the cutoff, a batch
// at a time, and returns the total removed. It stops early — returning what it
// has already deleted, plus ctx.Err() — when the caller cancels.
//
// The LIMIT rides on a rowid subquery rather than `DELETE ... LIMIT`, which is
// only available when SQLite is built with SQLITE_ENABLE_UPDATE_DELETE_LIMIT
// and is therefore not portable across drivers.
func (s *Store) pruneOlderThan(ctx context.Context, table, tsCol string, before time.Time) (int64, error) {
	stmt := fmt.Sprintf(
		`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s < ? LIMIT ?)`,
		table, table, tsCol)

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, stmt, before.UnixMilli(), pruneBatch)
		if err != nil {
			return total, fmt.Errorf("store: prune %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			// Cannot tell how far we got; stop rather than risk looping forever.
			return total, nil
		}
		total += n
		// A short batch means the last matching row is gone.
		if n < pruneBatch {
			return total, nil
		}
	}
}

// PruneRaw deletes captured request/response bodies (the raw table) older than
// the cutoff, by capture time. The shortest-lived tier — bodies are large and
// only needed for recent debugging/parse. Returns rows deleted.
func (s *Store) PruneRaw(ctx context.Context, before time.Time) (int64, error) {
	return s.pruneOlderThan(ctx, "raw", "created_at", before)
}

// PruneCalls deletes call-level stats rows older than the cutoff, by start time
// (ts). Foreign-key cascade drops each pruned call's raw/parsed/composition
// children, so this also reclaims any raw bodies the 7-day PruneRaw hasn't
// already removed. Returns rows deleted.
//
// The cascade makes this the most expensive of the three — each deleted call
// takes up to three child rows with it, one of them holding the captured bodies
// — which is the main reason the batching above exists.
func (s *Store) PruneCalls(ctx context.Context, before time.Time) (int64, error) {
	return s.pruneOlderThan(ctx, "calls", "ts", before)
}

// PruneSessions deletes materialized session rollups whose last activity is
// older than the cutoff (last_ts). Because the rollup is never recomputed (see
// docs/arch-insights.md), this is final: an aged-out session is gone, not
// rebuilt. Returns rows deleted.
func (s *Store) PruneSessions(ctx context.Context, before time.Time) (int64, error) {
	return s.pruneOlderThan(ctx, "sessions", "last_ts", before)
}

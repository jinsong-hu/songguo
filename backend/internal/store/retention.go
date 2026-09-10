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
//
// Batching alone is NOT sufficient, and reading it as sufficient is the trap. It
// bounds how many rows one statement DELETES; it does nothing about how long that
// statement takes to FIND them. The LIMIT subquery is evaluated inside the same
// write transaction, so an unindexed cutoff column means a full table scan under
// the write lock — and the LIMIT does not even cap that, because a sweep with
// fewer expired rows than pruneBatch never fills its limit and therefore scans to
// the end of the table. Every prune below is index-backed on its cutoff column,
// and that is a correctness requirement rather than an optimization:
// idx_raw_created_at, idx_calls_ts, idx_sessions_last_ts. A prune added later
// without one reintroduces the outage described next.
//
// > History: raw.created_at had no index until 2026-08-14, while calls.ts and
// > sessions.last_ts did. raw is the captured-bodies table — on the production
// > gateway, 16.5k rows averaging 1.8 MB each, ~30 GB — so every hourly sweep
// > full-scanned tens of GB while holding the write lock, ~9 minutes at a time,
// > stepping past each blob to reach created_at (the last declared column). Every
// > other writer in that window exhausted its 5s busy_timeout and failed
// > SQLITE_BUSY: 1737 failures in 23h, mostly swallowed ledger and spend writes.
// > The visible symptom was an operator getting "internal error" when adding a
// > provider. TestPruneQueriesAreIndexBacked is the regression guard.

// pruneBatch is how many metadata rows one DELETE removes before releasing the
// write lock. raw gets its own much smaller batch because one captured body can
// be megabytes: deleting only a few hundred production rows held the writer for
// 15–25 seconds even with the cutoff index present.
const (
	pruneBatch    = 2000
	pruneRawBatch = 16

	// A full batch means more work is immediately available. Pause after
	// releasing writeMu so a queued ledger/spend/config write acquires the gate
	// before retention starts the next transaction.
	pruneYield = time.Millisecond
)

// pruneStmt builds the batched delete for one prune tier. It exists as its own
// function so TestPruneQueriesAreIndexBacked can run EXPLAIN QUERY PLAN over the
// statement this package actually executes, rather than over a copy of it in the
// test that could drift.
//
// The LIMIT rides on a rowid subquery rather than `DELETE ... LIMIT`, which is
// only available when SQLite is built with SQLITE_ENABLE_UPDATE_DELETE_LIMIT
// and is therefore not portable across drivers.
func pruneStmt(table, tsCol string) string {
	return fmt.Sprintf(
		`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s < ? LIMIT ?)`,
		table, table, tsCol)
}

// pruneOlderThan deletes rows of table whose tsCol predates the cutoff, a batch
// at a time, and returns the total removed. It stops early — returning what it
// has already deleted, plus ctx.Err() — when the caller cancels.
func (s *Store) pruneOlderThan(ctx context.Context, table, tsCol string, before time.Time, batch int) (int64, error) {
	stmt := pruneStmt(table, tsCol)

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		s.writeMu.Lock()
		res, err := s.db.ExecContext(ctx, stmt, before.UnixMilli(), batch)
		s.writeMu.Unlock()
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
		if n < int64(batch) {
			return total, nil
		}
		t := time.NewTimer(pruneYield)
		select {
		case <-ctx.Done():
			if !t.Stop() {
				<-t.C
			}
			return total, ctx.Err()
		case <-t.C:
		}
	}
}

// PruneRaw deletes captured request/response bodies (the raw table) older than
// the cutoff, by capture time. The shortest-lived tier — bodies are large and
// only needed for recent debugging/parse. Returns rows deleted.
func (s *Store) PruneRaw(ctx context.Context, before time.Time) (int64, error) {
	return s.pruneOlderThan(ctx, "raw", "created_at", before, pruneRawBatch)
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
	return s.pruneOlderThan(ctx, "calls", "ts", before, pruneBatch)
}

// PruneSessions deletes materialized session rollups whose last activity is
// older than the cutoff (last_ts). Because the rollup is never recomputed (see
// docs/arch-insights.md), this is final: an aged-out session is gone, not
// rebuilt. Returns rows deleted.
func (s *Store) PruneSessions(ctx context.Context, before time.Time) (int64, error) {
	return s.pruneOlderThan(ctx, "sessions", "last_ts", before, pruneBatch)
}

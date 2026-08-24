package store

import (
	"context"
	"fmt"
)

// The query planner chooses between indexes using sqlite_stat1, and without it
// it guesses from a fixed heuristic. `calls` carries six indexes — ts, user_id,
// model, vendor, status, session_id — and several of them are terrible for a
// filtered query (status has a handful of distinct values). A planner with no
// statistics can pick one of those over idx_calls_ts and turn a bounded range
// scan into most of the table, and that misjudgment gets worse as the table
// grows, which is the shape of "it got slower as we accumulated data".
//
// The production database had no sqlite_stat1 at all: ANALYZE had never run in
// the database's lifetime.
//
// This is housekeeping, so it lives with the janitor rather than on startup.
// ANALYZE reads every index btree, which on a heavily fragmented file is
// seconds of scattered IO — acceptable hourly and off the hot path, not
// acceptable in front of the first request after a deploy.

// Optimize refreshes the query planner's statistics. Safe to call repeatedly and
// safe to call concurrently with traffic: it takes the write lock only briefly,
// at the end, to store the statistics it gathered.
//
// A database that has never been analyzed gets a full ANALYZE; after that the
// cheaper PRAGMA optimize suffices.
//
// PRAGMA optimize is deliberately NOT used for the first pass. Its heuristics
// are scoped to queries seen on the CURRENT connection, so running it from a
// fresh janitor connection — which has issued no queries — can legitimately
// decide there is nothing to do and leave a database with no statistics exactly
// as it found it. The explicit ANALYZE is what guarantees the first pass
// actually produces stats.
func (s *Store) Optimize(ctx context.Context) error {
	has, err := s.hasQueryStats()
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.db.ExecContext(ctx, `ANALYZE`); err != nil {
			return fmt.Errorf("store: analyze: %w", err)
		}
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return fmt.Errorf("store: optimize: %w", err)
	}
	return nil
}

// hasQueryStats reports whether ANALYZE has ever produced statistics. It checks
// for rows, not just for the table: ANALYZE creates sqlite_stat1 and can leave it
// empty, and an empty stat table tells the planner nothing.
func (s *Store) hasQueryStats() (bool, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1'`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("store: query stats present: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_stat1`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: query stats rows: %w", err)
	}
	return n > 0, nil
}

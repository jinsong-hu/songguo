package store

import (
	"fmt"
	"time"
)

// Retention prunes derived and captured data on a fixed clock (see
// docs/arch.md). It is analysis-side housekeeping — never on the gateway hot
// path — and each prune is an independent DELETE keyed by a timestamp column.
// There is no VACUUM: freed pages are reused by new inserts, so the database
// plateaus rather than shrinks (a periodic full vacuum would lock the DB and
// churn disk for a steady-state gateway).

// PruneRaw deletes captured request/response bodies (the raw table) older than
// the cutoff, by capture time. The shortest-lived tier — bodies are large and
// only needed for recent debugging/parse. Returns rows deleted.
func (s *Store) PruneRaw(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM raw WHERE created_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune raw: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneCalls deletes call-level stats rows older than the cutoff, by start time
// (ts). Foreign-key cascade drops each pruned call's raw/parsed/composition
// children, so this also reclaims any raw bodies the 7-day PruneRaw hasn't
// already removed. Returns rows deleted.
func (s *Store) PruneCalls(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM calls WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune calls: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneSessions deletes materialized session rollups whose last activity is
// older than the cutoff (last_ts). Because the rollup is never recomputed (see
// docs/arch-insights.md), this is final: an aged-out session is gone, not
// rebuilt. Returns rows deleted.
func (s *Store) PruneSessions(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE last_ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

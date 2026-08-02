package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Per-user running spend totals. See internal/spend for why budget enforcement
// reads a running total instead of SUM(cost) over the call log: the aggregate
// was slow and growing, and it silently DECREASED as retention pruned calls, so
// a user's budget refilled on its own.

// LoadSpend returns a user's stored running total and whether a row exists. A
// missing row is not an error — it is a user who has never been charged.
func (s *Store) LoadSpend(userID string) (float64, bool, error) {
	var total float64
	err := s.db.QueryRow(`SELECT spend FROM user_spend WHERE user_id = ?`, userID).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: load spend: %w", err)
	}
	return total, true, nil
}

// SaveSpend writes a user's running total.
func (s *Store) SaveSpend(userID string, total float64) error {
	if _, err := s.db.Exec(
		`INSERT INTO user_spend (user_id, spend, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET spend = excluded.spend, updated_at = excluded.updated_at`,
		userID, total, time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: save spend: %w", err)
	}
	return nil
}

// DeleteSpend removes a user's running total, called when the user is deleted.
// user_spend deliberately has no FK to users, so this is explicit rather than a
// cascade: an orphaned total must never be able to block deleting a user.
func (s *Store) DeleteSpend(userID string) error {
	if _, err := s.db.Exec(`DELETE FROM user_spend WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: delete spend: %w", err)
	}
	return nil
}

// backfillUserSpend seeds running totals from the existing call log, but only
// when user_spend is empty and calls has cost-bearing rows — so it runs at most
// once. A populated table is left alone; from then on the total is maintained
// incrementally and never recomputed, exactly like the sessions rollup.
//
// Note this seeds from the SURVIVING calls, so spend already lost to retention
// before this migration stays lost. It cannot be recovered, and the alternative
// (leaving everyone at zero) would be worse.
func (s *Store) backfillUserSpend() error {
	var existing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_spend`).Scan(&existing); err != nil {
		return fmt.Errorf("store: count user_spend: %w", err)
	}
	if existing > 0 {
		return nil // already seeded
	}

	if _, err := s.db.Exec(
		`INSERT INTO user_spend (user_id, spend, updated_at)
		 SELECT user_id, COALESCE(SUM(cost), 0), ?
		   FROM calls
		  WHERE user_id != ''
		  GROUP BY user_id`,
		time.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("store: backfill user_spend: %w", err)
	}
	return nil
}

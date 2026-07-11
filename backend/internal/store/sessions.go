package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// UpsertSessionCall folds one finalized call into its session's materialized
// rollup (the write-through cache in docs/arch-insights.md). It is incremental
// by construction — one call in, one row updated or created — and is NEVER a
// recompute from calls. Called off the hot path by the insights fork for calls
// that carry a non-empty session id; session-less calls have no session row.
//
// The update is a single upsert: on conflict it accumulates turns, tokens, cost,
// and error count, extends first_ts/last_ts, and — only when this call is at or
// after the current last_ts — advances last_status (which drives the inferred
// outcome). Ordering by last_ts, not arrival, keeps the outcome correct even if
// the insights fork processes two of a session's calls out of order.
func (s *Store) UpsertSessionCall(e calls.Entry, title string) error {
	if e.SessionID == "" {
		return nil // session-less traffic lives only in calls
	}
	// A finalized call's activity time is its end time; fall back to start.
	ts := e.TSEnd
	if ts.IsZero() {
		ts = e.TS
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	tsMs := ts.UnixMilli()

	isErr := 0
	if e.Status == 0 || e.Status >= 400 {
		isErr = 1
	}
	hasSub := 0
	if e.ParentAgentID != "" {
		hasSub = 1
	}

	if _, err := s.db.Exec(
		`INSERT INTO sessions
		   (id, title, first_ts, last_ts, turns, error_count, input_tokens, output_tokens, cost, tool_calls, tool_tokens, last_status, has_subagents)
		 VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title         = CASE
		                     WHEN sessions.title = '' AND excluded.title != '' THEN excluded.title
		                     ELSE sessions.title
		                   END,
		   first_ts      = MIN(first_ts, excluded.first_ts),
		   last_ts       = MAX(last_ts, excluded.last_ts),
		   turns         = turns + 1,
		   error_count   = error_count + excluded.error_count,
		   input_tokens  = input_tokens + excluded.input_tokens,
		   output_tokens = output_tokens + excluded.output_tokens,
		   cost          = cost + excluded.cost,
		   tool_calls    = tool_calls + excluded.tool_calls,
		   tool_tokens   = tool_tokens + excluded.tool_tokens,
		   -- Advance the outcome-bearing status only when this call is the newest
		   -- seen so far, so out-of-order processing can't regress it.
		   last_status   = CASE WHEN excluded.last_ts >= last_ts THEN excluded.last_status ELSE last_status END,
		   has_subagents = MAX(has_subagents, excluded.has_subagents)`,
		e.SessionID, title, tsMs, tsMs, isErr, e.InputTokens, e.OutputTokens, e.Cost, e.ToolCalls, e.ToolTokens, e.Status, hasSub,
	); err != nil {
		return fmt.Errorf("store: upsert session call: %w", err)
	}
	return nil
}

// SessionRow is one materialized session rollup.
type SessionRow struct {
	ID           string
	Title        string
	FirstTS      time.Time
	LastTS       time.Time
	Turns        int
	ErrorCount   int
	InputTokens  float64
	OutputTokens float64
	Cost         float64
	LastStatus   int
	HasSubagents bool
}

// SessionTitle returns the durable title for a materialized session.
func (s *Store) SessionTitle(id string) (string, error) {
	var title string
	err := s.db.QueryRow(`SELECT title FROM sessions WHERE id = ?`, id).Scan(&title)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: session title: %w", err)
	}
	return title, nil
}

// Outcome classifies the session from its last-seen call status, mirroring the
// interaction-level signal documented on the old on-the-fly SessionStats:
// interrupted (no upstream response), errored (4xx/5xx), completed (2xx/3xx),
// or pending (still in flight — last call created but not finalized).
func (r SessionRow) Outcome() string {
	switch {
	case r.LastStatus == calls.StatusPending:
		return "pending"
	case r.LastStatus == 0:
		return "interrupted"
	case r.LastStatus >= 400:
		return "errored"
	default:
		return "completed"
	}
}

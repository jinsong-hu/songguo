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
//
// Utility calls (monitor, count_tokens, title/compaction — see Entry.Entrypoint)
// are folded differently: their tokens and cost still count toward the session's
// spend (real spend the user paid for but never saw), but they do NOT advance the
// accretion metrics — turns, tool_calls, tool_tokens — which only mean something
// for the visible conversation. The utility slice is also broken out into the
// utility_* columns so the dashboard can show a separate track and subtract it.
// A utility call still extends the time bounds and can carry the latest status,
// since it is a real call on the session's timeline.
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

	// Split the accretion metrics from spend. A utility call contributes 0 turns
	// and 0 tool activity, and its counts/tokens/cost land in the utility_* slice;
	// a main call contributes 1 turn, its tool metrics, and 0 to the utility slice.
	// Either way its tokens/cost flow into the session's spend columns below.
	turnInc, toolCalls, toolTokens := 1, e.ToolCalls, e.ToolTokens
	utilCalls := 0
	var utilIn, utilOut, utilCacheRead, utilCacheCreate, utilCost float64
	if e.Entrypoint.IsUtility() {
		turnInc, toolCalls, toolTokens = 0, 0, 0
		utilCalls = 1
		utilIn, utilOut = e.InputTokens, e.OutputTokens
		utilCacheRead, utilCacheCreate = e.CachedTokens, e.CacheCreationTokens
		utilCost = e.Cost
	}

	if _, err := s.db.Exec(
		`INSERT INTO sessions
		   (id, title, user_id, first_ts, last_ts, turns, error_count, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, thinking_tokens, cost, tool_calls, tool_tokens, last_status, has_subagents,
		    utility_calls, utility_input_tokens, utility_output_tokens, utility_cache_read_input_tokens, utility_cache_creation_input_tokens, utility_cost)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title         = CASE
		                     WHEN sessions.title = '' AND excluded.title != '' THEN excluded.title
		                     ELSE sessions.title
		                   END,
		   -- A session belongs to one consumer key; latch the first non-empty id
		   -- and never overwrite it (mirrors the title fill-if-empty above).
		   user_id       = CASE
		                     WHEN sessions.user_id = '' AND excluded.user_id != '' THEN excluded.user_id
		                     ELSE sessions.user_id
		                   END,
		   first_ts                    = MIN(first_ts, excluded.first_ts),
		   last_ts                     = MAX(last_ts, excluded.last_ts),
		   turns                       = turns + excluded.turns,
		   error_count                 = error_count + excluded.error_count,
		   input_tokens                = input_tokens + excluded.input_tokens,
		   output_tokens               = output_tokens + excluded.output_tokens,
		   cache_read_input_tokens     = cache_read_input_tokens + excluded.cache_read_input_tokens,
		   cache_creation_input_tokens = cache_creation_input_tokens + excluded.cache_creation_input_tokens,
		   thinking_tokens             = thinking_tokens + excluded.thinking_tokens,
		   cost                        = cost + excluded.cost,
		   tool_calls                  = tool_calls + excluded.tool_calls,
		   tool_tokens                 = tool_tokens + excluded.tool_tokens,
		   utility_calls                       = utility_calls + excluded.utility_calls,
		   utility_input_tokens                = utility_input_tokens + excluded.utility_input_tokens,
		   utility_output_tokens               = utility_output_tokens + excluded.utility_output_tokens,
		   utility_cache_read_input_tokens     = utility_cache_read_input_tokens + excluded.utility_cache_read_input_tokens,
		   utility_cache_creation_input_tokens = utility_cache_creation_input_tokens + excluded.utility_cache_creation_input_tokens,
		   utility_cost                        = utility_cost + excluded.utility_cost,
		   -- Advance the outcome-bearing status only when this call is the newest
		   -- seen so far, so out-of-order processing can't regress it.
		   last_status   = CASE WHEN excluded.last_ts >= last_ts THEN excluded.last_status ELSE last_status END,
		   has_subagents = MAX(has_subagents, excluded.has_subagents)`,
		e.SessionID, title, e.UserID, tsMs, tsMs, turnInc, isErr, e.InputTokens, e.OutputTokens,
		e.CachedTokens, e.CacheCreationTokens, e.ThinkingTokens, e.Cost,
		toolCalls, toolTokens, e.Status, hasSub,
		utilCalls, utilIn, utilOut, utilCacheRead, utilCacheCreate, utilCost,
	); err != nil {
		return fmt.Errorf("store: upsert session call: %w", err)
	}
	return nil
}

// SessionRow is one materialized session rollup.
type SessionRow struct {
	ID                  string
	Title               string
	FirstTS             time.Time
	LastTS              time.Time
	Turns               int
	ErrorCount          int
	InputTokens         float64
	OutputTokens        float64
	CachedTokens        float64 // cache_read_input_tokens
	CacheCreationTokens float64
	ThinkingTokens      float64
	Cost                float64
	LastStatus          int
	HasSubagents        bool
	// Utility-call slice: harness calls (monitor, count_tokens, title/compaction)
	// that share the wire but are not visible turns. These counts/tokens/cost are
	// ALSO included in the fields above (spend keeps everything); they are broken
	// out so a caller can show a separate utility track or subtract to get the
	// context/turn view. Turns and the tool_* fields already EXCLUDE utility calls.
	UtilityCalls               int
	UtilityInputTokens         float64
	UtilityOutputTokens        float64
	UtilityCachedTokens        float64 // utility_cache_read_input_tokens
	UtilityCacheCreationTokens float64
	UtilityCost                float64
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

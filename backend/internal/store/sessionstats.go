package store

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// SessionStats summarizes coding-agent sessions — calls sharing a non-empty
// captured session id — over a time window. Non-session traffic (calls with an
// empty session id) is ignored entirely.
//
// Outcome is inferred from each session's LAST call by timestamp. It is a purely
// interaction-level signal read off the ledger — NOT a judgment about whether the
// underlying coding task actually succeeded (the proxy never sees that):
//   - Interrupted: the last call had no upstream response (status 0), e.g. the
//     client aborted mid-stream (user hit ESC or walked away).
//   - Errored:     the last call returned a 4xx/5xx.
//   - Completed:   the last call returned a 2xx/3xx.
//
// Turns/tokens/duration distributions use nearest-rank percentiles across
// sessions. Duration is wall-clock LastTS−FirstTS in seconds; a single-call
// session reads as 0 (the ledger only stores per-call completion time).
type SessionStats struct {
	Sessions    int
	Completed   int
	Errored     int
	Interrupted int

	// WithSubagents counts sessions that spawned at least one subagent (any call
	// carried a non-empty parent_agent_id) when the client exposes agent-tree
	// headers.
	WithSubagents int

	// Totals and means for headline cards.
	TotalTurns     int
	TotalTokens    float64
	TotalToolCalls int
	AvgTurns       float64
	AvgTokens      float64
	AvgDuration    float64 // seconds
	AvgToolCalls   float64

	// Per-session distributions (nearest-rank percentiles).
	TurnsP50     int64
	TurnsP95     int64
	TokensP50    int64
	TokensP95    int64
	DurationP50  int64 // seconds
	DurationP95  int64 // seconds
	ToolCallsP50 int64
	ToolCallsP95 int64
}

// SessionStats aggregates coding-agent sessions over the optional [since, until)
// window. It reads the materialized `sessions` rollup (the write-through cache
// maintained incrementally by UpsertSessionCall — see docs/arch-insights.md),
// NOT a live GROUP BY over calls. The window filters on each session's last
// activity (last_ts), the same key the rollup is pruned by. Outcomes, totals,
// and per-session percentiles are derived from the rolled-up rows.
func (s *Store) SessionStats(userID string, since, until *time.Time) (SessionStats, error) {
	// windowClause emits predicates on `ts`; the sessions table keys activity on
	// last_ts, so build the clause by hand. A non-empty userID restricts to that
	// consumer key's own sessions (rows predating the user_id column carry '' and
	// so only ever appear in the unscoped operator view).
	var (
		conds []string
		args  []any
	)
	if since != nil {
		conds = append(conds, "last_ts >= ?")
		args = append(args, since.UnixMilli())
	}
	if until != nil {
		conds = append(conds, "last_ts < ?")
		args = append(args, until.UnixMilli())
	}
	if userID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, userID)
	}
	clause := ""
	if len(conds) > 0 {
		clause = " WHERE " + strings.Join(conds, " AND ")
	}

	rows, err := s.db.Query(
		`SELECT first_ts, last_ts, turns, tool_calls, input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, last_status, has_subagents
		   FROM sessions`+clause, args...,
	)
	if err != nil {
		return SessionStats{}, fmt.Errorf("store: session stats: %w", err)
	}
	defer rows.Close()

	type agg struct {
		turns       int
		toolCalls   int
		tokens      float64
		firstMs     int64
		lastMs      int64
		lastStatus  int
		hasSubagent bool
	}
	var aggs []agg
	for rows.Next() {
		var (
			a                              agg
			inTok, outTok                  float64
			cacheReadTok, cacheCreationTok float64
			hasSub                         int
		)
		if err := rows.Scan(&a.firstMs, &a.lastMs, &a.turns, &a.toolCalls, &inTok, &outTok, &cacheReadTok, &cacheCreationTok, &a.lastStatus, &hasSub); err != nil {
			return SessionStats{}, fmt.Errorf("store: scan session stats: %w", err)
		}
		// Total tokens = the disjoint input parts + output (thinking is a subset of
		// output and is not added on top).
		a.tokens = inTok + cacheReadTok + cacheCreationTok + outTok
		a.hasSubagent = hasSub != 0
		aggs = append(aggs, a)
	}
	if err := rows.Err(); err != nil {
		return SessionStats{}, fmt.Errorf("store: session stats: %w", err)
	}

	out := SessionStats{Sessions: len(aggs)}
	var (
		turnsVals     = make([]int64, 0, len(aggs))
		tokensVals    = make([]int64, 0, len(aggs))
		durationVals  = make([]int64, 0, len(aggs))
		toolCallsVals = make([]int64, 0, len(aggs))
	)
	for _, agg := range aggs {
		switch {
		case agg.lastStatus == calls.StatusPending:
			// Still in flight — count as interrupted-in-progress for the mix.
			out.Interrupted++
		case agg.lastStatus == 0:
			out.Interrupted++
		case agg.lastStatus >= 400:
			out.Errored++
		default:
			out.Completed++
		}
		if agg.hasSubagent {
			out.WithSubagents++
		}

		out.TotalTurns += agg.turns
		out.TotalTokens += agg.tokens
		out.TotalToolCalls += agg.toolCalls

		durSec := (agg.lastMs - agg.firstMs) / 1000
		turnsVals = append(turnsVals, int64(agg.turns))
		tokensVals = append(tokensVals, int64(agg.tokens))
		durationVals = append(durationVals, durSec)
		toolCallsVals = append(toolCallsVals, int64(agg.toolCalls))
	}

	if out.Sessions > 0 {
		n := float64(out.Sessions)
		out.AvgTurns = float64(out.TotalTurns) / n
		out.AvgTokens = out.TotalTokens / n
		out.AvgToolCalls = float64(out.TotalToolCalls) / n
		var totalDur int64
		for _, d := range durationVals {
			totalDur += d
		}
		out.AvgDuration = float64(totalDur) / n
	}

	sort.Slice(turnsVals, func(i, j int) bool { return turnsVals[i] < turnsVals[j] })
	sort.Slice(tokensVals, func(i, j int) bool { return tokensVals[i] < tokensVals[j] })
	sort.Slice(durationVals, func(i, j int) bool { return durationVals[i] < durationVals[j] })
	sort.Slice(toolCallsVals, func(i, j int) bool { return toolCallsVals[i] < toolCallsVals[j] })
	out.TurnsP50 = percentileNearestRank(turnsVals, 50)
	out.TurnsP95 = percentileNearestRank(turnsVals, 95)
	out.TokensP50 = percentileNearestRank(tokensVals, 50)
	out.TokensP95 = percentileNearestRank(tokensVals, 95)
	out.DurationP50 = percentileNearestRank(durationVals, 50)
	out.DurationP95 = percentileNearestRank(durationVals, 95)
	out.ToolCallsP50 = percentileNearestRank(toolCallsVals, 50)
	out.ToolCallsP95 = percentileNearestRank(toolCallsVals, 95)

	return out, nil
}

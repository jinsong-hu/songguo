package store

import (
	"fmt"
	"sort"
	"time"
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
	TotalTurns  int
	TotalTokens float64
	AvgTurns    float64
	AvgTokens   float64
	AvgDuration float64 // seconds

	// Per-session distributions (nearest-rank percentiles).
	TurnsP50    int64
	TurnsP95    int64
	TokensP50   int64
	TokensP95   int64
	DurationP50 int64 // seconds
	DurationP95 int64 // seconds
}

// sessionAgg accumulates one session's calls as they stream out of SQLite,
// ordered by (session_id, ts). lastStatus tracks the status of the newest call
// seen, which drives the inferred outcome.
type sessionAgg struct {
	turns       int
	tokens      float64
	firstMs     int64
	lastMs      int64
	lastStatus  int
	hasSubagent bool
}

// SessionStats aggregates coding-agent sessions over the optional [since, until)
// window. It streams the window's session-bearing calls ordered by session then
// time, folds them into per-session aggregates in Go (mirroring OverviewStats),
// then derives outcomes, totals, and percentiles.
func (s *Store) SessionStats(since, until *time.Time) (SessionStats, error) {
	clause, args := windowClause(since, until)
	// Restrict to session-bearing traffic. windowClause emits a leading
	// " WHERE ..." or "", so splice the session-id predicate in accordingly.
	if clause == "" {
		clause = " WHERE session_id != ''"
	} else {
		clause += " AND session_id != ''"
	}

	rows, err := s.db.Query(
		`SELECT session_id, ts, status, input_tokens, output_tokens, parent_agent_id
		   FROM calls`+clause+`
		  ORDER BY session_id ASC, ts ASC, id ASC`,
		args...,
	)
	if err != nil {
		return SessionStats{}, fmt.Errorf("store: session stats: %w", err)
	}
	defer rows.Close()

	sessions := make(map[string]*sessionAgg)
	for rows.Next() {
		var (
			sid           string
			tsMs          int64
			status        int
			inTok, outTok float64
			parentAgent   string
		)
		if err := rows.Scan(&sid, &tsMs, &status, &inTok, &outTok, &parentAgent); err != nil {
			return SessionStats{}, fmt.Errorf("store: scan session stats: %w", err)
		}
		agg := sessions[sid]
		if agg == nil {
			agg = &sessionAgg{firstMs: tsMs}
			sessions[sid] = agg
		}
		agg.turns++
		agg.tokens += inTok + outTok
		if tsMs < agg.firstMs {
			agg.firstMs = tsMs
		}
		// Rows arrive in ascending ts, so the newest call wins lastStatus/lastMs.
		agg.lastMs = tsMs
		agg.lastStatus = status
		if parentAgent != "" {
			agg.hasSubagent = true
		}
	}
	if err := rows.Err(); err != nil {
		return SessionStats{}, fmt.Errorf("store: session stats: %w", err)
	}

	out := SessionStats{Sessions: len(sessions)}
	var (
		turnsVals    = make([]int64, 0, len(sessions))
		tokensVals   = make([]int64, 0, len(sessions))
		durationVals = make([]int64, 0, len(sessions))
	)
	for _, agg := range sessions {
		switch {
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

		durSec := (agg.lastMs - agg.firstMs) / 1000
		turnsVals = append(turnsVals, int64(agg.turns))
		tokensVals = append(tokensVals, int64(agg.tokens))
		durationVals = append(durationVals, durSec)
	}

	if out.Sessions > 0 {
		n := float64(out.Sessions)
		out.AvgTurns = float64(out.TotalTurns) / n
		out.AvgTokens = out.TotalTokens / n
		var totalDur int64
		for _, d := range durationVals {
			totalDur += d
		}
		out.AvgDuration = float64(totalDur) / n
	}

	sort.Slice(turnsVals, func(i, j int) bool { return turnsVals[i] < turnsVals[j] })
	sort.Slice(tokensVals, func(i, j int) bool { return tokensVals[i] < tokensVals[j] })
	sort.Slice(durationVals, func(i, j int) bool { return durationVals[i] < durationVals[j] })
	out.TurnsP50 = percentileNearestRank(turnsVals, 50)
	out.TurnsP95 = percentileNearestRank(turnsVals, 95)
	out.TokensP50 = percentileNearestRank(tokensVals, 50)
	out.TokensP95 = percentileNearestRank(tokensVals, 95)
	out.DurationP50 = percentileNearestRank(durationVals, 50)
	out.DurationP95 = percentileNearestRank(durationVals, 95)

	return out, nil
}

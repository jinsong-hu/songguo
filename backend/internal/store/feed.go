package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// FeedRow is one row of the activity feed: either an aggregated coding-agent
// session (Kind == "session") or a standalone request that carried no session id
// (Kind == "request"). Calls sharing a captured session id collapse into a
// single session row; every other call is its own request row.
type FeedRow struct {
	Kind      string // "session" | "request"
	SessionID string // set when Kind == "session"
	Title     string // durable captured-session title
	RequestID string // the call id (UUID) to link to when Kind == "request"
	Calls     int    // number of calls in the group (1 for a request row)
	Cost      float64
	// Disjoint input-side token parts (InputTokens is fresh/uncached) plus output;
	// a total is input + cache_read + cache_creation + output.
	InputTokens         float64
	OutputTokens        float64
	CachedTokens        float64 // cache_read_input_tokens
	CacheCreationTokens float64
	// Non-token metered units: Seconds is billed audio duration (ASR wires),
	// Chars is billed text length (TTS wires). Both 0 for token-metered rows.
	Seconds    float64
	Chars      float64
	ToolCalls  int     // tool calls issued across the group's turns
	ToolTokens float64 // local o200k estimate of tool-result tokens (see compose.ToolTurn)
	FirstTS    time.Time
	LastTS     time.Time // ordering key + "last activity" display
	DurationMS int64     // max(request start + latency) - first request start
	ErrorCount int       // calls with status 0 or >= 400
	MajorModel string    // model with the most calls in the group
	Models     []string  // distinct models touched (session rows)
	Vendors    []string  // distinct vendors touched (session rows)

	// Single-call fields, meaningful only for request rows.
	Model      string
	Vendor     string
	Wire       string
	Confidence calls.Confidence
	Modality   calls.Modality
	Status     int
	LatencyMS  int64
	Stream     bool
}

// The group key folds a session's calls together while leaving every
// non-session call in its own singleton group.
const feedGroupKey = `CASE WHEN session_id != '' THEN session_id ELSE 'req:' || id END`

// feedErrorExpr counts a call as an error when it has no upstream status (0) or
// a 4xx/5xx status.
const feedErrorExpr = `SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END)`

// feedTotalTokensExpr is the row's total token count across all input-side parts
// plus output — the "tokens" sort key. Mirrors the frontend's usage total.
const feedTotalTokensExpr = `(COALESCE(SUM(input_tokens),0) + COALESCE(SUM(output_tokens),0) + COALESCE(SUM(cache_read_input_tokens),0) + COALESCE(SUM(cache_creation_input_tokens),0))`

// feedOrder maps a whitelisted sort key to its ORDER BY clause. The values are
// fixed SQL — the caller's raw sort string only ever selects a key, it is never
// interpolated — so this is injection-safe. Every clause tiebreaks on
// MAX(ts) DESC, req_id DESC so paging stays deterministic. Unknown/empty keys
// fall back to recent-first via feedOrderClause.
var feedOrder = map[string]string{
	"recent":   `MAX(ts) DESC, req_id DESC`,
	"tokens":   feedTotalTokensExpr + ` DESC, MAX(ts) DESC, req_id DESC`,
	"cost":     `COALESCE(SUM(cost),0) DESC, MAX(ts) DESC, req_id DESC`,
	"duration": `COALESCE(MAX(ts + latency_ms) - MIN(ts), 0) DESC, MAX(ts) DESC, req_id DESC`,
	"turns":    `COUNT(*) DESC, MAX(ts) DESC, req_id DESC`,
	"slow":     `MAX(latency_ms) DESC, MAX(ts) DESC, req_id DESC`,
	"failures": feedErrorExpr + ` DESC, MAX(ts) DESC, req_id DESC`,
}

// feedOrderClause resolves a sort key to its ORDER BY clause (recent-first for
// empty/unknown) and the HAVING clause that scopes the grouped rows. Only
// "failures" scopes — it drops groups with no errored calls; every other sort
// keeps them all. Both count and page queries apply the same HAVING so totals
// and pages stay consistent.
func feedOrderClause(sort string) (order, having string) {
	order, ok := feedOrder[sort]
	if !ok {
		order = feedOrder["recent"]
	}
	if sort == "failures" {
		having = " HAVING " + feedErrorExpr + " > 0"
	}
	return order, having
}

// Feed returns the activity feed and the total group count for pagination. The
// ordering is selected by f.FeedSort (recent-first by default; see feedOrder);
// the "failures" sort additionally scopes to groups with at least one errored
// call. The filter's model/vendor/status conditions apply per-call before
// grouping, so a session surfaces when any of its calls match — its rolled-up
// totals then cover only the matching calls. Limit defaults to 100 and is
// capped at 1000.
func (s *Store) Feed(f CallFilter) ([]FeedRow, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultCallsLimit
	}
	if limit > maxCallsLimit {
		limit = maxCallsLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	clause, args := f.where()
	order, having := feedOrderClause(f.FeedSort)

	var total int
	countQuery := `SELECT COUNT(*) FROM (SELECT 1 FROM calls` + clause + ` GROUP BY ` + feedGroupKey + having + `)`
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: feed count: %w", err)
	}

	query := `SELECT
		` + feedGroupKey + ` AS gkey,
		MAX(CASE WHEN session_id != '' THEN 1 ELSE 0 END) AS is_session,
		MAX(session_id) AS session_id,
		MAX(id) AS req_id,
		COUNT(*) AS calls,
		COALESCE(SUM(cost), 0) AS cost,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cache_read_input_tokens), 0) AS cache_read_input_tokens,
		COALESCE(SUM(cache_creation_input_tokens), 0) AS cache_creation_input_tokens,
		COALESCE(SUM(seconds), 0) AS seconds,
		COALESCE(SUM(chars), 0) AS chars,
		COALESCE(SUM(tool_calls), 0) AS tool_calls,
		COALESCE(SUM(tool_tokens), 0) AS tool_tokens,
		MIN(ts) AS first_ts,
		MAX(ts) AS last_ts,
		COALESCE(MAX(ts + latency_ms) - MIN(ts), 0) AS duration_ms,
		` + feedErrorExpr + ` AS error_count,
		group_concat(model) AS model_samples,
		group_concat(DISTINCT model) AS models,
		group_concat(DISTINCT vendor) AS vendors,
		MAX(model) AS model,
		MAX(vendor) AS vendor,
		MAX(wire) AS wire,
		MAX(confidence) AS confidence,
		MAX(modality) AS modality,
		MAX(status) AS status,
		MAX(latency_ms) AS latency_ms,
		MAX(stream) AS stream
	FROM calls` + clause + `
	GROUP BY gkey` + having + `
	ORDER BY ` + order + `
	LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: feed: %w", err)
	}
	defer rows.Close()

	var out []FeedRow
	for rows.Next() {
		row, err := scanFeedRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan feed row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: feed: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("store: close feed rows: %w", err)
	}
	if err := s.attachFeedTitles(out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Store) attachFeedTitles(feed []FeedRow) error {
	ids := make([]string, 0, len(feed))
	seen := map[string]struct{}{}
	for _, row := range feed {
		if row.Kind != "session" || row.SessionID == "" {
			continue
		}
		if _, ok := seen[row.SessionID]; ok {
			continue
		}
		seen[row.SessionID] = struct{}{}
		ids = append(ids, row.SessionID)
	}
	if len(ids) == 0 {
		return nil
	}

	args := make([]any, len(ids))
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id
		placeholders[i] = "?"
	}
	rows, err := s.db.Query(
		`SELECT id, title FROM sessions WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("store: feed titles: %w", err)
	}
	defer rows.Close()

	titles := make(map[string]string, len(ids))
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			return fmt.Errorf("store: scan feed title: %w", err)
		}
		titles[id] = title
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: feed titles: %w", err)
	}
	for i := range feed {
		feed[i].Title = titles[feed[i].SessionID]
	}
	return nil
}

func scanFeedRow(rows *sql.Rows) (FeedRow, error) {
	var (
		r          FeedRow
		gkey       string
		isSession  int
		firstMs    int64
		lastMs     int64
		modelsAll  sql.NullString
		models     sql.NullString
		vendors    sql.NullString
		confidence string
		modality   string
		stream     int
	)
	if err := rows.Scan(
		&gkey, &isSession, &r.SessionID, &r.RequestID, &r.Calls,
		&r.Cost, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.CacheCreationTokens,
		&r.Seconds, &r.Chars,
		&r.ToolCalls, &r.ToolTokens, &firstMs, &lastMs, &r.DurationMS, &r.ErrorCount,
		&modelsAll, &models, &vendors,
		&r.Model, &r.Vendor, &r.Wire, &confidence, &modality, &r.Status, &r.LatencyMS, &stream,
	); err != nil {
		return FeedRow{}, err
	}

	r.FirstTS = time.UnixMilli(firstMs)
	r.LastTS = time.UnixMilli(lastMs)
	r.Confidence = calls.Confidence(confidence)
	r.Modality = calls.Modality(modality)
	r.Stream = stream != 0
	r.MajorModel = majorModel(modelsAll.String)
	r.Models = splitDistinct(models.String)
	r.Vendors = splitDistinct(vendors.String)

	if isSession != 0 {
		r.Kind = "session"
		r.RequestID = "" // a session row is not a single call
	} else {
		r.Kind = "request"
		r.SessionID = ""
	}
	return r, nil
}

func majorModel(s string) string {
	if s == "" {
		return ""
	}
	counts := map[string]int{}
	best := ""
	bestCount := 0
	for _, p := range strings.Split(s, ",") {
		if p == "" {
			continue
		}
		counts[p]++
		if counts[p] > bestCount || (counts[p] == bestCount && (best == "" || p < best)) {
			best = p
			bestCount = counts[p]
		}
	}
	return best
}

// splitDistinct turns a group_concat result into a trimmed, empties-dropped
// slice. Order is unspecified (SQLite does not guarantee group_concat ordering).
func splitDistinct(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

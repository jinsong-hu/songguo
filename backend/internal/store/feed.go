package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/calls"
)

// FeedRow is one row of the activity feed: either an aggregated Claude Code
// session (Kind == "session") or a standalone request that carried no session id
// (Kind == "request"). Calls sharing an X-Claude-Code-Session-Id collapse into a
// single session row; every other call is its own request row.
type FeedRow struct {
	Kind         string // "session" | "request"
	SessionID    string // set when Kind == "session"
	RequestID    int64  // the call id to link to when Kind == "request"
	Calls        int    // number of calls in the group (1 for a request row)
	Cost         float64
	InputTokens  float64
	OutputTokens float64
	FirstTS      time.Time
	LastTS       time.Time // ordering key + "last activity" display
	ErrorCount   int       // calls with status 0 or >= 400
	Models       []string  // distinct models touched (session rows)
	Vendors      []string  // distinct vendors touched (session rows)

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

// Feed returns the activity feed ordered by last activity (newest first) and the
// total group count for pagination. The filter's model/vendor/status conditions
// apply per-call before grouping, so a session surfaces when any of its calls
// match — its rolled-up totals then cover only the matching calls. Limit
// defaults to 100 and is capped at 1000.
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

	var total int
	countQuery := `SELECT COUNT(*) FROM (SELECT 1 FROM calls` + clause + ` GROUP BY ` + feedGroupKey + `)`
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
		MIN(ts) AS first_ts,
		MAX(ts) AS last_ts,
		` + feedErrorExpr + ` AS error_count,
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
	GROUP BY gkey
	ORDER BY last_ts DESC, req_id DESC
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
	return out, total, nil
}

func scanFeedRow(rows *sql.Rows) (FeedRow, error) {
	var (
		r          FeedRow
		gkey       string
		isSession  int
		firstMs    int64
		lastMs     int64
		models     sql.NullString
		vendors    sql.NullString
		confidence string
		modality   string
		stream     int
	)
	if err := rows.Scan(
		&gkey, &isSession, &r.SessionID, &r.RequestID, &r.Calls,
		&r.Cost, &r.InputTokens, &r.OutputTokens, &firstMs, &lastMs, &r.ErrorCount,
		&models, &vendors,
		&r.Model, &r.Vendor, &r.Wire, &confidence, &modality, &r.Status, &r.LatencyMS, &stream,
	); err != nil {
		return FeedRow{}, err
	}

	r.FirstTS = time.UnixMilli(firstMs)
	r.LastTS = time.UnixMilli(lastMs)
	r.Confidence = calls.Confidence(confidence)
	r.Modality = calls.Modality(modality)
	r.Stream = stream != 0
	r.Models = splitDistinct(models.String)
	r.Vendors = splitDistinct(vendors.String)

	if isSession != 0 {
		r.Kind = "session"
		r.RequestID = 0 // a session row is not a single call
	} else {
		r.Kind = "request"
		r.SessionID = ""
	}
	return r, nil
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

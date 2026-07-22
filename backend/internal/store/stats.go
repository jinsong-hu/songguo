package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// OverviewStats summarizes request volume, error rate, and latency
// percentiles over a time window. Latencies are in milliseconds.
//
// An "error" is any row whose upstream status is 0 (transport failure) or
// >= 400. Percentiles use the nearest-rank method over the sorted, non-empty
// set of latencies; they are 0 when there are no rows.
type OverviewStats struct {
	Requests     int
	Errors       int
	P50          int64
	P95          int64
	P99          int64
	TTFTP50      int64
	TTFTP95      int64
	TTFTP99      int64
	OutputTPSP50 float64
	OutputTPSP95 float64
	OutputTPSP99 float64
}

// VendorStat holds per-vendor request/error counts, average latency, and the
// status of the most recent row (by ts) for that vendor.
type VendorStat struct {
	Requests   int
	Errors     int
	AvgLatency float64 // milliseconds
	LastStatus int     // status of the most recent row for this vendor
}

// ModelStat holds per-model request/error counts and average latency.
type ModelStat struct {
	Requests   int
	Errors     int
	AvgLatency float64 // milliseconds
}

// windowClause builds the optional "[since, until)" WHERE clause shared by the
// stats queries.
// windowClause builds the time-window (and optional per-user) WHERE clause
// shared by the aggregate stats queries. userID == "" leaves the query unscoped
// (the operator/admin view); a non-empty userID restricts to that consumer key's
// own calls via the indexed user_id column.
func windowClause(userID string, since, until *time.Time) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if since != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, since.UnixMilli())
	}
	if until != nil {
		conds = append(conds, "ts < ?")
		args = append(args, until.UnixMilli())
	}
	if userID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, userID)
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// userScopeClause returns a trailing ` AND user_id = ?` fragment (and its bound
// arg) for the per-user-scoped series queries that build their WHERE by hand.
// userID == "" yields an empty clause and no args, leaving the query unscoped.
func userScopeClause(userID string) (string, []any) {
	if userID == "" {
		return "", nil
	}
	return " AND user_id = ?", []any{userID}
}

// OverviewStats returns total requests, error count, and p50/p95/p99 latency
// (ms) over the optional [since, until) window. It pulls the latencies sorted
// from SQLite and computes percentiles in Go via nearest-rank.
func (s *Store) OverviewStats(userID string, since, until *time.Time) (OverviewStats, error) {
	clause, args := windowClause(userID, since, until)

	rows, err := s.db.Query(
		`SELECT latency_ms, status, ttft_ms, generation_ms, output_tokens
		   FROM calls`+clause+` ORDER BY latency_ms ASC`,
		args...,
	)
	if err != nil {
		return OverviewStats{}, fmt.Errorf("store: overview stats: %w", err)
	}
	defer rows.Close()

	var (
		out       OverviewStats
		latencies []int64
		ttfts     []int64
		outputTPS []float64
	)
	for rows.Next() {
		var (
			latency      int64
			status       int
			ttft         int64
			generation   int64
			outputTokens float64
		)
		if err := rows.Scan(&latency, &status, &ttft, &generation, &outputTokens); err != nil {
			return OverviewStats{}, fmt.Errorf("store: scan overview stats: %w", err)
		}
		out.Requests++
		if isErrorStatus(status) {
			out.Errors++
		}
		latencies = append(latencies, latency)
		if ttft > 0 {
			ttfts = append(ttfts, ttft)
		}
		if generation > 0 && outputTokens > 0 {
			outputTPS = append(outputTPS, outputTokens*1000/float64(generation))
		}
	}
	if err := rows.Err(); err != nil {
		return OverviewStats{}, fmt.Errorf("store: overview stats: %w", err)
	}

	// latencies is already sorted ascending by the query.
	out.P50 = percentileNearestRank(latencies, 50)
	out.P95 = percentileNearestRank(latencies, 95)
	out.P99 = percentileNearestRank(latencies, 99)
	out.TTFTP50 = percentileNearestRank(ttfts, 50)
	out.TTFTP95 = percentileNearestRank(ttfts, 95)
	out.TTFTP99 = percentileNearestRank(ttfts, 99)
	out.OutputTPSP50 = percentileNearestRankFloat(outputTPS, 50)
	out.OutputTPSP95 = percentileNearestRankFloat(outputTPS, 95)
	out.OutputTPSP99 = percentileNearestRankFloat(outputTPS, 99)
	return out, nil
}

// VendorStats returns per-vendor request/error counts, average latency, and
// last status over the optional [since, until) window. The map is keyed by
// vendor name; vendors with no rows in the window are absent.
func (s *Store) VendorStats(since, until *time.Time) (map[string]VendorStat, error) {
	clause, args := windowClause("", since, until)

	// Aggregate counts and average latency per vendor.
	aggRows, err := s.db.Query(
		`SELECT vendor,
		        COUNT(*),
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END),
		        COALESCE(AVG(latency_ms), 0)
		   FROM calls`+clause+`
		  GROUP BY vendor`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: vendor stats: %w", err)
	}
	defer aggRows.Close()

	out := make(map[string]VendorStat)
	for aggRows.Next() {
		var (
			vendor string
			stat   VendorStat
		)
		if err := aggRows.Scan(&vendor, &stat.Requests, &stat.Errors, &stat.AvgLatency); err != nil {
			return nil, fmt.Errorf("store: scan vendor stats: %w", err)
		}
		out[vendor] = stat
	}
	if err := aggRows.Err(); err != nil {
		return nil, fmt.Errorf("store: vendor stats: %w", err)
	}

	// Resolve the last status per vendor: the row with the greatest ts (the most
	// recent call) within the window. The call id is now a random UUID, so it is
	// no longer a recency proxy — order by ts, tie-broken by id for determinism.
	lastRows, err := s.db.Query(
		`SELECT l.vendor, l.status
		   FROM calls l
		   JOIN (SELECT vendor, MAX(ts) AS mts FROM calls`+clause+` GROUP BY vendor) m
		     ON l.vendor = m.vendor AND l.ts = m.mts`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: vendor last status: %w", err)
	}
	defer lastRows.Close()

	for lastRows.Next() {
		var (
			vendor string
			status int
		)
		if err := lastRows.Scan(&vendor, &status); err != nil {
			return nil, fmt.Errorf("store: scan vendor last status: %w", err)
		}
		if stat, ok := out[vendor]; ok {
			stat.LastStatus = status
			out[vendor] = stat
		}
	}
	if err := lastRows.Err(); err != nil {
		return nil, fmt.Errorf("store: vendor last status: %w", err)
	}

	return out, nil
}

// ModelStats returns per-model request/error counts and average latency over
// the optional [since, until) window. The map is keyed by model name; models
// with no rows in the window are absent.
func (s *Store) ModelStats(since, until *time.Time) (map[string]ModelStat, error) {
	clause, args := windowClause("", since, until)

	rows, err := s.db.Query(
		`SELECT model,
		        COUNT(*),
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END),
		        COALESCE(AVG(latency_ms), 0)
		   FROM calls`+clause+`
		  GROUP BY model`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("store: model stats: %w", err)
	}
	defer rows.Close()

	out := make(map[string]ModelStat)
	for rows.Next() {
		var (
			model string
			stat  ModelStat
		)
		if err := rows.Scan(&model, &stat.Requests, &stat.Errors, &stat.AvgLatency); err != nil {
			return nil, fmt.Errorf("store: scan model stats: %w", err)
		}
		out[model] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: model stats: %w", err)
	}
	return out, nil
}

// TokenTotals holds summed normalized token counts over a window. Input, Cached,
// and CacheCreation are disjoint input-side parts; Thinking is a subset of Output.
type TokenTotals struct {
	Input         float64
	Output        float64
	Cached        float64 // cache_read_input_tokens
	CacheCreation float64
	Thinking      float64
}

// TokenTotals sums normalized tokens over the optional [since, until) window.
func (s *Store) TokenTotals(userID string, since, until *time.Time) (TokenTotals, error) {
	clause, args := windowClause(userID, since, until)
	var t TokenTotals
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(cache_creation_input_tokens), 0),
		        COALESCE(SUM(thinking_tokens), 0)
		   FROM calls`+clause, args...,
	).Scan(&t.Input, &t.Output, &t.Cached, &t.CacheCreation, &t.Thinking)
	if err != nil {
		return TokenTotals{}, fmt.Errorf("store: token totals: %w", err)
	}
	return t, nil
}

// DistinctUsers counts distinct non-empty user_ids with at least one call in the
// optional [since, until) window. The empty user id (admin/unknown traffic) is
// excluded so the count reflects real callers.
func (s *Store) DistinctUsers(since, until *time.Time) (int, error) {
	clause, args := windowClause("", since, until)
	if clause == "" {
		clause = " WHERE user_id != ''"
	} else {
		clause += " AND user_id != ''"
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT user_id) FROM calls`+clause, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: distinct users: %w", err)
	}
	return n, nil
}

// BreakdownDimension is a column the call log can be grouped by.
type BreakdownDimension string

const (
	BreakdownByModel    BreakdownDimension = "model"
	BreakdownByVendor   BreakdownDimension = "vendor"
	BreakdownByUser     BreakdownDimension = "user"
	BreakdownByModality BreakdownDimension = "modality"
)

// ErrBadDimension is returned by Breakdown for an unrecognized dimension.
var ErrBadDimension = errors.New("store: unknown breakdown dimension")

// breakdownColumn maps a dimension to its calls column, whitelisting the input so
// it can be safely interpolated into the query (column names cannot be bound as
// query parameters).
func breakdownColumn(d BreakdownDimension) (string, bool) {
	switch d {
	case BreakdownByModel:
		return "model", true
	case BreakdownByVendor:
		return "vendor", true
	case BreakdownByUser:
		return "user_id", true
	case BreakdownByModality:
		return "modality", true
	default:
		return "", false
	}
}

// BreakdownRow is one group's aggregates in a Breakdown result. CachedTokens
// (cache reads), CacheCreationTokens, and InputTokens are disjoint input parts;
// ThinkingTokens is a subset of OutputTokens.
type BreakdownRow struct {
	Key                 string
	Requests            int
	Errors              int
	InputTokens         float64
	OutputTokens        float64
	CachedTokens        float64
	CacheCreationTokens float64
	ThinkingTokens      float64
	Cost                float64
	AvgLatencyMS        float64
}

// Breakdown groups the call log by dimension over the optional [since, until)
// window, returning per-group request/error counts, token sums, cost, and mean
// latency, ordered by request count descending. dimension must be one of the
// Breakdown* constants; otherwise ErrBadDimension is returned.
func (s *Store) Breakdown(dimension BreakdownDimension, since, until *time.Time) ([]BreakdownRow, error) {
	col, ok := breakdownColumn(dimension)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadDimension, dimension)
	}
	clause, args := windowClause("", since, until)
	rows, err := s.db.Query(
		`SELECT `+col+` AS k,
		        COUNT(*),
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(cache_creation_input_tokens), 0),
		        COALESCE(SUM(thinking_tokens), 0),
		        COALESCE(SUM(cost), 0),
		        COALESCE(AVG(latency_ms), 0)
		   FROM calls`+clause+`
		  GROUP BY k
		  ORDER BY COUNT(*) DESC, k ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: breakdown: %w", err)
	}
	defer rows.Close()

	var out []BreakdownRow
	for rows.Next() {
		var r BreakdownRow
		if err := rows.Scan(&r.Key, &r.Requests, &r.Errors,
			&r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.CacheCreationTokens,
			&r.ThinkingTokens, &r.Cost, &r.AvgLatencyMS); err != nil {
			return nil, fmt.Errorf("store: scan breakdown: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: breakdown: %w", err)
	}
	return out, nil
}

// ErrorClasses counts error rows by class over a window. Successful rows
// (status 2xx/3xx) are not counted in any field.
type ErrorClasses struct {
	RateLimited int // HTTP 429
	ClientError int // other 4xx
	ServerError int // 5xx
	Transport   int // status 0 (no response / transport failure)
}

// ErrorClassCounts groups error rows into {rate-limited, client, server,
// transport} over the optional [since, until) window.
func (s *Store) ErrorClassCounts(since, until *time.Time) (ErrorClasses, error) {
	clause, args := windowClause("", since, until)
	var c ErrorClasses
	err := s.db.QueryRow(
		`SELECT
		   COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status >= 400 AND status < 500 AND status != 429 THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN status = 0 THEN 1 ELSE 0 END), 0)
		 FROM calls`+clause, args...,
	).Scan(&c.RateLimited, &c.ClientError, &c.ServerError, &c.Transport)
	if err != nil {
		return ErrorClasses{}, fmt.Errorf("store: error class counts: %w", err)
	}
	return c, nil
}

// ErrorCodeRow is one upstream status code and how many error rows carried it
// over the queried window. Status 0 means a transport failure (no response).
type ErrorCodeRow struct {
	Status int
	Count  int
}

// TopErrorCodes returns error rows grouped by upstream status, ranked by count
// (desc, tie-broken by status asc), capped at limit. Only error rows are counted
// — status 0 (transport failure) or >= 400, matching isErrorStatus. When dim is a
// recognized dimension and key is non-empty, the count is scoped to rows whose
// dimension column equals key (e.g. one model, vendor, or user); an empty key
// leaves the result unscoped. An unrecognized non-empty dim returns
// ErrBadDimension. limit <= 0 defaults to 8. userID, when non-empty, further
// restricts the count to that consumer key's own calls (ANDed with any dim/key
// scope), so a user cannot read another user's error breakdown.
func (s *Store) TopErrorCodes(userID string, dim BreakdownDimension, key string, since, until *time.Time, limit int) ([]ErrorCodeRow, error) {
	if limit <= 0 {
		limit = 8
	}
	conds := []string{"(status = 0 OR status >= 400)"}
	var args []any
	if since != nil {
		conds = append(conds, "ts >= ?")
		args = append(args, since.UnixMilli())
	}
	if until != nil {
		conds = append(conds, "ts < ?")
		args = append(args, until.UnixMilli())
	}
	if key != "" {
		col, ok := breakdownColumn(dim)
		if !ok {
			return nil, ErrBadDimension
		}
		// col comes from the breakdownColumn whitelist, so it is safe to
		// interpolate (column names cannot be bound as query parameters).
		conds = append(conds, col+" = ?")
		args = append(args, key)
	}
	// Per-user scope is a separate conjunct so it composes with (dim,key): a user
	// probing another user's rows via ?dimension=user&key=<other> still gets
	// user_id = <other> AND user_id = <self> → empty.
	if userID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, userID)
	}
	args = append(args, limit)

	rows, err := s.db.Query(
		`SELECT status, COUNT(*) AS n
		   FROM calls
		  WHERE `+strings.Join(conds, " AND ")+`
		  GROUP BY status
		  ORDER BY n DESC, status ASC
		  LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: top error codes: %w", err)
	}
	defer rows.Close()

	out := make([]ErrorCodeRow, 0, limit)
	for rows.Next() {
		var r ErrorCodeRow
		if err := rows.Scan(&r.Status, &r.Count); err != nil {
			return nil, fmt.Errorf("store: scan top error codes: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: top error codes: %w", err)
	}
	return out, nil
}

// maxSeriesBuckets caps the number of buckets UsageSeries will produce, so an
// absurd range/bucket combination cannot allocate unbounded memory.
const maxSeriesBuckets = 10000

// ErrTooManyBuckets is returned by UsageSeries when the requested range/bucket
// combination would exceed maxSeriesBuckets. Callers can map it to a 400.
var ErrTooManyBuckets = errors.New("store: too many buckets")

// SeriesPoint is one bucket of the usage timeseries: the bucket start (UTC) and
// the cost/request/error/token totals for rows whose ts falls in that bucket.
// Performance averages exclude rows where the corresponding streaming timing is
// unavailable (stored as zero).
type SeriesPoint struct {
	Bucket              time.Time
	Cost                float64
	Requests            int
	Errors              int
	InputTokens         float64
	OutputTokens        float64
	CachedTokens        float64 // cache_read_input_tokens
	CacheCreationTokens float64
	ThinkingTokens      float64
	AvgLatencyMS        float64
	AvgTTFTMS           float64
	AvgOutputTokensSec  float64
}

// UsageSeries returns cost/request/error totals grouped into fixed time buckets
// across [since, until). bucket is time.Hour or 24*time.Hour. Bucket starts are
// aligned to the unix epoch. EVERY bucket in the range is present (gaps filled
// with zeroes) so the chart has no holes. Bucket timestamps are in UTC.
//
// An "error" is any row whose status is 0 (transport failure) or >= 400.
func (s *Store) UsageSeries(since, until time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	if bucket <= 0 {
		return nil, fmt.Errorf("store: usage series: bucket must be positive")
	}
	bucketMs := bucket.Milliseconds()
	if bucketMs <= 0 {
		return nil, fmt.Errorf("store: usage series: bucket too small")
	}

	// Align the range to bucket boundaries: the first bucket contains `since`,
	// and we emit buckets up to (but not including) `until`.
	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()
	startMs := (sinceMs / bucketMs) * bucketMs
	if untilMs <= startMs {
		return []SeriesPoint{}, nil
	}

	// Number of buckets from the aligned start up to the bucket containing the
	// last instant before `until`.
	count := (untilMs-startMs-1)/bucketMs + 1
	if count > maxSeriesBuckets {
		return nil, fmt.Errorf("%w: %d exceeds limit of %d", ErrTooManyBuckets, count, maxSeriesBuckets)
	}

	rows, err := s.db.Query(
		`SELECT (ts / ?) * ? AS bucket_start,
		        COALESCE(SUM(cost), 0),
		        COUNT(*),
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(cache_creation_input_tokens), 0),
		        COALESCE(SUM(thinking_tokens), 0),
		        COALESCE(AVG(latency_ms), 0),
		        COALESCE(AVG(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0),
		        COALESCE(AVG(CASE
		          WHEN generation_ms > 0 AND output_tokens > 0
		          THEN output_tokens * 1000.0 / generation_ms
		        END), 0)
		   FROM calls
		  WHERE ts >= ? AND ts < ?
		  GROUP BY bucket_start`,
		bucketMs, bucketMs, sinceMs, untilMs,
	)
	if err != nil {
		return nil, fmt.Errorf("store: usage series: %w", err)
	}
	defer rows.Close()

	type agg struct {
		cost         float64
		requests     int
		errors       int
		inTokens     float64
		outTokens    float64
		cacheTok     float64
		cacheCreate  float64
		thinkingTok  float64
		avgLat       float64
		avgTTFT      float64
		avgOutputTPS float64
	}
	byBucket := make(map[int64]agg)
	for rows.Next() {
		var (
			bucketStart int64
			a           agg
		)
		if err := rows.Scan(&bucketStart, &a.cost, &a.requests, &a.errors,
			&a.inTokens, &a.outTokens, &a.cacheTok, &a.cacheCreate, &a.thinkingTok,
			&a.avgLat, &a.avgTTFT, &a.avgOutputTPS); err != nil {
			return nil, fmt.Errorf("store: scan usage series: %w", err)
		}
		byBucket[bucketStart] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: usage series: %w", err)
	}

	out := make([]SeriesPoint, 0, count)
	for i := int64(0); i < count; i++ {
		bs := startMs + i*bucketMs
		p := SeriesPoint{Bucket: time.UnixMilli(bs).UTC()}
		if a, ok := byBucket[bs]; ok {
			p.Cost = a.cost
			p.Requests = a.requests
			p.Errors = a.errors
			p.InputTokens = a.inTokens
			p.OutputTokens = a.outTokens
			p.CachedTokens = a.cacheTok
			p.CacheCreationTokens = a.cacheCreate
			p.ThinkingTokens = a.thinkingTok
			p.AvgLatencyMS = a.avgLat
			p.AvgTTFTMS = a.avgTTFT
			p.AvgOutputTokensSec = a.avgOutputTPS
		}
		out = append(out, p)
	}
	return out, nil
}

// tokensByModelTopN caps the number of distinct model series in
// TokensByModelSeries; models beyond the cap are aggregated under "Other".
const tokensByModelTopN = 5

// otherModelKey is the synthetic key for tokens from models outside the top N.
const otherModelKey = "Other"

// TokensByModelBucket is one time bucket of the tokens-by-model series: the
// bucket start (UTC), the total cost over the bucket, total tokens
// (input+output) per model, cost per model, and per-model average TTFT and
// output throughput. Only the top models are kept as distinct keys; the
// remaining models are aggregated under "Other". Tokens, CostByModel,
// TTFTByModel, and TPSByModel all carry the same key set.
//
// TTFTByModel is the mean time-to-first-token (ms) over calls that reported a
// TTFT; TPSByModel is the mean output tokens/sec over calls that generated
// output — both per-call averages, matching UsageSeries. A key with no
// qualifying calls in the bucket reports 0.
type TokensByModelBucket struct {
	Bucket      time.Time
	Cost        float64
	Tokens      map[string]float64
	CostByModel map[string]float64
	TTFTByModel map[string]float64
	TPSByModel  map[string]float64
}

// TokensByModelSeries returns, for each fixed time bucket across [since, until),
// the total cost and total tokens (input+output) broken down by the given
// dimension (model, vendor, or user). The top tokensByModelTopN keys by total
// tokens over the whole range are kept as distinct series; every other key is
// summed into "Other". Every bucket in the range is present (gaps filled with
// zeroes), and every bucket's Tokens map carries the same key set. The returned
// slice is that key set, ordered descending by total tokens with "Other" (when
// present) last. Bucket timestamps are UTC. Empty key values are reported as
// "unknown". An unrecognized dimension returns ErrBadDimension.
func (s *Store) TokensByModelSeries(userID string, dim BreakdownDimension, since, until time.Time, bucket time.Duration) ([]string, []TokensByModelBucket, error) {
	col, ok := breakdownColumn(dim)
	if !ok {
		return nil, nil, ErrBadDimension
	}
	if bucket <= 0 {
		return nil, nil, fmt.Errorf("store: tokens by model series: bucket must be positive")
	}
	bucketMs := bucket.Milliseconds()
	if bucketMs <= 0 {
		return nil, nil, fmt.Errorf("store: tokens by model series: bucket too small")
	}

	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()
	startMs := (sinceMs / bucketMs) * bucketMs
	if untilMs <= startMs {
		return []string{}, []TokensByModelBucket{}, nil
	}
	count := (untilMs-startMs-1)/bucketMs + 1
	if count > maxSeriesBuckets {
		return nil, nil, fmt.Errorf("%w: %d exceeds limit of %d", ErrTooManyBuckets, count, maxSeriesBuckets)
	}

	// col comes from the breakdownColumn whitelist, so it is safe to interpolate
	// (column names cannot be bound as query parameters).
	userClause, userArgs := userScopeClause(userID)
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT (ts / ?) * ? AS bucket_start,
		        %s,
		        COALESCE(SUM(input_tokens + cache_read_input_tokens + cache_creation_input_tokens + output_tokens), 0),
		        COALESCE(SUM(cost), 0),
		        COALESCE(SUM(CASE WHEN ttft_ms > 0 THEN ttft_ms END), 0),
		        COUNT(CASE WHEN ttft_ms > 0 THEN 1 END),
		        COALESCE(SUM(CASE WHEN generation_ms > 0 AND output_tokens > 0 THEN output_tokens * 1000.0 / generation_ms END), 0),
		        COUNT(CASE WHEN generation_ms > 0 AND output_tokens > 0 THEN 1 END)
		   FROM calls
		  WHERE ts >= ? AND ts < ?%s
		  GROUP BY bucket_start, %s`, col, userClause, col),
		append([]any{bucketMs, bucketMs, sinceMs, untilMs}, userArgs...)...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: tokens by model series: %w", err)
	}
	defer rows.Close()

	// ttftSum/ttftN and tpsSum/tpsN are the numerator/denominator of each key's
	// per-call average, kept unreduced so they fold correctly into "Other".
	type cell struct {
		bucket  int64
		model   string
		tokens  float64
		cost    float64
		ttftSum float64
		ttftN   int64
		tpsSum  float64
		tpsN    int64
	}
	var cells []cell
	modelTotals := make(map[string]float64)
	bucketCost := make(map[int64]float64)
	for rows.Next() {
		var (
			b int64
			c cell
		)
		if err := rows.Scan(&b, &c.model, &c.tokens, &c.cost, &c.ttftSum, &c.ttftN, &c.tpsSum, &c.tpsN); err != nil {
			return nil, nil, fmt.Errorf("store: scan tokens by model series: %w", err)
		}
		if c.model == "" {
			c.model = "unknown"
		}
		c.bucket = b
		cells = append(cells, c)
		modelTotals[c.model] += c.tokens
		bucketCost[b] += c.cost
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: tokens by model series: %w", err)
	}

	// Rank models by total tokens (desc), tie-break by name (asc); keep top N.
	ranked := make([]string, 0, len(modelTotals))
	for m := range modelTotals {
		ranked = append(ranked, m)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if modelTotals[ranked[i]] != modelTotals[ranked[j]] {
			return modelTotals[ranked[i]] > modelTotals[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})

	top := make(map[string]bool)
	models := make([]string, 0, tokensByModelTopN+1)
	for _, m := range ranked {
		if len(models) >= tokensByModelTopN {
			break
		}
		top[m] = true
		models = append(models, m)
	}
	hasOther := len(ranked) > len(models)

	// Fold each cell into its bucket, remapping non-top models to "Other".
	// Tokens, cost, and the TTFT/TPS sum+count pairs are folded in parallel so
	// they share the same key set. Averages are deferred to emit time so the
	// "Other" group averages across all its folded calls.
	perBucket := make(map[int64]map[string]float64)
	perBucketCost := make(map[int64]map[string]float64)
	perBucketTTFTSum := make(map[int64]map[string]float64)
	perBucketTTFTN := make(map[int64]map[string]int64)
	perBucketTPSSum := make(map[int64]map[string]float64)
	perBucketTPSN := make(map[int64]map[string]int64)
	ensureF := func(m map[int64]map[string]float64, b int64) map[string]float64 {
		if m[b] == nil {
			m[b] = make(map[string]float64)
		}
		return m[b]
	}
	ensureI := func(m map[int64]map[string]int64, b int64) map[string]int64 {
		if m[b] == nil {
			m[b] = make(map[string]int64)
		}
		return m[b]
	}
	for _, c := range cells {
		key := c.model
		if !top[key] {
			key = otherModelKey
		}
		ensureF(perBucket, c.bucket)[key] += c.tokens
		ensureF(perBucketCost, c.bucket)[key] += c.cost
		ensureF(perBucketTTFTSum, c.bucket)[key] += c.ttftSum
		ensureI(perBucketTTFTN, c.bucket)[key] += c.ttftN
		ensureF(perBucketTPSSum, c.bucket)[key] += c.tpsSum
		ensureI(perBucketTPSN, c.bucket)[key] += c.tpsN
	}
	if hasOther {
		models = append(models, otherModelKey)
	}

	out := make([]TokensByModelBucket, 0, count)
	for i := int64(0); i < count; i++ {
		bs := startMs + i*bucketMs
		tokens := make(map[string]float64, len(models))
		costByModel := make(map[string]float64, len(models))
		ttftByModel := make(map[string]float64, len(models))
		tpsByModel := make(map[string]float64, len(models))
		for _, m := range models {
			tokens[m] = 0
			costByModel[m] = 0
			ttftByModel[m] = 0
			tpsByModel[m] = 0
		}
		for m, v := range perBucket[bs] {
			tokens[m] += v
		}
		for m, v := range perBucketCost[bs] {
			costByModel[m] += v
		}
		for m, n := range perBucketTTFTN[bs] {
			if n > 0 {
				ttftByModel[m] = perBucketTTFTSum[bs][m] / float64(n)
			}
		}
		for m, n := range perBucketTPSN[bs] {
			if n > 0 {
				tpsByModel[m] = perBucketTPSSum[bs][m] / float64(n)
			}
		}
		out = append(out, TokensByModelBucket{
			Bucket:      time.UnixMilli(bs).UTC(),
			Cost:        bucketCost[bs],
			Tokens:      tokens,
			CostByModel: costByModel,
			TTFTByModel: ttftByModel,
			TPSByModel:  tpsByModel,
		})
	}
	return models, out, nil
}

// SuccessByModelBucket is one time bucket of the success-rate series: the bucket
// start (UTC) and the request/error counts per dimension key. Requests and Errors
// carry the same key set (top N by request volume + "Other"). Callers derive the
// per-key success rate as (Requests-Errors)/Requests.
type SuccessByModelBucket struct {
	Bucket   time.Time
	Requests map[string]int
	Errors   map[string]int
}

// SuccessByModelSeries returns, for each fixed time bucket across [since, until),
// the request and error counts broken down by the given dimension (model, vendor,
// or user). The top tokensByModelTopN keys by total request count over the whole
// range are kept as distinct series; every other key is summed into "Other". Every
// bucket in the range is present (gaps filled with zeroes), and every bucket's maps
// carry the same key set. The returned slice is that key set, ordered descending by
// total requests with "Other" (when present) last. Bucket timestamps are UTC. An
// "error" is any row whose status is 0 (transport failure) or >= 400. Empty key
// values are reported as "unknown". An unrecognized dimension returns ErrBadDimension.
func (s *Store) SuccessByModelSeries(userID string, dim BreakdownDimension, since, until time.Time, bucket time.Duration) ([]string, []SuccessByModelBucket, error) {
	col, ok := breakdownColumn(dim)
	if !ok {
		return nil, nil, ErrBadDimension
	}
	if bucket <= 0 {
		return nil, nil, fmt.Errorf("store: success by model series: bucket must be positive")
	}
	bucketMs := bucket.Milliseconds()
	if bucketMs <= 0 {
		return nil, nil, fmt.Errorf("store: success by model series: bucket too small")
	}

	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()
	startMs := (sinceMs / bucketMs) * bucketMs
	if untilMs <= startMs {
		return []string{}, []SuccessByModelBucket{}, nil
	}
	count := (untilMs-startMs-1)/bucketMs + 1
	if count > maxSeriesBuckets {
		return nil, nil, fmt.Errorf("%w: %d exceeds limit of %d", ErrTooManyBuckets, count, maxSeriesBuckets)
	}

	// col comes from the breakdownColumn whitelist, so it is safe to interpolate
	// (column names cannot be bound as query parameters).
	userClause, userArgs := userScopeClause(userID)
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT (ts / ?) * ? AS bucket_start,
		        %s,
		        COUNT(*),
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END)
		   FROM calls
		  WHERE ts >= ? AND ts < ?%s
		  GROUP BY bucket_start, %s`, col, userClause, col),
		append([]any{bucketMs, bucketMs, sinceMs, untilMs}, userArgs...)...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: success by model series: %w", err)
	}
	defer rows.Close()

	type cell struct {
		bucket   int64
		model    string
		requests int
		errors   int
	}
	var cells []cell
	modelTotals := make(map[string]int)
	for rows.Next() {
		var (
			b        int64
			model    string
			requests int
			errCount int
		)
		if err := rows.Scan(&b, &model, &requests, &errCount); err != nil {
			return nil, nil, fmt.Errorf("store: scan success by model series: %w", err)
		}
		if model == "" {
			model = "unknown"
		}
		cells = append(cells, cell{bucket: b, model: model, requests: requests, errors: errCount})
		modelTotals[model] += requests
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: success by model series: %w", err)
	}

	// Rank models by total requests (desc), tie-break by name (asc); keep top N.
	ranked := make([]string, 0, len(modelTotals))
	for m := range modelTotals {
		ranked = append(ranked, m)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if modelTotals[ranked[i]] != modelTotals[ranked[j]] {
			return modelTotals[ranked[i]] > modelTotals[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})

	top := make(map[string]bool)
	models := make([]string, 0, tokensByModelTopN+1)
	for _, m := range ranked {
		if len(models) >= tokensByModelTopN {
			break
		}
		top[m] = true
		models = append(models, m)
	}
	hasOther := len(ranked) > len(models)

	// Fold each cell into its bucket, remapping non-top models to "Other".
	// Requests and errors are folded in parallel so they share the same key set.
	perBucketReq := make(map[int64]map[string]int)
	perBucketErr := make(map[int64]map[string]int)
	for _, c := range cells {
		key := c.model
		if !top[key] {
			key = otherModelKey
		}
		mr := perBucketReq[c.bucket]
		if mr == nil {
			mr = make(map[string]int)
			perBucketReq[c.bucket] = mr
		}
		mr[key] += c.requests
		me := perBucketErr[c.bucket]
		if me == nil {
			me = make(map[string]int)
			perBucketErr[c.bucket] = me
		}
		me[key] += c.errors
	}
	if hasOther {
		models = append(models, otherModelKey)
	}

	out := make([]SuccessByModelBucket, 0, count)
	for i := int64(0); i < count; i++ {
		bs := startMs + i*bucketMs
		requests := make(map[string]int, len(models))
		errCounts := make(map[string]int, len(models))
		for _, m := range models {
			requests[m] = 0
			errCounts[m] = 0
		}
		for m, v := range perBucketReq[bs] {
			requests[m] += v
		}
		for m, v := range perBucketErr[bs] {
			errCounts[m] += v
		}
		out = append(out, SuccessByModelBucket{
			Bucket:   time.UnixMilli(bs).UTC(),
			Requests: requests,
			Errors:   errCounts,
		})
	}
	return models, out, nil
}

// CacheByModelBucket is one time bucket of the cache-hit series: the bucket start
// (UTC) and the cache-read and total-input token sums per dimension key. CacheRead
// and Input carry the same key set (top N by total input + "Other"). Callers derive
// the per-key cache-hit ratio as CacheRead/Input, where total input is fresh input +
// cache read + cache creation (the three disjoint input buckets).
type CacheByModelBucket struct {
	Bucket    time.Time
	CacheRead map[string]float64
	Input     map[string]float64
}

// CacheByModelSeries returns, for each fixed time bucket across [since, until), the
// cache-read and total-input token sums broken down by the given dimension (model,
// vendor, or user). Cache read is the ratio's numerator; total input (fresh input +
// cache read + cache creation) is its denominator — both are summed raw here so the
// caller can divide after folding, keeping the "Other" group's ratio correct. The
// top tokensByModelTopN keys by total input over the whole range are kept as distinct
// series; every other key is summed into "Other". Every bucket in the range is
// present (gaps filled with zeroes), and every bucket's maps carry the same key set.
// The returned slice is that key set, ordered descending by total input with "Other"
// (when present) last. Bucket timestamps are UTC. Empty key values are reported as
// "unknown". An unrecognized dimension returns ErrBadDimension.
func (s *Store) CacheByModelSeries(userID string, dim BreakdownDimension, since, until time.Time, bucket time.Duration) ([]string, []CacheByModelBucket, error) {
	col, ok := breakdownColumn(dim)
	if !ok {
		return nil, nil, ErrBadDimension
	}
	if bucket <= 0 {
		return nil, nil, fmt.Errorf("store: cache by model series: bucket must be positive")
	}
	bucketMs := bucket.Milliseconds()
	if bucketMs <= 0 {
		return nil, nil, fmt.Errorf("store: cache by model series: bucket too small")
	}

	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()
	startMs := (sinceMs / bucketMs) * bucketMs
	if untilMs <= startMs {
		return []string{}, []CacheByModelBucket{}, nil
	}
	count := (untilMs-startMs-1)/bucketMs + 1
	if count > maxSeriesBuckets {
		return nil, nil, fmt.Errorf("%w: %d exceeds limit of %d", ErrTooManyBuckets, count, maxSeriesBuckets)
	}

	// col comes from the breakdownColumn whitelist, so it is safe to interpolate
	// (column names cannot be bound as query parameters).
	userClause, userArgs := userScopeClause(userID)
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT (ts / ?) * ? AS bucket_start,
		        %s,
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(input_tokens + cache_read_input_tokens + cache_creation_input_tokens), 0)
		   FROM calls
		  WHERE ts >= ? AND ts < ?%s
		  GROUP BY bucket_start, %s`, col, userClause, col),
		append([]any{bucketMs, bucketMs, sinceMs, untilMs}, userArgs...)...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: cache by model series: %w", err)
	}
	defer rows.Close()

	type cell struct {
		bucket    int64
		model     string
		cacheRead float64
		input     float64
	}
	var cells []cell
	modelTotals := make(map[string]float64)
	for rows.Next() {
		var (
			b         int64
			model     string
			cacheRead float64
			input     float64
		)
		if err := rows.Scan(&b, &model, &cacheRead, &input); err != nil {
			return nil, nil, fmt.Errorf("store: scan cache by model series: %w", err)
		}
		if model == "" {
			model = "unknown"
		}
		cells = append(cells, cell{bucket: b, model: model, cacheRead: cacheRead, input: input})
		modelTotals[model] += input
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: cache by model series: %w", err)
	}

	// Rank models by total input (desc), tie-break by name (asc); keep top N.
	ranked := make([]string, 0, len(modelTotals))
	for m := range modelTotals {
		ranked = append(ranked, m)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if modelTotals[ranked[i]] != modelTotals[ranked[j]] {
			return modelTotals[ranked[i]] > modelTotals[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})

	top := make(map[string]bool)
	models := make([]string, 0, tokensByModelTopN+1)
	for _, m := range ranked {
		if len(models) >= tokensByModelTopN {
			break
		}
		top[m] = true
		models = append(models, m)
	}
	hasOther := len(ranked) > len(models)

	// Fold each cell into its bucket, remapping non-top models to "Other".
	// CacheRead and Input are folded in parallel so they share the same key set.
	perBucketCache := make(map[int64]map[string]float64)
	perBucketInput := make(map[int64]map[string]float64)
	for _, c := range cells {
		key := c.model
		if !top[key] {
			key = otherModelKey
		}
		mc := perBucketCache[c.bucket]
		if mc == nil {
			mc = make(map[string]float64)
			perBucketCache[c.bucket] = mc
		}
		mc[key] += c.cacheRead
		mi := perBucketInput[c.bucket]
		if mi == nil {
			mi = make(map[string]float64)
			perBucketInput[c.bucket] = mi
		}
		mi[key] += c.input
	}
	if hasOther {
		models = append(models, otherModelKey)
	}

	out := make([]CacheByModelBucket, 0, count)
	for i := int64(0); i < count; i++ {
		bs := startMs + i*bucketMs
		cacheRead := make(map[string]float64, len(models))
		input := make(map[string]float64, len(models))
		for _, m := range models {
			cacheRead[m] = 0
			input[m] = 0
		}
		for m, v := range perBucketCache[bs] {
			cacheRead[m] += v
		}
		for m, v := range perBucketInput[bs] {
			input[m] += v
		}
		out = append(out, CacheByModelBucket{
			Bucket:    time.UnixMilli(bs).UTC(),
			CacheRead: cacheRead,
			Input:     input,
		})
	}
	return models, out, nil
}

// isErrorStatus reports whether a recorded upstream status counts as an error:
// 0 (transport failure / no response) or any 4xx/5xx.
func isErrorStatus(status int) bool {
	return status == 0 || status >= 400
}

// percentileNearestRank returns the p-th percentile (1..100) of an
// ascending-sorted slice using the nearest-rank method. It returns 0 for an
// empty slice. The input is assumed sorted; it is defensively re-sorted only
// if a caller passes unsorted data is not a concern here since callers sort.
func percentileNearestRank(sorted []int64, p int) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if !sortedAsc(sorted) {
		// Defensive: copy and sort so the method is correct regardless of input.
		cp := append([]int64(nil), sorted...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		sorted = cp
	}
	// Nearest-rank: rank = ceil(p/100 * n), 1-based.
	rank := (p*n + 99) / 100 // == ceil(p*n/100)
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

func percentileNearestRankFloat(values []float64, p int) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	rank := (p*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

// sortedAsc reports whether s is in non-decreasing order.
func sortedAsc(s []int64) bool {
	for i := 1; i < len(s); i++ {
		if s[i] < s[i-1] {
			return false
		}
	}
	return true
}

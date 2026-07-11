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
func windowClause(since, until *time.Time) (string, []any) {
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
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// OverviewStats returns total requests, error count, and p50/p95/p99 latency
// (ms) over the optional [since, until) window. It pulls the latencies sorted
// from SQLite and computes percentiles in Go via nearest-rank.
func (s *Store) OverviewStats(since, until *time.Time) (OverviewStats, error) {
	clause, args := windowClause(since, until)

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
	clause, args := windowClause(since, until)

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
	clause, args := windowClause(since, until)

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

// TokenTotals holds summed normalized token counts over a window.
type TokenTotals struct {
	Input  float64
	Output float64
	Cached float64
}

// TokenTotals sums normalized input/output/cached tokens over the optional
// [since, until) window.
func (s *Store) TokenTotals(since, until *time.Time) (TokenTotals, error) {
	clause, args := windowClause(since, until)
	var t TokenTotals
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0)
		   FROM calls`+clause, args...,
	).Scan(&t.Input, &t.Output, &t.Cached)
	if err != nil {
		return TokenTotals{}, fmt.Errorf("store: token totals: %w", err)
	}
	return t, nil
}

// DistinctUsers counts distinct non-empty user_ids with at least one call in the
// optional [since, until) window. The empty user id (admin/unknown traffic) is
// excluded so the count reflects real callers.
func (s *Store) DistinctUsers(since, until *time.Time) (int, error) {
	clause, args := windowClause(since, until)
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

// BreakdownRow is one group's aggregates in a Breakdown result.
type BreakdownRow struct {
	Key          string
	Requests     int
	Errors       int
	InputTokens  float64
	OutputTokens float64
	CachedTokens float64
	Cost         float64
	AvgLatencyMS float64
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
	clause, args := windowClause(since, until)
	rows, err := s.db.Query(
		`SELECT `+col+` AS k,
		        COUNT(*),
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0),
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
			&r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.Cost, &r.AvgLatencyMS); err != nil {
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
	clause, args := windowClause(since, until)
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
	Bucket             time.Time
	Cost               float64
	Requests           int
	Errors             int
	InputTokens        float64
	OutputTokens       float64
	CachedTokens       float64
	AvgLatencyMS       float64
	AvgTTFTMS          float64
	AvgOutputTokensSec float64
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
		        COALESCE(SUM(cached_tokens), 0),
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
			&a.inTokens, &a.outTokens, &a.cacheTok, &a.avgLat, &a.avgTTFT, &a.avgOutputTPS); err != nil {
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
// (input+output) per model, and cost per model. Only the top models are kept as
// distinct keys; the remaining models are aggregated under "Other". Tokens and
// CostByModel carry the same key set.
type TokensByModelBucket struct {
	Bucket      time.Time
	Cost        float64
	Tokens      map[string]float64
	CostByModel map[string]float64
}

// TokensByModelSeries returns, for each fixed time bucket across [since, until),
// the total cost and total tokens (input+output) broken down by model. The top
// tokensByModelTopN models by total tokens over the whole range are kept as
// distinct keys; every other model is summed into "Other". Every bucket in the
// range is present (gaps filled with zeroes), and every bucket's Tokens map
// carries the same key set. The returned slice is that key set, ordered
// descending by total tokens with "Other" (when present) last. Bucket
// timestamps are UTC. Empty model names are reported as "unknown".
func (s *Store) TokensByModelSeries(since, until time.Time, bucket time.Duration) ([]string, []TokensByModelBucket, error) {
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

	rows, err := s.db.Query(
		`SELECT (ts / ?) * ? AS bucket_start,
		        model,
		        COALESCE(SUM(input_tokens + output_tokens), 0),
		        COALESCE(SUM(cost), 0)
		   FROM calls
		  WHERE ts >= ? AND ts < ?
		  GROUP BY bucket_start, model`,
		bucketMs, bucketMs, sinceMs, untilMs,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: tokens by model series: %w", err)
	}
	defer rows.Close()

	type cell struct {
		bucket int64
		model  string
		tokens float64
		cost   float64
	}
	var cells []cell
	modelTotals := make(map[string]float64)
	bucketCost := make(map[int64]float64)
	for rows.Next() {
		var (
			b      int64
			model  string
			tokens float64
			cost   float64
		)
		if err := rows.Scan(&b, &model, &tokens, &cost); err != nil {
			return nil, nil, fmt.Errorf("store: scan tokens by model series: %w", err)
		}
		if model == "" {
			model = "unknown"
		}
		cells = append(cells, cell{bucket: b, model: model, tokens: tokens, cost: cost})
		modelTotals[model] += tokens
		bucketCost[b] += cost
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
	// Tokens and cost are folded in parallel so they share the same key set.
	perBucket := make(map[int64]map[string]float64)
	perBucketCost := make(map[int64]map[string]float64)
	for _, c := range cells {
		key := c.model
		if !top[key] {
			key = otherModelKey
		}
		m := perBucket[c.bucket]
		if m == nil {
			m = make(map[string]float64)
			perBucket[c.bucket] = m
		}
		m[key] += c.tokens
		mc := perBucketCost[c.bucket]
		if mc == nil {
			mc = make(map[string]float64)
			perBucketCost[c.bucket] = mc
		}
		mc[key] += c.cost
	}
	if hasOther {
		models = append(models, otherModelKey)
	}

	out := make([]TokensByModelBucket, 0, count)
	for i := int64(0); i < count; i++ {
		bs := startMs + i*bucketMs
		tokens := make(map[string]float64, len(models))
		costByModel := make(map[string]float64, len(models))
		for _, m := range models {
			tokens[m] = 0
			costByModel[m] = 0
		}
		for m, v := range perBucket[bs] {
			tokens[m] += v
		}
		for m, v := range perBucketCost[bs] {
			costByModel[m] += v
		}
		out = append(out, TokensByModelBucket{
			Bucket:      time.UnixMilli(bs).UTC(),
			Cost:        bucketCost[bs],
			Tokens:      tokens,
			CostByModel: costByModel,
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

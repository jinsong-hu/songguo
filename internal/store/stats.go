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
	Requests int
	Errors   int
	P50      int64
	P95      int64
	P99      int64
}

// VendorStat holds per-vendor request/error counts, average latency, and the
// status of the most recent row (by ts) for that vendor.
type VendorStat struct {
	Requests   int
	Errors     int
	AvgLatency float64 // milliseconds
	LastStatus int     // status of the most recent row for this vendor
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
		`SELECT latency_ms, status FROM calls`+clause+` ORDER BY latency_ms ASC`,
		args...,
	)
	if err != nil {
		return OverviewStats{}, fmt.Errorf("store: overview stats: %w", err)
	}
	defer rows.Close()

	var (
		out       OverviewStats
		latencies []int64
	)
	for rows.Next() {
		var (
			latency int64
			status  int
		)
		if err := rows.Scan(&latency, &status); err != nil {
			return OverviewStats{}, fmt.Errorf("store: scan overview stats: %w", err)
		}
		out.Requests++
		if isErrorStatus(status) {
			out.Errors++
		}
		latencies = append(latencies, latency)
	}
	if err := rows.Err(); err != nil {
		return OverviewStats{}, fmt.Errorf("store: overview stats: %w", err)
	}

	// latencies is already sorted ascending by the query.
	out.P50 = percentileNearestRank(latencies, 50)
	out.P95 = percentileNearestRank(latencies, 95)
	out.P99 = percentileNearestRank(latencies, 99)
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

	// Resolve the last status per vendor: the row with the largest id (the
	// calls table is append-only, so the max id is the most recent row) within the window.
	lastRows, err := s.db.Query(
		`SELECT l.vendor, l.status
		   FROM calls l
		   JOIN (SELECT vendor, MAX(id) AS mid FROM calls`+clause+` GROUP BY vendor) m
		     ON l.vendor = m.vendor AND l.id = m.mid`,
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

// maxSeriesBuckets caps the number of buckets UsageSeries will produce, so an
// absurd range/bucket combination cannot allocate unbounded memory.
const maxSeriesBuckets = 10000

// ErrTooManyBuckets is returned by UsageSeries when the requested range/bucket
// combination would exceed maxSeriesBuckets. Callers can map it to a 400.
var ErrTooManyBuckets = errors.New("store: too many buckets")

// SeriesPoint is one bucket of the usage timeseries: the bucket start (UTC) and
// the cost/request/error totals for rows whose ts falls in that bucket.
type SeriesPoint struct {
	Bucket   time.Time
	Cost     float64
	Requests int
	Errors   int
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
		        SUM(CASE WHEN status = 0 OR status >= 400 THEN 1 ELSE 0 END)
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
		cost     float64
		requests int
		errors   int
	}
	byBucket := make(map[int64]agg)
	for rows.Next() {
		var (
			bucketStart int64
			a           agg
		)
		if err := rows.Scan(&bucketStart, &a.cost, &a.requests, &a.errors); err != nil {
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
		}
		out = append(out, p)
	}
	return out, nil
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

// sortedAsc reports whether s is in non-decreasing order.
func sortedAsc(s []int64) bool {
	for i := 1; i < len(s); i++ {
		if s[i] < s[i-1] {
			return false
		}
	}
	return true
}

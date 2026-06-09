package store

import (
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
		`SELECT latency_ms, status FROM ledger`+clause+` ORDER BY latency_ms ASC`,
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
		   FROM ledger`+clause+`
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

	// Resolve the last status per vendor: the row with the largest id (ledger
	// is append-only, so the max id is the most recent row) within the window.
	lastRows, err := s.db.Query(
		`SELECT l.vendor, l.status
		   FROM ledger l
		   JOIN (SELECT vendor, MAX(id) AS mid FROM ledger`+clause+` GROUP BY vendor) m
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

package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/calls"
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

// Scope narrows which rows of the call log an aggregate query considers. It
// carries two kinds of narrowing that happen to share a WHERE clause but not a
// provenance, and the difference matters:
//
//   - UserID is server-enforced. It comes from the authenticated key (see
//     api.scopeUserID) and is never read from the query string. "" means the
//     operator view — all traffic.
//   - Models, Vendors and Clients are the operator's own dashboard filters, read
//     from the request. They can only ever narrow what the caller may already
//     see, so they need no enforcement of their own.
//
// An empty slice means "no filter", matching the convention User.Scope already
// uses for model allowlists — none selected = all.
type Scope struct {
	UserID  string
	Models  []string
	Vendors []string
	// Clients filters on the normalized caller client (calls.ParseClientInfo:
	// claude-code, codex-openai). Unlike Models and Vendors the option list is
	// not exhaustive — an unrecognized caller stores '' and is offered by no
	// facet — so selecting every option is narrower than selecting none. See
	// Facets for why we decline to synthesize an "Other".
	Clients []string
}

// filtered reports whether any model/provider/client filter is set. Callers that
// must reach for a more expensive query shape when filtering (SessionStats, which
// aggregates a rollup table with no model column) branch on this.
func (sc Scope) filtered() bool {
	return len(sc.Models) > 0 || len(sc.Vendors) > 0 || len(sc.Clients) > 0
}

// conds returns the scope's WHERE conjuncts and their bound args, all predicates
// on the `calls` table. Column names are literals here, never caller input.
func (sc Scope) conds() ([]string, []any) {
	var (
		conds []string
		args  []any
	)
	if sc.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, sc.UserID)
	}
	if c, a := inClause("model", sc.Models); c != "" {
		conds = append(conds, c)
		args = append(args, a...)
	}
	if c, a := inClause("vendor", sc.Vendors); c != "" {
		conds = append(conds, c)
		args = append(args, a...)
	}
	if c, a := inClause("client_name", sc.Clients); c != "" {
		conds = append(conds, c)
		args = append(args, a...)
	}
	return conds, args
}

// inClause builds a `col IN (?, ?, ...)` predicate for a value allowlist. An
// empty list yields an empty clause — no filter — rather than `IN ()`, which
// would match nothing and silently blank the dashboard.
func inClause(col string, vals []string) (string, []any) {
	if len(vals) == 0 {
		return "", nil
	}
	args := make([]any, len(vals))
	placeholders := make([]string, len(vals))
	for i, v := range vals {
		placeholders[i] = "?"
		args[i] = v
	}
	return col + " IN (" + strings.Join(placeholders, ", ") + ")", args
}

// windowClause builds the time-window (and scope) WHERE clause shared by the
// aggregate stats queries over `calls`. A zero Scope leaves the query unscoped
// (the operator/admin view, all models, all providers).
func windowClause(sc Scope, since, until *time.Time) (string, []any) {
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
	scopeConds, scopeArgs := sc.conds()
	conds = append(conds, scopeConds...)
	args = append(args, scopeArgs...)
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// scopeClause returns a trailing ` AND ...` fragment (and its bound args) for the
// series queries that build their WHERE by hand. A zero Scope yields an empty
// clause and no args, leaving the query unscoped.
func scopeClause(sc Scope) (string, []any) {
	conds, args := sc.conds()
	if len(conds) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(conds, " AND "), args
}

// OverviewStats returns total requests, error count, and p50/p95/p99 latency
// (ms) over the optional [since, until) window. It pulls the latencies sorted
// from SQLite and computes percentiles in Go via nearest-rank.
func (s *Store) OverviewStats(sc Scope, since, until *time.Time) (OverviewStats, error) {
	clause, args := windowClause(sc, since, until)

	rows, err := s.db.Query(
		`SELECT latency_ms, status, err, ttft_ms, generation_ms, output_tokens
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
			callErr      string
			ttft         int64
			generation   int64
			outputTokens float64
		)
		if err := rows.Scan(&latency, &status, &callErr, &ttft, &generation, &outputTokens); err != nil {
			return OverviewStats{}, fmt.Errorf("store: scan overview stats: %w", err)
		}
		// A call with no verdict is not a request that succeeded, and its
		// latency_ms is 0 from the create-at-start row — left in, those zeros
		// sorted first and dragged every percentile down.
		if !hasVerdict(status) {
			continue
		}
		out.Requests++
		if isErrorStatus(status, callErr) {
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
	clause, args := windowClause(Scope{}, since, until)

	// Aggregate counts and average latency per vendor.
	aggRows, err := s.db.Query(
		`SELECT vendor,
		        `+sqlCountWhere(sqlHasVerdict)+`,
		        `+sqlCountWhere(sqlProviderFailed)+`,
		        COALESCE(AVG(CASE WHEN `+sqlHasVerdict+` THEN latency_ms END), 0)
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
		   JOIN (SELECT vendor, MAX(ts) AS mts FROM calls`+clause+lastStatusFilter(clause)+`
		          GROUP BY vendor) m
		     ON l.vendor = m.vendor AND l.ts = m.mts
		  WHERE `+sqlHasVerdict+`
		  ORDER BY l.id`,
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
	clause, args := windowClause(Scope{}, since, until)

	rows, err := s.db.Query(
		`SELECT model,
		        `+sqlCountWhere(sqlHasVerdict)+`,
		        `+sqlCountWhere(sqlFailed)+`,
		        COALESCE(AVG(CASE WHEN `+sqlHasVerdict+` THEN latency_ms END), 0)
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
func (s *Store) TokenTotals(sc Scope, since, until *time.Time) (TokenTotals, error) {
	clause, args := windowClause(sc, since, until)
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
// excluded so the count reflects real callers. The scope's model/provider
// filters narrow which calls count, so the KPI answers "callers of the models
// I am looking at" rather than "callers of anything".
func (s *Store) DistinctUsers(sc Scope, since, until *time.Time) (int, error) {
	clause, args := windowClause(sc, since, until)
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

// FacetRow is one selectable value of a dashboard filter, with the request count
// that earned it a place in the list.
type FacetRow struct {
	Key      string
	Requests int
}

// Facets lists the distinct models, vendors and clients that actually appear in
// the call log over the window, ranked by request count — the option lists behind
// the Overview page's Models, Providers and Clients filters.
//
// Three deliberate choices:
//
//   - Observed, not configured. A provider may declare fifty models; the filter
//     offers the handful that ran. This keeps the list short and means every
//     option is guaranteed to change what you see.
//   - No cross-filtering. Only sc.UserID narrows this query — a caller's own
//     model/provider selections are ignored, so picking a model never removes
//     providers from the other list. Cross-filtered facets read as options
//     "disappearing" and can strand a selection the user can no longer clear.
//   - Recognized clients only, and no synthesized "Other". The HAVING clause
//     drops the rows whose User-Agent named no client ParseClientInfo knows, and
//     those rows get no facet of their own. An empty client_name does not mean
//     "some other agent" — it is curl, a raw SDK, a browser, a health check — so
//     offering it beside Claude Code and Codex would assert a peer client that
//     does not exist. It is also not a stable bucket: teaching songguo a third
//     client would silently move historical rows out of it, making it the one
//     option whose meaning changes between releases. The consequence, unique to
//     this column: the client list is not exhaustive, so selecting every option
//     is narrower than selecting none. If "Other" is ever wanted, the honest
//     route is for ParseClientInfo to *record* a value for an
//     unrecognized-but-present UA, not for this query to invent one at read time.
func (s *Store) Facets(sc Scope, since, until *time.Time) (models, vendors, clients []FacetRow, err error) {
	scope := Scope{UserID: sc.UserID}
	clause, args := windowClause(scope, since, until)

	load := func(col string) ([]FacetRow, error) {
		// col is a literal from this function's own call sites, never input.
		rows, qerr := s.db.Query(
			`SELECT `+col+` AS k, COUNT(*) AS n
			   FROM calls`+clause+`
			  GROUP BY k
			 HAVING k != ''
			  ORDER BY n DESC, k ASC`,
			args...,
		)
		if qerr != nil {
			return nil, fmt.Errorf("store: facets %s: %w", col, qerr)
		}
		defer rows.Close()
		out := []FacetRow{}
		for rows.Next() {
			var r FacetRow
			if serr := rows.Scan(&r.Key, &r.Requests); serr != nil {
				return nil, fmt.Errorf("store: scan facets %s: %w", col, serr)
			}
			out = append(out, r)
		}
		return out, rows.Err()
	}

	if models, err = load("model"); err != nil {
		return nil, nil, nil, err
	}
	if vendors, err = load("vendor"); err != nil {
		return nil, nil, nil, err
	}
	if clients, err = load("client_name"); err != nil {
		return nil, nil, nil, err
	}
	return models, vendors, clients, nil
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
	clause, args := windowClause(Scope{}, since, until)
	rows, err := s.db.Query(
		`SELECT `+col+` AS k,
		        `+sqlCountWhere(sqlHasVerdict)+`,
		        `+sqlCountWhere(sqlFailed)+`,
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(cache_creation_input_tokens), 0),
		        COALESCE(SUM(thinking_tokens), 0),
		        COALESCE(SUM(cost), 0),
		        COALESCE(AVG(CASE WHEN `+sqlHasVerdict+` THEN latency_ms END), 0)
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

// ErrorClasses counts error rows by class over a window. Rows that served, that
// the caller cancelled, and that have not finished are counted in no field.
//
// The classes are disjoint and split on WHOSE failure it was before splitting on
// the code, because the code alone cannot say: songguo's own 429 and a
// provider's 429 are the same integer.
type ErrorClasses struct {
	RateLimited int // provider's 429
	ClientError int // provider's other 4xx
	ServerError int // provider's 5xx
	Transport   int // never reached the provider, or the stream broke mid-body
	Gateway     int // songguo refused it: budget, rate, scope, no wire, no route
}

// ErrorClassCounts groups failures into {rate-limited, client, server,
// transport, gateway} over the optional [since, until) window.
func (s *Store) ErrorClassCounts(since, until *time.Time) (ErrorClasses, error) {
	clause, args := windowClause(Scope{}, since, until)
	var c ErrorClasses
	// Forwarded rows carry err = ''; the status on those is the provider's own.
	// Anything with a slug is ours, the caller's, or a transport/stream failure,
	// and must not be filed under the provider's status classes.
	forwarded := `err = '' AND ` + sqlHasVerdict
	err := s.db.QueryRow(
		`SELECT
		   COALESCE(`+sqlCountWhere(forwarded+` AND status = 429`)+`, 0),
		   COALESCE(`+sqlCountWhere(forwarded+` AND status >= 400 AND status < 500 AND status != 429`)+`, 0),
		   COALESCE(`+sqlCountWhere(forwarded+` AND status >= 500`)+`, 0),
		   COALESCE(`+sqlCountWhere(
			// status = 0 is legacy rows, which had no slug to carry the detail.
			sqlHasVerdict+` AND (err LIKE 'transport_error:%' OR err LIKE 'stream_error:%'`+
				` OR (err = '' AND status = 0))`)+`, 0),
		   COALESCE(`+sqlCountWhere(sqlFailed+` AND NOT (`+sqlProviderFailed+`)`)+`, 0)
		 FROM calls`+clause, args...,
	).Scan(&c.RateLimited, &c.ClientError, &c.ServerError, &c.Transport, &c.Gateway)
	if err != nil {
		return ErrorClasses{}, fmt.Errorf("store: error class counts: %w", err)
	}
	return c, nil
}

// ErrorCodeRow is one failure kind and how many rows carried it over the
// queried window.
//
// Status alone is not the key. songguo mints statuses of its own, so grouping by
// the integer merged its 429 with the provider's, its 403 with the provider's,
// and four distinct causes into one 502. Outcome is what separates them; Status
// is kept alongside so the real code is still displayable.
type ErrorCodeRow struct {
	Status  int
	Outcome string // calls.Outcome — what actually happened
	Count   int
}

// TopErrorCodes returns error rows grouped by upstream status, ranked by count
// (desc, tie-broken by status asc), capped at limit. Only error rows are counted
// — status 0 (transport failure) or >= 400, matching isErrorStatus. When dim is a
// recognized dimension and key is non-empty, the count is scoped to rows whose
// dimension column equals key (e.g. one model, vendor, or user); an empty key
// leaves the result unscoped. An unrecognized non-empty dim returns
// ErrBadDimension. limit <= 0 defaults to 8. The scope further restricts the
// count — to that consumer key's own calls, and to the dashboard's selected
// models/providers — ANDed with any dim/key scope, so a user cannot read another
// user's error breakdown.
func (s *Store) TopErrorCodes(sc Scope, dim BreakdownDimension, key string, since, until *time.Time, limit int) ([]ErrorCodeRow, error) {
	if limit <= 0 {
		limit = 8
	}
	conds := []string{"(" + sqlFailed + ")"}
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
	// Scope is a separate conjunct so it composes with (dim,key) rather than
	// replacing it: a user probing another user's rows via
	// ?dimension=user&key=<other> still gets user_id = <other> AND
	// user_id = <self> → empty. The dashboard's model/provider filters compose
	// the same way, so clicking a Success row narrows within the filter.
	scopeConds, scopeArgs := sc.conds()
	conds = append(conds, scopeConds...)
	args = append(args, scopeArgs...)
	// limit is applied in Go, after folding err strings into outcomes — a SQL
	// LIMIT here would truncate before the fold and lose groups that merge.

	// Group by (status, err) rather than status so distinct causes stay distinct;
	// the outcome is derived per group in Go, where the classifier already lives,
	// rather than being reimplemented as a SQL CASE that could drift from it.
	rows, err := s.db.Query(
		`SELECT status, err, COUNT(*) AS n
		   FROM calls
		  WHERE `+strings.Join(conds, " AND ")+`
		  GROUP BY status, err
		  ORDER BY n DESC, status ASC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: top error codes: %w", err)
	}
	defer rows.Close()

	// Several err strings collapse to one outcome — every distinct transport
	// failure text is one transport_error — so fold in Go, then take the top N.
	type codeKey struct {
		status  int
		outcome calls.Outcome
	}
	totals := make(map[codeKey]int)
	var order []codeKey
	for rows.Next() {
		var (
			status  int
			callErr string
			n       int
		)
		if err := rows.Scan(&status, &callErr, &n); err != nil {
			return nil, fmt.Errorf("store: scan top error codes: %w", err)
		}
		k := codeKey{status: status, outcome: calls.OutcomeOf(status, callErr)}
		if _, seen := totals[k]; !seen {
			order = append(order, k)
		}
		totals[k] += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: top error codes: %w", err)
	}

	sort.SliceStable(order, func(i, j int) bool {
		if totals[order[i]] != totals[order[j]] {
			return totals[order[i]] > totals[order[j]]
		}
		return order[i].status < order[j].status
	})
	if len(order) > limit {
		order = order[:limit]
	}
	out := make([]ErrorCodeRow, 0, len(order))
	for _, k := range order {
		out = append(out, ErrorCodeRow{Status: k.status, Outcome: string(k.outcome), Count: totals[k]})
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
		        `+sqlCountWhere(sqlHasVerdict)+`,
		        `+sqlCountWhere(sqlFailed)+`,
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(cache_creation_input_tokens), 0),
		        COALESCE(SUM(thinking_tokens), 0),
		        COALESCE(AVG(CASE WHEN `+sqlHasVerdict+` THEN latency_ms END), 0),
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
func (s *Store) TokensByModelSeries(sc Scope, dim BreakdownDimension, since, until time.Time, bucket time.Duration) ([]string, []TokensByModelBucket, error) {
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
	scopeSQL, scopeArgs := scopeClause(sc)
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
		  GROUP BY bucket_start, %s`, col, scopeSQL, col),
		append([]any{bucketMs, bucketMs, sinceMs, untilMs}, scopeArgs...)...,
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
func (s *Store) SuccessByModelSeries(sc Scope, dim BreakdownDimension, since, until time.Time, bucket time.Duration) ([]string, []SuccessByModelBucket, error) {
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
	scopeSQL, scopeArgs := scopeClause(sc)
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT (ts / ?) * ? AS bucket_start,
		        %s,
		        `+sqlCountWhere(sqlHasVerdict)+`,
		        `+sqlCountWhere(sqlFailed)+`
		   FROM calls
		  WHERE ts >= ? AND ts < ?%s
		  GROUP BY bucket_start, %s`, col, scopeSQL, col),
		append([]any{bucketMs, bucketMs, sinceMs, untilMs}, scopeArgs...)...,
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
func (s *Store) CacheByModelSeries(sc Scope, dim BreakdownDimension, since, until time.Time, bucket time.Duration) ([]string, []CacheByModelBucket, error) {
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
	scopeSQL, scopeArgs := scopeClause(sc)
	rows, err := s.db.Query(
		fmt.Sprintf(`SELECT (ts / ?) * ? AS bucket_start,
		        %s,
		        COALESCE(SUM(cache_read_input_tokens), 0),
		        COALESCE(SUM(input_tokens + cache_read_input_tokens + cache_creation_input_tokens), 0)
		   FROM calls
		  WHERE ts >= ? AND ts < ?%s
		  GROUP BY bucket_start, %s`, col, scopeSQL, col),
		append([]any{bucketMs, bucketMs, sinceMs, untilMs}, scopeArgs...)...,
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

// --- What counts as a failure -------------------------------------------------
//
// These are the SQL form of calls.OutcomeOf, named so the aggregates below
// cannot drift apart from each other or from the pill rendered beside them.
//
// Three rules, each of which the old `status = 0 OR status >= 400` broke:
//
//  1. A pending row carries no verdict. It used to count as a SUCCESS — -1 is
//     not >= 400 — so abandoned calls quietly raised the success rate. Now
//     excluded from numerator and denominator both: counting it either way
//     asserts an outcome nobody observed.
//  2. A client cancellation is not a failure. router.Classify already calls it
//     neutral and proxy.go says 499 exists to stay out of the error rate, yet
//     every ratio counted it. A user pressing Esc must not mark a provider down.
//  3. The status alone cannot say whose fault it was. songguo mints 402/403/404/
//     429/502 of its own, so a budget denial was indistinguishable from the
//     provider rejecting the request. sqlProviderFailed is the narrower question
//     and is what per-vendor stats must use.
const (
	// sqlHasVerdict: the call finished, so it can be counted at all.
	sqlHasVerdict = `status <> -1`

	// sqlNotCallerAbort: exclude calls the caller walked away from.
	sqlNotCallerAbort = `err <> 'client_gone'`

	// sqlFailed: the caller got no answer, and it was not their own doing.
	// Includes songguo's denials — from the caller's seat a refused request did
	// fail. `status = 0` matches legacy rows only; nothing writes it now.
	sqlFailed = sqlHasVerdict + ` AND ` + sqlNotCallerAbort +
		` AND (status = 0 OR status >= 400 OR err <> '')`

	// sqlProviderFailed: the PROVIDER failed — the only thing that may count
	// against a vendor. A forwarded error (err = '') is theirs, as is a transport
	// or stream failure while reaching them; everything else carrying a slug is
	// songguo's doing or the caller's and is excluded.
	sqlProviderFailed = sqlHasVerdict + ` AND ` + sqlNotCallerAbort +
		` AND ((err = '' AND (status = 0 OR status >= 400))` +
		` OR err LIKE 'transport_error:%' OR err LIKE 'stream_error:%')`
)

// sqlCountWhere renders a predicate as a summable 0/1 expression.
func sqlCountWhere(pred string) string {
	return `SUM(CASE WHEN ` + pred + ` THEN 1 ELSE 0 END)`
}

// qualify prefixes the column names in one of the predicates above with a table
// alias, for the joined queries that need `c.status` rather than `status`. It
// rewrites only the two identifiers the predicates use, so it cannot corrupt the
// string literals ('client_gone', the LIKE patterns) alongside them.
func qualify(pred, alias string) string {
	if alias == "" {
		return pred
	}
	r := strings.NewReplacer(
		"status", alias+".status",
		"err ", alias+".err ",
	)
	return r.Replace(pred)
}

// IsCallError reports whether a finalized call failed and it was not the
// caller's doing. Exported so the API's live session rollup agrees with the
// stored aggregates instead of keeping its own copy of the rule.
func IsCallError(status int, err string) bool { return isErrorStatus(status, err) }

// lastStatusFilter appends sqlHasVerdict to a query that may or may not already
// have a WHERE, so "the vendor's most recent status" means its most recent
// FINISHED call. Without it the Providers page renders the -1 sentinel whenever
// a vendor happens to have a request in flight.
func lastStatusFilter(clause string) string {
	if clause == "" {
		return " WHERE " + sqlHasVerdict
	}
	return " AND " + sqlHasVerdict
}

// isErrorStatus reports whether a finalized call failed and it was not the
// caller's doing — the Go twin of sqlFailed. It takes err as well as status
// because the status alone cannot tell a provider's 429 from songguo's.
func isErrorStatus(status int, err string) bool {
	switch calls.OutcomeOf(status, err) {
	case calls.OutcomeOK, calls.OutcomeClientGone,
		calls.OutcomeInFlight, calls.OutcomeAbandoned:
		return false
	}
	return true
}

// hasVerdict reports whether a call finished, so it belongs in a rate at all.
func hasVerdict(status int) bool { return status != calls.StatusPending }

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

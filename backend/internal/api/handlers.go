package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/sessiontitle"
	"github.com/songguo/songguo/internal/store"
)

// bearerToken extracts the key from an Authorization header value, accepting
// either "Bearer <key>" (case-insensitive scheme) or a raw "<key>".
func bearerToken(header string) string {
	h := strings.TrimSpace(header)
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return h
}

// --- query param helpers ---

// parseUnixTime parses a unix-seconds query value into a *time.Time. Missing or
// invalid values yield (nil, false).
func parseUnixTime(r *http.Request, key string) (*time.Time, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return nil, false
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, false
	}
	t := time.Unix(sec, 0).UTC()
	return &t, true
}

// parseIntDefault returns the int value of a query param, or def if missing or
// unparseable.
func parseIntDefault(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// callFilterFromQuery builds a store.CallFilter from the request query,
// applying the given default and cap to the limit.
func callFilterFromQuery(r *http.Request, defLimit, capLimit int) store.CallFilter {
	f := store.CallFilter{
		UserID:   r.URL.Query().Get("user_id"),
		Model:    r.URL.Query().Get("model"),
		Vendor:   r.URL.Query().Get("vendor"),
		FeedSort: r.URL.Query().Get("sort"),
	}
	if since, ok := parseUnixTime(r, "since"); ok {
		f.Since = since
	}
	if until, ok := parseUnixTime(r, "until"); ok {
		f.Until = until
	}
	if raw := r.URL.Query().Get("status"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			f.Status = &n
		}
	}
	limit := parseIntDefault(r, "limit", defLimit)
	if limit <= 0 {
		limit = defLimit
	}
	if limit > capLimit {
		limit = capLimit
	}
	f.Limit = limit
	offset := parseIntDefault(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	f.Offset = offset
	return f
}

// --- handlers ---

const (
	defaultCallsAPILimit = 50
	maxCallsAPILimit     = 500
	exportMaxRows        = 100000
)

// handleOverview computes the dashboard summary over a window (default last 30d).
func (a *api) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	view, err := a.overviewData(since, until)
	if err != nil {
		a.writeDataErr(w, "overview", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// overviewData computes the dashboard summary over [since, until]: total spend,
// spend by modality, request/error/latency stats, active vendors/users, daily
// burn and (when any budget is set) runway in days.
func (a *api) overviewData(since, until time.Time) (overviewView, error) {
	totalSpend, err := a.store.TotalSpend(&since, &until)
	if err != nil {
		return overviewView{}, err
	}
	byMod, err := a.store.SpendByModality(&since, &until)
	if err != nil {
		return overviewView{}, err
	}
	stats, err := a.store.OverviewStats(&since, &until)
	if err != nil {
		return overviewView{}, err
	}
	tokens, err := a.store.TokenTotals(&since, &until)
	if err != nil {
		return overviewView{}, err
	}
	activeCallers, err := a.store.DistinctUsers(&since, &until)
	if err != nil {
		return overviewView{}, err
	}

	errorRate := 0.0
	if stats.Requests > 0 {
		errorRate = float64(stats.Errors) / float64(stats.Requests)
	}

	// vendors_active = vendors in the current snapshot.
	vendorsActive := 0
	if snap := a.snap(); snap != nil {
		vendorsActive = len(snap.Vendors())
	}

	// users_active = non-revoked users; also compute runway from budgets.
	users, err := a.store.ListUsers()
	if err != nil {
		return overviewView{}, err
	}
	usersActive := 0
	var remainingBudget float64
	anyBudget := false
	for _, u := range users {
		if u.RevokedAt == nil {
			usersActive++
		}
		if u.Budget != nil {
			anyBudget = true
			spent, err := a.store.SpendByUser(u.ID, nil)
			if err != nil {
				return overviewView{}, err
			}
			rem := *u.Budget - spent
			if rem > 0 {
				remainingBudget += rem
			}
		}
	}

	// daily_burn = spend over the last 7 days / 7.
	now := a.now().UTC()
	weekAgo := now.AddDate(0, 0, -7)
	weekSpend, err := a.store.TotalSpend(&weekAgo, &now)
	if err != nil {
		return overviewView{}, err
	}
	dailyBurn := weekSpend / 7.0

	var runway *float64
	if anyBudget && dailyBurn > 0 {
		rd := remainingBudget / dailyBurn
		runway = &rd
	}

	if byMod == nil {
		byMod = map[string]float64{}
	}

	return overviewView{
		Range:           rangeView{Since: since.Unix(), Until: until.Unix()},
		TotalSpend:      totalSpend,
		SpendByModality: byMod,
		Tokens:          tokenView{Input: tokens.Input, Output: tokens.Output, Cached: tokens.Cached, CacheCreation: tokens.CacheCreation, Thinking: tokens.Thinking},
		Requests:        stats.Requests,
		Errors:          stats.Errors,
		ErrorRate:       errorRate,
		LatencyMS:       latencyView{P50: stats.P50, P95: stats.P95, P99: stats.P99},
		TTFTMS:          latencyView{P50: stats.TTFTP50, P95: stats.TTFTP95, P99: stats.TTFTP99},
		OutputTPS:       rateView{P50: stats.OutputTPSP50, P95: stats.OutputTPSP95, P99: stats.OutputTPSP99},
		VendorsActive:   vendorsActive,
		UsersActive:     usersActive,
		ActiveCallers:   activeCallers,
		DailyBurn:       dailyBurn,
		RunwayDays:      runway,
	}, nil
}

// handleSessionsOverview returns aggregate stats over coding-agent sessions in
// the window (default last 30d): count, inferred outcomes, subagent fan-out, and
// turns/tokens/duration/tool-call totals and percentiles. It powers the
// Behavioral section of the overview dashboard.
func (a *api) handleSessionsOverview(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	st, err := a.store.SessionStats(&since, &until)
	if err != nil {
		a.writeDataErr(w, "sessions overview", err)
		return
	}
	writeJSON(w, http.StatusOK, sessionStatsView{
		Range:         rangeView{Since: since.Unix(), Until: until.Unix()},
		Sessions:      st.Sessions,
		Completed:     st.Completed,
		Errored:       st.Errored,
		Interrupted:   st.Interrupted,
		WithSubagents:  st.WithSubagents,
		TotalTurns:     st.TotalTurns,
		TotalTokens:    st.TotalTokens,
		TotalToolCalls: st.TotalToolCalls,
		AvgTurns:       st.AvgTurns,
		AvgTokens:      st.AvgTokens,
		AvgDuration:    st.AvgDuration,
		AvgToolCalls:   st.AvgToolCalls,
		TurnsP50:       st.TurnsP50,
		TurnsP95:       st.TurnsP95,
		TokensP50:      st.TokensP50,
		TokensP95:      st.TokensP95,
		DurationP50:    st.DurationP50,
		DurationP95:    st.DurationP95,
		ToolCallsP50:   st.ToolCallsP50,
		ToolCallsP95:   st.ToolCallsP95,
	})
}

// handleUsageSeries returns cost/request/error totals bucketed over time for the
// spend-over-time chart. Window defaults to the last 7 days; bucket defaults
// to "day" when the range exceeds 2 days, else "hour". Only "hour"/"day" are
// accepted.
func (a *api) handleUsageSeries(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -7)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	view, err := a.usageSeriesData(since, until, r.URL.Query().Get("bucket"))
	if err != nil {
		a.writeDataErr(w, "usage series", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// usageSeriesData buckets cost/request/error totals over [since, until].
// bucketRaw is "", "hour" or "day"; "" auto-selects day for ranges over 2 days,
// else hour. An invalid bucket or a range too large for the bucket returns a
// *apiError (400).
func (a *api) usageSeriesData(since, until time.Time, bucketRaw string) (usageSeriesView, error) {
	bucket, label, err := resolveBucket(bucketRaw, since, until)
	if err != nil {
		return usageSeriesView{}, err
	}

	points, err := a.store.UsageSeries(since, until, bucket)
	if err != nil {
		if errors.Is(err, store.ErrTooManyBuckets) {
			return usageSeriesView{}, badRequestErr("requested range is too large for the chosen bucket")
		}
		return usageSeriesView{}, err
	}

	views := make([]seriesPoint, 0, len(points))
	for _, p := range points {
		views = append(views, seriesPoint{
			TS:                  p.Bucket.UTC().Format(time.RFC3339),
			Cost:                p.Cost,
			Requests:            p.Requests,
			Errors:              p.Errors,
			InputTokens:         p.InputTokens,
			OutputTokens:        p.OutputTokens,
			CachedTokens:        p.CachedTokens,
			CacheCreationTokens: p.CacheCreationTokens,
			ThinkingTokens:      p.ThinkingTokens,
			AvgLatencyMS:        p.AvgLatencyMS,
			AvgTTFTMS:           p.AvgTTFTMS,
			AvgOutputTPS:        p.AvgOutputTokensSec,
		})
	}

	return usageSeriesView{Bucket: label, Points: views}, nil
}

// resolveBucket maps the raw "bucket" query value to a duration and its label.
// "" auto-selects day for ranges over 2 days, else hour; "hour"/"day" are taken
// as-is; anything else is a *apiError (400).
func resolveBucket(bucketRaw string, since, until time.Time) (time.Duration, string, error) {
	switch bucketRaw {
	case "":
		if until.Sub(since) > 48*time.Hour {
			return 24 * time.Hour, "day", nil
		}
		return time.Hour, "hour", nil
	case "hour":
		return time.Hour, "hour", nil
	case "day":
		return 24 * time.Hour, "day", nil
	default:
		return 0, "", badRequestErr("bucket must be hour or day")
	}
}

// handleTokensByModel returns per-bucket token totals broken down by a dimension
// (model, vendor, or user; top N + "Other") alongside total cost, for the Usage
// stacked charts. Dimension defaults to model; window defaults to the last 7
// days; bucket auto-selects like handleUsageSeries.
func (a *api) handleTokensByModel(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -7)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	dim := store.BreakdownByModel
	if d := r.URL.Query().Get("dimension"); d != "" {
		dim = store.BreakdownDimension(d)
	}

	bucket, label, err := resolveBucket(r.URL.Query().Get("bucket"), since, until)
	if err != nil {
		a.writeDataErr(w, "tokens by model", err)
		return
	}
	models, buckets, err := a.store.TokensByModelSeries(dim, since, until, bucket)
	if err != nil {
		if errors.Is(err, store.ErrTooManyBuckets) {
			a.writeDataErr(w, "tokens by model", badRequestErr("requested range is too large for the chosen bucket"))
			return
		}
		if errors.Is(err, store.ErrBadDimension) {
			a.writeDataErr(w, "tokens by model", badRequestErr("dimension must be model, vendor, or user"))
			return
		}
		a.writeDataErr(w, "tokens by model", err)
		return
	}

	points := make([]tokensByModelPoint, 0, len(buckets))
	for _, b := range buckets {
		points = append(points, tokensByModelPoint{
			TS:     b.Bucket.UTC().Format(time.RFC3339),
			Cost:   b.Cost,
			Tokens: b.Tokens,
			Costs:  b.CostByModel,
			TTFT:   b.TTFTByModel,
			TPS:    b.TPSByModel,
		})
	}
	writeJSON(w, http.StatusOK, tokensByModelView{Bucket: label, Models: models, Points: points})
}

// handleSuccessByModel returns per-bucket request/error counts broken down by a
// dimension (model, vendor, or user; top N by requests + "Other"), for the
// Success % over-time chart. Dimension defaults to model; window defaults to the
// last 7 days; bucket auto-selects like handleUsageSeries.
func (a *api) handleSuccessByModel(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -7)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	dim := store.BreakdownByModel
	if d := r.URL.Query().Get("dimension"); d != "" {
		dim = store.BreakdownDimension(d)
	}

	bucket, label, err := resolveBucket(r.URL.Query().Get("bucket"), since, until)
	if err != nil {
		a.writeDataErr(w, "success by model", err)
		return
	}
	models, buckets, err := a.store.SuccessByModelSeries(dim, since, until, bucket)
	if err != nil {
		if errors.Is(err, store.ErrTooManyBuckets) {
			a.writeDataErr(w, "success by model", badRequestErr("requested range is too large for the chosen bucket"))
			return
		}
		if errors.Is(err, store.ErrBadDimension) {
			a.writeDataErr(w, "success by model", badRequestErr("dimension must be model, vendor, or user"))
			return
		}
		a.writeDataErr(w, "success by model", err)
		return
	}

	points := make([]successByModelPoint, 0, len(buckets))
	for _, b := range buckets {
		points = append(points, successByModelPoint{
			TS:       b.Bucket.UTC().Format(time.RFC3339),
			Requests: b.Requests,
			Errors:   b.Errors,
		})
	}
	writeJSON(w, http.StatusOK, successByModelView{Bucket: label, Models: models, Points: points})
}

// handleCacheByModel returns per-bucket cache-read and total-input token sums broken
// down by a dimension (model, vendor, or user; top N by total input + "Other"), for
// the cache-hit % over-time chart. Dimension defaults to model; window defaults to
// the last 7 days; bucket auto-selects like handleUsageSeries.
func (a *api) handleCacheByModel(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -7)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	dim := store.BreakdownByModel
	if d := r.URL.Query().Get("dimension"); d != "" {
		dim = store.BreakdownDimension(d)
	}

	bucket, label, err := resolveBucket(r.URL.Query().Get("bucket"), since, until)
	if err != nil {
		a.writeDataErr(w, "cache by model", err)
		return
	}
	models, buckets, err := a.store.CacheByModelSeries(dim, since, until, bucket)
	if err != nil {
		if errors.Is(err, store.ErrTooManyBuckets) {
			a.writeDataErr(w, "cache by model", badRequestErr("requested range is too large for the chosen bucket"))
			return
		}
		if errors.Is(err, store.ErrBadDimension) {
			a.writeDataErr(w, "cache by model", badRequestErr("dimension must be model, vendor, or user"))
			return
		}
		a.writeDataErr(w, "cache by model", err)
		return
	}

	points := make([]cacheByModelPoint, 0, len(buckets))
	for _, b := range buckets {
		points = append(points, cacheByModelPoint{
			TS:        b.Bucket.UTC().Format(time.RFC3339),
			CacheRead: b.CacheRead,
			Input:     b.Input,
		})
	}
	writeJSON(w, http.StatusOK, cacheByModelView{Bucket: label, Models: models, Points: points})
}

// handleBreakdown groups the call log by a dimension (model, vendor, user, or
// modality) over a window (default last 30d) for the breakdown table and the
// category bar charts.
func (a *api) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	dim := store.BreakdownDimension(r.URL.Query().Get("dimension"))
	rows, err := a.store.Breakdown(dim, &since, &until)
	if err != nil {
		if errors.Is(err, store.ErrBadDimension) {
			a.writeDataErr(w, "usage breakdown", badRequestErr("dimension must be model, vendor, user, or modality"))
			return
		}
		a.writeDataErr(w, "usage breakdown", err)
		return
	}

	views := make([]breakdownRow, 0, len(rows))
	for _, b := range rows {
		views = append(views, breakdownRow{
			Key:                 b.Key,
			Requests:            b.Requests,
			Errors:              b.Errors,
			InputTokens:         b.InputTokens,
			OutputTokens:        b.OutputTokens,
			CachedTokens:        b.CachedTokens,
			CacheCreationTokens: b.CacheCreationTokens,
			ThinkingTokens:      b.ThinkingTokens,
			Cost:                b.Cost,
			AvgLatencyMS:        b.AvgLatencyMS,
		})
	}
	writeJSON(w, http.StatusOK, breakdownView{
		Range:     rangeView{Since: since.Unix(), Until: until.Unix()},
		Dimension: string(dim),
		Rows:      views,
	})
}

// handleErrors returns error-row counts grouped by class (rate-limited, client,
// server, transport) over a window (default last 30d) for the reliability section.
func (a *api) handleErrors(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	c, err := a.store.ErrorClassCounts(&since, &until)
	if err != nil {
		a.writeDataErr(w, "usage errors", err)
		return
	}
	writeJSON(w, http.StatusOK, errorsView{
		Range:       rangeView{Since: since.Unix(), Until: until.Unix()},
		RateLimited: c.RateLimited,
		ClientError: c.ClientError,
		ServerError: c.ServerError,
		Transport:   c.Transport,
	})
}

// handleTopErrorCodes returns error-row counts grouped by upstream status code,
// ranked by count (top 8), over a window (default last 30d). An optional
// dimension+key pair scopes the result to one series (e.g. dimension=model &
// key=<model> for a single service), so the Overview error-codes panel can filter
// to the row the user clicked.
func (a *api) handleTopErrorCodes(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	dim := store.BreakdownByModel
	if d := r.URL.Query().Get("dimension"); d != "" {
		dim = store.BreakdownDimension(d)
	}
	key := r.URL.Query().Get("key")

	rows, err := a.store.TopErrorCodes(dim, key, &since, &until, 8)
	if err != nil {
		if errors.Is(err, store.ErrBadDimension) {
			a.writeDataErr(w, "usage error codes", badRequestErr("dimension must be model, vendor, or user"))
			return
		}
		a.writeDataErr(w, "usage error codes", err)
		return
	}

	views := make([]errorCodeRow, 0, len(rows))
	for _, row := range rows {
		views = append(views, errorCodeRow{Status: row.Status, Count: row.Count})
	}
	writeJSON(w, http.StatusOK, errorCodesView{
		Range: rangeView{Since: since.Unix(), Until: until.Unix()},
		Rows:  views,
	})
}

// handleCalls returns a filtered, paginated page of call entries plus the
// total count for the same filter.
func (a *api) handleCalls(w http.ResponseWriter, r *http.Request) {
	view, err := a.callsData(callFilterFromQuery(r, defaultCallsAPILimit, maxCallsAPILimit))
	if err != nil {
		a.writeDataErr(w, "query calls", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// callsData returns a page of calls plus the total count for the same filter.
// Limit/offset are clamped defensively (default 50, cap 500, offset >= 0) so the
// method is safe to call with a raw, un-parsed filter (e.g. from an MCP tool).
func (a *api) callsData(f store.CallFilter) (callsView, error) {
	if f.Limit <= 0 {
		f.Limit = defaultCallsAPILimit
	}
	if f.Limit > maxCallsAPILimit {
		f.Limit = maxCallsAPILimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	entries, err := a.store.QueryCalls(f)
	if err != nil {
		return callsView{}, err
	}
	total, err := a.store.CountCalls(f)
	if err != nil {
		return callsView{}, err
	}

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	hasTrace, err := a.store.HasPayloads(ids)
	if err != nil {
		return callsView{}, err
	}
	views := make([]entryView, 0, len(entries))
	for _, e := range entries {
		v := newEntryView(e)
		v.HasTrace = hasTrace[e.ID]
		views = append(views, v)
	}

	return callsView{
		Entries: views,
		Total:   total,
		Limit:   f.Limit,
		Offset:  f.Offset,
	}, nil
}

// handleCallTrace returns the captured request/response payload for a call, or
// 404 when no payload was stored for it. Bodies are returned as UTF-8 text when
// valid, else base64 (flagged per side).
func (a *api) handleCallTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "trace not found")
		return
	}
	view, err := a.callTraceData(id)
	if err != nil {
		a.writeDataErr(w, "get payload", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// callTraceData returns the captured request/response payload for a call, or a
// *apiError (404) when no payload was stored for it.
func (a *api) callTraceData(id string) (traceView, error) {
	p, err := a.store.GetPayload(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return traceView{}, notFoundErr("trace not found")
		}
		return traceView{}, err
	}
	return newTraceView(p), nil
}

// handleFeed returns the activity feed: one row per coding-agent session
// (aggregated) or per standalone request, newest activity first.
func (a *api) handleFeed(w http.ResponseWriter, r *http.Request) {
	view, err := a.feedData(callFilterFromQuery(r, defaultCallsAPILimit, maxCallsAPILimit))
	if err != nil {
		a.writeDataErr(w, "query feed", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// feedData returns a page of feed rows plus the total group count. Limit/offset
// are clamped defensively so the method is safe with a raw filter.
func (a *api) feedData(f store.CallFilter) (feedView, error) {
	if f.Limit <= 0 {
		f.Limit = defaultCallsAPILimit
	}
	if f.Limit > maxCallsAPILimit {
		f.Limit = maxCallsAPILimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	rows, total, err := a.store.Feed(f)
	if err != nil {
		return feedView{}, err
	}
	views := make([]feedRowView, 0, len(rows))
	for _, row := range rows {
		views = append(views, newFeedRowView(row))
	}
	return feedView{Rows: views, Total: total, Limit: f.Limit, Offset: f.Offset}, nil
}

// handleCall returns a single call entry by id (404 when absent). The request
// detail page pairs this with GET /api/calls/{id}/trace.
func (a *api) handleCall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusNotFound, "not_found", "call not found")
		return
	}
	view, err := a.callData(id)
	if err != nil {
		a.writeDataErr(w, "get call", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// callData returns a single call entry, or a *apiError (404) when absent.
func (a *api) callData(id string) (entryView, error) {
	e, err := a.store.GetCall(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return entryView{}, notFoundErr("call not found")
		}
		return entryView{}, err
	}
	v := newEntryView(e)
	hasTrace, err := a.store.HasPayloads([]string{id})
	if err != nil {
		return entryView{}, err
	}
	v.HasTrace = hasTrace[id]
	if comp, err := a.store.GetComposition(id); err == nil {
		v.Composition = &comp
	} else if !errors.Is(err, store.ErrNotFound) {
		return entryView{}, err
	}
	a.enrichClientFromTrace(&v)
	return v, nil
}

// handleSession returns one session's rollups, agent tree, and calls (404 when
// no call carries the session id).
func (a *api) handleSession(w http.ResponseWriter, r *http.Request) {
	view, err := a.sessionData(r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "get session", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// sessionData aggregates a session's calls into rollups + the main-loop→subagent
// tree, and returns the calls oldest-first. A *apiError (404) is returned when
// the session has no calls. Bounded to the store's 1000-call page.
func (a *api) sessionData(id string) (sessionView, error) {
	if id == "" {
		return sessionView{}, notFoundErr("session not found")
	}
	entries, err := a.store.QueryCalls(store.CallFilter{SessionID: id, Limit: 1000})
	if err != nil {
		return sessionView{}, err
	}
	if len(entries) == 0 {
		return sessionView{}, notFoundErr("session not found")
	}
	// QueryCalls returns newest-first; present the session oldest-first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}

	var (
		cost, in, out                float64
		cacheRead, cacheCreate, think float64
		errCount                     int
		modelsSet                    = map[string]struct{}{}
		vendorsSet                   = map[string]struct{}{}
		first                        = entries[0].TS
		last                         = entries[0].TS
		ids                          = make([]string, 0, len(entries))
	)
	for _, e := range entries {
		cost += e.Cost
		in += e.InputTokens
		out += e.OutputTokens
		cacheRead += e.CachedTokens
		cacheCreate += e.CacheCreationTokens
		think += e.ThinkingTokens
		if e.Status == 0 || e.Status >= 400 {
			errCount++
		}
		if e.Model != "" {
			modelsSet[e.Model] = struct{}{}
		}
		if e.Vendor != "" {
			vendorsSet[e.Vendor] = struct{}{}
		}
		if e.TS.Before(first) {
			first = e.TS
		}
		if e.TS.After(last) {
			last = e.TS
		}
		ids = append(ids, e.ID)
	}
	hasTrace, err := a.store.HasPayloads(ids)
	if err != nil {
		return sessionView{}, err
	}
	entViews := make([]entryView, 0, len(entries))
	for _, e := range entries {
		v := newEntryView(e)
		v.HasTrace = hasTrace[e.ID]
		a.enrichClientFromTrace(&v)
		entViews = append(entViews, v)
	}

	return sessionView{
		SessionID:           id,
		Title:               a.sessionTitleFromEntries(id, entries),
		Calls:               len(entries),
		Cost:                cost,
		InputTokens:         in,
		OutputTokens:        out,
		CachedTokens:        cacheRead,
		CacheCreationTokens: cacheCreate,
		ThinkingTokens:      think,
		ErrorCount:          errCount,
		FirstTS:             first.UTC().Format(time.RFC3339),
		LastTS:              last.UTC().Format(time.RFC3339),
		Models:              sortedStringSet(modelsSet),
		Vendors:             sortedStringSet(vendorsSet),
		Agents:              buildAgentTree(entries),
		Entries:      entViews,
	}, nil
}

func (a *api) enrichClientFromTrace(v *entryView) {
	if v.ClientName != "" || !v.HasTrace {
		return
	}
	p, err := a.store.GetPayload(v.ID)
	if err != nil {
		return
	}
	ci := calls.ParseClientInfo(headerValue(p.ReqHeaders, "User-Agent"), headerValue(p.ReqHeaders, "X-Stainless-Os"))
	v.ClientName = ci.Name
	v.ClientVersion = ci.Version
	v.ClientOS = ci.OS
	v.ClientOSVersion = ci.OSVersion
}

func (a *api) sessionTitle(id string) string {
	entries, err := a.store.QueryCalls(store.CallFilter{SessionID: id, Limit: 1000})
	if err != nil || len(entries) == 0 {
		return ""
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return a.sessionTitleFromEntries(id, entries)
}

func (a *api) sessionTitleFromEntries(id string, entries []calls.Entry) string {
	if title, err := a.store.SessionTitle(id); err == nil && title != "" {
		return title
	}
	for _, e := range entries {
		if e.Wire != "anthropic/messages" {
			continue
		}
		p, err := a.store.GetPayload(e.ID)
		if err != nil {
			continue
		}
		if title := titleFromPayload(p); title != "" {
			return title
		}
	}
	return ""
}

func titleFromPayload(p store.Payload) string {
	reqBody := p.ReqBody
	if decoded, ok := decodeTraceBody(reqBody, headerValue(p.ReqHeaders, "Content-Encoding")); ok {
		reqBody = decoded
	}
	body := p.RespBody
	if decoded, ok := decodeTraceBody(body, headerValue(p.RespHeaders, "Content-Encoding")); ok {
		body = decoded
	}
	return sessiontitle.FromClaude(reqBody, body)
}

func headerValue(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// handleContextComposition returns the aggregated context-window decomposition
// over a window (defaults to the last 30 days, like the overview).
func (a *api) handleContextComposition(w http.ResponseWriter, r *http.Request) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if v, ok := parseUnixTime(r, "since"); ok {
		since = *v
	}
	if v, ok := parseUnixTime(r, "until"); ok {
		until = *v
	}

	agg, err := a.store.AggregateComposition(&since, &until)
	if err != nil {
		a.writeDataErr(w, "context composition", err)
		return
	}
	sources := agg.Sources
	if sources == nil {
		sources = []compose.Source{}
	}
	writeJSON(w, http.StatusOK, contextCompositionView{
		Range:    rangeView{Since: since.Unix(), Until: until.Unix()},
		Requests: agg.Requests,
		AvgTotal: agg.AvgTotal,
		Sources:  sources,
	})
}

// handleSessionContext returns one session's per-turn context composition, its
// request-weighted distribution, the latest turn's full snapshot, and a
// (currently empty) dwell list.
func (a *api) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rows, err := a.store.SessionComposition(id)
	if err != nil {
		a.writeDataErr(w, "session context", err)
		return
	}
	agg, err := a.store.AggregateSessionComposition(id)
	if err != nil {
		a.writeDataErr(w, "session context", err)
		return
	}
	if agg.Sources == nil {
		agg.Sources = []compose.Source{}
	}

	turns := make([]contextTurnView, 0, len(rows))
	snapshot := []compose.Source{}
	for i, row := range rows {
		srcMap := make(map[string]int64, len(row.C.Sources))
		for _, s := range row.C.Sources {
			srcMap[s.Key] = s.Tokens
		}
		turns = append(turns, contextTurnView{
			CallID:  row.CallID,
			Seq:     i,
			TS:      row.TS.UTC().Format(time.RFC3339),
			AgentID: row.AgentID,
			Total:   row.C.Total,
			Cached:  row.C.Cached,
			Sources: srcMap,
		})
		if i == len(rows)-1 {
			snapshot = row.C.Sources
			if snapshot == nil {
				snapshot = []compose.Source{}
			}
		}
	}

	writeJSON(w, http.StatusOK, sessionContextView{
		SessionID: id,
		Title:     a.sessionTitle(id),
		Turns:     turns,
		Distribution: contextDistributionView{
			Requests: agg.Requests,
			AvgTotal: agg.AvgTotal,
			Sources:  agg.Sources,
			Blocks:   a.aggregateSessionBlocks(rows),
		},
		Snapshot: snapshot,
		Dwell:    []dwellBlockView{},
	})
}

func (a *api) aggregateSessionBlocks(rows []store.SessionCompositionRow) []contextBlockView {
	type acc struct {
		source      string
		producer    string
		typ         string
		hash        string
		total       int64
		cached      int64
		occurrences int
	}
	byKey := map[string]*acc{}
	var order []string

	for _, row := range rows {
		blocks := row.C.Blocks
		if len(blocks) == 0 {
			blocks = sourceFallbackBlocks(row.C.Sources)
		} else {
			blocks = reconcileBlocksToSources(blocks, row.C.Sources)
		}
		if len(blocks) == 0 {
			continue
		}

		seen := map[string]compose.Block{}
		var seenOrder []string
		for _, block := range blocks {
			if block.Tokens <= 0 || block.Source == "" {
				continue
			}
			key := contextBlockKey(block)
			existing, ok := seen[key]
			if !ok {
				seen[key] = block
				seenOrder = append(seenOrder, key)
				continue
			}
			existing.Tokens += block.Tokens
			existing.Cached += block.Cached
			seen[key] = existing
		}

		for _, key := range seenOrder {
			block := seen[key]
			a := byKey[key]
			if a == nil {
				a = &acc{source: block.Source, producer: block.Producer, typ: block.Type, hash: block.Hash}
				byKey[key] = a
				order = append(order, key)
			}
			a.total += block.Tokens
			a.cached += block.Cached
			a.occurrences++
		}
	}

	out := make([]contextBlockView, 0, len(order))
	for _, key := range order {
		a := byKey[key]
		tokens := int64(0)
		if a.occurrences > 0 {
			tokens = a.total / int64(a.occurrences)
		}
		out = append(out, contextBlockView{
			Source:      a.source,
			Producer:    a.producer,
			Type:        a.typ,
			Hash:        a.hash,
			Tokens:      tokens,
			Cached:      a.cached,
			Occurrences: a.occurrences,
			Total:       a.total,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Producer != out[j].Producer {
			return out[i].Producer < out[j].Producer
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Hash < out[j].Hash
	})
	return out
}

func sourceFallbackBlocks(sources []compose.Source) []compose.Block {
	var out []compose.Block
	seq := 0
	for _, src := range sources {
		if src.Tokens <= 0 || src.Key == "" {
			continue
		}
		var childTotal int64
		for _, child := range src.Children {
			if child.Tokens <= 0 || child.Key == "" {
				continue
			}
			childTotal += child.Tokens
			out = append(out, compose.Block{
				Seq:      seq,
				Source:   src.Key,
				Producer: child.Key,
				Type:     fallbackBlockType(src.Key, child.Key),
				Hash:     fallbackBlockHash(src.Key, child.Key),
				Tokens:   child.Tokens,
			})
			seq++
		}
		if remainder := src.Tokens - childTotal; remainder > 0 {
			out = append(out, compose.Block{
				Seq:    seq,
				Source: src.Key,
				Type:   fallbackBlockType(src.Key, ""),
				Hash:   fallbackBlockHash(src.Key, ""),
				Tokens: remainder,
				Cached: src.Cached,
			})
			seq++
		}
	}
	return out
}

func reconcileBlocksToSources(blocks []compose.Block, sources []compose.Source) []compose.Block {
	type target struct {
		source   string
		producer string
		tokens   int64
		cached   int64
	}
	var targets []target
	targetByKey := map[string]target{}
	for _, src := range sources {
		var childTotal int64
		for _, child := range src.Children {
			childTotal += child.Tokens
			t := target{source: src.Key, producer: child.Key, tokens: child.Tokens}
			targets = append(targets, t)
			targetByKey[groupKey(src.Key, child.Key)] = t
		}
		remainderTarget := src.Tokens - childTotal
		if remainderTarget < 0 {
			remainderTarget = 0
		}
		if len(src.Children) == 0 {
			remainderTarget = src.Tokens
		}
		t := target{source: src.Key, tokens: remainderTarget, cached: src.Cached}
		targets = append(targets, t)
		targetByKey[groupKey(src.Key, "")] = t
	}

	grouped := map[string][]compose.Block{}
	for _, block := range blocks {
		if block.Tokens <= 0 || block.Source == "" {
			continue
		}
		key := groupKey(block.Source, block.Producer)
		if _, ok := targetByKey[key]; !ok {
			key = groupKey(block.Source, "")
		}
		grouped[key] = append(grouped[key], block)
	}

	var out []compose.Block
	nextSeq := 0
	for _, target := range targets {
		if target.tokens <= 0 {
			continue
		}
		key := groupKey(target.source, target.producer)
		scaled, missing := scaleBlocksToTarget(grouped[key], target.tokens)
		for _, block := range scaled {
			block.Seq = nextSeq
			out = append(out, block)
			nextSeq++
		}
		if missing > 0 {
			out = append(out, compose.Block{
				Seq:      nextSeq,
				Source:   target.source,
				Producer: target.producer,
				Type:     fallbackBlockType(target.source, target.producer),
				Hash:     fallbackBlockHash(target.source, target.producer),
				Tokens:   missing,
				Cached:   target.cached,
			})
			nextSeq++
		}
	}
	return out
}

func groupKey(source, producer string) string {
	return source + "\x00" + producer
}

func scaleBlocksToTarget(blocks []compose.Block, target int64) ([]compose.Block, int64) {
	var total int64
	for _, block := range blocks {
		total += block.Tokens
	}
	if total <= 0 {
		return nil, target
	}
	if total <= target {
		return append([]compose.Block(nil), blocks...), target - total
	}

	out := make([]compose.Block, 0, len(blocks))
	var assigned int64
	type rem struct {
		i int
		r int64
	}
	var remainders []rem
	for _, block := range blocks {
		numer := block.Tokens * target
		scaled := numer / total
		if scaled <= 0 {
			continue
		}
		block.Tokens = scaled
		if block.Cached > block.Tokens {
			block.Cached = block.Tokens
		}
		out = append(out, block)
		assigned += scaled
		remainders = append(remainders, rem{i: len(out) - 1, r: numer % total})
	}
	sort.SliceStable(remainders, func(i, j int) bool {
		return remainders[i].r > remainders[j].r
	})
	for i := int64(0); i < target-assigned && int(i) < len(remainders); i++ {
		out[remainders[i].i].Tokens++
	}
	return out, 0
}

func fallbackBlockType(source, producer string) string {
	base := sourceLabel(source)
	if producer == "" {
		return base
	}
	return base + " / " + producerLabel(producer)
}

func fallbackBlockHash(source, producer string) string {
	return "source-total:" + source + ":" + producer
}

func sourceLabel(source string) string {
	switch source {
	case "tool_results":
		return "Tool results"
	case "tool_calls":
		return "Tool calls"
	case "tool_schemas":
		return "Tool schemas"
	case "system":
		return "System & instructions"
	case "assistant":
		return "Assistant turns"
	case "user":
		return "User turns"
	case "other":
		return "Other"
	// Legacy source keys, kept so historical rows still label.
	case "reasoning":
		return "Assistant reasoning"
	case "actions":
		return "Assistant actions"
	case "attachments":
		return "Attachments"
	default:
		return source
	}
}

func producerLabel(producer string) string {
	switch producer {
	// Synthetic producer keys within the user/assistant buckets.
	case "text":
		return "Text"
	case "reasoning":
		return "Reasoning"
	case "attachments":
		return "Attachments"
	case "unknown":
		return "unknown"
	// Legacy normalized keys, kept so historical rows still label; new rows
	// carry the request's verbatim tool name, which falls through unchanged.
	case "read":
		return "Read"
	case "bash":
		return "Bash"
	case "grep":
		return "Grep"
	case "glob":
		return "Glob"
	case "task":
		return "Task"
	case "web":
		return "Web"
	case "builtin":
		return "built-in"
	default:
		return strings.TrimPrefix(producer, "mcp:")
	}
}

func contextBlockKey(block compose.Block) string {
	return strings.Join([]string{block.Source, block.Producer, block.Type, block.Hash}, "\x00")
}

// buildAgentTree folds a session's calls by agent id into a forest, nesting each
// agent under its parent when parent-agent attribution is available. A root is
// an agent whose parent is empty or absent from the session. Every node's
// rollups cover its whole subtree (itself plus descendants). Agents appear in
// first-seen order.
func buildAgentTree(entries []calls.Entry) []agentNodeView {
	type agg struct {
		calls                         int
		cost, in, out                 float64
		cacheRead, cacheCreate, think float64
		parent                        string
	}
	aggs := map[string]*agg{}
	order := []string{}
	for _, e := range entries {
		a, ok := aggs[e.AgentID]
		if !ok {
			a = &agg{parent: e.ParentAgentID}
			aggs[e.AgentID] = a
			order = append(order, e.AgentID)
		}
		a.calls++
		a.cost += e.Cost
		a.in += e.InputTokens
		a.out += e.OutputTokens
		a.cacheRead += e.CachedTokens
		a.cacheCreate += e.CacheCreationTokens
		a.think += e.ThinkingTokens
	}

	children := map[string][]string{}
	var roots []string
	for _, id := range order {
		parent := aggs[id].parent
		if _, present := aggs[parent]; parent == "" || !present {
			roots = append(roots, id)
		} else {
			children[parent] = append(children[parent], id)
		}
	}

	visited := map[string]bool{}
	var build func(id string) agentNodeView
	build = func(id string) agentNodeView {
		visited[id] = true
		a := aggs[id]
		node := agentNodeView{
			AgentID:             id,
			Calls:               a.calls,
			Cost:                a.cost,
			InputTokens:         a.in,
			OutputTokens:        a.out,
			CachedTokens:        a.cacheRead,
			CacheCreationTokens: a.cacheCreate,
			ThinkingTokens:      a.think,
			Children:            []agentNodeView{},
		}
		for _, c := range children[id] {
			if visited[c] {
				continue // guard against a malformed parent cycle
			}
			ch := build(c)
			node.Calls += ch.Calls
			node.Cost += ch.Cost
			node.InputTokens += ch.InputTokens
			node.OutputTokens += ch.OutputTokens
			node.CachedTokens += ch.CachedTokens
			node.CacheCreationTokens += ch.CacheCreationTokens
			node.ThinkingTokens += ch.ThinkingTokens
			node.Children = append(node.Children, ch)
		}
		return node
	}

	out := make([]agentNodeView, 0, len(roots))
	for _, id := range roots {
		out = append(out, build(id))
	}
	return out
}

// sortedStringSet returns a set's keys in ascending order.
func sortedStringSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// handleListUsers returns all users with computed lifetime spend.
func (a *api) handleListUsers(w http.ResponseWriter, r *http.Request) {
	views, err := a.usersData()
	if err != nil {
		a.writeDataErr(w, "list users", err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// usersData returns all users with computed lifetime spend (keys never exposed).
func (a *api) usersData() ([]userView, error) {
	users, err := a.store.ListUsers()
	if err != nil {
		return nil, err
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		spent, err := a.store.SpendByUser(u.ID, nil)
		if err != nil {
			return nil, err
		}
		v := newUserView(u, spent)
		lastSeen, err := a.store.LastSeenByUser(u.ID)
		if err != nil {
			return nil, err
		}
		if lastSeen != nil {
			s := lastSeen.UTC().Format(time.RFC3339)
			v.LastSeen = &s
		}
		views = append(views, v)
	}
	return views, nil
}

// handleGetUser returns one user with computed lifetime spend. The plaintext key
// is never exposed on a read (only creation returns it).
func (a *api) handleGetUser(w http.ResponseWriter, r *http.Request) {
	u, err := a.store.GetUser(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		a.writeDataErr(w, "get user", err)
		return
	}
	spent, err := a.store.SpendByUser(u.ID, nil)
	if err != nil {
		a.writeDataErr(w, "get user spend", err)
		return
	}
	v := newUserView(u, spent)
	v.Key = "" // never expose the plaintext key on read
	lastSeen, err := a.store.LastSeenByUser(u.ID)
	if err != nil {
		a.writeDataErr(w, "get user last seen", err)
		return
	}
	if lastSeen != nil {
		s := lastSeen.UTC().Format(time.RFC3339)
		v.LastSeen = &s
	}
	writeJSON(w, http.StatusOK, v)
}

// createUserReq is the POST /api/users body.
type createUserReq struct {
	Name    string    `json:"name"`
	Budget  *float64  `json:"budget,omitempty"`
	Scope   *[]string `json:"scope,omitempty"`
	RPM     *int      `json:"rpm,omitempty"`
	Capture *bool     `json:"capture,omitempty"`
	// Idempotent: when true, if a non-revoked user with this name already exists,
	// return it (with its plaintext key) and update its mutable fields to the requested
	// value, instead of creating a duplicate. Lets a caller that lost its local
	// record — or a second instance sharing this gateway — re-adopt the existing
	// token rather than mint another with the same name.
	Idempotent bool `json:"idempotent,omitempty"`
}

// handleCreateUser creates a user and returns it once with the plaintext key.
func (a *api) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	v, err := a.createUserData(req)
	if err != nil {
		a.writeDataErr(w, "create user", err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// createUserData creates a user, returning the view with the plaintext key set
// (the only time it is ever exposed). A missing name is a *apiError (400). When
// req.Idempotent is set and a non-revoked user with the same name already exists,
// it adopts that user (returning its key) and updates its budget, instead of
// minting a duplicate.
func (a *api) createUserData(req createUserReq) (userView, error) {
	if req.Name == "" {
		return userView{}, badRequestErr("name is required")
	}
	if req.Idempotent {
		existing, err := a.store.GetUserByName(req.Name)
		if err == nil {
			return a.adoptExistingUser(existing, req)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return userView{}, err
		}
		// Not found → fall through and create it.
	}
	nu := store.NewUser{Name: req.Name, Budget: req.Budget}
	if req.Scope != nil {
		nu.Scope = *req.Scope
	}
	if req.RPM != nil {
		nu.RPM = *req.RPM
	}
	if req.Capture != nil {
		nu.Capture = *req.Capture
	}

	usr, plaintext, err := a.store.CreateUser(nu)
	if err != nil {
		return userView{}, err
	}
	v := newUserView(usr, 0)
	v.Key = plaintext
	return v, nil
}

// adoptExistingUser returns an existing user for an idempotent create — its view
// carries the plaintext key (from KeyFull) so the caller can reuse the token —
// after updating the requested mutable fields if they changed. The token id/key
// are untouched (no rotation).
func (a *api) adoptExistingUser(u store.User, req createUserReq) (userView, error) {
	upd := store.UserUpdate{}
	if req.Budget != nil && !sameFloatPtr(u.Budget, req.Budget) {
		upd.Budget = req.Budget
	}
	if req.Scope != nil {
		upd.Scope = req.Scope
	}
	if req.RPM != nil && u.RPM != *req.RPM {
		upd.RPM = req.RPM
	}
	if req.Capture != nil && u.Capture != *req.Capture {
		upd.Capture = req.Capture
	}
	if upd.Budget != nil || upd.Scope != nil || upd.RPM != nil || upd.Capture != nil {
		updated, err := a.store.UpdateUser(u.ID, upd)
		if err != nil {
			return userView{}, err
		}
		u = updated
	}
	spent, err := a.store.SpendByUser(u.ID, nil)
	if err != nil {
		return userView{}, err
	}
	return newUserView(u, spent), nil // newUserView carries KeyFull as Key
}

// sameFloatPtr reports whether two optional floats are equal (both nil, or both
// set to the same value).
func sameFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// patchUserReq distinguishes "omitted" from "present" for each field via
// pointers. Note: a null budget cannot be applied because the store's
// UserUpdate uses a *float64 set-or-unchanged; PATCH cannot reset budget to
// null (documented limitation for v1).
type patchUserReq struct {
	Name    *string   `json:"name,omitempty"`
	Budget  *float64  `json:"budget,omitempty"`
	Scope   *[]string `json:"scope,omitempty"`
	RPM     *int      `json:"rpm,omitempty"`
	Capture *bool     `json:"capture,omitempty"`
}

// handlePatchUser applies a subset of fields to a user.
func (a *api) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	var req patchUserReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	view, err := a.updateUserData(r.PathValue("id"), req)
	if err != nil {
		a.writeDataErr(w, "update user", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// updateUserData applies a subset of fields to a user and returns the updated
// view with computed spend. An unknown id is a *apiError (404).
func (a *api) updateUserData(id string, req patchUserReq) (userView, error) {
	upd := store.UserUpdate{
		Name:    req.Name,
		Budget:  req.Budget,
		Scope:   req.Scope,
		RPM:     req.RPM,
		Capture: req.Capture,
	}
	usr, err := a.store.UpdateUser(id, upd)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return userView{}, notFoundErr("user not found")
		}
		return userView{}, err
	}
	spent, err := a.store.SpendByUser(usr.ID, nil)
	if err != nil {
		return userView{}, err
	}
	return newUserView(usr, spent), nil
}

// handleRevokeUser revokes a user and returns its updated view.
func (a *api) handleRevokeUser(w http.ResponseWriter, r *http.Request) {
	view, err := a.revokeUserData(r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "revoke user", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// revokeUserData revokes a user and returns its updated view. An unknown id is a
// *apiError (404).
func (a *api) revokeUserData(id string) (userView, error) {
	if err := a.store.RevokeUser(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return userView{}, notFoundErr("user not found")
		}
		return userView{}, err
	}
	usr, err := a.store.GetUser(id)
	if err != nil {
		return userView{}, err
	}
	spent, err := a.store.SpendByUser(usr.ID, nil)
	if err != nil {
		return userView{}, err
	}
	return newUserView(usr, spent), nil
}

// handleDeleteUser permanently deletes a user. The seeded admin user cannot be
// deleted (it mirrors the admin key and is re-created on startup anyway).
func (a *api) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := a.deleteUserData(r.PathValue("id")); err != nil {
		a.writeDataErr(w, "delete user", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteUserData deletes a user. Deleting the admin user is a *apiError (400);
// an unknown id is a *apiError (404).
func (a *api) deleteUserData(id string) error {
	if id == store.AdminUserID {
		return badRequestErr("the admin user cannot be deleted")
	}
	if err := a.store.DeleteUser(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFoundErr("user not found")
		}
		return err
	}
	return nil
}

// handleListVendors returns vendors (without secrets) plus per-vendor stats.
func (a *api) handleListVendors(w http.ResponseWriter, r *http.Request) {
	snap := a.snap()
	if snap == nil {
		writeJSON(w, http.StatusOK, []vendorView{})
		return
	}
	stats, err := a.store.VendorStats(nil, nil)
	if err != nil {
		a.serverError(w, "vendor stats", err)
		return
	}
	vendors := snap.Vendors()
	views := make([]vendorView, 0, len(vendors))
	for _, v := range vendors {
		st, ok := stats[v.Name]
		views = append(views, newVendorView(v, st, ok))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleTestVendor performs a best-effort connectivity check against a vendor.
func (a *api) handleTestVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	snap := a.snap()
	if snap == nil {
		writeError(w, http.StatusNotFound, "not_found", "vendor not found")
		return
	}
	v, ok := snap.Vendor(name)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "vendor not found")
		return
	}

	// Probe the vendor's host origin (scheme://host); the per-wire endpoints
	// carry vendor-specific paths we can't assume an OpenAI /v1/models route on.
	origin := v.Origin
	if origin == "" {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: "vendor has no origin"})
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: err.Error()})
		return
	}
	if v.Credential.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+v.Credential.APIKey)
	}

	start := a.now()
	resp, err := a.client.Do(req)
	latency := a.now().Sub(start).Milliseconds()
	if err != nil {
		// DNS/connection/timeout: host did not answer.
		writeJSON(w, http.StatusOK, testVendorView{
			Reachable: false, LatencyMS: latency, Error: err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	drain(resp.Body)

	// Any HTTP response (even 401/404) means the host answered: reachable.
	writeJSON(w, http.StatusOK, testVendorView{
		Reachable: true, Status: resp.StatusCode, LatencyMS: latency,
	})
}

// handleSettings returns non-secret runtime settings.
func (a *api) handleSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.settingsData())
}

// settingsData returns non-secret runtime settings (never the admin key).
func (a *api) settingsData() settingsView {
	return settingsView{
		Listen:         a.listenAddr,
		DBPath:         a.dbPath,
		AdminProtected: a.adminKey != "",
		Version:        a.version,
	}
}

// handlePricing returns a flattened list of all per-vendor model prices.
func (a *api) handlePricing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.pricingData())
}

// pricingData returns a flattened, sorted list of all per-vendor model prices.
func (a *api) pricingData() []pricingRow {
	snap := a.snap()
	rows := []pricingRow{}
	if snap != nil {
		for _, v := range snap.Vendors() {
			models := make([]string, 0, len(v.Prices))
			for m := range v.Prices {
				models = append(models, m)
			}
			sort.Strings(models)
			for _, m := range models {
				p := v.Prices[m]
				rows = append(rows, pricingRow{
					Vendor: v.Name, Model: m, Input: p.Input, Output: p.Output, Unit: p.Unit,
				})
			}
		}
	}
	return rows
}

// --- small handler helpers ---

// snap returns the current config snapshot, or nil if no provider is set.
func (a *api) snap() *config.Snapshot {
	if a.snapshot == nil {
		return nil
	}
	return a.snapshot()
}

package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/songguo/songguo/internal/config"
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
		TokenID: r.URL.Query().Get("token_id"),
		Model:   r.URL.Query().Get("model"),
		Vendor:  r.URL.Query().Get("vendor"),
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

	totalSpend, err := a.store.TotalSpend(&since, &until)
	if err != nil {
		a.serverError(w, "total spend", err)
		return
	}
	byMod, err := a.store.SpendByModality(&since, &until)
	if err != nil {
		a.serverError(w, "spend by modality", err)
		return
	}
	stats, err := a.store.OverviewStats(&since, &until)
	if err != nil {
		a.serverError(w, "overview stats", err)
		return
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

	// tokens_active = non-revoked tokens; also compute runway from budgets.
	tokens, err := a.store.ListTokens()
	if err != nil {
		a.serverError(w, "list tokens", err)
		return
	}
	tokensActive := 0
	var remainingBudget float64
	anyBudget := false
	for _, t := range tokens {
		if t.RevokedAt == nil {
			tokensActive++
		}
		if t.Budget != nil {
			anyBudget = true
			spent, err := a.store.SpendByToken(t.ID, nil)
			if err != nil {
				a.serverError(w, "spend by token", err)
				return
			}
			rem := *t.Budget - spent
			if rem > 0 {
				remainingBudget += rem
			}
		}
	}

	// daily_burn = spend over the last 7 days / 7.
	weekAgo := now.AddDate(0, 0, -7)
	weekSpend, err := a.store.TotalSpend(&weekAgo, &now)
	if err != nil {
		a.serverError(w, "weekly spend", err)
		return
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

	writeJSON(w, http.StatusOK, overviewView{
		Range:           rangeView{Since: since.Unix(), Until: until.Unix()},
		TotalSpend:      totalSpend,
		SpendByModality: byMod,
		Requests:        stats.Requests,
		Errors:          stats.Errors,
		ErrorRate:       errorRate,
		LatencyMS:       latencyView{P50: stats.P50, P95: stats.P95, P99: stats.P99},
		VendorsActive:   vendorsActive,
		TokensActive:    tokensActive,
		DailyBurn:       dailyBurn,
		RunwayDays:      runway,
	})
}

// handleUsageSeries returns cost/request/error totals bucketed over time for
// the spend-over-time chart. Window defaults to the last 7 days; bucket defaults
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

	raw := r.URL.Query().Get("bucket")
	var (
		bucket time.Duration
		label  string
	)
	switch raw {
	case "":
		// Default by range: day if range > 2 days, else hour.
		if until.Sub(since) > 48*time.Hour {
			bucket, label = 24*time.Hour, "day"
		} else {
			bucket, label = time.Hour, "hour"
		}
	case "hour":
		bucket, label = time.Hour, "hour"
	case "day":
		bucket, label = 24*time.Hour, "day"
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "bucket must be hour or day")
		return
	}

	points, err := a.store.UsageSeries(since, until, bucket)
	if err != nil {
		if errors.Is(err, store.ErrTooManyBuckets) {
			writeError(w, http.StatusBadRequest, "bad_request", "requested range is too large for the chosen bucket")
			return
		}
		a.serverError(w, "usage series", err)
		return
	}

	views := make([]seriesPoint, 0, len(points))
	for _, p := range points {
		views = append(views, seriesPoint{
			TS:       p.Bucket.UTC().Format(time.RFC3339),
			Cost:     p.Cost,
			Requests: p.Requests,
			Errors:   p.Errors,
		})
	}

	writeJSON(w, http.StatusOK, usageSeriesView{Bucket: label, Points: views})
}

// handleCalls returns a filtered, paginated page of call entries plus the
// total count for the same filter.
func (a *api) handleCalls(w http.ResponseWriter, r *http.Request) {
	f := callFilterFromQuery(r, defaultCallsAPILimit, maxCallsAPILimit)

	entries, err := a.store.QueryCalls(f)
	if err != nil {
		a.serverError(w, "query calls", err)
		return
	}
	total, err := a.store.CountCalls(f)
	if err != nil {
		a.serverError(w, "count calls", err)
		return
	}

	views := make([]entryView, 0, len(entries))
	ids := make([]int64, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	hasTrace, err := a.store.HasPayloads(ids)
	if err != nil {
		a.serverError(w, "has payloads", err)
		return
	}
	for _, e := range entries {
		v := newEntryView(e)
		v.HasTrace = hasTrace[e.ID]
		views = append(views, v)
	}

	writeJSON(w, http.StatusOK, callsView{
		Entries: views,
		Total:   total,
		Limit:   f.Limit,
		Offset:  f.Offset,
	})
}

// handleCallTrace returns the captured request/response payload for a call, or
// 404 when no payload was stored for it. Bodies are returned as UTF-8 text when
// valid, else base64 (flagged per side).
func (a *api) handleCallTrace(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "trace not found")
		return
	}
	p, err := a.store.GetPayload(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "trace not found")
			return
		}
		a.serverError(w, "get payload", err)
		return
	}
	writeJSON(w, http.StatusOK, newTraceView(p))
}

// handleListTokens returns all tokens with computed lifetime spend.
func (a *api) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := a.store.ListTokens()
	if err != nil {
		a.serverError(w, "list tokens", err)
		return
	}
	views := make([]tokenView, 0, len(tokens))
	for _, t := range tokens {
		spent, err := a.store.SpendByToken(t.ID, nil)
		if err != nil {
			a.serverError(w, "spend by token", err)
			return
		}
		views = append(views, newTokenView(t, spent))
	}
	writeJSON(w, http.StatusOK, views)
}

// createTokenReq is the POST /api/tokens body.
type createTokenReq struct {
	Name    string    `json:"name"`
	Budget  *float64  `json:"budget"`
	Scope   *[]string `json:"scope"`
	RPM     *int      `json:"rpm"`
	Capture *bool     `json:"capture"`
}

// handleCreateToken creates a token and returns it once with the plaintext key.
func (a *api) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	nt := store.NewToken{Name: req.Name, Budget: req.Budget, Capture: req.Capture}
	if req.Scope != nil {
		nt.Scope = *req.Scope
	}
	if req.RPM != nil {
		nt.RPM = *req.RPM
	}

	tok, plaintext, err := a.store.CreateToken(nt)
	if err != nil {
		a.serverError(w, "create token", err)
		return
	}
	v := newTokenView(tok, 0)
	v.Key = plaintext
	writeJSON(w, http.StatusCreated, v)
}

// patchTokenReq distinguishes "omitted" from "present" for each field via
// pointers. Note: a null budget cannot be applied because the store's
// TokenUpdate uses a *float64 set-or-unchanged; PATCH cannot reset budget to
// null (documented limitation for v1).
type patchTokenReq struct {
	Name    *string   `json:"name"`
	Budget  *float64  `json:"budget"`
	Scope   *[]string `json:"scope"`
	RPM     *int      `json:"rpm"`
	Capture *bool     `json:"capture"`
}

// handlePatchToken applies a subset of fields to a token.
func (a *api) handlePatchToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req patchTokenReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	upd := store.TokenUpdate{
		Name:    req.Name,
		Budget:  req.Budget,
		Scope:   req.Scope,
		RPM:     req.RPM,
		Capture: req.Capture,
	}
	tok, err := a.store.UpdateToken(id, upd)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		a.serverError(w, "update token", err)
		return
	}
	spent, err := a.store.SpendByToken(tok.ID, nil)
	if err != nil {
		a.serverError(w, "spend by token", err)
		return
	}
	writeJSON(w, http.StatusOK, newTokenView(tok, spent))
}

// handleRevokeToken revokes a token and returns its updated view.
func (a *api) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.store.RevokeToken(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		a.serverError(w, "revoke token", err)
		return
	}
	tok, err := a.store.GetToken(id)
	if err != nil {
		a.serverError(w, "get token", err)
		return
	}
	spent, err := a.store.SpendByToken(tok.ID, nil)
	if err != nil {
		a.serverError(w, "spend by token", err)
		return
	}
	writeJSON(w, http.StatusOK, newTokenView(tok, spent))
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

	// Probe the vendor's host origin (scheme://host), since base_url now carries
	// a vendor-specific path prefix we can't assume an OpenAI /v1/models route on.
	url, err := originOf(v.BaseURL)
	if err != nil {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: err.Error()})
		return
	}
	ctx, cancel := contextWithTimeout(r, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, testVendorView{Reachable: false, Error: err.Error()})
		return
	}
	if len(v.Credentials) > 0 && v.Credentials[0].APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+v.Credentials[0].APIKey)
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
	var settings config.Settings
	if snap := a.snap(); snap != nil {
		settings = snap.Settings()
	}
	writeJSON(w, http.StatusOK, settingsView{
		Listen:          settings.Listen,
		ConfigPath:      a.configPath,
		DBPath:          a.dbPath,
		AdminProtected:  a.adminKey != "",
		Version:         a.version,
		Capture:         settings.Capture,
		CaptureMaxBytes: settings.CaptureMaxBytes,
		CaptureRetain:   settings.CaptureRetain,
	})
}

// handlePricing returns a flattened list of all per-vendor model prices.
func (a *api) handlePricing(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, rows)
}

// --- small handler helpers ---

// snap returns the current config snapshot, or nil if no provider is set.
func (a *api) snap() *config.Snapshot {
	if a.snapshot == nil {
		return nil
	}
	return a.snapshot()
}

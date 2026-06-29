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
		UserID: r.URL.Query().Get("user_id"),
		Model:  r.URL.Query().Get("model"),
		Vendor: r.URL.Query().Get("vendor"),
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
		Requests:        stats.Requests,
		Errors:          stats.Errors,
		ErrorRate:       errorRate,
		LatencyMS:       latencyView{P50: stats.P50, P95: stats.P95, P99: stats.P99},
		VendorsActive:   vendorsActive,
		UsersActive:     usersActive,
		DailyBurn:       dailyBurn,
		RunwayDays:      runway,
	}, nil
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
	var (
		bucket time.Duration
		label  string
	)
	switch bucketRaw {
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
		return usageSeriesView{}, badRequestErr("bucket must be hour or day")
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
			TS:       p.Bucket.UTC().Format(time.RFC3339),
			Cost:     p.Cost,
			Requests: p.Requests,
			Errors:   p.Errors,
		})
	}

	return usageSeriesView{Bucket: label, Points: views}, nil
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

	ids := make([]int64, 0, len(entries))
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
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
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
func (a *api) callTraceData(id int64) (traceView, error) {
	p, err := a.store.GetPayload(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return traceView{}, notFoundErr("trace not found")
		}
		return traceView{}, err
	}
	return newTraceView(p), nil
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

// createUserReq is the POST /api/users body.
type createUserReq struct {
	Name   string    `json:"name"`
	Budget *float64  `json:"budget,omitempty"`
	Scope  *[]string `json:"scope,omitempty"`
	RPM    *int      `json:"rpm,omitempty"`
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
// (the only time it is ever exposed). A missing name is a *apiError (400).
func (a *api) createUserData(req createUserReq) (userView, error) {
	if req.Name == "" {
		return userView{}, badRequestErr("name is required")
	}
	nu := store.NewUser{Name: req.Name, Budget: req.Budget}
	if req.Scope != nil {
		nu.Scope = *req.Scope
	}
	if req.RPM != nil {
		nu.RPM = *req.RPM
	}

	usr, plaintext, err := a.store.CreateUser(nu)
	if err != nil {
		return userView{}, err
	}
	v := newUserView(usr, 0)
	v.Key = plaintext
	return v, nil
}

// patchUserReq distinguishes "omitted" from "present" for each field via
// pointers. Note: a null budget cannot be applied because the store's
// UserUpdate uses a *float64 set-or-unchanged; PATCH cannot reset budget to
// null (documented limitation for v1).
type patchUserReq struct {
	Name   *string   `json:"name,omitempty"`
	Budget *float64  `json:"budget,omitempty"`
	Scope  *[]string `json:"scope,omitempty"`
	RPM    *int      `json:"rpm,omitempty"`
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
		Name:   req.Name,
		Budget: req.Budget,
		Scope:  req.Scope,
		RPM:    req.RPM,
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
	var settings config.Settings
	if snap := a.snap(); snap != nil {
		settings = snap.Settings()
	}
	return settingsView{
		Listen:         a.listenAddr,
		DBPath:         a.dbPath,
		AdminProtected: a.adminKey != "",
		Version:        a.version,
		Capture:        settings.Capture,
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

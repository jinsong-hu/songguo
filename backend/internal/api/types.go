package api

import (
	"encoding/base64"
	"time"
	"unicode/utf8"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

// userView is the JSON representation of a user, including computed lifetime
// spend and active state. It never exposes the key hash or plaintext key.
type userView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	Budget    *float64 `json:"budget"`
	Scope     []string `json:"scope"`
	RPM       int      `json:"rpm"`
	CreatedAt string   `json:"created_at"`
	RevokedAt *string  `json:"revoked_at"`
	Spent     float64  `json:"spent"`
	Active    bool     `json:"active"`
	// LastSeen is the RFC3339 timestamp of the user's most recent call, or nil
	// if the user has never made one.
	LastSeen *string `json:"last_seen"`
	// Key carries the plaintext key. Empty for users created before key storage
	// existed; omitted in that case.
	Key string `json:"key,omitempty"`
}

// newUserView converts a store.User plus its lifetime spend into a view.
func newUserView(u store.User, spent float64) userView {
	scope := u.Scope
	if scope == nil {
		scope = []string{}
	}
	v := userView{
		ID:        u.ID,
		Name:      u.Name,
		KeyPrefix: u.KeyPrefix,
		Budget:    u.Budget,
		Scope:     scope,
		RPM:       u.RPM,
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
		Spent:     spent,
		Active:    u.RevokedAt == nil,
		Key:       u.KeyFull,
	}
	if u.RevokedAt != nil {
		s := u.RevokedAt.UTC().Format(time.RFC3339)
		v.RevokedAt = &s
	}
	return v
}

// entryView is the JSON representation of a call entry.
type entryView struct {
	ID           int64             `json:"id"`
	TS           string            `json:"ts"`
	UserID       string            `json:"user_id"`
	Model        string            `json:"model"`
	Modality     string            `json:"modality"`
	Vendor       string            `json:"vendor"`
	CredentialID string            `json:"credential_id"`
	Wire         string            `json:"wire"`
	Confidence   string            `json:"confidence"`
	Status       int               `json:"status"`
	Err          string            `json:"err"`
	Usage        map[string]any    `json:"usage"`
	Cost         float64           `json:"cost"`
	LatencyMS    int64             `json:"latency_ms"`
	Stream       bool              `json:"stream"`
	Tags         map[string]string `json:"tags"`
	HasTrace     bool              `json:"has_trace"`
}

// newEntryView converts a calls.Entry into its JSON view.
func newEntryView(e calls.Entry) entryView {
	usage := e.Usage
	if usage == nil {
		usage = map[string]any{}
	}
	tags := e.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	return entryView{
		ID:           e.ID,
		TS:           e.TS.UTC().Format(time.RFC3339),
		UserID:       e.UserID,
		Model:        e.Model,
		Modality:     string(e.Modality),
		Vendor:       e.Vendor,
		CredentialID: e.CredentialID,
		Wire:         e.Wire,
		Confidence:   string(e.Confidence),
		Status:       e.Status,
		Err:          e.Err,
		Usage:        usage,
		Cost:         e.Cost,
		LatencyMS:    e.LatencyMS,
		Stream:       e.Stream,
		Tags:         tags,
	}
}

// rangeView reports the resolved [since, until) window as unix seconds.
type rangeView struct {
	Since int64 `json:"since"`
	Until int64 `json:"until"`
}

// latencyView holds latency percentiles in milliseconds.
type latencyView struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
}

// tokenView holds summed normalized token counts over a window.
type tokenView struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Cached float64 `json:"cached"`
}

// overviewView is the GET /api/overview response.
type overviewView struct {
	Range           rangeView          `json:"range"`
	TotalSpend      float64            `json:"total_spend"`
	SpendByModality map[string]float64 `json:"spend_by_modality"`
	Tokens          tokenView          `json:"tokens"`
	Requests        int                `json:"requests"`
	Errors          int                `json:"errors"`
	ErrorRate       float64            `json:"error_rate"`
	LatencyMS       latencyView        `json:"latency_ms"`
	VendorsActive   int                `json:"vendors_active"`
	UsersActive     int                `json:"users_active"`
	// ActiveCallers is the count of distinct users with traffic in the window,
	// as opposed to UsersActive (non-revoked users in config).
	ActiveCallers int      `json:"active_callers"`
	DailyBurn     float64  `json:"daily_burn"`
	RunwayDays    *float64 `json:"runway_days"`
}

// seriesPoint is one bucket in the GET /api/usage/series response.
type seriesPoint struct {
	TS           string  `json:"ts"`
	Cost         float64 `json:"cost"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	InputTokens  float64 `json:"input_tokens"`
	OutputTokens float64 `json:"output_tokens"`
	CachedTokens float64 `json:"cached_tokens"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

// usageSeriesView is the GET /api/usage/series response.
type usageSeriesView struct {
	Bucket string        `json:"bucket"`
	Points []seriesPoint `json:"points"`
}

// tokensByModelPoint is one bucket in the GET /api/usage/tokens-by-model
// response: total cost plus total tokens (input+output) keyed by model.
type tokensByModelPoint struct {
	TS     string             `json:"ts"`
	Cost   float64            `json:"cost"`
	Tokens map[string]float64 `json:"tokens"`
}

// tokensByModelView is the GET /api/usage/tokens-by-model response: the ordered
// model key set (top N + "Other") and per-bucket token/cost points.
type tokensByModelView struct {
	Bucket string               `json:"bucket"`
	Models []string             `json:"models"`
	Points []tokensByModelPoint `json:"points"`
}

// breakdownRow is one group's aggregates in the GET /api/usage/breakdown response.
type breakdownRow struct {
	Key          string  `json:"key"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	InputTokens  float64 `json:"input_tokens"`
	OutputTokens float64 `json:"output_tokens"`
	CachedTokens float64 `json:"cached_tokens"`
	Cost         float64 `json:"cost"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

// breakdownView is the GET /api/usage/breakdown response.
type breakdownView struct {
	Range     rangeView      `json:"range"`
	Dimension string         `json:"dimension"`
	Rows      []breakdownRow `json:"rows"`
}

// errorsView is the GET /api/usage/errors response: error-row counts by class.
type errorsView struct {
	Range       rangeView `json:"range"`
	RateLimited int       `json:"rate_limited"`
	ClientError int       `json:"client_error"`
	ServerError int       `json:"server_error"`
	Transport   int       `json:"transport"`
}

// callsView is the GET /api/calls response.
type callsView struct {
	Entries []entryView `json:"entries"`
	Total   int         `json:"total"`
	Limit   int         `json:"limit"`
	Offset  int         `json:"offset"`
}

// credentialView is a credential with its key masked. The raw key is NEVER
// included.
type credentialView struct {
	ID        string `json:"id"`
	MaskedKey string `json:"masked_key"`
}

// priceView is a single model price.
type priceView struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
}

// vendorStatsView is the per-vendor health/usage summary.
type vendorStatsView struct {
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	ErrorRate    float64 `json:"error_rate"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	LastStatus   int     `json:"last_status"`
	Healthy      bool    `json:"healthy"`
}

// vendorView is the JSON representation of a vendor (without secrets).
type vendorView struct {
	Name         string               `json:"name"`
	Origin       string               `json:"origin"`
	Endpoints    map[string]string    `json:"endpoints"`
	ServedModels []string             `json:"served_models"`
	Priority     int                  `json:"priority"`
	Weight       int                  `json:"weight"`
	Credential   credentialView       `json:"credential"`
	Prices       map[string]priceView `json:"prices"`
	Stats        vendorStatsView      `json:"stats"`
}

// newVendorView builds a vendor view from config plus computed stats. The raw
// api_key is intentionally dropped; only a masked preview is emitted.
func newVendorView(v config.Vendor, stat store.VendorStat, hasStat bool) vendorView {
	models := v.ServedModels
	if models == nil {
		models = []string{}
	}

	cred := credentialView{ID: v.Credential.ID, MaskedKey: maskKey(v.Credential.APIKey)}

	prices := make(map[string]priceView, len(v.Prices))
	for model, p := range v.Prices {
		prices[model] = priceView{Input: p.Input, Output: p.Output, Unit: p.Unit}
	}

	endpoints := v.Endpoints
	if endpoints == nil {
		endpoints = map[string]string{}
	}

	sv := vendorStatsView{Healthy: true} // no traffic => healthy.
	if hasStat {
		sv.Requests = stat.Requests
		sv.Errors = stat.Errors
		sv.AvgLatencyMS = stat.AvgLatency
		sv.LastStatus = stat.LastStatus
		if stat.Requests > 0 {
			sv.ErrorRate = float64(stat.Errors) / float64(stat.Requests)
		}
		sv.Healthy = stat.Errors == 0
	}

	return vendorView{
		Name:         v.Name,
		Origin:       v.Origin,
		Endpoints:    endpoints,
		ServedModels: models,
		Priority:     v.Priority,
		Weight:       v.Weight,
		Credential:   cred,
		Prices:       prices,
		Stats:        sv,
	}
}

// testVendorView is the POST /api/vendors/{name}/test response.
type testVendorView struct {
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// settingsView is the GET /api/settings response. It never exposes the admin
// key.
type settingsView struct {
	Listen         string `json:"listen"`
	DBPath         string `json:"db_path"`
	AdminProtected bool   `json:"admin_protected"`
	Version        string `json:"version"`
	Capture        bool   `json:"capture"`
}

// traceSideView is one side (request or response) of a captured trace.
type traceSideView struct {
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	BodyBase64  bool              `json:"body_base64,omitempty"`
	ContentType string            `json:"content_type"`
}

// traceView is the GET /api/calls/{id}/trace response.
type traceView struct {
	CallID     int64         `json:"call_id"`
	Request    traceSideView `json:"request"`
	Response   traceSideView `json:"response"`
	CapturedAt string        `json:"captured_at"`
}

// pricingRow is one flattened pricing entry for GET /api/pricing.
type pricingRow struct {
	Vendor string  `json:"vendor"`
	Model  string  `json:"model"`
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
}

// newTraceView converts a stored payload into its JSON trace view, encoding
// each body as UTF-8 text when valid, else base64.
func newTraceView(p store.Payload) traceView {
	return traceView{
		CallID:     p.CallID,
		Request:    newTraceSide(p.ReqHeaders, p.ReqBody, p.ReqContentType),
		Response:   newTraceSide(p.RespHeaders, p.RespBody, p.RespContentType),
		CapturedAt: p.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// newTraceSide builds one side of a trace, choosing a UTF-8 string body when
// the bytes are valid UTF-8 and a base64 encoding (with body_base64=true)
// otherwise so binary payloads survive JSON transport.
func newTraceSide(headers map[string]string, body []byte, contentType string) traceSideView {
	if headers == nil {
		headers = map[string]string{}
	}
	side := traceSideView{
		Headers:     headers,
		ContentType: contentType,
	}
	if utf8.Valid(body) {
		side.Body = string(body)
	} else {
		side.Body = base64.StdEncoding.EncodeToString(body)
		side.BodyBase64 = true
	}
	return side
}

// maskKey returns a masked preview of an API key: first 3 + "…" + last 2 chars,
// or "••••" if the key is too short to mask meaningfully. It never returns the
// raw key.
func maskKey(key string) string {
	const ellipsis = "…"
	if len(key) < 6 {
		return "••••"
	}
	return key[:3] + ellipsis + key[len(key)-2:]
}

package api

import (
	"encoding/base64"
	"time"
	"unicode/utf8"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
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
	InputTokens  float64           `json:"input_tokens"`
	OutputTokens float64           `json:"output_tokens"`
	CachedTokens float64           `json:"cached_tokens"`
	LatencyMS    int64             `json:"latency_ms"`
	Stream       bool              `json:"stream"`
	Tags         map[string]string `json:"tags"`
	// Coding-agent attribution (empty for ordinary API traffic).
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	ParentAgentID string `json:"parent_agent_id"`
	HasTrace      bool   `json:"has_trace"`
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
		ID:            e.ID,
		TS:            e.TS.UTC().Format(time.RFC3339),
		UserID:        e.UserID,
		Model:         e.Model,
		Modality:      string(e.Modality),
		Vendor:        e.Vendor,
		CredentialID:  e.CredentialID,
		Wire:          e.Wire,
		Confidence:    string(e.Confidence),
		Status:        e.Status,
		Err:           e.Err,
		Usage:         usage,
		Cost:          e.Cost,
		InputTokens:   e.InputTokens,
		OutputTokens:  e.OutputTokens,
		CachedTokens:  e.CachedTokens,
		LatencyMS:     e.LatencyMS,
		Stream:        e.Stream,
		Tags:          tags,
		SessionID:     e.SessionID,
		AgentID:       e.AgentID,
		ParentAgentID: e.ParentAgentID,
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

// sessionStatsView is the GET /api/sessions/overview response: aggregate stats
// over coding-agent sessions in the window. Outcome (completed/errored/
// interrupted) is inferred from each session's last call — an interaction-level
// signal off the ledger, not a judgment on the coding task itself.
type sessionStatsView struct {
	Range       rangeView `json:"range"`
	Sessions    int       `json:"sessions"`
	Completed   int       `json:"completed"`
	Errored     int       `json:"errored"`
	Interrupted int       `json:"interrupted"`
	// WithSubagents: sessions that spawned at least one subagent.
	WithSubagents int     `json:"with_subagents"`
	TotalTurns    int     `json:"total_turns"`
	TotalTokens   float64 `json:"total_tokens"`
	AvgTurns      float64 `json:"avg_turns"`
	AvgTokens     float64 `json:"avg_tokens"`
	AvgDuration   float64 `json:"avg_duration"` // seconds
	TurnsP50      int64   `json:"turns_p50"`
	TurnsP95      int64   `json:"turns_p95"`
	TokensP50     int64   `json:"tokens_p50"`
	TokensP95     int64   `json:"tokens_p95"`
	DurationP50   int64   `json:"duration_p50"` // seconds
	DurationP95   int64   `json:"duration_p95"` // seconds
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

// feedRowView is one row of the activity feed: an aggregated session or a
// standalone request (see kind). Fields not relevant to a kind are zero-valued.
type feedRowView struct {
	Kind         string   `json:"kind"` // "session" | "request"
	SessionID    string   `json:"session_id,omitempty"`
	RequestID    int64    `json:"request_id,omitempty"`
	Calls        int      `json:"calls"`
	Cost         float64  `json:"cost"`
	InputTokens  float64  `json:"input_tokens"`
	OutputTokens float64  `json:"output_tokens"`
	FirstTS      string   `json:"first_ts"`
	LastTS       string   `json:"last_ts"`
	ErrorCount   int      `json:"error_count"`
	Models       []string `json:"models"`
	Vendors      []string `json:"vendors"`
	// Single-call fields, populated only for request rows.
	Model      string `json:"model,omitempty"`
	Vendor     string `json:"vendor,omitempty"`
	Wire       string `json:"wire,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Modality   string `json:"modality,omitempty"`
	Status     int    `json:"status,omitempty"`
	LatencyMS  int64  `json:"latency_ms,omitempty"`
	Stream     bool   `json:"stream,omitempty"`
}

// newFeedRowView converts a store.FeedRow into its JSON view.
func newFeedRowView(r store.FeedRow) feedRowView {
	models := r.Models
	if models == nil {
		models = []string{}
	}
	vendors := r.Vendors
	if vendors == nil {
		vendors = []string{}
	}
	return feedRowView{
		Kind:         r.Kind,
		SessionID:    r.SessionID,
		RequestID:    r.RequestID,
		Calls:        r.Calls,
		Cost:         r.Cost,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		FirstTS:      r.FirstTS.UTC().Format(time.RFC3339),
		LastTS:       r.LastTS.UTC().Format(time.RFC3339),
		ErrorCount:   r.ErrorCount,
		Models:       models,
		Vendors:      vendors,
		Model:        r.Model,
		Vendor:       r.Vendor,
		Wire:         r.Wire,
		Confidence:   string(r.Confidence),
		Modality:     string(r.Modality),
		Status:       r.Status,
		LatencyMS:    r.LatencyMS,
		Stream:       r.Stream,
	}
}

// feedView is the GET /api/feed response.
type feedView struct {
	Rows   []feedRowView `json:"rows"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// agentNodeView is one node in a session's main-loop→subagent tree. Rollups
// cover the whole subtree (this agent plus its descendants).
type agentNodeView struct {
	AgentID      string          `json:"agent_id"`
	Calls        int             `json:"calls"`
	Cost         float64         `json:"cost"`
	InputTokens  float64         `json:"input_tokens"`
	OutputTokens float64         `json:"output_tokens"`
	Children     []agentNodeView `json:"children"`
}

// sessionView is the GET /api/sessions/{id} response: session-level rollups, the
// agent tree, and the session's calls (oldest first).
type sessionView struct {
	SessionID    string          `json:"session_id"`
	Calls        int             `json:"calls"`
	Cost         float64         `json:"cost"`
	InputTokens  float64         `json:"input_tokens"`
	OutputTokens float64         `json:"output_tokens"`
	ErrorCount   int             `json:"error_count"`
	FirstTS      string          `json:"first_ts"`
	LastTS       string          `json:"last_ts"`
	Models       []string        `json:"models"`
	Vendors      []string        `json:"vendors"`
	Agents       []agentNodeView `json:"agents"`
	Entries      []entryView     `json:"entries"`
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

// contextCompositionView is the GET /api/context/composition response: the
// aggregated context-window decomposition over a window. Sources reuse
// compose.Source, whose JSON ({key, tokens, cached, children:[{key,tokens}]})
// matches the frontend SourceSlice/ProducerSlice contract exactly.
type contextCompositionView struct {
	Range    rangeView        `json:"range"`
	Requests int              `json:"requests"`
	AvgTotal float64          `json:"avg_total"`
	Sources  []compose.Source `json:"sources"`
}

// sessionContextView is the GET /api/sessions/{id}/context response: per-turn
// composition, the latest turn's full snapshot (with producers), and a dwell
// list (empty until lineage tracking lands).
type sessionContextView struct {
	SessionID string            `json:"session_id"`
	Turns     []contextTurnView `json:"turns"`
	Snapshot  []compose.Source  `json:"snapshot"`
	Dwell     []dwellBlockView  `json:"dwell"`
}

// contextTurnView is one turn's composition. Sources maps top-level source key
// to tokens only (producers and cached are dropped from this map).
type contextTurnView struct {
	Seq     int              `json:"seq"`
	TS      string           `json:"ts"`
	AgentID string           `json:"agent_id"`
	Total   int64            `json:"total"`
	Cached  int64            `json:"cached"`
	Sources map[string]int64 `json:"sources"`
}

// dwellBlockView describes how long a producer's block has persisted in the
// context across turns. Reserved for a later lineage phase; currently unused.
type dwellBlockView struct {
	Label    string `json:"label"`
	Producer string `json:"producer"`
	Tokens   int64  `json:"tokens"`
	Turns    int    `json:"turns"`
	Dwell    int    `json:"dwell"`
}

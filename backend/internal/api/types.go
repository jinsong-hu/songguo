package api

import (
	"encoding/base64"
	"time"
	"unicode/utf8"

	"github.com/songguo/songguo/internal/bodycodec"
	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/compose"
	"github.com/songguo/songguo/internal/concurrency"
	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/router"
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
	Capture   bool     `json:"capture"`
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
		Capture:   u.Capture,
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
	ID           string `json:"id"`
	TS           string `json:"ts"`
	TSEnd        string `json:"ts_end,omitempty"` // empty while the call is in flight (pending)
	Pending      bool   `json:"pending"`          // true when created but not yet finalized
	UserID       string `json:"user_id"`
	Model        string `json:"model"`
	Modality     string `json:"modality"`
	Vendor       string `json:"vendor"`
	CredentialID string `json:"credential_id"`
	Wire         string `json:"wire"`
	Confidence   string `json:"confidence"`
	Status       int    `json:"status"`
	Err          string `json:"err"`
	// Outcome is what actually happened, derived from (Status, Err) — see
	// calls.OutcomeOf. Status alone cannot say: a 429 songguo issued and a 429 a
	// provider returned are the same integer. Status and Pending are unchanged
	// and still authoritative; this is additive.
	Outcome string `json:"outcome"`
	// Abandoned marks a pending row created before this gateway process booted,
	// so no live request owns it and none ever will. Only the gateway knows its
	// own boot time, which is why the split is made here and not in the client.
	Abandoned           bool              `json:"abandoned"`
	Usage               map[string]any    `json:"usage"`
	Cost                float64           `json:"cost"`
	InputTokens         float64           `json:"input_tokens"`
	OutputTokens        float64           `json:"output_tokens"`
	CachedTokens        float64           `json:"cache_read_input_tokens"`
	CacheCreationTokens float64           `json:"cache_creation_input_tokens"`
	ThinkingTokens      float64           `json:"thinking_tokens"`
	ToolCalls           int               `json:"tool_calls"`
	ToolTokens          float64           `json:"tool_tokens"`
	LatencyMS           int64             `json:"latency_ms"`
	TTFTMS              int64             `json:"ttft_ms"`
	GenerationMS        int64             `json:"generation_ms"`
	OutputTPS           float64           `json:"output_tokens_per_second"`
	Stream              bool              `json:"stream"`
	Tags                map[string]string `json:"tags"`
	ClientName          string            `json:"client_name"`
	ClientVersion       string            `json:"client_version"`
	// Best-effort caller OS (normalized family, e.g. MacOS) and version; empty
	// when unavailable.
	ClientOS        string `json:"client_os"`
	ClientOSVersion string `json:"client_os_version"`
	// Coding-agent attribution (empty for ordinary API traffic).
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	ParentAgentID string `json:"parent_agent_id"`
	// Why the call was made: "main" (a visible turn) or a harness utility kind
	// (monitor | count_tokens | utility). Empty on legacy rows ⇒ treated as main.
	Entrypoint  string               `json:"entrypoint"`
	HasTrace    bool                 `json:"has_trace"`
	Composition *compose.Composition `json:"composition,omitempty"`
}

// isAbandoned reports whether a pending row was created before this gateway
// process booted, so nothing alive owns it and nothing ever will.
//
// A zero bootTime declines the question: every pending row then reads as in
// flight. That is the safe direction — it withholds a verdict rather than
// inventing one.
//
// Known limitation, and the reason in-flight rows show elapsed time instead of a
// verdict: this cannot see a row that leaked to pending while the process stayed
// up. Same boot reads as in flight indefinitely. A boot id column would not help
// either — only an invented timeout would, and songguo does not invent those.
func isAbandoned(e calls.Entry, bootTime time.Time) bool {
	return e.Status == calls.StatusPending && !bootTime.IsZero() && e.TS.Before(bootTime)
}

// outcomeFor classifies a call, resolving the one case calls.OutcomeOf cannot:
// which kind of unfinished a pending row is.
func outcomeFor(e calls.Entry, bootTime time.Time) calls.Outcome {
	if isAbandoned(e, bootTime) {
		return calls.OutcomeAbandoned
	}
	return calls.OutcomeOf(e.Status, e.Err)
}

// newEntryView converts a calls.Entry into its JSON view.
//
// bootTime is when this gateway process started; it is what separates a pending
// row that is still running from one nothing will ever finish. Pass the zero
// time to decline the distinction — every pending row then reads as in flight,
// which is the safe direction: it withholds a verdict rather than inventing one.
func newEntryView(e calls.Entry, bootTime time.Time) entryView {
	usage := e.Usage
	if usage == nil {
		usage = map[string]any{}
	}
	tags := e.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	tsEnd := ""
	if !e.TSEnd.IsZero() {
		tsEnd = e.TSEnd.UTC().Format(time.RFC3339)
	}
	pending := e.Status == calls.StatusPending
	abandoned := isAbandoned(e, bootTime)
	return entryView{
		ID:                  e.ID,
		TS:                  e.TS.UTC().Format(time.RFC3339),
		TSEnd:               tsEnd,
		Pending:             pending,
		Outcome:             string(outcomeFor(e, bootTime)),
		Abandoned:           abandoned,
		UserID:              e.UserID,
		Model:               e.Model,
		Modality:            string(e.Modality),
		Vendor:              e.Vendor,
		CredentialID:        e.CredentialID,
		Wire:                e.Wire,
		Confidence:          string(e.Confidence),
		Status:              e.Status,
		Err:                 e.Err,
		Usage:               usage,
		Cost:                e.Cost,
		InputTokens:         e.InputTokens,
		OutputTokens:        e.OutputTokens,
		CachedTokens:        e.CachedTokens,
		CacheCreationTokens: e.CacheCreationTokens,
		ThinkingTokens:      e.ThinkingTokens,
		ToolCalls:           e.ToolCalls,
		ToolTokens:          e.ToolTokens,
		LatencyMS:           e.LatencyMS,
		TTFTMS:              e.TTFTMS,
		GenerationMS:        e.GenerationMS,
		OutputTPS:           outputTokensPerSecond(e.OutputTokens, e.GenerationMS),
		Stream:              e.Stream,
		Tags:                tags,
		ClientName:          e.ClientName,
		ClientVersion:       e.ClientVersion,
		// Caller OS, read-only from headers (X-Stainless-Os, else codex UA comment).
		ClientOS:        e.ClientOS,
		ClientOSVersion: e.ClientOSVersion,
		// Coding-agent attribution.
		SessionID:     e.SessionID,
		AgentID:       e.AgentID,
		ParentAgentID: e.ParentAgentID,
		Entrypoint:    string(e.Entrypoint),
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

type rateView struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

// tokenView holds summed normalized token counts over a window. Input, Cached
// (cache reads), and CacheCreation are disjoint input parts; Thinking is a subset
// of Output.
type tokenView struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Cached        float64 `json:"cached"`
	CacheCreation float64 `json:"cache_creation"`
	Thinking      float64 `json:"thinking"`
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
	TTFTMS          latencyView        `json:"ttft_ms"`
	OutputTPS       rateView           `json:"output_tokens_per_second"`
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
	Pending     int       `json:"pending"`
	// WithSubagents: sessions that spawned at least one subagent.
	WithSubagents  int     `json:"with_subagents"`
	TotalTurns     int     `json:"total_turns"`
	TotalTokens    float64 `json:"total_tokens"`
	TotalToolCalls int     `json:"total_tool_calls"`
	AvgTurns       float64 `json:"avg_turns"`
	AvgTokens      float64 `json:"avg_tokens"`
	AvgDuration    float64 `json:"avg_duration"` // seconds
	AvgToolCalls   float64 `json:"avg_tool_calls"`
	TurnsP50       int64   `json:"turns_p50"`
	TurnsP95       int64   `json:"turns_p95"`
	TokensP50      int64   `json:"tokens_p50"`
	TokensP95      int64   `json:"tokens_p95"`
	DurationP50    int64   `json:"duration_p50"` // seconds
	DurationP95    int64   `json:"duration_p95"` // seconds
	ToolCallsP50   int64   `json:"tool_calls_p50"`
	ToolCallsP95   int64   `json:"tool_calls_p95"`
}

// seriesPoint is one bucket in the GET /api/usage/series response.
type seriesPoint struct {
	TS                  string  `json:"ts"`
	Cost                float64 `json:"cost"`
	Requests            int     `json:"requests"`
	Errors              int     `json:"errors"`
	InputTokens         float64 `json:"input_tokens"`
	OutputTokens        float64 `json:"output_tokens"`
	CachedTokens        float64 `json:"cache_read_input_tokens"`
	CacheCreationTokens float64 `json:"cache_creation_input_tokens"`
	ThinkingTokens      float64 `json:"thinking_tokens"`
	AvgLatencyMS        float64 `json:"avg_latency_ms"`
	AvgTTFTMS           float64 `json:"avg_ttft_ms"`
	AvgOutputTPS        float64 `json:"avg_output_tokens_per_second"`
}

func outputTokensPerSecond(tokens float64, generationMS int64) float64 {
	if tokens <= 0 || generationMS <= 0 {
		return 0
	}
	return tokens * 1000 / float64(generationMS)
}

// usageSeriesView is the GET /api/usage/series response.
type usageSeriesView struct {
	Bucket string        `json:"bucket"`
	Points []seriesPoint `json:"points"`
}

// tokensByModelPoint is one bucket in the GET /api/usage/tokens-by-model
// response: total cost, total tokens (input+output) keyed by model, cost keyed
// by model, and per-model average TTFT (ms) and output throughput (tokens/sec).
// All four maps carry the same key set.
type tokensByModelPoint struct {
	TS     string             `json:"ts"`
	Cost   float64            `json:"cost"`
	Tokens map[string]float64 `json:"tokens"`
	Costs  map[string]float64 `json:"costs"`
	TTFT   map[string]float64 `json:"ttft"`
	TPS    map[string]float64 `json:"tps"`
}

// tokensByModelView is the GET /api/usage/tokens-by-model response: the ordered
// model key set (top N + "Other") and per-bucket token/cost points.
type tokensByModelView struct {
	Bucket string               `json:"bucket"`
	Models []string             `json:"models"`
	Points []tokensByModelPoint `json:"points"`
}

// successByModelPoint is one bucket in the GET /api/usage/success-by-model
// response: request and error counts keyed by dimension key. Requests and Errors
// carry the same key set; the client derives success % as (req-err)/req.
type successByModelPoint struct {
	TS       string         `json:"ts"`
	Requests map[string]int `json:"requests"`
	Errors   map[string]int `json:"errors"`
}

// successByModelView is the GET /api/usage/success-by-model response: the ordered
// key set (top N by requests + "Other") and per-bucket request/error points.
type successByModelView struct {
	Bucket string                `json:"bucket"`
	Models []string              `json:"models"`
	Points []successByModelPoint `json:"points"`
}

// cacheByModelPoint is one bucket in the GET /api/usage/cache-by-model response:
// cache-read and total-input token sums keyed by dimension key. CacheRead and Input
// carry the same key set; the client derives the cache-hit ratio as CacheRead/Input.
type cacheByModelPoint struct {
	TS        string             `json:"ts"`
	CacheRead map[string]float64 `json:"cache_read"`
	Input     map[string]float64 `json:"input"`
}

// cacheByModelView is the GET /api/usage/cache-by-model response: the ordered key
// set (top N by total input + "Other") and per-bucket cache-read/input points.
type cacheByModelView struct {
	Bucket string              `json:"bucket"`
	Models []string            `json:"models"`
	Points []cacheByModelPoint `json:"points"`
}

// breakdownRow is one group's aggregates in the GET /api/usage/breakdown response.
type breakdownRow struct {
	Key                 string  `json:"key"`
	Requests            int     `json:"requests"`
	Errors              int     `json:"errors"`
	InputTokens         float64 `json:"input_tokens"`
	OutputTokens        float64 `json:"output_tokens"`
	CachedTokens        float64 `json:"cache_read_input_tokens"`
	CacheCreationTokens float64 `json:"cache_creation_input_tokens"`
	ThinkingTokens      float64 `json:"thinking_tokens"`
	Cost                float64 `json:"cost"`
	AvgLatencyMS        float64 `json:"avg_latency_ms"`
}

// breakdownView is the GET /api/usage/breakdown response.
type breakdownView struct {
	Range     rangeView      `json:"range"`
	Dimension string         `json:"dimension"`
	Rows      []breakdownRow `json:"rows"`
}

// errorsView is the GET /api/usage/errors response: failure counts by class.
// The classes split on whose failure it was before splitting on the code,
// because the code alone cannot say — see store.ErrorClasses.
type errorsView struct {
	Range       rangeView `json:"range"`
	RateLimited int       `json:"rate_limited"`
	ClientError int       `json:"client_error"`
	ServerError int       `json:"server_error"`
	Transport   int       `json:"transport"`
	Gateway     int       `json:"gateway"`
}

// errorCodeRow is one failure kind and its count. Keyed by outcome as well as
// status: grouping on the integer alone merged songguo's own 429 with the
// provider's, and four distinct causes into one 502.
type errorCodeRow struct {
	Status  int    `json:"status"`
	Outcome string `json:"outcome"`
	Count   int    `json:"count"`
}

// errorCodesView is the GET /api/usage/error-codes response: error-row counts
// grouped by upstream status code, ranked by count desc.
type errorCodesView struct {
	Range rangeView      `json:"range"`
	Rows  []errorCodeRow `json:"rows"`
}

// facetRow is one selectable value of a dashboard filter and the request count
// that ranked it.
type facetRow struct {
	Key      string `json:"key"`
	Requests int    `json:"requests"`
}

// facetsView is the GET /api/usage/facets response: the models, vendors and
// caller clients that appear in the window, ranked by request count desc, for
// the Overview page's Models, Providers and Clients filters.
type facetsView struct {
	Range   rangeView  `json:"range"`
	Models  []facetRow `json:"models"`
	Vendors []facetRow `json:"vendors"`
	// Clients holds only the clients ParseClientInfo recognizes, so unlike the
	// other two lists it is not exhaustive — see store.Facets.
	Clients []facetRow `json:"clients"`
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
	Kind                string  `json:"kind"` // "session" | "request"
	SessionID           string  `json:"session_id,omitempty"`
	Title               string  `json:"title,omitempty"`
	RequestID           string  `json:"request_id,omitempty"`
	Calls               int     `json:"calls"`
	Cost                float64 `json:"cost"`
	InputTokens         float64 `json:"input_tokens"`
	OutputTokens        float64 `json:"output_tokens"`
	CachedTokens        float64 `json:"cache_read_input_tokens"`
	CacheCreationTokens float64 `json:"cache_creation_input_tokens"`
	// Non-token metered units: seconds (ASR audio duration), chars (TTS text).
	Seconds    float64  `json:"seconds"`
	Chars      float64  `json:"chars"`
	ToolCalls  int      `json:"tool_calls"`
	ToolTokens float64  `json:"tool_tokens"`
	FirstTS    string   `json:"first_ts"`
	LastTS     string   `json:"last_ts"`
	DurationMS int64    `json:"duration_ms"`
	ErrorCount int      `json:"error_count"`
	MajorModel string   `json:"major_model,omitempty"`
	Models     []string `json:"models"`
	Vendors    []string `json:"vendors"`
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
		Kind:                r.Kind,
		SessionID:           r.SessionID,
		Title:               r.Title,
		RequestID:           r.RequestID,
		Calls:               r.Calls,
		Cost:                r.Cost,
		InputTokens:         r.InputTokens,
		OutputTokens:        r.OutputTokens,
		CachedTokens:        r.CachedTokens,
		CacheCreationTokens: r.CacheCreationTokens,
		Seconds:             r.Seconds,
		Chars:               r.Chars,
		ToolCalls:           r.ToolCalls,
		ToolTokens:          r.ToolTokens,
		FirstTS:             r.FirstTS.UTC().Format(time.RFC3339),
		LastTS:              r.LastTS.UTC().Format(time.RFC3339),
		DurationMS:          r.DurationMS,
		ErrorCount:          r.ErrorCount,
		MajorModel:          r.MajorModel,
		Models:              models,
		Vendors:             vendors,
		Model:               r.Model,
		Vendor:              r.Vendor,
		Wire:                r.Wire,
		Confidence:          string(r.Confidence),
		Modality:            string(r.Modality),
		Status:              r.Status,
		LatencyMS:           r.LatencyMS,
		Stream:              r.Stream,
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
	AgentID             string          `json:"agent_id"`
	Calls               int             `json:"calls"`
	Cost                float64         `json:"cost"`
	InputTokens         float64         `json:"input_tokens"`
	OutputTokens        float64         `json:"output_tokens"`
	CachedTokens        float64         `json:"cache_read_input_tokens"`
	CacheCreationTokens float64         `json:"cache_creation_input_tokens"`
	ThinkingTokens      float64         `json:"thinking_tokens"`
	Children            []agentNodeView `json:"children"`
}

// sessionView is the GET /api/sessions/{id} response: session-level rollups, the
// agent tree, and the session's calls (oldest first).
type sessionView struct {
	SessionID           string          `json:"session_id"`
	Title               string          `json:"title,omitempty"`
	Calls               int             `json:"calls"`
	Cost                float64         `json:"cost"`
	InputTokens         float64         `json:"input_tokens"`
	OutputTokens        float64         `json:"output_tokens"`
	CachedTokens        float64         `json:"cache_read_input_tokens"`
	CacheCreationTokens float64         `json:"cache_creation_input_tokens"`
	ThinkingTokens      float64         `json:"thinking_tokens"`
	ErrorCount          int             `json:"error_count"`
	FirstTS             string          `json:"first_ts"`
	LastTS              string          `json:"last_ts"`
	Models              []string        `json:"models"`
	Vendors             []string        `json:"vendors"`
	Agents              []agentNodeView `json:"agents"`
	Entries             []entryView     `json:"entries"`
}

// credentialView is a credential with its key masked. The raw key is NEVER
// included.
type credentialView struct {
	ID        string `json:"id"`
	MaskedKey string `json:"masked_key"`
}

// priceView is a single model price. Source carries the rate's provenance so a
// borrowed ("fallback:<model>") rate is never read as a published one.
type priceView struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Unit   string  `json:"unit"`
	Source string  `json:"source"`
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
//
// Note that Stats and Routing answer different questions and can legitimately
// disagree. Stats is derived from the ledger and is historical ("this vendor
// has served N requests, E of them errors, ever"). Routing is the router's live
// in-memory state and is predictive ("this vendor is demoted, so the next
// request will go elsewhere"). A vendor with one error months ago reads
// stats.healthy=false while routing has it perfectly live.
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
	Routing      *routingStateView    `json:"routing,omitempty"`
	Capacity     capacityView         `json:"capacity"`
}

// routingStateView is the live routing state of one vendor: what the router
// will do with the NEXT request. Absent when the router has no opinion yet
// (a vendor nothing has been reported on is live by default).
type routingStateView struct {
	Cooling      bool       `json:"cooling"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`
	// Dead means the vendor failed consistently enough to be presumed broken.
	// Unlike Cooling it does not lapse on a timer — while any healthy vendor
	// exists this one will not be tried again, so it clears only on a success or
	// when an operator disables the provider. This is the state worth alerting
	// on.
	Dead        bool `json:"dead"`
	ConsecFails int  `json:"consec_fails"`
	Demotions   int  `json:"demotions"`
	// Sessions is how many live agent sessions are pinned to this vendor for
	// prompt-cache locality.
	Sessions int `json:"sessions"`
}

// capacityView is this vendor's provider-concurrency occupancy. It is keyed by
// credential upstream, so vendors sharing one provider key report identical
// numbers — that is the point: they share the quota.
//
// Distinct from routing state. Capacity does not steer WHERE a request goes; it
// decides WHEN. A request to a full provider queues rather than being sent
// somewhere else, so the session keeps its prompt cache.
type capacityView struct {
	// Limit is the configured ceiling; 0 means unlimited.
	Limit int `json:"limit"`
	// InFlight is how many requests are being served right now.
	InFlight int `json:"in_flight"`
	// Waiting is how many are queued for a slot. Persistently non-zero means
	// the limit is below what the provider actually allows.
	Waiting int `json:"waiting"`
}

// newVendorView builds a vendor view from config plus computed stats. The raw
// api_key is intentionally dropped; only a masked preview is emitted. rs is the
// vendor's live routing state, or nil when the router has no entry for it.
func newVendorView(v config.Vendor, stat store.VendorStat, hasStat bool, rs *router.VendorState, occ concurrency.State) vendorView {
	models := v.ServedModels
	if models == nil {
		models = []string{}
	}

	cred := credentialView{ID: v.Credential.ID, MaskedKey: maskKey(v.Credential.APIKey)}

	prices := make(map[string]priceView, len(v.Prices))
	for model, p := range v.Prices {
		prices[model] = priceView{Input: p.Input, Output: p.Output, Unit: p.Unit, Source: p.Source}
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

	capacity := capacityView{
		Limit:    v.MaxConcurrency,
		InFlight: occ.InFlight,
		Waiting:  occ.Waiting,
	}

	var routing *routingStateView
	if rs != nil {
		routing = &routingStateView{
			Cooling:     rs.Cooling,
			Dead:        rs.Dead,
			ConsecFails: rs.ConsecFails,
			Demotions:   rs.Demotions,
			Sessions:    rs.Sessions,
		}
		if rs.Cooling {
			until := rs.CoolingUntil
			routing.CoolingUntil = &until
		}
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
		Routing:      routing,
		Capacity:     capacity,
	}
}

// testVendorView is the POST /api/vendors/{name}/test response.
type testVendorView struct {
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// testProxyView is the POST /api/proxies/{id}/test response. Target names the
// origin that was dialled through the proxy, which varies with what the proxy
// is assigned to — the operator needs it to read the result.
type testProxyView struct {
	Reachable bool   `json:"reachable"`
	Target    string `json:"target"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// settingsView is the GET /api/settings response. It never exposes the admin
// key.
type settingsView struct {
	Listen         string      `json:"listen"`
	DBPath         string      `json:"db_path"`
	AdminProtected bool        `json:"admin_protected"`
	Version        string      `json:"version"`
	Ledger         *ledgerView `json:"ledger,omitempty"`
}

// ledgerView is the ledger write queue's occupancy — the gateway's clearest
// load signal, since every proxied call passes through it.
//
// Depth is live and should read ~0: the writer drains far faster than a gateway
// proxying (inherently slow) LLM calls can fill it. HighWater is what reveals a
// burst that has already drained. Blocked is the one that matters — the queue
// never discards a record, so when it is full a request WAITS, and a non-zero
// count here is the only visible sign that the database could not keep up.
type ledgerView struct {
	Capacity  int   `json:"capacity"`
	Depth     int   `json:"depth"`
	HighWater int   `json:"high_water"`
	Written   int64 `json:"written"`
	Failed    int64 `json:"failed"`
	Blocked   int64 `json:"blocked"`
	BlockedMS int64 `json:"blocked_ms"`
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
	CallID     string        `json:"call_id"`
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
	Source string  `json:"source"`
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
	displayBody := body
	if decoded, ok := decodeTraceBody(body, headers["Content-Encoding"]); ok {
		displayBody = decoded
	}
	side := traceSideView{
		Headers:     headers,
		ContentType: contentType,
	}
	if utf8.Valid(displayBody) {
		side.Body = string(displayBody)
	} else {
		side.Body = base64.StdEncoding.EncodeToString(displayBody)
		side.BodyBase64 = true
	}
	return side
}

func decodeTraceBody(body []byte, contentEncoding string) ([]byte, bool) {
	decoded, ok, err := bodycodec.Decode(body, contentEncoding)
	if err != nil {
		return nil, false
	}
	return decoded, ok
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

// contextDistributionView is the same source tree without a global time range,
// used for session/request-local context distribution cards.
type contextDistributionView struct {
	Requests int                `json:"requests"`
	AvgTotal float64            `json:"avg_total"`
	Sources  []compose.Source   `json:"sources"`
	Blocks   []contextBlockView `json:"blocks,omitempty"`
}

// contextBlockView is an aggregate of itemized locally counted composition
// blocks across request windows. Tokens is the average per occurrence; Total is
// the exact sum and is what percentage calculations should use.
type contextBlockView struct {
	Source      string `json:"source"`
	Producer    string `json:"producer,omitempty"`
	Type        string `json:"type"`
	Hash        string `json:"hash"`
	Tokens      int64  `json:"tokens"`
	Cached      int64  `json:"cached"`
	Occurrences int    `json:"occurrences"`
	Total       int64  `json:"total"`
}

// sessionContextView is the GET /api/sessions/{id}/context response: per-turn
// composition, the session's request-weighted aggregate distribution, the
// latest turn's full snapshot (with producers), and a dwell list (empty until
// lineage tracking lands).
//
// The view is scoped to a single agent (the ?agent query param; default is the
// main loop). Turns, Distribution, and Snapshot cover only the selected agent's
// calls; Agents lists every selectable scope so the client can render a picker.
type sessionContextView struct {
	SessionID    string                  `json:"session_id"`
	Title        string                  `json:"title,omitempty"`
	Agent        string                  `json:"agent"`
	Agents       []agentScopeView        `json:"agents"`
	Turns        []contextTurnView       `json:"turns"`
	Distribution contextDistributionView `json:"distribution"`
	Snapshot     []compose.Source        `json:"snapshot"`
	Dwell        []dwellBlockView        `json:"dwell"`
}

// agentScopeView is one selectable agent in a session's context charts. AgentID
// is "" for the main loop; sub-agents carry a non-empty id and a positional
// label ("Sub-agent N") since the ledger holds no semantic sub-agent name.
type agentScopeView struct {
	AgentID string `json:"agent_id"`
	Label   string `json:"label"`
	Turns   int    `json:"turns"`
}

// contextTurnView is one turn's composition. Sources maps top-level source key
// to tokens only (producers and cached are dropped from this map).
type contextTurnView struct {
	CallID  string           `json:"call_id"`
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

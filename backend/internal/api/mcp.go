package api

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/songguo/songguo/internal/store"
)

// NewMCPHandler builds an MCP server that exposes Songguo's control plane (the
// same surface as the /api REST endpoints) as tools, served over stateless
// streamable HTTP and gated by the same admin bearer key as the REST API.
//
// Read tools are always registered. Write tools (create/update/delete) are
// registered only when enableWrites is true (SONGGUO_MCP_WRITE=1), because the
// admin key already grants full control over budgets and upstream credentials —
// an agent should not get write access implicitly.
func NewMCPHandler(d Deps, enableWrites bool) http.Handler {
	a := newAPI(d)
	srv := a.buildMCPServer(enableWrites)
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
	return a.authMiddleware(streamable)
}

// buildMCPServer registers the tool catalogue on a fresh server. Reuses the same
// transport-free *Data methods as the REST handlers, so behavior never drifts.
func (a *api) buildMCPServer(enableWrites bool) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "songguo", Version: a.version}, nil)

	// --- read tools (always on) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_overview",
		Description: "Spend summary for a time window: total spend, spend by modality, request/error counts, error rate, latency percentiles, active providers/users, daily burn and runway-in-days. Defaults to the last 30 days.",
	}, a.mcpGetOverview)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_usage_series",
		Description: "Cost, request and error totals bucketed over time, for plotting spend trends. Window defaults to the last 7 days; bucket is 'hour' or 'day' (omit to auto-select by range).",
	}, a.mcpGetUsageSeries)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_calls",
		Description: "Browse individual gateway calls (the per-request ledger), newest first, with optional filters by user, model, vendor, HTTP status and time window. Returns entries plus the total count for the filter. Use the returned id with get_call_trace.",
	}, a.mcpListCalls)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_call_trace",
		Description: "Return the captured request/response payload for one call id (only available when capture is enabled for that call). Returns not found when no payload was stored.",
	}, a.mcpGetCallTrace)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_users",
		Description: "List all gateway users (consumer keys) with their budget, scope, RPM limit, lifetime spend and active state. Plaintext keys are never returned.",
	}, a.mcpListUsers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_providers",
		Description: "List all configured upstream providers (one credential each) with their wire endpoints, models/prices, quirks and health stats. API keys are masked.",
	}, a.mcpListProviders)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_services",
		Description: "List the auto-derived, model-centric services: each unique model name served by one or more providers, with the providers behind it and aggregate call stats.",
	}, a.mcpListServices)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_pricing",
		Description: "List every per-provider model price (input, output, unit) currently configured.",
	}, a.mcpListPricing)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_settings",
		Description: "Return non-secret runtime settings: listen address, db path, whether the admin API is protected, version, and payload-capture configuration.",
	}, a.mcpGetSettings)

	if !enableWrites {
		return srv
	}

	// --- write tools (SONGGUO_MCP_WRITE=1 only) ---
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_user",
		Description: "Create a gateway user (consumer key). Returns the user including the plaintext key — shown only once. Fields: name (required), budget (USD, optional), scope (allowed models, optional), rpm (per-minute limit, optional), capture (per-user payload capture override, optional).",
	}, a.mcpCreateUser)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_user",
		Description: "Update a user's mutable fields. Provide the user id and a patch object with only the fields to change (name, budget, scope, rpm, capture).",
	}, a.mcpUpdateUser)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "revoke_user",
		Description: "Revoke a user by id, immediately disabling its key. Returns the updated user.",
	}, a.mcpRevokeUser)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_provider",
		Description: "Create an upstream provider. Fields: name (required), vendor, api_key, priority, weight, enabled, allow_unmatched, quirks, models (name + input/output/cached_input prices + unit), and endpoints (each a wire + its full upstream URL + adapter/auth scheme).",
	}, a.mcpCreateProvider)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_provider",
		Description: "Update a provider's mutable fields. Provide the provider id and a patch object with only the fields to change. Supplying api_key replaces the key; supplying models or endpoints replaces those lists wholesale.",
	}, a.mcpUpdateProvider)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_provider",
		Description: "Delete a provider by id. This removes its credential and endpoints; services it backed are re-derived without it.",
	}, a.mcpDeleteProvider)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "update_settings",
		Description: "Update gateway capture settings: capture (on/off), capture_max_bytes, capture_retain (count). Only provided fields change. Returns the resulting settings.",
	}, a.mcpUpdateSettings)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "test_provider",
		Description: "Probe a provider's host for reachability using its API key. Returns reachability, HTTP status and latency; never throws on an unreachable host.",
	}, a.mcpTestProvider)

	return srv
}

// --- read tool args + handlers ---

type noArgs struct{}

type overviewArgs struct {
	Since *int64 `json:"since,omitempty" jsonschema:"start of the window, unix seconds (default: 30 days ago)"`
	Until *int64 `json:"until,omitempty" jsonschema:"end of the window, unix seconds (default: now)"`
}

func (a *api) mcpGetOverview(_ context.Context, _ *mcp.CallToolRequest, args overviewArgs) (*mcp.CallToolResult, overviewView, error) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -30)
	until := now
	if args.Since != nil {
		since = time.Unix(*args.Since, 0).UTC()
	}
	if args.Until != nil {
		until = time.Unix(*args.Until, 0).UTC()
	}
	v, err := a.overviewData(since, until)
	if err != nil {
		return nil, overviewView{}, err
	}
	return nil, v, nil
}

type usageSeriesArgs struct {
	Since  *int64 `json:"since,omitempty" jsonschema:"start of the window, unix seconds (default: 7 days ago)"`
	Until  *int64 `json:"until,omitempty" jsonschema:"end of the window, unix seconds (default: now)"`
	Bucket string `json:"bucket,omitempty" jsonschema:"bucket size: 'hour' or 'day' (default: auto by range)"`
}

func (a *api) mcpGetUsageSeries(_ context.Context, _ *mcp.CallToolRequest, args usageSeriesArgs) (*mcp.CallToolResult, usageSeriesView, error) {
	now := a.now().UTC()
	since := now.AddDate(0, 0, -7)
	until := now
	if args.Since != nil {
		since = time.Unix(*args.Since, 0).UTC()
	}
	if args.Until != nil {
		until = time.Unix(*args.Until, 0).UTC()
	}
	v, err := a.usageSeriesData(since, until, args.Bucket)
	if err != nil {
		return nil, usageSeriesView{}, err
	}
	return nil, v, nil
}

type listCallsArgs struct {
	UserID string `json:"user_id,omitempty" jsonschema:"filter by user id"`
	Model  string `json:"model,omitempty" jsonschema:"filter by model string"`
	Vendor string `json:"vendor,omitempty" jsonschema:"filter by vendor/provider name"`
	Status *int   `json:"status,omitempty" jsonschema:"filter by HTTP status code, e.g. 200 or 429"`
	Since  *int64 `json:"since,omitempty" jsonschema:"only calls at/after this unix-seconds time"`
	Until  *int64 `json:"until,omitempty" jsonschema:"only calls before this unix-seconds time"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max rows (default 50, max 500)"`
	Offset int    `json:"offset,omitempty" jsonschema:"pagination offset"`
}

func (a *api) mcpListCalls(_ context.Context, _ *mcp.CallToolRequest, args listCallsArgs) (*mcp.CallToolResult, callsView, error) {
	f := store.CallFilter{
		UserID: args.UserID,
		Model:  args.Model,
		Vendor: args.Vendor,
		Status: args.Status,
		Limit:  args.Limit,
		Offset: args.Offset,
	}
	if args.Since != nil {
		t := time.Unix(*args.Since, 0).UTC()
		f.Since = &t
	}
	if args.Until != nil {
		t := time.Unix(*args.Until, 0).UTC()
		f.Until = &t
	}
	v, err := a.callsData(f)
	if err != nil {
		return nil, callsView{}, err
	}
	return nil, v, nil
}

type getCallTraceArgs struct {
	ID int64 `json:"id" jsonschema:"the call id (from list_calls)"`
}

func (a *api) mcpGetCallTrace(_ context.Context, _ *mcp.CallToolRequest, args getCallTraceArgs) (*mcp.CallToolResult, traceView, error) {
	v, err := a.callTraceData(args.ID)
	if err != nil {
		return nil, traceView{}, err
	}
	return nil, v, nil
}

type usersOut struct {
	Users []userView `json:"users"`
}

func (a *api) mcpListUsers(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, usersOut, error) {
	v, err := a.usersData()
	if err != nil {
		return nil, usersOut{}, err
	}
	return nil, usersOut{Users: v}, nil
}

type providersOut struct {
	Providers []providerView `json:"providers"`
}

func (a *api) mcpListProviders(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, providersOut, error) {
	v, err := a.providersData()
	if err != nil {
		return nil, providersOut{}, err
	}
	return nil, providersOut{Providers: v}, nil
}

type servicesOut struct {
	Services []serviceView `json:"services"`
}

func (a *api) mcpListServices(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, servicesOut, error) {
	v, err := a.servicesData()
	if err != nil {
		return nil, servicesOut{}, err
	}
	return nil, servicesOut{Services: v}, nil
}

type pricingOut struct {
	Pricing []pricingRow `json:"pricing"`
}

func (a *api) mcpListPricing(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, pricingOut, error) {
	return nil, pricingOut{Pricing: a.pricingData()}, nil
}

func (a *api) mcpGetSettings(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, settingsView, error) {
	return nil, a.settingsData(), nil
}

// --- write tool args + handlers ---

func (a *api) mcpCreateUser(_ context.Context, _ *mcp.CallToolRequest, args createUserReq) (*mcp.CallToolResult, userView, error) {
	v, err := a.createUserData(args)
	if err != nil {
		return nil, userView{}, err
	}
	return nil, v, nil
}

type updateUserArgs struct {
	ID    string       `json:"id" jsonschema:"the user id to update"`
	Patch patchUserReq `json:"patch" jsonschema:"fields to change; omit a field to leave it unchanged"`
}

func (a *api) mcpUpdateUser(_ context.Context, _ *mcp.CallToolRequest, args updateUserArgs) (*mcp.CallToolResult, userView, error) {
	v, err := a.updateUserData(args.ID, args.Patch)
	if err != nil {
		return nil, userView{}, err
	}
	return nil, v, nil
}

type idArgs struct {
	ID string `json:"id" jsonschema:"the resource id"`
}

func (a *api) mcpRevokeUser(_ context.Context, _ *mcp.CallToolRequest, args idArgs) (*mcp.CallToolResult, userView, error) {
	v, err := a.revokeUserData(args.ID)
	if err != nil {
		return nil, userView{}, err
	}
	return nil, v, nil
}

func (a *api) mcpCreateProvider(_ context.Context, _ *mcp.CallToolRequest, args createProviderReq) (*mcp.CallToolResult, providerView, error) {
	v, err := a.createProviderData(args)
	if err != nil {
		return nil, providerView{}, err
	}
	return nil, v, nil
}

type updateProviderArgs struct {
	ID    string          `json:"id" jsonschema:"the provider id to update"`
	Patch patchProviderReq `json:"patch" jsonschema:"fields to change; omit a field to leave it unchanged"`
}

func (a *api) mcpUpdateProvider(_ context.Context, _ *mcp.CallToolRequest, args updateProviderArgs) (*mcp.CallToolResult, providerView, error) {
	v, err := a.updateProviderData(args.ID, args.Patch)
	if err != nil {
		return nil, providerView{}, err
	}
	return nil, v, nil
}

type deletedOut struct {
	Deleted bool `json:"deleted"`
}

func (a *api) mcpDeleteProvider(_ context.Context, _ *mcp.CallToolRequest, args idArgs) (*mcp.CallToolResult, deletedOut, error) {
	if err := a.deleteProviderData(args.ID); err != nil {
		return nil, deletedOut{}, err
	}
	return nil, deletedOut{Deleted: true}, nil
}

func (a *api) mcpUpdateSettings(_ context.Context, _ *mcp.CallToolRequest, args patchSettingsReq) (*mcp.CallToolResult, settingsView, error) {
	v, err := a.updateSettingsData(args)
	if err != nil {
		return nil, settingsView{}, err
	}
	return nil, v, nil
}

func (a *api) mcpTestProvider(ctx context.Context, _ *mcp.CallToolRequest, args idArgs) (*mcp.CallToolResult, testVendorView, error) {
	v, err := a.testProviderData(ctx, args.ID)
	if err != nil {
		return nil, testVendorView{}, err
	}
	return nil, v, nil
}

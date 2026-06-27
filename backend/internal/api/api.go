// Package api implements the admin/dashboard HTTP API.
//
// It exposes a read-mostly JSON API under /api consumed by the React
// dashboard: usage overview, call browsing/export, token CRUD, vendor
// inspection/health, settings, and pricing. Every route is gated by a single
// admin bearer key compared in constant time. Vendor API keys are never
// serialized — only masked previews are returned.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/songguo/songguo/internal/config"
	"github.com/songguo/songguo/internal/store"
)

// Deps are the collaborators the admin API handler needs.
type Deps struct {
	Store      *store.Store
	Snapshot   func() *config.Snapshot
	Reload     func() error // rebuild the live snapshot after a config write
	AdminKey   string       // from SONGGUO_ADMIN_KEY; empty = unprotected (logged once)
	Logger     *slog.Logger
	HTTPClient *http.Client     // for vendor test-connection; default if nil
	Now        func() time.Time // defaults to time.Now
	Version    string           // build version string, default "dev"
	ListenAddr string           // from SONGGUO_LISTEN; shown in settings
	DBPath     string
}

// api is the concrete handler holding resolved dependencies.
type api struct {
	store      *store.Store
	snapshot   func() *config.Snapshot
	reload     func() error
	adminKey   string
	logger     *slog.Logger
	client     *http.Client
	now        func() time.Time
	version    string
	listenAddr string
	dbPath     string

	warnOnce sync.Once
}

// newAPI resolves Deps into a concrete *api with defaults applied. It is shared
// by NewHandler (REST) and NewMCPHandler (MCP) so both expose identical behavior
// over the same store/snapshot.
func newAPI(d Deps) *api {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	version := d.Version
	if version == "" {
		version = "dev"
	}
	reload := d.Reload
	if reload == nil {
		reload = func() error { return nil }
	}

	return &api{
		store:      d.Store,
		snapshot:   d.Snapshot,
		reload:     reload,
		adminKey:   d.AdminKey,
		logger:     logger,
		client:     client,
		now:        now,
		version:    version,
		listenAddr: d.ListenAddr,
		dbPath:     d.DBPath,
	}
}

// adminRoute is one admin API route: an HTTP method, a Go-mux path pattern and
// its handler (as a method expression bound at registration). This table is the
// single source of truth for both route registration and the OpenAPI drift test.
type adminRoute struct {
	Method  string
	Pattern string
	Handler func(*api, http.ResponseWriter, *http.Request)
}

var adminRoutes = []adminRoute{
	{"GET", "/api/overview", (*api).handleOverview},
	{"GET", "/api/usage/series", (*api).handleUsageSeries},
	{"GET", "/api/calls", (*api).handleCalls},
	{"GET", "/api/calls/export", (*api).handleCallsExport},
	{"GET", "/api/calls/{id}/trace", (*api).handleCallTrace},
	{"GET", "/api/users", (*api).handleListUsers},
	{"POST", "/api/users", (*api).handleCreateUser},
	{"PATCH", "/api/users/{id}", (*api).handlePatchUser},
	{"DELETE", "/api/users/{id}", (*api).handleDeleteUser},
	{"POST", "/api/users/{id}/revoke", (*api).handleRevokeUser},
	{"GET", "/api/vendors", (*api).handleListVendors},
	{"POST", "/api/vendors/{name}/test", (*api).handleTestVendor},
	// Services: auto-derived, model-centric view (read-only).
	{"GET", "/api/services", (*api).handleListServices},
	// Providers: SQLite-backed upstream config, managed from the dashboard.
	{"GET", "/api/providers", (*api).handleListProviders},
	{"POST", "/api/providers", (*api).handleCreateProvider},
	{"GET", "/api/providers/{id}", (*api).handleGetProvider},
	{"PATCH", "/api/providers/{id}", (*api).handlePatchProvider},
	{"DELETE", "/api/providers/{id}", (*api).handleDeleteProvider},
	{"POST", "/api/providers/{id}/test", (*api).handleTestProvider},
	{"GET", "/api/catalog", (*api).handleCatalog},
	{"GET", "/api/wires", (*api).handleWires},
	{"GET", "/api/settings", (*api).handleSettings},
	{"PATCH", "/api/settings", (*api).handlePatchSettings},
	{"GET", "/api/pricing", (*api).handlePricing},
}

// NewHandler builds the admin API as an http.Handler. Routes are registered from
// adminRoutes on an internal ServeMux using Go 1.22 method+path patterns and
// wrapped in the admin-auth middleware.
func NewHandler(d Deps) http.Handler {
	a := newAPI(d)

	mux := http.NewServeMux()
	for _, rt := range adminRoutes {
		h := rt.Handler
		mux.HandleFunc(rt.Method+" "+rt.Pattern, func(w http.ResponseWriter, r *http.Request) {
			h(a, w, r)
		})
	}

	return a.authMiddleware(mux)
}

// authMiddleware enforces the admin bearer key. When AdminKey is empty the API
// runs unprotected (the server already warned at startup) and all requests are
// allowed.
func (a *api) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.adminKey == "" {
			a.warnOnce.Do(func() {
				a.logger.Warn("admin API is UNPROTECTED (SONGGUO_ADMIN_KEY is empty)")
			})
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(key), []byte(a.adminKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid admin key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- JSON + error helpers ---

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorBody is the JSON error envelope, matching the proxy's shape.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// writeError writes a JSON error with a songguo_-prefixed type.
func writeError(w http.ResponseWriter, status int, reason, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{
		Message: message,
		Type:    "songguo_" + reason,
	}})
}

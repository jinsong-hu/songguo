// Package api implements the admin/dashboard HTTP API.
//
// It exposes a read-mostly JSON API under /api consumed by the React
// dashboard: usage overview, call browsing/export, token CRUD, vendor
// inspection/health, settings, and pricing. Every route is gated by a single
// admin bearer key compared in constant time. Vendor API keys are never
// serialized — only masked previews are returned.
package api

import (
	"context"
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
//
// Shared routes also accept a valid (non-revoked) consumer key, not just the
// admin key — the minimum read-only slice the non-admin playground needs to
// discover what a key may play. Every other route stays admin-only.
type adminRoute struct {
	Method  string
	Pattern string
	Handler func(*api, http.ResponseWriter, *http.Request)
	Shared  bool
}

var adminRoutes = []adminRoute{
	// Bootstrap/whoami — the SPA calls this to detect the key's role and, for a
	// user key, the models it may play. Accepts either key type.
	{"GET", "/api/me", (*api).handleMe, true},
	{"GET", "/api/overview", (*api).handleOverview, false},
	{"GET", "/api/usage/series", (*api).handleUsageSeries, false},
	{"GET", "/api/usage/tokens-by-model", (*api).handleTokensByModel, false},
	{"GET", "/api/usage/success-by-model", (*api).handleSuccessByModel, false},
	{"GET", "/api/usage/cache-by-model", (*api).handleCacheByModel, false},
	{"GET", "/api/usage/breakdown", (*api).handleBreakdown, false},
	{"GET", "/api/usage/errors", (*api).handleErrors, false},
	{"GET", "/api/usage/error-codes", (*api).handleTopErrorCodes, false},
	{"GET", "/api/context/composition", (*api).handleContextComposition, false},
	{"GET", "/api/feed", (*api).handleFeed, false},
	{"GET", "/api/calls", (*api).handleCalls, false},
	{"GET", "/api/calls/export", (*api).handleCallsExport, false},
	{"GET", "/api/calls/{id}", (*api).handleCall, false},
	{"GET", "/api/calls/{id}/trace", (*api).handleCallTrace, false},
	// Literal path — Go's mux prefers it over the {id} wildcard below.
	{"GET", "/api/sessions/overview", (*api).handleSessionsOverview, false},
	{"GET", "/api/sessions/{id}", (*api).handleSession, false},
	{"GET", "/api/sessions/{id}/messages", (*api).handleSessionMessages, false},
	{"GET", "/api/sessions/{id}/context", (*api).handleSessionContext, false},
	{"GET", "/api/users", (*api).handleListUsers, false},
	{"GET", "/api/users/{id}", (*api).handleGetUser, false},
	{"POST", "/api/users", (*api).handleCreateUser, false},
	{"PATCH", "/api/users/{id}", (*api).handlePatchUser, false},
	{"DELETE", "/api/users/{id}", (*api).handleDeleteUser, false},
	{"POST", "/api/users/{id}/revoke", (*api).handleRevokeUser, false},
	{"GET", "/api/vendors", (*api).handleListVendors, false},
	{"POST", "/api/vendors/{name}/test", (*api).handleTestVendor, false},
	// Services: auto-derived, model-centric view. Shared: scoped to the caller's
	// allowed models when a user key is used (see handleListServices).
	{"GET", "/api/services", (*api).handleListServices, true},
	// Providers: SQLite-backed upstream config. The list is Shared but sanitized
	// for a user key (no endpoint URLs or key previews — see handleListProviders);
	// every mutating/detail route below stays admin-only.
	{"GET", "/api/providers", (*api).handleListProviders, true},
	{"POST", "/api/providers", (*api).handleCreateProvider, false},
	{"GET", "/api/providers/{id}", (*api).handleGetProvider, false},
	{"PATCH", "/api/providers/{id}", (*api).handlePatchProvider, false},
	{"DELETE", "/api/providers/{id}", (*api).handleDeleteProvider, false},
	{"POST", "/api/providers/{id}/test", (*api).handleTestProvider, false},
	// Catalog + wire names are static model/wire metadata the test panel needs.
	{"GET", "/api/catalog", (*api).handleCatalog, true},
	{"GET", "/api/wires", (*api).handleWires, true},
	{"GET", "/api/settings", (*api).handleSettings, false},
	{"GET", "/api/pricing", (*api).handlePricing, false},
}

// NewHandler builds the admin API as an http.Handler. Routes are registered from
// adminRoutes on an internal ServeMux using Go 1.22 method+path patterns. Each
// route is wrapped in its own auth middleware: admin-only routes require the
// admin key; Shared routes also accept a valid consumer key.
func NewHandler(d Deps) http.Handler {
	a := newAPI(d)

	mux := http.NewServeMux()
	for _, rt := range adminRoutes {
		h := rt.Handler
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h(a, w, r)
		})
		if rt.Shared {
			mux.Handle(rt.Method+" "+rt.Pattern, a.sharedAuthMiddleware(handler))
		} else {
			mux.Handle(rt.Method+" "+rt.Pattern, a.authMiddleware(handler))
		}
	}

	return mux
}

// role identifies who authenticated on a request: the operator (admin key) or a
// consumer key holder (a users-table row).
type role string

const (
	roleAdmin role = "admin"
	roleUser  role = "user"
)

type ctxKey int

const (
	ctxRole ctxKey = iota
	ctxUser
)

// roleFrom returns the authenticated role stashed by an auth middleware. Absent
// (e.g. unprotected admin API) it defaults to admin, matching the effective
// privilege of an unauthenticated-but-allowed request.
func roleFrom(r *http.Request) role {
	if v, ok := r.Context().Value(ctxRole).(role); ok {
		return v
	}
	return roleAdmin
}

// userFrom returns the consumer user attached to a user-role request, if any.
func userFrom(r *http.Request) (store.User, bool) {
	u, ok := r.Context().Value(ctxUser).(store.User)
	return u, ok
}

// authMiddleware enforces the admin bearer key. When AdminKey is empty the API
// runs unprotected (the server already warned at startup) and all requests are
// allowed.
func (a *api) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.adminKey == "" {
			a.warnUnprotected()
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(key), []byte(a.adminKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid admin key")
			return
		}
		next.ServeHTTP(w, withRole(r, roleAdmin, store.User{}))
	})
}

// sharedAuthMiddleware guards routes usable by either the operator or a consumer
// key holder. The admin key authenticates as roleAdmin; any other bearer is
// looked up as a consumer key and, if it maps to an active user, authenticates
// as roleUser with that user attached. Unknown/revoked keys get 401. When the
// admin key is unset the whole admin API is unprotected, so we match that here.
func (a *api) sharedAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.adminKey == "" {
			a.warnUnprotected()
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(key), []byte(a.adminKey)) == 1 {
			next.ServeHTTP(w, withRole(r, roleAdmin, store.User{}))
			return
		}
		if key != "" && a.store != nil {
			if u, err := a.store.GetUserByKey(key); err == nil {
				next.ServeHTTP(w, withRole(r, roleUser, u))
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid key")
	})
}

// withRole returns r with the authenticated role (and user, for roleUser)
// stashed in its context.
func withRole(r *http.Request, ro role, u store.User) *http.Request {
	ctx := context.WithValue(r.Context(), ctxRole, ro)
	if ro == roleUser {
		ctx = context.WithValue(ctx, ctxUser, u)
	}
	return r.WithContext(ctx)
}

// warnUnprotected logs the unprotected-admin-API warning exactly once.
func (a *api) warnUnprotected() {
	a.warnOnce.Do(func() {
		a.logger.Warn("admin API is UNPROTECTED (SONGGUO_ADMIN_KEY is empty)")
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

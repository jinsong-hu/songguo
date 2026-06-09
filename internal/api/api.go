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
	AdminKey   string // from SONGGUO_ADMIN_KEY; empty = unprotected (logged once)
	Logger     *slog.Logger
	HTTPClient *http.Client     // for vendor test-connection; default if nil
	Now        func() time.Time // defaults to time.Now
	Version    string           // build version string, default "dev"
	ConfigPath string
	DBPath     string
}

// api is the concrete handler holding resolved dependencies.
type api struct {
	store      *store.Store
	snapshot   func() *config.Snapshot
	adminKey   string
	logger     *slog.Logger
	client     *http.Client
	now        func() time.Time
	version    string
	configPath string
	dbPath     string

	warnOnce sync.Once
}

// NewHandler builds the admin API as an http.Handler. Routes are registered on
// an internal ServeMux using Go 1.22 method+path patterns and wrapped in the
// admin-auth middleware.
func NewHandler(d Deps) http.Handler {
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

	a := &api{
		store:      d.Store,
		snapshot:   d.Snapshot,
		adminKey:   d.AdminKey,
		logger:     logger,
		client:     client,
		now:        now,
		version:    version,
		configPath: d.ConfigPath,
		dbPath:     d.DBPath,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", a.handleOverview)
	mux.HandleFunc("GET /api/usage/series", a.handleUsageSeries)
	mux.HandleFunc("GET /api/calls", a.handleCalls)
	mux.HandleFunc("GET /api/calls/export", a.handleCallsExport)
	mux.HandleFunc("GET /api/calls/{id}/trace", a.handleCallTrace)
	mux.HandleFunc("GET /api/tokens", a.handleListTokens)
	mux.HandleFunc("POST /api/tokens", a.handleCreateToken)
	mux.HandleFunc("PATCH /api/tokens/{id}", a.handlePatchToken)
	mux.HandleFunc("POST /api/tokens/{id}/revoke", a.handleRevokeToken)
	mux.HandleFunc("GET /api/vendors", a.handleListVendors)
	mux.HandleFunc("POST /api/vendors/{name}/test", a.handleTestVendor)
	mux.HandleFunc("GET /api/settings", a.handleSettings)
	mux.HandleFunc("GET /api/pricing", a.handlePricing)

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

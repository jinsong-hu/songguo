// Package server provides the HTTP server that fronts the gateway and admin API.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/songguo/songguo/web"
)

// Options configures a Server.
type Options struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// ProxyHandler, if non-nil, is the transparent proxy. It is mounted at the
	// native vendor path prefixes (/v1/ for OpenAI/Anthropic-shaped APIs, /api/v3/
	// for Volcengine speech); the provider is selected by header/model/default.
	// There is no /x/ passthrough mount — all addressing is native + explicit.
	ProxyHandler http.Handler
	// AdminHandler, if non-nil, is mounted under /api/ as the admin/dashboard API.
	AdminHandler http.Handler
	// MCPHandler, if non-nil, is mounted at /mcp as the agent-facing MCP server
	// over the same control plane as AdminHandler (admin-key gated).
	MCPHandler http.Handler
	// OpenAPIHandler, if non-nil, serves the admin API's OpenAPI spec at
	// /openapi.yaml and /openapi.json (unauthenticated; schema only).
	OpenAPIHandler http.Handler
}

// Server wraps an *http.Server and its route mux.
type Server struct {
	httpServer *http.Server
	mux        *http.ServeMux
	opts       Options
}

// New constructs a Server and registers its routes.
func New(cfg Options) *Server {
	mux := http.NewServeMux()
	s := &Server{
		mux:  mux,
		opts: cfg,
		httpServer: &http.Server{
			Addr:    cfg.Addr,
			Handler: mux,
		},
	}
	s.registerRoutes()
	return s
}

// registerRoutes wires up the HTTP routes.
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", handleHealthz)
	if s.opts.ProxyHandler != nil {
		// Consumers call native vendor paths directly: OpenAI/Anthropic-shaped
		// APIs under /v1/, Volcengine speech under /api/v3/. The proxy matches the
		// wire by path suffix and selects the provider by header/model/default.
		// /api/v3/ is more specific than the admin API's /api/ mount, so ServeMux
		// routes it here; new native prefixes are added as further mounts.
		s.mux.Handle("/v1/", s.opts.ProxyHandler)
		s.mux.Handle("/api/v3/", s.opts.ProxyHandler)
	}
	if s.opts.AdminHandler != nil {
		// The dashboard and CLI call the admin API under http://<songguo>/api.
		s.mux.Handle("/api/", s.opts.AdminHandler)
	}
	if s.opts.MCPHandler != nil {
		// Agents connect an MCP client to http://<songguo>/mcp (admin-key gated).
		s.mux.Handle("/mcp", s.opts.MCPHandler)
		s.mux.Handle("/mcp/", s.opts.MCPHandler)
	}
	if s.opts.OpenAPIHandler != nil {
		// The machine-readable admin-API contract, unauthenticated (schema only).
		s.mux.Handle("GET /openapi.yaml", s.opts.OpenAPIHandler)
		s.mux.Handle("GET /openapi.json", s.opts.OpenAPIHandler)
	}
	// Serve the embedded React dashboard at "/". The more specific /healthz,
	// /v1/, /api/v3/, and /api/ patterns registered above take precedence in
	// ServeMux, so this catch-all only handles dashboard assets and client-side
	// routes.
	if sub, err := web.FS(); err == nil {
		s.mux.Handle("/", spaHandler(sub))
	}
}

// spaHandler serves the single-page app from the embedded filesystem. If the
// requested path maps to an existing file it is served directly; otherwise
// (a client-side route, which has no file extension) it falls back to
// index.html with a 200 so the browser router can take over.
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if clean == "" || clean == "." {
			clean = "index.html"
		}
		if _, err := fs.Stat(fsys, clean); err != nil {
			// Unknown path: a deep client route or a 404. Serve the SPA shell so
			// the React router can render the right view (or its own 404).
			serveIndex(w, fsys)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// serveIndex writes the SPA entry document with a 200 status.
func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "dashboard not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// Start runs the HTTP server until it is shut down or fails.
func (s *Server) Start() error {
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

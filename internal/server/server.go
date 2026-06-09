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
	// ProxyHandler, if non-nil, is mounted under /v1/ as the transparent proxy.
	ProxyHandler http.Handler
	// AdminHandler, if non-nil, is mounted under /api/ as the admin/dashboard API.
	AdminHandler http.Handler
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
		// Consumers point their SDK base URL at http://<songguo>/v1.
		s.mux.Handle("/v1/", s.opts.ProxyHandler)
	}
	if s.opts.AdminHandler != nil {
		// The dashboard and CLI call the admin API under http://<songguo>/api.
		s.mux.Handle("/api/", s.opts.AdminHandler)
	}
	// Serve the embedded React dashboard at "/". The more specific /healthz,
	// /v1/, and /api/ patterns registered above take precedence in ServeMux, so
	// this catch-all only handles dashboard assets and client-side routes.
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

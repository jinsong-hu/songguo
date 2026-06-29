// Package server provides the HTTP server that fronts the gateway and admin API.
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

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
	// TestWSHandler, if non-nil, is the dashboard's browser-facing streaming test
	// driver, mounted at /api/test/ (more specific than the admin /api/ mount).
	TestWSHandler http.Handler
	// MCPHandler, if non-nil, is mounted at /mcp as the agent-facing MCP server
	// over the same control plane as AdminHandler (admin-key gated).
	MCPHandler http.Handler
	// OpenAPIHandler, if non-nil, serves the admin API's OpenAPI spec at
	// /openapi.yaml and /openapi.json (unauthenticated; schema only).
	OpenAPIHandler http.Handler
	// Logger, if non-nil, enables a per-request access log wrapping every route.
	Logger *slog.Logger
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
	var root http.Handler = mux
	if cfg.Logger != nil {
		root = accessLog(cfg.Logger, mux)
	}
	s := &Server{
		mux:  mux,
		opts: cfg,
		httpServer: &http.Server{
			Addr:    cfg.Addr,
			Handler: root,
		},
	}
	s.registerRoutes()
	return s
}

// accessLog wraps next with a per-request log line recording method, path,
// status, response bytes, and latency. It is the gateway's access log; the
// proxy adds upstream/vendor detail of its own on top of this.
func accessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		status := rec.status
		if rec.hijacked {
			// A hijacked connection (WebSocket upgrade) completes its 101
			// handshake on the raw conn, bypassing WriteHeader.
			status = http.StatusSwitchingProtocols
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
		)
	})
}

// recorder wraps an http.ResponseWriter to capture the status code and number
// of bytes written, while transparently forwarding the http.Flusher and
// http.Hijacker the proxy relies on for streaming and WebSocket relays.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
	hijacked    bool
}

func (rec *recorder) WriteHeader(code int) {
	if !rec.wroteHeader {
		rec.status = code
		rec.wroteHeader = true
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.wroteHeader = true
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

// Flush forwards to the underlying flusher so streamed (SSE) responses are not
// buffered by the access-log wrapper.
func (rec *recorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying hijacker so WebSocket upgrades still work.
func (rec *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	rec.hijacked = true
	return hj.Hijack()
}

// clientIP returns the caller's address for the access log, preferring the
// left-most X-Forwarded-For hop when present, else the raw remote address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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
	if s.opts.TestWSHandler != nil {
		// The dashboard's browser streaming-test driver. Mounted before the admin
		// /api/ catch-all (more specific prefix wins in ServeMux).
		s.mux.Handle("/api/test/", s.opts.TestWSHandler)
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

// Command songguo is the entrypoint for the Songguo AI usage gateway.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/songguo/songguo/internal/api"
	"github.com/songguo/songguo/internal/configsvc"
	"github.com/songguo/songguo/internal/janitor"
	"github.com/songguo/songguo/internal/proxy"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/server"
	"github.com/songguo/songguo/internal/store"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getdays reads an integer number of days from an env var, falling back to def
// (also in days) when unset or unparseable. A value of 0 disables that
// retention tier (the janitor treats a zero window as "never prune").
func getdays(key string, def int) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return time.Duration(def) * 24 * time.Hour
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	listen := getenv("SONGGUO_LISTEN", ":12345")
	dbPath := getenv("SONGGUO_DB", "./songguo.db")
	adminKey := os.Getenv("SONGGUO_ADMIN_KEY")

	if adminKey == "" {
		logger.Warn("SONGGUO_ADMIN_KEY is empty; the admin API will be UNPROTECTED")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		logger.Error("failed to open store", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Mirror the admin key as a consumer user so it authenticates proxied
	// service calls too (the dashboard playground signs in with the admin key).
	if err := st.EnsureAdminUser(adminKey); err != nil {
		logger.Error("failed to seed admin user", "err", err)
		os.Exit(1)
	}

	manager, err := configsvc.NewManager(st, logger)
	if err != nil {
		logger.Error("failed to build config", "err", err)
		os.Exit(1)
	}

	rt := router.New(manager.Current)
	proxyDeps := proxy.Deps{
		Snapshot: manager.Current,
		Store:    st,
		Router:   rt,
		Logger:   logger,
	}
	proxyHandler := proxy.NewHandler(proxyDeps)
	testWSHandler := proxy.NewWSTestHandler(proxyDeps)

	adminDeps := api.Deps{
		Store:      st,
		Snapshot:   manager.Current,
		Reload:     manager.Reload,
		AdminKey:   adminKey,
		Logger:     logger,
		Version:    "dev",
		ListenAddr: listen,
		DBPath:     dbPath,
	}
	adminHandler := api.NewHandler(adminDeps)

	// The MCP server exposes the same control plane as tools (admin-key gated).
	// Write tools are opt-in: only registered when SONGGUO_MCP_WRITE is truthy,
	// since the admin key already controls budgets and upstream credentials.
	mcpWrite := getenv("SONGGUO_MCP_WRITE", "") != ""
	mcpHandler := api.NewMCPHandler(adminDeps, mcpWrite)
	if mcpWrite {
		logger.Warn("MCP write tools are ENABLED (SONGGUO_MCP_WRITE is set)")
	}

	srv := server.New(server.Options{
		Addr:           listen,
		ProxyHandler:   proxyHandler,
		TestWSHandler:  testWSHandler,
		AdminHandler:   adminHandler,
		MCPHandler:     mcpHandler,
		OpenAPIHandler: api.NewOpenAPIHandler(),
		Logger:         logger,
	})

	// Retention janitor: prune derived/captured data on a fixed clock, off the
	// gateway hot path. Windows are days, overridable per tier; 0 disables a tier.
	// See docs/arch.md.
	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	defer stopJanitor()
	jan := janitor.New(st, logger, janitor.Windows{
		Raw:      getdays("SONGGUO_RETAIN_RAW_DAYS", 7),
		Calls:    getdays("SONGGUO_RETAIN_CALLS_DAYS", 90),
		Sessions: getdays("SONGGUO_RETAIN_SESSIONS_DAYS", 90),
	}, time.Hour)
	go jan.Run(janitorCtx)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("songguo listening", "addr", listen)
		errCh <- srv.Start()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
			os.Exit(1)
		}
	}
}

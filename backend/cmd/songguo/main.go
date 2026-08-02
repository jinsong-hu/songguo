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
	"github.com/songguo/songguo/internal/concurrency"
	"github.com/songguo/songguo/internal/configsvc"
	"github.com/songguo/songguo/internal/janitor"
	"github.com/songguo/songguo/internal/outbound"
	"github.com/songguo/songguo/internal/proxy"
	"github.com/songguo/songguo/internal/router"
	"github.com/songguo/songguo/internal/server"
	"github.com/songguo/songguo/internal/spend"
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

	// From here on a failure must NOT call os.Exit directly: that skips every
	// defer below, including the ledger drain, and losing the queued call
	// records is a worse outcome than the failure being reported a moment later.
	// Registered before st.Close so it runs last of all.
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()
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

	rt := router.New(manager.Current, router.Options{Logger: logger})
	// One gate shared by the HTTP and WebSocket paths, and read by the admin API
	// for live occupancy. It bounds in-flight requests per credential; the
	// router never sees it, because capacity decides WHEN a request goes, not
	// WHERE (see internal/concurrency).
	gate := concurrency.New()
	out := outbound.New(outbound.Options{})

	// Running per-user spend, shared so the admin API reports the same number
	// budget enforcement acts on. Flushed on a ticker by spendTracker.Run below.
	spendTracker := spend.New(st, logger)

	proxyDeps := proxy.Deps{
		Snapshot: manager.Current,
		Store:    st,
		Router:   rt,
		Gate:     gate,
		Spend:    spendTracker,
		Logger:   logger,
		Outbound: out,
	}
	proxyHandler := proxy.NewHandler(proxyDeps)
	testWSHandler := proxy.NewWSTestHandler(proxyDeps)

	adminDeps := api.Deps{
		Store:    st,
		Snapshot: manager.Current,
		Router:   rt,
		Gate:     gate,
		Spend:    spendTracker,
		// Ledger queue occupancy, so an operator can see backlog and whether
		// any request has ever had to wait on it.
		LedgerStats: proxyHandler.Stats,
		Reload: func() error {
			if err := manager.Reload(); err != nil {
				return err
			}
			out.Reset()
			// An operator edit is an explicit statement about a provider, so
			// clear routing health rather than let a vendor inherit a cooldown
			// it earned under different config. This also drops health entries
			// for vendors the reload removed.
			rt.ResetHealth()
			return nil
		},
		AdminKey:   adminKey,
		Logger:     logger,
		Outbound:   out,
		Version:    "dev",
		ListenAddr: listen,
		DBPath:     dbPath,
	}
	adminHandler := api.NewHandler(adminDeps)

	// The admin MCP server exposes the same control plane as tools (admin-key
	// gated), mounted at /admin/mcp. Write tools are opt-in: only registered when
	// SONGGUO_MCP_WRITE is truthy, since the admin key already controls budgets
	// and upstream credentials.
	mcpWrite := getenv("SONGGUO_MCP_WRITE", "") != ""
	adminMCPHandler := api.NewMCPHandler(adminDeps, mcpWrite)
	if mcpWrite {
		logger.Warn("MCP write tools are ENABLED (SONGGUO_MCP_WRITE is set)")
	}

	// The services MCP (bare /mcp, consumer-key gated) exposes capability tools
	// that originate native vendor calls through the proxy — routed and metered
	// against the caller's own key.
	servicesMCPHandler := api.NewServicesMCPHandler(adminDeps, proxyHandler)

	srv := server.New(server.Options{
		Addr:               listen,
		ProxyHandler:       proxyHandler,
		TestWSHandler:      testWSHandler,
		AdminHandler:       adminHandler,
		AdminMCPHandler:    adminMCPHandler,
		ServicesMCPHandler: servicesMCPHandler,
		OpenAPIHandler:     api.NewOpenAPIHandler(),
		Logger:             logger,
	})

	// Retention janitor: prune derived/captured data on a fixed clock, off the
	// gateway hot path. Windows are days, overridable per tier; 0 disables a tier.
	// See docs/arch.md.
	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	jan := janitor.New(st, logger, janitor.Windows{
		Raw:      getdays("SONGGUO_RETAIN_RAW_DAYS", 7),
		Calls:    getdays("SONGGUO_RETAIN_CALLS_DAYS", 90),
		Sessions: getdays("SONGGUO_RETAIN_SESSIONS_DAYS", 90),
	}, time.Hour)

	// Deferred calls run LIFO, so these are registered in reverse of the order
	// they must happen in. On the way out: cancel the janitor and the spend
	// flusher, wait for both to return, drain the proxy's background forks, and
	// only then does the `defer st.Close()` registered further up run.
	//
	// Draining the proxy is not optional. Its ledger queue is what makes the
	// request path non-blocking, and everything still in it at exit is a call
	// record — not a derived rollup — so skipping the drain loses ledger rows on
	// every restart.
	spendCtx, stopSpend := context.WithCancel(context.Background())
	defer proxyHandler.Close()
	defer spendTracker.Wait()
	defer jan.Wait()
	defer stopSpend()
	defer stopJanitor()
	go jan.Run(janitorCtx)
	go spendTracker.Run(spendCtx, 0)

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
			exitCode = 1
		}
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
			exitCode = 1
		}
	}
}

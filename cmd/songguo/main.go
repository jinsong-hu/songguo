// Command songguo is the entrypoint for the Songguo AI usage gateway.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/songguo/songguo/internal/api"
	"github.com/songguo/songguo/internal/config"
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

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	listen := getenv("SONGGUO_LISTEN", ":8080")
	configPath := getenv("SONGGUO_CONFIG", "./config.yaml")
	dbPath := getenv("SONGGUO_DB", "./songguo.db")
	adminKey := os.Getenv("SONGGUO_ADMIN_KEY")

	if adminKey == "" {
		logger.Warn("SONGGUO_ADMIN_KEY is empty; the admin API will be UNPROTECTED")
	}

	manager, err := config.NewManager(configPath, logger)
	if err != nil {
		logger.Error("failed to load config", "path", configPath, "err", err)
		os.Exit(1)
	}
	defer manager.Close()

	st, err := store.Open(dbPath)
	if err != nil {
		logger.Error("failed to open store", "path", dbPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()

	rt := router.New(manager.Current)
	proxyHandler := proxy.NewHandler(proxy.Deps{
		Snapshot: manager.Current,
		Store:    st,
		Router:   rt,
		Logger:   logger,
	})

	adminHandler := api.NewHandler(api.Deps{
		Store:      st,
		Snapshot:   manager.Current,
		AdminKey:   adminKey,
		Logger:     logger,
		Version:    "dev",
		ConfigPath: configPath,
		DBPath:     dbPath,
	})

	srv := server.New(server.Options{
		Addr:         listen,
		ProxyHandler: proxyHandler,
		AdminHandler: adminHandler,
	})

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

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/s3ntin3l8/go-http-template/internal/config"
	"github.com/s3ntin3l8/go-http-template/internal/httpapi"
)

// version is stamped at build time via -ldflags "-X main.version=...";
// defaults to "dev" for local builds (see Dockerfile).
var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	addr := flag.String("addr", "", "override listen address (default from config)")
	healthcheck := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit (for container HEALTHCHECK)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	listenAddr := cfg.ListenAddr
	if *addr != "" {
		listenAddr = *addr
	}
	cfg.ListenAddr = listenAddr

	if *healthcheck {
		os.Exit(runHealthcheck(listenAddr))
	}

	slog.Info("starting server", "version", version, "addr", listenAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := httpapi.New(cfg)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	// Shut the HTTP server down first, then close any other resources
	// (a DB pool, a worker pool, etc., if this template grows one) -- in
	// that order, so nothing still accepting requests outlives what it
	// depends on. ctx (the signal context) is already Done here, which is
	// what such background work should watch to know it's time to stop.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
		os.Exit(1)
	}

	slog.Info("server stopped")
}

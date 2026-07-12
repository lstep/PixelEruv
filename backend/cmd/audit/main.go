// audit is the standalone audit service. It subscribes to "audit.event" on
// NATS, persists events to its own SQLite database, and serves a web UI.
//
// Env vars:
//
//	NATS_URL           NATS connection URL (default: nats://localhost:4222)
//	AUDIT_DB           SQLite database path (default: ./audit.db)
//	AUDIT_HTTP_ADDR    HTTP listen address (default: :8082)
//	AUDIT_BASE_PATH    base path for URLs when proxied (e.g. /audit; default: empty)
//	AUDIT_AUTH_USER    basic auth username (if set with AUDIT_AUTH_PASS, enables auth)
//	AUDIT_AUTH_PASS    basic auth password
//	PUSHER_HEALTHZ     pusher /healthz URL for dashboard health cards
//	OTEL_BASE_URL      OpenObserve base URL for trace deep-links (e.g. http://localhost:5080)
//	AUDIT_RETENTION_HOURS  retention period in hours (default: 720 = 30 days)
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	dbPath := envOr("AUDIT_DB", "./audit.db")
	httpAddr := envOr("AUDIT_HTTP_ADDR", ":8082")
	healthzURL := os.Getenv("PUSHER_HEALTHZ")
	otelBaseURL := envOr("OTEL_BASE_URL", "http://localhost:5080")
	basePath := os.Getenv("AUDIT_BASE_PATH")
	authUser := os.Getenv("AUDIT_AUTH_USER")
	authPass := os.Getenv("AUDIT_AUTH_PASS")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		logger.Error("open store", "err", err, "db", dbPath)
		os.Exit(1)
	}
	defer store.Close()
	logger.Info("audit store ready", "db", dbPath)

	nc, err := nats.Connect(natsURL,
		nats.Name("audit"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	srv, err := NewServer(nc, store, logger, healthzURL, otelBaseURL, basePath, authUser, authPass)
	if err != nil {
		logger.Error("create server", "err", err)
		os.Exit(1)
	}

	if err := srv.Run(ctx, httpAddr); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

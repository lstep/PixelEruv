// admin is the admin portal service. It provides email/password login
// via PocketBase, a signed cookie session, an auth_request endpoint for
// nginx, and a landing page with links to admin services (PocketBase
// admin, audit UI).
//
// Env vars:
//
//	ADMIN_HTTP_ADDR       HTTP listen address (default: :8083)
//	ADMIN_SESSION_SECRET  HMAC-SHA256 signing key for session cookies (required)
//	PB_API_URL            PocketBase API URL (default: http://worldsim:8090/api)
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/lstep/pixeleruv/backend/internal/version"
)

func main() {
	addr := envOr("ADMIN_HTTP_ADDR", ":8083")
	cfg := Config{
		SessionSecret:   os.Getenv("ADMIN_SESSION_SECRET"),
		PBApiURL:        envOr("PB_API_URL", "http://worldsim:8090/api"),
		PBAdminEmail:    os.Getenv("PB_ADMIN_EMAIL"),
		PBAdminPassword: os.Getenv("PB_ADMIN_PASSWORD"),
		RecordingsDir:   envOr("RECORDINGS_DIR", "/recordings"),
		NATSURL:         envOr("NATS_URL", ""),
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if cfg.SessionSecret == "" {
		logger.Error("ADMIN_SESSION_SECRET is required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv, err := NewServer(cfg, logger)
	if err != nil {
		logger.Error("create server", "err", err)
		os.Exit(1)
	}

	logger.Info("admin service starting", "addr", addr, "version", version.Version)
	if err := srv.Run(ctx, addr); err != nil && err != http.ErrServerClosed {
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

// admin is the admin portal service. It provides OIDC login via Dex,
// a signed cookie session, an auth_request endpoint for nginx, and a
// landing page with links to admin services (PocketBase admin, audit UI).
//
// Env vars:
//
//	ADMIN_HTTP_ADDR       HTTP listen address (default: :8083)
//	ADMIN_SESSION_SECRET  HMAC-SHA256 signing key for session cookies (required)
//	DEX_ISSUER            Dex issuer URL (default: http://dex:5556/dex)
//	DEX_CLIENT_ID         Dex client ID (default: pixeleruv-admin)
//	DEX_REDIRECT_URL      OIDC redirect URL (default: https://localhost/admin/callback)
//	PB_API_URL            PocketBase API URL for is_admin check (default: http://worldsim:8090/api)
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
		SessionSecret:  os.Getenv("ADMIN_SESSION_SECRET"),
		DexIssuer:      envOr("DEX_ISSUER", "http://localhost:5556/dex"),
		DexInternalURL: envOr("DEX_INTERNAL_URL", "http://dex:5556/dex"),
		DexBrowserURL:  envOr("DEX_BROWSER_URL", "/dex"),
		DexClientID:    envOr("DEX_CLIENT_ID", "pixeleruv-admin"),
		DexRedirectURL: envOr("DEX_REDIRECT_URL", "https://localhost/admin/callback"),
		PBApiURL:       envOr("PB_API_URL", "http://worldsim:8090/api"),
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

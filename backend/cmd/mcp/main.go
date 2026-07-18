// mcp is the MCP (Model Context Protocol) server for PixelEruv internals.
//
// It exposes worldsim world state, audit history + live events, and
// PocketBase records to MCP clients (Claude Desktop, Devin, Cursor, etc.)
// over HTTP/SSE. Transport is HTTP/SSE only — the binary is designed to run
// as a Docker service, not as a stdio subprocess.
//
// Env vars:
//
//	NATS_URL           NATS connection URL (default: nats://localhost:4222)
//	MCP_HTTP_ADDR      HTTP listen address (default: :8085)
//	MCP_AUTH_TOKEN     Bearer token clients must present (REQUIRED; server
//	                   refuses all requests if unset). Treat as a secret.
//	MCP_ACTOR          actor.extension stamped on admin action audit events
//	                   (default: "mcp")
//	AUDIT_BASE_URL     Audit service base URL for historical queries
//	                   (e.g. http://audit:8082/audit). If empty, audit tools
//	                   return an error.
//	AUDIT_AUTH_USER    Optional basic auth user for audit HTTP API
//	AUDIT_AUTH_PASS    Optional basic auth password for audit HTTP API
//	PB_BASE_URL        PocketBase base URL (e.g. http://pocketbase:8090).
//	                   If empty, PB tools return an error.
//	PB_ADMIN_TOKEN     Optional PocketBase admin token (Authorization header).
//	                   If empty, requests are unauthenticated (works only if
//	                   PB has no API rules).
//
// Endpoints:
//
//	/mcp      SSE MCP endpoint (requires Bearer token)
//	/healthz  Unauthenticated health check (returns {"service":"mcp","status":"OK"})
//
// See documentation/plans/2026-07-19-mcp-server-design.md for the design.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	httpAddr := envOr("MCP_HTTP_ADDR", ":8085")
	token := os.Getenv("MCP_AUTH_TOKEN")
	actor := envOr("MCP_ACTOR", "mcp")
	auditBase := os.Getenv("AUDIT_BASE_URL")
	auditUser := os.Getenv("AUDIT_AUTH_USER")
	auditPass := os.Getenv("AUDIT_AUTH_PASS")
	pbBase := os.Getenv("PB_BASE_URL")
	pbToken := os.Getenv("PB_ADMIN_TOKEN")

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if token == "" {
		logger.Error("MCP_AUTH_TOKEN is required; refusing to start without auth")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	nc, err := nats.Connect(natsURL,
		nats.Name("mcp"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		logger.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer nc.Close()
	logger.Info("connected to NATS", "url", natsURL)

	deps := Deps{
		Worldsim: NewWorldsimClient(nc, actor),
		Audit:    NewAuditClient(auditBase, auditUser, auditPass, nc),
		PB:       NewPocketBaseClient(pbBase, pbToken),
	}

	if err := ServeHTTP(ctx, httpAddr, token, deps, logger); err != nil {
		logger.Error("mcp server stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

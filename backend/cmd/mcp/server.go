package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Deps bundles the external clients the MCP server talks to. Each is
// optional: if a client's base URL / connection is nil, the corresponding
// tools/resources return an error when called, but the server still serves
// the rest. This lets ops run the MCP server with only a subset of backends.
type Deps struct {
	Worldsim *WorldsimClient
	Audit    *AuditClient
	PB       *PocketBaseClient
	Docker   *DockerClient
}

// NewMCPServer builds the MCP server: registers all tools, resources, and
// prompts against the supplied deps. The returned *mcp.Server is ready to be
// handed to NewSSEHandler.
func NewMCPServer(deps Deps, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "pixeleruv-mcp",
		Version: "0.1.0",
	}, &mcp.ServerOptions{
		// Log messages get sent to MCP clients via notifications/message.
		// The SDK's default logger is fine; we route through slog.
		Logger: logger,
	})

	registerTools(s, deps.Worldsim, deps.Audit, deps.PB, deps.Docker)
	registerResources(s, deps.Worldsim, deps.Audit, deps.PB, deps.Docker)
	registerPrompts(s, deps.Worldsim, deps.Audit)

	return s
}

// ServeHTTP runs the MCP server over HTTP/SSE on addr. Bearer-token auth is
// required (token from MCP_AUTH_TOKEN env). The /mcp path serves the SSE
// endpoint; /healthz is an unauthenticated health check for Docker / nginx.
//
// If liveAudit is true and the audit client has NATS, we subscribe to
// audit.event and log each event (the SDK forwards log messages to clients
// that subscribed to notifications/message). This is a lightweight bridge —
// a future version can use ServerSession.SendNotification to push custom
// notifications.
func ServeHTTP(ctx context.Context, addr, token string, deps Deps, logger *slog.Logger) error {
	server := NewMCPServer(deps, logger)

	// Optional: subscribe to live audit events and log them so MCP clients
	// that subscribed to notifications/message see them.
	if deps.Audit != nil && deps.Audit.nc != nil {
		if err := deps.Audit.SubscribeLive(ctx, func(ev audit.Event) {
			logger.Info("audit.event",
				"type", ev.EventType,
				"severity", ev.Severity,
				"entity", ev.Actor.EntityID,
				"client", ev.Actor.ClientID,
				"trace_id", ev.TraceID)
		}); err != nil {
			logger.Warn("subscribe audit.event", "err", err)
		}
	}

	handler := mcp.NewSSEHandler(func(request *http.Request) *mcp.Server {
		// Single server for all requests; the SDK manages sessions per-SSE
		// connection. If you want per-tenant servers in the future, branch
		// on request.Host / Authorization here.
		return server
	}, &mcp.SSEOptions{
		// The MCP server runs inside Docker behind nginx. Disable the
		// localhost-rebinding guard so external clients can connect via the
		// published port. Auth is enforced by the bearer-token middleware
		// below instead.
		DisableLocalhostProtection: true,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"service":"mcp","status":"OK"}`))
	})
	mux.Handle("/mcp", bearerAuth(handler, token, logger))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("MCP server listening", "addr", addr, "path", "/mcp")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// bearerAuth wraps an http.Handler with a bearer-token check. The token is
// compared in constant time. /healthz is exempt (handled by the caller's mux
// before this wrapper sees it).
func bearerAuth(next http.Handler, token string, logger *slog.Logger) http.Handler {
	if token == "" {
		// No token configured: refuse all requests. The MCP server must not
		// expose admin actions (kick/ban/chat-as) without auth.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Error("MCP_AUTH_TOKEN not set; refusing request")
			http.Error(w, "MCP server auth not configured", http.StatusServiceUnavailable)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) || !constantTimeEqual(auth[len(prefix):], token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="PixelEruv MCP"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// constantTimeEqual returns true iff a == b in constant time.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

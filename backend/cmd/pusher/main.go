package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/pusher"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	wsAddr := envOr("WS_ADDR", ":8081")
	dexURL := envOr("DEX_URL", "")
	dexClientID := envOr("DEX_CLIENT_ID", "pixeleruv")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, shutdown, err := otel.Init(ctx, "pusher")
	if err != nil {
		log.Fatalf("otel init: %v", err)
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), otel.FlushTimeout)
		defer scancel()
		shutdown(sctx)
	}()

	srv, err := pusher.New(wsAddr, natsURL, dexURL, dexClientID, logger)
	if err != nil {
		log.Fatalf("pusher init: %v", err)
	}

	logger.Info("pusher listening", "ws_addr", wsAddr, "nats", natsURL)
	if err := srv.Run(ctx); err != nil {
		logger.Info("pusher stopped", "err", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

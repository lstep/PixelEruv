package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lstep/pixeleruv/backend/internal/pusher"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	wsAddr := envOr("WS_ADDR", ":8081")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv, err := pusher.New(wsAddr, natsURL)
	if err != nil {
		log.Fatalf("pusher init: %v", err)
	}

	log.Printf("pusher listening on %s (nats=%s)", wsAddr, natsURL)
	if err := srv.Run(ctx); err != nil {
		log.Printf("pusher stopped: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

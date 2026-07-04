package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/lstep/pixeleruv/backend/internal/worldsim"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	tickHz := envInt("TICK_HZ", 20)
	mapID := envOr("MAP_ID", "test-map")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sim, err := worldsim.New(natsURL, mapID, tickHz)
	if err != nil {
		log.Fatalf("worldsim init: %v", err)
	}

	log.Printf("worldsim starting (nats=%s, tick=%dHz, map=%s)", natsURL, tickHz, mapID)
	if err := sim.Run(ctx); err != nil {
		log.Printf("worldsim stopped: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

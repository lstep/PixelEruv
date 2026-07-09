package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/worldsim"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	tickHz := envInt("TICK_HZ", 20)
	mapID := envOr("MAP_ID", "map1")
	pocketbaseURL := envOr("POCKETBASE_URL", "http://localhost:8090")
	pbAdminEmail := envOr("PB_ADMIN_EMAIL", "admin@pixeleruv.local")
	pbAdminPassword := envOr("PB_ADMIN_PASSWORD", "password123")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger, shutdown, err := otel.Init(ctx, "worldsim")
	if err != nil {
		log.Fatalf("otel init: %v", err)
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), otel.FlushTimeout)
		defer scancel()
		shutdown(sctx)
	}()

	sim, err := worldsim.New(natsURL, mapID, pocketbaseURL, pbAdminEmail, pbAdminPassword, tickHz, logger)
	if err != nil {
		log.Fatalf("worldsim init: %v", err)
	}

	logger.Info("worldsim starting", "nats", natsURL, "tick_hz", tickHz, "map", mapID)
	if err := sim.Run(ctx); err != nil {
		logger.Info("worldsim stopped", "err", err)
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

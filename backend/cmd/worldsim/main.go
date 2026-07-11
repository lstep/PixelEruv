package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"
	"github.com/pocketbase/pocketbase/tools/osutils"

	"github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/lstep/pixeleruv/backend/internal/worldsim"

	// Register Go migrations (side-effect import)
	_ "github.com/lstep/pixeleruv/backend/migrations"
)

func main() {
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	tickHz := envInt("TICK_HZ", 20)
	mapID := envOr("MAP_ID", "map1")
	pbDataDir := envOr("PB_DATA_DIR", "./pb_data")
	pbHTTPAddr := envOr("PB_HTTP_ADDR", ":8090")

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

	// Initialize PocketBase as an embedded library.
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: pbDataDir,
	})

	// Configure the serve command to bind on the specified HTTP address.
	// PB's default is 127.0.0.1:8090; for Docker we need 0.0.0.0:8090.
	app.RootCmd.SetArgs([]string{"serve", "--http=" + pbHTTPAddr})

	// Register the migrate command (for manual migration operations).
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: osutils.IsProbablyGoRun(),
	})

	// Bootstrap: init DB + run system migrations (no HTTP server yet).
	if err := app.Bootstrap(); err != nil {
		log.Fatalf("pocketbase bootstrap: %v", err)
	}

	// Run app migrations (our Go migrations in backend/migrations/).
	// Bootstrap() only runs system migrations; app migrations run on
	// serve, but we need collections to exist before worldsim starts.
	if err := app.RunAllMigrations(); err != nil {
		log.Fatalf("pocketbase migrations: %v", err)
	}

	// Start PB's HTTP server in a goroutine (admin GUI + file serving for
	// the frontend). The HTTP server runs alongside worldsim's tick loop.
	go func() {
		if err := app.Start(); err != nil {
			log.Fatalf("pocketbase start: %v", err)
		}
	}()

	sim, err := worldsim.New(natsURL, mapID, app, tickHz, logger)
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

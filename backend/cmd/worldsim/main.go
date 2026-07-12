package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
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
	defaultMap := envOr("DEFAULT_MAP", "main")
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

	// Configure SMTP settings from env vars so PocketBase can send
	// verification emails. In dev, point SMTP_HOST at MailHog.
	configureSMTP(app)

	// Configure OAuth2 providers from env vars. Only providers with both
	// client ID and secret set are enabled.
	configureOAuth2(app)

	// Start PB's HTTP server in a goroutine (admin GUI + file serving for
	// the frontend). The HTTP server runs alongside worldsim's tick loop.
	go func() {
		if err := app.Start(); err != nil {
			log.Fatalf("pocketbase start: %v", err)
		}
	}()

	sim, err := worldsim.New(natsURL, defaultMap, app, tickHz, logger)
	if err != nil {
		log.Fatalf("worldsim init: %v", err)
	}

	logger.Info("worldsim starting", "nats", natsURL, "tick_hz", tickHz, "default_map", defaultMap)
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

// configureSMTP sets PocketBase SMTP settings from env vars. In dev,
// SMTP_HOST=mailhog + SMTP_PORT=1025 routes emails to MailHog's web UI.
func configureSMTP(app core.App) {
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		return
	}
	port := envInt("SMTP_PORT", 587)
	s := app.Settings()
	s.SMTP.Enabled = true
	s.SMTP.Host = host
	s.SMTP.Port = port
	s.SMTP.Username = os.Getenv("SMTP_USERNAME")
	s.SMTP.Password = os.Getenv("SMTP_PASSWORD")
	s.SMTP.TLS = os.Getenv("SMTP_TLS") == "true"
	if from := os.Getenv("SMTP_FROM"); from != "" {
		s.Meta.SenderAddress = from
	}
	if name := os.Getenv("SMTP_SENDER_NAME"); name != "" {
		s.Meta.SenderName = name
	}
	if err := app.Save(s); err != nil {
		log.Printf("configure SMTP: %v", err)
	}
}

// configureOAuth2 enables OAuth2 providers on the users collection from
// env vars. Only providers with both client ID and secret set are added.
func configureOAuth2(app core.App) {
	type providerEnv struct {
		name   string
		idKey  string
		secKey string
	}
	providers := []providerEnv{
		{"google", "OAUTH2_GOOGLE_CLIENT_ID", "OAUTH2_GOOGLE_SECRET"},
		{"github", "OAUTH2_GITHUB_CLIENT_ID", "OAUTH2_GITHUB_SECRET"},
		{"facebook", "OAUTH2_FACEBOOK_CLIENT_ID", "OAUTH2_FACEBOOK_SECRET"},
	}

	collection, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		log.Printf("configure OAuth2: users collection not found: %v", err)
		return
	}

	var configs []core.OAuth2ProviderConfig
	for _, p := range providers {
		clientID := os.Getenv(p.idKey)
		clientSecret := os.Getenv(p.secKey)
		if clientID == "" || clientSecret == "" {
			continue
		}
		configs = append(configs, core.OAuth2ProviderConfig{
			Name:         p.name,
			ClientId:     clientID,
			ClientSecret: clientSecret,
		})
	}

	collection.OAuth2.Enabled = len(configs) > 0
	collection.OAuth2.Providers = configs

	if err := app.Save(collection); err != nil {
		log.Printf("configure OAuth2: %v", err)
	}
}

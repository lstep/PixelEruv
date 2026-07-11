// seed-sprites uploads character spritesheets from a directory into
// PocketBase's sprite_bases collection. By default it only seeds when the
// collection is empty (first run). Use -force to upload new sheets, skipping
// any whose name already exists.
//
// Usage:
//
//	seed-sprites [-dir ./sprites] [-force]
//
// Env vars:
//
//	PB_DATA_DIR    (default ./pb_data) — must match worldsim's data dir
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/pocketbase/pocketbase"

	"github.com/lstep/pixeleruv/backend/internal/worldsim"

	// Register Go migrations (side-effect import)
	_ "github.com/lstep/pixeleruv/backend/migrations"
)

func main() {
	dir := flag.String("dir", "./sprites", "directory containing PNG spritesheets")
	force := flag.Bool("force", false, "upload all PNGs, skipping existing names")
	flag.Parse()

	pbDataDir := envOr("PB_DATA_DIR", "./pb_data")

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: pbDataDir,
	})
	if err := app.Bootstrap(); err != nil {
		fmt.Fprintf(os.Stderr, "seed-sprites: pocketbase bootstrap: %v\n", err)
		os.Exit(1)
	}

	store := worldsim.NewSpriteStore(app)
	if err := store.Seed(*dir, *force); err != nil {
		fmt.Fprintf(os.Stderr, "seed-sprites: %v\n", err)
		os.Exit(1)
	}
	log.Printf("seed-sprites: done (dir=%s, force=%v)", *dir, *force)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

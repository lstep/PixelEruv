// seed-sprites uploads character spritesheets from a directory into
// PocketBase's sprite_bases collection. By default it only seeds when the
// collection is empty (first run). Use -force to upload new sheets, skipping
// any whose name already exists.
//
// Usage:
//
//	seed-sprites [-dir ./sprites] [-force]
//
// Env vars (same as worldsim):
//
//	POCKETBASE_URL       (default http://localhost:8090)
//	PB_ADMIN_EMAIL       (default admin@pixeleruv.local)
//	PB_ADMIN_PASSWORD    (default password123)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/lstep/pixeleruv/backend/internal/worldsim"
)

func main() {
	dir := flag.String("dir", "./sprites", "directory containing PNG spritesheets")
	force := flag.Bool("force", false, "upload all PNGs, skipping existing names")
	flag.Parse()

	pocketbaseURL := envOr("POCKETBASE_URL", "http://localhost:8090")
	pbAdminEmail := envOr("PB_ADMIN_EMAIL", "admin@pixeleruv.local")
	pbAdminPassword := envOr("PB_ADMIN_PASSWORD", "password123")

	store := worldsim.NewSpriteStore(pocketbaseURL, pbAdminEmail, pbAdminPassword)
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

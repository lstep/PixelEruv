package worldsim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

// SpriteBase represents a record in PocketBase's sprite_bases collection.
type SpriteBase struct {
	ID   string
	Name string
}

// SpriteStore handles the sprite_bases PB collection: catalog reads for
// validation, and seeding on first run. Uses the PocketBase Go SDK.
type SpriteStore struct {
	app core.App
}

func NewSpriteStore(app core.App) *SpriteStore {
	return &SpriteStore{app: app}
}

// ListBases returns all sprite_bases records.
func (s *SpriteStore) ListBases() ([]SpriteBase, error) {
	records, err := s.app.FindAllRecords("sprite_bases")
	if err != nil {
		return nil, err
	}
	bases := make([]SpriteBase, 0, len(records))
	for _, r := range records {
		bases = append(bases, SpriteBase{
			ID:   r.Id,
			Name: r.GetString("name"),
		})
	}
	return bases, nil
}

// BaseExists checks if a sprite_bases record ID exists. Used by
// handleSetSpriteBase for validation.
func (s *SpriteStore) BaseExists(id string) (bool, error) {
	if id == "" {
		return true, nil // empty = fallback, always "valid"
	}
	record, err := s.app.FindRecordById("sprite_bases", id)
	if err != nil {
		return false, nil
	}
	return record != nil, nil
}

// SeedIfEmpty uploads every PNG in dir as a sprite_bases record, but only if
// the collection is currently empty. Idempotent: no-op if any records exist.
// Called once at worldsim startup.
func (s *SpriteStore) SeedIfEmpty(dir string) error {
	bases, err := s.ListBases()
	if err != nil {
		return fmt.Errorf("list sprite_bases: %w", err)
	}
	if len(bases) > 0 {
		return nil
	}
	return s.Seed(dir, false)
}

// Seed uploads PNGs from dir. With force=false it returns early if the
// collection is non-empty (equivalent to SeedIfEmpty). With force=true it
// uploads every PNG, skipping per-file if a record with that name already
// exists. Used by the cmd/seed-sprites CLI.
func (s *SpriteStore) Seed(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read sprites dir %s: %w", dir, err)
	}

	// Build a set of existing names for per-file skip (force mode).
	existing := map[string]bool{}
	bases, err := s.ListBases()
	if err != nil {
		return fmt.Errorf("list sprite_bases: %w", err)
	}
	if !force && len(bases) > 0 {
		return nil
	}
	for _, b := range bases {
		existing[b.Name] = true
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.ToLower(filepath.Ext(name)) != ".png" {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if existing[stem] {
			continue
		}
		if err := s.uploadBase(filepath.Join(dir, name), stem); err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
	}
	return nil
}

// uploadBase creates a single sprite_bases record with the given PNG file
// and name.
func (s *SpriteStore) uploadBase(path, name string) error {
	collection, err := s.app.FindCollectionByNameOrId("sprite_bases")
	if err != nil {
		return fmt.Errorf("find sprite_bases collection: %w", err)
	}

	f, err := filesystem.NewFileFromPath(path)
	if err != nil {
		return fmt.Errorf("create file %s: %w", path, err)
	}

	record := core.NewRecord(collection)
	record.Set("name", name)
	record.Set("sheet", f)
	return s.app.Save(record)
}

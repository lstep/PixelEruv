package worldsim

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

// MapStore handles the maps PB collection: it can seed a default map record
// (Tiled JSON + tileset PNGs) on first run when no record exists for the
// configured map name. Uses the PocketBase Go SDK (in-process DAO access).
type MapStore struct {
	app core.App
}

func NewMapStore(app core.App) *MapStore {
	return &MapStore{app: app}
}

// ListAllMaps returns all map records from PocketBase.
func (s *MapStore) ListAllMaps() ([]*MapRecordInfo, error) {
	collection, err := s.app.FindCollectionByNameOrId("maps")
	if err != nil {
		return nil, err
	}
	records, err := s.app.FindAllRecords(collection)
	if err != nil {
		return nil, err
	}
	var result []*MapRecordInfo
	for _, r := range records {
		result = append(result, &MapRecordInfo{
			Name:              r.GetString("name"),
			TiledJSONFilename: r.GetString("tiled_json"),
			Options:           json.RawMessage(r.GetString("options")),
			IsDefault:         r.GetBool("is_default"),
		})
	}
	return result, nil
}

// mapRecordExists returns true if a maps record with the given name exists.
func (s *MapStore) mapRecordExists(mapName string) (bool, error) {
	record, _ := s.app.FindFirstRecordByData("maps", "name", mapName)
	return record != nil, nil
}

// SeedMapIfMissing uploads the Tiled JSON and referenced tileset PNGs from
// mapDir as a new maps record named mapName, but only if no record with that
// name exists yet. Idempotent: no-op if the map is already present.
// jsonFile is the filename of the Tiled JSON inside mapDir (e.g.
// "default-map.json").
func (s *MapStore) SeedMapIfMissing(mapName, mapDir, jsonFile string) error {
	exists, err := s.mapRecordExists(mapName)
	if err != nil {
		return fmt.Errorf("check map record: %w", err)
	}
	if exists {
		return nil
	}

	jsonPath := filepath.Join(mapDir, jsonFile)
	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		return fmt.Errorf("read map json %s: %w", jsonPath, err)
	}

	// Extract tileset image filenames from the Tiled JSON so we upload exactly
	// the referenced PNGs (PocketBase requires at least one tileset file).
	var tiled struct {
		Tilesets []struct {
			Image string `json:"image"`
		} `json:"tilesets"`
	}
	if err := json.Unmarshal(jsonBytes, &tiled); err != nil {
		return fmt.Errorf("parse map json: %w", err)
	}

	collection, err := s.app.FindCollectionByNameOrId("maps")
	if err != nil {
		return fmt.Errorf("find maps collection: %w", err)
	}

	record := core.NewRecord(collection)
	record.Set("name", mapName)
	// Seeded map is the default on a fresh deploy (no other maps exist yet).
	record.Set("is_default", true)

	// tiled_json file field.
	jsonFileObj, err := filesystem.NewFileFromPath(jsonPath)
	if err != nil {
		return fmt.Errorf("create tiled_json file: %w", err)
	}
	record.Set("tiled_json", jsonFileObj)

	// tilesets file field — one per referenced PNG.
	var tilesetFiles []*filesystem.File
	for _, ts := range tiled.Tilesets {
		pngName := ts.Image
		if pngName == "" {
			continue
		}
		if base := filepath.Base(pngName); base != "" {
			pngName = base
		}
		pngPath := filepath.Join(mapDir, pngName)
		pngFile, err := filesystem.NewFileFromPath(pngPath)
		if err != nil {
			return fmt.Errorf("create tileset file %s: %w", pngName, err)
		}
		tilesetFiles = append(tilesetFiles, pngFile)
	}
	if len(tilesetFiles) == 0 {
		// PocketBase requires at least one tileset file (field is required).
		return fmt.Errorf("no tileset PNGs found in %s", mapDir)
	}
	record.Set("tilesets", tilesetFiles)

	return s.app.Save(record)
}

// FetchMapRecordInfo returns lightweight metadata for a map record, used to
// detect when the map has been re-uploaded (filename changes).
func (s *MapStore) FetchMapRecordInfo(mapName string) (*MapRecordInfo, error) {
	record, err := s.app.FindFirstRecordByData("maps", "name", mapName)
	if err != nil || record == nil {
		return nil, fmt.Errorf("no map named %q", mapName)
	}
	return &MapRecordInfo{
		Name:              record.GetString("name"),
		TiledJSONFilename: record.GetString("tiled_json"),
		Options:           json.RawMessage(record.GetString("options")),
		IsDefault:         record.GetBool("is_default"),
	}, nil
}

// LoadMapData fetches the Tiled map JSON from PocketBase by map name and
// builds a collision grid from the "Walls" layer (any non-zero tile =
// blocked). With PB embedded, no retry is needed — the collection exists
// by the time this is called (migrations run during Bootstrap).
func (s *MapStore) LoadMapData(mapName string) (*MapData, error) {
	return s.loadMapOnce(mapName)
}

// loadMapOnce fetches and parses a single map record.
func (s *MapStore) loadMapOnce(mapName string) (*MapData, error) {
	record, err := s.app.FindFirstRecordByData("maps", "name", mapName)
	if err != nil || record == nil {
		return nil, fmt.Errorf("no map named %q", mapName)
	}

	tiledJSONFilename := record.GetString("tiled_json")
	if tiledJSONFilename == "" {
		return nil, fmt.Errorf("map %q has no tiled_json file", mapName)
	}

	// Read the Tiled JSON file from PB's filesystem.
	fileKey := record.BaseFilesPath() + "/" + tiledJSONFilename
	fsys, err := s.app.NewFilesystem()
	if err != nil {
		return nil, fmt.Errorf("init filesystem: %w", err)
	}
	defer fsys.Close()

	r, err := fsys.GetReader(fileKey)
	if err != nil {
		return nil, fmt.Errorf("read tiled json: %w", err)
	}
	defer r.Close()

	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read tiled json body: %w", err)
	}

	md, err := ParseTiledMapJSON(body)
	if err != nil {
		return nil, err
	}
	md.Options = json.RawMessage(record.GetString("options"))
	return md, nil
}

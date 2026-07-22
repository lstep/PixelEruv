package worldsim

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// asset HTTP handlers serve map and sprite data from PocketBase via the Go SDK,
// bypassing collection API rules. This allows the maps/sprite_bases collections
// to be fully locked (nil rules) while still letting the frontend fetch game
// assets. Registered on PB's embedded router in worldsim.go OnServe.

type tilesetAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type mapAssetsResponse struct {
	Name      string          `json:"name"`
	TiledJson json.RawMessage `json:"tiledJson"`
	Tilesets  []tilesetAsset  `json:"tilesets"`
}

type spriteBaseAsset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Minimal Tiled JSON shape — only the tileset fields we need for name matching.
type tiledMapForTilesets struct {
	Tilesets []struct {
		Name  string `json:"name"`
		Image string `json:"image"`
	} `json:"tilesets"`
}

// handleAssetMapDefault serves GET /api/assets/maps/default — the map marked
// is_default=true, falling back to name "main" if none is marked default.
func (s *Simulator) handleAssetMapDefault(e *core.RequestEvent) error {
	record, err := s.app.FindFirstRecordByData("maps", "is_default", true)
	if err != nil || record == nil {
		// Fall back to "main" for backwards compatibility.
		return s.serveMapAssetsByName(e, "main")
	}
	return s.serveMapAssetsFromRecord(e, record)
}

// handleAssetMap serves GET /api/assets/maps/{name}.
func (s *Simulator) handleAssetMap(e *core.RequestEvent) error {
	name := e.Request.PathValue("name")
	if name == "" {
		return e.JSON(http.StatusBadRequest, map[string]any{"error": "missing map name"})
	}
	return s.serveMapAssetsByName(e, name)
}

func (s *Simulator) serveMapAssetsByName(e *core.RequestEvent, name string) error {
	record, err := s.app.FindFirstRecordByData("maps", "name", name)
	if err != nil || record == nil {
		return e.JSON(http.StatusNotFound, map[string]any{"error": "map not found: " + name})
	}
	return s.serveMapAssetsFromRecord(e, record)
}

func (s *Simulator) serveMapAssetsFromRecord(e *core.RequestEvent, record *core.Record) error {
	mapName := record.GetString("name")
	tiledJSONFilename := record.GetString("tiled_json")
	if tiledJSONFilename == "" {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "map has no tiled_json file"})
	}

	// Read the Tiled JSON from PB's filesystem.
	fileKey := record.BaseFilesPath() + "/" + tiledJSONFilename
	fsys, err := s.app.NewFilesystem()
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "init filesystem"})
	}
	defer fsys.Close()

	r, err := fsys.GetReader(fileKey)
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "read tiled json"})
	}
	defer r.Close()

	tiledBytes, err := io.ReadAll(r)
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "read tiled json body"})
	}

	// Parse tileset entries for name-to-filename matching.
	var tiled tiledMapForTilesets
	if err := json.Unmarshal(tiledBytes, &tiled); err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "parse tiled json"})
	}

	// PB tileset filenames (PB renames uploads, e.g. tileset.png → tileset_abc123.png).
	pbTilesets := record.GetStringSlice("tilesets")

	// Match each Tiled tileset to its PB file by normalized stem, then construct
	// a URL pointing to the file-serving endpoint. Same matching logic as the
	// old frontend mapLoader.ts.
	tilesets := make([]tilesetAsset, 0, len(tiled.Tilesets))
	for _, ts := range tiled.Tilesets {
		basename := ts.Image
		if parts := strings.Split(ts.Image, "/"); len(parts) > 0 {
			basename = parts[len(parts)-1]
		}
		stem := normalizeAssetStem(basename)
		pbFile := ts.Image
		for _, f := range pbTilesets {
			if strings.HasPrefix(normalizeAssetStem(f), stem) {
				pbFile = f
				break
			}
		}
		tilesets = append(tilesets, tilesetAsset{
			Name: ts.Name,
			URL:  "/api/assets/maps/" + mapName + "/tilesets/" + pbFile,
		})
	}

	return e.JSON(http.StatusOK, mapAssetsResponse{
		Name:      mapName,
		TiledJson: json.RawMessage(tiledBytes),
		Tilesets:  tilesets,
	})
}

// handleAssetTileset serves GET /api/assets/maps/{name}/tilesets/{filename} —
// streams a tileset PNG from PB's filesystem.
func (s *Simulator) handleAssetTileset(e *core.RequestEvent) error {
	mapName := e.Request.PathValue("name")
	filename := e.Request.PathValue("filename")
	if mapName == "" || filename == "" {
		return e.JSON(http.StatusBadRequest, map[string]any{"error": "missing map name or filename"})
	}

	record, err := s.app.FindFirstRecordByData("maps", "name", mapName)
	if err != nil || record == nil {
		return e.JSON(http.StatusNotFound, map[string]any{"error": "map not found"})
	}

	// Verify the requested filename is one of the map's tileset files.
	pbTilesets := record.GetStringSlice("tilesets")
	found := false
	for _, f := range pbTilesets {
		if f == filename {
			found = true
			break
		}
	}
	if !found {
		return e.JSON(http.StatusNotFound, map[string]any{"error": "tileset not found"})
	}

	return s.servePBFile(e, record.BaseFilesPath()+"/"+filename, filename)
}

// handleAssetSprites serves GET /api/assets/sprites — lists all sprite_bases
// with URLs pointing to the sheet file-serving endpoint.
func (s *Simulator) handleAssetSprites(e *core.RequestEvent) error {
	records, err := s.app.FindAllRecords("sprite_bases")
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "list sprite_bases"})
	}

	result := make([]spriteBaseAsset, 0, len(records))
	for _, r := range records {
		result = append(result, spriteBaseAsset{
			ID:   r.Id,
			Name: r.GetString("name"),
			URL:  "/api/assets/sprites/" + r.Id + "/sheet",
		})
	}
	return e.JSON(http.StatusOK, result)
}

// handleAssetSpriteSheet serves GET /api/assets/sprites/{id}/sheet — streams
// the sprite PNG from PB's filesystem.
func (s *Simulator) handleAssetSpriteSheet(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if id == "" {
		return e.JSON(http.StatusBadRequest, map[string]any{"error": "missing sprite id"})
	}

	record, err := s.app.FindRecordById("sprite_bases", id)
	if err != nil || record == nil {
		return e.JSON(http.StatusNotFound, map[string]any{"error": "sprite not found"})
	}

	sheet := record.GetString("sheet")
	if sheet == "" {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "sprite has no sheet file"})
	}

	return s.servePBFile(e, record.BaseFilesPath()+"/"+sheet, sheet)
}

// servePBFile reads a file from PB's filesystem and streams it to the HTTP
// response with a Content-Type based on the file extension.
func (s *Simulator) servePBFile(e *core.RequestEvent, fileKey, filename string) error {
	fsys, err := s.app.NewFilesystem()
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "init filesystem"})
	}
	defer fsys.Close()

	r, err := fsys.GetReader(fileKey)
	if err != nil {
		return e.JSON(http.StatusNotFound, map[string]any{"error": "file not found"})
	}
	defer r.Close()

	e.Response.Header().Set("Content-Type", contentTypeForFile(filename))
	e.Response.Header().Set("Cache-Control", "public, max-age=3600")
	_, err = io.Copy(e.Response, r)
	return err
}

// contentTypeForFile returns a Content-Type based on the file extension.
func contentTypeForFile(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".json":
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// normalizeAssetStem lowercases a filename, strips its extension, and removes
// non-alphanumeric characters — used for matching Tiled tileset image names to
// PB's renamed file filenames. Mirrors the old frontend mapLoader.ts logic.
func normalizeAssetStem(filename string) string {
	stem := filename
	if ext := filepath.Ext(filename); ext != "" {
		stem = strings.TrimSuffix(stem, ext)
	}
	stem = strings.ToLower(stem)
	var b strings.Builder
	for _, c := range stem {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

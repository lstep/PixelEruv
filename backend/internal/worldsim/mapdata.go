package worldsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MapData holds the spatial properties of a Tiled map needed by the
// simulation: dimensions, collision grid, zones, and base entities.
type MapData struct {
	Width     int
	Height    int
	Collision [][]bool // [y][x] — true = blocked
	Zones     []*Zone
	Entities  []*PropEntity
}

// PropEntity is a base entity authored on the "Entities" object layer in
// Tiled (e.g. an interactive box) — see
// documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md
// Part C. It exists in the ECS from map load with no owning extension; an
// extension claims it by registering an input trigger and self-filtering on
// EntityType/OwnerExtension when dispatched an input event.
type PropEntity struct {
	ID             string
	X, Y           float32 // tile coordinates
	EntityType     string
	OwnerExtension string
	TriggerRadius  float32
	Gid            uint32 // Tiled global tile ID (for sprite rendering)
}

// tiledMapJSON is the minimal subset of the Tiled JSON format we read.
type tiledMapJSON struct {
	Width      int  `json:"width"`
	Height     int  `json:"height"`
	TileWidth  int  `json:"tilewidth"`
	TileHeight int  `json:"tileheight"`
	Layers     []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"` // "tilelayer" or "objectgroup"
		Data    []uint32 `json:"data"`
		Objects []struct {
			Name       string `json:"name"`
			Class      string `json:"class"`
			Gid        uint32 `json:"gid"`
			X          float64 `json:"x"`
			Y          float64 `json:"y"`
			Width      float64 `json:"width"`
			Height     float64 `json:"height"`
			Ellipse    bool   `json:"ellipse"`
			Polygon    []struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			} `json:"polygon"`
			Properties []struct {
				Name  string      `json:"name"`
				Type  string      `json:"type"`
				Value interface{} `json:"value"`
			} `json:"properties"`
		} `json:"objects"`
	} `json:"layers"`
}

// LoadMap fetches the Tiled map JSON from PocketBase by map name and builds
// a collision grid from the "Walls" layer (any non-zero tile = blocked).
// It retries for up to 30 seconds in case PocketBase is still starting.
func LoadMap(pocketbaseURL, mapName string) (*MapData, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		md, err := loadMapOnce(pocketbaseURL, mapName)
		if err == nil {
			return md, nil
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, lastErr
}

// MapRecordInfo is the lightweight metadata for a map record in PocketBase,
// used to detect when the map has been re-uploaded (filename changes).
type MapRecordInfo struct {
	TiledJSONFilename string
}

// FetchMapRecordInfo fetches just the map record metadata (without parsing
// the full Tiled JSON). Used by the periodic reload checker to detect
// changes by comparing the tiled_json filename.
func FetchMapRecordInfo(pocketbaseURL, mapName string) (*MapRecordInfo, error) {
	resp, err := http.Get(fmt.Sprintf(
		"%s/api/collections/maps/records?filter=(name=\"%s\")&perPage=1",
		pocketbaseURL, mapName,
	))
	if err != nil {
		return nil, fmt.Errorf("fetch map record: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pocketbase responded %d", resp.StatusCode)
	}
	var record struct {
		Items []struct {
			TiledJSON string `json:"tiled_json"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	if len(record.Items) == 0 {
		return nil, fmt.Errorf("no map named %q", mapName)
	}
	return &MapRecordInfo{TiledJSONFilename: record.Items[0].TiledJSON}, nil
}

func loadMapOnce(pocketbaseURL, mapName string) (*MapData, error) {
	// Fetch the maps record by name.
	resp, err := http.Get(fmt.Sprintf(
		"%s/api/collections/maps/records?filter=(name=\"%s\")&perPage=1",
		pocketbaseURL, mapName,
	))
	if err != nil {
		return nil, fmt.Errorf("fetch map record: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pocketbase responded %d", resp.StatusCode)
	}

	var record struct {
		Items []struct {
			ID           string   `json:"id"`
			CollectionID string   `json:"collectionId"`
			TiledJSON    string   `json:"tiled_json"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	if len(record.Items) == 0 {
		return nil, fmt.Errorf("no map named %q in pocketbase", mapName)
	}

	r := record.Items[0]
	jsonURL := fmt.Sprintf("%s/api/files/%s/%s/%s",
		pocketbaseURL, r.CollectionID, r.ID, r.TiledJSON)

	// Fetch the Tiled JSON.
	jresp, err := http.Get(jsonURL)
	if err != nil {
		return nil, fmt.Errorf("fetch tiled json: %w", err)
	}
	defer jresp.Body.Close()
	if jresp.StatusCode != 200 {
		return nil, fmt.Errorf("tiled json responded %d", jresp.StatusCode)
	}

	body, err := io.ReadAll(jresp.Body)
	if err != nil {
		return nil, fmt.Errorf("read tiled json: %w", err)
	}

	return parseTiledMapJSON(body)
}

// parseTiledMapJSON parses a Tiled JSON export into MapData: collision grid,
// zones, and base entities. Extracted from loadMapOnce so it can be tested
// without a PocketBase server.
func parseTiledMapJSON(body []byte) (*MapData, error) {
	var tiled tiledMapJSON
	if err := json.Unmarshal(body, &tiled); err != nil {
		return nil, fmt.Errorf("parse tiled json: %w", err)
	}

	// Build collision grid from the "Walls" layer.
	collision := make([][]bool, tiled.Height)
	for y := range collision {
		collision[y] = make([]bool, tiled.Width)
	}

	for _, layer := range tiled.Layers {
		if strings.ToLower(layer.Name) != "walls" {
			continue
		}
		for y := 0; y < tiled.Height && y*tiled.Width < len(layer.Data); y++ {
			for x := 0; x < tiled.Width; x++ {
				idx := y*tiled.Width + x
				if idx < len(layer.Data) && layer.Data[idx] != 0 {
					collision[y][x] = true
				}
			}
		}
		break
	}

	// Parse zones from the "Zones" object layer.
	var zones []*Zone
	tileW := float32(tiled.TileWidth)
	tileH := float32(tiled.TileHeight)
	if tileW == 0 {
		tileW = 32 // fallback
	}
	if tileH == 0 {
		tileH = 32
	}

	for _, layer := range tiled.Layers {
		if strings.ToLower(layer.Name) != "zones" || layer.Type != "objectgroup" {
			continue
		}
		for _, obj := range layer.Objects {
			if obj.Name == "" {
				continue
			}
			z := &Zone{
				ID:  obj.Name,
				X:   float32(obj.X) / tileW,
				Y:   float32(obj.Y) / tileH,
				W:   float32(obj.Width) / tileW,
				H:   float32(obj.Height) / tileH,
			}

			// Determine shape.
			if obj.Ellipse {
				if obj.Width != obj.Height {
					return nil, fmt.Errorf("zone %q: ellipse must have width == height", obj.Name)
				}
				z.Shape = ShapeCircle
				z.Radius = float32(obj.Width) / tileW / 2
			} else if len(obj.Polygon) > 0 {
				z.Shape = ShapePolygon
				for _, p := range obj.Polygon {
					z.Polygon = append(z.Polygon, [2]float32{
						float32(p.X) / tileW,
						float32(p.Y) / tileH,
					})
				}
			} else {
				z.Shape = ShapeRect
			}

			// Parse custom properties.
			for _, prop := range obj.Properties {
				switch prop.Name {
				case "zone_type":
					if s, ok := prop.Value.(string); ok {
						z.ZoneType = s
					}
				case "is_exclusive":
					if b, ok := prop.Value.(bool); ok {
						z.IsExclusive = b
					}
				case "mobility":
					if s, ok := prop.Value.(string); ok {
						z.Mobility = s
					}
				case "av_enabled":
					if b, ok := prop.Value.(bool); ok {
						z.AvEnabled = b
					}
				}
			}

			zones = append(zones, z)
		}
		break
	}

	// Parse base entities from the "Entities" object layer.
	var entities []*PropEntity
	for _, layer := range tiled.Layers {
		if strings.ToLower(layer.Name) != "entities" || layer.Type != "objectgroup" {
			continue
		}
		for _, obj := range layer.Objects {
			if obj.Name == "" {
				continue
			}
			pe := &PropEntity{
				ID:  obj.Name,
				Gid: obj.Gid,
				X:   float32(obj.X) / tileW,
				Y:   float32(obj.Y) / tileH,
			}
			for _, prop := range obj.Properties {
				switch prop.Name {
				case "entity_type":
					if s, ok := prop.Value.(string); ok {
						pe.EntityType = s
					}
				case "owner_extension":
					if s, ok := prop.Value.(string); ok {
						pe.OwnerExtension = s
					}
				case "trigger_radius":
					if f, ok := prop.Value.(float64); ok {
						pe.TriggerRadius = float32(f)
					}
				}
			}
			entities = append(entities, pe)
		}
		break
	}

	return &MapData{
		Width:     tiled.Width,
		Height:    tiled.Height,
		Collision: collision,
		Zones:     zones,
		Entities:  entities,
	}, nil
}

// IsBlocked returns true if the tile at (tx, ty) is a wall or out of bounds.
func (m *MapData) IsBlocked(tx, ty int) bool {
	if tx < 0 || tx >= m.Width || ty < 0 || ty >= m.Height {
		return true
	}
	return m.Collision[ty][tx]
}

// FindSpawn returns the first non-blocked tile near the center of the map.
func (m *MapData) FindSpawn() (float32, float32) {
	cx, cy := m.Width/2, m.Height/2
	if !m.IsBlocked(cx, cy) {
		return float32(cx), float32(cy)
	}
	// Spiral outward from center.
	for r := 1; r < max(m.Width, m.Height); r++ {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				if abs(dx) != r && abs(dy) != r {
					continue
				}
				tx, ty := cx+dx, cy+dy
				if !m.IsBlocked(tx, ty) {
					return float32(tx), float32(ty)
				}
			}
		}
	}
	return float32(cx), float32(cy)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

package worldsim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
)

// MapData holds the spatial properties of a Tiled map needed by the
// simulation: dimensions, collision grid, zones, and base entities.
type MapData struct {
	Width      int
	Height     int
	Collision  [][]bool // [y][x] — true = blocked
	Zones      []*Zone
	SpawnZones []*Zone // subset of Zones with zone_type=spawn
	Entities   []*PropEntity
	Options    json.RawMessage // JSON options from the maps PB record
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
	GidOn          uint32                // alternate sprite gid for "on" state (0 = no alternate)
	OnInteractAction string              // immediate-mode: action_id fired on E press
	Actions          string              // popup-mode: comma-separated action_ids
	Interactions     map[string][]Effect // action_id -> effects list
	// LightEmitter attributes parsed from Tiled properties. LightIntensity
	// is 0-100 (0 = no light), LightColor is 0xRRGGBB (0 = default warm
	// white), LightRadius is in tiles (0 = default 3). Copied into the
	// entity's LightEmitter component at provisioning.
	LightIntensity uint32
	LightColor     uint32
	LightRadius    float32
}

// Effect is a single action to apply to a set of targets, declared in
// the entity's "interactions" Tiled property. The extension interprets
// the Action verb; the framework just routes.
type Effect struct {
	Action    string   `json:"action"`
	Payload   string   `json:"payload,omitempty"`
	TargetIDs []string `json:"target_ids"`
}

// FindEntityByName returns the base entity with the given name on this map,
// or nil if not found. Used by portal transitions to teleport to a named
// beacon entity on the target map.
func (md *MapData) FindEntityByName(name string) *PropEntity {
	for _, pe := range md.Entities {
		if pe.ID == name {
			return pe
		}
	}
	return nil
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

// MapRecordInfo is the lightweight metadata for a map record in PocketBase,
// used to detect when the map has been re-uploaded (filename changes).
type MapRecordInfo struct {
	Name              string
	TiledJSONFilename string
	Options           json.RawMessage
	IsDefault         bool
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
				case "target_map":
					if s, ok := prop.Value.(string); ok {
						z.PortalTargetMap = s
					}
				case "target_entity":
					if s, ok := prop.Value.(string); ok {
						z.PortalTargetEntity = s
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
				ID:           obj.Name,
				Gid:          obj.Gid,
				X:            float32(obj.X) / tileW,
				Y:            float32(obj.Y) / tileH,
				Interactions: map[string][]Effect{},
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
				case "gid_on":
					if f, ok := prop.Value.(float64); ok {
						pe.GidOn = uint32(f)
					}
				case "on_interact_action":
					if s, ok := prop.Value.(string); ok {
						pe.OnInteractAction = s
					}
				case "actions":
					if s, ok := prop.Value.(string); ok {
						pe.Actions = s
					}
				case "interactions":
					if s, ok := prop.Value.(string); ok {
						pe.Interactions = make(map[string][]Effect)
						if err := json.Unmarshal([]byte(s), &pe.Interactions); err != nil {
							// Malformed JSON — leave Interactions as empty map.
							// The entity will be inert (no effects to fire).
						}
					}
				case "light_intensity":
					if f, ok := prop.Value.(float64); ok {
						pe.LightIntensity = uint32(f)
					}
				case "light_color":
					if s, ok := prop.Value.(string); ok {
						pe.LightColor = parseHexColor(s)
					}
				case "light_radius":
					if f, ok := prop.Value.(float64); ok {
						pe.LightRadius = float32(f)
					}
				}
			}
			entities = append(entities, pe)
		}
		break
	}

	var spawnZones []*Zone
	for _, z := range zones {
		if z.ZoneType == "spawn" {
			spawnZones = append(spawnZones, z)
		}
	}

	return &MapData{
		Width:      tiled.Width,
		Height:     tiled.Height,
		Collision:  collision,
		Zones:      zones,
		SpawnZones: spawnZones,
		Entities:   entities,
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

// walkableTilesInZone returns the integer tiles inside z's bounding box
// (clamped to map bounds) that are not blocked and satisfy z.Contains.
// The bounding box is only the iteration range; the Contains check is what
// enforces the zone's actual shape (rect/circle/polygon).
func walkableTilesInZone(m *MapData, z *Zone) [][2]int {
	x0 := int(z.X)
	y0 := int(z.Y)
	x1 := int(z.X + z.W)
	y1 := int(z.Y + z.H)
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 >= m.Width {
		x1 = m.Width - 1
	}
	if y1 >= m.Height {
		y1 = m.Height - 1
	}
	var tiles [][2]int
	for ty := y0; ty <= y1; ty++ {
		for tx := x0; tx <= x1; tx++ {
			if m.IsBlocked(tx, ty) {
				continue
			}
			if !z.Contains(float32(tx), float32(ty)) {
				continue
			}
			tiles = append(tiles, [2]int{tx, ty})
		}
	}
	return tiles
}

// FindSpawnPoint returns a random non-blocked tile inside a random spawn
// zone. Falls back to FindSpawn() if there are no spawn zones or no spawn
// zone yields a walkable tile.
func (m *MapData) FindSpawnPoint(rng *rand.Rand) (float32, float32) {
	if len(m.SpawnZones) == 0 {
		return m.FindSpawn()
	}
	// Try spawn zones in random order; return the first walkable tile found.
	order := make([]int, len(m.SpawnZones))
	for i := range order {
		order[i] = i
	}
	for i := len(order) - 1; i > 0; i-- {
		j := rng.IntN(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	for _, i := range order {
		tiles := walkableTilesInZone(m, m.SpawnZones[i])
		if len(tiles) == 0 {
			continue
		}
		pick := tiles[rng.IntN(len(tiles))]
		return float32(pick[0]), float32(pick[1])
	}
	return m.FindSpawn()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// parseHexColor parses a hex color string like "#ffe6b4" or "ffe6b4" into a
// 0xRRGGBB uint32. Returns 0 on malformed input (caller treats 0 as
// "use default warm white").
func parseHexColor(s string) uint32 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return 0
	}
	var v uint32
	for _, c := range s {
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0
		}
		v = v<<4 | d
	}
	return v
}

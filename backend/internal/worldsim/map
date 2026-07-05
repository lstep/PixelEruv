package worldsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MapData holds the spatial properties of a Tiled map needed by the
// simulation: dimensions and a per-tile collision grid.
type MapData struct {
	Width     int
	Height    int
	Collision [][]bool // [y][x] — true = blocked
}

// tiledMapJSON is the minimal subset of the Tiled JSON format we read.
type tiledMapJSON struct {
	Width  int  `json:"width"`
	Height int  `json:"height"`
	Layers []struct {
		Name string   `json:"name"`
		Data []uint32 `json:"data"`
	} `json:"layers"`
}

// LoadMap fetches the Tiled map JSON from PocketBase by map name and builds
// a collision grid from the "Walls" layer (any non-zero tile = blocked).
func LoadMap(pocketbaseURL, mapName string) (*MapData, error) {
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

	return &MapData{
		Width:     tiled.Width,
		Height:    tiled.Height,
		Collision: collision,
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

//go:build ignore

// Generates a minimal Tiled map (test-map.json) and a 4-tile solid-color
// tileset PNG (tileset.png) for the lite MVP.
//
//   go run gen-map.go
//
// Output:
//   frontend/public/maps/test-map.json
//   frontend/public/maps/tileset.png
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

const (
	mapW, mapH = 20, 20
	tileSize   = 32
	tileCount  = 4 // grass, wall, floor, path
)

func main() {
	outDir := "frontend/public/maps"

	// --- Generate tileset PNG ---
	tilesetImg := image.NewRGBA(image.Rect(0, 0, tileSize*tileCount, tileSize))
	colors := []color.Color{
		color.RGBA{R: 74, G: 124, B: 58, A: 255},   // 0: grass (green)
		color.RGBA{R: 80, G: 80, B: 90, A: 255},    // 1: wall (dark gray)
		color.RGBA{R: 180, G: 160, B: 130, A: 255}, // 2: floor (tan)
		color.RGBA{R: 120, G: 100, B: 80, A: 255},  // 3: path (brown)
	}
	for i := range tileCount {
		c := colors[i]
		for y := range tileSize {
			for x := range tileSize {
				tilesetImg.SetRGBA(x+i*tileSize, y, color.RGBAModel.Convert(c).(color.RGBA))
			}
		}
	}

	tilesetPath := filepath.Join(outDir, "tileset.png")
	tilesetFile, err := os.Create(tilesetPath)
	if err != nil {
		panic(err)
	}
	if err := png.Encode(tilesetFile, tilesetImg); err != nil {
		panic(err)
	}
	tilesetFile.Close()
	fmt.Println("wrote", tilesetPath)

	// --- Generate map data ---
	// Layer 0 (ground): grass everywhere, floor in center room
	ground := make([][]int, mapH)
	for y := range mapH {
		ground[y] = make([]int, mapW)
		for x := range mapW {
			ground[y][x] = 0 // grass
		}
	}
	// Inner room floor (tiles 4..16)
	for y := 4; y <= 16; y++ {
		for x := 4; x <= 16; x++ {
			ground[y][x] = 2 // floor
		}
	}

	// Layer 1 (walls): border + room walls
	walls := make([][]int, mapH)
	for y := range mapH {
		walls[y] = make([]int, mapW)
		for x := range mapW {
			walls[y][x] = -1 // empty
		}
	}
	// Map border
	for i := range mapW {
		walls[0][i] = 1
		walls[mapH-1][i] = 1
	}
	for i := range mapH {
		walls[i][0] = 1
		walls[i][mapW-1] = 1
	}
	// Room walls (around the floor area)
	for i := 4; i <= 16; i++ {
		walls[4][i] = 1
		walls[16][i] = 1
		walls[i][4] = 1
		walls[i][16] = 1
	}
	// Doorways (gaps in the room walls)
	walls[4][10] = -1
	walls[16][10] = -1
	walls[10][4] = -1
	walls[10][16] = -1

	// Flatten layers for Tiled (row-major)
	groundFlat := flatten(ground)
	wallsFlat := flatten(walls)

	// Encode as base64 (Tiled CSV format is simpler but base64 is standard)
	groundB64 := encodeLayer(groundFlat)
	wallsB64 := encodeLayer(wallsFlat)

	tiledMap := map[string]any{
		"compressionlevel": -1,
		"width":            mapW,
		"height":           mapH,
		"tilewidth":        tileSize,
		"tileheight":       tileSize,
		"orientation":      "orthogonal",
		"renderorder":      "right-down",
		"tiledversion":     "1.11.0",
		"type":             "map",
		"version":          "1.10",
		"tilesets": []map[string]any{
			{
				"firstgid":          1,
				"source":            "tileset.json",
			},
		},
		"layers": []map[string]any{
			{
				"id":       1,
				"name":     "Ground",
				"type":     "tilelayer",
				"width":    mapW,
				"height":   mapH,
				"x":        0,
				"y":        0,
				"visible":  true,
				"opacity":  1,
				"data":     groundB64,
				"encoding": "base64",
			},
			{
				"id":       2,
				"name":     "Walls",
				"type":     "tilelayer",
				"width":    mapW,
				"height":   mapH,
				"x":        0,
				"y":        0,
				"visible":  true,
				"opacity":  1,
				"data":     wallsB64,
				"encoding": "base64",
			},
		},
	}

	mapPath := filepath.Join(outDir, "test-map.json")
	mapFile, err := os.Create(mapPath)
	if err != nil {
		panic(err)
	}
	enc := json.NewEncoder(mapFile)
	enc.SetIndent("", "  ")
	if err := enc.Encode(tiledMap); err != nil {
		panic(err)
	}
	mapFile.Close()
	fmt.Println("wrote", mapPath)

	// --- Generate tileset.json (Tiled tileset definition) ---
	tilesetJSON := map[string]any{
		"columns":     tileCount,
		"image":       "tileset.png",
		"imageheight": tileSize,
		"imagewidth":  tileSize * tileCount,
		"margin":      0,
		"spacing":     0,
		"tilecount":   tileCount,
		"tiledversion": "1.11.0",
		"tilewidth":   tileSize,
		"tileheight":  tileSize,
		"type":        "tileset",
		"version":     "1.10",
		"tiles": []map[string]any{
			{"id": 0, "properties": []map[string]any{{"name": "name", "value": "grass"}}},
			{"id": 1, "properties": []map[string]any{{"name": "name", "value": "wall"}}},
			{"id": 2, "properties": []map[string]any{{"name": "name", "value": "floor"}}},
			{"id": 3, "properties": []map[string]any{{"name": "name", "value": "path"}}},
		},
	}

	tilesetJSONPath := filepath.Join(outDir, "tileset.json")
	tilesetJSONFile, err := os.Create(tilesetJSONPath)
	if err != nil {
		panic(err)
	}
	enc2 := json.NewEncoder(tilesetJSONFile)
	enc2.SetIndent("", "  ")
	if err := enc2.Encode(tilesetJSON); err != nil {
		panic(err)
	}
	tilesetJSONFile.Close()
	fmt.Println("wrote", tilesetJSONPath)
}

func flatten(grid [][]int) []int {
	flat := make([]int, 0, mapW*mapH)
	for y := range mapH {
		flat = append(flat, grid[y]...)
	}
	return flat
}

// encodeLayer encodes a tile grid as base64 of raw uint32 GIDs (little-endian).
func encodeLayer(tiles []int) string {
	bytes := make([]byte, len(tiles)*4)
	for i, gid := range tiles {
		if gid < 0 {
			gid = 0
		}
		bytes[i*4+0] = byte(gid)
		bytes[i*4+1] = byte(gid >> 8)
		bytes[i*4+2] = byte(gid >> 16)
		bytes[i*4+3] = byte(gid >> 24)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

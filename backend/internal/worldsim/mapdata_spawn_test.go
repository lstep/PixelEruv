package worldsim

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// newGrid builds a Width×Height collision grid with the given blocked tiles
// keyed by "x,y". Unlisted tiles are walkable.
func newGrid(w, h int, blocked map[[2]int]bool) [][]bool {
	g := make([][]bool, h)
	for y := 0; y < h; y++ {
		g[y] = make([]bool, w)
		for x := 0; x < w; x++ {
			if blocked[[2]int{x, y}] {
				g[y][x] = true
			}
		}
	}
	return g
}

func TestFindSpawnPoint_NoSpawnZones_FallsBackToFindSpawn(t *testing.T) {
	md := &MapData{
		Width:     10,
		Height:    10,
		Collision: newGrid(10, 10, nil),
	}
	wantX, wantY := md.FindSpawn()
	gotX, gotY := md.FindSpawnPoint(rand.New(rand.NewPCG(1, 0)))
	if gotX != wantX || gotY != wantY {
		t.Errorf("FindSpawnPoint with no spawn zones = (%v,%v), want FindSpawn (%v,%v)",
			gotX, gotY, wantX, wantY)
	}
}

func TestFindSpawnPoint_PicksTileInsideRectZone(t *testing.T) {
	// 10x10 map, all walkable. One rect spawn zone covering tiles (2,2)-(5,5).
	md := &MapData{
		Width:     10,
		Height:    10,
		Collision: newGrid(10, 10, nil),
		SpawnZones: []*Zone{
			{ID: "s1", Shape: ShapeRect, X: 2, Y: 2, W: 4, H: 4},
		},
	}
	rng := rand.New(rand.NewPCG(42, 0))
	gotX, gotY := md.FindSpawnPoint(rng)
	tx, ty := int(gotX), int(gotY)
	if md.IsBlocked(tx, ty) {
		t.Errorf("spawn (%v,%v) is on a blocked tile", gotX, gotY)
	}
	if !md.SpawnZones[0].Contains(gotX, gotY) {
		t.Errorf("spawn (%v,%v) not inside zone per Contains", gotX, gotY)
	}
}

func TestFindSpawnPoint_AllZonesBlocked_FallsBack(t *testing.T) {
	// 10x10 map. Spawn zone at (2,2) W=3 H=3 fully walled (closed-interval
	// Contains admits x,y in [2,5], so wall that whole 4x4 block); center
	// (5,5) stays walkable as the FindSpawn fallback target.
	blocked := map[[2]int]bool{}
	for x := 2; x <= 5; x++ {
		for y := 2; y <= 5; y++ {
			blocked[[2]int{x, y}] = true
		}
	}
	md := &MapData{
		Width:     10,
		Height:    10,
		Collision: newGrid(10, 10, blocked),
		SpawnZones: []*Zone{
			{ID: "walled", Shape: ShapeRect, X: 2, Y: 2, W: 3, H: 3},
		},
	}
	wantX, wantY := md.FindSpawn()
	gotX, gotY := md.FindSpawnPoint(rand.New(rand.NewPCG(7, 0)))
	if gotX != wantX || gotY != wantY {
		t.Errorf("all-blocked spawn zones = (%v,%v), want FindSpawn fallback (%v,%v)",
			gotX, gotY, wantX, wantY)
	}
}

func TestFindSpawnPoint_DistributionCoversAllWalkableTiles(t *testing.T) {
	// 6x6 map, all walkable, one spawn zone covering the whole map.
	md := &MapData{
		Width:     6,
		Height:    6,
		Collision: newGrid(6, 6, nil),
		SpawnZones: []*Zone{
			{ID: "whole", Shape: ShapeRect, X: 0, Y: 0, W: 6, H: 6},
		},
	}
	// Enumerate expected walkable tiles via the same Contains semantics.
	want := map[[2]int]bool{}
	for ty := 0; ty < 6; ty++ {
		for tx := 0; tx < 6; tx++ {
			if !md.IsBlocked(tx, ty) && md.SpawnZones[0].Contains(float32(tx), float32(ty)) {
				want[[2]int{tx, ty}] = true
			}
		}
	}
	rng := rand.New(rand.NewPCG(123, 0))
	seen := map[[2]int]bool{}
	for i := 0; i < 5000; i++ {
		gotX, gotY := md.FindSpawnPoint(rng)
		k := [2]int{int(gotX), int(gotY)}
		if !want[k] {
			t.Fatalf("spawn %v not in expected walkable set", k)
		}
		seen[k] = true
	}
	if len(seen) != len(want) {
		t.Errorf("after 5000 draws, saw %d distinct tiles, want %d (all walkable tiles)",
			len(seen), len(want))
	}
}

func TestFindSpawnPoint_CircleZone(t *testing.T) {
	// 10x10 walkable map. Circle spawn zone centered at (5,5), radius 3.
	md := &MapData{
		Width:     10,
		Height:    10,
		Collision: newGrid(10, 10, nil),
		SpawnZones: []*Zone{
			{ID: "circ", Shape: ShapeCircle, X: 2, Y: 2, W: 6, H: 6, Radius: 3},
		},
	}
	rng := rand.New(rand.NewPCG(9, 0))
	for i := 0; i < 200; i++ {
		gotX, gotY := md.FindSpawnPoint(rng)
		tx, ty := int(gotX), int(gotY)
		if md.IsBlocked(tx, ty) {
			t.Fatalf("draw %d: spawn (%v,%v) blocked", i, gotX, gotY)
		}
		if !md.SpawnZones[0].Contains(gotX, gotY) {
			t.Fatalf("draw %d: spawn (%v,%v) outside circle per Contains", i, gotX, gotY)
		}
	}
}

func TestParseTiledMapJSON_SpawnZones(t *testing.T) {
	body := []byte(`{
		"width": 10, "height": 10, "tilewidth": 32, "tileheight": 32,
		"layers": [
			{
				"name": "Zones", "type": "objectgroup",
				"objects": [
					{"name": "spawn_a", "x": 64, "y": 64, "width": 96, "height": 96,
					 "properties": [{"name": "zone_type", "type": "string", "value": "spawn"}]},
					{"name": "meeting1", "x": 0, "y": 0, "width": 64, "height": 64,
					 "properties": [{"name": "zone_type", "type": "string", "value": "meeting"}]}
				]
			}
		]
	}`)
	md, err := parseTiledMapJSON(body)
	if err != nil {
		t.Fatalf("parseTiledMapJSON: %v", err)
	}
	if len(md.SpawnZones) != 1 {
		t.Fatalf("expected 1 spawn zone, got %d (zones total: %d)", len(md.SpawnZones), len(md.Zones))
	}
	if md.SpawnZones[0].ID != "spawn_a" {
		t.Errorf("spawn zone ID = %q, want spawn_a", md.SpawnZones[0].ID)
	}
	if md.SpawnZones[0].ZoneType != "spawn" {
		t.Errorf("spawn zone ZoneType = %q, want spawn", md.SpawnZones[0].ZoneType)
	}
	// Spawn zone must still be in the full Zones slice (registered as a zone).
	if len(md.Zones) != 2 {
		t.Errorf("expected 2 total zones, got %d", len(md.Zones))
	}
}

func TestCheckMapIntegrity_SpawnZoneNoWalkableTiles_Warns(t *testing.T) {
	// 6x6 map. Spawn zone at (1,1) W=2 H=2 fully walled (closed-interval
	// Contains admits x,y in [1,3], so wall that 3x3 block).
	blocked := map[[2]int]bool{}
	for x := 1; x <= 3; x++ {
		for y := 1; y <= 3; y++ {
			blocked[[2]int{x, y}] = true
		}
	}
	md := &MapData{
		Width:     6,
		Height:    6,
		Collision: newGrid(6, 6, blocked),
		SpawnZones: []*Zone{
			{ID: "walled_spawn", Shape: ShapeRect, X: 1, Y: 1, W: 2, H: 2, ZoneType: "spawn"},
		},
	}
	results := CheckMapIntegrity(md)
	var found bool
	for _, r := range results {
		if r.Zone == "walled_spawn" && strings.Contains(r.Message, "no walkable tiles") {
			found = true
			if r.Level != LevelWarning {
				t.Errorf("spawn-zone-no-walkable check level = %v, want LevelWarning", r.Level)
			}
		}
	}
	if !found {
		t.Errorf("expected a warning that spawn zone has no walkable tiles; got: %v", results)
	}
}

func TestCheckMapIntegrity_SpawnZoneWithWalkableTiles_NoWarn(t *testing.T) {
	md := &MapData{
		Width:     6,
		Height:    6,
		Collision: newGrid(6, 6, nil),
		SpawnZones: []*Zone{
			{ID: "ok_spawn", Shape: ShapeRect, X: 1, Y: 1, W: 2, H: 2, ZoneType: "spawn"},
		},
	}
	results := CheckMapIntegrity(md)
	for _, r := range results {
		if r.Zone == "ok_spawn" && strings.Contains(r.Message, "no walkable tiles") {
			t.Errorf("did not expect walkable-tiles warning for valid spawn zone; got: %v", r)
		}
	}
}

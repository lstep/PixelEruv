package worldsim

import (
	"log/slog"
	"testing"
)

// TestIsMoveBlocked_FeetOffset verifies that zone collision is evaluated
// at the avatar's feet, not at Position.Y (the sprite origin/upper-body).
//
// The frontend renders avatars with origin (0.5, 0.75) on a 64px-tall frame
// placed at (pos.X*32+16, pos.Y*32+16) — see GameScene.ts. This puts the
// feet at continuous Y = Position.Y + 1.0 (bottom of the tile below
// Position). Collision must check the feet position, otherwise the player
// stops with feet buried in a wall (moving down) or stops a full tile short
// (moving up).
func TestIsMoveBlocked_FeetOffset(t *testing.T) {
	// Wall zone w1 covers continuous tile space X[5,6], Y[5,6], expanded by
	// playerCollisionRadius (0.1) → effective X[4.9,6.1], Y[4.9,6.1].
	zones := []*Zone{{ID: "w1", Shape: ShapeRect, X: 5, Y: 5, W: 1, H: 1}}
	extMgr := NewExtensionManager(slog.Default())
	s := &Simulator{
		World: World{
			zones: map[string]*ZoneRegistry{"map1": NewZoneRegistry(zones, 20, 20)},
			maps:  map[string]*MapData{"map1": {Width: 20, Height: 20, Collision: make([][]bool, 20)}},
		},
		extMgr:   extMgr,
		movement: NewMovementSystem(extMgr),
	}
	for y := range s.maps["map1"].Collision {
		s.maps["map1"].Collision[y] = make([]bool, 20)
	}
	if err := s.extMgr.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "ext-walls",
		"gate_triggers": [{"zone_id": "w1", "behavior": "block"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	// Feet render at Position.Y + 1.0. Wall Y[5,6] expanded by 0.1 → [4.9,6.1].
	// Zero-length segment = point check at the feet.
	cases := []struct {
		name      string
		px, py    float32
		wantBlock bool
	}{
		{"feet above expanded wall (py=3.8 -> feet=4.8)", 5.5, 3.8, false},
		{"feet at expanded wall top edge (py=3.9 -> feet=4.9)", 5.5, 3.9, true},
		{"feet below expanded wall (py=5.4 -> feet=6.4)", 5.5, 5.4, false},
	}
	for _, c := range cases {
		got := s.movement.isMoveBlocked(s.zones["map1"], s.maps["map1"], c.px, c.py, c.px, c.py)
		if got != c.wantBlock {
			t.Errorf("%s: isMoveBlocked(%v, %v) = %v, want %v (feet at Y=%v, wall Y[5,6] expanded to [4.9,6.1])",
				c.name, c.px, c.py, got, c.wantBlock, c.py+1.0)
		}
	}
}

// TestIsMoveBlocked_WallsTileLayer_Symmetric verifies the Walls tile-layer
// fallback (md.IsBlocked) blocks symmetrically from all four directions.
//
// Coordinate convention (matches frontend GameScene.ts): the avatar sprite is
// 1 tile wide centered on Position.X, and the feet sit at Position.Y + 1.0
// (origin 0.5/0.75 on a 64px frame placed at (pos.X*32, pos.Y*32+16)). So
// Position.X = N is the LEFT edge of tile N, and feet at Position.Y+1 = M is
// the TOP edge of tile M. Tile index = floor(Position coord).
//
// A wall at tile W covers Position.X in [W, W+1) (X) / feet-Y in [W, W+1) (Y).
// The sprite's leading edge must stop at the wall boundary with no overlap:
//   - moving +X: stop at Position.X = W - 0.5 (right edge at W)
//   - moving -X: stop at Position.X = W + 1.5 (left edge at W+1)
//   - moving +Y: stop at Position.Y = W - 1   (feet at W)
//   - moving -Y: stop at Position.Y = W       (feet at W+1)
//
// The old code rounded with int(x+0.5) (always the +edge), which only checked
// the leading edge when moving in the + direction. Moving in - direction
// checked the trailing edge, letting the sprite tunnel ~1 tile into the wall.
func TestIsMoveBlocked_WallsTileLayer_Symmetric(t *testing.T) {
	// 12x12 map. Vertical wall at column 5 (all rows); horizontal wall at
	// row 5 (all columns). They don't intersect the test corridors (we move
	// along y=0 for the vertical wall, along x=0 for the horizontal wall).
	const w = 5
	mkWall := func(blocked [][2]int) [][]bool {
		g := make([][]bool, 12)
		for y := range g {
			g[y] = make([]bool, 12)
		}
		for _, p := range blocked {
			g[p[1]][p[0]] = true
		}
		return g
	}
	var vWall, hWall [][2]int
	for i := 0; i < 12; i++ {
		vWall = append(vWall, [2]int{w, i}) // column w, all rows
		hWall = append(hWall, [2]int{i, w}) // row w, all columns
	}
	vertical := &MapData{Width: 12, Height: 12, Collision: mkWall(vWall)}
	horizontal := &MapData{Width: 12, Height: 12, Collision: mkWall(hWall)}

	// Minimal simulator: nil zone registry means only the Walls tile-layer
	// fallback runs inside isMoveBlocked.
	s := &Simulator{
		movement: NewMovementSystem(nil), // extMgr unused when zr is nil
	}

	cases := []struct {
		name      string
		md        *MapData
		oldX, oldY, newX, newY float32
		wantBlock bool
	}{
		// Vertical wall at column 5 (covers Position.X [5,6)). Move along Y=0.
		{"+X: stop at wall (right edge at 5)", vertical, 4.0, 0, 4.5, 0, true},
		{"+X: just before wall (right edge at 4.9)", vertical, 4.0, 0, 4.4, 0, false},
		{"-X: stop at wall (left edge at 6)", vertical, 7.0, 0, 6.5, 0, false},
		{"-X: into wall (left edge at 5.9)", vertical, 7.0, 0, 6.4, 0, true},
		// Horizontal wall at row 5 (covers feet-Y [5,6), i.e. Position.Y [4,5)).
		{"+Y: stop at wall (feet at 5)", horizontal, 0, 3.0, 0, 4.0, true},
		{"+Y: just before wall (feet at 4.9)", horizontal, 0, 3.0, 0, 3.9, false},
		{"-Y: stop at wall (feet at 6)", horizontal, 0, 6.0, 0, 5.0, false},
		{"-Y: into wall (feet at 5.9)", horizontal, 0, 6.0, 0, 4.9, true},
	}
	for _, c := range cases {
		got := s.movement.isMoveBlocked(nil, c.md, c.oldX, c.oldY, c.newX, c.newY)
		if got != c.wantBlock {
			t.Errorf("%s: isMoveBlocked = %v, want %v", c.name, got, c.wantBlock)
		}
	}
}

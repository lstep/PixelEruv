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
	s := &Simulator{
		zones:  map[string]*ZoneRegistry{"map1": NewZoneRegistry(zones, 20, 20)},
		maps:   map[string]*MapData{"map1": {Width: 20, Height: 20, Collision: make([][]bool, 20)}},
		extMgr: NewExtensionManager(slog.Default()),
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
		got := s.isMoveBlocked(s.zones["map1"], s.maps["map1"], c.px, c.py, c.px, c.py)
		if got != c.wantBlock {
			t.Errorf("%s: isMoveBlocked(%v, %v) = %v, want %v (feet at Y=%v, wall Y[5,6] expanded to [4.9,6.1])",
				c.name, c.px, c.py, got, c.wantBlock, c.py+1.0)
		}
	}
}

package worldsim

import (
	"log/slog"
	"testing"
)

// TestIsPositionBlocked_FeetOffset verifies that zone collision is evaluated
// at the avatar's feet, not at Position.Y (the sprite origin/upper-body).
//
// The frontend renders avatars with origin (0.5, 0.75) on a 64px-tall frame
// placed at (pos.X*32+16, pos.Y*32+16) — see GameScene.ts. This puts the
// feet at continuous Y = Position.Y + 1.0 (bottom of the tile below
// Position). Collision must check the feet position, otherwise the player
// stops with feet buried in a wall (moving down) or stops a full tile short
// (moving up).
func TestIsPositionBlocked_FeetOffset(t *testing.T) {
	// Wall zone w1 covers continuous tile space X[5,6], Y[5,6].
	zones := []*Zone{{ID: "w1", Shape: ShapeRect, X: 5, Y: 5, W: 1, H: 1}}
	s := &Simulator{
		zoneReg: NewZoneRegistry(zones, 20, 20),
		extMgr:  NewExtensionManager(slog.Default()),
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

	// Feet render at Position.Y + 1.0. Wall covers Y[5,6].
	cases := []struct {
		name      string
		px, py    float32
		wantBlock bool
	}{
		{"feet above wall (py=3.6 -> feet=4.6)", 5.5, 3.6, false},
		{"feet at wall top edge (py=4.0 -> feet=5.0)", 5.5, 4.0, true},
		{"feet below wall (py=5.5 -> feet=6.5)", 5.5, 5.5, false},
	}
	for _, c := range cases {
		got := s.isPositionBlocked(c.px, c.py)
		if got != c.wantBlock {
			t.Errorf("%s: isPositionBlocked(%v, %v) = %v, want %v (feet at Y=%v, wall Y[5,6])",
				c.name, c.px, c.py, got, c.wantBlock, c.py+1.0)
		}
	}
}

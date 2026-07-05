package worldsim

import (
	"log/slog"
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestIsMoveBlocked_SubTileWall probes whether a wall zone thinner than a
// tile still blocks movement via swept (segment-vs-shape) collision. The
// wall is 0.2 tiles thick — well within the swept test's detection range
// (no point-sampling gap).
func TestIsMoveBlocked_SubTileWall(t *testing.T) {
	// Wall 0.2 tiles thick at feet-Y [5.0, 5.2], spanning X.
	zones := []*Zone{{ID: "thin", Shape: ShapeRect, X: 0, Y: 5, W: 20, H: 0.2}}
	s := &Simulator{
		zoneReg: NewZoneRegistry(zones, 20, 20),
		extMgr:  NewExtensionManager(slog.Default()),
	}
	if err := s.extMgr.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "ext-walls",
		"gate_triggers": [{"zone_id": "thin", "behavior": "block"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	// isMoveBlocked takes (oldX, oldY, newX, newY) in Position coords;
	// feet Y = Position.Y + 1.0. Wall covers feet-Y [5.0, 5.2].
	cases := []struct {
		name      string
		oldY, newY float32
		wantBlock bool
	}{
		{"segment below wall (feet 4.5->4.7)", 3.5, 3.7, false},
		{"segment ends at wall top (feet 4.7->5.0)", 3.7, 4.0, true},
		{"segment crosses wall (feet 4.8->5.5)", 3.8, 4.5, true},
		{"segment starts inside wall (feet 5.1->5.5)", 4.1, 4.5, true},
		{"segment above wall (feet 5.5->5.7)", 4.5, 4.7, false},
	}
	for _, c := range cases {
		got := s.isMoveBlocked(5.0, c.oldY, 5.0, c.newY)
		if got != c.wantBlock {
			t.Errorf("%s: isMoveBlocked(5.0, %v, 5.0, %v) = %v, want %v (feet %v->%v, wall [5.0,5.2])",
				c.name, c.oldY, c.newY, got, c.wantBlock, c.oldY+1.0, c.newY+1.0)
		}
	}
}

// TestTick_ThinWallBlocksMovement drives the real movement loop against a
// 0.1-tile-thick wall — thinner than the 0.4 tiles/tick movement distance.
// Swept (segment-vs-shape) collision must block the player from crossing;
// point-sampling at the destination would tunnel through.
func TestTick_ThinWallBlocksMovement(t *testing.T) {
	// Wall 0.1 tiles thick at feet-Y [5.05, 5.15], spanning X.
	zones := []*Zone{{ID: "razor", Shape: ShapeRect, X: 0, Y: 5.05, W: 20, H: 0.1}}
	s := &Simulator{
		zoneReg: NewZoneRegistry(zones, 20, 20),
		extMgr:  NewExtensionManager(slog.Default()),
		mapData: &MapData{Width: 20, Height: 20, Collision: make([][]bool, 20)},
	}
	for y := range s.mapData.Collision {
		s.mapData.Collision[y] = make([]bool, 20)
	}
	if err := s.extMgr.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "ext-walls",
		"gate_triggers": [{"zone_id": "razor", "behavior": "block"}]
	}`)); err != nil {
		t.Fatalf("RegisterTriggers: %v", err)
	}

	// Player at Position.Y = 4.2 (feet at 5.2, just below the wall at
	// 5.05-5.15). Pressing "up" moves -0.4 to Position.Y = 3.8 (feet 4.8,
	// above the wall). The movement segment in feet-space goes from
	// feet-Y 5.2 to 4.8 — crossing the wall [5.05, 5.15]. Point-sampling
	// at the destination (feet 4.8) would miss; swept must catch it.
	const startX = 5.0
	const startY = 4.2
	e := &Entity{
		ID:       "e_test",
		Position: &pb.Position{X: startX, Y: startY},
		NetworkSession: &NetworkSession{
			ClientID: "c_test",
			Input:    &pb.InputState{Up: true},
		},
		currentZones: make(map[string]bool),
	}
	s.entities = map[string]*Entity{"e_test": e}

	// Call the movement system directly (bypasses replication, which needs NATS).
	s.runMovementSystem()

	// The wall is at feet-Y [5.05, 5.15]. The player started at feet-Y 5.2
	// (just below the wall) and tried to move up. With point-sampling at the
	// destination (feet 4.8), the wall would be missed and the player would
	// tunnel to Position.Y = 3.8. Swept collision must prevent crossing.
	const tunnelDest = 3.8 // where point-sampling would let the player go
	if e.Position.Y <= tunnelDest+0.01 {
		t.Errorf("tunneled through 0.1-tile wall: Position.Y = %v (feet %v), "+
			"expected to be blocked above feet-Y 5.15 (Position.Y >= 4.15)",
			e.Position.Y, e.Position.Y+1.0)
	}
	// The player must not have crossed to the other side of the wall.
	if e.Position.Y+1.0 < 5.15 {
		t.Errorf("player crossed wall: feet-Y = %v, wall bottom at 5.15",
			e.Position.Y+1.0)
	}
}

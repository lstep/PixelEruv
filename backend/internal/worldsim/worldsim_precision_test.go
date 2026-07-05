package worldsim

import (
	"log/slog"
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestMovement_CornerSkipThinWall reproduces the corner-skip with a wall
// positioned where the player's diagonal movement crosses it but neither
// axis-separated segment does.
func TestMovement_CornerSkipThinWall(t *testing.T) {
	// Wall at X[5, 5.1], Y[5, 5.1] in zone coords. Player feet space = zone
	// space (we add avatarFeetYOffset to Position.Y). So we need the player's
	// FEET to cross [5, 5.1] x [5, 5.1] diagonally.
	// Player at Position (4.9, 3.9) → feet (4.9, 4.9). Moving right+down by
	// 0.283 → feet (5.183, 5.183). Diagonal crosses [5,5.1]x[5,5.1].
	zones := []*Zone{{ID: "corner", Shape: ShapeRect, X: 5, Y: 5, W: 0.1, H: 0.1}}
	s := newMovementSim(zones)
	e := &Entity{
		ID:       "e_test",
		Position: &pb.Position{X: 4.9, Y: 3.9}, // feet (4.9, 4.9)
		NetworkSession: &NetworkSession{
			ClientID: "c_test",
			Input:    &pb.InputState{Right: true, Down: true},
		},
		currentZones: make(map[string]bool),
	}
	s.entities = map[string]*Entity{"e_test": e}
	s.runMovementSystem()

	// Without the diagonal guard, the player tunnels to (5.183, 4.183)
	// (feet 5.183, 5.183) — past the wall. With the guard, the player must
	// not end up with feet inside or past the wall on both axes.
	feetX := e.Position.X
	feetY := e.Position.Y + 1.0
	// The player should not have crossed through the wall corner. If both
	// feet X and Y ended up > 5.1, the player skipped past the wall.
	if feetX > 5.1 && feetY > 5.1 {
		t.Errorf("corner-skip: player ended at feet (%v, %v), past wall [5,5.1]x[5,5.1] "+
			"— diagonal guard failed", feetX, feetY)
	}
}

// TestMovement_PlayerRadius verifies the player has a collision radius —
// the feet center should stop BEFORE reaching the wall edge, not at it.
// Without a radius, the sprite (1 tile wide) visually overlaps the wall by
// half before stopping.
func TestMovement_PlayerRadius(t *testing.T) {
	// Wall at X[5, 6], Y[0, 20] (a vertical wall the player approaches from
	// the left, moving right).
	zones := []*Zone{{ID: "vwall", Shape: ShapeRect, X: 5, Y: 0, W: 1, H: 20}}
	s := newMovementSim(zones)
	e := &Entity{
		ID:       "e_test",
		Position: &pb.Position{X: 4.5, Y: 5.0}, // feet (4.5, 6.0)
		NetworkSession: &NetworkSession{
			ClientID: "c_test",
			Input:    &pb.InputState{Right: true},
		},
		currentZones: make(map[string]bool),
	}
	s.entities = map[string]*Entity{"e_test": e}
	s.runMovementSystem()

	// Player moves right by 0.4 → X would be 4.9. With a radius r, the
	// player should stop at X = 5.0 - r (feet center can't get closer than
	// r to the wall). Without a radius, the player reaches X = 4.9 (feet
	// 0.1 from the wall) or even X = 5.0 (touching).
	const playerRadius = 0.3
	if e.Position.X > 5.0-playerRadius {
		t.Errorf("no collision radius: player X = %v (feet %v), wall at X=5.0, "+
			"expected to stop at X <= %v (wall edge minus radius %v)",
			e.Position.X, e.Position.X, 5.0-playerRadius, playerRadius)
	}
}

func newMovementSim(zones []*Zone) *Simulator {
	s := &Simulator{
		zoneReg: NewZoneRegistry(zones, 20, 20),
		extMgr:  NewExtensionManager(slog.Default()),
		mapData: &MapData{Width: 20, Height: 20, Collision: make([][]bool, 20)},
	}
	for y := range s.mapData.Collision {
		s.mapData.Collision[y] = make([]bool, 20)
	}
	s.extMgr.Register([]byte(`{"extension_id":"ext-walls","heartbeat_interval_s":10}`))
	s.extMgr.RegisterTriggers([]byte(`{
		"extension_id": "ext-walls",
		"gate_triggers": [{"zone_id": "corner", "behavior": "block"},
		                  {"zone_id": "vwall", "behavior": "block"},
		                  {"zone_id": "razor", "behavior": "block"}]
	}`))
	return s
}

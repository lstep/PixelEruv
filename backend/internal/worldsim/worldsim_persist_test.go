package worldsim

import (
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestCollectChangedPositionsLocked_SkipsUnchanged verifies the periodic
// position persister only collects players whose position or map differs from
// the last saved value, so idle players don't generate PocketBase writes.
func TestCollectChangedPositionsLocked_SkipsUnchanged(t *testing.T) {
	s := &Simulator{
		World: World{
			clients: make(map[string]*Entity),
		},
		lastSavedPos: make(map[string]savedPos),
	}

	// player-moved: position differs from last saved → should be collected.
	s.clients["c1"] = &Entity{
		ID:       "e1",
		Position: &pb.Position{X: 7, Y: 8, MapId: "main"},
	}
	s.lastSavedPos["e1"] = savedPos{x: 1, y: 1, mapID: "main"}

	// player-idle: identical to last saved → skipped.
	s.clients["c2"] = &Entity{
		ID:       "e2",
		Position: &pb.Position{X: 3, Y: 3, MapId: "main"},
	}
	s.lastSavedPos["e2"] = savedPos{x: 3, y: 3, mapID: "main"}

	// player-portal: same x/y but different map → should be collected.
	s.clients["c3"] = &Entity{
		ID:       "e3",
		Position: &pb.Position{X: 3, Y: 3, MapId: "castle"},
	}
	s.lastSavedPos["e3"] = savedPos{x: 3, y: 3, mapID: "main"}

	// player-never-saved: no lastSavedPos entry → should be collected.
	s.clients["c4"] = &Entity{
		ID:       "e4",
		Position: &pb.Position{X: 9, Y: 9, MapId: "main"},
	}

	got := s.collectChangedPositionsLocked()
	if len(got) != 3 {
		t.Fatalf("expected 3 changed positions, got %d: %+v", len(got), got)
	}

	gotByID := make(map[string]positionSave, len(got))
	for _, p := range got {
		gotByID[p.entityID] = p
	}
	for _, want := range []positionSave{
		{entityID: "e1", x: 7, y: 8, mapID: "main"},
		{entityID: "e3", x: 3, y: 3, mapID: "castle"},
		{entityID: "e4", x: 9, y: 9, mapID: "main"},
	} {
		if g, ok := gotByID[want.entityID]; !ok {
			t.Errorf("missing %q in collected saves", want.entityID)
		} else if g != want {
			t.Errorf("collected for %q = %+v, want %+v", want.entityID, g, want)
		}
	}
	if _, ok := gotByID["e2"]; ok {
		t.Errorf("idle player e2 should have been skipped")
	}
}

package worldsim

import (
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// newTestSimulator builds a Simulator with a couple of entities for
// unit-testing action dispatch helpers, without any NATS/PocketBase
// dependency (nc is left nil — not used by the functions under test).
func newTestSimulator() *Simulator {
	s := &Simulator{
		entities: make(map[string]*Entity),
		clients:  make(map[string]*Entity),
	}
	s.entities["player-1"] = &Entity{
		ID:       "player-1",
		Position: &pb.Position{X: 5, Y: 5},
	}
	s.entities["box-near"] = &Entity{
		ID:             "box-near",
		Position:       &pb.Position{X: 5.5, Y: 5},
		EntityType:     "light_switch",
		OwnerExtension: "ext-props",
	}
	s.entities["box-far"] = &Entity{
		ID:       "box-far",
		Position: &pb.Position{X: 9, Y: 9},
	}
	return s
}

func TestAdjacentEntitiesLocked_ExcludesSelfAndFarEntities(t *testing.T) {
	s := newTestSimulator()

	adjacent := s.adjacentEntitiesLocked("player-1", 5, 5)
	if len(adjacent) != 1 {
		t.Fatalf("expected 1 adjacent entity, got %d: %+v", len(adjacent), adjacent)
	}
	if adjacent[0].EntityID != "box-near" {
		t.Errorf("EntityID = %q, want box-near", adjacent[0].EntityID)
	}
	if adjacent[0].EntityType != "light_switch" || adjacent[0].OwnerExtension != "ext-props" {
		t.Errorf("adjacent entity info = %+v, missing type/owner", adjacent[0])
	}
}

func TestApplyActionReply_UpdatesStateAndQueuesAnimation(t *testing.T) {
	s := newTestSimulator()

	resp := &actionReplyMsg{Handled: true}
	resp.Updates = append(resp.Updates, struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	}{EntityID: "box-near", State: "on"})
	resp.Animations = append(resp.Animations, struct {
		EntityID    string `json:"entity_id"`
		AnimationID uint32 `json:"animation_id"`
	}{EntityID: "box-near", AnimationID: 3})

	s.applyActionReply(resp)

	e := s.entities["box-near"]
	if e.State != "on" || !e.dirtyState {
		t.Errorf("state = %q dirty=%v, want on/true", e.State, e.dirtyState)
	}
	if len(e.pendingAnimations) != 1 || e.pendingAnimations[0] != 3 {
		t.Errorf("pendingAnimations = %v, want [3]", e.pendingAnimations)
	}
}

package worldsim

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// fakePortalSink is a no-op PortalSink for portal transition tests.
type fakePortalSink struct{}

func (fakePortalSink) PublishMapTransition(context.Context, string, string, float32, float32, string, []*pb.MapWarning) {
}
func (fakePortalSink) SaveMapID(string, string) error { return nil }
func (fakePortalSink) EmitTransitionAudit(string, string, string, string, string, float32, float32) {
}

// TestPortalTransition_ClearsSpawnedToOnOtherEntities verifies that when a
// client transitions to a new map, the client's spawnedTo entry is cleared on
// every other entity. Without this, entities the client previously saw on a
// prior visit to the target map (props, other players) keep
// spawnedTo[clientID]=true, so the replication loop sends Updates instead of
// Spawns — and the client (which destroyed all avatars on the transition) has
// no avatar to apply the update to, so the entity never reappears.
func TestPortalTransition_ClearsSpawnedToOnOtherEntities(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	mu := &sync.Mutex{}
	portal := NewPortalSystem(fakePortalSink{}, logger, mu)

	map1 := &MapData{Width: 20, Height: 20, Collision: newCollisionGrid(20, 20)}
	map2 := &MapData{Width: 20, Height: 20, Collision: newCollisionGrid(20, 20)}

	// Player A (the transitioning client) starts on map1.
	a := &Entity{
		ID:       "ent-a",
		Position: &pb.Position{X: 5, Y: 5, MapId: "map1"},
		NetworkSession: &NetworkSession{
			ClientID: "client-a",
		},
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
	// Player B lives on map2. A visited map2 before, so B was spawned to A.
	b := &Entity{
		ID:       "ent-b",
		Position: &pb.Position{X: 6, Y: 6, MapId: "map2"},
		NetworkSession: &NetworkSession{
			ClientID: "client-b",
		},
		spawnedTo:    map[string]bool{"client-a": true, "client-b": true},
		currentZones: make(map[string]bool),
	}
	// A prop on map2 that was previously spawned to A.
	prop := &Entity{
		ID:           "prop-light",
		Position:     &pb.Position{X: 3, Y: 3, MapId: "map2"},
		spawnedTo:    map[string]bool{"client-a": true},
		currentZones: make(map[string]bool),
	}
	// An entity on map1 (the map A is leaving) that was spawned to A.
	c := &Entity{
		ID:       "ent-c",
		Position: &pb.Position{X: 7, Y: 7, MapId: "map1"},
		NetworkSession: &NetworkSession{
			ClientID: "client-c",
		},
		spawnedTo:    map[string]bool{"client-a": true, "client-c": true},
		currentZones: make(map[string]bool),
	}

	entities := map[string]*Entity{
		"ent-a": a, "ent-b": b, "prop-light": prop, "ent-c": c,
	}
	var destroyed []string
	pending := []portalTransitionReq{{entityID: "ent-a", targetMap: "map2"}}

	in := PortalInput{
		Entities:                 entities,
		Maps:                     map[string]*MapData{"map1": map1, "map2": map2},
		Zones:                    map[string]*ZoneRegistry{"map1": NewZoneRegistry(nil, 20, 20), "map2": NewZoneRegistry(nil, 20, 20)},
		RNG:                      rand.New(rand.NewPCG(1, 2)),
		PendingPortalTransitions: &pending,
		DestroyedEntities:        &destroyed,
	}

	portal.Step(context.Background(), in)

	// A's own spawnedTo was fully reset (existing behavior).
	if len(a.spawnedTo) != 0 {
		t.Errorf("transitioning entity spawnedTo should be reset, got %v", a.spawnedTo)
	}
	// A is now on map2.
	if a.Position.MapId != "map2" {
		t.Errorf("ent-a map = %q, want map2", a.Position.MapId)
	}
	// The client's spawnedTo entry must be cleared on every other entity so
	// replication re-spawns them for A on map2.
	if b.spawnedTo["client-a"] {
		t.Errorf("ent-b still has spawnedTo[client-a]=true; expected cleared so A gets a fresh SpawnEntity on map2")
	}
	if prop.spawnedTo["client-a"] {
		t.Errorf("prop-light still has spawnedTo[client-a]=true; expected cleared so A gets a fresh SpawnEntity on map2")
	}
	if c.spawnedTo["client-a"] {
		t.Errorf("ent-c still has spawnedTo[client-a]=true; expected cleared so A gets a fresh SpawnEntity if it returns to map1")
	}
	// Other clients' entries on those entities must be preserved.
	if !b.spawnedTo["client-b"] {
		t.Errorf("ent-b spawnedTo[client-b] should be preserved, got %v", b.spawnedTo)
	}
	if !c.spawnedTo["client-c"] {
		t.Errorf("ent-c spawnedTo[client-c] should be preserved, got %v", c.spawnedTo)
	}
}

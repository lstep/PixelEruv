package worldsim

import (
	"log/slog"
	"testing"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// newRaceTestSim builds a minimal Simulator wired to an embedded NATS,
// ready for reconnect-race tests. Returns the sim.
func newRaceTestSim(t *testing.T) *Simulator {
	t.Helper()
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:            map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities:         map[string]*Entity{},
			clients:          map[string]*Entity{},
			entityIDToClient: map[string]string{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()
	return sim
}

// makeRaceEntity builds a player entity with a mobile proximity zone, ready
// to insert into s.entities/s.clients. Mirrors what provisionClient does.
func makeRaceEntity(entityID, clientID string, x, y float32) *Entity {
	e := &Entity{
		ID:             entityID,
		Position:       &pb.Position{X: x, Y: y, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: clientID, Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	feetY := y + avatarFeetYOffset
	e.mobileZone = &Zone{
		ID:       "prox-" + entityID,
		Shape:    ShapeCircle,
		X:        x - proximityRadius,
		Y:        feetY - proximityRadius,
		W:        proximityRadius * 2,
		H:        proximityRadius * 2,
		Radius:   proximityRadius,
		Mobility: "mobile",
	}
	return e
}

// insertEntity registers an entity in s.entities, s.clients, s.entityIDToClient,
// and adds its mobile zone to the zone registry. Mirrors provisionClient's
// tail.
func insertEntity(sim *Simulator, e *Entity) {
	sim.entities[e.ID] = e
	sim.clients[e.NetworkSession.ClientID] = e
	sim.entityIDToClient[e.ID] = e.NetworkSession.ClientID
	if zr := sim.zones[e.Position.MapId]; zr != nil {
		zr.AddZone(e.mobileZone)
	}
}

// TestDespawnClient_ReconnectRace verifies that a late client.disconnected
// for the OLD session does not delete the NEW entity when a reconnect has
// already provisioned a fresh entity with the same persistent entityID.
//
// Sequence (laptop sleep/wake):
//  1. Old session provisions entity e_old at s.entities["e_user"]
//  2. New session provisions entity e_new, overwriting s.entities["e_user"]
//  3. Late client.disconnected for old session must NOT delete e_new
//
// Without the fix, step 3 deletes s.entities[e.ID] (which is now e_new),
// silently breaking replication, proximity A/V, and trigger visual feedback
// for the reconnected player.
func TestDespawnClient_ReconnectRace(t *testing.T) {
	sim := newRaceTestSim(t)

	const entityID = "e_user"
	const oldClientID = "c_old"
	const newClientID = "c_new"

	// Step 1: old session's entity.
	eOld := makeRaceEntity(entityID, oldClientID, 5, 5)
	insertEntity(sim, eOld)

	// Step 2: new session reconnects — same entityID, new clientID.
	// Simulate provisionClient's stale-zone cleanup + overwrite.
	eNew := makeRaceEntity(entityID, newClientID, 5, 5)
	if old, exists := sim.entities[entityID]; exists && old.mobileZone != nil {
		if zr := sim.zones["test-map"]; zr != nil {
			zr.RemoveZone(old.mobileZone.ID)
		}
	}
	insertEntity(sim, eNew)

	if sim.entities[entityID] != eNew {
		t.Fatal("setup: new entity should be in s.entities")
	}

	// Step 3: late disconnect for the old session.
	sim.despawnClient(t.Context(), oldClientID)

	// The new entity MUST still be in s.entities.
	eCurrent, ok := sim.entities[entityID]
	if !ok {
		t.Fatal("RACE: despawnClient(oldClientID) deleted the new entity from s.entities")
	}
	if eCurrent != eNew {
		t.Fatal("RACE: s.entities[entityID] is not the new entity after old despawn")
	}
	// New client mapping must be intact.
	if sim.clients[newClientID] != eNew {
		t.Fatal("new client mapping lost after old despawn")
	}
	if sim.entityIDToClient[entityID] != newClientID {
		t.Fatalf("entityIDToClient should be %q, got %q", newClientID, sim.entityIDToClient[entityID])
	}
	// Old client mapping must be gone.
	if _, ok := sim.clients[oldClientID]; ok {
		t.Fatal("old client mapping should be removed by despawn")
	}
	// No DestroyEntity should be queued for the new entity.
	for _, id := range sim.destroyedEntities {
		if id == entityID {
			t.Fatal("RACE: despawnClient(oldClientID) queued DestroyEntity for the new entity")
		}
	}
	// The new entity's mobile zone must still be in the registry.
	found := false
	for _, z := range sim.zones["test-map"].zones {
		if z.ID == "prox-"+entityID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RACE: new entity's mobile zone was removed from the registry")
	}
}

// TestDespawnClient_Normal verifies the non-race path: despawning a client
// whose entity is still the current one in s.entities removes everything.
func TestDespawnClient_Normal(t *testing.T) {
	sim := newRaceTestSim(t)

	const entityID = "e_user"
	const clientID = "c_1"

	e := makeRaceEntity(entityID, clientID, 5, 5)
	insertEntity(sim, e)

	sim.despawnClient(t.Context(), clientID)

	if _, ok := sim.entities[entityID]; ok {
		t.Fatal("entity should be removed from s.entities after normal despawn")
	}
	if _, ok := sim.clients[clientID]; ok {
		t.Fatal("client should be removed from s.clients after normal despawn")
	}
	if _, ok := sim.entityIDToClient[entityID]; ok {
		t.Fatal("entityIDToClient should be removed after normal despawn")
	}
	// DestroyEntity should be queued.
	found := false
	for _, id := range sim.destroyedEntities {
		if id == entityID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("DestroyEntity should be queued after normal despawn")
	}
	// Mobile zone should be gone.
	for _, z := range sim.zones["test-map"].zones {
		if z.ID == "prox-"+entityID {
			t.Fatal("mobile zone should be removed after normal despawn")
		}
	}
}

// TestDespawnClient_UnknownClient is a no-op guard: despawning a clientID
// that doesn't exist must not panic or mutate state.
func TestDespawnClient_UnknownClient(t *testing.T) {
	sim := newRaceTestSim(t)

	// Should not panic.
	sim.despawnClient(t.Context(), "c_nonexistent")

	if len(sim.entities) != 0 || len(sim.clients) != 0 {
		t.Fatal("despawn of unknown client should not mutate state")
	}
}

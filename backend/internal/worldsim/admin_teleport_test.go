package worldsim

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// newAdminTeleportTestSim builds a Simulator with two maps ("src-map" and
// "dst-map") so the worldsim.entity.teleport handler can transition a player
// between them. Returns the sim and the publish connection.
func newAdminTeleportTestSim(t *testing.T) (*Simulator, *nats.Conn) {
	t.Helper()
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	srcMap := &MapData{Width: 20, Height: 20, Collision: newCollisionGrid(20, 20)}
	dstMap := &MapData{Width: 20, Height: 20, Collision: newCollisionGrid(20, 20)}

	sim := &Simulator{
		World: World{
			zones: map[string]*ZoneRegistry{
				"src-map": NewZoneRegistry(nil, 20, 20),
				"dst-map": NewZoneRegistry(nil, 20, 20),
			},
			maps:     map[string]*MapData{"src-map": srcMap, "dst-map": dstMap},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
			rng:      rand.New(rand.NewPCG(1, 2)),
		},
		nc:         pubNc,
		defaultMap: "src-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()
	if err := sim.subscribeEntityTeleport(); err != nil {
		t.Fatalf("subscribeEntityTeleport: %v", err)
	}
	pubNc.Flush()
	return sim, pubNc
}

// publishAdminTeleport publishes a worldsim.entity.teleport request with the
// given sender/entity/map/x/y/exact fields and waits for the handler to run.
func publishAdminTeleport(t *testing.T, nc *nats.Conn, senderClientID, targetEntityID, mapID string, x, y float32, exact bool) {
	t.Helper()
	payload, _ := json.Marshal(entityTeleportRequest{
		SenderClientID: senderClientID,
		EntityID:       targetEntityID,
		MapID:          mapID,
		X:              x,
		Y:              y,
		ExactPosition:  exact,
	})
	if err := nc.Publish("worldsim.entity.teleport", payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	nc.Flush()
	time.Sleep(50 * time.Millisecond) // let the async handler run
}

// addAdminTeleportPlayer adds a player entity + client mapping on src-map.
func addAdminTeleportPlayer(sim *Simulator, entityID, clientID string, isAdmin bool, x, y float32) *Entity {
	e := &Entity{
		ID:             entityID,
		Position:       &pb.Position{X: x, Y: y, MapId: "src-map"},
		NetworkSession: &NetworkSession{ClientID: clientID, Input: &pb.InputState{}},
		IsAdmin:        isAdmin,
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities[entityID] = e
	sim.clients[clientID] = e
	return e
}

// TestAdminTeleport_AdminExactPosition verifies an admin sender can teleport
// another player to an exact x/y on a target map (the "teleport to me" path).
func TestAdminTeleport_AdminExactPosition(t *testing.T) {
	sim, nc := newAdminTeleportTestSim(t)
	addAdminTeleportPlayer(sim, "e_admin", "c_admin", true, 1, 1)
	target := addAdminTeleportPlayer(sim, "e_tgt", "c_tgt", false, 5, 5)

	publishAdminTeleport(t, nc, "c_admin", "e_tgt", "dst-map", 12.5, 7.5, true)

	if target.Position.MapId != "dst-map" {
		t.Errorf("target map = %q, want dst-map", target.Position.MapId)
	}
	if target.Position.X != 12.5 || target.Position.Y != 7.5 {
		t.Errorf("target position = (%v,%v), want (12.5,7.5)", target.Position.X, target.Position.Y)
	}
	if !target.dirtyPosition {
		t.Error("target.dirtyPosition = false, want true")
	}
}

// TestAdminTeleport_AdminRandomSpawn verifies an admin sender can teleport
// another player to a target map with exact_position=false (random spawn).
func TestAdminTeleport_AdminRandomSpawn(t *testing.T) {
	sim, nc := newAdminTeleportTestSim(t)
	addAdminTeleportPlayer(sim, "e_admin2", "c_admin2", true, 1, 1)
	target := addAdminTeleportPlayer(sim, "e_tgt2", "c_tgt2", false, 5, 5)

	publishAdminTeleport(t, nc, "c_admin2", "e_tgt2", "dst-map", 0, 0, false)

	if target.Position.MapId != "dst-map" {
		t.Errorf("target map = %q, want dst-map", target.Position.MapId)
	}
	// Random spawn on a 20x20 map with no blocked tiles → any tile in [0,20).
	if target.Position.X < 0 || target.Position.X >= 20 || target.Position.Y < 0 || target.Position.Y >= 20 {
		t.Errorf("target position = (%v,%v), want within map bounds", target.Position.X, target.Position.Y)
	}
}

// TestAdminTeleport_NonAdminRejected verifies a non-admin sender cannot
// teleport another player — the target stays put.
func TestAdminTeleport_NonAdminRejected(t *testing.T) {
	sim, nc := newAdminTeleportTestSim(t)
	addAdminTeleportPlayer(sim, "e_reg", "c_reg", false, 1, 1)
	target := addAdminTeleportPlayer(sim, "e_tgt3", "c_tgt3", false, 5, 5)

	publishAdminTeleport(t, nc, "c_reg", "e_tgt3", "dst-map", 12, 12, true)

	if target.Position.MapId != "src-map" {
		t.Errorf("non-admin teleport moved target to %q, want src-map (unchanged)", target.Position.MapId)
	}
	if target.Position.X != 5 || target.Position.Y != 5 {
		t.Errorf("non-admin teleport moved target to (%v,%v), want (5,5) unchanged", target.Position.X, target.Position.Y)
	}
}

// TestAdminTeleport_TrustedCallerNoSender verifies a request with no
// sender_client_id (trusted MCP/extension caller) bypasses the admin check
// and applies the teleport — preserving the original behavior.
func TestAdminTeleport_TrustedCallerNoSender(t *testing.T) {
	sim, nc := newAdminTeleportTestSim(t)
	target := addAdminTeleportPlayer(sim, "e_tgt4", "c_tgt4", false, 5, 5)

	publishAdminTeleport(t, nc, "", "e_tgt4", "dst-map", 3, 3, true)

	if target.Position.MapId != "dst-map" {
		t.Errorf("trusted teleport target map = %q, want dst-map", target.Position.MapId)
	}
	if target.Position.X != 3 || target.Position.Y != 3 {
		t.Errorf("trusted teleport target position = (%v,%v), want (3,3)", target.Position.X, target.Position.Y)
	}
}

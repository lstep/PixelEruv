package worldsim

import (
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestReapStaleClients_DespawnsOrphans verifies the client reaper removes
// player avatars whose pusher heartbeat has gone silent (e.g. pusher
// crash/restart or a lost client.disconnected), while leaving live players
// alone. Without the reaper these orphaned entities linger forever and inflate
// the /audit/world player count.
func TestReapStaleClients_DespawnsOrphans(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}

	now := time.Now()
	stale := &Entity{
		ID:             "e_stale",
		Position:       &pb.Position{X: 5, Y: 5, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: "c_stale", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
		lastHeartbeat:  now.Add(-2 * time.Minute), // silent well past the 90s timeout
	}
	sim.entities["e_stale"] = stale
	sim.clients["c_stale"] = stale

	fresh := &Entity{
		ID:             "e_fresh",
		Position:       &pb.Position{X: 10, Y: 10, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: "c_fresh", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
		lastHeartbeat:  now, // live
	}
	sim.entities["e_fresh"] = fresh
	sim.clients["c_fresh"] = fresh

	sim.reapStaleClients()

	if _, ok := sim.clients["c_stale"]; ok {
		t.Fatal("stale client c_stale was not reaped")
	}
	if _, ok := sim.entities["e_stale"]; ok {
		t.Fatal("stale entity e_stale was not reaped")
	}
	if _, ok := sim.clients["c_fresh"]; !ok {
		t.Fatal("fresh client c_fresh was wrongly reaped")
	}
	if _, ok := sim.entities["e_fresh"]; !ok {
		t.Fatal("fresh entity e_fresh was wrongly reaped")
	}
}

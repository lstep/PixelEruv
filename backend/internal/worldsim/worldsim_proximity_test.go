package worldsim

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestMobileZone_FollowsAvatar verifies that a player's mobile proximity zone
// follows the avatar's position each tick, and that zone.enter/zone.exit fire
// when another player crosses the proximity boundary.
func TestMobileZone_FollowsAvatar(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	// No static zones — only mobile proximity zones.
	sim := &Simulator{
		nc:      pubNc,
		mapID:   "test-map",
		zoneReg: NewZoneRegistry(nil, 20, 20),
		extMgr:  NewExtensionManager(logger),
		logger:  logger,
		tracer:  otel.Tracer("test"),
		entities: map[string]*Entity{},
		clients:  map[string]*Entity{},
	}

	// Player A at (5, 5), player B at (10, 10) — far apart.
	a := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	a.mobileZone = &Zone{
		ID:       "prox-e_a",
		Shape:    ShapeCircle,
		X:        5 - proximityRadius,
		Y:        5 - proximityRadius,
		W:        proximityRadius * 2,
		H:        proximityRadius * 2,
		Radius:   proximityRadius,
		Mobility: "mobile",
	}
	sim.zoneReg.AddZone(a.mobileZone)
	sim.entities["e_a"] = a
	sim.clients["c_a"] = a

	b := &Entity{
		ID:             "e_b",
		Position:       &pb.Position{X: 10, Y: 10},
		NetworkSession: &NetworkSession{ClientID: "c_b", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	b.mobileZone = &Zone{
		ID:       "prox-e_b",
		Shape:    ShapeCircle,
		X:        10 - proximityRadius,
		Y:        10 - proximityRadius,
		W:        proximityRadius * 2,
		H:        proximityRadius * 2,
		Radius:   proximityRadius,
		Mobility: "mobile",
	}
	sim.zoneReg.AddZone(b.mobileZone)
	sim.entities["e_b"] = b
	sim.clients["c_b"] = b

	type zoneEventPayload struct {
		EntityID string `json:"entity_id"`
		ClientID string `json:"client_id"`
		ZoneID   string `json:"zone_id"`
	}

	sub, err := subNc.SubscribeSync("zone.enter")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	// Tick 1: A and B are far apart — no proximity events.
	sim.tick()
	pubNc.Flush()
	// Drain any messages (shouldn't be any for prox- zones).
	for {
		_, err := sub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
	}

	// Move B next to A (within 2 tiles).
	b.Position.X = 6
	b.Position.Y = 5

	// Tick 2: B's feet (Y+1=6) are at (6, 6). A's mobile zone is centered at
	// (5, 5) with radius 2 → covers X[3,7], Y[3,7]. Point (6, 6) is inside.
	// B enters A's proximity zone → zone.enter for B into prox-e_a.
	sim.tick()
	pubNc.Flush()

	foundEnter := false
	deadline := time.Now().Add(2 * time.Second)
	for !foundEnter && time.Now().Before(deadline) {
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			t.Fatalf("expected zone.enter for B entering prox-e_a, got timeout")
		}
		var ev zoneEventPayload
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			continue
		}
		if ev.EntityID == "e_b" && ev.ZoneID == "prox-e_a" {
			foundEnter = true
			if ev.ClientID != "c_b" {
				t.Errorf("zone.enter client_id = %q, want c_b", ev.ClientID)
			}
		}
	}
	if !foundEnter {
		t.Fatal("B did not enter A's proximity zone")
	}

	// Move B far away again.
	b.Position.X = 15
	b.Position.Y = 15

	// Tick 3: B leaves A's proximity zone → zone.exit for B from prox-e_a.
	subExit, err := subNc.SubscribeSync("zone.exit")
	if err != nil {
		t.Fatalf("subscribe exit: %v", err)
	}
	subNc.Flush()

	sim.tick()
	pubNc.Flush()

	foundExit := false
	deadline = time.Now().Add(2 * time.Second)
	for !foundExit && time.Now().Before(deadline) {
		msg, err := subExit.NextMsg(500 * time.Millisecond)
		if err != nil {
			t.Fatalf("expected zone.exit for B leaving prox-e_a, got timeout")
		}
		var ev zoneEventPayload
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			continue
		}
		if ev.EntityID == "e_b" && ev.ZoneID == "prox-e_a" {
			foundExit = true
		}
	}
	if !foundExit {
		t.Fatal("B did not exit A's proximity zone")
	}
}

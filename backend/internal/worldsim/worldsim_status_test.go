package worldsim

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestHandleSetStatus verifies that handleSetStatus validates the enum range,
// updates Entity.Status, marks dirtyName (so the DisplayName component — which
// carries status — is re-replicated), and broadcasts the change on
// worldsim.player_status. Invalid values are rejected.
func TestHandleSetStatus(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

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

	a := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_a"] = a
	sim.clients["c_a"] = a

	statusSub, err := subNc.SubscribeSync("worldsim.player_status")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	// Valid status: DND (2).
	sim.handleSetStatus(context.Background(), "c_a", &pb.SetStatusFrame{Status: statusDoNotDisturb})
	pubNc.Flush()

	if a.Status != statusDoNotDisturb {
		t.Errorf("entity.Status = %d, want %d", a.Status, statusDoNotDisturb)
	}
	if !a.dirtyName {
		t.Error("dirtyName not set; status rides on the DisplayName component")
	}

	msg, err := statusSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected worldsim.player_status broadcast: %v", err)
	}
	var ev struct {
		EntityID string `json:"entity_id"`
		Status   uint32 `json:"status"`
	}
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.EntityID != "e_a" || ev.Status != statusDoNotDisturb {
		t.Errorf("player_status = {%s, %d}, want {e_a, %d}", ev.EntityID, ev.Status, statusDoNotDisturb)
	}

	// Invalid status (3) is rejected — no change, no broadcast.
	a.dirtyName = false
	sim.handleSetStatus(context.Background(), "c_a", &pb.SetStatusFrame{Status: 3})
	if a.Status != statusDoNotDisturb {
		t.Errorf("invalid status changed entity.Status to %d", a.Status)
	}
	if a.dirtyName {
		t.Error("invalid status should not set dirtyName")
	}
	if _, err := statusSub.NextMsg(100 * time.Millisecond); err == nil {
		t.Error("invalid status should not broadcast")
	}
}

// TestProximityClustering_DNDExcluded verifies that a DND player is excluded
// from proximity groups: when two players are near each other and one is DND,
// no proximity.join fires (the non-DND player is a singleton). Toggling the
// DND player back to Available re-includes them on the next clustering tick.
func TestProximityClustering_DNDExcluded(t *testing.T) {
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

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	a := makePlayer("e_a", "c_a", 5, 5)
	makePlayer("e_b", "c_b", 6, 5) // adjacent to A

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	subNc.Flush()

	// A is DND — excluded from proximity clustering. B is a singleton → no
	// group → no proximity.join for either.
	a.Status = statusDoNotDisturb
	sim.tick()
	pubNc.Flush()
	drainSync(joinSub, 200*time.Millisecond)
	if n, _ := joinSub.QueuedMsgs(); n != 0 {
		t.Fatalf("expected no proximity.join with DND player, got %d queued", n)
	}

	// A toggles back to Available — both join a group on the next clustering
	// tick (clustering runs every 5 ticks, so tick until it fires).
	a.Status = statusAvailable
	joins := map[string]bool{}
	deadline := time.Now().Add(3 * time.Second)
	for len(joins) < 2 && time.Now().Before(deadline) {
		sim.tick()
		pubNc.Flush()
		for {
			msg, err := joinSub.NextMsg(200 * time.Millisecond)
			if err != nil {
				break
			}
			var ev struct {
				EntityID string `json:"entity_id"`
			}
			json.Unmarshal(msg.Data, &ev)
			joins[ev.EntityID] = true
		}
	}
	if !joins["e_a"] || !joins["e_b"] {
		t.Fatalf("expected both e_a and e_b to join after DND cleared; got %v", joins)
	}
}

// drainSync drains any queued messages on a sync subscription within the
// given per-message timeout.
func drainSync(sub *nats.Subscription, timeout time.Duration) {
	for {
		if _, err := sub.NextMsg(timeout); err != nil {
			return
		}
	}
}

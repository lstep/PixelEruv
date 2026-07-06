package worldsim

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestDespawnClient_NotifiesOthersViaDestroyEntity is a regression test for the
// bug where closing a player's browser left their avatar visible on other
// players' screens. The cause: despawnClient removed the entity from the ECS
// but never queued a DestroyEntity for replication, so remaining clients never
// learned the entity was gone.
//
// This test builds a minimal Simulator with two player entities, marks A as
// already spawned to B, subscribes to B's replication NATS subject, despawns
// A, runs one tick, and asserts B receives a ReplicationBatch containing a
// DestroyEntity for A's entity ID.
func TestDespawnClient_NotifiesOthersViaDestroyEntity(t *testing.T) {
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
		nc:      pubNc,
		mapID:   "test-map",
		zoneReg: NewZoneRegistry(nil, 20, 20),
		extMgr:  NewExtensionManager(logger),
		logger:  logger,
		tracer:  otel.Tracer("test"),
		entities: map[string]*Entity{},
		clients:  map[string]*Entity{},
	}

	a := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_a"] = a
	sim.clients["c_a"] = a

	b := &Entity{
		ID:             "e_b",
		Position:       &pb.Position{X: 10, Y: 10},
		NetworkSession: &NetworkSession{ClientID: "c_b", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_b"] = b
	sim.clients["c_b"] = b

	// Pretend A was already spawned to B in a prior tick (so B's client has
	// the avatar on screen). This is the state that must be cleaned up.
	a.spawnedTo["c_b"] = true

	// Subscribe to B's replication subject to capture the next batch.
	got := make(chan *pb.ReplicationBatch, 1)
	sub, err := subNc.Subscribe("client.c_b.replication", func(m *nats.Msg) {
		var sf pb.ServerFrame
		if err := proto.Unmarshal(m.Data, &sf); err != nil {
			return
		}
		if batch := sf.GetReplication(); batch != nil {
			select {
			case got <- batch:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })
	if err := subNc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Despawn A (simulates the player closing their browser) and run one tick.
	sim.despawnClient(context.Background(), "c_a")
	sim.tick()

	select {
	case batch := <-got:
		found := false
		for _, d := range batch.Destroys {
			if d.EntityId == "e_a" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected DestroyEntity for e_a in batch, got %d destroys: %v",
				len(batch.Destroys), batch.Destroys)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch on client.c_b.replication")
	}
}

package worldsim

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestSetSpriteBase_UpdatesEntityAndReplicates verifies that handleSetSpriteBase
// updates the entity's SpriteBase, marks it dirty, and the next tick replicates
// an UpdateComponent (componentId=3) to other clients.
func TestSetSpriteBase_UpdatesEntityAndReplicates(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_alice", "c_alice", "Alice", "")
	addPlayer(sim, "e_bob", "c_bob", "Bob", "")

	// First tick: spawn Alice to Bob (so subsequent changes go via UpdateComponent).
	sim.tick()

	// Now change Alice's sprite_base.
	sim.handleSetSpriteBase(context.Background(), "c_alice", &pb.SetSpriteBaseFrame{SpriteBase: "sb_newlook"})

	if sim.entities["e_alice"].SpriteBase != "sb_newlook" {
		t.Fatalf("entity SpriteBase = %q, want %q",
			sim.entities["e_alice"].SpriteBase, "sb_newlook")
	}
	if !sim.entities["e_alice"].dirtyAppearance {
		t.Fatal("dirtyAppearance not set after handleSetSpriteBase")
	}

	// Subscribe to Bob's replication and tick to verify the update is sent.
	got := make(chan *pb.UpdateComponent, 1)
	sub, err := subNc.Subscribe("client.c_bob.replication", func(m *nats.Msg) {
		var sf pb.ServerFrame
		if err := proto.Unmarshal(m.Data, &sf); err != nil {
			return
		}
		if batch := sf.GetReplication(); batch != nil {
			for _, upd := range batch.Updates {
				if upd.EntityId == "e_alice" && upd.ComponentId == compAppearance {
					select {
					case got <- upd:
					default:
					}
				}
			}
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })
	subNc.Flush()

	sim.tick()

	select {
	case upd := <-got:
		var app pb.Appearance
		if err := proto.Unmarshal(upd.Data, &app); err != nil {
			t.Fatalf("unmarshal Appearance: %v", err)
		}
		if app.SpriteBase != "sb_newlook" {
			t.Fatalf("replicated SpriteBase = %q, want %q",
				app.SpriteBase, "sb_newlook")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UpdateComponent with Appearance")
	}
}

// TestSetSpriteBase_GuestUpdatesEntity verifies that a guest entity (no PB
// record) can still change its sprite_base in-session — the entity updates and
// replicates, but persistence is a no-op (UserStore is nil in test sim).
func TestSetSpriteBase_GuestUpdatesEntity(t *testing.T) {
	sim, _ := newChatTestSim(t)
	addPlayer(sim, "e_guest", "c_guest", "Guest abcd", "")

	sim.handleSetSpriteBase(context.Background(), "c_guest", &pb.SetSpriteBaseFrame{SpriteBase: "sb_guest"})

	if sim.entities["e_guest"].SpriteBase != "sb_guest" {
		t.Fatalf("guest entity SpriteBase = %q, want %q",
			sim.entities["e_guest"].SpriteBase, "sb_guest")
	}
}

// TestSetSpriteBase_UnknownClientRejected verifies that handleSetSpriteBase
// from an unknown client ID is rejected (no entity update, no panic).
func TestSetSpriteBase_UnknownClientRejected(t *testing.T) {
	sim, _ := newChatTestSim(t)
	addPlayer(sim, "e_alice", "c_alice", "Alice", "")

	sim.handleSetSpriteBase(context.Background(), "c_unknown", &pb.SetSpriteBaseFrame{SpriteBase: "sb_hack"})

	if sim.entities["e_alice"].SpriteBase != "" {
		t.Fatalf("entity SpriteBase = %q, want empty (unknown client should be rejected)",
			sim.entities["e_alice"].SpriteBase)
	}
}

// TestSetSpriteBase_EmptyRevertsToFallback verifies that sending an empty
// sprite_base clears the entity's SpriteBase (revert to fallback).
func TestSetSpriteBase_EmptyRevertsToFallback(t *testing.T) {
	sim, _ := newChatTestSim(t)
	e := addPlayer(sim, "e_alice", "c_alice", "Alice", "")
	e.SpriteBase = "sb_old"

	sim.handleSetSpriteBase(context.Background(), "c_alice", &pb.SetSpriteBaseFrame{SpriteBase: ""})

	if sim.entities["e_alice"].SpriteBase != "" {
		t.Fatalf("entity SpriteBase = %q, want empty (revert to fallback)",
			sim.entities["e_alice"].SpriteBase)
	}
}

package worldsim

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestReplication_SpawnIncludesSpriteBase verifies that when a player avatar
// spawns, the SpawnEntity sent to other clients includes an Appearance
// component (componentId=3) with the server-assigned SpriteBase. This
// ensures all clients render the same character sprite for the same player.
func TestReplication_SpawnIncludesSpriteBase(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_a", "c_a", "Alice", "")
	addPlayer(sim, "e_b", "c_b", "Bob", "")

	// Set a sprite_base on Alice's entity to verify it replicates.
	sim.entities["e_a"].SpriteBase = "sb_test123"

	// Subscribe to B's replication subject.
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
	subNc.Flush()

	sim.tick()

	select {
	case batch := <-got:
		found := false
		for _, sp := range batch.Spawns {
			if sp.EntityId != "e_a" {
				continue
			}
			for _, comp := range sp.Components {
				if comp.ComponentId != compAppearance {
					continue
				}
				var app pb.Appearance
				if err := proto.Unmarshal(comp.Data, &app); err != nil {
					t.Fatalf("unmarshal Appearance: %v", err)
				}
				if app.SpriteBase != "sb_test123" {
					t.Fatalf("replicated SpriteBase = %q, want %q",
						app.SpriteBase, "sb_test123")
				}
				found = true
			}
			if !found {
				t.Fatalf("SpawnEntity for e_a has no Appearance component, got %d components",
					len(sp.Components))
			}
		}
		if !found {
			t.Fatalf("expected SpawnEntity for e_a with Appearance, got %d spawns", len(batch.Spawns))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}
}

// TestReplication_SpawnAlwaysIncludesAppearanceForPlayers verifies that the
// Appearance component is always sent for player avatars, even when
// SpriteBase is empty (the guest case). Without this, the client would fall
// back to a client-side hash and could desync if the fallback differs.
func TestReplication_SpawnAlwaysIncludesAppearanceForPlayers(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_a", "c_a", "Alice", "")
	addPlayer(sim, "e_b", "c_b", "Bob", "")

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
	subNc.Flush()

	sim.tick()

	select {
	case batch := <-got:
		found := false
		for _, sp := range batch.Spawns {
			if sp.EntityId != "e_a" {
				continue
			}
			for _, comp := range sp.Components {
				if comp.ComponentId == compAppearance {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("SpawnEntity for player with empty SpriteBase has no Appearance component — client would desync")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}
}

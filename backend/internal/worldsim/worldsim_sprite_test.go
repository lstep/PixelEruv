package worldsim

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestSpriteIndex_Deterministic verifies that spriteIndexForEntity is
// deterministic — the same entity ID always maps to the same sprite index,
// and different entity IDs can map to different indices.
func TestSpriteIndex_Deterministic(t *testing.T) {
	idx1 := spriteIndexForEntity("e_abc123")
	idx2 := spriteIndexForEntity("e_abc123")
	if idx1 != idx2 {
		t.Fatalf("spriteIndexForEntity not deterministic: %d vs %d", idx1, idx2)
	}
	if idx1 >= charSpriteCount {
		t.Fatalf("sprite index %d >= charSpriteCount %d", idx1, charSpriteCount)
	}

	// At least two different entity IDs should produce different indices
	// (extremely unlikely for a hash to collide on all 5 values).
	found := make(map[uint32]bool)
	for _, id := range []string{"e_a", "e_b", "e_c", "e_d", "e_e", "e_f", "e_g", "e_h"} {
		found[spriteIndexForEntity(id)] = true
	}
	if len(found) < 2 {
		t.Fatalf("expected at least 2 distinct sprite indices, got %d", len(found))
	}
}

// TestReplication_SpawnIncludesSpriteIndex verifies that when a player avatar
// spawns, the SpawnEntity sent to other clients includes an Appearance
// component (componentId=3) with the server-assigned SpriteIndex. This
// ensures all clients render the same character sprite for the same player.
func TestReplication_SpawnIncludesSpriteIndex(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_a", "c_a", "Alice", "")
	addPlayer(sim, "e_b", "c_b", "Bob", "")

	expectedIdx := sim.entities["e_a"].SpriteIndex

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
				if app.SpriteIndex != expectedIdx {
					t.Fatalf("replicated SpriteIndex = %d, want %d",
						app.SpriteIndex, expectedIdx)
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
// SpriteIndex is 0 (which happens for 1 in 5 entity IDs). Without this, the
// client would fall back to a client-side counter and desync.
func TestReplication_SpawnAlwaysIncludesAppearanceForPlayers(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	// Find an entity ID whose sprite index is 0.
	var zeroID string
	for _, id := range []string{"e_0", "e_1", "e_2", "e_3", "e_4", "e_5", "e_6", "e_7", "e_8", "e_9"} {
		if spriteIndexForEntity(id) == 0 {
			zeroID = id
			break
		}
	}
	if zeroID == "" {
		t.Skip("could not find an entity ID with sprite index 0")
	}
	addPlayer(sim, zeroID, "c_a", "Alice", "")
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
			if sp.EntityId != zeroID {
				continue
			}
			for _, comp := range sp.Components {
				if comp.ComponentId == compAppearance {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("SpawnEntity for player with SpriteIndex=0 has no Appearance component — client would desync")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}
}

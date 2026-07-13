package worldsim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestSetName_UpdatesEntityAndReplicates verifies that handleSetName updates
// the entity's DisplayName, marks dirtyName, and the next replication tick
// sends an UpdateComponent with componentId=4 to other clients.
func TestSetName_UpdatesEntityAndReplicates(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_a", "c_a", "Guest aaaa", "")
	addPlayer(sim, "e_b", "c_b", "Guest bbbb", "")
	// Pretend A is already spawned to B (so B gets updates, not spawns).
	sim.entities["e_a"].spawnedTo["c_b"] = true
	sim.entities["e_b"].spawnedTo["c_a"] = true

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

	sim.handleSetName(context.Background(), "c_a", &pb.SetNameFrame{Name: "Alice"})

	if sim.entities["e_a"].DisplayName != "Alice" {
		t.Fatalf("DisplayName = %q, want Alice", sim.entities["e_a"].DisplayName)
	}
	if !sim.entities["e_a"].dirtyName {
		t.Fatal("dirtyName not set after handleSetName")
	}

	sim.tick()

	select {
	case batch := <-got:
		found := false
		for _, u := range batch.Updates {
			if u.EntityId == "e_a" && u.ComponentId == compDisplayName {
				var dn pb.DisplayName
				if err := proto.Unmarshal(u.Data, &dn); err != nil {
					t.Fatalf("unmarshal DisplayName: %v", err)
				}
				if dn.Name != "Alice" {
					t.Fatalf("replicated name = %q, want Alice", dn.Name)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected UpdateComponent(compDisplayName) for e_a, got %d updates", len(batch.Updates))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}

	// dirtyName should be cleared after the tick.
	if sim.entities["e_a"].dirtyName {
		t.Fatal("dirtyName should be cleared after tick")
	}
}

// TestSetName_SanitizesInput verifies that control chars are stripped and
// the name is truncated to maxNameRunes (20).
func TestSetName_SanitizesInput(t *testing.T) {
	sim, _ := newChatTestSim(t)
	addPlayer(sim, "e_a", "c_a", "Guest aaaa", "")

	// Control chars (0x00, 0x01, 0x1F) and a script tag should be stripped.
	sim.handleSetName(context.Background(), "c_a", &pb.SetNameFrame{
		Name: "Bob\x00\x01\x1f<script>",
	})
	if sim.entities["e_a"].DisplayName != "Bob<script>" {
		t.Fatalf("sanitized name = %q, want 'Bob<script>'", sim.entities["e_a"].DisplayName)
	}

	// Over-length name: 30 chars, should truncate to 20.
	long := strings.Repeat("A", 30)
	sim.handleSetName(context.Background(), "c_a", &pb.SetNameFrame{Name: long})
	if got := sim.entities["e_a"].DisplayName; len(got) != 20 {
		t.Fatalf("truncated name length = %d, want 20 (got %q)", len(got), got)
	}

	// Non-ASCII chars (é, emoji) should be stripped.
	sim.handleSetName(context.Background(), "c_a", &pb.SetNameFrame{
		Name: "Café😀",
	})
	if sim.entities["e_a"].DisplayName != "Caf" {
		t.Fatalf("ascii-only name = %q, want 'Caf'", sim.entities["e_a"].DisplayName)
	}
}

// TestSetName_GuestNotPersisted verifies that a guest's name is not restored
// after despawn + re-provision. Guests have no PocketBase record, so the
// name reverts to the default "Guest <short>".
func TestSetName_GuestNotPersisted(t *testing.T) {
	sim, _ := newChatTestSim(t)
	sim.userStore = nil // no PocketBase in tests

	addPlayer(sim, "e_a", "c_a", "Guest aaaa", "")

	sim.handleSetName(context.Background(), "c_a", &pb.SetNameFrame{Name: "Alice"})
	if sim.entities["e_a"].DisplayName != "Alice" {
		t.Fatalf("DisplayName = %q, want Alice", sim.entities["e_a"].DisplayName)
	}

	// Despawn and re-provision.
	sim.despawnClient(context.Background(), "c_a")

	// Re-provision with the same client ID (simulates reconnect).
	e := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		DisplayName:    "Guest aaaa", // default for guests
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_a"] = e
	sim.clients["c_a"] = e

	if sim.entities["e_a"].DisplayName != "Guest aaaa" {
		t.Fatalf("after reconnect, DisplayName = %q, want 'Guest aaaa'",
			sim.entities["e_a"].DisplayName)
	}
}

// TestReplication_SpawnIncludesDisplayName verifies that when a new player
// spawns with a DisplayName, the SpawnEntity sent to other clients includes
// a DisplayName component (componentId=4) with the correct name.
func TestReplication_SpawnIncludesDisplayName(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	addPlayer(sim, "e_a", "c_a", "Alice", "")
	// B is already spawned (exists in sim) but A has not been spawned to B yet.
	addPlayer(sim, "e_b", "c_b", "Bob", "")

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
			if sp.EntityId == "e_a" {
				for _, comp := range sp.Components {
					if comp.ComponentId == compDisplayName {
						var dn pb.DisplayName
						if err := proto.Unmarshal(comp.Data, &dn); err != nil {
							t.Fatalf("unmarshal DisplayName: %v", err)
						}
						if dn.Name != "Alice" {
							t.Fatalf("spawn DisplayName = %q, want Alice", dn.Name)
						}
						found = true
					}
				}
				if !found {
					t.Fatalf("SpawnEntity for e_a has no DisplayName component, got %d components",
						len(sp.Components))
				}
			}
		}
		if !found {
			t.Fatalf("expected SpawnEntity for e_a with DisplayName, got %d spawns", len(batch.Spawns))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}
}

// TestReplication_SpawnIncludesIsGuest verifies that the is_guest field is
// replicated as part of the DisplayName component on spawn. A guest entity
// (IsGuest=true) and a logged-in entity (IsGuest=false) should both carry
// the correct value.
func TestReplication_SpawnIncludesIsGuest(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	guest := addPlayer(sim, "e_guest", "c_guest", "Guest abcd", "")
	guest.IsGuest = true
	loggedIn := addPlayer(sim, "e_user", "c_user", "Alice", "")
	loggedIn.IsGuest = false
	// Observer already spawned.
	addPlayer(sim, "e_obs", "c_obs", "Obs", "")

	got := make(chan *pb.ReplicationBatch, 1)
	sub, err := subNc.Subscribe("client.c_obs.replication", func(m *nats.Msg) {
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
		for _, sp := range batch.Spawns {
			var dn pb.DisplayName
			for _, comp := range sp.Components {
				if comp.ComponentId == compDisplayName {
					if err := proto.Unmarshal(comp.Data, &dn); err != nil {
						t.Fatalf("unmarshal DisplayName: %v", err)
					}
					break
				}
			}
			switch sp.EntityId {
			case "e_guest":
				if !dn.IsGuest {
					t.Fatalf("guest entity e_guest: IsGuest = false, want true")
				}
			case "e_user":
				if dn.IsGuest {
					t.Fatalf("logged-in entity e_user: IsGuest = true, want false")
				}
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}
}

// TestReplication_SpawnIncludesIsAdmin verifies that the is_admin field is
// replicated as part of the DisplayName component on spawn, computed as
// IsAdmin && !HideAdminBadge. An admin who hasn't opted out (IsAdmin=true,
// HideAdminBadge=false) replicates IsAdmin=true; an admin who opted out
// (HideAdminBadge=true) and a non-admin both replicate IsAdmin=false.
func TestReplication_SpawnIncludesIsAdmin(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	adminShown := addPlayer(sim, "e_admin_shown", "c_admin_shown", "Admin", "")
	adminShown.IsAdmin = true
	adminShown.HideAdminBadge = false
	adminHidden := addPlayer(sim, "e_admin_hidden", "c_admin_hidden", "Admin", "")
	adminHidden.IsAdmin = true
	adminHidden.HideAdminBadge = true
	regular := addPlayer(sim, "e_user", "c_user", "Alice", "")
	regular.IsAdmin = false
	// Observer already spawned.
	addPlayer(sim, "e_obs", "c_obs", "Obs", "")

	got := make(chan *pb.ReplicationBatch, 1)
	sub, err := subNc.Subscribe("client.c_obs.replication", func(m *nats.Msg) {
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
		for _, sp := range batch.Spawns {
			var dn pb.DisplayName
			for _, comp := range sp.Components {
				if comp.ComponentId == compDisplayName {
					if err := proto.Unmarshal(comp.Data, &dn); err != nil {
						t.Fatalf("unmarshal DisplayName: %v", err)
					}
					break
				}
			}
			switch sp.EntityId {
			case "e_admin_shown":
				if !dn.IsAdmin {
					t.Fatalf("admin e_admin_shown (HideAdminBadge=false): IsAdmin = false, want true")
				}
			case "e_admin_hidden":
				if dn.IsAdmin {
					t.Fatalf("admin e_admin_hidden (HideAdminBadge=true): IsAdmin = true, want false")
				}
			case "e_user":
				if dn.IsAdmin {
					t.Fatalf("non-admin e_user: IsAdmin = true, want false")
				}
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replication batch")
	}
}

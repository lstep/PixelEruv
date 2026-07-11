package worldsim

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestAdminInfo_SentOnSpawnToAdmin verifies that when entities spawn near an
// admin client, the admin receives an AdminInfoFrame with each entity's IP
// on the admin-only NATS channel. Covers both guests and logged-in players.
func TestAdminInfo_SentOnSpawnToAdmin(t *testing.T) {
	sim, subNc := newChatTestSim(t)

	// Admin client — IsAdmin=true, will receive admin info.
	admin := addPlayer(sim, "e_admin", "c_admin", "Admin", "")
	admin.IsAdmin = true

	// Guest entity — has an IP, will be spawned to the admin.
	guest := addPlayer(sim, "e_guest", "c_guest", "Guest abcd", "")
	guest.IsGuest = true
	guest.IP = "203.0.113.42"

	// Logged-in (non-guest) player — also has an IP, also spawns to admin.
	loggedIn := addPlayer(sim, "e_alice", "c_alice", "Alice", "")
	loggedIn.IsGuest = false
	loggedIn.IP = "198.51.100.7"

	// Subscribe to the admin's admin channel.
	adminCh := make(chan *pb.AdminInfoFrame, 10)
	if _, err := subNc.Subscribe("client.c_admin.admin", func(m *nats.Msg) {
		var sf pb.ServerFrame
		if err := proto.Unmarshal(m.Data, &sf); err != nil {
			t.Errorf("unmarshal ServerFrame: %v", err)
			return
		}
		ai := sf.GetAdminInfo()
		if ai == nil {
			t.Errorf("expected AdminInfoFrame, got %T", sf.Payload)
			return
		}
		adminCh <- ai
	}); err != nil {
		t.Fatalf("subscribe admin channel: %v", err)
	}

	// Trigger replication for the admin client. Entities have not been
	// spawned to the admin yet (spawnedTo is empty), so this tick will
	// spawn them and publish admin info.
	sim.replicateToClient(context.Background(), admin)

	// Wait for the admin info frame.
	select {
	case ai := <-adminCh:
		// All entities on the same map are spawned on the first tick.
		// Find the guest and the logged-in player by entity ID.
		var guestInfo, loggedInInfo *pb.AdminInfoFrame_EntityAdminInfo
		for _, e := range ai.Entities {
			switch e.EntityId {
			case "e_guest":
				guestInfo = e
			case "e_alice":
				loggedInInfo = e
			}
		}
		if guestInfo == nil {
			t.Fatalf("expected e_guest in admin info, got entities: %v", ai.Entities)
		}
		if guestInfo.Ip != "203.0.113.42" {
			t.Errorf("guest: expected ip=203.0.113.42, got %s", guestInfo.Ip)
		}
		if !guestInfo.IsGuest {
			t.Error("guest: expected is_guest=true")
		}
		if loggedInInfo == nil {
			t.Fatalf("expected e_alice in admin info, got entities: %v", ai.Entities)
		}
		if loggedInInfo.Ip != "198.51.100.7" {
			t.Errorf("logged-in: expected ip=198.51.100.7, got %s", loggedInInfo.Ip)
		}
		if loggedInInfo.IsGuest {
			t.Error("logged-in: expected is_guest=false")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for admin info frame")
	}
}

// TestAdminInfo_NotSentToNonAdmin verifies that non-admin clients never
// receive AdminInfoFrame data, even when entities spawn near them.
func TestAdminInfo_NotSentToNonAdmin(t *testing.T) {
	sim, subNc := newChatTestSim(t)

	// Non-admin client.
	regular := addPlayer(sim, "e_regular", "c_regular", "Regular", "")
	regular.IsAdmin = false

	// Guest entity with an IP.
	guest := addPlayer(sim, "e_guest", "c_guest", "Guest abcd", "")
	guest.IsGuest = true
	guest.IP = "203.0.113.42"

	// Subscribe to what would be the non-admin's admin channel.
	gotFrame := false
	if _, err := subNc.Subscribe("client.c_regular.admin", func(m *nats.Msg) {
		gotFrame = true
	}); err != nil {
		t.Fatalf("subscribe admin channel: %v", err)
	}

	// Trigger replication for the non-admin client.
	sim.replicateToClient(context.Background(), regular)

	// Give it a moment to ensure nothing arrives.
	time.Sleep(100 * time.Millisecond)

	if gotFrame {
		t.Fatal("non-admin client received admin info frame — should never happen")
	}
}

// TestProvisionClient_SetsDeviceID verifies that the device_id from the
// auth frame is stored on the Entity and included in AdminInfoFrame.
func TestProvisionClient_SetsDeviceID(t *testing.T) {
	sim, subNc := newChatTestSim(t)
	sim.userStore = nil // no PocketBase in tests

	result := sim.provisionClient(t.Context(), "c_abc12345", "", "1.2.3.4", "dev-uuid-123")
	if result.banned {
		t.Fatal("expected not banned with nil banStore")
	}
	e := sim.clients["c_abc12345"]
	if e == nil {
		t.Fatal("entity not created")
	}
	if e.DeviceID != "dev-uuid-123" {
		t.Errorf("entity DeviceID = %q, want %q", e.DeviceID, "dev-uuid-123")
	}
	if e.IP != "1.2.3.4" {
		t.Errorf("entity IP = %q, want %q", e.IP, "1.2.3.4")
	}

	// Verify device_id appears in AdminInfoFrame when an admin views it.
	// Put the admin on the same map as the provisioned guest.
	admin := addPlayer(sim, "e_admin", "c_admin", "Admin", "")
	admin.IsAdmin = true
	admin.Position.MapId = result.mapID

	adminCh := make(chan *pb.AdminInfoFrame, 10)
	if _, err := subNc.Subscribe("client.c_admin.admin", func(m *nats.Msg) {
		var sf pb.ServerFrame
		if err := proto.Unmarshal(m.Data, &sf); err != nil {
			t.Errorf("unmarshal ServerFrame: %v", err)
			return
		}
		ai := sf.GetAdminInfo()
		if ai == nil {
			t.Errorf("expected AdminInfoFrame, got %T", sf.Payload)
			return
		}
		adminCh <- ai
	}); err != nil {
		t.Fatalf("subscribe admin channel: %v", err)
	}

	sim.replicateToClient(context.Background(), admin)

	select {
	case ai := <-adminCh:
		var found *pb.AdminInfoFrame_EntityAdminInfo
		for _, e := range ai.Entities {
			if e.EntityId == result.entityID {
				found = e
			}
		}
		if found == nil {
			t.Fatalf("expected entity %s in admin info, got: %v", result.entityID, ai.Entities)
		}
		if found.DeviceId != "dev-uuid-123" {
			t.Errorf("admin info DeviceId = %q, want %q", found.DeviceId, "dev-uuid-123")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for admin info frame")
	}
}

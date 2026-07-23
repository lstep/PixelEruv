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

// newTeleportTestSim builds a Simulator with a real WorldOptionsManager so the
// teleport_to_entity handler can read allow_player_teleport. Returns the sim
// and the publish connection.
func newTeleportTestSim(t *testing.T, allowPlayerTeleport bool) (*Simulator, *nats.Conn) {
	t.Helper()
	_, natsURL := startEmbeddedNATSWithJetStream(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	mgr, err := NewWorldOptionsManager(pubNc, logger, "h", "ws://h:7880")
	if err != nil {
		t.Fatalf("NewWorldOptionsManager: %v", err)
	}
	if err := mgr.Set(WorldOptions{
		SMTPHost:                 "mailhog",
		SMTPPort:                 1025,
		AppURL:                   "https://h",
		ErrorEmailRecipientsMode: "none",
		FFmpegConcurrency:        2,
		FFmpegTimeout:            10 * time.Minute,
		AllowPlayerTeleport:      allowPlayerTeleport,
	}); err != nil {
		t.Fatalf("Set world options: %v", err)
	}

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		worldOpts:  mgr,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()
	if err := sim.subscribeTeleportToEntity(); err != nil {
		t.Fatalf("subscribeTeleportToEntity: %v", err)
	}
	pubNc.Flush()
	return sim, pubNc
}

// addTeleportPlayer adds a player entity + client mapping to the sim.
func addTeleportPlayer(sim *Simulator, entityID, clientID string, isAdmin, isGuest bool, x, y float32) *Entity {
	e := &Entity{
		ID:             entityID,
		Position:       &pb.Position{X: x, Y: y, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: clientID, Input: &pb.InputState{}},
		IsAdmin:        isAdmin,
		IsGuest:        isGuest,
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities[entityID] = e
	sim.clients[clientID] = e
	return e
}

// publishTeleport publishes a teleport_to_entity request and waits for the
// handler to process it.
func publishTeleport(t *testing.T, nc *nats.Conn, senderClientID, targetEntityID string) {
	t.Helper()
	payload, _ := json.Marshal(teleportToRequest{SenderClientID: senderClientID, TargetEntityID: targetEntityID})
	if err := nc.Publish("worldsim.entity.teleport_to_entity", payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
	nc.Flush()
	time.Sleep(50 * time.Millisecond) // let the async handler run
}

// TestTeleportToEntity_Admin verifies an admin sender is moved to the target's
// exact position regardless of allow_player_teleport.
func TestTeleportToEntity_Admin(t *testing.T) {
	sim, nc := newTeleportTestSim(t, false)
	sender := addTeleportPlayer(sim, "e_admin", "c_admin", true, false, 1, 1)
	target := addTeleportPlayer(sim, "e_tgt", "c_tgt", false, false, 10, 20)

	publishTeleport(t, nc, "c_admin", "e_tgt")

	if sender.Position.X != 10 || sender.Position.Y != 20 {
		t.Errorf("admin sender position = (%v,%v), want (10,20)", sender.Position.X, sender.Position.Y)
	}
	if !sender.dirtyPosition {
		t.Error("sender.dirtyPosition = false, want true")
	}
	if target.Position.X != 10 || target.Position.Y != 20 {
		t.Errorf("target position changed = (%v,%v), want unchanged (10,20)", target.Position.X, target.Position.Y)
	}
}

// TestTeleportToEntity_RegisteredAllowed verifies a registered non-admin
// sender is moved when allow_player_teleport is on.
func TestTeleportToEntity_RegisteredAllowed(t *testing.T) {
	sim, nc := newTeleportTestSim(t, true)
	sender := addTeleportPlayer(sim, "e_reg", "c_reg", false, false, 1, 1)
	addTeleportPlayer(sim, "e_tgt2", "c_tgt2", false, false, 7, 8)

	publishTeleport(t, nc, "c_reg", "e_tgt2")

	if sender.Position.X != 7 || sender.Position.Y != 8 {
		t.Errorf("registered sender position = (%v,%v), want (7,8)", sender.Position.X, sender.Position.Y)
	}
}

// TestTeleportToEntity_RegisteredRejectedWhenOff verifies a registered
// non-admin sender is NOT moved when allow_player_teleport is off.
func TestTeleportToEntity_RegisteredRejectedWhenOff(t *testing.T) {
	sim, nc := newTeleportTestSim(t, false)
	sender := addTeleportPlayer(sim, "e_reg2", "c_reg2", false, false, 1, 1)
	addTeleportPlayer(sim, "e_tgt3", "c_tgt3", false, false, 7, 8)

	publishTeleport(t, nc, "c_reg2", "e_tgt3")

	if sender.Position.X != 1 || sender.Position.Y != 1 {
		t.Errorf("registered sender moved despite option off = (%v,%v), want (1,1)", sender.Position.X, sender.Position.Y)
	}
}

// TestTeleportToEntity_GuestRejected verifies a guest sender is never moved,
// even when allow_player_teleport is on.
func TestTeleportToEntity_GuestRejected(t *testing.T) {
	sim, nc := newTeleportTestSim(t, true)
	sender := addTeleportPlayer(sim, "e_guest", "c_guest", false, true, 1, 1)
	addTeleportPlayer(sim, "e_tgt4", "c_tgt4", false, false, 7, 8)

	publishTeleport(t, nc, "c_guest", "e_tgt4")

	if sender.Position.X != 1 || sender.Position.Y != 1 {
		t.Errorf("guest sender moved despite guest = (%v,%v), want (1,1)", sender.Position.X, sender.Position.Y)
	}
}

// TestTeleportToEntity_CrossMapRejected verifies a cross-map target is not
// applied (the sender stays put). The Players panel is same-map only, but the
// handler defends anyway.
func TestTeleportToEntity_CrossMapRejected(t *testing.T) {
	sim, nc := newTeleportTestSim(t, false)
	sender := addTeleportPlayer(sim, "e_admin2", "c_admin2", true, false, 1, 1)
	target := &Entity{
		ID:             "e_othermap",
		Position:       &pb.Position{X: 50, Y: 50, MapId: "other-map"},
		NetworkSession: &NetworkSession{ClientID: "c_othermap", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_othermap"] = target

	publishTeleport(t, nc, "c_admin2", "e_othermap")

	if sender.Position.X != 1 || sender.Position.Y != 1 {
		t.Errorf("cross-map teleport applied = (%v,%v), want (1,1)", sender.Position.X, sender.Position.Y)
	}
}

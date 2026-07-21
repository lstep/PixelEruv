package worldsim

import (
	"log/slog"
	"testing"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// TestClientKick_Despawn verifies worldsim.client.kick despawns the named
// client and emits a player.kicked audit event.
func TestClientKick_Despawn(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities:        map[string]*Entity{},
			clients:         map[string]*Entity{},
			entityIDToClient: map[string]string{},
		},
		nc:           nc,
		defaultMap:   "main",
		logger:       logger,
		tracer:       otel.Tracer("test"),
		lastSavedPos: map[string]savedPos{},
	}
	a := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5, MapId: "main"},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_a"] = a
	sim.clients["c_a"] = a
	sim.entityIDToClient["e_a"] = "c_a"

	auditSub, err := nc.SubscribeSync("audit.event")
	if err != nil {
		t.Fatalf("subscribe audit: %v", err)
	}
	nc.Flush()

	if err := sim.subscribeClientKick(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	body := mustJSON(t, kickRequest{
		ClientID: "c_a",
		Reason:   "test kick",
		adminActionRequest: adminActionRequest{
			Actor: audit.Actor{Extension: "mcp", Sub: "admin@test"},
		},
	})
	if _, err := nc.Request("worldsim.client.kick", body, 2*time.Second); err != nil {
		t.Fatalf("kick request: %v", err)
	}

	// Entity should be gone.
	if _, ok := sim.clients["c_a"]; ok {
		t.Error("client c_a still present after kick")
	}
	if _, ok := sim.entities["e_a"]; ok {
		t.Error("entity e_a still present after kick")
	}

	// Audit event should fire (player.despawned from despawnClient, then
	// player.kicked from the kick handler).
	ev1, err := auditSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected audit event: %v", err)
	}
	var ev audit.Event
	mustUnmarshal(t, ev1.Data, &ev)
	// Either despawned or kicked is fine for the first event; we just want
	// to see a kicked event eventually.
	var sawKicked bool
	for i := 0; i < 4; i++ {
		if ev.EventType == "player.kicked" {
			sawKicked = true
			if ev.Actor.Extension != "mcp" {
				t.Errorf("kicked actor.extension = %q, want mcp", ev.Actor.Extension)
			}
			break
		}
		m, err := auditSub.NextMsg(time.Second)
		if err != nil {
			break
		}
		mustUnmarshal(t, m.Data, &ev)
	}
	if !sawKicked {
		t.Error("did not see player.kicked audit event")
	}
}

// TestClientKick_NotConnected verifies kicking a non-connected client is a
// no-op that still emits an audit (with result=not_connected).
func TestClientKick_NotConnected(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, _ := nats.Connect(natsURL)
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{}, clients: map[string]*Entity{},
		},
		nc: nc, defaultMap: "main", logger: logger, tracer: otel.Tracer("test"),
	}
	if err := sim.subscribeClientKick(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	auditSub, _ := nc.SubscribeSync("audit.event")
	nc.Flush()

	if _, err := nc.Request("worldsim.client.kick",
		mustJSON(t, kickRequest{ClientID: "c_ghost"}), 2*time.Second); err != nil {
		t.Fatalf("request: %v", err)
	}

	msg, err := auditSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected audit: %v", err)
	}
	var ev audit.Event
	mustUnmarshal(t, msg.Data, &ev)
	if ev.EventType != "player.kick" {
		t.Errorf("event_type = %q, want player.kick", ev.EventType)
	}
	if ev.Details["result"] != "not_connected" {
		t.Errorf("details.result = %v, want not_connected", ev.Details["result"])
	}
}

// TestAdminSetName verifies worldsim.admin.set_name updates an entity's
// DisplayName, marks dirtyName, and sanitizes the name.
func TestAdminSetName(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, _ := nats.Connect(natsURL)
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{}, clients: map[string]*Entity{},
		},
		nc: nc, defaultMap: "main", logger: logger, tracer: otel.Tracer("test"),
	}
	sim.entities["e1"] = &Entity{
		ID:       "e1",
		Position: &pb.Position{X: 1, Y: 1, MapId: "main"},
	}
	if err := sim.subscribeAdminSetName(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	// Name with non-ASCII + over limit → sanitized to ASCII, truncated to 20.
	long := "Hello\x01World!ThisNameIsWayTooLongAndShouldBeTruncated"
	if _, err := nc.Request("worldsim.admin.set_name",
		mustJSON(t, adminSetNameRequest{EntityID: "e1", Name: long}), 2*time.Second); err != nil {
		t.Fatalf("request: %v", err)
	}

	got := sim.entities["e1"].DisplayName
	if len([]rune(got)) > maxNameRunes {
		t.Errorf("name not truncated: %q (len %d)", got, len([]rune(got)))
	}
	for _, r := range got {
		if r < 32 || r > 126 {
			t.Errorf("name contains non-ASCII-printable rune %d in %q", r, got)
		}
	}
	if !sim.entities["e1"].dirtyName {
		t.Error("dirtyName not set after admin set_name")
	}
}

// TestAdminSetStatus_Invalid verifies an out-of-range status is rejected
// without mutating the entity.
func TestAdminSetStatus_Invalid(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, _ := nats.Connect(natsURL)
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{}, clients: map[string]*Entity{},
		},
		nc: nc, defaultMap: "main", logger: logger, tracer: otel.Tracer("test"),
	}
	sim.entities["e1"] = &Entity{ID: "e1", Position: &pb.Position{X: 1, Y: 1, MapId: "main"}, Status: statusAvailable}
	if err := sim.subscribeAdminSetStatus(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	if _, err := nc.Request("worldsim.admin.set_status",
		mustJSON(t, adminSetStatusRequest{EntityID: "e1", Status: 5}), 2*time.Second); err != nil {
		t.Fatalf("request: %v", err)
	}
	if sim.entities["e1"].Status != statusAvailable {
		t.Errorf("invalid status mutated entity to %d", sim.entities["e1"].Status)
	}
}

// TestAdminChat_Global verifies worldsim.admin.chat on the global channel
// publishes a chat.broadcast message stamped with the entity's display name.
func TestAdminChat_Global(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, _ := nats.Connect(natsURL)
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities:        map[string]*Entity{},
			clients:         map[string]*Entity{},
			entityIDToClient: map[string]string{},
		},
		nc: nc, defaultMap: "main", logger: logger, tracer: otel.Tracer("test"),
	}
	sim.entities["e_npc"] = &Entity{
		ID:          "e_npc",
		DisplayName: "Narrator",
		Position:    &pb.Position{X: 1, Y: 1, MapId: "main"},
	}
	if err := sim.subscribeAdminChat(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	broadcastSub, _ := nc.SubscribeSync("chat.broadcast")
	nc.Flush()

	if _, err := nc.Request("worldsim.admin.chat",
		mustJSON(t, adminChatRequest{
			EntityID: "e_npc",
			Channel:  "global",
			Text:     "Welcome, travelers.",
			adminActionRequest: adminActionRequest{
				Actor: audit.Actor{Extension: "mcp"},
			},
		}), 2*time.Second); err != nil {
		t.Fatalf("request: %v", err)
	}

	msg, err := broadcastSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected chat.broadcast: %v", err)
	}
	// chat.broadcast carries a wire ServerFrame; we just check the bytes
	// contain the stamped display name and text. Full proto decode is
	// overkill here — the existing chat tests cover that path.
	s := string(msg.Data)
	if !contains(s, "Narrator") || !contains(s, "Welcome, travelers.") {
		t.Errorf("broadcast payload missing expected fields: %q", s)
	}
}

// TestAdminChat_UnknownChannel verifies an unknown channel is rejected
// (no broadcast).
func TestAdminChat_UnknownChannel(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, _ := nats.Connect(natsURL)
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{}, clients: map[string]*Entity{},
		},
		nc: nc, defaultMap: "main", logger: logger, tracer: otel.Tracer("test"),
	}
	sim.entities["e1"] = &Entity{ID: "e1", Position: &pb.Position{X: 1, Y: 1, MapId: "main"}}
	if err := sim.subscribeAdminChat(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	broadcastSub, _ := nc.SubscribeSync("chat.broadcast")
	nc.Flush()

	if _, err := nc.Request("worldsim.admin.chat",
		mustJSON(t, adminChatRequest{EntityID: "e1", Channel: "yell", Text: "hi"}), 2*time.Second); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := broadcastSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("expected no broadcast for unknown channel")
	}
}

// TestAdminChat_MissingEntity verifies admin chat on a non-existent entity
// is a no-op (no broadcast).
func TestAdminChat_MissingEntity(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	nc, _ := nats.Connect(natsURL)
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{}, clients: map[string]*Entity{},
		},
		nc: nc, defaultMap: "main", logger: logger, tracer: otel.Tracer("test"),
	}
	if err := sim.subscribeAdminChat(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	broadcastSub, _ := nc.SubscribeSync("chat.broadcast")
	nc.Flush()

	if _, err := nc.Request("worldsim.admin.chat",
		mustJSON(t, adminChatRequest{EntityID: "e_ghost", Channel: "global", Text: "boo"}), 2*time.Second); err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := broadcastSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("expected no broadcast for missing entity")
	}
}

// TestBanStoreAdd_InvalidTarget verifies Add rejects unknown target types.
func TestBanStoreAdd_InvalidTarget(t *testing.T) {
	s := &BanStore{} // app nil; we never reach the PB call
	if err := s.Add("bogus", "v", "r", 0, ""); err == nil {
		t.Error("expected error for invalid target_type")
	}
	if err := s.Add(BanTargetUserID, "", "r", 0, ""); err == nil {
		t.Error("expected error for empty target_value")
	}
}

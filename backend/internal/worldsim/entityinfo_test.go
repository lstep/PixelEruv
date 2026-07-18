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

// TestEntityInfo_RequestReply verifies that worldsim responds to
// worldsim.entity_info with the entity's is_admin, status, display_name, and
// map_id; that an admin entity reports is_admin=true; and that an unknown
// entity returns an empty EntityID so the caller can distinguish "not found"
// from a transport error.
func TestEntityInfo_RequestReply(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		nc:         pubNc,
		defaultMap: "test-map",
		zones:      map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
		entities:   map[string]*Entity{},
		clients:    map[string]*Entity{},
	}

	admin := &Entity{
		ID:          "e_admin",
		Position:    &pb.Position{X: 1, Y: 2, MapId: "test-map"},
		DisplayName: "Alice",
		IsAdmin:     true,
		Status:      statusAvailable,
	}
	regular := &Entity{
		ID:          "e_regular",
		Position:    &pb.Position{X: 3, Y: 4, MapId: "test-map"},
		DisplayName: "Bob",
		IsAdmin:     false,
		Status:      statusDoNotDisturb,
	}
	sim.entities["e_admin"] = admin
	sim.entities["e_regular"] = regular

	if err := sim.subscribeEntityInfo(); err != nil {
		t.Fatalf("subscribeEntityInfo: %v", err)
	}
	pubNc.Flush()

	// Admin entity.
	reply, err := pubNc.Request("worldsim.entity_info",
		mustMarshal(t, entityInfoMsg{EntityID: "e_admin"}), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var got entityInfoReply
	if err := json.Unmarshal(reply.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EntityID != "e_admin" || !got.IsAdmin || got.Status != statusAvailable ||
		got.DisplayName != "Alice" || got.MapID != "test-map" {
		t.Errorf("admin reply = %+v, want {e_admin true 0 Alice test-map}", got)
	}

	// Regular (non-admin) entity.
	reply, err = pubNc.Request("worldsim.entity_info",
		mustMarshal(t, entityInfoMsg{EntityID: "e_regular"}), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := json.Unmarshal(reply.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.IsAdmin {
		t.Errorf("regular entity is_admin = true, want false")
	}
	if got.Status != statusDoNotDisturb {
		t.Errorf("regular status = %d, want %d", got.Status, statusDoNotDisturb)
	}

	// Unknown entity — empty EntityID in reply.
	reply, err = pubNc.Request("worldsim.entity_info",
		mustMarshal(t, entityInfoMsg{EntityID: "e_missing"}), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := json.Unmarshal(reply.Data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EntityID != "" {
		t.Errorf("unknown entity reply EntityID = %q, want empty", got.EntityID)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

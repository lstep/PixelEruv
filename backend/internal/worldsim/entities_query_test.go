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

// TestEntitiesQuery_Filter verifies worldsim.entities.query filters by
// map_id, entity_type, owner_extension, and zone_id, and respects the limit.
func TestEntitiesQuery_Filter(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         nc,
		defaultMap: "main",
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	// Two maps, three base entities, two players.
	sim.entities["e_wall1"] = &Entity{
		ID:             "e_wall1",
		EntityType:     "wall",
		OwnerExtension: "ext-walls",
		Position:       &pb.Position{X: 1, Y: 1, MapId: "main"},
		State:          "off",
	}
	sim.entities["e_wall2"] = &Entity{
		ID:             "e_wall2",
		EntityType:     "wall",
		OwnerExtension: "ext-walls",
		Position:       &pb.Position{X: 2, Y: 2, MapId: "second"},
	}
	sim.entities["e_light1"] = &Entity{
		ID:             "e_light1",
		EntityType:     "light",
		OwnerExtension: "ext-props",
		Position:       &pb.Position{X: 3, Y: 3, MapId: "main"},
		LightIntensity: 50,
	}
	playerA := &Entity{
		ID:             "e_player_a",
		EntityType:     "",
		Position:       &pb.Position{X: 5, Y: 5, MapId: "main"},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		DisplayName:    "Alice",
		currentZones:   map[string]bool{"zone_meeting": true},
		spawnedTo:      make(map[string]bool),
	}
	sim.entities["e_player_a"] = playerA
	sim.clients["c_a"] = playerA

	if err := sim.subscribeEntitiesQuery(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	// Filter by map_id=main → wall1, light1, player_a (3).
	reply, err := nc.Request("worldsim.entities.query",
		mustJSON(t, entitiesQueryRequest{MapID: "main"}), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var got []entitySnapshot
	mustUnmarshal(t, reply.Data, &got)
	if len(got) != 3 {
		t.Fatalf("map=main: expected 3 entities, got %d", len(got))
	}

	// Filter by entity_type=wall → wall1, wall2 (2).
	reply, err = nc.Request("worldsim.entities.query",
		mustJSON(t, entitiesQueryRequest{EntityType: "wall"}), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	mustUnmarshal(t, reply.Data, &got)
	if len(got) != 2 {
		t.Fatalf("type=wall: expected 2, got %d", len(got))
	}

	// Filter by owner_extension=ext-props → light1 (1).
	reply, _ = nc.Request("worldsim.entities.query",
		mustJSON(t, entitiesQueryRequest{OwnerExtension: "ext-props"}), 2*time.Second)
	mustUnmarshal(t, reply.Data, &got)
	if len(got) != 1 || got[0].EntityID != "e_light1" {
		t.Fatalf("owner=ext-props: expected [e_light1], got %+v", got)
	}

	// Filter by zone_id=zone_meeting → player_a (1).
	reply, _ = nc.Request("worldsim.entities.query",
		mustJSON(t, entitiesQueryRequest{ZoneID: "zone_meeting"}), 2*time.Second)
	mustUnmarshal(t, reply.Data, &got)
	if len(got) != 1 || got[0].EntityID != "e_player_a" {
		t.Fatalf("zone=zone_meeting: expected [e_player_a], got %+v", got)
	}

	// Limit=2 on the full set → 2 results (sorted by entity_id).
	reply, _ = nc.Request("worldsim.entities.query",
		mustJSON(t, entitiesQueryRequest{Limit: 2}), 2*time.Second)
	mustUnmarshal(t, reply.Data, &got)
	if len(got) != 2 {
		t.Fatalf("limit=2: expected 2, got %d", len(got))
	}
	if got[0].EntityID >= got[1].EntityID {
		t.Errorf("expected sorted by entity_id, got %s before %s", got[0].EntityID, got[1].EntityID)
	}

	// Limit over the cap is clamped server-side; we asked for 2, got 2.
	// Verify the cap by requesting limit=10000 — should return at most
	// maxEntitiesQueryLimit (500), but with only 4 entities here, we get 4.
	reply, _ = nc.Request("worldsim.entities.query",
		mustJSON(t, entitiesQueryRequest{Limit: 10000}), 2*time.Second)
	mustUnmarshal(t, reply.Data, &got)
	if len(got) != 4 {
		t.Fatalf("limit=10000 with 4 entities: expected 4, got %d", len(got))
	}
}

// TestEntityGet verifies worldsim.entity.get returns a single entity by ID,
// or an error payload if not found.
func TestEntityGet(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	sim := &Simulator{
		World: World{
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         nc,
		defaultMap: "main",
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.entities["e1"] = &Entity{
		ID:         "e1",
		EntityType: "light",
		Position:   &pb.Position{X: 7, Y: 8, MapId: "main"},
		State:      "on",
	}
	if err := sim.subscribeEntitiesQuery(); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	nc.Flush()

	reply, err := nc.Request("worldsim.entity.get",
		mustJSON(t, struct{ EntityID string `json:"entity_id"` }{"e1"}), 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var snap entitySnapshot
	mustUnmarshal(t, reply.Data, &snap)
	if snap.EntityID != "e1" || snap.State != "on" || snap.X != 7 || snap.Y != 8 {
		t.Errorf("entity.get = %+v, want e1/on/7/8", snap)
	}

	// Not found → error payload.
	reply, _ = nc.Request("worldsim.entity.get",
		mustJSON(t, struct{ EntityID string `json:"entity_id"` }{"nope"}), 2*time.Second)
	var errResp map[string]any
	mustUnmarshal(t, reply.Data, &errResp)
	if errResp["error"] == nil {
		t.Errorf("expected error payload for missing entity, got %s", reply.Data)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", string(data), err)
	}
}

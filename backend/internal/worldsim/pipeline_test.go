package worldsim

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
	"go.opentelemetry.io/otel"
)

// recordingSink implements ZoneSink, ProximitySink, and ReplicationSink,
// recording the order of calls so tests can verify pipeline ordering.
type recordingSink struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingSink) record(call string) {
	r.mu.Lock()
	r.calls = append(r.calls, call)
	r.mu.Unlock()
}

func (r *recordingSink) calls_snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// ZoneSink
func (r *recordingSink) PublishZoneEvent(_ context.Context, event, entityID, clientID, zoneID, mapID string) {
	r.record("zone:" + event)
}

// ProximitySink
func (r *recordingSink) PublishProximityEvent(_ context.Context, event, entityID, clientID, groupID, mapID string, members []string) {
	r.record("proximity:" + event)
}

// ReplicationSink
func (r *recordingSink) PublishReplication(_ context.Context, clientID string, batch *pb.ReplicationBatch, spawned []*Entity, isAdmin bool) bool {
	r.record("replication:" + clientID)
	return true
}

// newCollisionGrid creates a w×h collision grid with no walls.
func newCollisionGrid(w, h int) [][]bool {
	grid := make([][]bool, h)
	for y := range grid {
		grid[y] = make([]bool, w)
	}
	return grid
}

// TestPipeline_Order verifies that the tick pipeline runs systems in the
// correct data-flow order: Movement → Zone → Proximity → Replication.
// The test uses a recording sink to capture the side-effect order.
func TestPipeline_Order(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sink := &recordingSink{}

	// Build a minimal world with two players on a 20x20 map.
	w := World{
		maps: map[string]*MapData{
			"map1": {Width: 20, Height: 20, Collision: newCollisionGrid(20, 20)},
		},
		zones: map[string]*ZoneRegistry{
			"map1": NewZoneRegistry(nil, 20, 20),
		},
		entities:         make(map[string]*Entity),
		clients:          make(map[string]*Entity),
		entityIDToClient: make(map[string]string),
	}

	// Player A at (5,5) with a mobile proximity zone.
	a := &Entity{
		ID:       "ent-a",
		Position: &pb.Position{X: 5, Y: 5, MapId: "map1"},
		NetworkSession: &NetworkSession{
			ClientID: "client-a",
			Input:    &pb.InputState{},
		},
		DisplayName:  "PlayerA",
		mobileZone:   &Zone{ID: "prox-ent-a", Shape: ShapeCircle, Radius: proximityRadius, X: 5 - proximityRadius, Y: 5 + avatarFeetYOffset - proximityRadius},
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
	w.entities["ent-a"] = a
	w.clients["client-a"] = a

	// Player B at (6,5) — within proximityRadius of A.
	b := &Entity{
		ID:       "ent-b",
		Position: &pb.Position{X: 6, Y: 5, MapId: "map1"},
		NetworkSession: &NetworkSession{
			ClientID: "client-b",
			Input:    &pb.InputState{},
		},
		DisplayName:  "PlayerB",
		mobileZone:   &Zone{ID: "prox-ent-b", Shape: ShapeCircle, Radius: proximityRadius, X: 6 - proximityRadius, Y: 5 + avatarFeetYOffset - proximityRadius},
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
	w.entities["ent-b"] = b
	w.clients["client-b"] = b

	// Add the mobile zones to the zone registry so zone detection sees them.
	w.zones["map1"].AddZone(a.mobileZone)
	w.zones["map1"].AddZone(b.mobileZone)

	// Mark both as stationary (stationaryTicks >= threshold) so proximity
	// can activate on the first proximity tick.
	a.stationaryTicks = proximityStationaryThreshold
	b.stationaryTicks = proximityStationaryThreshold

	// Build systems with the recording sink.
	movement := NewMovementSystem(NewExtensionManager(logger))
	zoneSys := NewZoneSystem(sink, logger)
	proxSys := NewProximitySystem(sink, logger)
	repSys := NewReplicationSystem(sink, otel.Tracer("test"))

	ctx := context.Background()

	// Run the locked phase in pipeline order, matching tick().
	w.Tick.SnapshotSeq++

	movement.Step(ctx, &w)
	zoneSys.Step(ctx, &w)
	// Proximity runs every 5th tick; TickCount%5 == 0 on first tick.
	proxSys.Step(ctx, &w)
	repSys.Step(ctx, &w)

	calls := sink.calls_snapshot()

	// Verify ordering: zone events must appear before proximity events,
	// which must appear before replication events.
	var firstZone, firstProx, firstRepl int = -1, -1, -1
	for i, c := range calls {
		if firstZone == -1 && len(c) > 5 && c[:5] == "zone:" {
			firstZone = i
		}
		if firstProx == -1 && len(c) > 10 && c[:10] == "proximity:" {
			firstProx = i
		}
		if firstRepl == -1 && len(c) > 12 && c[:12] == "replication:" {
			firstRepl = i
		}
	}

	// Zone events should have fired (both players enter each other's prox zones).
	if firstZone == -1 {
		t.Fatalf("expected zone events in pipeline, got calls: %v", calls)
	}
	// Proximity events should have fired (both players are stationary and adjacent).
	if firstProx == -1 {
		t.Fatalf("expected proximity events in pipeline, got calls: %v", calls)
	}
	// Replication should have fired for both clients.
	if firstRepl == -1 {
		t.Fatalf("expected replication events in pipeline, got calls: %v", calls)
	}

	if !(firstZone < firstProx) {
		t.Errorf("zone events (idx %d) must fire before proximity events (idx %d); calls: %v", firstZone, firstProx, calls)
	}
	if !(firstProx < firstRepl) {
		t.Errorf("proximity events (idx %d) must fire before replication (idx %d); calls: %v", firstProx, firstRepl, calls)
	}
}

// TestPipeline_MovementBeforeZone verifies that movement updates position
// before zone detection evaluates membership, so a player walking into a
// zone triggers zone.enter on the same tick.
func TestPipeline_MovementBeforeZone(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	sink := &recordingSink{}

	// Build a world with a rect zone at x=[8,12], y=[8,12].
	zr := NewZoneRegistry([]*Zone{
		{ID: "zone1", Shape: ShapeRect, X: 8, Y: 8, W: 4, H: 4},
	}, 20, 20)

	w := World{
		maps: map[string]*MapData{
			"map1": {Width: 20, Height: 20, Collision: newCollisionGrid(20, 20)},
		},
		zones: map[string]*ZoneRegistry{
			"map1": zr,
		},
		entities:         make(map[string]*Entity),
		clients:          make(map[string]*Entity),
		entityIDToClient: make(map[string]string),
	}

	// Player at (7,9) — just outside the zone (feet at 7,9+1=10; x=7 not in [8,12]).
	// With input Right, speed 0.4, new X = 7.4. Feet at x=7.4, still outside [8,12].
	// Let's place at (7.5, 9) so 7.5+0.4=7.9... still outside.
	// Place at (7.6, 9): 7.6+0.4=8.0 → feet x=8.0, which is in [8,12]. Zone enter!
	e := &Entity{
		ID:       "ent-a",
		Position: &pb.Position{X: 7.6, Y: 9, MapId: "map1"},
		NetworkSession: &NetworkSession{
			ClientID: "client-a",
			Input:    &pb.InputState{Right: true},
		},
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
	w.entities["ent-a"] = e
	w.clients["client-a"] = e

	movement := NewMovementSystem(NewExtensionManager(logger))
	zoneSys := NewZoneSystem(sink, logger)

	ctx := context.Background()
	w.Tick.SnapshotSeq++

	movement.Step(ctx, &w)
	zoneSys.Step(ctx, &w)

	// After movement, the player should have moved right into the zone.
	// Zone detection should have fired zone.enter.
	calls := sink.calls_snapshot()
	found := false
	for _, c := range calls {
		if c == "zone:zone.enter" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected zone.enter after movement moved player into zone; calls: %v; pos=%v", calls, e.Position)
	}

	// Verify the entity is now in the zone.
	if !e.currentZones["zone1"] {
		t.Errorf("expected entity to be in zone1 after movement+zone detection; currentZones=%v", e.currentZones)
	}
}

package worldsim

import (
	"context"
	"log/slog"
	"testing"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
	"go.opentelemetry.io/otel"
)

// batchSink captures replication batches per client so tests can inspect
// spawns, updates, and destroys.
type batchSink struct {
	batches map[string]*pb.ReplicationBatch // clientID -> last batch
}

func newBatchSink() *batchSink {
	return &batchSink{batches: make(map[string]*pb.ReplicationBatch)}
}

func (s *batchSink) PublishReplication(_ context.Context, clientID string, batch *pb.ReplicationBatch, _ []*Entity, _ bool) bool {
	s.batches[clientID] = batch
	return true
}

func (s *batchSink) batch(clientID string) *pb.ReplicationBatch {
	return s.batches[clientID]
}

func (s *batchSink) spawnIDs(clientID string) []string {
	b := s.batches[clientID]
	if b == nil {
		return nil
	}
	ids := make([]string, 0, len(b.Spawns))
	for _, sp := range b.Spawns {
		ids = append(ids, sp.EntityId)
	}
	return ids
}

func (s *batchSink) destroyIDs(clientID string) []string {
	b := s.batches[clientID]
	if b == nil {
		return nil
	}
	ids := make([]string, 0, len(b.Destroys))
	for _, d := range b.Destroys {
		ids = append(ids, d.EntityId)
	}
	return ids
}

func (s *batchSink) reset() {
	s.batches = make(map[string]*pb.ReplicationBatch)
}

// makeAOIClientEntity creates a player entity at the given position with a
// NetworkSession and initialized spawnedTo map.
func makeAOIClientEntity(id, clientID, mapID string, x, y float32) *Entity {
	return &Entity{
		ID:             id,
		Position:       &pb.Position{X: x, Y: y, MapId: mapID},
		NetworkSession: &NetworkSession{ClientID: clientID, Input: &pb.InputState{}},
		DisplayName:    id,
		spawnedTo:      make(map[string]bool),
		currentZones:   make(map[string]bool),
	}
}

// makeAOIEntity creates a non-client entity (no NetworkSession) at the given
// position, with initialized spawnedTo.
func makeAOIEntity(id, mapID string, x, y float32) *Entity {
	return &Entity{
		ID:           id,
		Position:     &pb.Position{X: x, Y: y, MapId: mapID},
		DisplayName:  id,
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
}

// buildAOIWorld creates a World with entities and an AOI grid rebuilt from
// their positions. Returns the world and the replication system.
func buildAOIWorld(entities map[string]*Entity, logger *slog.Logger) (*World, *ReplicationSystem, *batchSink) {
	w := &World{
		entities:         entities,
		clients:          make(map[string]*Entity),
		entityIDToClient: make(map[string]string),
	}
	for _, e := range entities {
		if e.NetworkSession != nil {
			w.clients[e.NetworkSession.ClientID] = e
			w.entityIDToClient[e.ID] = e.NetworkSession.ClientID
		}
	}
	sink := newBatchSink()
	repSys := NewReplicationSystem(sink, otel.Tracer("test"))
	return w, repSys, sink
}

// runAOIReplication rebuilds the AOI grid from the world's entities and runs
// one replication tick.
func runAOIReplication(t *testing.T, w *World, repSys *ReplicationSystem) {
	t.Helper()
	// Rebuild AOI grids (mirrors Simulator.rebuildAOIGrids).
	w.aoiGrids = make(map[string]*AOIGrid)
	for _, e := range w.entities {
		if e.Position == nil {
			continue
		}
		g, ok := w.aoiGrids[e.Position.MapId]
		if !ok {
			g = NewAOIGrid(aoiCellSize)
			w.aoiGrids[e.Position.MapId] = g
		}
		g.Insert(e)
	}
	w.Tick.SnapshotSeq++
	destroyed := []string{}
	repSys.Step(context.Background(), ReplicationInput{
		Entities:          w.entities,
		TickSnapshotSeq:   w.Tick.SnapshotSeq,
		DestroyedEntities: &destroyed,
		AOIGrids:          w.aoiGrids,
	})
}

// containsID checks if a string slice contains a given ID.
func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// TestAOI_BasicFiltering verifies that an entity far outside the client's AOI
// is not replicated (no SpawnEntity), while a nearby entity is.
func TestAOI_BasicFiltering(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Client at (0, 0). Near entity at (1, 1) — same cell, well within AOI.
	// Far entity at (500, 500) — ~31 cells away, far outside unsubscribe radius.
	client := makeAOIClientEntity("ent-client", "client-1", "map1", 0, 0)
	near := makeAOIEntity("ent-near", "map1", 1, 1)
	far := makeAOIEntity("ent-far", "map1", 500, 500)

	entities := map[string]*Entity{
		client.ID: client,
		near.ID:   near,
		far.ID:    far,
	}

	w, repSys, sink := buildAOIWorld(entities, logger)
	runAOIReplication(t, w, repSys)

	spawns := sink.spawnIDs("client-1")
	if !containsID(spawns, "ent-near") {
		t.Errorf("expected ent-near in spawns (within AOI), got %v", spawns)
	}
	if containsID(spawns, "ent-far") {
		t.Errorf("ent-far should NOT be in spawns (outside AOI), got %v", spawns)
	}
	// Client should not have ent-far in spawnedTo.
	if far.spawnedTo["client-1"] {
		t.Errorf("ent-far should not be marked spawnedTo client-1 (outside AOI)")
	}
}

// TestAOI_BoundaryCrossing verifies that an entity entering the client's AOI
// gets a SpawnEntity, and an entity leaving gets a DestroyEntity.
func TestAOI_BoundaryCrossing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Client at (0, 0). Entity starts within AOI at (1, 1).
	client := makeAOIClientEntity("ent-client", "client-1", "map1", 0, 0)
	other := makeAOIEntity("ent-other", "map1", 1, 1)

	entities := map[string]*Entity{
		client.ID: client,
		other.ID:  other,
	}

	w, repSys, sink := buildAOIWorld(entities, logger)

	// Tick 1: entity is within AOI — should be spawned.
	runAOIReplication(t, w, repSys)
	spawns := sink.spawnIDs("client-1")
	if !containsID(spawns, "ent-other") {
		t.Fatalf("tick 1: expected ent-other in spawns, got %v", spawns)
	}
	if !other.spawnedTo["client-1"] {
		t.Fatalf("tick 1: ent-other should be spawnedTo client-1")
	}

	// Move entity far away (outside unsubscribe radius = 4 cells = 64 tiles).
	sink.reset()
	other.Position.X = 500
	other.Position.Y = 500

	// Tick 2: entity is outside AOI — should be destroyed.
	runAOIReplication(t, w, repSys)
	destroys := sink.destroyIDs("client-1")
	if !containsID(destroys, "ent-other") {
		t.Errorf("tick 2: expected ent-other in destroys, got %v", destroys)
	}
	if other.spawnedTo["client-1"] {
		t.Errorf("tick 2: ent-other should NOT be spawnedTo client-1 after destroy")
	}

	// Move entity back within AOI.
	sink.reset()
	other.Position.X = 1
	other.Position.Y = 1

	// Tick 3: entity re-enters AOI — should be spawned again.
	runAOIReplication(t, w, repSys)
	spawns = sink.spawnIDs("client-1")
	if !containsID(spawns, "ent-other") {
		t.Errorf("tick 3: expected ent-other in spawns on re-entry, got %v", spawns)
	}
}

// TestAOI_Hysteresis verifies that an entity in the hysteresis band (between
// subscribe and unsubscribe radius) stays spawned but does not get newly
// spawned, and that oscillating across the boundary does not cause
// spawn/despawn storms.
func TestAOI_Hysteresis(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// With cellSize=16, subscribeRadius=3 (48 tiles), unsubscribeRadius=4 (64 tiles).
	// Place entity at 50 tiles away: cell (3,0), distance 3 cells from client cell (0,0).
	// This is within subscribe radius (3) — should spawn on first tick.
	// Then move to 60 tiles: cell (3,0), still within subscribe radius.
	// Then move to 70 tiles: cell (4,0), within unsubscribe (4) but outside subscribe (3).
	// Entity should STAY spawned (hysteresis) — no destroy, no re-spawn.
	client := makeAOIClientEntity("ent-client", "client-1", "map1", 0, 0)
	other := makeAOIEntity("ent-other", "map1", 50, 0)

	entities := map[string]*Entity{
		client.ID: client,
		other.ID:  other,
	}

	w, repSys, sink := buildAOIWorld(entities, logger)

	// Tick 1: entity at 50 tiles (cell 3, within subscribe radius 3) — spawn.
	runAOIReplication(t, w, repSys)
	spawns := sink.spawnIDs("client-1")
	if !containsID(spawns, "ent-other") {
		t.Fatalf("tick 1: expected ent-other in spawns (within subscribe radius), got %v", spawns)
	}
	if !other.spawnedTo["client-1"] {
		t.Fatalf("tick 1: ent-other should be spawnedTo client-1")
	}

	// Move to 70 tiles (cell 4, within unsubscribe but outside subscribe).
	// Entity should STAY spawned — hysteresis band.
	sink.reset()
	other.Position.X = 70
	runAOIReplication(t, w, repSys)

	destroys := sink.destroyIDs("client-1")
	if containsID(destroys, "ent-other") {
		t.Errorf("tick 2: ent-other should NOT be destroyed (hysteresis band), destroys: %v", destroys)
	}
	if !other.spawnedTo["client-1"] {
		t.Errorf("tick 2: ent-other should still be spawnedTo client-1 (hysteresis)")
	}
	// Should not get a re-spawn (already spawned).
	spawns = sink.spawnIDs("client-1")
	if containsID(spawns, "ent-other") {
		t.Errorf("tick 2: ent-other should NOT re-spawn (already spawned), spawns: %v", spawns)
	}

	// Move back to 50 tiles (cell 3, within subscribe). Still spawned, no change.
	sink.reset()
	other.Position.X = 50
	runAOIReplication(t, w, repSys)
	destroys = sink.destroyIDs("client-1")
	if containsID(destroys, "ent-other") {
		t.Errorf("tick 3: ent-other should NOT be destroyed (back within subscribe), destroys: %v", destroys)
	}
	spawns = sink.spawnIDs("client-1")
	if containsID(spawns, "ent-other") {
		t.Errorf("tick 3: ent-other should NOT re-spawn (already spawned), spawns: %v", spawns)
	}

	// Move to 100 tiles (cell 6, outside unsubscribe radius 4) — destroy.
	sink.reset()
	other.Position.X = 100
	runAOIReplication(t, w, repSys)
	destroys = sink.destroyIDs("client-1")
	if !containsID(destroys, "ent-other") {
		t.Errorf("tick 4: expected ent-other in destroys (outside unsubscribe), got %v", destroys)
	}
	if other.spawnedTo["client-1"] {
		t.Errorf("tick 4: ent-other should NOT be spawnedTo client-1 after destroy")
	}
}

// TestAOI_FallbackNoGrid verifies that when no AOI grid is provided, the
// replication system falls back to whole-map replication (pre-AOI behavior).
func TestAOI_FallbackNoGrid(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	client := makeAOIClientEntity("ent-client", "client-1", "map1", 0, 0)
	far := makeAOIEntity("ent-far", "map1", 500, 500)

	entities := map[string]*Entity{
		client.ID: client,
		far.ID:    far,
	}

	w, repSys, sink := buildAOIWorld(entities, logger)

	// Run replication WITHOUT building AOI grids (nil AOIGrids).
	w.Tick.SnapshotSeq++
	destroyed := []string{}
	repSys.Step(context.Background(), ReplicationInput{
		Entities:          w.entities,
		TickSnapshotSeq:   w.Tick.SnapshotSeq,
		DestroyedEntities: &destroyed,
		AOIGrids:          nil, // no grid — fallback to whole-map
	})

	spawns := sink.spawnIDs("client-1")
	if !containsID(spawns, "ent-far") {
		t.Errorf("fallback: expected ent-far in spawns (no AOI grid = whole-map), got %v", spawns)
	}
}

// TestAOI_ClientAlwaysSeesSelf verifies the client's own entity is always
// replicated regardless of AOI (the player always sees themselves).
func TestAOI_ClientAlwaysSeesSelf(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	client := makeAOIClientEntity("ent-client", "client-1", "map1", 0, 0)

	entities := map[string]*Entity{
		client.ID: client,
	}

	w, repSys, sink := buildAOIWorld(entities, logger)
	runAOIReplication(t, w, repSys)

	spawns := sink.spawnIDs("client-1")
	if !containsID(spawns, "ent-client") {
		t.Errorf("client should always see self in spawns, got %v", spawns)
	}
}

// TestAOIGrid_BasicInsert tests the AOIGrid struct directly.
func TestAOIGrid_BasicInsert(t *testing.T) {
	g := NewAOIGrid(16)

	e1 := makeAOIEntity("e1", "map1", 0, 0)
	e2 := makeAOIEntity("e2", "map1", 17, 0) // cell (1,0)
	e3 := makeAOIEntity("e3", "map1", 0, 17) // cell (0,1)

	g.Insert(e1)
	g.Insert(e2)
	g.Insert(e3)

	// Query radius 0: only same cell.
	result := g.EntitiesInRadius(&pb.Position{X: 0, Y: 0, MapId: "map1"}, 0)
	if len(result) != 1 || result["e1"] == nil {
		t.Errorf("radius 0 from (0,0): expected only e1, got %d entities", len(result))
	}

	// Query radius 1: 3x3 cells around (0,0) = cells (0,0),(1,0),(0,1),(-1,*),(*,-1).
	result = g.EntitiesInRadius(&pb.Position{X: 0, Y: 0, MapId: "map1"}, 1)
	if len(result) != 3 {
		t.Errorf("radius 1 from (0,0): expected 3 entities, got %d", len(result))
	}
	if result["e1"] == nil || result["e2"] == nil || result["e3"] == nil {
		t.Errorf("radius 1: expected e1, e2, e3, got keys missing")
	}

	// Query from a different position.
	result = g.EntitiesInRadius(&pb.Position{X: 17, Y: 0, MapId: "map1"}, 0)
	if len(result) != 1 || result["e2"] == nil {
		t.Errorf("radius 0 from (17,0): expected only e2, got %d entities", len(result))
	}
}

// TestAOIGrid_NilPosition verifies that Insert and EntitiesInRadius handle
// nil positions gracefully.
func TestAOIGrid_NilPosition(t *testing.T) {
	g := NewAOIGrid(16)

	e := &Entity{ID: "e1"} // no Position
	g.Insert(e)            // should be no-op

	result := g.EntitiesInRadius(nil, 3)
	if result != nil {
		t.Errorf("EntitiesInRadius(nil) should return nil, got %d entities", len(result))
	}
}

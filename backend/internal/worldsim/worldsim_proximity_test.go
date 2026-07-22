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

// TestMobileZone_FollowsAvatar verifies that a player's mobile proximity zone
// follows the avatar's position each tick, and that zone.enter/zone.exit fire
// when another player crosses the proximity boundary.
func TestMobileZone_FollowsAvatar(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	// No static zones — only mobile proximity zones.
	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	// Player A at (5, 5), player B at (10, 10) — far apart.
	a := &Entity{
		ID:             "e_a",
		Position:       &pb.Position{X: 5, Y: 5, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: "c_a", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	a.mobileZone = &Zone{
		ID:       "prox-e_a",
		Shape:    ShapeCircle,
		X:        5 - proximityRadius,
		Y:        5 - proximityRadius,
		W:        proximityRadius * 2,
		H:        proximityRadius * 2,
		Radius:   proximityRadius,
		Mobility: "mobile",
	}
	sim.zones["test-map"].AddZone(a.mobileZone)
	sim.entities["e_a"] = a
	sim.clients["c_a"] = a

	b := &Entity{
		ID:             "e_b",
		Position:       &pb.Position{X: 10, Y: 10, MapId: "test-map"},
		NetworkSession: &NetworkSession{ClientID: "c_b", Input: &pb.InputState{}},
		currentZones:   make(map[string]bool),
		spawnedTo:      make(map[string]bool),
	}
	b.mobileZone = &Zone{
		ID:       "prox-e_b",
		Shape:    ShapeCircle,
		X:        10 - proximityRadius,
		Y:        10 - proximityRadius,
		W:        proximityRadius * 2,
		H:        proximityRadius * 2,
		Radius:   proximityRadius,
		Mobility: "mobile",
	}
	sim.zones["test-map"].AddZone(b.mobileZone)
	sim.entities["e_b"] = b
	sim.clients["c_b"] = b

	type zoneEventPayload struct {
		EntityID string `json:"entity_id"`
		ClientID string `json:"client_id"`
		ZoneID   string `json:"zone_id"`
	}

	sub, err := subNc.SubscribeSync("zone.enter")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subNc.Flush()

	// Tick 1: A and B are far apart — no proximity events.
	sim.tick()
	pubNc.Flush()
	// Drain any messages (shouldn't be any for prox- zones).
	for {
		_, err := sub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
	}

	// Move B next to A (within 2 tiles).
	b.Position.X = 6
	b.Position.Y = 5

	// Tick 2: B's feet (Y+1=6) are at (6, 6). A's mobile zone is centered at
	// (5, 5) with radius 2 → covers X[3,7], Y[3,7]. Point (6, 6) is inside.
	// B enters A's proximity zone → zone.enter for B into prox-e_a.
	sim.tick()
	pubNc.Flush()

	foundEnter := false
	deadline := time.Now().Add(2 * time.Second)
	for !foundEnter && time.Now().Before(deadline) {
		msg, err := sub.NextMsg(500 * time.Millisecond)
		if err != nil {
			t.Fatalf("expected zone.enter for B entering prox-e_a, got timeout")
		}
		var ev zoneEventPayload
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			continue
		}
		if ev.EntityID == "e_b" && ev.ZoneID == "prox-e_a" {
			foundEnter = true
			if ev.ClientID != "c_b" {
				t.Errorf("zone.enter client_id = %q, want c_b", ev.ClientID)
			}
		}
	}
	if !foundEnter {
		t.Fatal("B did not enter A's proximity zone")
	}

	// Move B far away again.
	b.Position.X = 15
	b.Position.Y = 15

	// Tick 3: B leaves A's proximity zone → zone.exit for B from prox-e_a.
	subExit, err := subNc.SubscribeSync("zone.exit")
	if err != nil {
		t.Fatalf("subscribe exit: %v", err)
	}
	subNc.Flush()

	sim.tick()
	pubNc.Flush()

	foundExit := false
	deadline = time.Now().Add(2 * time.Second)
	for !foundExit && time.Now().Before(deadline) {
		msg, err := subExit.NextMsg(500 * time.Millisecond)
		if err != nil {
			t.Fatalf("expected zone.exit for B leaving prox-e_a, got timeout")
		}
		var ev zoneEventPayload
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			continue
		}
		if ev.EntityID == "e_b" && ev.ZoneID == "prox-e_a" {
			foundExit = true
		}
	}
	if !foundExit {
		t.Fatal("B did not exit A's proximity zone")
	}
}

// TestProximityClustering_TwoPlayersJoin verifies that two players within
// proximity range get a proximity.join event with the same group_id, and
// that proximity.leave fires when they move apart.
func TestProximityClustering_TwoPlayersJoin(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	a := makePlayer("e_a", "c_a", 5, 5)
	b := makePlayer("e_b", "c_b", 10, 10)

	type proxEvent struct {
		EntityID string   `json:"entity_id"`
		ClientID string   `json:"client_id"`
		GroupID  string   `json:"group_id"`
		Members  []string `json:"members"`
	}

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	leaveSub, _ := subNc.SubscribeSync("proximity.leave")
	subNc.Flush()

	// Move B next to A.
	b.Position.X = 6
	b.Position.Y = 5

	// Tick: zone detection updates currentZones, then clustering runs
	// (tickCount starts at 0, 0%5==0).
	sim.tick()
	pubNc.Flush()

	// Both A and B should get proximity.join with the same group_id.
	joins := map[string]proxEvent{}
	deadline := time.Now().Add(2 * time.Second)
	for len(joins) < 2 && time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		joins[ev.EntityID] = ev
	}
	if len(joins) < 2 {
		t.Fatalf("expected 2 proximity.join events, got %d", len(joins))
	}
	if joins["e_a"].GroupID != joins["e_b"].GroupID {
		t.Errorf("A and B have different group IDs: %q vs %q",
			joins["e_a"].GroupID, joins["e_b"].GroupID)
	}
	if len(joins["e_a"].Members) != 2 {
		t.Errorf("expected 2 members, got %d", len(joins["e_a"].Members))
	}

	// Move B far away.
	b.Position.X = 15
	b.Position.Y = 15

	// Tick until clustering runs again (need tickCount%5==0).
	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// Both should get proximity.leave.
	leaves := map[string]bool{}
	deadline = time.Now().Add(2 * time.Second)
	for len(leaves) < 2 && time.Now().Before(deadline) {
		msg, err := leaveSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		leaves[ev.EntityID] = true
	}
	if len(leaves) < 2 {
		t.Fatalf("expected 2 proximity.leave events, got %d", len(leaves))
	}

	_ = a // suppress unused
}

// TestProximityClustering_ThreePlayerChain verifies that three players in a
// line (A-B-C, where A and C are >2 tiles apart but both near B) form one
// group via connected components.
func TestProximityClustering_ThreePlayerChain(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	// A at (5,5), B at (7,5), C at (9,5).
	// A-B distance: 2 tiles (within proximityRadius).
	// B-C distance: 2 tiles (within proximityRadius).
	// A-C distance: 4 tiles (> proximityRadius).
	// Connected component: {A, B, C}.
	makePlayer("e_a", "c_a", 5, 5)
	makePlayer("e_b", "c_b", 7, 5)
	makePlayer("e_c", "c_c", 9, 5)

	type proxEvent struct {
		EntityID string   `json:"entity_id"`
		GroupID  string   `json:"group_id"`
		Members  []string `json:"members"`
	}

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	subNc.Flush()

	sim.tick()
	pubNc.Flush()

	joins := map[string]proxEvent{}
	deadline := time.Now().Add(2 * time.Second)
	for len(joins) < 3 && time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		joins[ev.EntityID] = ev
	}
	if len(joins) < 3 {
		t.Fatalf("expected 3 proximity.join events, got %d", len(joins))
	}

	// All three should have the same group_id.
	gid := joins["e_a"].GroupID
	for _, id := range []string{"e_b", "e_c"} {
		if joins[id].GroupID != gid {
			t.Errorf("%s group_id = %q, want %q (same as A)", id, joins[id].GroupID, gid)
		}
	}
	// Each join should list all 3 members.
	for id, ev := range joins {
		if len(ev.Members) != 3 {
			t.Errorf("%s: expected 3 members, got %d: %v", id, len(ev.Members), ev.Members)
		}
	}
}

// TestProximityClustering_ZoneOverride verifies that a player inside an
// av_enabled zone does not get proximity.join events, even if near another
// player.
func TestProximityClustering_ZoneOverride(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	// One static av_enabled zone covering (5,5) area, plus mobile zones.
	avZone := &Zone{ID: "av1", Shape: ShapeRect, X: 4, Y: 4, W: 4, H: 4, AvEnabled: true}
	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry([]*Zone{avZone}, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	// A inside the av_enabled zone (feet at Y=6, inside av1 which covers Y[4,8]).
	// B next to A but outside the av_enabled zone.
	a := makePlayer("e_a", "c_a", 5, 5) // feet at (5, 6), inside av1
	b := makePlayer("e_b", "c_b", 6, 5) // feet at (6, 6), inside av1 too

	// Actually both are inside av1. Let me move B outside.
	b.Position.X = 8
	b.Position.Y = 5 // feet at (8, 6), av1 covers X[4,8] so (8,6) is on the edge.
	// Move B clearly outside av1.
	b.Position.X = 9
	b.Position.Y = 5 // feet at (9, 6), outside av1 (X[4,8]).

	// But B at (9,5) is 4 tiles from A at (5,5) — too far for proximity.
	// Let me make the av zone smaller and place B just outside but within 2 tiles.
	// Reset: av1 covers X[4,6], Y[4,6]. A at (5,5) feet (5,6) — inside.
	// B at (7,5) feet (7,6) — outside av1, but 2 tiles from A.
	sim.zones["test-map"].RemoveZone("av1")
	avZone = &Zone{ID: "av1", Shape: ShapeRect, X: 4, Y: 4, W: 2, H: 2, AvEnabled: true}
	sim.zones["test-map"].AddZone(avZone)

	a.Position.X = 5
	a.Position.Y = 5 // feet (5, 6) — inside av1 (X[4,6], Y[4,6])
	b.Position.X = 7
	b.Position.Y = 5 // feet (7, 6) — outside av1, 2 tiles from A

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	subNc.Flush()

	sim.tick()
	pubNc.Flush()

	// A is in the av_enabled zone → no proximity.join for A.
	// B is outside but near A → B should NOT get proximity.join either,
	// because A is suppressed (A is not in the players list, so the
	// adjacency check won't find A as a neighbor for B).
	// B is a singleton → no group.
	msg, err := joinSub.NextMsg(500 * time.Millisecond)
	if err == nil {
		var ev struct {
			EntityID string `json:"entity_id"`
		}
		json.Unmarshal(msg.Data, &ev)
		t.Errorf("expected no proximity.join events, got one for %s", ev.EntityID)
	}
}

// TestProximityClustering_StableGroupID verifies that the group ID stays
// stable when a third player joins or leaves an existing group of two.
// Existing members should NOT receive proximity.leave/join events — only
// the joining/leaving player gets an event. This prevents the LiveKit
// room (and all video tiles) from being torn down and rebuilt on every
// membership change.
func TestProximityClustering_StableGroupID(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 20, 20)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	a := makePlayer("e_a", "c_a", 5, 5)
	b := makePlayer("e_b", "c_b", 6, 5)
	c := makePlayer("e_c", "c_c", 15, 15) // far away initially

	type proxEvent struct {
		EntityID string   `json:"entity_id"`
		GroupID  string   `json:"group_id"`
		Members  []string `json:"members"`
	}

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	leaveSub, _ := subNc.SubscribeSync("proximity.leave")
	subNc.Flush()

	// Tick 1: A and B are adjacent → both get proximity.join with same group ID.
	sim.tick()
	pubNc.Flush()

	initialJoins := map[string]proxEvent{}
	deadline := time.Now().Add(2 * time.Second)
	for len(initialJoins) < 2 && time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		initialJoins[ev.EntityID] = ev
	}
	if len(initialJoins) < 2 {
		t.Fatalf("expected 2 initial proximity.join events, got %d", len(initialJoins))
	}
	groupID := initialJoins["e_a"].GroupID
	if initialJoins["e_b"].GroupID != groupID {
		t.Fatalf("A and B have different group IDs: %q vs %q",
			initialJoins["e_a"].GroupID, initialJoins["e_b"].GroupID)
	}

	// Move C next to A and B.
	c.Position.X = 7
	c.Position.Y = 5

	// Tick until clustering runs (tickCount%5==0).
	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// C should get proximity.join with the SAME group ID.
	// A and B should get NO new events (no leave, no join).
	cJoined := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(200 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_a" || ev.EntityID == "e_b" {
			t.Errorf("existing member %s got unexpected proximity.join on 3rd player join", ev.EntityID)
		}
		if ev.EntityID == "e_c" {
			if ev.GroupID != groupID {
				t.Errorf("C joined group %q, expected stable group %q", ev.GroupID, groupID)
			}
			cJoined = true
		}
	}
	if !cJoined {
		t.Fatal("C did not get proximity.join")
	}

	// Check no leave events fired for A or B during the join.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, err := leaveSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_a" || ev.EntityID == "e_b" {
			t.Errorf("existing member %s got unexpected proximity.leave on 3rd player join", ev.EntityID)
		}
	}

	// Move C far away.
	c.Position.X = 15
	c.Position.Y = 15

	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// C should get proximity.leave. A and B should get NO events.
	cLeft := false
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := leaveSub.NextMsg(200 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_a" || ev.EntityID == "e_b" {
			t.Errorf("existing member %s got unexpected proximity.leave on 3rd player departure", ev.EntityID)
		}
		if ev.EntityID == "e_c" {
			cLeft = true
		}
	}
	if !cLeft {
		t.Fatal("C did not get proximity.leave")
	}

	// Check no join events fired for A or B during the leave.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_a" || ev.EntityID == "e_b" {
			t.Errorf("existing member %s got unexpected proximity.join on 3rd player departure", ev.EntityID)
		}
	}

	_ = a
	_ = b
}

// TestProximityClustering_Hysteresis verifies that the proximity zone exit
// is delayed by hysteresis: a player within proximityExitRadius (but beyond
// proximityRadius) of another player does NOT get a proximity.leave event.
// Only when the distance exceeds proximityExitRadius does the leave fire.
// See issue #88.
func TestProximityClustering_Hysteresis(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 30, 30)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	a := makePlayer("e_a", "c_a", 5, 5)
	b := makePlayer("e_b", "c_b", 6, 5) // 1 tile from A — within enter radius

	type proxEvent struct {
		EntityID string   `json:"entity_id"`
		GroupID  string   `json:"group_id"`
		Members  []string `json:"members"`
	}

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	leaveSub, _ := subNc.SubscribeSync("proximity.leave")
	subNc.Flush()

	// Tick to form the group (both stationary).
	sim.tick()
	pubNc.Flush()

	joins := map[string]proxEvent{}
	deadline := time.Now().Add(2 * time.Second)
	for len(joins) < 2 && time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		joins[ev.EntityID] = ev
	}
	if len(joins) < 2 {
		t.Fatalf("expected 2 proximity.join events, got %d", len(joins))
	}

	// Move B to 2.5 tiles from A — beyond proximityRadius (2.0) but within
	// proximityExitRadius (3.0). Hysteresis should suppress the exit.
	b.Position.X = 7.5
	b.Position.Y = 5
	// Update B's mobile zone position (tick does this, but we need the
	// zone check to see the new position).
	// Mark B as stationary so movement gating doesn't interfere.
	b.stationaryTicks = proximityStationaryThreshold

	// Tick until clustering runs (every 5 ticks).
	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// No proximity.leave should fire — B is still within exit radius.
	// Drain any messages with a short timeout.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, err := leaveSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		t.Errorf("unexpected proximity.leave for %s at 2.5 tiles (within exit radius)", ev.EntityID)
	}

	// Move B to 4 tiles from A — beyond proximityExitRadius (3.0).
	// Exit should now fire.
	b.Position.X = 9
	b.Position.Y = 5
	b.stationaryTicks = proximityStationaryThreshold

	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()

	foundLeave := false
	deadline = time.Now().Add(2 * time.Second)
	for !foundLeave && time.Now().Before(deadline) {
		msg, err := leaveSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		foundLeave = true
	}
	if !foundLeave {
		t.Fatal("expected proximity.leave at 4 tiles (beyond exit radius), got none")
	}

	_ = a
}

// TestProximityClustering_MovementGating verifies that proximity.join is
// suppressed when a player is moving, and only fires after the player has
// been stationary for proximityStationaryThreshold ticks. This prevents A/V
// thrashing when a player walks past another without stopping. See issue #88.
func TestProximityClustering_MovementGating(t *testing.T) {
	_, natsURL := startEmbeddedNATS(t)
	logger := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	subNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	t.Cleanup(subNc.Close)

	pubNc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	t.Cleanup(pubNc.Close)

	sim := &Simulator{
		World: World{
			zones:    map[string]*ZoneRegistry{"test-map": NewZoneRegistry(nil, 30, 30)},
			entities: map[string]*Entity{},
			clients:  map[string]*Entity{},
		},
		nc:         pubNc,
		defaultMap: "test-map",
		extMgr:     NewExtensionManager(logger),
		logger:     logger,
		tracer:     otel.Tracer("test"),
	}
	sim.initTestSystems()

	makePlayer := func(id, cid string, x, y float32) *Entity {
		e := &Entity{
			ID:              id,
			Position:        &pb.Position{X: x, Y: y, MapId: "test-map"},
			NetworkSession:  &NetworkSession{ClientID: cid, Input: &pb.InputState{}},
			currentZones:    make(map[string]bool),
			spawnedTo:       make(map[string]bool),
			stationaryTicks: proximityStationaryThreshold,
		}
		e.mobileZone = &Zone{
			ID: "prox-" + id, Shape: ShapeCircle,
			X: x - proximityRadius, Y: y - proximityRadius,
			W: proximityRadius * 2, H: proximityRadius * 2,
			Radius: proximityRadius, Mobility: "mobile",
		}
		sim.zones["test-map"].AddZone(e.mobileZone)
		sim.entities[id] = e
		sim.clients[cid] = e
		return e
	}

	a := makePlayer("e_a", "c_a", 5, 5)
	b := makePlayer("e_b", "c_b", 15, 15) // far away initially

	type proxEvent struct {
		EntityID string   `json:"entity_id"`
		GroupID  string   `json:"group_id"`
		Members  []string `json:"members"`
	}

	joinSub, _ := subNc.SubscribeSync("proximity.join")
	leaveSub, _ := subNc.SubscribeSync("proximity.leave")
	subNc.Flush()

	// Tick enough times for A to become stationary (A never moves).
	for i := 0; i < proximityStationaryThreshold; i++ {
		sim.tick()
	}
	pubNc.Flush()
	// Drain any messages (A is alone, no events expected).
	for {
		_, err := joinSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
	}

	// Move B next to A but mark B as moving (stationaryTicks = 0).
	b.Position.X = 6
	b.Position.Y = 5
	b.stationaryTicks = 0

	// Tick: clustering runs (tickCount%5==0 at tick 10). B is moving →
	// new group {A,B} skipped (not all stationary). No events.
	sim.tick()
	pubNc.Flush()

	// No proximity.join should fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		t.Errorf("unexpected proximity.join for %s while B is moving", ev.EntityID)
	}

	// B stops. Tick enough times for B to become stationary.
	// stationaryTicks increments each tick B doesn't move (dirtyPosition
	// is false since runMovementSystem has zero input).
	for i := 0; i < proximityStationaryThreshold; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// Now both should get proximity.join.
	joins := map[string]proxEvent{}
	deadline = time.Now().Add(2 * time.Second)
	for len(joins) < 2 && time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		joins[ev.EntityID] = ev
	}
	if len(joins) < 2 {
		t.Fatalf("expected 2 proximity.join events after B stopped, got %d", len(joins))
	}
	if joins["e_a"].GroupID != joins["e_b"].GroupID {
		t.Errorf("A and B have different group IDs: %q vs %q",
			joins["e_a"].GroupID, joins["e_b"].GroupID)
	}

	// --- Part 2: existing group, new moving player C ---
	c := makePlayer("e_c", "c_c", 20, 20) // far away
	// Make C stationary to start.
	c.stationaryTicks = proximityStationaryThreshold

	// Tick to settle C's zone state.
	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()
	// Drain any messages.
	for {
		_, err := joinSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
	}

	// Move C next to the group but mark as moving.
	c.Position.X = 7
	c.Position.Y = 5
	c.stationaryTicks = 0

	// Tick: C is moving → join suppressed for C. A and B unaffected.
	for i := 0; i < 5; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// No proximity.join for C while moving.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_c" {
			t.Errorf("unexpected proximity.join for C while moving")
		}
	}

	// C stops. Tick enough for C to become stationary.
	for i := 0; i < proximityStationaryThreshold; i++ {
		sim.tick()
	}
	pubNc.Flush()

	// C should now get proximity.join with the same group ID.
	cJoined := false
	groupID := joins["e_a"].GroupID
	deadline = time.Now().Add(2 * time.Second)
	for !cJoined && time.Now().Before(deadline) {
		msg, err := joinSub.NextMsg(500 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_c" {
			cJoined = true
			if ev.GroupID != groupID {
				t.Errorf("C joined group %q, expected stable group %q", ev.GroupID, groupID)
			}
		}
	}
	if !cJoined {
		t.Fatal("C did not get proximity.join after stopping")
	}

	// A and B should NOT have received any leave or join during C's join.
	deadline = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		msg, err := leaveSub.NextMsg(100 * time.Millisecond)
		if err != nil {
			break
		}
		var ev proxEvent
		json.Unmarshal(msg.Data, &ev)
		if ev.EntityID == "e_a" || ev.EntityID == "e_b" {
			t.Errorf("existing member %s got unexpected proximity.leave on C join", ev.EntityID)
		}
	}

	_ = a
	_ = b
}

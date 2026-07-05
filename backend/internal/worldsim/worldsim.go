// Package worldsim is the spatial authority and replication gateway.
// For the lite MVP it runs a fixed tick loop, a minimal hand-rolled ECS
// (Position + NetworkSession), player avatar movement, and replication.
package worldsim

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// --- Minimal ECS ---

const (
	compPosition = 1
)

type Entity struct {
	ID         string
	Position   *pb.Position
	NetworkSession *NetworkSession
	// dirty: which components changed since last replication tick
	dirtyPosition bool
	// spawnedTo tracks which clients have received a SpawnEntity for this
	// entity. Per-client rather than global so a late-joining client gets
	// spawns for entities that already exist.
	spawnedTo map[string]bool
	// currentZones tracks which zone IDs the entity is currently inside.
	// Used to detect enter/exit transitions.
	currentZones map[string]bool
}

type NetworkSession struct {
	ClientID string
	// Latest input state from the client
	Input *pb.InputState
	Seq   uint32
}

// --- World Sim ---

type Simulator struct {
	nc          *nats.Conn
	mapID       string
	mapData     *MapData
	zoneReg     *ZoneRegistry
	userStore   *UserStore
	extMgr      *ExtensionManager
	tickHz      int
	tickDur     time.Duration
	logger      *slog.Logger
	tracer      trace.Tracer

	mu       sync.Mutex
	entities map[string]*Entity // entity_id -> entity
	clients  map[string]*Entity // client_id -> entity (player avatar)

	snapshotSeq uint32
}

func New(natsURL, mapID, pocketbaseURL, pbAdminEmail, pbAdminPassword string, tickHz int, logger *slog.Logger) (*Simulator, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("worldsim"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	// Load map data (dimensions + collision grid + zones) from PocketBase.
	mapData, err := LoadMap(pocketbaseURL, mapID)
	if err != nil {
		logger.Warn("failed to load map from pocketbase, using fallback bounds",
			"err", err, "pocketbase", pocketbaseURL, "map", mapID)
		mapData = &MapData{Width: 20, Height: 20}
	}

	s := &Simulator{
		nc:        nc,
		mapID:     mapID,
		mapData:   mapData,
		zoneReg:   NewZoneRegistry(mapData.Zones, mapData.Width, mapData.Height),
		userStore: NewUserStore(pocketbaseURL, pbAdminEmail, pbAdminPassword),
		extMgr:    NewExtensionManager(logger),
		tickHz:    tickHz,
		tickDur:   time.Second / time.Duration(tickHz),
		logger:    logger,
		tracer:    otel.Tracer("worldsim"),
		entities:  make(map[string]*Entity),
		clients:   make(map[string]*Entity),
	}

	if err := s.subscribe(); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	return s, nil
}

func (s *Simulator) subscribe() error {
	// client.connected — provision a new player entity
	if _, err := s.nc.Subscribe("client.connected", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.client.connected")
		defer span.End()
		var ar pb.AuthResultFrame
		if err := proto.Unmarshal(m.Data, &ar); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			s.logger.Warn("client.connected unmarshal", "err", err)
			return
		}
		span.SetAttributes(attribute.String("client.id", ar.ClientId), attribute.String("user.sub", ar.GetSub()))
		s.provisionClient(ctx, ar.ClientId, ar.GetSub())
	}); err != nil {
		return fmt.Errorf("subscribe client.connected: %w", err)
	}

	// client.disconnected — despawn entity
	if _, err := s.nc.Subscribe("client.disconnected", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.client.disconnected")
		defer span.End()
		var ar pb.AuthResultFrame
		if err := proto.Unmarshal(m.Data, &ar); err != nil {
			return
		}
		span.SetAttributes(attribute.String("client.id", ar.ClientId))
		s.despawnClient(ctx, ar.ClientId)
	}); err != nil {
		return fmt.Errorf("subscribe client.disconnected: %w", err)
	}

	// client.<id>.input — queue input for the tick loop
	if _, err := s.nc.Subscribe("client.*.input", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.apply_input")
		defer span.End()
		clientID := subjectClientID(m.Subject, "input")
		span.SetAttributes(attribute.String("client.id", clientID))
		var input pb.InputFrame
		if err := proto.Unmarshal(m.Data, &input); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		span.SetAttributes(attribute.Int("input.seq", int(input.GetSeq())))
		s.applyInput(ctx, clientID, &input)
	}); err != nil {
		return fmt.Errorf("subscribe client.input: %w", err)
	}

	// Extension lifecycle subscriptions.
	if err := s.extMgr.Subscribe(s.nc); err != nil {
		return fmt.Errorf("extension subscribe: %w", err)
	}

	// Admin: map integrity check on demand.
	if _, err := s.nc.Subscribe("admin.map.integrity", func(m *nats.Msg) {
		s.runIntegrityCheck()
	}); err != nil {
		return fmt.Errorf("subscribe admin.map.integrity: %w", err)
	}

	return nil
}

func (s *Simulator) Run(ctx context.Context) error {
	// Start extension stale checker.
	go s.extMgr.StartStaleChecker(ctx)

	// Run map integrity check at startup.
	s.runIntegrityCheck()

	// Periodic integrity check (every 5 minutes).
	go s.startPeriodicIntegrityCheck(ctx)

	ticker := time.NewTicker(s.tickDur)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.nc.Close()
			return ctx.Err()
		case <-ticker.C:
			s.tick()
		}
	}
}

// provisionClient creates a player avatar entity for the given client.
// If the user has a record in PocketBase (by oidc_sub), their persistent
// entity_id and last position are restored. Otherwise a new user record
// is created.
func (s *Simulator) provisionClient(ctx context.Context, clientID, sub string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.clients[clientID]; exists {
		return
	}

	defaultEntityID := "e_" + clientID[2:]
	spawnX, spawnY := float32(10), float32(10)
	if s.mapData != nil {
		spawnX, spawnY = s.mapData.FindSpawn()
	}

	entityID := defaultEntityID

	// Look up or create the user in PocketBase for persistent identity.
	if s.userStore != nil && sub != "" && sub != "dev" {
		user, err := s.userStore.FindOrCreateUser(sub, defaultEntityID)
		if err != nil {
			s.logger.WarnContext(ctx, "user store lookup failed, using defaults",
				"err", err, "sub", sub)
		} else {
			entityID = user.EntityID
			// Restore saved position if it's valid (not 0,0 — the default).
			if user.PosX != 0 || user.PosY != 0 {
				spawnX, spawnY = user.PosX, user.PosY
			}
		}
	}

	e := &Entity{
		ID:       entityID,
		Position: &pb.Position{X: spawnX, Y: spawnY, MapId: s.mapID, Dir: 0},
		NetworkSession: &NetworkSession{
			ClientID: clientID,
			Input:    &pb.InputState{},
		},
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
	s.entities[entityID] = e
	s.clients[clientID] = e

	s.logger.InfoContext(ctx, "provisioned entity",
		"entity", entityID, "client", clientID, "sub", sub,
		"x", e.Position.X, "y", e.Position.Y)
}

func (s *Simulator) despawnClient(ctx context.Context, clientID string) {
	s.mu.Lock()

	e, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.entities, e.ID)
	delete(s.clients, clientID)
	posX, posY := e.Position.X, e.Position.Y
	entityID := e.ID
	s.mu.Unlock()

	// Save position to PocketBase outside the lock (network I/O).
	if s.userStore != nil {
		if err := s.userStore.SavePosition(entityID, posX, posY); err != nil {
			s.logger.WarnContext(ctx, "failed to save user position", "err", err, "entity", entityID)
		}
	}

	s.logger.InfoContext(ctx, "despawned entity", "entity", entityID, "client", clientID)
}

func (s *Simulator) applyInput(ctx context.Context, clientID string, input *pb.InputFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.clients[clientID]
	if !ok {
		return
	}
	e.NetworkSession.Input = input.GetState()
	e.NetworkSession.Seq = input.GetSeq()
}

// tick runs the game loop: movement system + replication.
func (s *Simulator) tick() {
	ctx, span := s.tracer.Start(context.Background(), "worldsim.tick")
	defer span.End()
	start := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshotSeq++

	// --- Movement system ---
	speed := float32(0.4) // tiles per tick (~8 tiles/sec at 20Hz)
	for _, e := range s.entities {
		if e.NetworkSession == nil || e.Position == nil {
			continue
		}
		input := e.NetworkSession.Input
		if input == nil {
			continue
		}

		dx, dy := float32(0), float32(0)
		if input.Up {
			dy -= 1
		}
		if input.Down {
			dy += 1
		}
		if input.Left {
			dx -= 1
		}
		if input.Right {
			dx += 1
		}

		// Normalize diagonal
		if dx != 0 && dy != 0 {
			dx *= float32(0.7071)
			dy *= float32(0.7071)
		}

		if dx == 0 && dy == 0 {
			continue
		}

		newX := e.Position.X + dx*speed
		newY := e.Position.Y + dy*speed

		if s.mapData != nil {
			// Clamp to map bounds.
			newX = clamp(newX, 0, float32(s.mapData.Width-1))
			newY = clamp(newY, 0, float32(s.mapData.Height-1))

			// Collision check: try X and Y independently so the avatar
			// slides along walls instead of sticking. Use +0.5 because the
			// sprite center is at position*TILE_SIZE + TILE_SIZE/2, so the
			// tile the center is in is floor(position + 0.5).
			if s.isTileBlocked(int(newX+0.5), int(e.Position.Y+0.5)) {
				newX = e.Position.X
			}
			if s.isTileBlocked(int(newX+0.5), int(newY+0.5)) {
				newY = e.Position.Y
			}
		} else {
			// Fallback: no map data, use hardcoded bounds.
			newX = clamp(newX, 1, 18)
			newY = clamp(newY, 1, 18)
		}

		if newX != e.Position.X || newY != e.Position.Y {
			e.Position.X = newX
			e.Position.Y = newY
			e.dirtyPosition = true

			// Update direction
			if absF(dx) > absF(dy) {
				if dx > 0 {
					e.Position.Dir = 2 // right
				} else {
					e.Position.Dir = 1 // left
				}
			} else {
				if dy > 0 {
					e.Position.Dir = 0 // down
				} else {
					e.Position.Dir = 3 // up
				}
			}
		}
	}

	// --- Zone enter/exit detection ---
	for _, e := range s.entities {
		if e.currentZones == nil {
			e.currentZones = make(map[string]bool)
		}
		newZones := s.zoneReg.ZonesAtPoint(e.Position.X, e.Position.Y)
		newSet := make(map[string]bool, len(newZones))
		for _, zid := range newZones {
			newSet[zid] = true
		}
		for zid := range newSet {
			if !e.currentZones[zid] {
				s.publishZoneEvent(ctx, "zone.enter", e.ID, zid)
			}
		}
		for zid := range e.currentZones {
			if !newSet[zid] {
				s.publishZoneEvent(ctx, "zone.exit", e.ID, zid)
			}
		}
		e.currentZones = newSet
	}

	// --- Replication ---
	// Lite MVP: replicate everything to everyone (no AOI filter).
	replicated := 0
	for _, e := range s.entities {
		if e.NetworkSession == nil {
			continue
		}
		if s.replicateToClient(ctx, e) {
			replicated++
		}
	}

	// Clear dirty flags
	for _, e := range s.entities {
		e.dirtyPosition = false
	}

	// Metric-as-log-attrs: tick duration, entity count, replication batches.
	// motel has no /v1/metrics endpoint, so we emit these as span attributes +
	// a structured log so they're queryable via log search.
	durMs := time.Since(start).Milliseconds()
	span.SetAttributes(
		attribute.Int("tick.duration_ms", int(durMs)),
		attribute.Int("tick.entity_count", len(s.entities)),
		attribute.Int("tick.replicated_clients", replicated),
		attribute.Int("tick.snapshot_seq", int(s.snapshotSeq)),
	)
	s.logger.InfoContext(ctx, "tick",
		"duration_ms", durMs,
		"entity_count", len(s.entities),
		"replicated_clients", replicated,
		"snapshot_seq", s.snapshotSeq,
	)
}

// replicateToClient builds and publishes a replication batch for one client.
// Returns true if a batch was published. The published NATS message carries
// this span's context so pusher's ws.write_replication span parents here.
func (s *Simulator) replicateToClient(ctx context.Context, clientEntity *Entity) bool {
	rctx, span := s.tracer.Start(ctx, "worldsim.replicate")
	defer span.End()

	clientID := clientEntity.NetworkSession.ClientID
	span.SetAttributes(attribute.String("client.id", clientID))

	batch := &pb.ReplicationBatch{
		LastInputSeq: clientEntity.NetworkSession.Seq,
	}

	for _, e := range s.entities {
		alreadySpawned := e.spawnedTo[clientID]

		// Don't replicate the client's own entity via SpawnEntity/UpdateComponent
		// in the full spec (predicted locally). But for the lite MVP with no
		// prediction, we DO send the client's own position so it can render.
		if e == clientEntity && alreadySpawned {
			// Send own position updates too (no client-side prediction in lite MVP)
			if e.dirtyPosition {
				posBytes, _ := proto.Marshal(e.Position)
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compPosition,
					Data:        posBytes,
					SnapshotSeq: s.snapshotSeq,
				})
			}
			continue
		}

		if !alreadySpawned {
			// Spawn
			posBytes, _ := proto.Marshal(e.Position)
			batch.Spawns = append(batch.Spawns, &pb.SpawnEntity{
				EntityId:    e.ID,
				SnapshotSeq: s.snapshotSeq,
				Components: []*pb.ComponentData{
					{ComponentId: compPosition, Data: posBytes},
				},
			})
			e.spawnedTo[clientID] = true
		} else if e.dirtyPosition {
			posBytes, _ := proto.Marshal(e.Position)
			batch.Updates = append(batch.Updates, &pb.UpdateComponent{
				EntityId:    e.ID,
				ComponentId: compPosition,
				Data:        posBytes,
				SnapshotSeq: s.snapshotSeq,
			})
		}
	}

	// Only publish if there's something to send
	if len(batch.Spawns) == 0 && len(batch.Updates) == 0 && len(batch.Destroys) == 0 {
		return false
	}

	span.SetAttributes(
		attribute.Int("batch.spawns", len(batch.Spawns)),
		attribute.Int("batch.updates", len(batch.Updates)),
		attribute.Int("batch.destroys", len(batch.Destroys)),
	)

	// Wrap in a ServerFrame so the pusher can forward bytes unchanged to the
	// client (the pusher is a pure WS<->NATS passthrough; it must not know the
	// replication wire format).
	frame := &pb.ServerFrame{Payload: &pb.ServerFrame_Replication{Replication: batch}}
	frameBytes, err := proto.Marshal(frame)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal")
		s.logger.WarnContext(rctx, "replication marshal", "client", clientID, "err", err)
		return false
	}

	subject := fmt.Sprintf("client.%s.replication", clientID)
	msg := &nats.Msg{Subject: subject, Data: frameBytes}
	otelinternal.Inject(rctx, msg) // pusher's forward span will parent here
	if err := s.nc.PublishMsg(msg); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish")
		s.logger.WarnContext(rctx, "replication publish", "client", clientID, "err", err)
		return false
	}
	return true
}

// runIntegrityCheck validates the current map and logs any issues.
func (s *Simulator) runIntegrityCheck() {
	results := CheckMapIntegrity(s.mapData)
	LogIntegrityResults(s.logger, results, s.mapID)
}

// startPeriodicIntegrityCheck runs the integrity check every 5 minutes.
func (s *Simulator) startPeriodicIntegrityCheck(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runIntegrityCheck()
		}
	}
}

// isTileBlocked returns true if the tile is blocked by either the Walls
// tile layer (fallback) or a zone with a block gate trigger from an
// active extension.
func (s *Simulator) isTileBlocked(tx, ty int) bool {
	// Check zone gate triggers first (extension-driven walls).
	if s.zoneReg != nil {
		for _, zoneID := range s.zoneReg.ZonesAt(tx, ty) {
			if s.extMgr.IsZoneBlocked(zoneID) {
				return true
			}
		}
	}
	// Fallback: Walls tile layer collision.
	if s.mapData != nil && s.mapData.IsBlocked(tx, ty) {
		return true
	}
	return false
}

// publishZoneEvent publishes a zone.enter or zone.exit event to NATS.
// Extensions subscribe to these subjects to observe zone transitions.
func (s *Simulator) publishZoneEvent(ctx context.Context, event, entityID, zoneID string) {
	subject := fmt.Sprintf("zone.%s", event)
	data := fmt.Sprintf(`{"entity_id":"%s","zone_id":"%s","map_id":"%s"}`, entityID, zoneID, s.mapID)
	if err := s.nc.Publish(subject, []byte(data)); err != nil {
		s.logger.WarnContext(ctx, "zone event publish", "event", event, "err", err)
	}
	s.logger.InfoContext(ctx, "zone event", "event", event, "entity", entityID, "zone", zoneID)
}

// subjectClientID extracts the client_id from a subject like "client.<id>.input".
func subjectClientID(subject, suffix string) string {
	// "client.c_abc123.input" -> "c_abc123"
	prefix := "client."
	s := subject[len(prefix):]
	end := len(s) - len("."+suffix)
	if end < 0 {
		return ""
	}
	return s[:end]
}

func clamp(v, min, max float32) float32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func absF(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

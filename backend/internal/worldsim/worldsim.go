// Package worldsim is the spatial authority and replication gateway.
// For the lite MVP it runs a fixed tick loop, a minimal hand-rolled ECS
// (Position + NetworkSession), player avatar movement, and replication.
package worldsim

import (
	"context"
	"encoding/json"
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
	compPosition   = 1
	compEntityState = 2
)

type Entity struct {
	ID         string
	Position   *pb.Position
	NetworkSession *NetworkSession
	// EntityType/OwnerExtension/TriggerRadius: set for base entities loaded
	// from the map's "Entities" object layer (see mapdata.go PropEntity).
	// Included in input-trigger dispatch payloads so extensions can
	// self-filter without re-reading the map.
	EntityType     string
	OwnerExtension string
	TriggerRadius  float32
	// State is a generic opaque string (EntityState component) that
	// extensions can set via an input-trigger reply, e.g. "on"/"off".
	State      string
	// dirty: which components changed since last replication tick
	dirtyPosition bool
	dirtyState    bool
	// pendingAnimations: animation IDs to replicate as PlayAnimation on the
	// next tick, then cleared.
	pendingAnimations []uint32
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
	nc           *nats.Conn
	mapID        string
	mapFilename  string
	mapData      *MapData
	zoneReg      *ZoneRegistry
	userStore    *UserStore
	extMgr       *ExtensionManager
	pocketbaseURL string
	tickHz       int
	tickDur      time.Duration
	tickCount    uint64
	logger       *slog.Logger
	tracer       trace.Tracer

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
		nc:            nc,
		mapID:         mapID,
		mapData:       mapData,
		zoneReg:       NewZoneRegistry(mapData.Zones, mapData.Width, mapData.Height),
		userStore:     NewUserStore(pocketbaseURL, pbAdminEmail, pbAdminPassword),
		extMgr:        NewExtensionManager(logger),
		pocketbaseURL: pocketbaseURL,
		tickHz:        tickHz,
		tickDur:       time.Second / time.Duration(tickHz),
		logger:        logger,
		tracer:        otel.Tracer("worldsim"),
		entities:      make(map[string]*Entity),
		clients:       make(map[string]*Entity),
	}

	s.loadBaseEntities()

	if err := s.subscribe(); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	return s, nil
}

// loadBaseEntities spawns ECS entities for props defined on the map's
// "Entities" object layer (see mapdata.go PropEntity and
// documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md
// Part C). These have no NetworkSession and are inert until an extension
// claims them via an input trigger.
func (s *Simulator) loadBaseEntities() {
	if s.mapData == nil {
		return
	}
	for _, pe := range s.mapData.Entities {
		s.entities[pe.ID] = &Entity{
			ID:             pe.ID,
			Position:       &pb.Position{X: pe.X, Y: pe.Y, MapId: s.mapID},
			EntityType:     pe.EntityType,
			OwnerExtension: pe.OwnerExtension,
			TriggerRadius:  pe.TriggerRadius,
			spawnedTo:      make(map[string]bool),
			currentZones:   make(map[string]bool),
		}
	}
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

	// client.<id>.action — input trigger (key/click), see 14-zones-and-interactions.md §3a.
	if _, err := s.nc.Subscribe("client.*.action", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.apply_action")
		defer span.End()
		clientID := subjectClientID(m.Subject, "action")
		span.SetAttributes(attribute.String("client.id", clientID))
		var action pb.ActionFrame
		if err := proto.Unmarshal(m.Data, &action); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		span.SetAttributes(attribute.String("action.input", action.GetInput()))
		s.applyAction(ctx, clientID, &action)
	}); err != nil {
		return fmt.Errorf("subscribe client.action: %w", err)
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

	// Periodic map reload check (every 30 seconds).
	go s.startMapReloadChecker(ctx)

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

// actionInputTimeout bounds how long the kernel waits for a single
// extension's reply to a dispatched input event before moving on.
const actionInputTimeout = 300 * time.Millisecond

// adjacentEntityInfo is the per-entity data included in an input-trigger
// dispatch payload, letting extensions self-filter without re-reading the
// map (see 13-ecs-design.md §6).
type adjacentEntityInfo struct {
	EntityID       string `json:"entity_id"`
	EntityType     string `json:"entity_type,omitempty"`
	OwnerExtension string `json:"owner_extension,omitempty"`
}

// actionDispatchMsg is published to extension.<id>.action for every
// extension registered for the triggered input type.
type actionDispatchMsg struct {
	EntityID         string                `json:"entity_id"` // the acting player
	Input            string                `json:"input"`
	AdjacentEntities []adjacentEntityInfo  `json:"adjacent_entities"`
}

// actionReplyMsg is the extension's reply to an actionDispatchMsg.
type actionReplyMsg struct {
	Handled bool `json:"handled"`
	Updates []struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	} `json:"updates"`
	Animations []struct {
		EntityID    string `json:"entity_id"`
		AnimationID uint32 `json:"animation_id"`
	} `json:"animations"`
}

// applyAction handles a player-initiated ActionFrame (InputHandlerSystem —
// see 13-ecs-design.md §5). It computes adjacent entities, broadcasts to all
// extensions registered for the input type, applies every reply, and
// replies to the client with the aggregate result.
func (s *Simulator) applyAction(ctx context.Context, clientID string, action *pb.ActionFrame) {
	s.mu.Lock()
	e, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		return
	}
	px, py := e.Position.X, e.Position.Y
	adjacent := s.adjacentEntitiesLocked(e.ID, px, py)
	s.mu.Unlock()

	extIDs := s.extMgr.ExtensionsForInput(action.GetInput())
	if len(extIDs) == 0 {
		s.sendActionResult(ctx, clientID, action.GetSeq(), false, "no_handler")
		return
	}

	payload, err := json.Marshal(actionDispatchMsg{
		EntityID:         e.ID,
		Input:            action.GetInput(),
		AdjacentEntities: adjacent,
	})
	if err != nil {
		s.logger.WarnContext(ctx, "action dispatch marshal", "err", err)
		return
	}

	handled := false
	for _, extID := range extIDs {
		subject := fmt.Sprintf("extension.%s.action", extID)
		reply, err := s.nc.RequestWithContext(ctx, subject, payload)
		if err != nil {
			// Timeout or no responder — extension may have chosen not to
			// reply because it doesn't own any of the adjacent entities.
			continue
		}
		var resp actionReplyMsg
		if err := json.Unmarshal(reply.Data, &resp); err != nil {
			s.logger.WarnContext(ctx, "action reply unmarshal", "extension", extID, "err", err)
			continue
		}
		if resp.Handled {
			handled = true
			s.applyActionReply(&resp)
		}
	}

	reason := ""
	if !handled {
		reason = "timeout"
	}
	s.sendActionResult(ctx, clientID, action.GetSeq(), handled, reason)
}

// adjacentEntitiesLocked returns non-avatar entities within trigger range of
// (px, py), excluding the acting entity. Caller must hold s.mu.
func (s *Simulator) adjacentEntitiesLocked(actingID string, px, py float32) []adjacentEntityInfo {
	const defaultRadius = float32(1.5) // tiles
	var result []adjacentEntityInfo
	for _, e := range s.entities {
		if e.ID == actingID || e.Position == nil {
			continue
		}
		radius := e.TriggerRadius
		if radius <= 0 {
			radius = defaultRadius
		}
		dx, dy := e.Position.X-px, e.Position.Y-py
		if dx*dx+dy*dy > radius*radius {
			continue
		}
		result = append(result, adjacentEntityInfo{
			EntityID:       e.ID,
			EntityType:     e.EntityType,
			OwnerExtension: e.OwnerExtension,
		})
	}
	return result
}

// applyActionReply applies component/animation updates from an extension's
// reply to the target entities. Unknown entity IDs are ignored.
func (s *Simulator) applyActionReply(resp *actionReplyMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range resp.Updates {
		if e, ok := s.entities[u.EntityID]; ok && e.State != u.State {
			e.State = u.State
			e.dirtyState = true
		}
	}
	for _, a := range resp.Animations {
		if e, ok := s.entities[a.EntityID]; ok {
			e.pendingAnimations = append(e.pendingAnimations, a.AnimationID)
		}
	}
}

// sendActionResult publishes an ActionResultFrame to the client on its
// replication subject (the pusher forwards ServerFrame bytes verbatim
// regardless of which oneof case is set, so no new subject is needed).
func (s *Simulator) sendActionResult(ctx context.Context, clientID string, seq uint32, ok bool, reason string) {
	frame := &pb.ServerFrame{Payload: &pb.ServerFrame_ActionResult{
		ActionResult: &pb.ActionResultFrame{Seq: seq, Ok: ok, Reason: reason},
	}}
	frameBytes, err := proto.Marshal(frame)
	if err != nil {
		s.logger.WarnContext(ctx, "action result marshal", "err", err)
		return
	}
	subject := fmt.Sprintf("client.%s.replication", clientID)
	if err := s.nc.Publish(subject, frameBytes); err != nil {
		s.logger.WarnContext(ctx, "action result publish", "client", clientID, "err", err)
	}
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
			// slides along walls instead of sticking. Check a 3x3 grid of
			// sample points around the player position to catch partial
			// tile overlaps (especially important for polygon zones where
			// the wall may cover only part of a tile).
			if s.isPositionBlocked(newX, e.Position.Y) {
				newX = e.Position.X
			}
			if s.isPositionBlocked(newX, newY) {
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
		e.dirtyState = false
		e.pendingAnimations = nil
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
	// Log tick summary every 5 seconds (every 300th tick at 60Hz) to avoid
	// flooding the logs. Span attributes are always set for tracing.
	s.tickCount++
	if s.tickCount%300 == 0 {
		s.logger.InfoContext(ctx, "tick",
			"duration_ms", durMs,
			"entity_count", len(s.entities),
			"replicated_clients", replicated,
			"snapshot_seq", s.snapshotSeq,
		)
	}
}

// appendAnimations queues any pending PlayAnimation events for an
// already-spawned entity onto the batch.
func (s *Simulator) appendAnimations(batch *pb.ReplicationBatch, e *Entity) {
	for _, animID := range e.pendingAnimations {
		batch.Animations = append(batch.Animations, &pb.PlayAnimation{
			EntityId:    e.ID,
			AnimationId: animID,
		})
	}
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
			s.appendAnimations(batch, e)
			continue
		}

		if !alreadySpawned {
			// Spawn
			posBytes, _ := proto.Marshal(e.Position)
			components := []*pb.ComponentData{
				{ComponentId: compPosition, Data: posBytes},
			}
			if e.State != "" {
				stateBytes, _ := proto.Marshal(&pb.EntityState{State: e.State})
				components = append(components, &pb.ComponentData{ComponentId: compEntityState, Data: stateBytes})
			}
			batch.Spawns = append(batch.Spawns, &pb.SpawnEntity{
				EntityId:    e.ID,
				SnapshotSeq: s.snapshotSeq,
				Components:  components,
			})
			e.spawnedTo[clientID] = true
		} else {
			if e.dirtyPosition {
				posBytes, _ := proto.Marshal(e.Position)
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compPosition,
					Data:        posBytes,
					SnapshotSeq: s.snapshotSeq,
				})
			}
			if e.dirtyState {
				stateBytes, _ := proto.Marshal(&pb.EntityState{State: e.State})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compEntityState,
					Data:        stateBytes,
					SnapshotSeq: s.snapshotSeq,
				})
			}
			s.appendAnimations(batch, e)
		}
	}

	// Only publish if there's something to send
	if len(batch.Spawns) == 0 && len(batch.Updates) == 0 && len(batch.Destroys) == 0 && len(batch.Animations) == 0 {
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

// startMapReloadChecker periodically checks if the map has been updated in
// PocketBase (by comparing the tiled_json filename). If changed, reloads the
// map, rebuilds the zone registry, and publishes a map.updated event on NATS
// so extensions can re-read the map too.
func (s *Simulator) startMapReloadChecker(ctx context.Context) {
	// Get the initial filename.
	currentInfo, err := FetchMapRecordInfo(s.pocketbaseURL, s.mapID)
	if err != nil {
		s.logger.Warn("map reload checker: failed to get initial record info", "err", err)
	} else if currentInfo != nil {
		s.mu.Lock()
		s.mapFilename = currentInfo.TiledJSONFilename
		s.mu.Unlock()
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkMapReload()
		}
	}
}

// checkMapReload fetches the current map record info and compares the
// filename. If it changed, reloads the map.
func (s *Simulator) checkMapReload() {
	info, err := FetchMapRecordInfo(s.pocketbaseURL, s.mapID)
	if err != nil {
		return // PocketBase might be temporarily unreachable
	}
	if info == nil {
		return
	}

	s.mu.Lock()
	oldFilename := s.mapFilename
	s.mu.Unlock()

	if info.TiledJSONFilename == oldFilename {
		return // no change
	}

	s.logger.Info("map updated, reloading",
		"map", s.mapID,
		"old_file", oldFilename,
		"new_file", info.TiledJSONFilename)

	// Reload the map.
	newMapData, err := LoadMap(s.pocketbaseURL, s.mapID)
	if err != nil {
		s.logger.Error("map reload failed", "err", err)
		return
	}

	s.mu.Lock()
	s.mapData = newMapData
	s.zoneReg = NewZoneRegistry(newMapData.Zones, newMapData.Width, newMapData.Height)
	s.mapFilename = info.TiledJSONFilename
	s.mu.Unlock()

	// Run integrity check on the new map.
	s.runIntegrityCheck()

	// Notify extensions that the map has been updated.
	s.nc.Publish("map.updated", []byte(s.mapID))
	s.logger.Info("map reloaded and map.updated event published", "map", s.mapID)
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

// isPositionBlocked checks whether the player at position (px, py) in tile
// coords is blocked. Zone collision is checked in continuous space (directly
// against zone shapes) for sub-tile precision. Tile-layer collision uses the
// tile grid as fallback.
func (s *Simulator) isPositionBlocked(px, py float32) bool {
	// Zone gate triggers: check player's bounding box directly against
	// zone shapes in continuous space. This handles polygons thinner than
	// a tile that tile rasterization would miss.
	if s.zoneReg != nil {
		const r = 0.3 // player collision radius in tiles
		points := [5][2]float32{
			{px, py},         // center
			{px - r, py - r}, // top-left
			{px + r, py - r}, // top-right
			{px - r, py + r}, // bottom-left
			{px + r, py + r}, // bottom-right
		}
		for _, p := range points {
			for _, zoneID := range s.zoneReg.ZonesAtPoint(p[0], p[1]) {
				if s.extMgr.IsZoneBlocked(zoneID) {
					return true
				}
			}
		}
	}
	// Fallback: Walls tile layer collision (tile-based by nature).
	if s.mapData != nil {
		tx := int(px + 0.5)
		ty := int(py + 0.5)
		if s.mapData.IsBlocked(tx, ty) {
			return true
		}
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

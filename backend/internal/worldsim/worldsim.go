// Package worldsim is the spatial authority and replication gateway.
// For the lite MVP it runs a fixed tick loop, a minimal hand-rolled ECS
// (Position + NetworkSession), player avatar movement, and replication.
package worldsim

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"os"
	"sort"
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
	compPosition    = 1
	compEntityState = 2
	compAppearance  = 3
	compDisplayName = 4
)

type Entity struct {
	ID             string
	Position       *pb.Position
	NetworkSession *NetworkSession
	// DisplayName is the server-stamped name used in chat messages. Set at
	// provision time: PocketBase display_name for logged-in users, or
	// "Guest <short>" for anonymous sessions. Empty for base (non-player)
	// entities.
	DisplayName string
	// EntityType/OwnerExtension/TriggerRadius: set for base entities loaded
	// from the map's "Entities" object layer (see mapdata.go PropEntity).
	// Included in input-trigger dispatch payloads so extensions can
	// self-filter without re-reading the map.
	EntityType     string
	OwnerExtension string
	TriggerRadius  float32
	// Gid is the Tiled global tile ID for base entities (from the "Entities"
	// object layer), sent as an Appearance component so the frontend can render
	// the correct tile sprite. 0 for player avatars.
	Gid uint32
	// SpriteBase is the sprite_bases PocketBase record ID selecting the
	// character sheet for player avatars. Set at provision time from the
	// player's persisted choice. Empty for base entities (they use Gid) and
	// for guests (frontend falls back to a hash-based index).
	SpriteBase string
	// State is a generic opaque string (EntityState component) that
	// extensions can set via an input-trigger reply, e.g. "on"/"off".
	State string
	// dirty: which components changed since last replication tick
	dirtyPosition   bool
	dirtyState      bool
	dirtyName       bool
	dirtyAppearance bool
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
	// mobileZone is the player's proximity circle zone (radius proximityRadius)
	// that follows the avatar's position each tick. Other players entering
	// this zone triggers zone.enter for the proximity clustering step.
	// nil for base entities (only player avatars get one).
	mobileZone *Zone
	// currentProximityGroup is the stable ID of the proximity A/V group the
	// player currently belongs to (e.g. "proxgroup-<hash>"). Empty when not
	// in a proximity group. Used to detect join/leave transitions.
	currentProximityGroup string
}

type NetworkSession struct {
	ClientID string
	// Latest input state from the client
	Input *pb.InputState
	Seq   uint32
}

// --- World Sim ---

type Simulator struct {
	nc            *nats.Conn
	mapID         string
	mapFilename   string
	mapData       *MapData
	zoneReg       *ZoneRegistry
	userStore     *UserStore
	spriteStore   *SpriteStore
	extMgr        *ExtensionManager
	pocketbaseURL string
	tickHz        int
	tickDur       time.Duration
	tickCount     uint64
	logger        *slog.Logger
	tracer        trace.Tracer
	rng           *rand.Rand

	mu       sync.Mutex
	entities map[string]*Entity // entity_id -> entity
	clients  map[string]*Entity // client_id -> entity (player avatar)
	// entityIDToClient maps player entity_id -> client_id, used by handleChat
	// to address proximity chat recipients. Maintained alongside s.clients.
	entityIDToClient map[string]string
	// destroyedEntities queues entity IDs removed since the last tick (base
	// entities removed during a map reload, or player avatars despawned on
	// disconnect), so the next replication tick can send DestroyEntity frames
	// to all connected clients. Drained after each tick's replication loop.
	destroyedEntities []string

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

	// Auto-seed the default map into PocketBase on first run, so a fresh
	// deployment boots without a manual upload step. MAP_DIR defaults to
	// ./maps (bundled in dist/) for production; for local dev, set MAP_DIR
	// to the repo's maps/ directory. Retries for up to 30s while PocketBase
	// is still starting. Non-fatal: if seeding ultimately fails, worldsim
	// still starts and LoadMap below will surface the real error.
	mapDir := os.Getenv("MAP_DIR")
	if mapDir == "" {
		mapDir = "./maps"
	}
	mapStore := NewMapStore(pocketbaseURL, pbAdminEmail, pbAdminPassword)
	for i := 0; i < 30; i++ {
		if err := mapStore.SeedMapIfMissing(mapID, mapDir, "default-map.json"); err == nil {
			break
		} else if i == 29 {
			logger.Warn("map seed failed", "err", err, "dir", mapDir)
		}
		time.Sleep(time.Second)
	}

	// Load map data (dimensions + collision grid + zones) from PocketBase.
	mapData, err := LoadMap(pocketbaseURL, mapID)
	if err != nil {
		logger.Warn("failed to load map from pocketbase, using fallback bounds",
			"err", err, "pocketbase", pocketbaseURL, "map", mapID)
		mapData = &MapData{Width: 20, Height: 20}
	}

	s := &Simulator{
		nc:               nc,
		mapID:            mapID,
		mapData:          mapData,
		zoneReg:          NewZoneRegistry(mapData.Zones, mapData.Width, mapData.Height),
		userStore:        NewUserStore(pocketbaseURL, pbAdminEmail, pbAdminPassword),
		spriteStore:      NewSpriteStore(pocketbaseURL, pbAdminEmail, pbAdminPassword),
		extMgr:           NewExtensionManager(logger),
		pocketbaseURL:    pocketbaseURL,
		tickHz:           tickHz,
		tickDur:          time.Second / time.Duration(tickHz),
		logger:           logger,
		tracer:           otel.Tracer("worldsim"),
		rng:              rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)),
		entities:         make(map[string]*Entity),
		clients:          make(map[string]*Entity),
		entityIDToClient: make(map[string]string),
	}

	s.loadBaseEntities()

	// Auto-seed the sprite_bases catalog from the bundled sprites directory on
	// first run. Non-fatal: if PB is down or seeding fails, worldsim still
	// starts and the frontend falls back to static char_0..char_4 sheets.
	// SPRITES_DIR defaults to ./sprites (bundled in dist/) for production; for
	// local dev, run `make sync-assets` and set SPRITES_DIR=frontend/public/sprites
	// (or point directly at spritesheets/ when running the binary outside Vite).
	spritesDir := os.Getenv("SPRITES_DIR")
	if spritesDir == "" {
		spritesDir = "./sprites"
	}
	if err := s.spriteStore.SeedIfEmpty(spritesDir); err != nil {
		logger.Warn("sprite seed failed", "err", err, "dir", spritesDir)
	}

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
			Gid:            pe.Gid,
			spawnedTo:      make(map[string]bool),
			currentZones:   make(map[string]bool),
		}
	}
}

// reloadBaseEntities reconciles the ECS base entities with the map's
// "Entities" object layer after a map reload. Removes entities that no longer
// exist (queuing them for destroy notification), updates existing ones, and
// adds new ones. Caller must hold s.mu.
func (s *Simulator) reloadBaseEntities(newEntities []*PropEntity) {
	newIDs := make(map[string]bool, len(newEntities))
	for _, pe := range newEntities {
		newIDs[pe.ID] = true
	}

	// Remove base entities that no longer exist in the new map.
	for id, e := range s.entities {
		if e.NetworkSession != nil {
			continue // player avatar, not a base entity
		}
		if !newIDs[id] {
			delete(s.entities, id)
			s.destroyedEntities = append(s.destroyedEntities, id)
		}
	}

	// Add or update base entities from the new map.
	for _, pe := range newEntities {
		if e, ok := s.entities[pe.ID]; ok {
			e.Position.X = pe.X
			e.Position.Y = pe.Y
			e.Position.MapId = s.mapID
			e.EntityType = pe.EntityType
			e.OwnerExtension = pe.OwnerExtension
			e.TriggerRadius = pe.TriggerRadius
			e.Gid = pe.Gid
			e.dirtyPosition = true
		} else {
			s.entities[pe.ID] = &Entity{
				ID:             pe.ID,
				Position:       &pb.Position{X: pe.X, Y: pe.Y, MapId: s.mapID},
				EntityType:     pe.EntityType,
				OwnerExtension: pe.OwnerExtension,
				TriggerRadius:  pe.TriggerRadius,
				Gid:            pe.Gid,
				spawnedTo:      make(map[string]bool),
				currentZones:   make(map[string]bool),
			}
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
		entityID := s.provisionClient(ctx, ar.ClientId, ar.GetSub())
		// Respond with the entity ID so the pusher can include it in the
		// AuthResultFrame sent to the client. The client needs the actual
		// entity ID (which may differ from "e_"+clientID[2:] when a
		// PocketBase-stored identity exists) to identify its own avatar.
		if m.Reply != "" {
			resp, _ := proto.Marshal(&pb.AuthResultFrame{EntityId: entityID})
			if err := s.nc.Publish(m.Reply, resp); err != nil {
				s.logger.Warn("client.connected reply", "err", err, "client", ar.ClientId)
			}
		}
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

	// client.<id>.chat — text chat (global or proximity channel).
	// See documentation/plans/2026-07-07-chat-design.md.
	if _, err := s.nc.Subscribe("client.*.chat", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.handle_chat_sub")
		defer span.End()
		clientID := subjectClientID(m.Subject, "chat")
		span.SetAttributes(attribute.String("client.id", clientID))
		var chat pb.ChatFrame
		if err := proto.Unmarshal(m.Data, &chat); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		s.handleChat(ctx, clientID, &chat)
	}); err != nil {
		return fmt.Errorf("subscribe client.chat: %w", err)
	}

	// client.<id>.set_name — display name change request.
	// See documentation/plans/2026-07-07-avatar-name-tags-design.md.
	if _, err := s.nc.Subscribe("client.*.set_name", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.handle_set_name_sub")
		defer span.End()
		clientID := subjectClientID(m.Subject, "set_name")
		span.SetAttributes(attribute.String("client.id", clientID))
		var frame pb.SetNameFrame
		if err := proto.Unmarshal(m.Data, &frame); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		s.handleSetName(ctx, clientID, &frame)
	}); err != nil {
		return fmt.Errorf("subscribe client.set_name: %w", err)
	}

	// client.<id>.set_sprite_base — character sheet change request.
	// See documentation/plans/2026-07-07-sprite-selection-design.md.
	if _, err := s.nc.Subscribe("client.*.set_sprite_base", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.handle_set_sprite_base_sub")
		defer span.End()
		clientID := subjectClientID(m.Subject, "set_sprite_base")
		span.SetAttributes(attribute.String("client.id", clientID))
		var frame pb.SetSpriteBaseFrame
		if err := proto.Unmarshal(m.Data, &frame); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		s.handleSetSpriteBase(ctx, clientID, &frame)
	}); err != nil {
		return fmt.Errorf("subscribe client.set_sprite_base: %w", err)
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

	// Announce readiness so extensions can register against a live subscriber
	// instead of racing their initial publish (NATS Core drops publishes with
	// no subscribers). Flush guarantees the broadcast is on the wire before
	// the tick loop starts. Extensions also listen for this to re-register on
	// worldsim restarts.
	if err := s.nc.Publish("worldsim.ready", []byte(s.mapID)); err != nil {
		s.logger.Warn("publish worldsim.ready", "err", err)
	}
	if err := s.nc.Flush(); err != nil {
		return fmt.Errorf("flush worldsim.ready: %w", err)
	}
	s.logger.Info("worldsim ready", "map", s.mapID)

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
func (s *Simulator) provisionClient(ctx context.Context, clientID, sub string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, exists := s.clients[clientID]; exists {
		return existing.ID
	}

	defaultEntityID := "e_" + clientID[2:]
	spawnX, spawnY := float32(10), float32(10)
	if s.mapData != nil {
		spawnX, spawnY = s.mapData.FindSpawnPoint(s.rng)
	}

	entityID := defaultEntityID
	displayName := ""
	spriteBase := ""

	// Look up or create the user in PocketBase for persistent identity.
	if s.userStore != nil && sub != "" && sub != "dev" {
		user, err := s.userStore.FindOrCreateUser(sub, defaultEntityID)
		if err != nil {
			s.logger.WarnContext(ctx, "user store lookup failed, using defaults",
				"err", err, "sub", sub)
		} else {
			entityID = user.EntityID
			displayName = user.DisplayName
			spriteBase = user.SpriteBase
			// Restore saved position if it's valid (not 0,0 — the default).
			if user.PosX != 0 || user.PosY != 0 {
				spawnX, spawnY = user.PosX, user.PosY
			}
		}
	}
	// Guests (sub == "" or "dev") get a per-session display name derived
	// from their entity ID. Logged-in users with an empty PocketBase
	// display_name fall back to their entity ID.
	if displayName == "" {
		if sub == "" || sub == "dev" {
			displayName = "Guest " + lastN(entityID, 4)
		} else {
			displayName = entityID
		}
	}

	e := &Entity{
		ID:       entityID,
		Position: &pb.Position{X: spawnX, Y: spawnY, MapId: s.mapID, Dir: 0},
		NetworkSession: &NetworkSession{
			ClientID: clientID,
			Input:    &pb.InputState{},
		},
		DisplayName:  displayName,
		SpriteBase:   spriteBase,
		spawnedTo:    make(map[string]bool),
		currentZones: make(map[string]bool),
	}
	// Create a mobile proximity zone that follows this avatar. Other players
	// entering it triggers zone.enter, which the proximity clustering step
	// uses to group nearby players into shared A/V rooms. Centered at the
	// avatar's feet (Position.Y + avatarFeetYOffset) to match where zone
	// detection evaluates membership.
	feetY := spawnY + avatarFeetYOffset
	e.mobileZone = &Zone{
		ID:       "prox-" + entityID,
		Shape:    ShapeCircle,
		X:        spawnX - proximityRadius,
		Y:        feetY - proximityRadius,
		W:        proximityRadius * 2,
		H:        proximityRadius * 2,
		Radius:   proximityRadius,
		Mobility: "mobile",
	}
	if s.zoneReg != nil {
		s.zoneReg.AddZone(e.mobileZone)
	}
	s.entities[entityID] = e
	s.clients[clientID] = e
	s.entityIDToClient[entityID] = clientID

	s.logger.InfoContext(ctx, "provisioned entity",
		"entity", entityID, "client", clientID, "sub", sub,
		"x", e.Position.X, "y", e.Position.Y)
	return entityID
}

func (s *Simulator) despawnClient(ctx context.Context, clientID string) {
	s.mu.Lock()

	e, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		return
	}
	// Publish zone.exit for all zones the entity is currently in, so
	// extensions like ext-av can clean up (e.g. send LiveKit "leave"
	// tokens). Without this, stale LiveKit participants linger after
	// a client disconnects or reconnects.
	clientIDForEvent := e.NetworkSession.ClientID
	for zid := range e.currentZones {
		s.publishZoneEvent(ctx, "zone.exit", e.ID, clientIDForEvent, zid)
	}
	// Leave proximity group if any.
	if e.currentProximityGroup != "" {
		s.publishProximityEvent(ctx, "proximity.leave", e.ID, clientIDForEvent, e.currentProximityGroup, nil)
	}
	// Remove the player's mobile proximity zone from the registry.
	if e.mobileZone != nil && s.zoneReg != nil {
		s.zoneReg.RemoveZone(e.mobileZone.ID)
	}
	delete(s.entities, e.ID)
	delete(s.clients, clientID)
	delete(s.entityIDToClient, e.ID)
	// Queue a DestroyEntity so the next replication tick notifies all other
	// clients. Without this, remaining clients never learn the entity is gone
	// and the avatar sprite stays on screen after the player disconnects.
	s.destroyedEntities = append(s.destroyedEntities, e.ID)
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
	EntityID         string               `json:"entity_id"` // the acting player
	Input            string               `json:"input"`
	AdjacentEntities []adjacentEntityInfo `json:"adjacent_entities"`
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
		reqCtx, cancel := context.WithTimeout(ctx, actionInputTimeout)
		reply, err := s.nc.RequestWithContext(reqCtx, subject, payload)
		cancel()
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

// maxChatRunes is the server-side truncation limit for chat messages.
const maxChatRunes = 500

// handleChat processes a client-sent ChatFrame: stamps display_name +
// timestamp, truncates text, then routes it to the appropriate NATS
// subject for the pusher to forward to recipients. Global messages go to
// chat.broadcast (pusher fans out to all sessions); proximity messages go
// to client.<recipientID>.chat_inbox per group member (including the
// sender, so they see their own message echoed). Messages from clients
// with no current proximity group are dropped silently (no one to hear).
// See documentation/plans/2026-07-07-chat-design.md.
func (s *Simulator) handleChat(ctx context.Context, clientID string, chat *pb.ChatFrame) {
	ctx, span := s.tracer.Start(ctx, "worldsim.handle_chat")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", clientID), attribute.String("chat.channel", chat.GetChannel()))

	// Truncate text to maxChatRunes (rune-safe).
	text := chat.GetText()
	if r := []rune(text); len(r) > maxChatRunes {
		text = string(r[:maxChatRunes])
	}

	s.mu.Lock()
	sender, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		span.SetStatus(codes.Error, "unknown client")
		return
	}
	msg := &pb.ChatMessageFrame{
		Channel:     chat.GetChannel(),
		EntityId:    sender.ID,
		DisplayName: sender.DisplayName,
		Text:        text,
		Timestamp:   uint64(time.Now().UnixMilli()),
	}
	frame := &pb.ServerFrame{Payload: &pb.ServerFrame_ChatMessage{ChatMessage: msg}}
	frameBytes, err := proto.Marshal(frame)
	if err != nil {
		s.mu.Unlock()
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal")
		return
	}

	var recipients []string // client IDs to deliver to
	switch chat.GetChannel() {
	case "global":
		recipients = nil // broadcast subject handles fan-out
	case "proximity":
		group := sender.currentProximityGroup
		if group == "" {
			s.mu.Unlock()
			return // solo — no one to hear
		}
		for _, e := range s.entities {
			if e.NetworkSession != nil && e.currentProximityGroup == group {
				if cid, ok := s.entityIDToClient[e.ID]; ok {
					recipients = append(recipients, cid)
				}
			}
		}
	default:
		s.mu.Unlock()
		span.SetStatus(codes.Error, "unknown channel")
		return
	}
	s.mu.Unlock()

	if chat.GetChannel() == "global" {
		if err := s.nc.Publish("chat.broadcast", frameBytes); err != nil {
			s.logger.WarnContext(ctx, "chat broadcast publish", "err", err)
			span.RecordError(err)
		}
		return
	}
	for _, cid := range recipients {
		subject := fmt.Sprintf("client.%s.chat_inbox", cid)
		if err := s.nc.Publish(subject, frameBytes); err != nil {
			s.logger.WarnContext(ctx, "chat inbox publish", "client", cid, "err", err)
		}
	}
}

// maxNameRunes is the server-side limit for display names (truncation +
// validation). See
// documentation/plans/2026-07-07-avatar-name-tags-design.md.
const maxNameRunes = 20

// handleSetName processes a client-sent SetNameFrame: sanitizes the name
// (ASCII printable only, max 20 runes), updates Entity.DisplayName, marks
// it dirty for replication, and persists to PocketBase for logged-in users.
// Guest names are session-only (not persisted). See
// documentation/plans/2026-07-07-avatar-name-tags-design.md.
func (s *Simulator) handleSetName(ctx context.Context, clientID string, frame *pb.SetNameFrame) {
	ctx, span := s.tracer.Start(ctx, "worldsim.handle_set_name")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", clientID))

	// Sanitize: keep only ASCII printable chars (32–126).
	raw := frame.GetName()
	cleaned := make([]rune, 0, len(raw))
	for _, r := range raw {
		if r >= 32 && r <= 126 {
			cleaned = append(cleaned, r)
		}
	}
	// Truncate to maxNameRunes (rune-safe).
	if len(cleaned) > maxNameRunes {
		cleaned = cleaned[:maxNameRunes]
	}
	name := string(cleaned)

	s.mu.Lock()
	entity, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		span.SetStatus(codes.Error, "unknown client")
		return
	}
	entity.DisplayName = name
	entity.dirtyName = true
	entityID := entity.ID
	s.mu.Unlock()

	// Persist to PocketBase for logged-in users. Guests have no
	// PocketBase record, so findByEntityID returns nil and the update is
	// a no-op — matching the design's "guest names are session-only".
	if s.userStore != nil {
		if err := s.userStore.UpdateDisplayName(entityID, name); err != nil {
			s.logger.WarnContext(ctx, "persist display name failed",
				"err", err, "entity", entityID)
			span.RecordError(err)
		}
	}
}

// handleSetSpriteBase processes a client-sent SetSpriteBaseFrame: validates
// the sprite_base ID exists in the sprite_bases collection, updates
// Entity.SpriteBase, marks it dirty for replication, and persists to
// PocketBase for logged-in users. Guests are rejected (no PB record). See
// documentation/plans/2026-07-07-sprite-selection-design.md.
func (s *Simulator) handleSetSpriteBase(ctx context.Context, clientID string, frame *pb.SetSpriteBaseFrame) {
	ctx, span := s.tracer.Start(ctx, "worldsim.handle_set_sprite_base")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", clientID))

	spriteBase := frame.GetSpriteBase()

	// Validate the sprite_base ID exists (when a SpriteStore is configured).
	// Empty sprite_base is allowed (revert to fallback). Skipped when
	// spriteStore is nil (tests / no-PB deployments), matching handleSetName's
	// userStore-nil pattern.
	if s.spriteStore != nil && spriteBase != "" {
		exists, err := s.spriteStore.BaseExists(spriteBase)
		if err != nil {
			span.RecordError(err)
			s.logger.WarnContext(ctx, "sprite_base validation failed",
				"err", err, "sprite_base", spriteBase)
			return
		}
		if !exists {
			span.SetStatus(codes.Error, "sprite_base not found")
			s.logger.WarnContext(ctx, "sprite_base not found", "sprite_base", spriteBase)
			return
		}
	}

	s.mu.Lock()
	entity, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		span.SetStatus(codes.Error, "unknown client")
		return
	}
	entity.SpriteBase = spriteBase
	entity.dirtyAppearance = true
	entityID := entity.ID
	s.mu.Unlock()

	// Persist to PocketBase for logged-in users. Guests have no
	// PocketBase record, so findByEntityID returns nil and the update is
	// a no-op — matching the design's "guests are session-only".
	if s.userStore != nil {
		if err := s.userStore.UpdateSpriteBase(entityID, spriteBase); err != nil {
			s.logger.WarnContext(ctx, "persist sprite_base failed",
				"err", err, "entity", entityID)
			span.RecordError(err)
		}
	}
}

// lastN returns the last n characters of s, or s if shorter.
func lastN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// tick runs the game loop: movement system + replication.
func (s *Simulator) tick() {
	ctx, span := s.tracer.Start(context.Background(), "worldsim.tick")
	defer span.End()
	start := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.snapshotSeq++

	s.runMovementSystem()

	// --- Update mobile zone positions ---
	// Move each player's proximity circle to follow their avatar's current
	// position (after movement was applied this tick). Centered at the feet
	// to match where zone detection evaluates membership. Must happen before
	// zone detection so the zone check sees up-to-date positions.
	for _, e := range s.entities {
		if e.mobileZone != nil && e.Position != nil {
			e.mobileZone.X = e.Position.X - proximityRadius
			e.mobileZone.Y = e.Position.Y + avatarFeetYOffset - proximityRadius
		}
	}

	// --- Zone enter/exit detection ---
	for _, e := range s.entities {
		if e.currentZones == nil {
			e.currentZones = make(map[string]bool)
		}
		// Evaluate zone membership at the avatar's feet (see
		// avatarFeetYOffset) so enter/exit transitions match where the
		// player visually crosses a zone boundary.
		newZones := s.zoneReg.ZonesAtPoint(e.Position.X, e.Position.Y+avatarFeetYOffset)
		newSet := make(map[string]bool, len(newZones))
		for _, zid := range newZones {
			newSet[zid] = true
		}
		clientID := ""
		if e.NetworkSession != nil {
			clientID = e.NetworkSession.ClientID
		}
		for zid := range newSet {
			if !e.currentZones[zid] {
				s.publishZoneEvent(ctx, "zone.enter", e.ID, clientID, zid)
			}
		}
		for zid := range e.currentZones {
			if !newSet[zid] {
				s.publishZoneEvent(ctx, "zone.exit", e.ID, clientID, zid)
			}
		}
		e.currentZones = newSet
	}

	// --- Proximity clustering (throttled to ~4Hz) ---
	if s.tickCount%5 == 0 {
		s.runProximityClustering(ctx)
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
		e.dirtyName = false
		e.dirtyAppearance = false
		e.pendingAnimations = nil
	}

	// Drain the destroyed entities queue — all clients have been replicated.
	s.destroyedEntities = nil

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
			// Player avatars (NetworkSession != nil) always get an Appearance
			// component with their SpriteBase, even when it's empty — otherwise
			// the client would fall back to a client-side hash and desync.
			if e.Gid != 0 || e.NetworkSession != nil {
				appearanceBytes, _ := proto.Marshal(&pb.Appearance{Gid: e.Gid, SpriteBase: e.SpriteBase})
				components = append(components, &pb.ComponentData{ComponentId: compAppearance, Data: appearanceBytes})
			}
			if e.State != "" {
				stateBytes, _ := proto.Marshal(&pb.EntityState{State: e.State})
				components = append(components, &pb.ComponentData{ComponentId: compEntityState, Data: stateBytes})
			}
			if e.NetworkSession != nil && e.DisplayName != "" {
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName})
				components = append(components, &pb.ComponentData{ComponentId: compDisplayName, Data: nameBytes})
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
			if e.dirtyName {
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compDisplayName,
					Data:        nameBytes,
					SnapshotSeq: s.snapshotSeq,
				})
			}
			if e.dirtyAppearance {
				appBytes, _ := proto.Marshal(&pb.Appearance{Gid: e.Gid, SpriteBase: e.SpriteBase})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compAppearance,
					Data:        appBytes,
					SnapshotSeq: s.snapshotSeq,
				})
			}
			s.appendAnimations(batch, e)
		}
	}

	// Send destroy notifications for entities removed since the last tick
	// (base entities removed during a map reload, or player avatars despawned
	// on disconnect).
	for _, id := range s.destroyedEntities {
		batch.Destroys = append(batch.Destroys, &pb.DestroyEntity{
			EntityId:    id,
			SnapshotSeq: s.snapshotSeq,
		})
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
	s.reloadBaseEntities(newMapData.Entities)
	// Re-add mobile proximity zones for all connected players — the registry
	// was rebuilt from scratch above, wiping them.
	for _, e := range s.clients {
		if e.mobileZone != nil {
			s.zoneReg.AddZone(e.mobileZone)
		}
	}
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

// runMovementSystem moves all player avatars one tick's worth of input,
// resolving collisions with swept (segment-vs-shape) zone checks so walls
// thinner than the per-tick movement distance cannot be tunneled through.
// Caller must hold s.mu.
func (s *Simulator) runMovementSystem() {
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
			// slides along walls instead of sticking. Swept (segment-vs-
			// shape) checks catch walls thinner than the per-tick movement
			// that point-sampling at the destination would miss.
			if s.isMoveBlocked(e.Position.X, e.Position.Y, newX, e.Position.Y) {
				newX = e.Position.X
			}
			if s.isMoveBlocked(newX, e.Position.Y, newX, newY) {
				newY = e.Position.Y
			}
			// Diagonal guard: if both axes moved, check the full diagonal
			// segment. The X-then-Y decomposition can skip a wall that the
			// diagonal crosses but neither axis-aligned segment does (the
			// X move jumps past a thin wall, then the Y move sits outside
			// its X range). If the diagonal is blocked, revert Y to slide
			// along the X axis.
			if newX != e.Position.X && newY != e.Position.Y {
				if s.isMoveBlocked(e.Position.X, e.Position.Y, newX, newY) {
					newY = e.Position.Y
				}
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
		} else if dx != 0 || dy != 0 {
			// Movement was attempted but fully blocked (e.g. walking
			// directly into a wall). Mark dirty so the client gets a
			// position correction even though the position didn't change —
			// otherwise the client's prediction runs ahead through the
			// wall and never gets snapped back.
			e.dirtyPosition = true
		}
	}
}

// avatarFeetYOffset is the vertical offset from an avatar's Position.Y to its
// feet in continuous tile coords. The frontend renders avatars with origin
// (0.5, 0.75) on a 64px-tall frame placed at (pos.X*32+16, pos.Y*32+16), which
// puts the feet at the bottom of the tile below Position — i.e. at
// Position.Y + 1.0. Collision and zone transitions must be evaluated at the
// feet, not at Position.Y (the sprite origin/upper-body), otherwise the player
// stops with feet buried in a wall or stops a full tile short of it.
const avatarFeetYOffset = 1.0

// proximityRadius is the radius (in tiles) of each player's mobile proximity
// zone. Two players within this distance of each other are grouped into the
// same proximity A/V LiveKit room. See
// documentation/plans/2026-07-06-livekit-av-design.md.
const proximityRadius = 2.0

// playerCollisionRadius is the half-width of the player's collision box in
// tiles, centered on the feet. Zone shapes are expanded by this radius
// (Minkowski sum) before the swept segment test, so the feet center stops
// `radius` tiles before the wall edge instead of at it. A small radius
// (0.1) keeps the visible gap against walls tight while still letting the
// player squeeze through 1-tile gaps without snagging on corners.
const playerCollisionRadius float32 = 0.1

// isMoveBlocked checks whether the movement segment from (oldX, oldY) to
// (newX, newY) in tile coords is blocked. Zone collision uses swept
// (segment-vs-shape) tests in continuous space, evaluated at the avatar's
// feet (Position.Y + avatarFeetYOffset), so walls thinner than the per-tick
// movement distance cannot be tunneled through. The Walls tile-layer fallback
// is checked at both endpoints' feet tiles (the tile grid is integer-indexed
// and movement is < 1 tile/tick, so endpoint sampling suffices there).
func (s *Simulator) isMoveBlocked(oldX, oldY, newX, newY float32) bool {
	// Translate to feet space.
	ofy := oldY + avatarFeetYOffset
	nfy := newY + avatarFeetYOffset
	r := playerCollisionRadius

	// Zone gate triggers: swept segment-vs-shape against each blocked zone.
	// Each shape is expanded by the player collision radius (Minkowski sum)
	// so the feet center stops `r` tiles before the wall edge, matching the
	// old 5-point sampling box width.
	if s.zoneReg != nil {
		for _, z := range s.zoneReg.zones {
			if !s.extMgr.IsZoneBlocked(z.ID) {
				continue
			}
			switch z.Shape {
			case ShapeRect:
				if segmentIntersectsRect(oldX, ofy, newX, nfy,
					z.X-r, z.Y-r, z.W+2*r, z.H+2*r) {
					return true
				}
			case ShapeCircle:
				cx, cy := z.X+z.Radius, z.Y+z.Radius
				if segmentIntersectsCircle(oldX, ofy, newX, nfy, cx, cy, z.Radius+r) {
					return true
				}
			case ShapePolygon:
				// Expand the polygon's bounding box by the radius. This is an
				// over-approximation (the expanded box is larger than the
				// true Minkowski sum of polygon + circle), so it may stop the
				// player slightly early near concave corners — safe but not
				// precise. A true polygon+circle Minkowski sum would require
				// offsetting each edge along its outward normal.
				abs := make([][2]float32, len(z.Polygon))
				for i, v := range z.Polygon {
					abs[i] = [2]float32{v[0] + z.X, v[1] + z.Y}
				}
				if segmentIntersectsPolygonExpanded(oldX, ofy, newX, nfy, abs, r) {
					return true
				}
			}
		}
	}
	// Fallback: Walls tile layer collision (tile-based by nature), at both
	// endpoints' feet tiles. Movement is < 1 tile/tick so if either endpoint
	// is in a blocked tile, the segment crossed it.
	if s.mapData != nil {
		if s.mapData.IsBlocked(int(oldX+0.5), int(ofy+0.5)) {
			return true
		}
		if s.mapData.IsBlocked(int(newX+0.5), int(nfy+0.5)) {
			return true
		}
	}
	return false
}

// publishZoneEvent publishes a zone.enter or zone.exit event to NATS.
// Extensions subscribe to these subjects to observe zone transitions.
// clientID is the player's client_id (empty for base entities without a
// NetworkSession); extensions like ext-av use it to address token replies.
func (s *Simulator) publishZoneEvent(ctx context.Context, event, entityID, clientID, zoneID string) {
	subject := event // event already contains the full subject (e.g. "zone.enter")
	data := fmt.Sprintf(`{"entity_id":"%s","client_id":"%s","zone_id":"%s","map_id":"%s"}`, entityID, clientID, zoneID, s.mapID)
	if err := s.nc.Publish(subject, []byte(data)); err != nil {
		s.logger.WarnContext(ctx, "zone event publish", "event", event, "err", err)
	}
	s.logger.InfoContext(ctx, "zone event", "event", event, "entity", entityID, "zone", zoneID)
}

// proximityEventPayload is the NATS payload for proximity.join/leave events.
type proximityEventPayload struct {
	EntityID string   `json:"entity_id"`
	ClientID string   `json:"client_id"`
	GroupID  string   `json:"group_id"`
	MapID    string   `json:"map_id"`
	Members  []string `json:"members,omitempty"`
}

// runProximityClustering groups nearby players (not in av_enabled zones) into
// proximity A/V groups via connected components on the "who is near whom"
// graph, then publishes edge-triggered proximity.join/proximity.leave events
// when a player's group assignment changes. Caller must hold s.mu.
func (s *Simulator) runProximityClustering(ctx context.Context) {
	if s.zoneReg == nil {
		return
	}

	// Build a set of av_enabled zone IDs. Players inside these zones get
	// zone-based A/V instead of proximity A/V (zones override proximity).
	avZones := make(map[string]bool)
	for _, z := range s.zoneReg.zones {
		if z.AvEnabled {
			avZones[z.ID] = true
		}
	}

	// Collect player entities not in any av_enabled zone.
	var players []*Entity
	inAVZone := make(map[string]bool)
	for _, e := range s.entities {
		if e.NetworkSession == nil {
			continue
		}
		for zid := range e.currentZones {
			if avZones[zid] {
				inAVZone[e.ID] = true
				break
			}
		}
		if !inAVZone[e.ID] {
			players = append(players, e)
		}
	}

	// Build adjacency: A and B are adjacent if A is inside B's proximity zone
	// or B is inside A's proximity zone (symmetric). Zone membership is
	// already tracked in currentZones from the zone detection step above.
	adj := make(map[string]map[string]bool)
	for _, e := range players {
		adj[e.ID] = make(map[string]bool)
	}
	for _, a := range players {
		for _, b := range players {
			if a.ID == b.ID {
				continue
			}
			if a.currentZones["prox-"+b.ID] || b.currentZones["prox-"+a.ID] {
				adj[a.ID][b.ID] = true
				adj[b.ID][a.ID] = true
			}
		}
	}

	// Find connected components via BFS.
	visited := make(map[string]bool)
	var groups [][]string
	for _, e := range players {
		if visited[e.ID] {
			continue
		}
		// BFS from this node.
		queue := []string{e.ID}
		visited[e.ID] = true
		var comp []string
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			comp = append(comp, cur)
			for neighbor := range adj[cur] {
				if !visited[neighbor] {
					visited[neighbor] = true
					queue = append(queue, neighbor)
				}
			}
		}
		groups = append(groups, comp)
	}

	// Assign group IDs and detect changes.
	newGroup := make(map[string]string)       // entity_id -> group_id
	groupMembers := make(map[string][]string) // group_id -> sorted member entity IDs
	for _, comp := range groups {
		if len(comp) < 2 {
			// Singleton — no proximity group.
			continue
		}
		sorted := append([]string(nil), comp...)
		sort.Strings(sorted)
		h := fnv.New64a()
		for _, id := range sorted {
			h.Write([]byte(id))
			h.Write([]byte{0}) // separator
		}
		gid := fmt.Sprintf("proxgroup-%016x", h.Sum64())
		for _, id := range comp {
			newGroup[id] = gid
		}
		groupMembers[gid] = sorted
	}

	// Publish edge-triggered events for changes.
	for _, e := range players {
		old := e.currentProximityGroup
		newG := newGroup[e.ID]

		if old == newG {
			continue
		}

		clientID := e.NetworkSession.ClientID

		// Leave old group (if any).
		if old != "" {
			s.publishProximityEvent(ctx, "proximity.leave", e.ID, clientID, old, nil)
		}

		// Join new group (if any).
		if newG != "" {
			s.publishProximityEvent(ctx, "proximity.join", e.ID, clientID, newG, groupMembers[newG])
		}

		e.currentProximityGroup = newG
	}

	// Players in av_enabled zones leave any proximity group they were in.
	for _, e := range s.entities {
		if e.NetworkSession == nil || !inAVZone[e.ID] {
			continue
		}
		if e.currentProximityGroup != "" {
			clientID := e.NetworkSession.ClientID
			s.publishProximityEvent(ctx, "proximity.leave", e.ID, clientID, e.currentProximityGroup, nil)
			e.currentProximityGroup = ""
		}
	}
}

// publishProximityEvent publishes a proximity.join or proximity.leave event
// to NATS. ext-av subscribes to these to mint LiveKit tokens for proximity
// A/V rooms.
func (s *Simulator) publishProximityEvent(ctx context.Context, event, entityID, clientID, groupID string, members []string) {
	payload := proximityEventPayload{
		EntityID: entityID,
		ClientID: clientID,
		GroupID:  groupID,
		MapID:    s.mapID,
		Members:  members,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.WarnContext(ctx, "proximity event marshal", "err", err)
		return
	}
	if err := s.nc.Publish(event, data); err != nil {
		s.logger.WarnContext(ctx, "proximity event publish", "event", event, "err", err)
	}
	s.logger.InfoContext(ctx, "proximity event", "event", event, "entity", entityID, "group", groupID)
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

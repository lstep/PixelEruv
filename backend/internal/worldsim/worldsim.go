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
	"github.com/pocketbase/pocketbase/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
	"github.com/lstep/pixeleruv/backend/internal/version"
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
	// IsGuest is true for anonymous sessions (no OIDC sub). Replicated as
	// part of the DisplayName component so clients can visually distinguish
	// guests (e.g. a "GUEST" badge on the name tag).
	IsGuest bool
	// IP is the client's IP address, captured at the pusher and threaded
	// via NATS. For logged-in users it's also persisted in PocketBase
	// (players.ip). For guests it's in-memory only. Sent to admin clients
	// via the admin-only NATS channel for all players — never to non-admins.
	IP string
	// DeviceID is the client-generated UUID stored in the browser's
	// localStorage, sent in the AuthFrame. Stable across sessions for the
	// same browser. Used as a ban target for guests (alongside IP and
	// user_id for logged-in users). Sent to admin clients via the
	// admin-only NATS channel.
	DeviceID string
	// IsAdmin is true for players with the is_admin flag in PocketBase.
	// Admins receive AdminInfoFrame data (guest IPs, etc.) via the
	// client.<id>.admin NATS subject. Always false for guests (no PB
	// record).
	IsAdmin bool
	// HideAdminBadge is true when the player opted out of the public
	// "admin" badge on their name tag (players.hide_admin_badge). Only
	// meaningful when IsAdmin is true; the replicated DisplayName.is_admin
	// flag is computed as IsAdmin && !HideAdminBadge.
	HideAdminBadge bool
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
	// PlayerOptions is the JSON-encoded player options string (e.g.
	// {"show_own_name_tag":true}). Persisted to PocketBase for logged-in
	// users; session-only for guests. Updated via SetPlayerOptionsFrame.
	PlayerOptions string
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
	app           core.App
	defaultMap    string
	maps          map[string]*MapData       // mapName → MapData
	zones         map[string]*ZoneRegistry  // mapName → ZoneRegistry
	mapFilenames  map[string]string         // mapName → last known tiled_json filename
	mapStore      *MapStore
	userStore     *UserStore
	banStore      *BanStore
	spriteStore   *SpriteStore
	extMgr        *ExtensionManager
	tickHz        int
	tickDur       time.Duration
	tickCount     uint64
	startTime     time.Time
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

func New(natsURL, defaultMap string, app core.App, tickHz int, logger *slog.Logger) (*Simulator, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("worldsim"),
		nats.ReconnectWait(2*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}

	mapStore := NewMapStore(app)
	userStore := NewUserStore(app)
	banStore := NewBanStore(app)
	spriteStore := NewSpriteStore(app)

	// Auto-seed the default map into PocketBase on first run, so a fresh
	// deployment boots without a manual upload step. MAP_DIR defaults to
	// ./maps (bundled in dist/) for production; for local dev, set MAP_DIR
	// to the repo's maps/ directory. Non-fatal: if seeding fails, worldsim
	// still starts and LoadMapData below will surface the real error.
	mapDir := os.Getenv("MAP_DIR")
	if mapDir == "" {
		mapDir = "./maps"
	}
	if err := mapStore.SeedMapIfMissing(defaultMap, mapDir, "default-map.json"); err != nil {
		logger.Warn("map seed failed", "err", err, "dir", mapDir)
	}

	// Load all maps from PocketBase.
	mapRecords, err := mapStore.ListAllMaps()
	if err != nil {
		logger.Warn("failed to list maps", "err", err)
	}
	maps := make(map[string]*MapData)
	zones := make(map[string]*ZoneRegistry)
	mapFilenames := make(map[string]string)
	for _, mr := range mapRecords {
		md, err := mapStore.LoadMapData(mr.Name)
		if err != nil {
			logger.Warn("failed to load map", "err", err, "map", mr.Name)
			continue
		}
		maps[mr.Name] = md
		zones[mr.Name] = NewZoneRegistry(md.Zones, md.Width, md.Height)
		mapFilenames[mr.Name] = mr.TiledJSONFilename
		logger.Info("loaded map", "map", mr.Name, "width", md.Width, "height", md.Height)
	}

	// Fallback: if no maps were loaded, try loading the default map by name.
	if len(maps) == 0 {
		logger.Warn("no maps loaded, trying default map", "default_map", defaultMap)
		md, err := mapStore.LoadMapData(defaultMap)
		if err != nil {
			logger.Warn("failed to load default map, using fallback bounds",
				"err", err, "map", defaultMap)
			md = &MapData{Width: 20, Height: 20}
		}
		maps[defaultMap] = md
		zones[defaultMap] = NewZoneRegistry(md.Zones, md.Width, md.Height)
	}

	s := &Simulator{
		nc:               nc,
		app:              app,
		defaultMap:       defaultMap,
		maps:             maps,
		zones:            zones,
		mapFilenames:     mapFilenames,
		mapStore:         mapStore,
		userStore:        userStore,
		banStore:         banStore,
		spriteStore:      spriteStore,
		extMgr:           NewExtensionManager(logger),
		tickHz:           tickHz,
		tickDur:          time.Second / time.Duration(tickHz),
		startTime:        time.Now(),
		logger:           logger,
		tracer:           otel.Tracer("worldsim"),
		rng:              rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)),
		entities:         make(map[string]*Entity),
		clients:          make(map[string]*Entity),
		entityIDToClient: make(map[string]string),
	}

	// Load base entities for all maps.
	for mapName, md := range s.maps {
		s.loadBaseEntities(mapName, md)
	}

	// Auto-seed the sprite_bases catalog from the bundled sprites directory on
	// first run. Non-fatal: if PB is down or seeding fails, worldsim still
	// starts and the frontend falls back to static char_0..char_3 sheets.
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

	// Wire up the extension options manager (PB + NATS).
	optsMgr := NewExtensionOptionsManager(app, nc, logger)
	s.extMgr.SetOptionsManager(optsMgr, nc)

	if err := s.subscribe(); err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	// Register a PB hook to reload the map immediately when the maps record
	// is updated, replacing the 30-second polling checker. The hook fires
	// in-process since PB is embedded.
	app.OnRecordAfterUpdateSuccess("maps").BindFunc(func(e *core.RecordEvent) error {
		mapName := e.Record.GetString("name")
		if _, ok := s.maps[mapName]; ok {
			s.logger.Info("map record updated via PB hook, reloading", "map", mapName)
			s.checkMapReload(mapName)
		}
		return e.Next()
	})

	// PB hook: when an extension_options record is updated (admin edits
	// options in the PB GUI), publish the updated options to the extension
	// via NATS so it can hot-reload its configuration.
	app.OnRecordAfterUpdateSuccess("extension_options").BindFunc(func(e *core.RecordEvent) error {
		extID := e.Record.GetString("extension_id")
		if extID != "" {
			s.logger.Info("extension options updated via PB hook", "extension", extID)
			optsMgr.PublishOptions(extID)
		}
		return e.Next()
	})
	app.OnRecordAfterCreateSuccess("extension_options").BindFunc(func(e *core.RecordEvent) error {
		extID := e.Record.GetString("extension_id")
		if extID != "" {
			s.logger.Info("extension options created via PB hook", "extension", extID)
			optsMgr.PublishOptions(extID)
		}
		return e.Next()
	})

	return s, nil
}

// loadBaseEntities spawns ECS entities for props defined on the map's
// "Entities" object layer (see mapdata.go PropEntity and
// documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md
// Part C). These have no NetworkSession and are inert until an extension
// claims them via an input trigger.
func (s *Simulator) loadBaseEntities(mapName string, md *MapData) {
	if md == nil {
		return
	}
	for _, pe := range md.Entities {
		s.entities[pe.ID] = &Entity{
			ID:             pe.ID,
			Position:       &pb.Position{X: pe.X, Y: pe.Y, MapId: mapName},
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
func (s *Simulator) reloadBaseEntities(mapName string, newEntities []*PropEntity) {
	newIDs := make(map[string]bool, len(newEntities))
	for _, pe := range newEntities {
		newIDs[pe.ID] = true
	}

	// Remove base entities that no longer exist in the new map.
	for id, e := range s.entities {
		if e.NetworkSession != nil {
			continue // player avatar, not a base entity
		}
		if e.Position == nil || e.Position.MapId != mapName {
			continue // belongs to a different map
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
			e.Position.MapId = mapName
			e.EntityType = pe.EntityType
			e.OwnerExtension = pe.OwnerExtension
			e.TriggerRadius = pe.TriggerRadius
			e.Gid = pe.Gid
			e.dirtyPosition = true
		} else {
			s.entities[pe.ID] = &Entity{
				ID:             pe.ID,
				Position:       &pb.Position{X: pe.X, Y: pe.Y, MapId: mapName},
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
		result := s.provisionClient(ctx, ar.ClientId, ar.GetSub(), ar.GetIp(), ar.GetDeviceId())
		// Respond with the entity ID, map_id, and admin flag so the pusher
		// can include them in the AuthResultFrame sent to the client. The
		// client needs the actual entity ID (which may differ from
		// "e_"+clientID[2:] when a PocketBase-stored identity exists) to
		// identify its own avatar, the map_id to know which tilemap to load
		// initially, and the admin flag to decide whether to subscribe to
		// the admin-only NATS channel. If the client is banned, the reply
		// carries the ban reason + expiry so the pusher can reject the
		// connection and the browser can display the ban message.
		if m.Reply != "" {
			resp, _ := proto.Marshal(&pb.AuthResultFrame{
				EntityId:      result.entityID,
				MapId:         result.mapID,
				IsAdmin:       result.isAdmin,
				Banned:        result.banned,
				BanReason:     result.banReason,
				BanUntil:      uint64(result.banUntil),
				MapOptions:    result.mapOptions,
				PlayerOptions: result.playerOptions,
			})
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

	// client.<id>.set_player_options — player options update request.
	if _, err := s.nc.Subscribe("client.*.set_player_options", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.handle_set_player_options_sub")
		defer span.End()
		clientID := subjectClientID(m.Subject, "set_player_options")
		span.SetAttributes(attribute.String("client.id", clientID))
		var frame pb.SetPlayerOptionsFrame
		if err := proto.Unmarshal(m.Data, &frame); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		s.handleSetPlayerOptions(ctx, clientID, &frame)
	}); err != nil {
		return fmt.Errorf("subscribe client.set_player_options: %w", err)
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

	// Extension-triggered map transitions: extensions can teleport a player
	// to a different map via NATS (e.g. clicking a door, admin teleport).
	// The target position is resolved by target_entity (beacon name) or, if
	// omitted, a random spawn zone on the target map.
	if _, err := s.nc.Subscribe("worldsim.entity.teleport", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.entity.teleport")
		defer span.End()
		var req struct {
			EntityID     string `json:"entity_id"`
			MapID        string `json:"map_id"`
			TargetEntity string `json:"target_entity,omitempty"`
		}
		if err := json.Unmarshal(m.Data, &req); err != nil {
			s.logger.WarnContext(ctx, "teleport unmarshal", "err", err)
			return
		}
		s.transitionEntity(ctx, req.EntityID, req.MapID, req.TargetEntity)
		audit.Emit(s.nc, "player.teleport", audit.SeverityInfo,
			audit.Actor{EntityID: req.EntityID},
			audit.Details{"target_map": req.MapID, "target_entity": req.TargetEntity},
			"")
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.entity.teleport: %w", err)
	}

	// Zone metadata request-reply handler: extensions fetch zone metadata on
	// startup/reconnect via worldsim.zones.get so they don't need to read
	// PocketBase directly.
	if err := s.subscribeZoneMetadata(); err != nil {
		return fmt.Errorf("subscribe worldsim.zones.get: %w", err)
	}

	// Stats request-reply handler: the audit service queries this for the
	// /world status page (per-map overview, players, extensions, zones).
	if err := s.subscribeStats(); err != nil {
		return fmt.Errorf("subscribe worldsim.stats.get: %w", err)
	}

	// Announce readiness so extensions can register against a live subscriber
	// instead of racing their initial publish (NATS Core drops publishes with
	// no subscribers). Flush guarantees the broadcast is on the wire before
	// the tick loop starts. Extensions also listen for this to re-register on
	// worldsim restarts.
	if err := s.nc.Publish("worldsim.ready", []byte(s.defaultMap)); err != nil {
		s.logger.Warn("publish worldsim.ready", "err", err)
	}
	if err := s.nc.Flush(); err != nil {
		return fmt.Errorf("flush worldsim.ready: %w", err)
	}
	s.logger.Info("worldsim ready", "default_map", s.defaultMap, "maps", len(s.maps))

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

	// Publish health to the "healthz" NATS subject every 10 seconds so the
	// pusher can aggregate and serve it via its HTTP /healthz endpoint.
	go s.startHealthPublisher(ctx)

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

// startHealthPublisher publishes a health JSON to the "healthz" NATS subject
// every 10 seconds. The pusher subscribes to this subject, aggregates the
// responses, and serves them via its HTTP /healthz endpoint.
func (s *Simulator) startHealthPublisher(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	s.publishHealth() // publish immediately on startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.publishHealth()
		}
	}
}

func (s *Simulator) publishHealth() {
	s.mu.Lock()
	entityCount := len(s.entities)
	playerCount := len(s.clients)
	s.mu.Unlock()
	runningExts := s.extMgr.ActiveCount()

	health := map[string]any{
		"service": "kernel",
		"status":  "OK",
		"version": version.Version,
		"uptime":  time.Since(s.startTime).Round(time.Second).String(),
		"extras": map[string]any{
			"entity_count":       entityCount,
			"connected_players":  playerCount,
			"running_extensions": runningExts,
		},
	}
	data, err := json.Marshal(health)
	if err != nil {
		s.logger.Warn("healthz marshal", "err", err)
		return
	}
	if err := s.nc.Publish("healthz", data); err != nil {
		s.logger.Warn("healthz publish", "err", err)
	}
}

// provisionResult is the return value of provisionClient. When banned is
// true, the other fields are zero — no entity is created.
type provisionResult struct {
	entityID      string
	mapID         string
	isAdmin       bool
	banned        bool
	banReason     string
	banUntil      int64
	mapOptions    string
	playerOptions string
}

// provisionClient creates a player avatar entity for the given client.
// If the user has a record in PocketBase (by user_id), their persistent
// entity_id and last position are restored. Otherwise a new user record
// is created. If the client matches an active ban (by user_id, IP, or
// device_id), no entity is created and the ban info is returned.
func (s *Simulator) provisionClient(ctx context.Context, clientID, sub, ip, deviceID string) provisionResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, exists := s.clients[clientID]; exists {
		mapOpts := ""
		if md := s.maps[existing.Position.MapId]; md != nil {
			mapOpts = string(md.Options)
		}
		return provisionResult{
			entityID:      existing.ID,
			mapID:         existing.Position.MapId,
			isAdmin:       existing.IsAdmin,
			mapOptions:    mapOpts,
			playerOptions: existing.PlayerOptions,
		}
	}

	defaultEntityID := "e_" + clientID[2:]
	spawnX, spawnY := float32(10), float32(10)
	mapName := s.defaultMap
	if mapName == "" {
		// Fallback: pick the first loaded map.
		for name := range s.maps {
			mapName = name
			break
		}
	}
	if md := s.maps[mapName]; md != nil {
		spawnX, spawnY = md.FindSpawnPoint(s.rng)
	}

	entityID := defaultEntityID
	displayName := ""
	spriteBase := ""
	isAdmin := false
	hideAdminBadge := false
	playerOptions := ""

	// Look up or create the user in PocketBase for persistent identity.
	if s.userStore != nil && sub != "" && sub != "dev" {
		user, err := s.userStore.FindOrCreateUser(sub, defaultEntityID, mapName, ip)
		if err != nil {
			s.logger.WarnContext(ctx, "user store lookup failed, using defaults",
				"err", err, "sub", sub)
		} else {
			entityID = user.EntityID
			displayName = user.DisplayName
			spriteBase = user.SpriteBase
			isAdmin = user.IsAdmin
			hideAdminBadge = user.HideAdminBadge
			playerOptions = user.Options
			// Restore saved position if it's valid (not 0,0 — the default).
			if user.PosX != 0 || user.PosY != 0 {
				spawnX, spawnY = user.PosX, user.PosY
			}
			// Restore saved map if it exists and is loaded.
			if user.MapID != "" {
				if _, ok := s.maps[user.MapID]; ok {
					mapName = user.MapID
				}
			}
			// Assign a random sprite_base from the catalog if the user
			// doesn't have one yet (new user or pre-existing record
			// created before sprite selection was added). Persist it so
			// the same sprite is used on reconnect.
			if spriteBase == "" && s.spriteStore != nil {
				if bases, err := s.spriteStore.ListBases(); err == nil && len(bases) > 0 {
					spriteBase = bases[s.rng.IntN(len(bases))].ID
					_ = s.userStore.UpdateSpriteBase(entityID, spriteBase)
				}
			}
		}
	}

	// Check ban list after the PB lookup so we know isAdmin. Admins are
	// exempt from bans — they can always connect. Guests (never admins)
	// and non-admin logged-in users are checked against all three
	// identifiers (user_id, IP, device_id).
	if !isAdmin && s.banStore != nil {
		if ban, found := s.banStore.CheckBan(sub, ip, deviceID); found {
			s.logger.InfoContext(ctx, "rejected banned client",
				"client", clientID, "sub", sub, "ip", ip, "device", deviceID,
				"reason", ban.Reason, "until", ban.BannedUntil)
			audit.Emit(s.nc, "player.banned", audit.SeverityWarn,
				audit.Actor{Sub: sub, ClientID: clientID, IP: ip, DeviceID: deviceID},
				audit.Details{"reason": ban.Reason, "until": ban.BannedUntil},
				"")
			return provisionResult{
				banned:    true,
				banReason: ban.Reason,
				banUntil:  ban.BannedUntil,
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
		Position: &pb.Position{X: spawnX, Y: spawnY, MapId: mapName, Dir: 0},
		NetworkSession: &NetworkSession{
			ClientID: clientID,
			Input:    &pb.InputState{},
		},
		DisplayName:   displayName,
		IsGuest:       sub == "" || sub == "dev",
		IP:            ip,
		DeviceID:      deviceID,
		IsAdmin:       isAdmin,
		HideAdminBadge: hideAdminBadge,
		SpriteBase:    spriteBase,
		PlayerOptions: playerOptions,
		spawnedTo:     make(map[string]bool),
		currentZones:  make(map[string]bool),
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
	if zr := s.zones[mapName]; zr != nil {
		zr.AddZone(e.mobileZone)
	}
	s.entities[entityID] = e
	s.clients[clientID] = e
	s.entityIDToClient[entityID] = clientID

	mapOpts := ""
	if md := s.maps[mapName]; md != nil {
		mapOpts = string(md.Options)
	}
	s.logger.InfoContext(ctx, "provisioned entity",
		"entity", entityID, "client", clientID, "sub", sub,
		"map", mapName, "x", e.Position.X, "y", e.Position.Y)
	audit.Emit(s.nc, "player.provisioned", audit.SeverityInfo,
		audit.Actor{Sub: sub, EntityID: entityID, ClientID: clientID, IP: ip, DeviceID: deviceID},
		audit.Details{"map": mapName, "x": e.Position.X, "y": e.Position.Y, "is_admin": isAdmin},
		"")
	return provisionResult{
		entityID:      entityID,
		mapID:         mapName,
		isAdmin:       isAdmin,
		mapOptions:    mapOpts,
		playerOptions: playerOptions,
	}
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
	mapIDForEvent := ""
	if e.Position != nil {
		mapIDForEvent = e.Position.MapId
	}
	for zid := range e.currentZones {
		s.publishZoneEvent(ctx, "zone.exit", e.ID, clientIDForEvent, zid, mapIDForEvent)
	}
	// Leave proximity group if any.
	if e.currentProximityGroup != "" {
		s.publishProximityEvent(ctx, "proximity.leave", e.ID, clientIDForEvent, e.currentProximityGroup, mapIDForEvent, nil)
	}
	// Remove the player's mobile proximity zone from the registry.
	if e.mobileZone != nil && e.Position != nil {
		if zr := s.zones[e.Position.MapId]; zr != nil {
			zr.RemoveZone(e.mobileZone.ID)
		}
	}
	delete(s.entities, e.ID)
	delete(s.clients, clientID)
	delete(s.entityIDToClient, e.ID)
	// Queue a DestroyEntity so the next replication tick notifies all other
	// clients. Without this, remaining clients never learn the entity is gone
	// and the avatar sprite stays on screen after the player disconnects.
	s.destroyedEntities = append(s.destroyedEntities, e.ID)
	posX, posY := e.Position.X, e.Position.Y
	mapID := e.Position.MapId
	entityID := e.ID
	s.mu.Unlock()

	// Save position and map_id to PocketBase outside the lock (network I/O).
	if s.userStore != nil {
		if err := s.userStore.SavePosition(entityID, posX, posY); err != nil {
			s.logger.WarnContext(ctx, "failed to save user position", "err", err, "entity", entityID)
		}
		if err := s.userStore.SaveMapID(entityID, mapID); err != nil {
			s.logger.WarnContext(ctx, "failed to save user map_id", "err", err, "entity", entityID)
		}
	}

	s.logger.InfoContext(ctx, "despawned entity", "entity", entityID, "client", clientID)
	audit.Emit(s.nc, "player.despawned", audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID},
		audit.Details{"map": mapID, "x": posX, "y": posY},
		"")
}

// handlePortalZone checks if the entered zone is a portal and triggers a map
// transition if so. Portal zones are defined in Tiled with zone_type="portal",
// target_map, target_x, and target_y properties. Caller must hold s.mu.
func (s *Simulator) handlePortalZone(ctx context.Context, e *Entity, zoneID, clientID string) {
	if e.NetworkSession == nil {
		return // only player avatars can transition
	}
	zr := s.zones[e.Position.MapId]
	if zr == nil {
		return
	}
	// Find the zone in the registry to check its properties.
	for _, z := range zr.zones {
		if z.ID != zoneID {
			continue
		}
		if z.PortalTargetMap == "" {
			return // not a portal zone
		}
		// Validate the target map exists.
		if _, ok := s.maps[z.PortalTargetMap]; !ok {
			s.logger.WarnContext(ctx, "portal target map not found",
				"entity", e.ID, "zone", zoneID, "target_map", z.PortalTargetMap)
			return
		}
		s.transitionEntity(ctx, e.ID, z.PortalTargetMap, z.PortalTargetEntity)
		return
	}
}

// transitionEntity moves an entity to a different map. The spawn position on
// the target map is resolved as follows:
//   - If targetEntity is set, teleport to that named base entity's position
//     (a "beacon"). Fails if the entity doesn't exist on the target map.
//   - Otherwise, pick a random "spawn" zone on the target map (FindSpawnPoint).
//
// It:
// 1. Resolves the spawn position (beacon or random spawn point).
// 2. Removes the entity's mobile zone from the old map's zone registry.
// 3. Changes Position.MapId, X, Y to the target.
// 4. Re-adds the mobile zone to the new map's zone registry.
// 5. Resets spawnedTo so the entity re-spawns for clients on the new map.
// 6. Sends a MapTransitionFrame to the client so the frontend loads the new map.
// 7. Persists the new map_id to PocketBase.
func (s *Simulator) transitionEntity(ctx context.Context, entityID, targetMap, targetEntity string) {
	s.mu.Lock()
	e, ok := s.entities[entityID]
	if !ok {
		s.mu.Unlock()
		return
	}

	targetMD := s.maps[targetMap]
	if targetMD == nil {
		s.logger.WarnContext(ctx, "transition target map not loaded",
			"entity", entityID, "target_map", targetMap)
		s.mu.Unlock()
		return
	}

	// Resolve spawn position on the target map.
	var spawnX, spawnY float32
	if targetEntity != "" {
		beacon := targetMD.FindEntityByName(targetEntity)
		if beacon == nil {
			s.logger.WarnContext(ctx, "transition target entity not found on target map",
				"entity", entityID, "target_map", targetMap, "target_entity", targetEntity)
			s.mu.Unlock()
			return
		}
		spawnX, spawnY = beacon.X, beacon.Y
	} else {
		spawnX, spawnY = targetMD.FindSpawnPoint(s.rng)
	}

	oldMap := e.Position.MapId

	// Remove mobile zone from old map's zone registry.
	if e.mobileZone != nil {
		if zr := s.zones[oldMap]; zr != nil {
			zr.RemoveZone(e.mobileZone.ID)
		}
	}

	// Update position to the new map.
	e.Position.MapId = targetMap
	e.Position.X = spawnX
	e.Position.Y = spawnY
	e.dirtyPosition = true

	// Clear current zones — the entity is on a new map with different zones.
	e.currentZones = make(map[string]bool)

	// Reset spawnedTo so the entity re-spawns for all clients on the new map.
	e.spawnedTo = make(map[string]bool)

	// Re-add mobile zone to the new map's zone registry.
	if e.mobileZone != nil {
		if zr := s.zones[targetMap]; zr != nil {
			e.mobileZone.X = spawnX - proximityRadius
			e.mobileZone.Y = spawnY + avatarFeetYOffset - proximityRadius
			zr.AddZone(e.mobileZone)
		}
	}

	// Queue a DestroyEntity for clients on the old map. Clients on the new
	// map will get a SpawnEntity via the normal replication loop.
	s.destroyedEntities = append(s.destroyedEntities, entityID)

	clientID := ""
	if e.NetworkSession != nil {
		clientID = e.NetworkSession.ClientID
	}
	mapOpts := ""
	if md := s.maps[targetMap]; md != nil {
		mapOpts = string(md.Options)
	}
	s.mu.Unlock()

	// Send MapTransitionFrame to the client so the frontend loads the new map.
	if clientID != "" {
		frame := &pb.ServerFrame{
			Payload: &pb.ServerFrame_MapTransition{
				MapTransition: &pb.MapTransitionFrame{
					MapId:       targetMap,
					SpawnX:      spawnX,
					SpawnY:      spawnY,
					MapOptions:  mapOpts,
				},
			},
		}
		frameBytes, _ := proto.Marshal(frame)
		subject := fmt.Sprintf("client.%s.replication", clientID)
		if err := s.nc.Publish(subject, frameBytes); err != nil {
			s.logger.WarnContext(ctx, "map transition publish", "err", err, "client", clientID)
		}
	}

	// Persist the new map_id to PocketBase.
	if s.userStore != nil {
		if err := s.userStore.SaveMapID(entityID, targetMap); err != nil {
			s.logger.WarnContext(ctx, "failed to save user map_id", "err", err, "entity", entityID)
		}
	}

	s.logger.InfoContext(ctx, "entity transitioned to new map",
		"entity", entityID, "old_map", oldMap, "new_map", targetMap,
		"target_entity", targetEntity, "x", spawnX, "y", spawnY)
	audit.Emit(s.nc, "player.map_transition", audit.SeverityInfo,
		audit.Actor{EntityID: entityID},
		audit.Details{"old_map": oldMap, "new_map": targetMap, "target_entity": targetEntity, "x": spawnX, "y": spawnY},
		"")
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

	audit.Emit(s.nc, "chat.message", audit.SeverityInfo,
		audit.Actor{EntityID: sender.ID, ClientID: clientID},
		audit.Details{"channel": chat.GetChannel(), "text": text, "display_name": sender.DisplayName},
		"")

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

	audit.Emit(s.nc, "player.set_name", audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID},
		audit.Details{"name": name},
		"")

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

	audit.Emit(s.nc, "player.set_sprite_base", audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID},
		audit.Details{"sprite_base": spriteBase},
		"")

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

// handleSetPlayerOptions processes a client-sent SetPlayerOptionsFrame: updates
// Entity.PlayerOptions in memory and persists the full options JSON to
// PocketBase for logged-in users. Guests have no PB record — options are
// session-only. The options field is a full JSON replace (not a partial merge).
func (s *Simulator) handleSetPlayerOptions(ctx context.Context, clientID string, frame *pb.SetPlayerOptionsFrame) {
	ctx, span := s.tracer.Start(ctx, "worldsim.handle_set_player_options")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", clientID))

	options := frame.GetOptions()

	s.mu.Lock()
	entity, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		span.SetStatus(codes.Error, "unknown client")
		return
	}
	entity.PlayerOptions = options
	entityID := entity.ID
	s.mu.Unlock()

	audit.Emit(s.nc, "player.set_player_options", audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID},
		audit.Details{"options": options},
		"")
	// Persist to PocketBase for logged-in users. Guests have no
	// PocketBase record, so findByEntityID returns nil and the update is
	// a no-op — matching handleSetName's "guest names are session-only".
	if s.userStore != nil {
		if err := s.userStore.UpdateOptions(entityID, options); err != nil {
			s.logger.WarnContext(ctx, "persist player options failed",
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
		// Look up the zone registry for this entity's current map.
		zr := s.zones[e.Position.MapId]
		if zr == nil {
			e.currentZones = make(map[string]bool)
			continue
		}
		// Evaluate zone membership at the avatar's feet (see
		// avatarFeetYOffset) so enter/exit transitions match where the
		// player visually crosses a zone boundary.
		newZones := zr.ZonesAtPoint(e.Position.X, e.Position.Y+avatarFeetYOffset)
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
				s.publishZoneEvent(ctx, "zone.enter", e.ID, clientID, zid, e.Position.MapId)
				// Check for portal zones — handle map transition.
				s.handlePortalZone(ctx, e, zid, clientID)
			}
		}
		for zid := range e.currentZones {
			if !newSet[zid] {
				s.publishZoneEvent(ctx, "zone.exit", e.ID, clientID, zid, e.Position.MapId)
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
	// Track entities spawned in this batch so we can send admin info for
	// them if the client is an admin.
	var spawnedEntities []*Entity

	for _, e := range s.entities {
		// Multi-map filtering: only replicate entities on the same map as the
		// client. The client's own entity is always included.
		if e != clientEntity && e.Position != nil && e.Position.MapId != clientEntity.Position.MapId {
			continue
		}

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
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName, IsGuest: e.IsGuest, IsAdmin: e.IsAdmin && !e.HideAdminBadge})
				components = append(components, &pb.ComponentData{ComponentId: compDisplayName, Data: nameBytes})
			}
			batch.Spawns = append(batch.Spawns, &pb.SpawnEntity{
				EntityId:    e.ID,
				SnapshotSeq: s.snapshotSeq,
				Components:  components,
			})
			e.spawnedTo[clientID] = true
			spawnedEntities = append(spawnedEntities, e)
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
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName, IsGuest: e.IsGuest, IsAdmin: e.IsAdmin && !e.HideAdminBadge})
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

	// If the client is an admin and we spawned new entities in this batch,
	// send admin-only info (IP, guest status, OIDC sub) for those entities
	// via the admin-only NATS channel. Non-admin clients never receive this.
	if clientEntity.IsAdmin && len(spawnedEntities) > 0 {
		s.publishAdminInfo(rctx, clientID, spawnedEntities)
	}
	return true
}

// publishAdminInfo sends an AdminInfoFrame for the given entities to the
// admin client's admin-only NATS subject (client.<id>.admin). Only called
// for admin clients — the pusher subscribes to this subject only for admin
// sessions, so the data never reaches non-admin browsers.
func (s *Simulator) publishAdminInfo(ctx context.Context, adminClientID string, entities []*Entity) {
	infos := make([]*pb.AdminInfoFrame_EntityAdminInfo, 0, len(entities))
	for _, e := range entities {
		infos = append(infos, &pb.AdminInfoFrame_EntityAdminInfo{
			EntityId: e.ID,
			Ip:       e.IP,
			IsGuest:  e.IsGuest,
			UserId:   "", // user_id is not stored on the Entity; available in PB
			DeviceId: e.DeviceID,
		})
	}
	frame := &pb.ServerFrame{Payload: &pb.ServerFrame_AdminInfo{AdminInfo: &pb.AdminInfoFrame{Entities: infos}}}
	frameBytes, err := proto.Marshal(frame)
	if err != nil {
		s.logger.WarnContext(ctx, "admin info marshal", "client", adminClientID, "err", err)
		return
	}
	subject := fmt.Sprintf("client.%s.admin", adminClientID)
	msg := &nats.Msg{Subject: subject, Data: frameBytes}
	otelinternal.Inject(ctx, msg)
	if err := s.nc.PublishMsg(msg); err != nil {
		s.logger.WarnContext(ctx, "admin info publish", "client", adminClientID, "err", err)
	}
}

// runIntegrityCheck validates all loaded maps and logs any issues.
func (s *Simulator) runIntegrityCheck() {
	for mapName, md := range s.maps {
		results := CheckMapIntegrity(md)
		LogIntegrityResults(s.logger, results, mapName)
		errors, warnings, infos := 0, 0, 0
		for _, r := range results {
			switch r.Level {
			case LevelError:
				errors++
			case LevelWarning:
				warnings++
			case LevelInfo:
				infos++
			}
		}
		sev := audit.SeverityInfo
		if errors > 0 {
			sev = audit.SeverityError
		} else if warnings > 0 {
			sev = audit.SeverityWarn
		}
		audit.Emit(s.nc, "map.integrity_check", sev,
			audit.Actor{},
			audit.Details{"map": mapName, "errors": errors, "warnings": warnings, "infos": infos},
			"")
	}
}

// startMapReloadChecker periodically checks if any loaded map has been
// updated in PocketBase (by comparing the tiled_json filename). If changed,
// reloads that map, rebuilds its zone registry, and publishes a map.updated
// event on NATS so extensions can re-read the map too.
func (s *Simulator) startMapReloadChecker(ctx context.Context) {
	// Get the initial filenames for all loaded maps.
	s.mu.Lock()
	for mapName := range s.maps {
		info, err := s.mapStore.FetchMapRecordInfo(mapName)
		if err != nil {
			s.logger.Warn("map reload checker: failed to get initial record info", "err", err, "map", mapName)
			continue
		}
		s.mapFilenames[mapName] = info.TiledJSONFilename
	}
	s.mu.Unlock()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for mapName := range s.maps {
				s.checkMapReload(mapName)
			}
		}
	}
}

// checkMapReload fetches the current map record info and compares the
// filename and options. If the filename changed, reloads the map. If only the
// options changed, updates in-memory options and pushes a MapOptionsUpdateFrame
// to connected clients on that map (hot-reload).
func (s *Simulator) checkMapReload(mapName string) {
	info, err := s.mapStore.FetchMapRecordInfo(mapName)
	if err != nil || info == nil {
		return // PocketBase might be temporarily unreachable
	}

	s.mu.Lock()
	oldFilename := s.mapFilenames[mapName]
	oldOptions := ""
	if md := s.maps[mapName]; md != nil {
		oldOptions = string(md.Options)
	}
	s.mu.Unlock()

	newOptions := string(info.Options)

	// If only options changed (filename unchanged), hot-reload options without
	// a full map reload.
	if info.TiledJSONFilename == oldFilename {
		if newOptions == oldOptions {
			return // no change
		}
		s.logger.Info("map options updated, hot-reloading",
			"map", mapName, "old_options", oldOptions, "new_options", newOptions)

		s.mu.Lock()
		if md := s.maps[mapName]; md != nil {
			md.Options = info.Options
		}
		// Collect client IDs of players on this map for hot-reload push.
		var clientIDs []string
		for _, e := range s.clients {
			if e.Position != nil && e.Position.MapId == mapName {
				if e.NetworkSession != nil {
					clientIDs = append(clientIDs, e.NetworkSession.ClientID)
				}
			}
		}
		s.mu.Unlock()

		// Push MapOptionsUpdateFrame to each connected client on this map.
		frame := &pb.ServerFrame{
			Payload: &pb.ServerFrame_MapOptionsUpdate{
				MapOptionsUpdate: &pb.MapOptionsUpdateFrame{
					MapOptions: newOptions,
				},
			},
		}
		frameBytes, _ := proto.Marshal(frame)
		for _, cid := range clientIDs {
			subject := fmt.Sprintf("client.%s.replication", cid)
			if err := s.nc.Publish(subject, frameBytes); err != nil {
				s.logger.Warn("map options hot-reload publish", "err", err, "client", cid)
			}
		}
		return
	}

	s.logger.Info("map updated, reloading",
		"map", mapName,
		"old_file", oldFilename,
		"new_file", info.TiledJSONFilename)
	audit.Emit(s.nc, "map.reloaded", audit.SeverityInfo,
		audit.Actor{},
		audit.Details{"map": mapName, "old_file": oldFilename, "new_file": info.TiledJSONFilename},
		"")

	// Reload the map.
	newMapData, err := s.mapStore.LoadMapData(mapName)
	if err != nil {
		s.logger.Error("map reload failed", "err", err, "map", mapName)
		return
	}

	s.mu.Lock()
	s.maps[mapName] = newMapData
	s.zones[mapName] = NewZoneRegistry(newMapData.Zones, newMapData.Width, newMapData.Height)
	s.mapFilenames[mapName] = info.TiledJSONFilename
	s.reloadBaseEntities(mapName, newMapData.Entities)
	// Re-add mobile proximity zones for connected players on this map — the
	// registry was rebuilt from scratch above, wiping them.
	for _, e := range s.clients {
		if e.mobileZone != nil && e.Position != nil && e.Position.MapId == mapName {
			s.zones[mapName].AddZone(e.mobileZone)
		}
	}
	s.mu.Unlock()

	// Run integrity check on the new map.
	results := CheckMapIntegrity(newMapData)
	LogIntegrityResults(s.logger, results, mapName)

	// Notify extensions that the map has been updated.
	s.nc.Publish("map.updated", []byte(mapName))
	// Broadcast updated zone metadata so extensions can refresh their zone
	// sets without reading PocketBase directly.
	s.broadcastZoneMetadata()
	s.logger.Info("map reloaded and map.updated event published", "map", mapName)
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

		md := s.maps[e.Position.MapId]
		zr := s.zones[e.Position.MapId]
		if md != nil {
			// Clamp to map bounds.
			newX = clamp(newX, 0, float32(md.Width-1))
			newY = clamp(newY, 0, float32(md.Height-1))

			// Collision check: try X and Y independently so the avatar
			// slides along walls instead of sticking. Swept (segment-vs-
			// shape) checks catch walls thinner than the per-tick movement
			// that point-sampling at the destination would miss.
			if s.isMoveBlocked(zr, md, e.Position.X, e.Position.Y, newX, e.Position.Y) {
				newX = e.Position.X
			}
			if s.isMoveBlocked(zr, md, newX, e.Position.Y, newX, newY) {
				newY = e.Position.Y
			}
			// Diagonal guard: if both axes moved, check the full diagonal
			// segment. The X-then-Y decomposition can skip a wall that the
			// diagonal crosses but neither axis-aligned segment does (the
			// X move jumps past a thin wall, then the Y move sits outside
			// its X range). If the diagonal is blocked, revert Y to slide
			// along the X axis.
			if newX != e.Position.X && newY != e.Position.Y {
				if s.isMoveBlocked(zr, md, e.Position.X, e.Position.Y, newX, newY) {
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
func (s *Simulator) isMoveBlocked(zr *ZoneRegistry, md *MapData, oldX, oldY, newX, newY float32) bool {
	// Translate to feet space.
	ofy := oldY + avatarFeetYOffset
	nfy := newY + avatarFeetYOffset
	r := playerCollisionRadius

	// Zone gate triggers: swept segment-vs-shape against each blocked zone.
	// Each shape is expanded by the player collision radius (Minkowski sum)
	// so the feet center stops `r` tiles before the wall edge, matching the
	// old 5-point sampling box width.
	if zr != nil {
		for _, z := range zr.zones {
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
	if md != nil {
		if md.IsBlocked(int(oldX+0.5), int(ofy+0.5)) {
			return true
		}
		if md.IsBlocked(int(newX+0.5), int(nfy+0.5)) {
			return true
		}
	}
	return false
}

// publishZoneEvent publishes a zone.enter or zone.exit event to NATS.
// Extensions subscribe to these subjects to observe zone transitions.
// clientID is the player's client_id (empty for base entities without a
// NetworkSession); extensions like ext-av use it to address token replies.
// mapID is the map the entity is on when the zone event fires.
func (s *Simulator) publishZoneEvent(ctx context.Context, event, entityID, clientID, zoneID, mapID string) {
	subject := event // event already contains the full subject (e.g. "zone.enter")
	data := fmt.Sprintf(`{"entity_id":"%s","client_id":"%s","zone_id":"%s","map_id":"%s"}`, entityID, clientID, zoneID, mapID)
	if err := s.nc.Publish(subject, []byte(data)); err != nil {
		s.logger.WarnContext(ctx, "zone event publish", "event", event, "err", err)
	}
	s.logger.InfoContext(ctx, "zone event", "event", event, "entity", entityID, "zone", zoneID)
	audit.Emit(s.nc, event, audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID},
		audit.Details{"zone": zoneID, "map": mapID},
		"")
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
	// Build a set of av_enabled zone IDs across all maps. Players inside
	// these zones get zone-based A/V instead of proximity A/V.
	avZones := make(map[string]bool)
	for _, zr := range s.zones {
		if zr == nil {
			continue
		}
		for _, z := range zr.zones {
			if z.AvEnabled {
				avZones[z.ID] = true
			}
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
	//
	// Group IDs are stable across membership changes: if any member of the
	// connected component already has a currentProximityGroup, that ID is
	// reused. A new ID is minted only for brand-new groups (no member has
	// an existing group). This prevents all existing members from being
	// torn down and re-joined to a new LiveKit room when a player joins or
	// leaves the group — only the joining/leaving player gets an event.
	newGroup := make(map[string]string)       // entity_id -> group_id
	groupMembers := make(map[string][]string) // group_id -> sorted member entity IDs
	for _, comp := range groups {
		if len(comp) < 2 {
			// Singleton — no proximity group.
			continue
		}
		sorted := append([]string(nil), comp...)
		sort.Strings(sorted)
		// Reuse an existing group ID from any member already in a group.
		// This keeps the LiveKit room stable when members join/leave.
		var gid string
		for _, id := range sorted {
			if e, ok := s.entities[id]; ok && e.currentProximityGroup != "" {
				gid = e.currentProximityGroup
				break
			}
		}
		if gid == "" {
			// Brand-new group: mint a new ID from the member hash.
			h := fnv.New64a()
			for _, id := range sorted {
				h.Write([]byte(id))
				h.Write([]byte{0}) // separator
			}
			gid = fmt.Sprintf("proxgroup-%016x", h.Sum64())
		}
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
			s.publishProximityEvent(ctx, "proximity.leave", e.ID, clientID, old, e.Position.MapId, nil)
		}

		// Join new group (if any).
		if newG != "" {
			s.publishProximityEvent(ctx, "proximity.join", e.ID, clientID, newG, e.Position.MapId, groupMembers[newG])
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
			s.publishProximityEvent(ctx, "proximity.leave", e.ID, clientID, e.currentProximityGroup, e.Position.MapId, nil)
			e.currentProximityGroup = ""
		}
	}
}

// publishProximityEvent publishes a proximity.join or proximity.leave event
// to NATS. ext-av subscribes to these to mint LiveKit tokens for proximity
// A/V rooms.
func (s *Simulator) publishProximityEvent(ctx context.Context, event, entityID, clientID, groupID, mapID string, members []string) {
	payload := proximityEventPayload{
		EntityID: entityID,
		ClientID: clientID,
		GroupID:  groupID,
		MapID:    mapID,
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

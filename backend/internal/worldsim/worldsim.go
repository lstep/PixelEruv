// Package worldsim is the spatial authority and replication gateway.
// For the lite MVP it runs a fixed tick loop, a minimal hand-rolled ECS
// (Position + NetworkSession), player avatar movement, and replication.
package worldsim

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/apis"
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
	compPosition     = 1
	compEntityState  = 2
	compAppearance   = 3
	compDisplayName  = 4
	compLightEmitter = 5
)

// Player presence status, replicated as DisplayName.status and used to gate
// A/V. DND fully excludes the player from A/V rooms.
const (
	statusAvailable    = 0
	statusBusy         = 1
	statusDoNotDisturb = 2
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
	// GidOff is the original gid from map load (the "off" sprite). Stored
	// separately so the extension can reference it when returning
	// AppearanceUpdates. Equal to Gid at load time.
	GidOff uint32
	// GidOn is the alternate sprite gid for the "on" state (0 = no alternate).
	// Passed to extensions in the dispatch payload so they can decide which gid
	// to set via AppearanceUpdates.
	GidOn uint32
	// OnInteractAction is the action_id fired immediately when the player
	// presses E near this entity (immediate mode). Empty for popup-mode entities.
	OnInteractAction string
	// Actions is a comma-separated list of action_ids shown in the interaction
	// popup (popup mode). Empty for immediate-mode entities.
	Actions string
	// Interactions maps action_id to a list of effects. Each effect has an
	// action verb, optional payload, and target IDs. Extensions read this to
	// know what to do when an action is triggered.
	Interactions map[string][]Effect
	// SpriteBase is the sprite_bases PocketBase record ID selecting the
	// character sheet for player avatars. Set at provision time from the
	// player's persisted choice. Empty for base entities (they use Gid) and
	// for guests (frontend falls back to a hash-based index).
	SpriteBase string
	// State is a generic opaque string (EntityState component) that
	// extensions can set via an input-trigger reply, e.g. "on"/"off".
	State string
	// LightEmitter component: intensity 0-100 (0 = no light), color 0xRRGGBB
	// (0 = default warm white 0xffe6b4), radius in tiles (0 = default 3).
	// Set from Tiled entity attributes at provisioning and updated at runtime
	// via the set_light effect verb in ext-props.
	LightIntensity uint32
	LightColor     uint32
	LightRadius    float32
	// dirty: which components changed since last replication tick
	dirtyPosition     bool
	dirtyState        bool
	dirtyName         bool
	dirtyAppearance   bool
	dirtyLightEmitter bool
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
	// stationaryTicks counts consecutive ticks the player has not moved.
	// Used to gate proximity A/V activation: a player must be stationary for
	// proximityStationaryThreshold ticks before proximity.join fires, so
	// walking past another player without stopping does not trigger A/V.
	// Reset to 0 on movement (dirtyPosition), incremented each stationary tick.
	stationaryTicks int
	// PlayerOptions is the JSON-encoded player options string (e.g.
	// {"show_own_name_tag":true}). Persisted to PocketBase for logged-in
	// users; session-only for guests. Updated via SetPlayerOptionsFrame.
	PlayerOptions string
	// Status is the player's presence status (statusAvailable/Busy/DND),
	// replicated as DisplayName.status and rendered as the nametag pill
	// color. DND fully excludes the player from A/V. Persisted to PocketBase
	// (players.status) on every change and restored at provision time, so it
	// survives page reloads. Guests have no PB record and are session-only.
	Status uint32
	// lastHeartbeat is the last time the pusher confirmed the WebSocket is
	// alive (via client.<id>.heartbeat, published on each successful WS ping).
	// The client reaper despawns entities whose heartbeat is older than
	// clientHeartbeatTimeout, so an entity survives a lost client.disconnected
	// (e.g. pusher crash/restart) instead of lingering forever. Only set for
	// player avatars (provisioned via client.connected).
	lastHeartbeat time.Time
}

type NetworkSession struct {
	ClientID string
	// Latest input state from the client
	Input *pb.InputState
	Seq   uint32
}

// --- World Sim ---

type Simulator struct {
	World // embedded — s.entities, s.maps, s.zones, etc. promoted

	nc                *nats.Conn
	app               core.App
	defaultMap        string
	mapFilenames      map[string]string // mapName → last known tiled_json filename
	mapStore          *MapStore
	userStore         *UserStore
	banStore          *BanStore
	recordingStore    *RecordingStore
	spriteStore       *SpriteStore
	extMgr            *ExtensionManager
	movement          *MovementSystem
	zone              *ZoneSystem
	zoneSink          ZoneSink
	proximity         *ProximitySystem
	proximitySink     ProximitySink
	replication       *ReplicationSystem
	replicationSink   ReplicationSink
	portal            *PortalSystem
	portalSink        PortalSink
	worldOpts         *WorldOptionsManager
	worldOptsOnUpdate []func(WorldOptions)
	tickHz            int
	tickDur           time.Duration
	startTime         time.Time
	logger            *slog.Logger
	tracer            trace.Tracer

	mu sync.Mutex

	// lastSavedPos records the last position/map persisted to PocketBase per
	// player entity, so startPositionPersister can skip writes for entities
	// that haven't moved since the last 30s tick. Cleared in despawnClient.
	lastSavedPos map[string]savedPos
}

// savedPos is the last position persisted to PocketBase for a player entity,
// used by startPositionPersister to skip unchanged entities.
type savedPos struct {
	x, y  float32
	mapID string
}

// selectDefaultMap returns the name of the map marked is_default in the PB
// records. If no map has is_default=true and records is non-empty, it returns
// an error (the admin must mark exactly one in the PocketBase admin UI). If
// records is empty, it returns "" — the caller handles that case (seeding or
// fallback bounds).
func selectDefaultMap(records []*MapRecordInfo) (string, error) {
	if len(records) == 0 {
		return "", nil
	}
	for _, r := range records {
		if r.IsDefault {
			return r.Name, nil
		}
	}
	return "", fmt.Errorf("no map marked is_default; mark exactly one map in the PocketBase admin UI")
}

func New(natsURL string, app core.App, tickHz int, logger *slog.Logger) (*Simulator, error) {
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
	recordingStore := NewRecordingStore(app)
	spriteStore := NewSpriteStore(app)

	// Auto-seed the bundled "main" map into PocketBase on first run (when no
	// maps exist yet), so a fresh deployment boots without a manual upload
	// step. MAP_DIR defaults to ./maps (bundled in dist/) for production; for
	// local dev, set MAP_DIR to the repo's maps/ directory. Non-fatal: if
	// seeding fails, worldsim still starts and the error below surfaces.
	mapDir := os.Getenv("MAP_DIR")
	if mapDir == "" {
		mapDir = "./maps"
	}

	// Load all maps from PocketBase.
	mapRecords, err := mapStore.ListAllMaps()
	if err != nil {
		logger.Warn("failed to list maps", "err", err)
	}

	// Seed only when no maps exist, so we don't overwrite an admin's choice.
	if len(mapRecords) == 0 {
		if err := mapStore.SeedMapIfMissing("main", mapDir, "default-map.json"); err != nil {
			logger.Warn("map seed failed", "err", err, "dir", mapDir)
		} else {
			// Re-list to pick up the seeded record.
			if r, err := mapStore.ListAllMaps(); err == nil {
				mapRecords = r
			}
		}
	}

	// Derive the default map (where new players spawn) from the is_default
	// flag in PB. Fails fast if maps exist but none is marked default — the
	// admin must mark exactly one in the PocketBase admin UI.
	defaultMap, err := selectDefaultMap(mapRecords)
	if err != nil {
		nc.Close()
		return nil, err
	}
	logger.Info("default map selected", "default_map", defaultMap)

	maps := make(map[string]*MapData)
	zones := make(map[string]*ZoneRegistry)
	mapFilenames := make(map[string]string)
	mapErrors := make(map[string]string)
	mapWarnings := make(map[string][]*pb.MapWarning)
	for _, mr := range mapRecords {
		md, err := mapStore.LoadMapData(mr.Name)
		if err != nil {
			logger.Warn("failed to load map", "err", err, "map", mr.Name)
			mapErrors[mr.Name] = fmt.Sprintf("failed to load map: %v", err)
			continue
		}
		// Validate before storing. Fatal issues block the map from loading;
		// warnings are stored and sent to clients on connect/transition.
		issues := CheckMapIntegrity(md)
		var fatalMsgs []string
		var warningMsgs []*pb.MapWarning
		for _, r := range issues {
			if r.Level == LevelError {
				fatalMsgs = append(fatalMsgs, r.String())
			} else if r.Level == LevelWarning {
				warningMsgs = append(warningMsgs, &pb.MapWarning{
					EntityId: r.EntityID,
					Message:  r.Message,
				})
			}
		}
		LogIntegrityResults(logger, issues, mr.Name)
		if len(fatalMsgs) > 0 {
			mapErrors[mr.Name] = fmt.Sprintf("map validation failed: %s", strings.Join(fatalMsgs, "; "))
			logger.Error("map rejected due to validation errors", "map", mr.Name, "errors", len(fatalMsgs))
			continue
		}
		maps[mr.Name] = md
		zones[mr.Name] = NewZoneRegistry(md.Zones, md.Width, md.Height)
		mapFilenames[mr.Name] = mr.TiledJSONFilename
		if len(warningMsgs) > 0 {
			mapWarnings[mr.Name] = warningMsgs
		}
		logger.Info("loaded map", "map", mr.Name, "width", md.Width, "height", md.Height, "warnings", len(warningMsgs))
	}

	// Fallback: if no maps were loaded (e.g. seeding failed and PB is empty),
	// use fallback bounds so worldsim can still start for diagnostics.
	if len(maps) == 0 {
		fallbackName := defaultMap
		if fallbackName == "" {
			fallbackName = "main"
		}
		logger.Warn("no maps loaded, using fallback bounds", "default_map", fallbackName)
		md := &MapData{Width: 20, Height: 20}
		maps[fallbackName] = md
		zones[fallbackName] = NewZoneRegistry(md.Zones, md.Width, md.Height)
		defaultMap = fallbackName
	}

	s := &Simulator{
		World: World{
			maps:             maps,
			zones:            zones,
			mapErrors:        mapErrors,
			mapWarnings:      mapWarnings,
			rng:              rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)),
			entities:         make(map[string]*Entity),
			clients:          make(map[string]*Entity),
			entityIDToClient: make(map[string]string),
		},
		nc:             nc,
		app:            app,
		defaultMap:     defaultMap,
		mapFilenames:   mapFilenames,
		mapStore:       mapStore,
		userStore:      userStore,
		banStore:       banStore,
		recordingStore: recordingStore,
		spriteStore:    spriteStore,
		extMgr:         NewExtensionManager(logger),
		tickHz:         tickHz,
		movement:       nil, // set after extMgr below
		tickDur:        time.Second / time.Duration(tickHz),
		startTime:      time.Now(),
		logger:         logger,
		tracer:         otel.Tracer("worldsim"),
		lastSavedPos:   make(map[string]savedPos),
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

	// Construct the movement system (needs extMgr for gate-trigger checks).
	s.movement = NewMovementSystem(s.extMgr)

	// Construct the zone system (needs a ZoneSink for publishing events).
	s.zoneSink = NewNatZoneSink(nc, logger)
	s.zone = NewZoneSystem(s.zoneSink, logger)

	// Construct the proximity system (needs a ProximitySink for publishing events).
	s.proximitySink = NewNatProximitySink(nc, logger)
	s.proximity = NewProximitySystem(s.proximitySink, logger)

	// Construct the replication system (needs a ReplicationSink for publishing).
	s.replicationSink = NewNatReplicationSink(nc, logger, s.tracer)
	s.replication = NewReplicationSystem(s.replicationSink, s.tracer)

	// Construct the portal system (needs a PortalSink + the world mutex).
	s.portalSink = NewNatPortalSink(nc, logger, userStore)
	s.portal = NewPortalSystem(s.portalSink, logger, &s.mu)

	// Wire up the world options manager (NATS KV bucket "world_options").
	// PUBLIC_HOST and LIVEKIT_PUBLIC_URL are mirrored read-only from env.
	publicHost := os.Getenv("PUBLIC_HOST")
	if publicHost == "" {
		publicHost = "localhost"
	}
	// Default LIVEKIT_PUBLIC_URL to ws://<publicHost>:7880 when unset, matching
	// ext-av/main.go and the compose default. Without this, running worldsim
	// outside Docker (make debug, direct binary) would store an empty string
	// in the KV bucket and expose it via /api/world-options, breaking the
	// frontend's LiveKit connection.
	livekitPublicURL := os.Getenv("LIVEKIT_PUBLIC_URL")
	if livekitPublicURL == "" {
		livekitPublicURL = "ws://" + publicHost + ":7880"
	}
	worldOpts, err := NewWorldOptionsManager(nc, logger, publicHost, livekitPublicURL)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("world options: %w", err)
	}
	s.worldOpts = worldOpts

	// Register a read-only HTTP endpoint on the embedded PocketBase for the
	// frontend to fetch the YouTube RTMP defaults (used by the "Stream to
	// YouTube" confirm modal). Admin-gated via the users JWT + players.is_admin
	// check. Returns only the YouTube fields + public_host, not the full
	// options (SMTP password etc. stay server-side).
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.GET("/api/world-options", s.handleWorldOptionsHTTP).Bind(apis.RequireAuth("users"))
		// /api/world-king is public (no auth) — returns only the king's
		// display name for the welcome page footer. King email is not exposed.
		e.Router.GET("/api/world-king", s.handleWorldKingHTTP)
		return nil
	})

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
			ID:               pe.ID,
			Position:         &pb.Position{X: pe.X, Y: pe.Y, MapId: mapName},
			EntityType:       pe.EntityType,
			OwnerExtension:   pe.OwnerExtension,
			TriggerRadius:    pe.TriggerRadius,
			Gid:              pe.Gid,
			GidOff:           pe.Gid,
			GidOn:            pe.GidOn,
			OnInteractAction: pe.OnInteractAction,
			Actions:          pe.Actions,
			Interactions:     pe.Interactions,
			LightIntensity:   pe.LightIntensity,
			LightColor:       pe.LightColor,
			LightRadius:      pe.LightRadius,
			spawnedTo:        make(map[string]bool),
			currentZones:     make(map[string]bool),
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
			e.GidOff = pe.Gid
			e.GidOn = pe.GidOn
			e.OnInteractAction = pe.OnInteractAction
			e.Actions = pe.Actions
			e.Interactions = pe.Interactions
			if e.LightIntensity != pe.LightIntensity || e.LightColor != pe.LightColor || e.LightRadius != pe.LightRadius {
				e.LightIntensity = pe.LightIntensity
				e.LightColor = pe.LightColor
				e.LightRadius = pe.LightRadius
				e.dirtyLightEmitter = true
			}
			e.dirtyPosition = true
		} else {
			s.entities[pe.ID] = &Entity{
				ID:               pe.ID,
				Position:         &pb.Position{X: pe.X, Y: pe.Y, MapId: mapName},
				EntityType:       pe.EntityType,
				OwnerExtension:   pe.OwnerExtension,
				TriggerRadius:    pe.TriggerRadius,
				Gid:              pe.Gid,
				GidOff:           pe.Gid,
				GidOn:            pe.GidOn,
				OnInteractAction: pe.OnInteractAction,
				Actions:          pe.Actions,
				Interactions:     pe.Interactions,
				LightIntensity:   pe.LightIntensity,
				LightColor:       pe.LightColor,
				LightRadius:      pe.LightRadius,
				spawnedTo:        make(map[string]bool),
				currentZones:     make(map[string]bool),
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
				MapWarnings:   result.mapWarnings,
				MapError:      result.mapError,
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

	// client.<id>.heartbeat — pusher confirms the WebSocket is alive on each
	// successful WS ping. Updates the entity's lastHeartbeat so the client
	// reaper can detect and despawn orphaned entities (pusher crash/restart,
	// lost client.disconnected) instead of leaving ghost avatars forever.
	if _, err := s.nc.Subscribe("client.*.heartbeat", func(m *nats.Msg) {
		clientID := subjectClientID(m.Subject, "heartbeat")
		s.mu.Lock()
		if e, ok := s.clients[clientID]; ok {
			e.lastHeartbeat = time.Now()
		}
		s.mu.Unlock()
	}); err != nil {
		return fmt.Errorf("subscribe client.heartbeat: %w", err)
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

	// client.<id>.set_status — presence status change request (Available /
	// Busy / Do Not Disturb). Session-only; broadcast on
	// worldsim.player_status so ext-av can enforce DND A/V exclusion.
	if _, err := s.nc.Subscribe("client.*.set_status", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.handle_set_status_sub")
		defer span.End()
		clientID := subjectClientID(m.Subject, "set_status")
		span.SetAttributes(attribute.String("client.id", clientID))
		var frame pb.SetStatusFrame
		if err := proto.Unmarshal(m.Data, &frame); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		s.handleSetStatus(ctx, clientID, &frame)
	}); err != nil {
		return fmt.Errorf("subscribe client.set_status: %w", err)
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
		s.portalOrInit().transition(ctx, &s.World, req.EntityID, req.MapID, req.TargetEntity)
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

	// Entity info request-reply handler: extensions query per-entity fields
	// (is_admin, status, display_name, map_id) to authorize actions (e.g.
	// ext-rec checks is_admin before starting a recording) without reading
	// PocketBase directly.
	if err := s.subscribeEntityInfo(); err != nil {
		return fmt.Errorf("subscribe worldsim.entity_info: %w", err)
	}

	// Recording store request-reply handlers: ext-rec creates/updates rows in
	// the recordings PocketBase collection via these subjects (extensions don't
	// have direct PocketBase access).
	if err := s.subscribeRecordingStore(); err != nil {
		return fmt.Errorf("subscribe worldsim.recording: %w", err)
	}

	// World options request-reply handlers: the admin portal reads/writes the
	// server-wide runtime config (SMTP, APP_URL, YouTube RTMP, ffmpeg limits)
	// via these subjects. worldsim owns the NATS KV bucket "world_options".
	// Skipped in tests that build a minimal Simulator without the manager.
	if s.worldOpts != nil {
		if err := s.subscribeWorldOptions(); err != nil {
			return fmt.Errorf("subscribe worldsim.world_options: %w", err)
		}

		// world_options.update: hot-reload SMTP/APP_URL in PocketBase when the
		// admin edits world_options. worldsim is the writer of the bucket, but
		// it also subscribes to its own broadcast so registered callbacks
		// (main.go's applySMTPFromOptions) fire on every change.
		if _, err := s.nc.Subscribe(worldOptionsUpdateSub, func(msg *nats.Msg) {
			var opts WorldOptions
			if err := json.Unmarshal(msg.Data, &opts); err != nil {
				s.logger.Warn("world_options.update unmarshal", "err", err)
				return
			}
			for _, fn := range s.worldOptsOnUpdate {
				fn(opts)
			}
		}); err != nil {
			return fmt.Errorf("subscribe world_options.update: %w", err)
		}
	}

	// Stats request-reply handler: the audit service queries this for the
	// /world status page (per-map overview, players, extensions, zones).
	if err := s.subscribeStats(); err != nil {
		return fmt.Errorf("subscribe worldsim.stats.get: %w", err)
	}

	// Entity query handlers (worldsim.entities.query / worldsim.entity.get):
	// read-only snapshots of the entity table, used by the MCP server to
	// expose entity reads to MCP clients. See entities_query.go.
	if err := s.subscribeEntitiesQuery(); err != nil {
		return fmt.Errorf("subscribe worldsim.entities.query: %w", err)
	}

	// Admin / control handlers (worldsim.client.kick, worldsim.client.ban,
	// worldsim.admin.chat / set_name / set_status / set_sprite /
	// set_player_options). Used by the MCP server to expose control actions
	// to MCP clients. See admin_actions.go.
	if err := s.subscribeClientKick(); err != nil {
		return fmt.Errorf("subscribe worldsim.client.kick: %w", err)
	}
	if err := s.subscribeClientBan(); err != nil {
		return fmt.Errorf("subscribe worldsim.client.ban: %w", err)
	}
	if err := s.subscribeAdminChat(); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.chat: %w", err)
	}
	if err := s.subscribeAdminSetName(); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_name: %w", err)
	}
	if err := s.subscribeAdminSetStatus(); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_status: %w", err)
	}
	if err := s.subscribeAdminSetSprite(); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_sprite: %w", err)
	}
	if err := s.subscribeAdminSetPlayerOptions(); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_player_options: %w", err)
	}

	// Announce readiness so extensions can register against a live subscriber
	// instead of racing their initial publish (NATS Core drops publishes with
	// no subscribers). Flush guarantees the broadcast is on the wire before
	// the tick loop starts. Extensions also listen for this to re-register on
	// worldsim restarts.
	if err := s.nc.Publish("worldsim.ready", []byte(s.defaultMap)); err != nil {
		s.logger.Warn("publish worldsim.ready", "err", err)
	}
	// Re-broadcast current world_options so late subscribers (ext-rec, etc.)
	// catch up after a worldsim restart without having to read KV. Guarded
	// for tests that build a minimal Simulator without the manager.
	if s.worldOpts != nil {
		s.worldOpts.PublishUpdate()
	}
	if err := s.nc.Flush(); err != nil {
		return fmt.Errorf("flush worldsim.ready: %w", err)
	}
	s.logger.Info("worldsim ready", "default_map", s.defaultMap, "maps", len(s.maps))

	return nil
}

// WorldOptions returns the current server-wide runtime options. Used by
// main.go to apply SMTP/APP_URL to PocketBase at startup.
func (s *Simulator) WorldOptions() WorldOptions {
	return s.worldOpts.Get()
}

// OnWorldOptionsUpdate registers a callback fired whenever world_options
// changes (via the world_options.update NATS broadcast). Used by main.go to
// hot-reload SMTP/APP_URL in PocketBase without restarting worldsim. Must be
// called before Run(). The callback receives the new options snapshot.
func (s *Simulator) OnWorldOptionsUpdate(fn func(WorldOptions)) {
	s.worldOptsOnUpdate = append(s.worldOptsOnUpdate, fn)
}

func (s *Simulator) Run(ctx context.Context) error {
	// Start extension stale checker.
	go s.extMgr.StartStaleChecker(ctx)

	// Start client reaper: despawn player avatars whose pusher heartbeat has
	// gone silent (pusher crash/restart, lost client.disconnected). Without
	// this, orphaned entities linger forever and inflate the player count.
	go s.startClientReaper(ctx)

	// Run map integrity check at startup.
	s.runIntegrityCheck()

	// Periodic integrity check (every 5 minutes).
	go s.startPeriodicIntegrityCheck(ctx)

	// Periodic map reload check (every 30 seconds).
	go s.startMapReloadChecker(ctx)

	// Publish health to the "healthz" NATS subject every 10 seconds so the
	// pusher can aggregate and serve it via its HTTP /healthz endpoint.
	go s.startHealthPublisher(ctx)

	// Periodic position persistence (every 30 seconds). Saves each connected
	// player's position and map_id to PocketBase so a worldsim/pusher crash
	// doesn't lose more than 30s of movement (despawnClient only fires on a
	// clean disconnect). Skips entities whose position hasn't changed since
	// the last save to avoid idle write load.
	go s.startPositionPersister(ctx)

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

// positionPersistInterval is how often startPositionPersister checkpoints
// connected players' positions to PocketBase. 30s bounds the movement loss
// from a worldsim/pusher crash; despawnClient still saves on clean disconnect.
const positionPersistInterval = 30 * time.Second

// startPositionPersister periodically saves every connected player's position
// and map_id to PocketBase. Without this, a worldsim or pusher crash (where
// client.disconnected never fires) loses the player's position back to their
// last spawn/restore point. Skips entities whose position hasn't changed since
// the last save to keep idle write load at zero.
func (s *Simulator) startPositionPersister(ctx context.Context) {
	ticker := time.NewTicker(positionPersistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.persistChangedPositions(ctx)
		}
	}
}

// positionSave is a pending position+map write collected by the persister.
type positionSave struct {
	entityID string
	x, y     float32
	mapID    string
}

// collectChangedPositionsLocked returns the set of connected players whose
// position or map differs from the last value persisted in lastSavedPos.
// Caller must hold s.mu.
func (s *Simulator) collectChangedPositionsLocked() []positionSave {
	var toSave []positionSave
	for _, e := range s.clients {
		if e.Position == nil {
			continue
		}
		last, ok := s.lastSavedPos[e.ID]
		if ok && last.x == e.Position.X && last.y == e.Position.Y && last.mapID == e.Position.MapId {
			continue
		}
		toSave = append(toSave, positionSave{entityID: e.ID, x: e.Position.X, y: e.Position.Y, mapID: e.Position.MapId})
	}
	return toSave
}

// persistChangedPositions snapshots connected players under s.mu, then for
// each whose position or map differs from the last saved value, saves to
// PocketBase outside the lock (network I/O) and updates lastSavedPos.
func (s *Simulator) persistChangedPositions(ctx context.Context) {
	s.mu.Lock()
	toSave := s.collectChangedPositionsLocked()
	s.mu.Unlock()

	if len(toSave) == 0 || s.userStore == nil {
		return
	}

	for _, p := range toSave {
		if err := s.userStore.SavePosition(p.entityID, p.x, p.y); err != nil {
			s.logger.WarnContext(ctx, "periodic position save failed", "err", err, "entity", p.entityID)
			continue
		}
		if err := s.userStore.SaveMapID(p.entityID, p.mapID); err != nil {
			s.logger.WarnContext(ctx, "periodic map_id save failed", "err", err, "entity", p.entityID)
			continue
		}
		s.mu.Lock()
		s.lastSavedPos[p.entityID] = savedPos{x: p.x, y: p.y, mapID: p.mapID}
		s.mu.Unlock()
	}
}

// clientHeartbeatTimeout is how long without a pusher heartbeat a player avatar
// may live before the reaper despawns it. The pusher publishes a heartbeat on
// every successful WS ping (every PingInterval, 30s), so 90s tolerates 3 lost
// pings before reaping a legitimately-connected player.
const clientHeartbeatTimeout = 90 * time.Second

// startClientReaper periodically despawns player avatars whose pusher
// heartbeat has gone silent. This catches entities orphaned when the pusher
// crashes/restarts or a client.disconnected message is lost — without it they
// linger in s.clients forever, inflating the player count and leaving ghost
// avatars on other players' screens.
func (s *Simulator) startClientReaper(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapStaleClients()
		}
	}
}

// reapStaleClients collects and despawns client entities whose lastHeartbeat is
// older than clientHeartbeatTimeout. Stale IDs are gathered under the lock;
// despawnClient is called per-ID outside the lock (it does PB I/O and takes the
// lock itself).
func (s *Simulator) reapStaleClients() {
	s.mu.Lock()
	now := time.Now()
	var stale []string
	for clientID, e := range s.clients {
		if now.Sub(e.lastHeartbeat) > clientHeartbeatTimeout {
			stale = append(stale, clientID)
		}
	}
	s.mu.Unlock()

	for _, clientID := range stale {
		s.logger.Warn("reaping stale client", "client", clientID)
		audit.Emit(s.nc, "client.reaped", audit.SeverityWarn,
			audit.Actor{ClientID: clientID},
			audit.Details{"timeout": clientHeartbeatTimeout.String()},
			"")
		s.despawnClient(context.Background(), clientID)
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
	mapWarnings   []*pb.MapWarning
	mapError      string
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
			mapWarnings:   s.mapWarnings[existing.Position.MapId],
			mapError:      s.mapErrors[existing.Position.MapId],
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
	entityStatus := uint32(statusAvailable)

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
			entityStatus = user.Status
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
		DisplayName:    displayName,
		IsGuest:        sub == "" || sub == "dev",
		IP:             ip,
		DeviceID:       deviceID,
		IsAdmin:        isAdmin,
		HideAdminBadge: hideAdminBadge,
		SpriteBase:     spriteBase,
		PlayerOptions:  playerOptions,
		Status:         entityStatus,
		spawnedTo:      make(map[string]bool),
		currentZones:   make(map[string]bool),
		lastHeartbeat:  time.Now(),
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
		mapWarnings:   s.mapWarnings[mapName],
		mapError:      s.mapErrors[mapName],
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
	delete(s.lastSavedPos, e.ID)
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

// portalTransitionReq is a deferred portal transition queued during the tick's
// zone-enter scan and applied after the tick releases s.mu.
type portalTransitionReq struct {
	entityID     string
	targetMap    string
	targetEntity string
}

// transitionEntity is a thin wrapper around PortalSystem.transition for
// backward compatibility. Delegates to the portal system.
func (s *Simulator) transitionEntity(ctx context.Context, entityID, targetMap, targetEntity string) {
	s.portalOrInit().transition(ctx, &s.World, entityID, targetMap, targetEntity)
}

// portalOrInit returns the portal system, constructing a default one if it
// wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) portalOrInit() *PortalSystem {
	if s.portal == nil {
		if s.portalSink == nil {
			s.portalSink = NewNatPortalSink(s.nc, s.logger, s.userStore)
		}
		s.portal = NewPortalSystem(s.portalSink, s.logger, &s.mu)
	}
	return s.portal
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
	EntityID         string              `json:"entity_id"`
	EntityType       string              `json:"entity_type,omitempty"`
	OwnerExtension   string              `json:"owner_extension,omitempty"`
	State            string              `json:"state,omitempty"`
	Gid              uint32              `json:"gid,omitempty"`
	GidOff           uint32              `json:"gid_off,omitempty"`
	GidOn            uint32              `json:"gid_on,omitempty"`
	OnInteractAction string              `json:"on_interact_action,omitempty"`
	Actions          string              `json:"actions,omitempty"`
	Interactions     map[string][]Effect `json:"interactions,omitempty"`
	LightIntensity   uint32              `json:"light_intensity,omitempty"`
	LightColor       uint32              `json:"light_color,omitempty"`
	LightRadius      float32             `json:"light_radius,omitempty"`
}

// actionDispatchMsg is published to extension.<id>.action for every
// extension registered for the triggered input type.
type actionDispatchMsg struct {
	EntityID         string               `json:"entity_id"` // the acting player
	Input            string               `json:"input"`
	AdjacentEntities []adjacentEntityInfo `json:"adjacent_entities"`
	// TargetEntities are far-away entities referenced by adjacent entities'
	// Interactions target_ids. Worldsim looks them up in s.entities so
	// extensions can read their current state without a separate query.
	TargetEntities []adjacentEntityInfo `json:"target_entities,omitempty"`
	// TargetEntityID and ActionID are set for "action:execute" dispatches
	// (the second phase of the popup flow). Empty for "key:E".
	TargetEntityID string `json:"target_entity_id,omitempty"`
	ActionID       string `json:"action_id,omitempty"`
}

// availableAction is a single action the client should display in the
// interaction popup. The extension builds this list from the entity's
// Actions property and current state.
type availableAction struct {
	EntityID    string `json:"entity_id"`
	ActionID    string `json:"action_id"`
	Label       string `json:"label"`
	EntityLabel string `json:"entity_label,omitempty"`
}

// actionReplyMsg is the extension's reply to an actionDispatchMsg.
type actionReplyMsg struct {
	Handled bool `json:"handled"`
	Updates []struct {
		EntityID string `json:"entity_id"`
		State    string `json:"state"`
	} `json:"updates,omitempty"`
	AppearanceUpdates []struct {
		EntityID string `json:"entity_id"`
		Gid      uint32 `json:"gid"`
	} `json:"appearance_updates,omitempty"`
	LightUpdates []struct {
		EntityID  string  `json:"entity_id"`
		Intensity uint32  `json:"intensity"`
		Color     uint32  `json:"color,omitempty"`
		Radius    float32 `json:"radius,omitempty"`
	} `json:"light_updates,omitempty"`
	Animations []struct {
		EntityID    string `json:"entity_id"`
		AnimationID uint32 `json:"animation_id"`
	} `json:"animations,omitempty"`
	AvailableActions []availableAction `json:"available_actions,omitempty"`
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

	// Collect target entities referenced by adjacent entities' Interactions
	// target_ids. These may be far away (e.g. ceiling lights controlled by a
	// wall switch). Worldsim looks them up in s.entities so extensions can
	// read their current state in a single dispatch.
	targetSet := make(map[string]struct{})
	for _, a := range adjacent {
		for _, effects := range a.Interactions {
			for _, fx := range effects {
				for _, tid := range fx.TargetIDs {
					if tid == "" {
						continue
					}
					targetSet[tid] = struct{}{}
				}
			}
		}
	}
	var targets []adjacentEntityInfo
	for tid := range targetSet {
		if te, ok := s.entities[tid]; ok && te.Position != nil {
			targets = append(targets, adjacentEntityInfo{
				EntityID:       te.ID,
				EntityType:     te.EntityType,
				OwnerExtension: te.OwnerExtension,
				State:          te.State,
				Gid:            te.Gid,
				GidOff:         te.GidOff,
				GidOn:          te.GidOn,
				LightIntensity: te.LightIntensity,
				LightColor:     te.LightColor,
				LightRadius:    te.LightRadius,
			})
		}
	}
	s.mu.Unlock()

	extIDs := s.extMgr.ExtensionsForInput(action.GetInput())
	if len(extIDs) == 0 {
		s.sendActionResult(ctx, clientID, action.GetSeq(), false, "no_handler", nil)
		return
	}

	payload, err := json.Marshal(actionDispatchMsg{
		EntityID:         e.ID,
		Input:            action.GetInput(),
		AdjacentEntities: adjacent,
		TargetEntities:   targets,
		TargetEntityID:   action.GetEntityId(),
		ActionID:         action.GetActionId(),
	})
	if err != nil {
		s.logger.WarnContext(ctx, "action dispatch marshal", "err", err)
		return
	}

	handled := false
	var allAvailableActions []availableAction
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
			allAvailableActions = append(allAvailableActions, resp.AvailableActions...)
		}
	}

	reason := ""
	if !handled {
		reason = "timeout"
	}
	s.sendActionResult(ctx, clientID, action.GetSeq(), handled, reason, allAvailableActions)
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
			EntityID:         e.ID,
			EntityType:       e.EntityType,
			OwnerExtension:   e.OwnerExtension,
			State:            e.State,
			Gid:              e.Gid,
			GidOff:           e.GidOff,
			GidOn:            e.GidOn,
			OnInteractAction: e.OnInteractAction,
			Actions:          e.Actions,
			Interactions:     e.Interactions,
			LightIntensity:   e.LightIntensity,
			LightColor:       e.LightColor,
			LightRadius:      e.LightRadius,
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
	for _, u := range resp.AppearanceUpdates {
		if e, ok := s.entities[u.EntityID]; ok && e.Gid != u.Gid {
			e.Gid = u.Gid
			e.dirtyAppearance = true
		}
	}
	for _, u := range resp.LightUpdates {
		if e, ok := s.entities[u.EntityID]; ok {
			// color/radius 0 = "unchanged" when intensity is non-zero, so
			// the extension can set just the intensity without clobbering
			// the existing color/radius.
			if u.Intensity != 0 && u.Color == 0 {
				u.Color = e.LightColor
			}
			if u.Intensity != 0 && u.Radius == 0 {
				u.Radius = e.LightRadius
			}
			if e.LightIntensity != u.Intensity || e.LightColor != u.Color || e.LightRadius != u.Radius {
				e.LightIntensity = u.Intensity
				e.LightColor = u.Color
				e.LightRadius = u.Radius
				e.dirtyLightEmitter = true
			}
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
func (s *Simulator) sendActionResult(ctx context.Context, clientID string, seq uint32, ok bool, reason string, availableActions []availableAction) {
	var pbActions []*pb.AvailableAction
	for _, a := range availableActions {
		pbActions = append(pbActions, &pb.AvailableAction{
			EntityId:    a.EntityID,
			ActionId:    a.ActionID,
			Label:       a.Label,
			EntityLabel: a.EntityLabel,
		})
	}
	frame := &pb.ServerFrame{Payload: &pb.ServerFrame_ActionResult{
		ActionResult: &pb.ActionResultFrame{Seq: seq, Ok: ok, Reason: reason, AvailableActions: pbActions},
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

// handleSetStatus processes a client-sent SetStatusFrame: validates the enum
// range (0-2), updates Entity.Status, marks dirtyName so the DisplayName
// component (which carries status) is re-replicated, persists the new status to
// PocketBase (players.status) so it survives page reloads, and broadcasts the
// change on worldsim.player_status so ext-av can enforce DND A/V exclusion
// (skip zone token minting, proactively eject from active zone rooms). See
// documentation/plans/2026-07-15-player-status-design.md.
func (s *Simulator) handleSetStatus(ctx context.Context, clientID string, frame *pb.SetStatusFrame) {
	ctx, span := s.tracer.Start(ctx, "worldsim.handle_set_status")
	defer span.End()
	span.SetAttributes(attribute.String("client.id", clientID), attribute.Int("status", int(frame.GetStatus())))

	status := frame.GetStatus()
	if status > statusDoNotDisturb {
		span.SetStatus(codes.Error, "invalid status")
		s.logger.WarnContext(ctx, "invalid status", "status", status, "client", clientID)
		return
	}

	s.mu.Lock()
	entity, ok := s.clients[clientID]
	if !ok {
		s.mu.Unlock()
		span.SetStatus(codes.Error, "unknown client")
		return
	}
	if entity.Status == status {
		s.mu.Unlock()
		return
	}
	entity.Status = status
	entity.dirtyName = true // status rides on the DisplayName component
	entityID := entity.ID
	s.mu.Unlock()

	// Persist to PocketBase so the status survives page reloads. No-op for
	// guests (no players record). Errors are logged but don't fail the
	// request — the in-memory status is already live and replicated.
	if s.userStore != nil {
		if err := s.userStore.UpdateStatus(entityID, status); err != nil {
			s.logger.WarnContext(ctx, "persist status", "err", err, "entity", entityID)
			span.RecordError(err)
		}
	}

	audit.Emit(s.nc, "player.set_status", audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID},
		audit.Details{"status": status},
		"")

	// Broadcast the status change so ext-av can enforce DND A/V exclusion
	// (skip zone token minting for DND, proactively eject from active zone
	// rooms). Proximity exclusion is handled in runProximityClustering by
	// skipping DND players, which naturally emits proximity.leave on the
	// next tick.
	payload, _ := json.Marshal(struct {
		EntityID string `json:"entity_id"`
		Status   uint32 `json:"status"`
	}{EntityID: entityID, Status: status})
	if err := s.nc.Publish("worldsim.player_status", payload); err != nil {
		s.logger.WarnContext(ctx, "publish player_status", "err", err, "entity", entityID)
		span.RecordError(err)
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

	s.Tick.SnapshotSeq++

	s.movementOrInit().Step(ctx, &s.World)

	// --- Zone enter/exit detection ---
	s.zoneOrInit().Step(ctx, &s.World)

	// --- Proximity clustering (throttled to ~4Hz) ---
	if s.Tick.TickCount%5 == 0 {
		s.proximityOrInit().Step(ctx, &s.World)
	}

	// --- Replication ---
	// Lite MVP: replicate everything to everyone (no AOI filter).
	replicated := s.replicationOrInit().Step(ctx, &s.World)

	// Metric-as-log-attrs: tick duration, entity count, replication batches.
	// motel has no /v1/metrics endpoint, so we emit these as span attributes +
	// a structured log so they're queryable via log search.
	durMs := time.Since(start).Milliseconds()
	span.SetAttributes(
		attribute.Int("tick.duration_ms", int(durMs)),
		attribute.Int("tick.entity_count", len(s.entities)),
		attribute.Int("tick.replicated_clients", replicated),
		attribute.Int("tick.snapshot_seq", int(s.Tick.SnapshotSeq)),
	)
	// Log tick summary every 5 seconds (every 300th tick at 60Hz) to avoid
	// flooding the logs. Span attributes are always set for tracing.
	s.Tick.TickCount++
	if s.Tick.TickCount%300 == 0 {
		s.logger.InfoContext(ctx, "tick",
			"duration_ms", durMs,
			"entity_count", len(s.entities),
			"replicated_clients", replicated,
			"snapshot_seq", s.Tick.SnapshotSeq,
		)
	}

	// Apply portal transitions after releasing s.mu. transitionEntity
	// re-locks s.mu, which would self-deadlock if called inline above
	// (sync.Mutex is not reentrant). No recover wraps tick(), so a panic
	// here crashes the process (Docker restarts it) — explicit unlock is
	// safe because the lock is never observed held after the process dies.
	s.mu.Unlock()
	s.portalOrInit().Step(ctx, &s.World)
}

// replicateToClient is a thin wrapper around ReplicationSystem.replicateToClient
// for backward compatibility with tests that call s.replicateToClient() directly.
func (s *Simulator) replicateToClient(ctx context.Context, clientEntity *Entity) bool {
	return s.replicationOrInit().replicateToClient(ctx, &s.World, clientEntity)
}

// replicationOrInit returns the replication system, constructing a default one
// if it wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) replicationOrInit() *ReplicationSystem {
	if s.replication == nil {
		if s.replicationSink == nil {
			s.replicationSink = NewNatReplicationSink(s.nc, s.logger, s.tracer)
		}
		s.replication = NewReplicationSystem(s.replicationSink, s.tracer)
	}
	return s.replication
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
		s.mu.Lock()
		s.mapErrors[mapName] = fmt.Sprintf("map reload failed: %v", err)
		s.mapWarnings[mapName] = nil
		s.mu.Unlock()
		return
	}

	// Validate the reloaded map before storing.
	issues := CheckMapIntegrity(newMapData)
	var fatalMsgs []string
	var warningMsgs []*pb.MapWarning
	for _, r := range issues {
		if r.Level == LevelError {
			fatalMsgs = append(fatalMsgs, r.String())
		} else if r.Level == LevelWarning {
			warningMsgs = append(warningMsgs, &pb.MapWarning{
				EntityId: r.EntityID,
				Message:  r.Message,
			})
		}
	}
	LogIntegrityResults(s.logger, issues, mapName)

	if len(fatalMsgs) > 0 {
		s.logger.Error("map reload rejected due to validation errors", "map", mapName, "errors", len(fatalMsgs))
		s.mu.Lock()
		s.mapErrors[mapName] = fmt.Sprintf("map validation failed: %s", strings.Join(fatalMsgs, "; "))
		s.mapWarnings[mapName] = nil
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	s.maps[mapName] = newMapData
	s.zones[mapName] = NewZoneRegistry(newMapData.Zones, newMapData.Width, newMapData.Height)
	s.mapFilenames[mapName] = info.TiledJSONFilename
	s.mapErrors[mapName] = ""
	s.mapWarnings[mapName] = warningMsgs
	s.reloadBaseEntities(mapName, newMapData.Entities)
	// Re-add mobile proximity zones for connected players on this map — the
	// registry was rebuilt from scratch above, wiping them.
	for _, e := range s.clients {
		if e.mobileZone != nil && e.Position != nil && e.Position.MapId == mapName {
			s.zones[mapName].AddZone(e.mobileZone)
		}
	}
	s.mu.Unlock()

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

// proximityExitRadius is the hysteresis exit radius (in tiles). A player must
// move this far from another player's feet before the proximity zone exit
// fires. Larger than proximityRadius so players at the boundary don't
// oscillate in/out every tick. See issue #88.
const proximityExitRadius = 3.0

// proximityStationaryThreshold is the number of consecutive stationary ticks
// required before proximity A/V activates. At 20Hz, 10 ticks = 500ms. This
// gates proximity.join so a player walking past another without stopping
// does not trigger A/V creation/destruction. See issue #88.
const proximityStationaryThreshold = 10

// playerCollisionRadius is the half-width of the player's collision box in
// tiles, centered on the feet. Zone shapes are expanded by this radius
// (Minkowski sum) before the swept segment test, so the feet center stops
// `radius` tiles before the wall edge instead of at it. A small radius
// (0.1) keeps the visible gap against walls tight while still letting the
// player squeeze through 1-tile gaps without snagging on corners.
const playerCollisionRadius float32 = 0.1

// runMovementSystem is a thin wrapper around MovementSystem.Step for
// backward compatibility with tests that call s.runMovementSystem() directly.
// Caller must hold s.mu.
func (s *Simulator) runMovementSystem() {
	s.movementOrInit().Step(context.Background(), &s.World)
}

// isMoveBlocked is a thin wrapper around MovementSystem.isMoveBlocked for
// backward compatibility with tests that call s.isMoveBlocked() directly.
func (s *Simulator) isMoveBlocked(zr *ZoneRegistry, md *MapData, oldX, oldY, newX, newY float32) bool {
	return s.movementOrInit().isMoveBlocked(zr, md, oldX, oldY, newX, newY)
}

// movementOrInit returns the movement system, constructing a default one from
// extMgr if it wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) movementOrInit() *MovementSystem {
	if s.movement == nil {
		s.movement = NewMovementSystem(s.extMgr)
	}
	return s.movement
}

// publishZoneEvent publishes a zone.enter or zone.exit event via the zone
// sink. Thin wrapper for backward compatibility (despawnClient calls this).
func (s *Simulator) publishZoneEvent(ctx context.Context, event, entityID, clientID, zoneID, mapID string) {
	s.zoneSinkOrInit().PublishZoneEvent(ctx, event, entityID, clientID, zoneID, mapID)
}

// zoneSinkOrInit returns the zone sink, constructing a default one if it
// wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) zoneSinkOrInit() ZoneSink {
	if s.zoneSink == nil {
		s.zoneSink = NewNatZoneSink(s.nc, s.logger)
	}
	return s.zoneSink
}

// zoneOrInit returns the zone system, constructing a default one if it
// wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) zoneOrInit() *ZoneSystem {
	if s.zone == nil {
		s.zone = NewZoneSystem(s.zoneSinkOrInit(), s.logger)
	}
	return s.zone
}

// proximityEventPayload is the NATS payload for proximity.join/leave events.
// publishProximityEvent is a thin wrapper around the proximity sink for
// backward compatibility (despawnClient calls this).
func (s *Simulator) publishProximityEvent(ctx context.Context, event, entityID, clientID, groupID, mapID string, members []string) {
	s.proximitySinkOrInit().PublishProximityEvent(ctx, event, entityID, clientID, groupID, mapID, members)
}

// proximitySinkOrInit returns the proximity sink, constructing a default one
// if it wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) proximitySinkOrInit() ProximitySink {
	if s.proximitySink == nil {
		s.proximitySink = NewNatProximitySink(s.nc, s.logger)
	}
	return s.proximitySink
}

// proximityOrInit returns the proximity system, constructing a default one
// if it wasn't set (e.g. in tests that build &Simulator{} directly).
func (s *Simulator) proximityOrInit() *ProximitySystem {
	if s.proximity == nil {
		s.proximity = NewProximitySystem(s.proximitySinkOrInit(), s.logger)
	}
	return s.proximity
}

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

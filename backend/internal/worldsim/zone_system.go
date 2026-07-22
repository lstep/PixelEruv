package worldsim

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/lstep/pixeleruv/backend/internal/audit"
)

// ZoneInput is the narrow read-view of World that ZoneSystem needs.
// PendingPortalTransitions is a pointer so ZoneSystem can append portal
// transition requests that PortalSystem drains after the tick releases the mutex.
type ZoneInput struct {
	Entities                 map[string]*Entity
	Zones                    map[string]*ZoneRegistry
	Maps                     map[string]*MapData
	PendingPortalTransitions *[]portalTransitionReq
}

// ZoneSystem detects zone enter/exit transitions for all entities each tick
// and publishes zone.enter/zone.exit events via ZoneSink. When a portal zone
// is entered, it enqueues a portal transition on World.pendingPortalTransitions
// for PortalSystem to apply after the tick releases the mutex.
//
// Field ownership (writes):
//   - Entity.currentZones
//   - World.pendingPortalTransitions (append only — drained by PortalSystem)
type ZoneSystem struct {
	sink   ZoneSink
	logger *slog.Logger
}

// NewZoneSystem constructs a ZoneSystem. sink receives zone.enter/zone.exit
// events; logger is used for portal target map warnings.
func NewZoneSystem(sink ZoneSink, logger *slog.Logger) *ZoneSystem {
	return &ZoneSystem{sink: sink, logger: logger}
}

// Step runs zone enter/exit detection for all entities. Caller must hold s.mu.
func (z *ZoneSystem) Step(ctx context.Context, in ZoneInput) {
	for _, e := range in.Entities {
		if e.currentZones == nil {
			e.currentZones = make(map[string]bool)
		}
		// Look up the zone registry for this entity's current map.
		zr := in.Zones[e.Position.MapId]
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
				z.sink.PublishZoneEvent(ctx, "zone.enter", e.ID, clientID, zid, e.Position.MapId, e.DisplayName)
				// Check for portal zones — handle map transition.
				z.handlePortalZone(ctx, in, e, zid)
			}
		}
		for zid := range e.currentZones {
			if !newSet[zid] {
				// Hysteresis for proximity zones: delay exit until the
				// player is beyond proximityExitRadius from the zone
				// owner's feet. Without this, players standing near the
				// 2-tile boundary oscillate in/out every tick, causing
				// A/V thrashing. See issue #88.
				if strings.HasPrefix(zid, "prox-") {
					ownerID := zid[len("prox-"):]
					if owner, ok := in.Entities[ownerID]; ok && owner.Position != nil {
						feetX := e.Position.X
						feetY := e.Position.Y + avatarFeetYOffset
						ownerFeetX := owner.Position.X
						ownerFeetY := owner.Position.Y + avatarFeetYOffset
						dx := feetX - ownerFeetX
						dy := feetY - ownerFeetY
						if dx*dx+dy*dy <= proximityExitRadius*proximityExitRadius {
							newSet[zid] = true
							continue
						}
					}
				}
				z.sink.PublishZoneEvent(ctx, "zone.exit", e.ID, clientID, zid, e.Position.MapId, e.DisplayName)
			}
		}
		e.currentZones = newSet
	}
}

// handlePortalZone checks if the entered zone is a portal and queues a map
// transition if so. Portal zones are defined in Tiled with zone_type="portal",
// target_map, and target_entity properties. The actual transition is applied
// after the tick releases s.mu (see tick); calling transitionEntity inline
// would self-deadlock on the non-reentrant s.mu. Caller must hold s.mu.
func (z *ZoneSystem) handlePortalZone(ctx context.Context, in ZoneInput, e *Entity, zoneID string) {
	if e.NetworkSession == nil {
		return // only player avatars can transition
	}
	zr := in.Zones[e.Position.MapId]
	if zr == nil {
		return
	}
	// Find the zone in the registry to check its properties.
	for _, zone := range zr.zones {
		if zone.ID != zoneID {
			continue
		}
		if zone.PortalTargetMap == "" {
			return // not a portal zone
		}
		// Validate the target map exists.
		if _, ok := in.Maps[zone.PortalTargetMap]; !ok {
			z.logger.WarnContext(ctx, "portal target map not found",
				"entity", e.ID, "zone", zoneID, "target_map", zone.PortalTargetMap)
			return
		}
		*in.PendingPortalTransitions = append(*in.PendingPortalTransitions, portalTransitionReq{
			entityID:     e.ID,
			targetMap:    zone.PortalTargetMap,
			targetEntity: zone.PortalTargetEntity,
		})
		return
	}
}

// natZoneSink is the production ZoneSink, publishing zone events to NATS
// with audit emission. It also satisfies the despawn-time zone event needs.
type natZoneSink struct {
	nc     *nats.Conn
	logger *slog.Logger
}

// NewNatZoneSink constructs a production ZoneSink backed by NATS.
func NewNatZoneSink(nc *nats.Conn, logger *slog.Logger) ZoneSink {
	return &natZoneSink{nc: nc, logger: logger}
}

func (s *natZoneSink) PublishZoneEvent(ctx context.Context, event, entityID, clientID, zoneID, mapID, displayName string) {
	subject := event // event already contains the full subject (e.g. "zone.enter")
	payload := struct {
		EntityID    string `json:"entity_id"`
		ClientID    string `json:"client_id"`
		ZoneID      string `json:"zone_id"`
		MapID       string `json:"map_id"`
		DisplayName string `json:"display_name"`
	}{entityID, clientID, zoneID, mapID, displayName}
	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.WarnContext(ctx, "zone event marshal", "event", event, "err", err)
		return
	}
	if err := s.nc.Publish(subject, data); err != nil {
		s.logger.WarnContext(ctx, "zone event publish", "event", event, "err", err)
	}
	s.logger.InfoContext(ctx, "zone event", "event", event, "entity", entityID, "zone", zoneID)
	audit.Emit(s.nc, event, audit.SeverityInfo,
		audit.Actor{EntityID: entityID, ClientID: clientID, DisplayName: displayName},
		audit.Details{"zone": zoneID, "map": mapID},
		"")
}

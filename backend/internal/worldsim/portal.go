package worldsim

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// PortalSystem applies deferred portal transitions queued by ZoneSystem during
// the tick's locked phase. It runs after the tick releases the world mutex
// (StepUnlocked phase), since transitionEntity re-locks the mutex and
// sync.Mutex is not reentrant.
//
// Field ownership (writes):
//   - Entity.Position (map transition)
//   - Entity.currentZones (reset)
//   - Entity.spawnedTo (reset)
//   - Entity.mobileZone.X/Y (repositioned)
//   - World.destroyedEntities (append)
//   - World.pendingPortalTransitions (drained)
type PortalSystem struct {
	sink   PortalSink
	logger *slog.Logger
	mu     *sync.Mutex
}

// NewPortalSystem constructs a PortalSystem. mu is the world mutex (shared
// with Simulator); transition re-locks it for the locked phase.
func NewPortalSystem(sink PortalSink, logger *slog.Logger, mu *sync.Mutex) *PortalSystem {
	return &PortalSystem{sink: sink, logger: logger, mu: mu}
}

// Step drains pending portal transitions and applies each one. Called after
// the tick releases s.mu — each transition re-locks s.mu for its locked phase.
func (p *PortalSystem) Step(ctx context.Context, w *World) {
	pending := w.pendingPortalTransitions
	w.pendingPortalTransitions = nil
	for _, t := range pending {
		p.transition(ctx, w, t.entityID, t.targetMap, t.targetEntity)
	}
}

// transition moves an entity to a different map. The spawn position on
// the target map is resolved as follows:
//   - If targetEntity is set, teleport to that named base entity's position
//     (a "beacon"). Fails if the entity doesn't exist on the target map.
//   - Otherwise, pick a random "spawn" zone on the target map (FindSpawnPoint).
//
// It:
//  1. Resolves the spawn position (beacon or random spawn point).
//  2. Removes the entity's mobile zone from the old map's zone registry.
//  3. Changes Position.MapId, X, Y to the target.
//  4. Re-adds the mobile zone to the new map's zone registry.
//  5. Resets spawnedTo so the entity re-spawns for clients on the new map.
//  6. Queues a DestroyEntity for clients on the old map.
//  7. Sends a MapTransitionFrame to the client (via sink).
//  8. Persists the new map_id to PocketBase (via sink).
//  9. Emits an audit event (via sink).
func (p *PortalSystem) transition(ctx context.Context, w *World, entityID, targetMap, targetEntity string) {
	p.mu.Lock()
	e, ok := w.entities[entityID]
	if !ok {
		p.mu.Unlock()
		return
	}

	targetMD := w.maps[targetMap]
	if targetMD == nil {
		p.logger.WarnContext(ctx, "transition target map not loaded",
			"entity", entityID, "target_map", targetMap)
		p.mu.Unlock()
		return
	}

	// Resolve spawn position on the target map.
	var spawnX, spawnY float32
	if targetEntity != "" {
		beacon := targetMD.FindEntityByName(targetEntity)
		if beacon == nil {
			p.logger.WarnContext(ctx, "transition target entity not found on target map",
				"entity", entityID, "target_map", targetMap, "target_entity", targetEntity)
			p.mu.Unlock()
			return
		}
		spawnX, spawnY = beacon.X, beacon.Y
	} else {
		spawnX, spawnY = targetMD.FindSpawnPoint(w.rng)
	}

	oldMap := e.Position.MapId

	// Remove mobile zone from old map's zone registry.
	if e.mobileZone != nil {
		if zr := w.zones[oldMap]; zr != nil {
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
		if zr := w.zones[targetMap]; zr != nil {
			e.mobileZone.X = spawnX - proximityRadius
			e.mobileZone.Y = spawnY + avatarFeetYOffset - proximityRadius
			zr.AddZone(e.mobileZone)
		}
	}

	// Queue a DestroyEntity for clients on the old map. Clients on the new
	// map will get a SpawnEntity via the normal replication loop.
	w.destroyedEntities = append(w.destroyedEntities, entityID)

	clientID := ""
	if e.NetworkSession != nil {
		clientID = e.NetworkSession.ClientID
	}
	mapOpts := ""
	if md := w.maps[targetMap]; md != nil {
		mapOpts = string(md.Options)
	}
	mapWarns := w.mapWarnings[targetMap]
	p.mu.Unlock()

	// Send MapTransitionFrame, persist map_id, emit audit — all via sink.
	p.sink.PublishMapTransition(ctx, clientID, targetMap, spawnX, spawnY, mapOpts, mapWarns)
	if err := p.sink.SaveMapID(entityID, targetMap); err != nil {
		p.logger.WarnContext(ctx, "failed to save user map_id", "err", err, "entity", entityID)
	}
	p.sink.EmitTransitionAudit(entityID, oldMap, targetMap, targetEntity, spawnX, spawnY)

	p.logger.InfoContext(ctx, "entity transitioned to new map",
		"entity", entityID, "old_map", oldMap, "new_map", targetMap,
		"target_entity", targetEntity, "x", spawnX, "y", spawnY)
}

// natPortalSink is the production PortalSink.
type natPortalSink struct {
	nc        *nats.Conn
	logger    *slog.Logger
	userStore *UserStore
}

// NewNatPortalSink constructs a production PortalSink backed by NATS + PocketBase.
func NewNatPortalSink(nc *nats.Conn, logger *slog.Logger, userStore *UserStore) PortalSink {
	return &natPortalSink{nc: nc, logger: logger, userStore: userStore}
}

func (s *natPortalSink) PublishMapTransition(ctx context.Context, clientID, targetMap string, x, y float32, mapOpts string, mapWarns []*pb.MapWarning) {
	if clientID == "" {
		return
	}
	frame := &pb.ServerFrame{
		Payload: &pb.ServerFrame_MapTransition{
			MapTransition: &pb.MapTransitionFrame{
				MapId:       targetMap,
				SpawnX:      x,
				SpawnY:      y,
				MapOptions:  mapOpts,
				MapWarnings: mapWarns,
			},
		},
	}
	frameBytes, _ := proto.Marshal(frame)
	subject := fmt.Sprintf("client.%s.replication", clientID)
	if err := s.nc.Publish(subject, frameBytes); err != nil {
		s.logger.WarnContext(ctx, "map transition publish", "err", err, "client", clientID)
	}
}

func (s *natPortalSink) SaveMapID(entityID, mapID string) error {
	if s.userStore == nil {
		return nil
	}
	return s.userStore.SaveMapID(entityID, mapID)
}

func (s *natPortalSink) EmitTransitionAudit(entityID, oldMap, targetMap, targetEntity string, x, y float32) {
	audit.Emit(s.nc, "player.map_transition", audit.SeverityInfo,
		audit.Actor{EntityID: entityID},
		audit.Details{"old_map": oldMap, "new_map": targetMap, "target_entity": targetEntity, "x": x, "y": y},
		"")
}

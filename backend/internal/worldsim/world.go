package worldsim

import (
	"math/rand/v2"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// World is the mutable simulation state owned by the kernel and shared across
// tick systems. Systems receive *World at Step time and read/write the fields
// they own (see field-ownership comments on Entity). World is private to the
// worldsim package — it is not part of Simulator's external interface.
//
// Field ownership by system (see tick() pipeline):
//   - MovementSystem writes: Entity.Position, Entity.dirtyPosition,
//     Entity.stationaryTicks, Entity.mobileZone.X/Y
//   - ZoneSystem writes: Entity.currentZones, PendingPortalTransitions (enqueue)
//   - ProximitySystem writes: Entity.currentProximityGroup
//   - ReplicationSystem writes: Entity.spawnedTo, Entity.dirty* (clears),
//     Entity.pendingAnimations (clears), DestroyedEntities (drains)
//   - PortalSystem writes: Entity.Position (map transition),
//     Entity.currentZones (reset), Entity.spawnedTo (reset),
//     Entity.mobileZone.X/Y (reposition), DestroyedEntities (append)
type World struct {
	entities map[string]*Entity // entity_id -> entity
	clients  map[string]*Entity // client_id -> entity (player avatar)

	// entityIDToClient maps player entity_id -> client_id, used by handleChat
	// to address proximity chat recipients. Maintained alongside clients.
	entityIDToClient map[string]string

	// destroyedEntities queues entity IDs removed since the last tick (base
	// entities removed during a map reload, or player avatars despawned on
	// disconnect), so the next replication tick can send DestroyEntity frames
	// to all connected clients. Drained after each tick's replication loop.
	destroyedEntities []string

	// pendingPortalTransitions queues portal zone transitions detected during
	// the tick's zone-enter scan. They cannot be applied inline because
	// transitionEntity re-locks s.mu, which tick already holds (sync.Mutex is
	// not reentrant — applying inline self-deadlocks the tick goroutine).
	// Drained after tick() releases s.mu.
	pendingPortalTransitions []portalTransitionReq

	maps        map[string]*MapData         // mapName -> MapData
	zones       map[string]*ZoneRegistry    // mapName -> ZoneRegistry
	mapWarnings map[string][]*pb.MapWarning // mapName -> non-fatal validation warnings
	mapErrors   map[string]string           // mapName -> fatal validation message

	rng *rand.Rand

	// Tick holds per-tick bookkeeping. The pipeline increments SnapshotSeq
	// before the locked phase and TickCount after. Systems read only.
	Tick TickMeta
}

// TickMeta carries per-tick metadata shared across systems.
type TickMeta struct {
	SnapshotSeq uint32 // incremented once per tick; stamped on every replicated component
	TickCount   uint64 // incremented once per tick; drives throttles (proximity %5, log %300)
}

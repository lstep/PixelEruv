package worldsim

import (
	"math/rand/v2"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// World is the mutable simulation state owned by the kernel and shared across
// tick systems. Systems receive a narrow input struct (e.g. MovementInput) at
// Step time — see each system's input type for the fields it reads, and the
// field-ownership comments on each system for the fields it writes.
// World is private to the worldsim package — it is not part of Simulator's
// external interface.
type World struct {
	testField int // throwaway for change-tracing verification

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

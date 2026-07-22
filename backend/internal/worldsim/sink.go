package worldsim

import (
	"context"

	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// ZoneSink is the side-effect interface for ZoneSystem. Production wires
// *natSink; tests wire a recording fake.
type ZoneSink interface {
	PublishZoneEvent(ctx context.Context, event, entityID, clientID, zoneID, mapID, displayName string)
}

// ProximitySink is the side-effect interface for ProximitySystem.
type ProximitySink interface {
	PublishProximityEvent(ctx context.Context, event, entityID, clientID, groupID, mapID string, members []string)
}

// ReplicationSink is the side-effect interface for ReplicationSystem. The
// system builds the batch and tracks spawned entities; the sink handles
// marshaling, publishing to client.<id>.replication, and the admin
// side-channel (publishAdminInfo) when isAdmin and spawned is non-empty.
// Returns true if a batch was published.
type ReplicationSink interface {
	PublishReplication(ctx context.Context, clientID string, batch *pb.ReplicationBatch, spawned []*Entity, isAdmin bool) bool
}

// PortalSink is the side-effect interface for PortalSystem. The system
// resolves the transition and mutates world state; the sink handles
// publishing the MapTransitionFrame, persisting map_id to PocketBase, and
// emitting the audit event.
type PortalSink interface {
	PublishMapTransition(ctx context.Context, clientID, targetMap string, x, y float32, mapOpts string, mapWarns []*pb.MapWarning)
	SaveMapID(entityID, mapID string) error
	EmitTransitionAudit(entityID, displayName, oldMap, targetMap, targetEntity string, x, y float32)
}

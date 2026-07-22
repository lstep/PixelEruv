package worldsim

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
	pb "github.com/lstep/pixeleruv/backend/internal/pb"
)

// ReplicationInput is the narrow read-view of World that ReplicationSystem needs.
// DestroyedEntities is a pointer so Step can drain it after replication.
type ReplicationInput struct {
	Entities          map[string]*Entity
	TickSnapshotSeq   uint32
	DestroyedEntities *[]string
}

// ReplicationSystem builds and publishes replication batches for all
// connected clients each tick. Lite MVP: replicate everything to everyone
// (no AOI filter), filtered by same-map.
//
// Field ownership (writes):
//   - Entity.spawnedTo (marks clients that received SpawnEntity)
//   - Entity.dirty* (cleared after replication)
//   - Entity.pendingAnimations (cleared after replication)
//   - World.destroyedEntities (drained after replication)
type ReplicationSystem struct {
	sink   ReplicationSink
	tracer trace.Tracer
}

// NewReplicationSystem constructs a ReplicationSystem. sink handles
// marshaling + publishing; tracer wraps the publish span.
func NewReplicationSystem(sink ReplicationSink, tracer trace.Tracer) *ReplicationSystem {
	return &ReplicationSystem{sink: sink, tracer: tracer}
}

// Step runs replication for all connected clients, then clears dirty flags
// and drains the destroyed entities queue. Returns the count of clients
// that received a batch. Caller must hold s.mu.
func (r *ReplicationSystem) Step(ctx context.Context, in ReplicationInput) int {
	replicated := 0
	for _, e := range in.Entities {
		if e.NetworkSession == nil {
			continue
		}
		if r.replicateToClient(ctx, in, e) {
			replicated++
		}
	}

	// Clear dirty flags
	for _, e := range in.Entities {
		e.dirtyPosition = false
		e.dirtyState = false
		e.dirtyName = false
		e.dirtyAppearance = false
		e.dirtyLightEmitter = false
		e.pendingAnimations = nil
	}

	// Drain the destroyed entities queue — all clients have been replicated.
	*in.DestroyedEntities = nil

	return replicated
}

// replicateToClient builds and publishes a replication batch for one client.
// Returns true if a batch was published.
func (r *ReplicationSystem) replicateToClient(ctx context.Context, in ReplicationInput, clientEntity *Entity) bool {
	rctx, span := r.tracer.Start(ctx, "worldsim.replicate")
	defer span.End()

	clientID := clientEntity.NetworkSession.ClientID
	span.SetAttributes(attribute.String("client.id", clientID))

	batch := &pb.ReplicationBatch{
		LastInputSeq: clientEntity.NetworkSession.Seq,
	}
	// Track entities spawned in this batch so we can send admin info for
	// them if the client is an admin.
	var spawnedEntities []*Entity

	for _, e := range in.Entities {
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
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			// Echo DisplayName updates (which carry presence status) back to
			// the originating client so it can sync its AvClient DND flag and
			// update its own nametag pill color. Without this, a status change
			// is replicated to OTHER clients but never to the player who
			// changed it.
			if e.dirtyName {
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName, IsGuest: e.IsGuest, IsAdmin: e.IsAdmin && !e.HideAdminBadge, Status: e.Status, Afk: e.AFK})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compDisplayName,
					Data:        nameBytes,
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			appendAnimations(batch, e)
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
				interactable := e.EntityType != "" || e.OwnerExtension != ""
				appearanceBytes, _ := proto.Marshal(&pb.Appearance{Gid: e.Gid, SpriteBase: e.SpriteBase, Interactable: interactable})
				components = append(components, &pb.ComponentData{ComponentId: compAppearance, Data: appearanceBytes})
			}
			if e.State != "" {
				stateBytes, _ := proto.Marshal(&pb.EntityState{State: e.State})
				components = append(components, &pb.ComponentData{ComponentId: compEntityState, Data: stateBytes})
			}
			if e.NetworkSession != nil && e.DisplayName != "" {
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName, IsGuest: e.IsGuest, IsAdmin: e.IsAdmin && !e.HideAdminBadge, Status: e.Status, Afk: e.AFK})
				components = append(components, &pb.ComponentData{ComponentId: compDisplayName, Data: nameBytes})
			}
			if e.LightIntensity > 0 {
				lightBytes, _ := proto.Marshal(&pb.LightEmitter{Intensity: e.LightIntensity, Color: e.LightColor, Radius: e.LightRadius})
				components = append(components, &pb.ComponentData{ComponentId: compLightEmitter, Data: lightBytes})
			}
			batch.Spawns = append(batch.Spawns, &pb.SpawnEntity{
				EntityId:    e.ID,
				SnapshotSeq: in.TickSnapshotSeq,
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
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			if e.dirtyState {
				stateBytes, _ := proto.Marshal(&pb.EntityState{State: e.State})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compEntityState,
					Data:        stateBytes,
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			if e.dirtyName {
				nameBytes, _ := proto.Marshal(&pb.DisplayName{Name: e.DisplayName, IsGuest: e.IsGuest, IsAdmin: e.IsAdmin && !e.HideAdminBadge, Status: e.Status, Afk: e.AFK})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compDisplayName,
					Data:        nameBytes,
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			if e.dirtyAppearance {
				interactable := e.EntityType != "" || e.OwnerExtension != ""
				appBytes, _ := proto.Marshal(&pb.Appearance{Gid: e.Gid, SpriteBase: e.SpriteBase, Interactable: interactable})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compAppearance,
					Data:        appBytes,
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			if e.dirtyLightEmitter {
				lightBytes, _ := proto.Marshal(&pb.LightEmitter{Intensity: e.LightIntensity, Color: e.LightColor, Radius: e.LightRadius})
				batch.Updates = append(batch.Updates, &pb.UpdateComponent{
					EntityId:    e.ID,
					ComponentId: compLightEmitter,
					Data:        lightBytes,
					SnapshotSeq: in.TickSnapshotSeq,
				})
			}
			appendAnimations(batch, e)
		}
	}

	// Send destroy notifications for entities removed since the last tick
	// (base entities removed during a map reload, or player avatars despawned
	// on disconnect). Skip entities that still exist and are on the client's
	// map — those are map transitions, not real destroys, and the client
	// gets a SpawnEntity for them in this same batch. Without this filter,
	// the client receives both Spawn and Destroy for the same entity, and
	// the Destroy wins (entity disappears).
	for _, id := range *in.DestroyedEntities {
		if e, ok := in.Entities[id]; ok && e.Position != nil && e.Position.MapId == clientEntity.Position.MapId {
			continue
		}
		batch.Destroys = append(batch.Destroys, &pb.DestroyEntity{
			EntityId:    id,
			SnapshotSeq: in.TickSnapshotSeq,
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

	return r.sink.PublishReplication(rctx, clientID, batch, spawnedEntities, clientEntity.IsAdmin)
}

// appendAnimations queues any pending PlayAnimation events for an entity
// into the replication batch.
func appendAnimations(batch *pb.ReplicationBatch, e *Entity) {
	for _, animID := range e.pendingAnimations {
		batch.Animations = append(batch.Animations, &pb.PlayAnimation{
			EntityId:    e.ID,
			AnimationId: animID,
		})
	}
}

// natReplicationSink is the production ReplicationSink. It wraps the batch
// in a ServerFrame, marshals, publishes to client.<id>.replication with
// otel context injection, and sends admin info via the admin-only channel
// when the client is an admin and spawned entities exist.
type natReplicationSink struct {
	nc     *nats.Conn
	logger *slog.Logger
	tracer trace.Tracer
}

// NewNatReplicationSink constructs a production ReplicationSink backed by NATS.
func NewNatReplicationSink(nc *nats.Conn, logger *slog.Logger, tracer trace.Tracer) ReplicationSink {
	return &natReplicationSink{nc: nc, logger: logger, tracer: tracer}
}

func (s *natReplicationSink) PublishReplication(ctx context.Context, clientID string, batch *pb.ReplicationBatch, spawned []*Entity, isAdmin bool) bool {
	// Wrap in a ServerFrame so the pusher can forward bytes unchanged to the
	// client (the pusher is a pure WS<->NATS passthrough; it must not know the
	// replication wire format).
	frame := &pb.ServerFrame{Payload: &pb.ServerFrame_Replication{Replication: batch}}
	frameBytes, err := proto.Marshal(frame)
	if err != nil {
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		span.SetStatus(codes.Error, "marshal")
		s.logger.WarnContext(ctx, "replication marshal", "client", clientID, "err", err)
		return false
	}

	subject := fmt.Sprintf("client.%s.replication", clientID)
	msg := &nats.Msg{Subject: subject, Data: frameBytes}
	otelinternal.Inject(ctx, msg) // pusher's forward span will parent here
	if err := s.nc.PublishMsg(msg); err != nil {
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		span.SetStatus(codes.Error, "publish")
		s.logger.WarnContext(ctx, "replication publish", "client", clientID, "err", err)
		return false
	}

	// If the client is an admin and we spawned new entities in this batch,
	// send admin-only info (IP, guest status, OIDC sub) for those entities
	// via the admin-only NATS channel. Non-admin clients never receive this.
	if isAdmin && len(spawned) > 0 {
		s.publishAdminInfo(ctx, clientID, spawned)
	}
	return true
}

// publishAdminInfo sends an AdminInfoFrame for the given entities to the
// admin client's admin-only NATS subject (client.<id>.admin). Only called
// for admin clients — the pusher subscribes to this subject only for admin
// sessions, so the data never reaches non-admin browsers.
func (s *natReplicationSink) publishAdminInfo(ctx context.Context, adminClientID string, entities []*Entity) {
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

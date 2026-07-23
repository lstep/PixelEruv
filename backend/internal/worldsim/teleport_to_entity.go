package worldsim

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
)

// teleportToRequest is the JSON payload published by the pusher on
// worldsim.entity.teleport_to_entity when a client sends a TeleportToFrame.
type teleportToRequest struct {
	SenderClientID  string `json:"sender_client_id"`
	TargetEntityID  string `json:"target_entity_id"`
}

// subscribeTeleportToEntity handles worldsim.entity.teleport_to_entity:
// teleports the sender to the target player's exact position on the same map.
//
// Authorization is enforced server-side (the frontend button visibility is
// cosmetic only):
//   - admins: always allowed;
//   - registered (non-guest) users: allowed only when the world option
//     allow_player_teleport is on;
//   - guests: always rejected.
//
// The target must be on the same map as the sender (the Players panel is
// current-map only). Fire-and-forget — no ack frame is sent back to the
// client; the sender sees its own position update via replication.
func (s *Simulator) subscribeTeleportToEntity() error {
	if _, err := s.nc.Subscribe("worldsim.entity.teleport_to_entity", func(m *nats.Msg) {
		_, span := otel.Tracer("worldsim").Start(otelinternal.Extract(context.Background(), m), "worldsim.entity.teleport_to_entity")
		defer span.End()
		var req teleportToRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		span.SetAttributes(
			attribute.String("teleport.sender_client_id", req.SenderClientID),
			attribute.String("teleport.target_entity_id", req.TargetEntityID),
		)

		s.mu.Lock()
		sender, senderOK := s.clients[req.SenderClientID]
		target, targetOK := s.entities[req.TargetEntityID]
		if !senderOK || !targetOK || sender.Position == nil || target.Position == nil {
			s.mu.Unlock()
			audit.Emit(s.nc, "player.teleport_to_entity", audit.SeverityWarn,
				audit.Actor{ClientID: req.SenderClientID},
				audit.Details{"target_entity_id": req.TargetEntityID, "result": "not_connected"},
				"")
			return
		}
		senderEntityID := sender.ID
		senderDisplayName := sender.DisplayName
		senderIsAdmin := sender.IsAdmin
		senderIsGuest := sender.IsGuest
		senderMap := sender.Position.MapId
		sameMap := senderMap == target.Position.MapId
		targetX := target.Position.X
		targetY := target.Position.Y
		targetMap := target.Position.MapId
		s.mu.Unlock()

		allowPlayerTeleport := s.worldOpts.Get().AllowPlayerTeleport
		allowed := senderIsAdmin || (!senderIsGuest && allowPlayerTeleport)

		if !allowed {
			audit.Emit(s.nc, "player.teleport_to_entity", audit.SeverityWarn,
				audit.Actor{ClientID: req.SenderClientID, EntityID: senderEntityID, DisplayName: senderDisplayName},
				audit.Details{"target_entity_id": req.TargetEntityID, "result": "forbidden",
					"is_admin": senderIsAdmin, "is_guest": senderIsGuest, "allow_player_teleport": allowPlayerTeleport},
				"")
			return
		}
		if !sameMap {
			audit.Emit(s.nc, "player.teleport_to_entity", audit.SeverityWarn,
				audit.Actor{ClientID: req.SenderClientID, EntityID: senderEntityID, DisplayName: senderDisplayName},
				audit.Details{"target_entity_id": req.TargetEntityID, "result": "cross_map",
					"sender_map": senderMap, "target_map": targetMap},
				"")
			return
		}

		// Authorized + same-map: apply the teleport.
		s.mu.Lock()
		sender.Position.X = targetX
		sender.Position.Y = targetY
		sender.dirtyPosition = true
		s.mu.Unlock()

		audit.Emit(s.nc, "player.teleport_to_entity", audit.SeverityInfo,
			audit.Actor{ClientID: req.SenderClientID, EntityID: senderEntityID, DisplayName: senderDisplayName},
			audit.Details{"target_entity_id": req.TargetEntityID, "target_map": targetMap,
				"x": targetX, "y": targetY, "result": "delivered"},
			"")
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.entity.teleport_to_entity: %w", err)
	}
	return nil
}

package worldsim

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	otelinternal "github.com/lstep/pixeleruv/backend/internal/otel"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/codes"
)

// entityTeleportRequest is the JSON payload published on
// worldsim.entity.teleport. Trusted callers (MCP, extensions) publish with an
// empty SenderClientID; the pusher forwards AdminTeleportFrame with
// SenderClientID set so worldsim can authorize the sender as an admin.
type entityTeleportRequest struct {
	EntityID       string  `json:"entity_id"`
	MapID          string  `json:"map_id"`
	TargetEntity   string  `json:"target_entity,omitempty"`
	SenderClientID string  `json:"sender_client_id,omitempty"`
	X              float32 `json:"x,omitempty"`
	Y              float32 `json:"y,omitempty"`
	ExactPosition  bool    `json:"exact_position,omitempty"`
}

// subscribeEntityTeleport handles worldsim.entity.teleport: moves an entity to
// a different map. The spawn position is resolved as:
//   - exact_position=true → use x/y directly (admin "teleport to me");
//   - else target_entity set → that named beacon's position on the target map;
//   - else a random spawn zone on the target map.
//
// When sender_client_id is set (the pusher forwards AdminTeleportFrame this
// way), the sender must be an admin — non-admins are rejected with a forbidden
// audit and no state change. An empty sender_client_id means a trusted caller
// (MCP, extensions) and skips the auth check, preserving the original behavior.
func (s *Simulator) subscribeEntityTeleport() error {
	if _, err := s.nc.Subscribe("worldsim.entity.teleport", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(otelinternal.Extract(context.Background(), m), "worldsim.entity.teleport")
		defer span.End()
		var req entityTeleportRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			s.logger.WarnContext(ctx, "teleport unmarshal", "err", err)
			return
		}

		// Sender authorization: only admins may teleport another player via
		// the pusher (AdminTeleportFrame). Trusted callers (MCP, extensions)
		// publish without sender_client_id and skip this check.
		if req.SenderClientID != "" {
			s.mu.Lock()
			sender, senderOK := s.clients[req.SenderClientID]
			senderIsAdmin := senderOK && sender.IsAdmin
			s.mu.Unlock()
			if !senderIsAdmin {
				audit.Emit(s.nc, "player.teleport", audit.SeverityWarn,
					audit.Actor{ClientID: req.SenderClientID},
					audit.Details{"target_entity_id": req.EntityID, "target_map": req.MapID, "result": "forbidden"},
					"")
				return
			}
		}

		s.mu.Lock()
		displayName := ""
		if e, ok := s.entities[req.EntityID]; ok {
			displayName = e.DisplayName
		}
		s.mu.Unlock()
		s.portal.transition(ctx, PortalInput{
			Entities:                 s.entities,
			Maps:                     s.maps,
			Zones:                    s.zones,
			MapWarnings:              s.mapWarnings,
			RNG:                      s.rng,
			PendingPortalTransitions: &s.pendingPortalTransitions,
			DestroyedEntities:        &s.destroyedEntities,
		}, req.EntityID, req.MapID, req.TargetEntity, req.X, req.Y, req.ExactPosition)
		audit.Emit(s.nc, "player.teleport", audit.SeverityInfo,
			audit.Actor{EntityID: req.EntityID, DisplayName: displayName},
			audit.Details{"target_map": req.MapID, "target_entity": req.TargetEntity,
				"exact_position": req.ExactPosition, "x": req.X, "y": req.Y},
			"")
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.entity.teleport: %w", err)
	}
	return nil
}

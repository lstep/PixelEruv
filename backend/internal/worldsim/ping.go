package worldsim

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/pb"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/protobuf/proto"
)

// pingRequest is the JSON payload published by the pusher on
// worldsim.client.ping when a client sends a PingPlayerFrame.
type pingRequest struct {
	EntityID       string `json:"entity_id"`
	SenderClientID string `json:"sender_client_id"`
}

// subscribeClientPing handles worldsim.client.ping: resolves the target
// entity_id → client_id, drops the ping silently if the target is in Do Not
// Disturb mode (status 2), and otherwise publishes a PlayerPingFrame
// (marshaled ServerFrame) to the target's client.<id>.ping_inbox so the
// pusher forwards it to the target's browser, which plays a notification
// sound. No-op if the target is not currently connected.
func (s *Simulator) subscribeClientPing() error {
	if _, err := s.nc.Subscribe("worldsim.client.ping", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.client.ping")
		defer span.End()
		var req pingRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		span.SetAttributes(
			attribute.String("ping.target_entity_id", req.EntityID),
			attribute.String("ping.sender_client_id", req.SenderClientID),
		)

		// Resolve target entity_id → client_id and snapshot sender info.
		s.mu.Lock()
		targetClientID := s.entityIDToClient[req.EntityID]
		var senderEntityID, senderDisplayName string
		if sender, ok := s.clients[req.SenderClientID]; ok {
			senderEntityID = sender.ID
			senderDisplayName = sender.DisplayName
		}
		targetDND := false
		if target, ok := s.entities[req.EntityID]; ok {
			targetDND = target.Status == statusDoNotDisturb
		}
		s.mu.Unlock()

		if targetClientID == "" {
			audit.Emit(s.nc, "player.ping", audit.SeverityInfo,
				audit.Actor{ClientID: req.SenderClientID, EntityID: senderEntityID, DisplayName: senderDisplayName},
				audit.Details{"target_entity_id": req.EntityID, "result": "not_connected"},
				"")
			return
		}

		if targetDND {
			audit.Emit(s.nc, "player.ping", audit.SeverityInfo,
				audit.Actor{ClientID: req.SenderClientID, EntityID: senderEntityID, DisplayName: senderDisplayName},
				audit.Details{"target_entity_id": req.EntityID, "target_client_id": targetClientID, "result": "dropped_dnd"},
				"")
			return
		}

		frame := &pb.ServerFrame{
			Payload: &pb.ServerFrame_PlayerPing{
				PlayerPing: &pb.PlayerPingFrame{
					EntityId:    senderEntityID,
					DisplayName: senderDisplayName,
				},
			},
		}
		frameBytes, err := proto.Marshal(frame)
		if err != nil {
			s.logger.WarnContext(ctx, "ping marshal", "err", err, "target", targetClientID)
			return
		}
		subject := fmt.Sprintf("client.%s.ping_inbox", targetClientID)
		if err := s.nc.Publish(subject, frameBytes); err != nil {
			s.logger.WarnContext(ctx, "ping publish", "err", err, "target", targetClientID)
		}
		audit.Emit(s.nc, "player.ping", audit.SeverityInfo,
			audit.Actor{ClientID: req.SenderClientID, EntityID: senderEntityID, DisplayName: senderDisplayName},
			audit.Details{"target_entity_id": req.EntityID, "target_client_id": targetClientID, "result": "delivered"},
			"")
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.client.ping: %w", err)
	}
	return nil
}

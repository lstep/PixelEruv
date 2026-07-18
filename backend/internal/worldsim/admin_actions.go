package worldsim

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/lstep/pixeleruv/backend/internal/pb"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/protobuf/proto"
)

// adminActor is the audit.Actor used for all admin actions initiated via the
// MCP server (or any other publisher on worldsim.admin.* / worldsim.client.kick
// / worldsim.client.ban). The actor's Extension field tags the source so audit
// consumers can filter admin-initiated events from client-initiated ones.
type adminActionRequest struct {
	// Actor is included by callers so audit events can attribute the action
	// (e.g. actor.extension="mcp", actor.sub="admin@idp"). Optional.
	Actor audit.Actor `json:"actor,omitempty"`
}

// adminResponse is the standard reply for admin action subjects. OK=false
// indicates the action was rejected (Error explains why); OK=true indicates
// the action was applied. All admin handlers reply so the MCP client can
// confirm the action landed.
type adminResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func respondAdmin(m *nats.Msg, resp adminResponse) {
	data, _ := json.Marshal(resp)
	if err := m.Respond(data); err != nil {
		_ = err // best-effort; caller may time out
	}
}

// --- worldsim.client.kick ---

type kickRequest struct {
	adminActionRequest
	ClientID string `json:"client_id"`
	Reason   string `json:"reason,omitempty"`
}

// subscribeClientKick handles worldsim.client.kick: despawns the named client
// via the existing despawnClient path (which saves position, emits zone.exit,
// publishes player.despawned audit) and emits a player.kicked audit event
// tagged with the admin actor. No-op if the client is not currently connected.
func (s *Simulator) subscribeClientKick() error {
	if _, err := s.nc.Subscribe("worldsim.client.kick", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.client.kick")
		defer span.End()
		var req kickRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		span.SetAttributes(attribute.String("client.id", req.ClientID))

		// Snapshot entity info under the lock so we can audit even though
		// despawnClient also locks.
		s.mu.Lock()
		e, ok := s.clients[req.ClientID]
		entityID := ""
		if ok {
			entityID = e.ID
		}
		s.mu.Unlock()

		if !ok {
			// Not currently connected — nothing to kick. Still audit the
			// attempt so failed admin actions are visible.
			audit.Emit(s.nc, "player.kick", audit.SeverityWarn,
				req.Actor,
				audit.Details{"client_id": req.ClientID, "result": "not_connected", "reason": req.Reason},
				"")
			respondAdmin(m, adminResponse{OK: false, Error: "not_connected"})
			return
		}

		s.despawnClient(ctx, req.ClientID)
		audit.Emit(s.nc, "player.kicked", audit.SeverityWarn,
			mergeActor(req.Actor, audit.Actor{EntityID: entityID, ClientID: req.ClientID}),
			audit.Details{"reason": req.Reason},
			"")
		respondAdmin(m, adminResponse{OK: true})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.client.kick: %w", err)
	}
	return nil
}

// --- worldsim.client.ban ---

type banRequest struct {
	adminActionRequest
	TargetType  string `json:"target_type"` // "user_id" | "ip" | "device_id"
	TargetValue string `json:"target_value"`
	Reason      string `json:"reason,omitempty"`
	BannedUntil int64  `json:"banned_until,omitempty"` // unix seconds; 0 = permanent
	BannedBy    string `json:"banned_by,omitempty"`
}

// banResponse is the reply to worldsim.client.ban.
type banResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Kicked  bool   `json:"kicked"`            // true if a currently-connected client was kicked as a result
	Reason  string `json:"reason,omitempty"`
}

// subscribeClientBan handles worldsim.client.ban: inserts a bans record via
// BanStore.Add, then kicks any currently-connected client matching the ban
// target. Emits player.banned audit. Replies with a banResponse so the caller
// (MCP) can confirm the ban landed and know whether a kick followed.
func (s *Simulator) subscribeClientBan() error {
	if _, err := s.nc.Subscribe("worldsim.client.ban", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.client.ban")
		defer span.End()
		var req banRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			respondBan(m, banResponse{Error: "bad request: " + err.Error()})
			return
		}
		span.SetAttributes(
			attribute.String("ban.target_type", req.TargetType),
			attribute.String("ban.target_value", req.TargetValue),
		)

		if s.banStore == nil {
			respondBan(m, banResponse{Error: "ban store not configured (PocketBase unavailable)"})
			return
		}
		if err := s.banStore.Add(req.TargetType, req.TargetValue, req.Reason, req.BannedUntil, req.BannedBy); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "ban store add")
			respondBan(m, banResponse{Error: "ban store: " + err.Error()})
			return
		}

		// Kick any currently-connected client matching the ban. We look up
		// the entity by the same three identifiers CheckBan uses.
		kickedClientID := ""
		s.mu.Lock()
		for clientID, e := range s.clients {
			if matchesBanTarget(e, req.TargetType, req.TargetValue) {
				kickedClientID = clientID
				break
			}
		}
		s.mu.Unlock()

		if kickedClientID != "" {
			s.despawnClient(ctx, kickedClientID)
		}

		audit.Emit(s.nc, "player.banned", audit.SeverityWarn,
			mergeActor(req.Actor, audit.Actor{}),
			audit.Details{
				"target_type":  req.TargetType,
				"target_value": req.TargetValue,
				"reason":       req.Reason,
				"until":        req.BannedUntil,
				"kicked":       kickedClientID != "",
			},
			"")

		respondBan(m, banResponse{OK: true, Kicked: kickedClientID != "", Reason: req.Reason})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.client.ban: %w", err)
	}
	return nil
}

func respondBan(m *nats.Msg, resp banResponse) {
	data, _ := json.Marshal(resp)
	if err := m.Respond(data); err != nil {
		// best-effort; the caller may time out
		_ = err
	}
}

// matchesBanTarget returns true if the entity's identifier of the given type
// equals value. Used by ban to find the currently-connected client to kick.
func matchesBanTarget(e *Entity, targetType, value string) bool {
	if value == "" {
		return false
	}
	switch targetType {
	case BanTargetUserID:
		// Entity doesn't carry sub directly; for logged-in users the sub is
		// the entity ID prefix in this codebase (entityID == sub for
		// logged-in users; see provisionClient). For guests we match by
		// device_id or IP instead.
		return e.ID == value
	case BanTargetIP:
		return e.IP == value
	case BanTargetDeviceID:
		return e.DeviceID == value
	}
	return false
}

// --- worldsim.admin.chat ---

type adminChatRequest struct {
	adminActionRequest
	EntityID string `json:"entity_id"`
	Channel  string `json:"channel"` // "global" or "proximity"
	Text     string `json:"text"`
}

// subscribeAdminChat lets an admin process (MCP) send a chat message as a
// specific entity, bypassing the client.<id>.chat path which requires the
// entity to be a connected client. The display name is stamped from the
// entity's current DisplayName. Routes through the same fan-out as
// handleChat (chat.broadcast for global, client.<recipient>.chat_inbox for
// proximity). Emits a chat.message audit tagged with the admin actor.
func (s *Simulator) subscribeAdminChat() error {
	if _, err := s.nc.Subscribe("worldsim.admin.chat", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.admin.chat")
		defer span.End()
		var req adminChatRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			return
		}
		span.SetAttributes(
			attribute.String("chat.entity", req.EntityID),
			attribute.String("chat.channel", req.Channel),
		)

		text := truncateRunes(req.Text, maxChatRunes)

		s.mu.Lock()
		e, ok := s.entities[req.EntityID]
		if !ok {
			s.mu.Unlock()
			span.SetStatus(codes.Error, "entity not found")
			respondAdmin(m, adminResponse{OK: false, Error: "entity not found"})
			return
		}
		displayName := e.DisplayName
		if displayName == "" {
			displayName = req.EntityID
		}
		msg := &pb.ChatMessageFrame{
			Channel:     req.Channel,
			EntityId:    req.EntityID,
			DisplayName: displayName,
			Text:        text,
			Timestamp:   uint64(time.Now().UnixMilli()),
		}
		frame := &pb.ServerFrame{Payload: &pb.ServerFrame_ChatMessage{ChatMessage: msg}}
		frameBytes, err := proto.Marshal(frame)
		if err != nil {
			s.mu.Unlock()
			span.RecordError(err)
			span.SetStatus(codes.Error, "marshal")
			respondAdmin(m, adminResponse{OK: false, Error: "marshal: " + err.Error()})
			return
		}

		var recipients []string
		switch req.Channel {
		case "global":
			recipients = nil
		case "proximity":
			group := e.currentProximityGroup
			if group == "" {
				s.mu.Unlock()
				span.SetStatus(codes.Error, "no proximity group")
				respondAdmin(m, adminResponse{OK: false, Error: "entity has no proximity group"})
				return
			}
			for _, other := range s.entities {
				if other.NetworkSession != nil && other.currentProximityGroup == group {
					if cid, ok := s.entityIDToClient[other.ID]; ok {
						recipients = append(recipients, cid)
					}
				}
			}
		default:
			s.mu.Unlock()
			span.SetStatus(codes.Error, "unknown channel")
			respondAdmin(m, adminResponse{OK: false, Error: "unknown channel: " + req.Channel})
			return
		}
		s.mu.Unlock()

		audit.Emit(s.nc, "chat.message", audit.SeverityInfo,
			mergeActor(req.Actor, audit.Actor{EntityID: req.EntityID}),
			audit.Details{
				"channel":      req.Channel,
				"text":         text,
				"display_name": displayName,
				"admin":        true,
			},
			"")

		if req.Channel == "global" {
			if err := s.nc.Publish("chat.broadcast", frameBytes); err != nil {
				s.logger.WarnContext(ctx, "admin chat broadcast publish", "err", err)
				span.RecordError(err)
				respondAdmin(m, adminResponse{OK: false, Error: "broadcast publish: " + err.Error()})
				return
			}
			respondAdmin(m, adminResponse{OK: true})
			return
		}
		for _, cid := range recipients {
			subject := fmt.Sprintf("client.%s.chat_inbox", cid)
			if err := s.nc.Publish(subject, frameBytes); err != nil {
				s.logger.WarnContext(ctx, "admin chat inbox publish", "err", err, "client", cid)
			}
		}
		respondAdmin(m, adminResponse{OK: true})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.chat: %w", err)
	}
	return nil
}

// --- worldsim.admin.set_name ---

type adminSetNameRequest struct {
	adminActionRequest
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
}

func (s *Simulator) subscribeAdminSetName() error {
	if _, err := s.nc.Subscribe("worldsim.admin.set_name", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.admin.set_name")
		defer span.End()
		var req adminSetNameRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			respondAdmin(m, adminResponse{OK: false, Error: "bad request: " + err.Error()})
			return
		}

		name := sanitizeName(req.Name)

		s.mu.Lock()
		e, ok := s.entities[req.EntityID]
		if !ok {
			s.mu.Unlock()
			span.SetStatus(codes.Error, "entity not found")
			respondAdmin(m, adminResponse{OK: false, Error: "entity not found"})
			return
		}
		e.DisplayName = name
		e.dirtyName = true
		isPlayer := e.NetworkSession != nil
		s.mu.Unlock()

		audit.Emit(s.nc, "player.set_name", audit.SeverityInfo,
			mergeActor(req.Actor, audit.Actor{EntityID: req.EntityID}),
			audit.Details{"name": name, "admin": true},
			"")

		// Persist for logged-in players. No-op for base entities / guests.
		if isPlayer && s.userStore != nil {
			if err := s.userStore.UpdateDisplayName(req.EntityID, name); err != nil {
				s.logger.WarnContext(ctx, "admin persist display name", "err", err, "entity", req.EntityID)
				span.RecordError(err)
			}
		}
		respondAdmin(m, adminResponse{OK: true})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_name: %w", err)
	}
	return nil
}

// --- worldsim.admin.set_status ---

type adminSetStatusRequest struct {
	adminActionRequest
	EntityID string `json:"entity_id"`
	Status   uint32 `json:"status"`
}

func (s *Simulator) subscribeAdminSetStatus() error {
	if _, err := s.nc.Subscribe("worldsim.admin.set_status", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.admin.set_status")
		defer span.End()
		var req adminSetStatusRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			respondAdmin(m, adminResponse{OK: false, Error: "bad request: " + err.Error()})
			return
		}
		if req.Status > statusDoNotDisturb {
			span.SetStatus(codes.Error, "invalid status")
			respondAdmin(m, adminResponse{OK: false, Error: "invalid status (must be 0-2)"})
			return
		}

		s.mu.Lock()
		e, ok := s.entities[req.EntityID]
		if !ok {
			s.mu.Unlock()
			span.SetStatus(codes.Error, "entity not found")
			respondAdmin(m, adminResponse{OK: false, Error: "entity not found"})
			return
		}
		if e.Status == req.Status {
			s.mu.Unlock()
			respondAdmin(m, adminResponse{OK: true})
			return
		}
		e.Status = req.Status
		e.dirtyName = true
		isPlayer := e.NetworkSession != nil
		s.mu.Unlock()

		if isPlayer && s.userStore != nil {
			if err := s.userStore.UpdateStatus(req.EntityID, req.Status); err != nil {
				s.logger.WarnContext(ctx, "admin persist status", "err", err, "entity", req.EntityID)
				span.RecordError(err)
			}
		}

		audit.Emit(s.nc, "player.set_status", audit.SeverityInfo,
			mergeActor(req.Actor, audit.Actor{EntityID: req.EntityID}),
			audit.Details{"status": req.Status, "admin": true},
			"")

		payload, _ := json.Marshal(struct {
			EntityID string `json:"entity_id"`
			Status   uint32 `json:"status"`
		}{EntityID: req.EntityID, Status: req.Status})
		if err := s.nc.Publish("worldsim.player_status", payload); err != nil {
			s.logger.WarnContext(ctx, "admin publish player_status", "err", err)
			span.RecordError(err)
		}
		respondAdmin(m, adminResponse{OK: true})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_status: %w", err)
	}
	return nil
}

// --- worldsim.admin.set_sprite ---

type adminSetSpriteRequest struct {
	adminActionRequest
	EntityID   string `json:"entity_id"`
	SpriteBase string `json:"sprite_base"`
}

func (s *Simulator) subscribeAdminSetSprite() error {
	if _, err := s.nc.Subscribe("worldsim.admin.set_sprite", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.admin.set_sprite")
		defer span.End()
		var req adminSetSpriteRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			respondAdmin(m, adminResponse{OK: false, Error: "bad request: " + err.Error()})
			return
		}

		if s.spriteStore != nil && req.SpriteBase != "" {
			exists, err := s.spriteStore.BaseExists(req.SpriteBase)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, "sprite store")
				respondAdmin(m, adminResponse{OK: false, Error: "sprite store: " + err.Error()})
				return
			}
			if !exists {
				span.SetStatus(codes.Error, "sprite_base not found")
				respondAdmin(m, adminResponse{OK: false, Error: "sprite_base not found"})
				return
			}
		}

		s.mu.Lock()
		e, ok := s.entities[req.EntityID]
		if !ok {
			s.mu.Unlock()
			span.SetStatus(codes.Error, "entity not found")
			respondAdmin(m, adminResponse{OK: false, Error: "entity not found"})
			return
		}
		e.SpriteBase = req.SpriteBase
		e.dirtyAppearance = true
		isPlayer := e.NetworkSession != nil
		s.mu.Unlock()

		audit.Emit(s.nc, "player.set_sprite_base", audit.SeverityInfo,
			mergeActor(req.Actor, audit.Actor{EntityID: req.EntityID}),
			audit.Details{"sprite_base": req.SpriteBase, "admin": true},
			"")

		if isPlayer && s.userStore != nil {
			if err := s.userStore.UpdateSpriteBase(req.EntityID, req.SpriteBase); err != nil {
				s.logger.WarnContext(ctx, "admin persist sprite_base", "err", err, "entity", req.EntityID)
				span.RecordError(err)
			}
		}
		respondAdmin(m, adminResponse{OK: true})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_sprite: %w", err)
	}
	return nil
}

// --- worldsim.admin.set_player_options ---

type adminSetPlayerOptionsRequest struct {
	adminActionRequest
	EntityID string `json:"entity_id"`
	Options  string `json:"options"` // JSON-encoded; full replace
}

func (s *Simulator) subscribeAdminSetPlayerOptions() error {
	if _, err := s.nc.Subscribe("worldsim.admin.set_player_options", func(m *nats.Msg) {
		ctx, span := s.tracer.Start(context.Background(), "worldsim.admin.set_player_options")
		defer span.End()
		var req adminSetPlayerOptionsRequest
		if err := json.Unmarshal(m.Data, &req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "unmarshal")
			respondAdmin(m, adminResponse{OK: false, Error: "bad request: " + err.Error()})
			return
		}

		s.mu.Lock()
		e, ok := s.entities[req.EntityID]
		if !ok {
			s.mu.Unlock()
			span.SetStatus(codes.Error, "entity not found")
			respondAdmin(m, adminResponse{OK: false, Error: "entity not found"})
			return
		}
		e.PlayerOptions = req.Options
		isPlayer := e.NetworkSession != nil
		s.mu.Unlock()

		audit.Emit(s.nc, "player.set_player_options", audit.SeverityInfo,
			mergeActor(req.Actor, audit.Actor{EntityID: req.EntityID}),
			audit.Details{"options": req.Options, "admin": true},
			"")

		if isPlayer && s.userStore != nil {
			if err := s.userStore.UpdateOptions(req.EntityID, req.Options); err != nil {
				s.logger.WarnContext(ctx, "admin persist player options", "err", err, "entity", req.EntityID)
				span.RecordError(err)
			}
		}
		respondAdmin(m, adminResponse{OK: true})
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.admin.set_player_options: %w", err)
	}
	return nil
}

// --- helpers ---

// sanitizeName mirrors handleSetName's sanitization: ASCII printable only,
// truncated to maxNameRunes.
func sanitizeName(raw string) string {
	cleaned := make([]rune, 0, len(raw))
	for _, r := range raw {
		if r >= 32 && r <= 126 {
			cleaned = append(cleaned, r)
		}
	}
	if len(cleaned) > maxNameRunes {
		cleaned = cleaned[:maxNameRunes]
	}
	return string(cleaned)
}

// truncateRunes returns s truncated to at most max runes (rune-safe).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}

// mergeActor overlays non-zero fields from b onto a, returning the merged
// actor. Fields from b win when set. Used so admin actions can attribute the
// actor (extension/sub from the request) while still stamping entity_id /
// client_id from the resolved entity.
func mergeActor(a, b audit.Actor) audit.Actor {
	out := a
	if out.Sub == "" {
		out.Sub = b.Sub
	}
	if out.EntityID == "" {
		out.EntityID = b.EntityID
	}
	if out.ClientID == "" {
		out.ClientID = b.ClientID
	}
	if out.IP == "" {
		out.IP = b.IP
	}
	if out.DeviceID == "" {
		out.DeviceID = b.DeviceID
	}
	if out.Extension == "" {
		out.Extension = b.Extension
	}
	return out
}

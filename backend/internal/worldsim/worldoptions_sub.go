package worldsim

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/lstep/pixeleruv/backend/internal/audit"
	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase/core"
)

// worldOptionsReply is the reply payload for worldsim.world_options.get and
// worldsim.world_options.set. On error, OK=false and Error is set; on
// success, Options holds the current value.
type worldOptionsReply struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Options WorldOptions `json:"options,omitempty"`
}

// worldOptionsSetRequest is the request payload for worldsim.world_options.set.
// Options is the full replacement value; Actor attributes the audit event so
// consumers can distinguish admin-portal-initiated sets (actor.extension="admin",
// actor.sub=<admin email>) from MCP-initiated sets (actor.extension="mcp"). If
// Actor is zero-valued, the handler defaults to actor.extension="admin" for
// backward compatibility with callers that omit it.
type worldOptionsSetRequest struct {
	Options WorldOptions  `json:"options"`
	Actor   audit.Actor   `json:"actor,omitempty"`
}

// subscribeWorldOptions sets up the worldsim.world_options.get and
// worldsim.world_options.set request-reply handlers. The admin portal calls
// these to read and write the server-wide runtime config; worldsim owns the
// NATS KV bucket. set is admin-gated by the admin portal's signed-cookie
// session (the portal refuses unauthenticated requests before calling
// worldsim), so no second auth check is needed here — worldsim trusts the
// admin service as a NATS peer, same as the recording.* handlers trust
// ext-rec.
func (s *Simulator) subscribeWorldOptions() error {
	if _, err := s.nc.Subscribe("worldsim.world_options.get", func(msg *nats.Msg) {
		opts := s.worldOpts.Get()
		reply, _ := json.Marshal(worldOptionsReply{OK: true, Options: opts})
		if err := msg.Respond(reply); err != nil {
			s.logger.Warn("world_options.get respond", "err", err)
		}
	}); err != nil {
		return err
	}

	if _, err := s.nc.Subscribe("worldsim.world_options.set", func(msg *nats.Msg) {
		var req worldOptionsSetRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			reply, _ := json.Marshal(worldOptionsReply{Error: "unmarshal: " + err.Error()})
			msg.Respond(reply)
			return
		}
		if err := s.worldOpts.Set(req.Options); err != nil {
			s.logger.Warn("world_options.set", "err", err)
			reply, _ := json.Marshal(worldOptionsReply{Error: err.Error()})
			msg.Respond(reply)
			return
		}
		actor := req.Actor
		if actor.Extension == "" {
			actor.Extension = "admin"
		}
		opts := req.Options
		audit.Emit(s.nc, "world_options.updated", audit.SeverityInfo,
			actor,
			audit.Details{
				"smtp_host":          opts.SMTPHost,
				"app_url":            opts.AppURL,
				"ffmpeg_concurrency": opts.FFmpegConcurrency,
				"ffmpeg_timeout_s":   int64(opts.FFmpegTimeout.Seconds()),
				"recording_enabled":     opts.RecordingEnabled,
				"allow_player_teleport": opts.AllowPlayerTeleport,
				"king_name":             opts.KingName,
				"error_email_mode":   opts.ErrorEmailRecipientsMode,
			},
			"")
		reply, _ := json.Marshal(worldOptionsReply{OK: true, Options: s.worldOpts.Get()})
		if err := msg.Respond(reply); err != nil {
			s.logger.Warn("world_options.set respond", "err", err)
		}
	}); err != nil {
		return err
	}

	// worldsim.admin_emails.get returns the email addresses of every user
	// linked to a players row with is_admin=true. Used by the audit service's
	// error-email notifier when error_email_recipients_mode == "all_admins".
	// worldsim owns PocketBase, so it resolves the query; the audit service
	// has no PB access.
	if _, err := s.nc.Subscribe("worldsim.admin_emails.get", func(msg *nats.Msg) {
		emails, err := s.userStore.AdminEmails()
		if err != nil {
			s.logger.Warn("admin_emails.get", "err", err)
			reply, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			msg.Respond(reply)
			return
		}
		reply, _ := json.Marshal(map[string]any{"ok": true, "emails": emails})
		if err := msg.Respond(reply); err != nil {
			s.logger.Warn("admin_emails.get respond", "err", err)
		}
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.admin_emails.get: %w", err)
	}

	// worldsim.players.list returns all registered players from PocketBase
	// (user_id, display_name, entity_id, is_admin, created). Used by the audit
	// service's /audit/players leaderboard to show all registered players,
	// not just those with audit events. worldsim owns PocketBase, so it
	// resolves the query; the audit service has no PB access.
	if _, err := s.nc.Subscribe("worldsim.players.list", func(msg *nats.Msg) {
		players, err := s.userStore.ListAllPlayers()
		if err != nil {
			s.logger.Warn("players.list", "err", err)
			reply, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			msg.Respond(reply)
			return
		}
		reply, _ := json.Marshal(map[string]any{"ok": true, "players": players})
		if err := msg.Respond(reply); err != nil {
			s.logger.Warn("players.list respond", "err", err)
		}
	}); err != nil {
		return fmt.Errorf("subscribe worldsim.players.list: %w", err)
	}
	return nil
}

// handleWorldOptionsHTTP is the GET /api/world-options handler on the
// embedded PocketBase. Admin-gated via the users JWT (RequireAuth("users")
// middleware loads e.Auth) + players.is_admin check. Returns only the
// YouTube fields + public_host — the fields the frontend's "Stream to
// YouTube" confirm modal needs. SMTP password and other sensitive fields
// are not exposed to the browser.
func (s *Simulator) handleWorldOptionsHTTP(e *core.RequestEvent) error {
	if e.Auth == nil {
		return e.JSON(http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	}
	// Check is_admin via the players collection (user_id == e.Auth.Id).
	isAdmin, err := s.userStore.IsAdmin(e.Auth.Id)
	if err != nil {
		s.logger.Warn("world-options HTTP: is_admin check", "err", err, "sub", e.Auth.Id)
		return e.JSON(http.StatusInternalServerError, map[string]any{"error": "is_admin check failed"})
	}
	if !isAdmin {
		return e.JSON(http.StatusForbidden, map[string]any{"error": "admin only"})
	}
	opts := s.worldOpts.Get()
	return e.JSON(http.StatusOK, map[string]any{
		"youtube_rtmp_url":   opts.YoutubeRTMPURL,
		"youtube_stream_key": opts.YoutubeStreamKey,
		"public_host":        opts.PublicHost,
		"recording_enabled":  opts.RecordingEnabled,
	})
}

// handlePlayerTeleportOptionHTTP is the GET /api/world-options/player-teleport
// handler. Auth-required (any logged-in users JWT) but NOT admin-gated —
// registered non-admin players need to know whether to show the Teleport-to
// button in the Players panel. Guests have no users JWT, so they get 401 and
// the frontend hides the button. Returns a single boolean field.
func (s *Simulator) handlePlayerTeleportOptionHTTP(e *core.RequestEvent) error {
	if e.Auth == nil {
		return e.JSON(http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
	}
	opts := s.worldOpts.Get()
	return e.JSON(http.StatusOK, map[string]any{
		"allow_player_teleport": opts.AllowPlayerTeleport,
	})
}

// handleWorldKingHTTP is the GET /api/world-king handler on the embedded
// PocketBase. Public (no auth) — returns only the king's display name so the
// welcome page footer can show it. The king's email is NOT exposed here
// (spam risk); it's visible only on the admin World Options page. Returns
// 200 with {"name":""} when no king is configured.
func (s *Simulator) handleWorldKingHTTP(e *core.RequestEvent) error {
	opts := s.worldOpts.Get()
	return e.JSON(http.StatusOK, map[string]any{
		"name": opts.KingName,
	})
}

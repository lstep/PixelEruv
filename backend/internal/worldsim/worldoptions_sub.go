package worldsim

import (
	"encoding/json"
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
		var opts WorldOptions
		if err := json.Unmarshal(msg.Data, &opts); err != nil {
			reply, _ := json.Marshal(worldOptionsReply{Error: "unmarshal: " + err.Error()})
			msg.Respond(reply)
			return
		}
		if err := s.worldOpts.Set(opts); err != nil {
			s.logger.Warn("world_options.set", "err", err)
			reply, _ := json.Marshal(worldOptionsReply{Error: err.Error()})
			msg.Respond(reply)
			return
		}
		audit.Emit(s.nc, "world_options.updated", audit.SeverityInfo,
			audit.Actor{Extension: "admin"},
			audit.Details{
				"smtp_host":          opts.SMTPHost,
				"app_url":            opts.AppURL,
				"ffmpeg_concurrency": opts.FFmpegConcurrency,
				"ffmpeg_timeout_s":   int64(opts.FFmpegTimeout.Seconds()),
			},
			"")
		reply, _ := json.Marshal(worldOptionsReply{OK: true, Options: s.worldOpts.Get()})
		if err := msg.Respond(reply); err != nil {
			s.logger.Warn("world_options.set respond", "err", err)
		}
	}); err != nil {
		return err
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
	})
}

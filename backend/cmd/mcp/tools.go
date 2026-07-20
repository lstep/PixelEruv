package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires all MCP tools onto the server. Tools are grouped:
//   - Read tools: world state, entities, audit, PocketBase, world options
//   - Control tools: teleport, kick, ban
//   - Admin override tools: send_chat_as, set_player_*, set_world_options,
//     dispatch_extension_action
//   - Docker tools: list containers, engine info (via docker-readonly-proxy)
//
// Each tool uses the typed ToolHandlerFor pattern so the SDK validates input
// against the In struct's JSON schema automatically.
func registerTools(s *mcp.Server, w *WorldsimClient, a *AuditClient, pb *PocketBaseClient, d *DockerClient) {
	registerReadTools(s, w, a, pb)
	registerControlTools(s, w)
	registerAdminTools(s, w)
	registerDockerTools(s, d)
}

// --- Read tools ---

func registerReadTools(s *mcp.Server, w *WorldsimClient, a *AuditClient, pb *PocketBaseClient) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_world_stats",
		Description: "Get the current worldsim snapshot: tick rate, uptime, total players/entities, per-map counts, per-player state (entity_id, client_id, display_name, map, x/y, is_admin, is_guest, IP), and per-extension health. No arguments.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		stats, err := w.GetStats(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(stats)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_zones",
		Description: "Get zone metadata for all maps: id, type, shape, AV flags, exclusive flag, portal targets. No arguments.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		zones, err := w.GetZones(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(zones)
	})

	type QueryEntitiesArgs struct {
		MapID          string `json:"map_id,omitempty"          jsonschema:"Filter by map ID"`
		EntityType     string `json:"entity_type,omitempty"     jsonschema:"Filter by entity type (e.g. wall, light)"`
		OwnerExtension string `json:"owner_extension,omitempty" jsonschema:"Filter by owning extension ID (e.g. ext-walls)"`
		ZoneID         string `json:"zone_id,omitempty"         jsonschema:"Filter to players currently inside this zone ID"`
		Limit          int    `json:"limit,omitempty"           jsonschema:"Max results (default 500, hard cap 500)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_entities",
		Description: "Query entities in worldsim with optional filters. Returns up to `limit` (default 500) entity snapshots sorted by entity_id. Each snapshot includes position, type, owner extension, display name (players), state, sprite, light fields.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args QueryEntitiesArgs) (*mcp.CallToolResult, any, error) {
		entities, err := w.QueryEntities(ctx, EntitiesQuery{
			MapID:          args.MapID,
			EntityType:     args.EntityType,
			OwnerExtension: args.OwnerExtension,
			ZoneID:         args.ZoneID,
			Limit:          args.Limit,
		})
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(entities)
	})

	type GetEntityArgs struct {
		EntityID string `json:"entity_id" jsonschema:"Entity ID to fetch"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_entity",
		Description: "Get a single entity snapshot by ID. Returns position, type, owner extension, display name (players), state, sprite, light fields.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetEntityArgs) (*mcp.CallToolResult, any, error) {
		snap, err := w.GetEntity(ctx, args.EntityID)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(snap)
	})

	type QueryAuditArgs struct {
		EventType string `json:"event_type,omitempty" jsonschema:"Filter by event type (e.g. player.kicked, chat.message)"`
		Severity  string `json:"severity,omitempty"  jsonschema:"Filter by severity: info, warn, error"`
		ActorSub  string `json:"actor_sub,omitempty"  jsonschema:"Filter by actor OIDC subject"`
		EntityID  string `json:"entity_id,omitempty"  jsonschema:"Filter by actor entity_id"`
		Limit     int    `json:"limit,omitempty"      jsonschema:"Max results (default 50, max 500)"`
		Offset    int    `json:"offset,omitempty"     jsonschema:"Pagination offset"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_audit_events",
		Description: "Query historical audit events from the audit service. Audit events capture lifecycle + interaction granularity: connects, disconnects, kicks, bans, chat, name/sprite/status changes, zone transitions, teleports. Each event carries an optional trace_id linking to the OpenTelemetry trace.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args QueryAuditArgs) (*mcp.CallToolResult, any, error) {
		events, err := a.QueryEvents(ctx, AuditQuery{
			EventType: args.EventType,
			Severity:  args.Severity,
			ActorSub:  args.ActorSub,
			EntityID:  args.EntityID,
			Limit:     args.Limit,
			Offset:    args.Offset,
		})
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(events)
	})

	type GetAuditEventArgs struct {
		ID int64 `json:"id" jsonschema:"Audit event ID"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_audit_event",
		Description: "Get a single audit event by ID, with full details and trace_id.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetAuditEventArgs) (*mcp.CallToolResult, any, error) {
		ev, err := a.GetEvent(ctx, args.ID)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(ev)
	})

	type PlayerTimelineArgs struct {
		Sub string `json:"sub" jsonschema:"Player OIDC subject"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "player_timeline",
		Description: "Get the audit event timeline for a player (by OIDC subject). Returns up to 200 recent events where the player was the actor.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args PlayerTimelineArgs) (*mcp.CallToolResult, any, error) {
		events, err := a.PlayerTimeline(ctx, args.Sub)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(events)
	})

	type ListPBRecordsArgs struct {
		Collection string `json:"collection" jsonschema:"PocketBase collection name (e.g. players, maps, sprite_bases, bans)"`
		PerPage    int    `json:"per_page,omitempty" jsonschema:"Page size (default 30)"`
		Page       int    `json:"page,omitempty" jsonschema:"Page number (1-based, default 1)"`
		Filter     string `json:"filter,omitempty" jsonschema:"PocketBase filter expression, e.g. 'is_default = true' or 'is_admin = true'"`
		Sort       string `json:"sort,omitempty" jsonschema:"Sort expression, e.g. '-created' or 'display_name'"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_pb_records",
		Description: "List records from a PocketBase collection with optional filter/sort/pagination. Returns the raw PocketBase paginated response (items + totalItems + totalPages).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListPBRecordsArgs) (*mcp.CallToolResult, any, error) {
		data, err := pb.ListRecords(ctx, args.Collection, ListParams{
			PerPage: args.PerPage,
			Page:    args.Page,
			Filter:  args.Filter,
			Sort:    args.Sort,
		})
		if err != nil {
			return nil, nil, err
		}
		return rawJSONResult(data)
	})

	type GetPBRecordArgs struct {
		Collection string `json:"collection" jsonschema:"PocketBase collection name"`
		ID         string `json:"id" jsonschema:"Record ID"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_pb_record",
		Description: "Get a single PocketBase record by collection + ID. Returns the raw record JSON.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args GetPBRecordArgs) (*mcp.CallToolResult, any, error) {
		data, err := pb.GetRecord(ctx, args.Collection, args.ID)
		if err != nil {
			return nil, nil, err
		}
		return rawJSONResult(data)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_world_options",
		Description: "Get the current server-wide runtime config (world_options KV bucket): SMTP, AppURL, YouTube RTMP defaults, ffmpeg limits, world king, error-email recipients, recording gate, and readOnly env mirrors (public_host, livekit_public_url). No arguments.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		opts, err := w.GetWorldOptions(ctx)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(opts)
	})
}

// --- Control tools ---

func registerControlTools(s *mcp.Server, w *WorldsimClient) {
	type TeleportArgs struct {
		EntityID     string `json:"entity_id" jsonschema:"Player entity ID to teleport"`
		MapID        string `json:"map_id" jsonschema:"Target map ID"`
		TargetEntity string `json:"target_entity,omitempty" jsonschema:"Optional target entity (beacon name) on the target map; if empty, a random spawn zone is used"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "teleport_entity",
		Description: "Teleport a player entity to a target map, optionally to a named beacon entity on that map. If target_entity is empty, lands at a random spawn zone. Fire-and-forget: no confirmation reply from worldsim.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args TeleportArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.Teleport(ctx, args.EntityID, args.MapID, args.TargetEntity)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type KickArgs struct {
		ClientID string `json:"client_id" jsonschema:"Pusher session ID (client_id from get_world_stats) to kick"`
		Reason   string `json:"reason,omitempty" jsonschema:"Human-readable kick reason (recorded in audit)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "kick_player",
		Description: "Force-disconnect a currently-connected player by client_id. Saves their position, emits zone.exit, publishes player.kicked audit. No-op (with audit) if the client is not currently connected.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args KickArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.Kick(ctx, args.ClientID, args.Reason)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type BanArgs struct {
		TargetType  string `json:"target_type" jsonschema:"Which identifier to ban: 'user_id', 'ip', or 'device_id'"`
		TargetValue string `json:"target_value" jsonschema:"The identifier value to ban"`
		Reason      string `json:"reason,omitempty" jsonschema:"Human-readable ban reason (recorded in audit + shown to the player on connect)"`
		BannedUntil int64  `json:"banned_until,omitempty" jsonschema:"Unix timestamp (seconds) when the ban expires. 0 = permanent."`
		BannedBy    string `json:"banned_by,omitempty" jsonschema:"Optional audit label (e.g. admin sub or 'mcp')"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ban_player",
		Description: "Insert a ban record into PocketBase (target_type: user_id / ip / device_id) and kick any currently-connected client matching the ban. Emits player.banned audit. banned_until=0 means permanent.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args BanArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.Ban(ctx, args.TargetType, args.TargetValue, args.Reason, args.BannedUntil, args.BannedBy)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})
}

// --- Admin override tools ---

func registerAdminTools(s *mcp.Server, w *WorldsimClient) {
	type SendChatAsArgs struct {
		EntityID string `json:"entity_id" jsonschema:"Entity ID to send as (display name is stamped from the entity)"`
		Channel  string `json:"channel" jsonschema:"Chat channel: 'global' or 'proximity'"`
		Text     string `json:"text" jsonschema:"Message text (truncated to 500 runes server-side)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_chat_as",
		Description: "Send a chat message as a specific entity, bypassing the connected-client requirement. Channel must be 'global' (broadcasts to all sessions) or 'proximity' (requires the entity to be in a proximity group). Emits chat.message audit tagged admin=true.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SendChatAsArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.SendChatAs(ctx, args.EntityID, args.Channel, args.Text)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type SetNameArgs struct {
		EntityID string `json:"entity_id" jsonschema:"Entity ID to rename"`
		Name     string `json:"name" jsonschema:"New display name (sanitized to ASCII printable, truncated to 20 runes)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_player_name",
		Description: "Change an entity's display name. Sanitized to ASCII printable (32-126), truncated to 20 runes. Persists to PocketBase for logged-in players; session-only for guests / base entities.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SetNameArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.SetName(ctx, args.EntityID, args.Name)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type SetStatusArgs struct {
		EntityID string `json:"entity_id" jsonschema:"Entity ID"`
		Status   uint32 `json:"status" jsonschema:"Presence status: 0=Available, 1=Busy, 2=Do Not Disturb"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_player_status",
		Description: "Change an entity's presence status (0=Available, 1=Busy, 2=DND). Persists to PocketBase for logged-in players. Broadcasts on worldsim.player_status so ext-av enforces DND A/V exclusion.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SetStatusArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.SetStatus(ctx, args.EntityID, args.Status)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type SetSpriteArgs struct {
		EntityID   string `json:"entity_id" jsonschema:"Entity ID"`
		SpriteBase string `json:"sprite_base" jsonschema:"sprite_bases PocketBase record ID; empty = revert to fallback"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_player_sprite",
		Description: "Change an entity's character sheet (sprite_bases record ID). Validates the ID exists in sprite_bases (unless empty). Persists to PocketBase for logged-in players.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SetSpriteArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.SetSprite(ctx, args.EntityID, args.SpriteBase)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type SetPlayerOptionsArgs struct {
		EntityID string `json:"entity_id" jsonschema:"Entity ID"`
		Options  string `json:"options" jsonschema:"JSON-encoded player options (full replace, e.g. {\"show_own_name_tag\":true,\"zoom\":2})"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_player_options",
		Description: "Replace an entity's player options JSON. Full replace (not partial merge). Persists to PocketBase for logged-in players. Common fields: show_own_name_tag (bool), zoom (number 1-4).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SetPlayerOptionsArgs) (*mcp.CallToolResult, any, error) {
		resp, err := w.SetPlayerOptions(ctx, args.EntityID, args.Options)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(resp)
	})

	type DispatchExtensionActionArgs struct {
		ExtensionID string         `json:"extension_id" jsonschema:"Extension ID (e.g. ext-walls, ext-props)"`
		Payload     map[string]any `json:"payload" jsonschema:"Action dispatch payload (entity_id, input, action_id, etc.) — passed through to extension.<id>.action"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "dispatch_extension_action",
		Description: "Publish an action dispatch to a specific extension's extension.<id>.action subject. Fire-and-forget: no reply. Use the audit log to confirm whether the extension handled it. Payload shape matches worldsim's actionDispatchMsg (entity_id, input, action_id, ...).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args DispatchExtensionActionArgs) (*mcp.CallToolResult, any, error) {
		if err := w.DispatchExtensionAction(ctx, args.ExtensionID, args.Payload); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]any{"ok": true, "extension": args.ExtensionID})
	})

	type SetWorldOptionsArgs struct {
		SMTPHost                  string `json:"smtp_host" jsonschema:"SMTP server hostname (required)"`
		SMTPPort                  int    `json:"smtp_port" jsonschema:"SMTP server port (1-65535)"`
		SMTPUsername              string `json:"smtp_username,omitempty" jsonschema:"SMTP auth username (empty = no auth)"`
		SMTPPassword              string `json:"smtp_password,omitempty" jsonschema:"SMTP auth password"`
		SMTPFrom                  string `json:"smtp_from,omitempty" jsonschema:"From: address for outgoing email"`
		SMTPSender                string `json:"smtp_sender_name,omitempty" jsonschema:"Display name for the From: address"`
		SMTPTLS                   bool   `json:"smtp_tls,omitempty" jsonschema:"Enable TLS for SMTP"`
		AppURL                    string `json:"app_url,omitempty" jsonschema:"Public app URL used in email templates (verification, reset)"`
		YoutubeRTMPURL            string `json:"youtube_rtmp_url,omitempty" jsonschema:"Default YouTube RTMP URL for the Stream to YouTube recording target (empty = YouTube disabled, MP4 still works)"`
		YoutubeStreamKey          string `json:"youtube_stream_key,omitempty" jsonschema:"Default YouTube stream key"`
		FFmpegConcurrency         int    `json:"ffmpeg_concurrency,omitempty" jsonschema:"Max simultaneous ffmpeg audio extractions (>= 1, default 2)"`
		FFmpegTimeout             int64  `json:"ffmpeg_timeout,omitempty" jsonschema:"Per-run ffmpeg deadline in nanoseconds (1 minute = 60000000000; default 10m = 600000000000)"`
		KingName                  string `json:"king_name,omitempty" jsonschema:"Display name shown on the welcome page footer"`
		KingEmail                 string `json:"king_email,omitempty" jsonschema:"World king contact email; also the default error-email recipient when error_email_recipients_mode=king"`
		ErrorEmailRecipientsMode  string `json:"error_email_recipients_mode,omitempty" jsonschema:"Error-email recipients: none | king | all_admins | custom"`
		ErrorEmailCustomAddresses string `json:"error_email_custom_addresses,omitempty" jsonschema:"Comma-separated emails, used only when error_email_recipients_mode=custom"`
		RecordingEnabled          bool   `json:"recording_enabled,omitempty" jsonschema:"Gates meeting recording globally (false = ext-rec refuses recording.start, frontend disables Record button)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_world_options",
		Description: "Replace the server-wide runtime config (world_options KV bucket). Full replace (not partial merge): call get_world_options first, modify the fields you want to change, then pass the full object back. worldsim validates, writes KV, and broadcasts world_options.update so consumers (SMTP client, ext-rec ffmpeg/YouTube, frontend recording gate) hot-reload without restart. readOnly fields (public_host, livekit_public_url) are preserved by worldsim and ignored in the input. Validation: smtp_host required, smtp_port 1-65535, ffmpeg_concurrency >= 1, ffmpeg_timeout >= 1s, error_email_recipients_mode must be one of none|king|all_admins|custom (king requires king_email, custom requires error_email_custom_addresses). Emits world_options.updated audit event tagged actor.extension=mcp (configurable via MCP_ACTOR).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args SetWorldOptionsArgs) (*mcp.CallToolResult, any, error) {
		opts := WorldOptions{
			SMTPHost:                  args.SMTPHost,
			SMTPPort:                  args.SMTPPort,
			SMTPUsername:              args.SMTPUsername,
			SMTPPassword:              args.SMTPPassword,
			SMTPFrom:                  args.SMTPFrom,
			SMTPSender:                args.SMTPSender,
			SMTPTLS:                   args.SMTPTLS,
			AppURL:                    args.AppURL,
			YoutubeRTMPURL:            args.YoutubeRTMPURL,
			YoutubeStreamKey:          args.YoutubeStreamKey,
			FFmpegConcurrency:         args.FFmpegConcurrency,
			FFmpegTimeout:             args.FFmpegTimeout,
			KingName:                  args.KingName,
			KingEmail:                 args.KingEmail,
			ErrorEmailRecipientsMode:  args.ErrorEmailRecipientsMode,
			ErrorEmailCustomAddresses: args.ErrorEmailCustomAddresses,
			RecordingEnabled:          args.RecordingEnabled,
		}
		updated, err := w.SetWorldOptions(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(updated)
	})
}

// --- Docker tools ---

// registerDockerTools exposes the running Docker container list + engine info
// via the docker-readonly-proxy (DOCKER_PROXY_URL). If the proxy URL is not
// configured, the tools return an error when called but still register, so
// the MCP server can run with a subset of backends.
func registerDockerTools(s *mcp.Server, d *DockerClient) {
	type ListDockerContainersArgs struct {
		AllProjects bool `json:"all_projects,omitempty" jsonschema:"If true, return every container on the host engine. Default false: filter to com.docker.compose.project=pixeleruv."`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_docker_containers",
		Description: "List Docker containers (running + stopped) via the docker-readonly-proxy. Default filters to the pixeleruv compose project; pass all_projects=true to list every container on the host engine. Each row: id, name, image, image_id, state (running|created|exited|paused|restarting|...), status (human string like 'Up 5 minutes'), created (unix seconds), labels (incl. com.docker.compose.project/service).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args ListDockerContainersArgs) (*mcp.CallToolResult, any, error) {
		rows, err := d.ListContainers(ctx, args.AllProjects)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(rows)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_docker_info",
		Description: "Get Docker engine info via the docker-readonly-proxy (GET /info). Returns the raw Docker engine JSON: containers/containersRunning/containersPaused/containersStopped counts, images count, OS, architecture, kernel version, docker root dir, etc. No arguments.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		data, err := d.Info(ctx)
		if err != nil {
			return nil, nil, err
		}
		return rawJSONResult(data)
	})
}

// jsonResult marshals v into a CallToolResult with a single TextContent
// containing the JSON. The same value is also returned as the typed Out so
// the SDK populates StructuredContent for clients that want it.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("marshal result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(data)},
		},
	}, v, nil
}

// rawJSONResult wraps a pre-marshaled JSON payload (e.g. from PocketBase REST)
// into a CallToolResult without re-marshaling.
func rawJSONResult(data []byte) (*mcp.CallToolResult, any, error) {
	var v any
	_ = json.Unmarshal(data, &v)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: string(data)},
		},
	}, v, nil
}

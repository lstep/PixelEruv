package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools wires all MCP tools onto the server. Tools are grouped:
//   - Read tools: world state, entities, audit, PocketBase
//   - Control tools: teleport, kick, ban
//   - Admin override tools: send_chat_as, set_player_*, dispatch_extension_action
//
// Each tool uses the typed ToolHandlerFor pattern so the SDK validates input
// against the In struct's JSON schema automatically.
func registerTools(s *mcp.Server, w *WorldsimClient, a *AuditClient, pb *PocketBaseClient) {
	registerReadTools(s, w, a, pb)
	registerControlTools(s, w)
	registerAdminTools(s, w)
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
		MapID          string `json:"map_id,omitempty"          jsonschema:"description=Filter by map ID"`
		EntityType     string `json:"entity_type,omitempty"     jsonschema:"description=Filter by entity type (e.g. wall, light)"`
		OwnerExtension string `json:"owner_extension,omitempty" jsonschema:"description=Filter by owning extension ID (e.g. ext-walls)"`
		ZoneID         string `json:"zone_id,omitempty"         jsonschema:"description=Filter to players currently inside this zone ID"`
		Limit          int    `json:"limit,omitempty"           jsonschema:"description=Max results (default 500, hard cap 500)"`
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
		EntityID string `json:"entity_id" jsonschema:"description=Entity ID to fetch,required"`
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
		EventType string `json:"event_type,omitempty" jsonschema:"description=Filter by event type (e.g. player.kicked, chat.message)"`
		Severity  string `json:"severity,omitempty"  jsonschema:"description=Filter by severity: info, warn, error"`
		ActorSub  string `json:"actor_sub,omitempty"  jsonschema:"description=Filter by actor OIDC subject"`
		EntityID  string `json:"entity_id,omitempty"  jsonschema:"description=Filter by actor entity_id"`
		Limit     int    `json:"limit,omitempty"      jsonschema:"description=Max results (default 50, max 500)"`
		Offset    int    `json:"offset,omitempty"     jsonschema:"description=Pagination offset"`
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
		ID int64 `json:"id" jsonschema:"description=Audit event ID,required"`
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
		Sub string `json:"sub" jsonschema:"description=Player OIDC subject,required"`
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
		Collection string `json:"collection" jsonschema:"description=PocketBase collection name (e.g. players, maps, sprite_bases, bans),required"`
		PerPage    int    `json:"per_page,omitempty" jsonschema:"description=Page size (default 30)"`
		Page       int    `json:"page,omitempty" jsonschema:"description=Page number (1-based, default 1)"`
		Filter     string `json:"filter,omitempty" jsonschema:"description=PocketBase filter expression, e.g. 'is_default = true' or 'is_admin = true'"`
		Sort       string `json:"sort,omitempty" jsonschema:"description=Sort expression, e.g. '-created' or 'display_name'"`
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
		Collection string `json:"collection" jsonschema:"description=PocketBase collection name,required"`
		ID         string `json:"id" jsonschema:"description=Record ID,required"`
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
}

// --- Control tools ---

func registerControlTools(s *mcp.Server, w *WorldsimClient) {
	type TeleportArgs struct {
		EntityID     string `json:"entity_id" jsonschema:"description=Player entity ID to teleport,required"`
		MapID        string `json:"map_id" jsonschema:"description=Target map ID,required"`
		TargetEntity string `json:"target_entity,omitempty" jsonschema:"description=Optional target entity (beacon name) on the target map; if empty, a random spawn zone is used"`
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
		ClientID string `json:"client_id" jsonschema:"description=Pusher session ID (client_id from get_world_stats) to kick,required"`
		Reason   string `json:"reason,omitempty" jsonschema:"description=Human-readable kick reason (recorded in audit)"`
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
		TargetType  string `json:"target_type" jsonschema:"description=Which identifier to ban: 'user_id', 'ip', or 'device_id',required"`
		TargetValue string `json:"target_value" jsonschema:"description=The identifier value to ban,required"`
		Reason      string `json:"reason,omitempty" jsonschema:"description=Human-readable ban reason (recorded in audit + shown to the player on connect)"`
		BannedUntil int64  `json:"banned_until,omitempty" jsonschema:"description=Unix timestamp (seconds) when the ban expires. 0 = permanent."`
		BannedBy    string `json:"banned_by,omitempty" jsonschema:"description=Optional audit label (e.g. admin sub or 'mcp')"`
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
		EntityID string `json:"entity_id" jsonschema:"description=Entity ID to send as (display name is stamped from the entity),required"`
		Channel  string `json:"channel" jsonschema:"description=Chat channel: 'global' or 'proximity',required"`
		Text     string `json:"text" jsonschema:"description=Message text (truncated to 500 runes server-side),required"`
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
		EntityID string `json:"entity_id" jsonschema:"description=Entity ID to rename,required"`
		Name     string `json:"name" jsonschema:"description=New display name (sanitized to ASCII printable, truncated to 20 runes),required"`
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
		EntityID string `json:"entity_id" jsonschema:"description=Entity ID,required"`
		Status   uint32 `json:"status" jsonschema:"description=Presence status: 0=Available, 1=Busy, 2=Do Not Disturb,required"`
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
		EntityID   string `json:"entity_id" jsonschema:"description=Entity ID,required"`
		SpriteBase string `json:"sprite_base" jsonschema:"description=sprite_bases PocketBase record ID; empty = revert to fallback,required"`
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
		EntityID string `json:"entity_id" jsonschema:"description=Entity ID,required"`
		Options  string `json:"options" jsonschema:"description=JSON-encoded player options (full replace, e.g. {\"show_own_name_tag\":true,\"zoom\":2}),required"`
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
		ExtensionID string         `json:"extension_id" jsonschema:"description=Extension ID (e.g. ext-walls, ext-props),required"`
		Payload     map[string]any `json:"payload" jsonschema:"description=Action dispatch payload (entity_id, input, action_id, etc.) — passed through to extension.<id>.action,required"`
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
